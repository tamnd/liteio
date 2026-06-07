// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"fmt"
	"testing"
	"time"
)

// countingWalk returns a walk func that records how many times it ran and yields
// a fixed key set each time.
func countingWalk(keys []string, calls *int) func() ([]string, error) {
	return func() ([]string, error) {
		*calls++
		out := make([]string, len(keys))
		copy(out, keys)
		return out, nil
	}
}

func TestMetacacheServesCachedWalk(t *testing.T) {
	c := newMetacache()
	calls := 0
	walk := countingWalk([]string{"a", "b"}, &calls)

	first, _ := c.keys("bkt", walk)
	second, _ := c.keys("bkt", walk)

	if calls != 1 {
		t.Fatalf("walk ran %d times, want 1 (second list should hit cache)", calls)
	}
	if fmt.Sprint(first) != "[a b]" || fmt.Sprint(second) != "[a b]" {
		t.Fatalf("keys = %v / %v, want [a b]", first, second)
	}
}

func TestMetacacheTTLExpiry(t *testing.T) {
	c := newMetacache()
	now := time.Unix(1700000000, 0)
	c.now = func() time.Time { return now }
	calls := 0
	walk := countingWalk([]string{"a"}, &calls)

	c.keys("bkt", walk)
	now = now.Add(metacacheTTL - time.Millisecond) // still fresh
	c.keys("bkt", walk)
	if calls != 1 {
		t.Fatalf("walk ran %d times before TTL, want 1", calls)
	}
	now = now.Add(2 * time.Millisecond) // now past the TTL
	c.keys("bkt", walk)
	if calls != 2 {
		t.Fatalf("walk ran %d times after TTL, want 2", calls)
	}
}

func TestMetacacheAddKeepsCacheWarm(t *testing.T) {
	c := newMetacache()
	calls := 0
	walk := countingWalk([]string{"a", "c"}, &calls)
	c.keys("bkt", walk)

	c.add("bkt", "b") // inserts in sorted position
	got, _ := c.keys("bkt", walk)

	if calls != 1 {
		t.Fatalf("add should not trigger a re-walk; walk ran %d times", calls)
	}
	if fmt.Sprint(got) != "[a b c]" {
		t.Fatalf("keys after add = %v, want [a b c]", got)
	}
}

func TestMetacacheAddIsIdempotentAndCopiesOnWrite(t *testing.T) {
	c := newMetacache()
	calls := 0
	c.keys("bkt", countingWalk([]string{"a", "b"}, &calls))

	// A reader holds the slice it was handed; a concurrent add must not mutate it.
	before, _ := c.keys("bkt", countingWalk([]string{"a", "b"}, &calls))
	c.add("bkt", "b") // already present: no-op
	c.add("bkt", "c") // new: copy-on-write
	if fmt.Sprint(before) != "[a b]" {
		t.Fatalf("previously returned slice was mutated: %v", before)
	}
	after, _ := c.keys("bkt", countingWalk(nil, &calls))
	if fmt.Sprint(after) != "[a b c]" {
		t.Fatalf("keys after add = %v, want [a b c]", after)
	}
}

func TestMetacacheAddNoOpWhenUncached(t *testing.T) {
	c := newMetacache()
	c.add("bkt", "a") // bucket never listed: nothing to update
	calls := 0
	got, _ := c.keys("bkt", countingWalk([]string{"x"}, &calls))
	if calls != 1 || fmt.Sprint(got) != "[x]" {
		t.Fatalf("uncached add should not populate; got %v (calls=%d)", got, calls)
	}
}

func TestMetacacheInvalidateForcesRewalk(t *testing.T) {
	c := newMetacache()
	calls := 0
	walk := countingWalk([]string{"a"}, &calls)
	c.keys("bkt", walk)
	c.invalidate("bkt")
	c.keys("bkt", walk)
	if calls != 2 {
		t.Fatalf("walk ran %d times, want 2 after invalidate", calls)
	}
}

func TestMetacacheEvictsOldestAtCap(t *testing.T) {
	c := newMetacache()
	now := time.Unix(1700000000, 0)
	c.now = func() time.Time { return now }
	calls := 0
	// Fill to capacity; each bucket populated one second apart.
	for i := range metacacheMaxBuckets {
		now = now.Add(time.Second)
		c.keys(fmt.Sprintf("b%d", i), countingWalk([]string{"k"}, &calls))
	}
	// One more bucket evicts the oldest (b0).
	now = now.Add(time.Second)
	c.keys("overflow", countingWalk([]string{"k"}, &calls))

	if len(c.m) != metacacheMaxBuckets {
		t.Fatalf("cache holds %d buckets, want %d", len(c.m), metacacheMaxBuckets)
	}
	if _, ok := c.m["b0"]; ok {
		t.Fatalf("oldest bucket b0 should have been evicted")
	}
	if _, ok := c.m["overflow"]; !ok {
		t.Fatalf("newest bucket should be present")
	}
}

