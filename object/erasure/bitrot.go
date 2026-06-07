// SPDX-License-Identifier: Apache-2.0

package erasure

import (
	"crypto/subtle"
	"fmt"
	"hash"

	"github.com/minio/highwayhash"
)

// BitrotAlgorithm names the shard checksum algorithm recorded in obj.meta.
const BitrotAlgorithm = "highwayhash256"

// bitrotKey is the fixed 32-byte key for the keyed HighwayHash used for bitrot
// detection. HighwayHash is keyed, but bitrot is an integrity check, not a
// security boundary, so a fixed cluster-wide key is correct and lets any node (or
// the offline meta inspector) verify a shard. It must never change for a given
// on-disk format version.
var bitrotKey = [32]byte{
	0x4c, 0x49, 0x4f, 0x32, 0x62, 0x69, 0x74, 0x72, // "LIO2bitr"
	0x6f, 0x74, 0x68, 0x77, 0x68, 0x32, 0x35, 0x36, // "othwh256"
	0x6b, 0x65, 0x79, 0x76, 0x31, 0x00, 0x00, 0x00, // "keyv1"
	0xa5, 0x5a, 0xc3, 0x3c, 0x0f, 0xf0, 0x96, 0x69,
}

// NewBitrotHash returns a fresh keyed HighwayHash-256 hasher. It never errors for
// the fixed valid key, but the error is surfaced for completeness.
func NewBitrotHash() (hash.Hash, error) {
	h, err := highwayhash.New(bitrotKey[:])
	if err != nil {
		return nil, fmt.Errorf("erasure: bitrot hasher: %w", err)
	}
	return h, nil
}

// HashShard computes the HighwayHash-256 checksum of a shard (or an inline data
// block). The checksum is stored alongside the shard in obj.meta and verified on
// read.
func HashShard(data []byte) []byte {
	sum := highwayhash.Sum(data, bitrotKey[:])
	return sum[:]
}

// VerifyShard reports whether data matches the expected HighwayHash-256 checksum,
// using a constant-time comparison. A false result means the shard is corrupt and
// must be treated as missing for reconstruction, and the object enqueued for heal.
func VerifyShard(data, expected []byte) bool {
	if len(expected) != highwayhash.Size {
		return false
	}
	got := highwayhash.Sum(data, bitrotKey[:])
	return subtle.ConstantTimeCompare(got[:], expected) == 1
}

// ChecksumSize is the byte length of a HighwayHash-256 checksum.
const ChecksumSize = highwayhash.Size
