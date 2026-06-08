// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
)

// putBytes is a helper that writes a small object and asserts success.
func putBytesQuota(t *testing.T, sp *ServerPools, bucket, key string, body []byte) ObjectInfo {
	t.Helper()
	oi, err := sp.PutObject(context.Background(), bucket, key, NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject(%q): %v", key, err)
	}
	return oi
}

// TestSetGetDeleteBucketQuota exercises the basic CRUD lifecycle for a quota
// configuration: set → get → verify → delete → get (expect not-found).
func TestSetGetDeleteBucketQuota(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "photos")

	q := BucketQuota{HardSize: 1024 * 1024, HardCount: 100}
	if err := sp.SetBucketQuota(ctx, "photos", q); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}

	got, err := sp.GetBucketQuota(ctx, "photos")
	if err != nil {
		t.Fatalf("GetBucketQuota: %v", err)
	}
	if got.HardSize != q.HardSize || got.HardCount != q.HardCount {
		t.Fatalf("quota mismatch: got %+v, want %+v", got, q)
	}

	if err := sp.DeleteBucketQuota(ctx, "photos"); err != nil {
		t.Fatalf("DeleteBucketQuota: %v", err)
	}
	if _, err := sp.GetBucketQuota(ctx, "photos"); !errors.Is(err, ErrNoSuchBucketQuota) {
		t.Fatalf("after delete GetBucketQuota = %v, want ErrNoSuchBucketQuota", err)
	}
}

// TestDeleteBucketQuotaIdempotent verifies that deleting a quota when none is
// set does not return an error (idempotent).
func TestDeleteBucketQuotaIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	if err := sp.DeleteBucketQuota(ctx, "b"); err != nil {
		t.Fatalf("delete with no quota: %v", err)
	}
}

// TestGetBucketQuotaMissingBucket checks that GetBucketQuota returns
// ErrBucketNotFound for a bucket that does not exist.
func TestGetBucketQuotaMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	if _, err := sp.GetBucketQuota(ctx, "ghost"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("GetBucketQuota missing bucket = %v, want ErrBucketNotFound", err)
	}
}

// TestHardSizeQuotaBlocksWrite verifies that a PUT that would exceed the hard
// size limit is rejected with ErrBucketQuotaExceeded.
func TestHardSizeQuotaBlocksWrite(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "limited")

	// Set a hard limit of 100 bytes.
	if err := sp.SetBucketQuota(ctx, "limited", BucketQuota{HardSize: 100}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}

	// Write 50 bytes: should succeed.
	putBytesQuota(t, sp, "limited", "a", bytes.Repeat([]byte("x"), 50))

	// Write another 60 bytes: 50+60 > 100, must be rejected.
	_, err := sp.PutObject(ctx, "limited", "b",
		NewPutReader(bytes.NewReader(bytes.Repeat([]byte("y"), 60)), 60),
		ObjectOptions{})
	if !errors.Is(err, ErrBucketQuotaExceeded) {
		t.Fatalf("second write = %v, want ErrBucketQuotaExceeded", err)
	}
}

// TestHardCountQuotaBlocksWrite verifies that a PUT that would exceed the hard
// object-count limit is rejected.
func TestHardCountQuotaBlocksWrite(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "counted")

	// Only 2 objects allowed.
	if err := sp.SetBucketQuota(ctx, "counted", BucketQuota{HardCount: 2}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}

	putBytesQuota(t, sp, "counted", "obj1", []byte("a"))
	putBytesQuota(t, sp, "counted", "obj2", []byte("b"))

	_, err := sp.PutObject(ctx, "counted", "obj3",
		NewPutReader(strings.NewReader("c"), 1),
		ObjectOptions{})
	if !errors.Is(err, ErrBucketQuotaExceeded) {
		t.Fatalf("third PUT = %v, want ErrBucketQuotaExceeded", err)
	}
}

// TestSoftQuotaDoesNotBlockWrite verifies that breaching only the soft limit
// does not prevent the write from completing.
func TestSoftQuotaDoesNotBlockWrite(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "soft")

	// Soft limit of 10 bytes; no hard limit.
	if err := sp.SetBucketQuota(ctx, "soft", BucketQuota{SoftSize: 10}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}

	// Write 20 bytes — crosses the soft limit but must succeed.
	putBytesQuota(t, sp, "soft", "big", bytes.Repeat([]byte("z"), 20))

	got := getBytes(t, sp, "soft", "big", ObjectOptions{})
	if len(got) != 20 {
		t.Fatalf("read back %d bytes, want 20", len(got))
	}
}

// TestQuotaUsageUpdatedAfterDelete checks that deleting an object frees quota
// so a subsequent write that would previously be over-limit succeeds.
func TestQuotaUsageUpdatedAfterDelete(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "reclaim")

	if err := sp.SetBucketQuota(ctx, "reclaim", BucketQuota{HardSize: 100}); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}

	// Fill to 80 bytes.
	putBytesQuota(t, sp, "reclaim", "first", bytes.Repeat([]byte("a"), 80))

	// A second 30-byte write would exceed the 100-byte limit.
	_, err := sp.PutObject(ctx, "reclaim", "second",
		NewPutReader(bytes.NewReader(bytes.Repeat([]byte("b"), 30)), 30),
		ObjectOptions{})
	if !errors.Is(err, ErrBucketQuotaExceeded) {
		t.Fatalf("pre-delete second write = %v, want ErrBucketQuotaExceeded", err)
	}

	// Delete the first object (not versioned, so real delete).
	if _, err := sp.DeleteObject(ctx, "reclaim", "first", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Now the 30-byte write should succeed (0 + 30 <= 100).
	putBytesQuota(t, sp, "reclaim", "second", bytes.Repeat([]byte("b"), 30))
}

// TestNoQuotaAllowsWrite confirms that PutObject is unrestricted when no quota
// is set on the bucket.
func TestNoQuotaAllowsWrite(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "free")
	// Write 10 MiB with no quota set — must succeed.
	data := bytes.Repeat([]byte("z"), 10<<20)
	_, err := sp.PutObject(ctx, "free", "big",
		NewPutReader(bytes.NewReader(data), int64(len(data))),
		ObjectOptions{})
	if err != nil {
		t.Fatalf("unrestricted PutObject: %v", err)
	}
}

// TestQuotaPersistedAcrossGet verifies that SetBucketQuota with soft limits
// round-trips correctly through GetBucketQuota.
func TestQuotaPersistedAcrossGet(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 2)
	mustMakeBucket(t, sp, "q")
	q := BucketQuota{HardSize: 500, HardCount: 50, SoftSize: 300, SoftCount: 25}
	if err := sp.SetBucketQuota(ctx, "q", q); err != nil {
		t.Fatalf("SetBucketQuota: %v", err)
	}
	got, err := sp.GetBucketQuota(ctx, "q")
	if err != nil {
		t.Fatalf("GetBucketQuota: %v", err)
	}
	if got != q {
		t.Fatalf("quota round-trip: got %+v, want %+v", got, q)
	}
}