// TestListReflectsWritesAndDeletes pins the end-to-end consistency the cache must
// preserve: a list, then a write, then a list shows the new key; a delete then a
// list drops it. This proves the add/invalidate hooks keep the warm cache honest.
func TestListReflectsWritesAndDeletes(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	put := func(key string) {
		_, err := sp.PutObject(ctx, "b", key, NewPutReader(bytes.NewReader([]byte("x")), 1), ObjectOptions{})
		if err != nil {
			t.Fatalf("PutObject %s: %v", key, err)
		}
	}
	put("a")
	put("b")

	r1, _ := sp.ListObjectsV2(ctx, "b", "", "", "", "", 1000, false)
	if len(r1.Objects) != 2 {
		t.Fatalf("first list = %d objects, want 2", len(r1.Objects))
	}

	// A write after the cache is warm must appear (add hook).
	put("c")
	r2, _ := sp.ListObjectsV2(ctx, "b", "", "", "", "", 1000, false)
	if len(r2.Objects) != 3 || r2.Objects[2].Name != "c" {
		t.Fatalf("list after put = %+v, want a,b,c", names(r2.Objects))
	}

	// A delete must drop the key (invalidate hook).
	if _, err := sp.DeleteObject(ctx, "b", "a", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	r3, _ := sp.ListObjectsV2(ctx, "b", "", "", "", "", 1000, false)
	if len(r3.Objects) != 2 || r3.Objects[0].Name != "b" {
		t.Fatalf("list after delete = %v, want b,c", names(r3.Objects))
	}
}

// TestMetacacheConcurrentListAndWrite races readers iterating a returned key
// slice against writers inserting keys. Under -race it proves add's copy-on-write
// never mutates a slice a reader still holds.
func TestMetacacheConcurrentListAndWrite(t *testing.T) {
	c := newMetacache()
	calls := 0
	c.keys("b", countingWalk([]string{"a"}, &calls))

	done := make(chan struct{})
	go func() {
		for i := range 200 {
			c.add("b", fmt.Sprintf("k%04d", i))
		}
		close(done)
	}()
	for {
		keys, _ := c.keys("b", countingWalk([]string{"a"}, &calls))
		total := 0
		for range keys { // iterate the handed-out slice
			total++
		}
		select {
		case <-done:
			return
		default:
		}
	}
}

func names(objs []ObjectInfo) []string {
	out := make([]string, len(objs))
	for i, o := range objs {
		out[i] = o.Name
	}
	return out
}

// benchBucket builds a bucket of n objects spread across a deep prefix tree, the
// shape that makes an index-free namespace walk expensive.
func benchBucket(b *testing.B, n int) *ServerPools {
	b.Helper()
	ctx := context.Background()
	sp := newLayer(b, 6, 3)
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		b.Fatalf("MakeBucket: %v", err)
	}
	for i := range n {
		key := fmt.Sprintf("prefix/%04d/object.dat", i)
		if _, err := sp.PutObject(ctx, "b", key, NewPutReader(bytes.NewReader([]byte("x")), 1), ObjectOptions{}); err != nil {
			b.Fatalf("PutObject: %v", err)
		}
	}
	return sp
}

// BenchmarkBucketWalkCold measures the namespace walk the metacache eliminates on
// a hit: the cache is invalidated before each iteration so every call re-descends
// the tree.
func BenchmarkBucketWalkCold(b *testing.B) {
	ctx := context.Background()
	sp := benchBucket(b, 1000)
	b.ResetTimer()
	for b.Loop() {
		sp.cache.invalidate("b")
		if _, err := sp.bucketKeys(ctx, "b"); err != nil {
			b.Fatalf("walk: %v", err)
		}
	}
}

// BenchmarkBucketKeysCached measures the same call served from a warm cache — the
// cost a paginated or repeat client pays on every page after the first.
func BenchmarkBucketKeysCached(b *testing.B) {
	ctx := context.Background()
	sp := benchBucket(b, 1000)
	sp.bucketKeys(ctx, "b") // warm
	b.ResetTimer()
	for b.Loop() {
		if _, err := sp.bucketKeys(ctx, "b"); err != nil {
			b.Fatalf("keys: %v", err)
		}
	}
}
