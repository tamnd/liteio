// SPDX-License-Identifier: Apache-2.0

package erasure

import (
	"bytes"
	"crypto/rand"
	"testing"
)

func mustCoder(t testing.TB, k, m int) Coder {
	t.Helper()
	c, err := NewCoder(k, m)
	if err != nil {
		t.Fatalf("NewCoder(%d,%d): %v", k, m, err)
	}
	return c
}

func TestNewCoderValidation(t *testing.T) {
	for _, tc := range []struct{ k, m int }{{0, 1}, {1, 0}, {-1, 4}, {200, 100}} {
		if _, err := NewCoder(tc.k, tc.m); err == nil {
			t.Fatalf("NewCoder(%d,%d) expected error", tc.k, tc.m)
		}
	}
	c := mustCoder(t, 4, 4)
	if c.DataShards() != 4 || c.ParityShards() != 4 {
		t.Fatalf("shard counts wrong: %d/%d", c.DataShards(), c.ParityShards())
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	c := mustCoder(t, 4, 4)
	for _, size := range []int{1, 100, 1 << 10, (1 << 20) + 7} {
		data := make([]byte, size)
		if _, err := rand.Read(data); err != nil {
			t.Fatal(err)
		}
		shards, err := EncodeData(c, data)
		if err != nil {
			t.Fatalf("EncodeData(%d): %v", size, err)
		}
		if len(shards) != 8 {
			t.Fatalf("got %d shards want 8", len(shards))
		}
		got, err := DecodeData(c, shards, size)
		if err != nil {
			t.Fatalf("DecodeData(%d): %v", size, err)
		}
		if !bytes.Equal(got, data) {
			t.Fatalf("round trip mismatch at size %d", size)
		}
	}
}

// Dropping up to M shards must still reconstruct the data (spec doc 03: tolerate
// loss of M drives).
func TestDecodeWithMissingShards(t *testing.T) {
	c := mustCoder(t, 4, 4) // tolerate 4 lost
	data := make([]byte, 64<<10)
	if _, err := rand.Read(data); err != nil {
		t.Fatal(err)
	}
	shards, err := EncodeData(c, data)
	if err != nil {
		t.Fatal(err)
	}

	// Drop 4 shards (the max tolerable): indices 0,2,5,7.
	dropped := append([][]byte(nil), shards...)
	for _, i := range []int{0, 2, 5, 7} {
		dropped[i] = nil
	}
	got, err := DecodeData(c, dropped, len(data))
	if err != nil {
		t.Fatalf("decode with 4 missing: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reconstruction mismatch with 4 missing shards")
	}
}

// Losing more than M shards must fail rather than return wrong bytes.
func TestDecodeTooFewShards(t *testing.T) {
	c := mustCoder(t, 4, 4)
	data := make([]byte, 8<<10)
	_, _ = rand.Read(data)
	shards, err := EncodeData(c, data)
	if err != nil {
		t.Fatal(err)
	}
	// Drop 5 shards: only 3 remain, below K=4.
	for _, i := range []int{0, 1, 2, 3, 4} {
		shards[i] = nil
	}
	if _, err := DecodeData(c, shards, len(data)); err != ErrTooFewShards {
		t.Fatalf("expected ErrTooFewShards, got %v", err)
	}
}

func TestBitrotHashAndVerify(t *testing.T) {
	data := []byte("a shard of object data that must verify")
	sum := HashShard(data)
	if len(sum) != ChecksumSize {
		t.Fatalf("checksum size %d want %d", len(sum), ChecksumSize)
	}
	if !VerifyShard(data, sum) {
		t.Fatal("VerifyShard rejected a good shard")
	}
	// Flip one bit: verification must fail.
	corrupt := append([]byte(nil), data...)
	corrupt[3] ^= 0x01
	if VerifyShard(corrupt, sum) {
		t.Fatal("VerifyShard accepted a corrupted shard")
	}
	// Wrong-length checksum is rejected.
	if VerifyShard(data, sum[:16]) {
		t.Fatal("VerifyShard accepted a truncated checksum")
	}
}

// End-to-end: encode, checksum each shard, corrupt one on disk, detect it via the
// checksum, nil it out, and reconstruct correctly. This is the read-path heal
// trigger in miniature (spec doc 03/05).
func TestBitrotDrivenReconstruction(t *testing.T) {
	c := mustCoder(t, 4, 2)
	data := make([]byte, 32<<10)
	_, _ = rand.Read(data)
	encoded, err := EncodeData(c, data)
	if err != nil {
		t.Fatal(err)
	}
	sums := make([][]byte, len(encoded))
	for i, s := range encoded {
		sums[i] = HashShard(s)
	}

	// Model the read path: each shard is fetched from a separate drive into its
	// own buffer (EncodeData's shards share one backing buffer; see its doc).
	shards := make([][]byte, len(encoded))
	for i, s := range encoded {
		shards[i] = append([]byte(nil), s...)
	}

	// Silently corrupt shard 1, as a bit flip on disk would.
	shards[1][10] ^= 0xff

	// Read path: verify each shard, nil out the corrupt one.
	healNeeded := false
	for i, s := range shards {
		if !VerifyShard(s, sums[i]) {
			shards[i] = nil
			healNeeded = true
		}
	}
	if !healNeeded {
		t.Fatal("corruption not detected")
	}
	got, err := DecodeData(c, shards, len(data))
	if err != nil {
		t.Fatalf("reconstruct after bitrot: %v", err)
	}
	if !bytes.Equal(got, data) {
		t.Fatal("bytes wrong after bitrot reconstruction")
	}
}

func TestVerifyDetectsParityMismatch(t *testing.T) {
	c := mustCoder(t, 4, 2)
	data := make([]byte, 4096)
	_, _ = rand.Read(data)
	shards, err := EncodeData(c, data)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := c.Verify(shards)
	if err != nil || !ok {
		t.Fatalf("Verify on good shards: ok=%v err=%v", ok, err)
	}
	shards[0][0] ^= 0xff
	ok, err = c.Verify(shards)
	if err != nil {
		t.Fatalf("Verify err: %v", err)
	}
	if ok {
		t.Fatal("Verify did not detect a corrupted data shard")
	}
}

func benchData(size int) []byte {
	d := make([]byte, size)
	_, _ = rand.Read(d)
	return d
}

func BenchmarkEncode1MiB(b *testing.B) {
	c := mustCoder(b, 4, 4)
	data := benchData(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := EncodeData(c, data); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeDegraded1MiB(b *testing.B) {
	c := mustCoder(b, 4, 4)
	data := benchData(1 << 20)
	shards, err := EncodeData(c, data)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		degraded := append([][]byte(nil), shards...)
		// Copy shards so reconstruction does not mutate the originals.
		for j := range degraded {
			s := make([]byte, len(shards[j]))
			copy(s, shards[j])
			degraded[j] = s
		}
		degraded[0], degraded[1], degraded[2] = nil, nil, nil
		if _, err := DecodeData(c, degraded, len(data)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkHashShard1MiB(b *testing.B) {
	data := benchData(1 << 20)
	b.SetBytes(int64(len(data)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = HashShard(data)
	}
}
