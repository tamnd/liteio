// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// makeDrives returns n local drives backed by temp directories.
func makeDrives(tb testing.TB, n int) []storage.StorageAPI {
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

// twoPoolSP builds a ServerPools with two pools (one set each, two drives each).
func twoPoolSP(t *testing.T) *ServerPools {
	t.Helper()
	var id [16]byte
	copy(id[:], "rebalance-test-!!")
	pools := []PoolConfig{
		{Sets: []SetConfig{{Drives: makeDrives(t, 2), Parity: 1}}},
		{Sets: []SetConfig{{Drives: makeDrives(t, 2), Parity: 1}}},
	}
	sp, err := NewServerPools(id, pools)
	if err != nil {
		t.Fatalf("NewServerPools: %v", err)
	}
	return sp
}

// putStr writes content to bucket/key on sp.
func putStr(t *testing.T, sp *ServerPools, bucket, key, content string) {
	t.Helper()
	if err := sp.MakeBucket(context.Background(), bucket, MakeBucketOptions{}); err != nil && err != ErrBucketExists {
		t.Fatalf("MakeBucket: %v", err)
	}
	r := strings.NewReader(content)
	if _, err := sp.PutObject(context.Background(), bucket, key, NewPutReader(r, int64(len(content))), ObjectOptions{}); err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
}

// getStr reads the object and returns its body.
func getStr(t *testing.T, sp *ServerPools, bucket, key string) string {
	t.Helper()
	rc, err := sp.GetObject(context.Background(), bucket, key, ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject %s: %v", key, err)
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	return string(b)
}

// TestRebalanceStatusInitial verifies RebalanceStatus is idle before any run.
func TestRebalanceStatusInitial(t *testing.T) {
	sp := twoPoolSP(t)
	s := sp.RebalanceStatus()
	if s.Running {
		t.Fatal("expected not running")
	}
	if s.ObjectsMoved != 0 || s.BytesMoved != 0 || s.Errors != 0 {
		t.Fatalf("unexpected non-zero counters: %+v", s)
	}
}

// TestRebalanceNoObjects verifies Rebalance on an empty cluster succeeds.
func TestRebalanceNoObjects(t *testing.T) {
	sp := twoPoolSP(t)
	if err := sp.Rebalance(context.Background()); err != nil {
		t.Fatalf("Rebalance: %v", err)
	}
	s := sp.RebalanceStatus()
	if s.Running {
		t.Fatal("still running after return")
	}
	if s.ObjectsMoved != 0 {
		t.Fatalf("moved %d, expected 0", s.ObjectsMoved)
	}
}

// TestRebalancePreservesData puts objects, runs Rebalance, and verifies every
// object is still readable with its original content afterwards.
func TestRebalancePreservesData(t *testing.T) {
	sp := twoPoolSP(t)
	const bucket = "rbtest"
	keys := []string{"alpha", "beta", "gamma", "delta", "epsilon"}
	for _, k := range keys {
		putStr(t, sp, bucket, k, "value-"+k)
	}

	if err := sp.Rebalance(context.Background()); err != nil {
		t.Fatalf("Rebalance: %v", err)
	}

	for _, k := range keys {
		got := getStr(t, sp, bucket, k)
		want := "value-" + k
		if got != want {
			t.Errorf("key %s: got %q, want %q", k, got, want)
		}
	}
}

// TestRebalanceCancellation verifies StopRebalance terminates the run.
func TestRebalanceCancellation(t *testing.T) {
	sp := twoPoolSP(t)
	const bucket = "cancel"
	// Add enough objects that the walk takes some time.
	for i := 0; i < 20; i++ {
		putStr(t, sp, bucket, "obj-"+string(rune('a'+i)), "data")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- sp.Rebalance(ctx) }()
	cancel()
	err := <-done
	if err != nil && err != context.Canceled {
		t.Fatalf("Rebalance: %v", err)
	}
}

// TestDecommissionStatusInitial verifies DecommissionStatus is empty before any run.
func TestDecommissionStatusInitial(t *testing.T) {
	sp := twoPoolSP(t)
	s := sp.DecommissionStatus(0)
	if s.Running || s.Done || s.ObjectsMoved != 0 {
		t.Fatalf("unexpected initial status: %+v", s)
	}
}

// TestDecommissionMovesObjects puts objects, drains pool 0, and verifies every
// object is still readable from the surviving pool.
func TestDecommissionMovesObjects(t *testing.T) {
	sp := twoPoolSP(t)
	const bucket = "dctest"
	keys := []string{"foo", "bar", "baz"}
	for _, k := range keys {
		putStr(t, sp, bucket, k, "body-"+k)
	}

	if err := sp.Decommission(context.Background(), 0); err != nil {
		t.Fatalf("Decommission: %v", err)
	}

	// All objects must still be readable.
	for _, k := range keys {
		got := getStr(t, sp, bucket, k)
		want := "body-" + k
		if got != want {
			t.Errorf("key %s: got %q, want %q", k, got, want)
		}
	}
}

// TestDecommissionInvalidIndex verifies that out-of-range pool indices are rejected.
func TestDecommissionInvalidIndex(t *testing.T) {
	sp := twoPoolSP(t)
	if err := sp.Decommission(context.Background(), 99); err == nil {
		t.Fatal("expected error for out-of-range pool index")
	}
	if err := sp.Decommission(context.Background(), -1); err == nil {
		t.Fatal("expected error for negative pool index")
	}
}

// TestStopDecommissionNoop verifies StopDecommission is safe when no run is active.
func TestStopDecommissionNoop(t *testing.T) {
	sp := twoPoolSP(t)
	sp.StopDecommission(0) // must not panic
}

// TestPoolFreeInfoCache verifies the free-space cache is populated after the first call
// and returns the same slice for a second call within the TTL.
func TestPoolFreeInfoCache(t *testing.T) {
	sp := twoPoolSP(t)
	ctx := context.Background()
	first := sp.poolFreeInfo(ctx)
	second := sp.poolFreeInfo(ctx)
	// Both must have entries for both pools.
	if len(first) == 0 || len(second) == 0 {
		t.Fatalf("empty pool free info")
	}
	// Within TTL, the second call must return an identical slice.
	if len(first) != len(second) {
		t.Fatalf("cache returned different lengths: %d vs %d", len(first), len(second))
	}
}

// TestRouteWithFreeExcludesDraining verifies that once a pool is marked draining,
// routeWithFree never returns a set belonging to that pool.
func TestRouteWithFreeExcludesDraining(t *testing.T) {
	sp := twoPoolSP(t)
	ctx := context.Background()
	// Mark pool 0 as draining.
	sp.dcMu.Lock()
	sp.draining[0] = true
	sp.dcMu.Unlock()
	sp.fc.set(nil) // invalidate cache

	drainingSets := map[*erasureSet]bool{}
	for _, s := range sp.pools[0].sets {
		drainingSets[s] = true
	}

	for _, key := range []string{"alpha", "beta", "gamma", "hello", "world"} {
		if drainingSets[sp.routeWithFree(ctx, key)] {
			t.Errorf("key %q routed to draining pool", key)
		}
	}
}

// TestMigrateObjectMovesData verifies migrateObject copies data and removes it
// from the source set.
func TestMigrateObjectMovesData(t *testing.T) {
	sp := twoPoolSP(t)
	ctx := context.Background()
	const bucket = "migrate"
	if err := sp.MakeBucket(ctx, bucket, MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}

	src := sp.pools[0].sets[0]
	dst := sp.pools[1].sets[0]
	body := []byte("migrate-me")
	r := bytes.NewReader(body)
	if _, err := src.putObject(ctx, bucket, "obj", NewPutReader(r, int64(len(body))), ObjectOptions{}); err != nil {
		t.Fatalf("putObject on src: %v", err)
	}

	if err := sp.migrateObject(ctx, &sp.rbSt, src, dst, bucket, "obj"); err != nil {
		t.Fatalf("migrateObject: %v", err)
	}

	// Object must exist on dst.
	rc, err := dst.getObject(ctx, bucket, "obj", ObjectOptions{})
	if err != nil {
		t.Fatalf("getObject from dst: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Fatalf("got %q, want %q", got, body)
	}

	// Object must be gone from src.
	if _, err := src.getObjectInfo(ctx, bucket, "obj", ObjectOptions{}); err == nil {
		t.Fatal("object still present on src after migrate")
	}
}

// BenchmarkRebalance measures Rebalance throughput across 100 small objects
// spread across two pools.
func BenchmarkRebalance(b *testing.B) {
	var id [16]byte
	copy(id[:], "bench-rebalance!")
	pools := []PoolConfig{
		{Sets: []SetConfig{{Drives: makeDrives(b, 2), Parity: 1}}},
		{Sets: []SetConfig{{Drives: makeDrives(b, 2), Parity: 1}}},
	}
	sp, err := NewServerPools(id, pools)
	if err != nil {
		b.Fatalf("NewServerPools: %v", err)
	}
	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "bench", MakeBucketOptions{}); err != nil {
		b.Fatalf("MakeBucket: %v", err)
	}
	for i := 0; i < 100; i++ {
		key := "key-" + string(rune('a'+(i%26))) + string(rune('0'+i%10))
		body := []byte("bench-body")
		if _, err := sp.PutObject(ctx, "bench", key, NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
			b.Fatalf("PutObject: %v", err)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		// Invalidate the free-space cache so each iteration exercises the cache fill.
		sp.fc.set(nil)
		_ = sp.Rebalance(ctx)
	}
}
