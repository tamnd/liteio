// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"slices"
	"testing"

	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// sharedDrives builds n local drives both nodes in a coherence test can mount, so
// two ServerPools see the same data through independent metacaches — the shape of
// a real cluster where every node reaches the same drives over RPC.
func sharedDrives(tb testing.TB, n int) []storage.StorageAPI {
	tb.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(tb.TempDir())
		if err != nil {
			tb.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	return drives
}

// poolsOver builds a single-set layer over the given drives with the given
// options, so two nodes can be constructed over one drive set.
func poolsOver(tb testing.TB, drives []storage.StorageAPI, opts ...Option) *ServerPools {
	tb.Helper()
	sp, err := NewServerPools(depID(7), []PoolConfig{{Sets: []SetConfig{{Drives: drives, Parity: 2}}}}, opts...)
	if err != nil {
		tb.Fatalf("NewServerPools: %v", err)
	}
	return sp
}

// listKeys returns the object keys a full listing of bucket reports.
func listKeys(tb testing.TB, sp *ServerPools, bucket string) []string {
	tb.Helper()
	res, err := sp.ListObjectsV2(context.Background(), bucket, "", "", "", "", 1000, false)
	if err != nil {
		tb.Fatalf("ListObjectsV2: %v", err)
	}
	keys := make([]string, 0, len(res.Objects))
	for _, o := range res.Objects {
		keys = append(keys, o.Name)
	}
	return keys
}

func putBody(tb testing.TB, sp *ServerPools, bucket, key string, body []byte) {
	tb.Helper()
	_, err := sp.PutObject(context.Background(), bucket, key, NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		tb.Fatalf("PutObject %s: %v", key, err)
	}
}

// TestCacheNotifierFiresOnMutations checks the layer reports every key-set change
// to its notifier with the right shape: a non-empty key for a write, an empty key
// for anything that invalidates the bucket.
func TestCacheNotifierFiresOnMutations(t *testing.T) {
	// Each event is encoded "bucket|key" so an empty key (an invalidation) is
	// distinguishable from a write.
	var events []string
	notify := func(bucket, key string) { events = append(events, bucket+"|"+key) }

	drives := sharedDrives(t, 4)
	sp := poolsOver(t, drives, WithCacheNotifier(notify))
	ctx := context.Background()

	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	putBody(t, sp, "b", "k1", healBytes(2<<10))
	if _, err := sp.DeleteObject(ctx, "b", "k1", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	putBody(t, sp, "b", "k2", healBytes(2<<10))
	if _, errs := sp.DeleteObjects(ctx, "b", []ObjectToDelete{{Name: "k2"}}, ObjectOptions{}); errs[0] != nil {
		t.Fatalf("DeleteObjects: %v", errs[0])
	}

	want := []string{
		"b|k1", // PutObject k1
		"b|",   // DeleteObject k1 invalidates
		"b|k2", // PutObject k2
		"b|",   // DeleteObjects k2 invalidates
	}
	if !slices.Equal(events, want) {
		t.Fatalf("notifier events = %v, want %v", events, want)
	}
}

// TestCrossNodeCacheCoherence is the end-to-end coherence proof. Two nodes mount
// the same drives. Node B warms its listing cache, then node A writes a new
// object and (via its notifier) tells B. B must see the new object on its next
// list without waiting for the cache TTL — and a third node with no coherence
// must still serve the stale listing, which is what makes the assertion meaningful.
func TestCrossNodeCacheCoherence(t *testing.T) {
	drives := sharedDrives(t, 4)
	ctx := context.Background()

	nodeB := poolsOver(t, drives)
	stale := poolsOver(t, drives) // a node that receives no coherence events
	nodeA := poolsOver(t, drives, WithCacheNotifier(func(bucket, key string) {
		nodeB.ApplyRemoteCache(bucket, key)
	}))

	if err := nodeA.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	putBody(t, nodeA, "b", "alpha", healBytes(4<<10))

	// Both B and the stale node warm their caches on this first list.
	if got := listKeys(t, nodeB, "b"); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("nodeB initial list = %v, want [alpha]", got)
	}
	if got := listKeys(t, stale, "b"); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("stale initial list = %v, want [alpha]", got)
	}

	// Node A writes a second object; coherence pushes the key to B only.
	putBody(t, nodeA, "b", "beta", healBytes(4<<10))

	if got := listKeys(t, nodeB, "b"); !slices.Equal(got, []string{"alpha", "beta"}) {
		t.Fatalf("nodeB after coherent write = %v, want [alpha beta]", got)
	}
	// The stale node serves its warm cache and misses beta until its TTL elapses,
	// which is exactly the gap coherence closes.
	if got := listKeys(t, stale, "b"); !slices.Equal(got, []string{"alpha"}) {
		t.Fatalf("stale node = %v, want stale [alpha] (proves the cache is warm)", got)
	}
}

// TestCrossNodeDeleteCoherence checks that a delete elsewhere invalidates a peer's
// cache so the removed key drops from its listing without waiting for the TTL.
func TestCrossNodeDeleteCoherence(t *testing.T) {
	drives := sharedDrives(t, 4)
	ctx := context.Background()

	nodeB := poolsOver(t, drives)
	nodeA := poolsOver(t, drives, WithCacheNotifier(func(bucket, key string) {
		nodeB.ApplyRemoteCache(bucket, key)
	}))

	if err := nodeA.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	putBody(t, nodeA, "b", "x", healBytes(4<<10))
	putBody(t, nodeA, "b", "y", healBytes(4<<10))

	if got := listKeys(t, nodeB, "b"); !slices.Equal(got, []string{"x", "y"}) {
		t.Fatalf("nodeB initial list = %v, want [x y]", got)
	}

	if _, err := nodeA.DeleteObject(ctx, "b", "x", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if got := listKeys(t, nodeB, "b"); !slices.Equal(got, []string{"y"}) {
		t.Fatalf("nodeB after coherent delete = %v, want [y]", got)
	}
}

// TestApplyRemoteCacheIsNoOpForUncachedBucket confirms applying an event to a
// bucket a node never listed does not fabricate a cache entry: the next list
// still walks the drives.
func TestApplyRemoteCacheIsNoOpForUncachedBucket(t *testing.T) {
	drives := sharedDrives(t, 4)
	sp := poolsOver(t, drives)
	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	putBody(t, sp, "b", "real", healBytes(2<<10))

	// The bucket was never listed, so it is uncached; a phantom add must not seed
	// a one-key cache that would hide the real object.
	sp.ApplyRemoteCache("b", "phantom")
	if got := listKeys(t, sp, "b"); !slices.Equal(got, []string{"real"}) {
		t.Fatalf("list after phantom add = %v, want [real] (walk, not phantom cache)", got)
	}
}
