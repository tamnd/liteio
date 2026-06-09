// SPDX-License-Identifier: Apache-2.0

// Package erasure implements liteio's Reed-Solomon erasure coding and bitrot
// protection (spec 2020, docs 03 and 06).
//
// An object's data is split into K data shards and M parity shards (N = K + M)
// over GF(2^8). Any K of the N shards reconstruct the data, so a set tolerates the
// loss of up to M drives. The Reed-Solomon backend is SIMD-accelerated
// (klauspost/reedsolomon: SSSE3/AVX2/AVX512/GFNI on amd64, NEON on arm64, with a
// pure-Go fallback above 1 GB/s/core) and sits behind the Coder interface so a
// faster backend can replace it without touching the data path.
//
// Every shard carries a HighwayHash-256 checksum (see bitrot.go) verified before
// the shard is used in reconstruction; a mismatch marks the shard missing for that
// read and enqueues a heal.
package erasure

import (
	"errors"
	"fmt"
	"io"

	"github.com/klauspost/reedsolomon"
)

// Algorithm is the erasure algorithm identifier recorded in obj.meta.
const Algorithm = "reedsolomon"

// Coder encodes, reconstructs, and verifies erasure shards. It is the swappable
// engine boundary (spec doc 11.7): the default backend is klauspost/reedsolomon;
// a future ISA-L-class backend can implement the same interface.
type Coder interface {
	// Encode fills the parity shards from the data shards. data must hold
	// DataShards+ParityShards slices, all the same length, with the data shards
	// populated and the parity shards allocated.
	Encode(shards [][]byte) error

	// Reconstruct recovers missing shards (passed as nil entries) from the present
	// ones. It needs at least DataShards present shards.
	Reconstruct(shards [][]byte) error

	// ReconstructData recovers only the missing data shards (not parity), which is
	// cheaper when the caller only needs to read the object back.
	ReconstructData(shards [][]byte) error

	// Verify reports whether the parity shards are consistent with the data shards.
	Verify(shards [][]byte) (bool, error)

	// Split slices data into DataShards data shards plus empty ParityShards parity
	// shards, zero-padding the final data shard as needed.
	Split(data []byte) ([][]byte, error)

	// Join writes the reconstructed object data (outSize bytes) to dst by
	// concatenating the data shards and trimming padding.
	Join(dst io.Writer, shards [][]byte, outSize int) error

	// DataShards returns K.
	DataShards() int
	// ParityShards returns M.
	ParityShards() int
}

// rsCoder adapts klauspost/reedsolomon to Coder.
type rsCoder struct {
	enc    reedsolomon.Encoder
	data   int
	parity int
}

// NewCoder returns a Coder for the given shard geometry. dataShards (K) and
// parityShards (M) must each be >= 1 and sum to at most 256.
func NewCoder(dataShards, parityShards int) (Coder, error) {
	if dataShards < 1 || parityShards < 1 {
		return nil, fmt.Errorf("erasure: need >=1 data and parity shard, got K=%d M=%d", dataShards, parityShards)
	}
	if dataShards+parityShards > 256 {
		return nil, fmt.Errorf("erasure: K+M=%d exceeds 256", dataShards+parityShards)
	}
	enc, err := reedsolomon.New(dataShards, parityShards)
	if err != nil {
		return nil, fmt.Errorf("erasure: %w", err)
	}
	return &rsCoder{enc: enc, data: dataShards, parity: parityShards}, nil
}

func (c *rsCoder) Encode(shards [][]byte) error          { return c.enc.Encode(shards) }
func (c *rsCoder) Reconstruct(shards [][]byte) error     { return c.enc.Reconstruct(shards) }
func (c *rsCoder) ReconstructData(shards [][]byte) error { return c.enc.ReconstructData(shards) }
func (c *rsCoder) Verify(shards [][]byte) (bool, error)  { return c.enc.Verify(shards) }
func (c *rsCoder) Split(data []byte) ([][]byte, error)   { return c.enc.Split(data) }
func (c *rsCoder) DataShards() int                       { return c.data }
func (c *rsCoder) ParityShards() int                     { return c.parity }

func (c *rsCoder) Join(dst io.Writer, shards [][]byte, outSize int) error {
	return c.enc.Join(dst, shards, outSize)
}

// ErrTooFewShards is returned by EncodeData/DecodeData when fewer than K shards
// are available to reconstruct the data.
var ErrTooFewShards = errors.New("erasure: not enough shards to reconstruct")

// EncodeData is a convenience that splits data into shards and computes parity in
// one call. It returns DataShards+ParityShards equal-length shards. It is used for
// inline small objects and for per-stripe encoding on the streaming path.
//
// The returned shards may share one backing buffer (the Reed-Solomon Split
// allocates a single block and slices it). They are meant to be written straight
// out to drives; a caller that needs to mutate shards independently (as the read
// path does, reading each shard from a separate drive) must copy them first.
func EncodeData(c Coder, data []byte) ([][]byte, error) {
	shards, err := c.Split(data)
	if err != nil {
		return nil, fmt.Errorf("erasure: split: %w", err)
	}
	if err := c.Encode(shards); err != nil {
		return nil, fmt.Errorf("erasure: encode: %w", err)
	}
	return shards, nil
}

// DecodeData reconstructs object data of outSize bytes from shards. Missing shards
// must be passed as nil. It reconstructs the data shards if any are missing and
// returns the joined object bytes. It does not verify bitrot checksums; callers
// that have checksums should verify and nil out bad shards first (see VerifyShard).
func DecodeData(c Coder, shards [][]byte, outSize int) ([]byte, error) {
	if outSize == 0 {
		return []byte{}, nil
	}
	present := 0
	for _, s := range shards {
		if s != nil {
			present++
		}
	}
	if present < c.DataShards() {
		return nil, ErrTooFewShards
	}
	if err := c.ReconstructData(shards); err != nil {
		return nil, fmt.Errorf("erasure: reconstruct: %w", err)
	}
	out := make([]byte, 0, outSize)
	w := &sliceWriter{buf: out}
	if err := c.Join(w, shards, outSize); err != nil {
		return nil, fmt.Errorf("erasure: join: %w", err)
	}
	return w.buf, nil
}

// sliceWriter is a tiny io.Writer that appends to an in-memory buffer, avoiding a
// bytes.Buffer's extra bookkeeping on the small-object inline path.
type sliceWriter struct{ buf []byte }

func (w *sliceWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}
