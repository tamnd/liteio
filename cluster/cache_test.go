// SPDX-License-Identifier: Apache-2.0

package cluster_test

import (
	"context"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/tamnd/liteio/cluster"
	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// recordingSink captures the cache events a node receives over RPC.
type recordingSink struct {
	mu     sync.Mutex
	events []string
}

func (r *recordingSink) ApplyRemoteCache(bucket, key string) {
	r.mu.Lock()
	r.events = append(r.events, bucket+"|"+key)
	r.mu.Unlock()
}

func (r *recordingSink) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

// TestServerServesCacheEndpoint drives the coherence wire path: a node serves a
// CacheSink at CachePath, and a RemoteCache reaches it to apply an add and an
// invalidation.
func TestServerServesCacheEndpoint(t *testing.T) {
	sink := &recordingSink{}
	l, _ := local.New(t.TempDir())
	srv, err := cluster.NewServer(map[string]storage.StorageAPI{"/drive0": l}, lock.NewLocalLocker("node-a"), sink)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	rc := cluster.NewRemoteCache(hs.URL, hs.Client())
	ctx := context.Background()
	if err := rc.Event(ctx, "photos", "cat.jpg"); err != nil {
		t.Fatalf("Event add: %v", err)
	}
	if err := rc.Event(ctx, "photos", ""); err != nil {
		t.Fatalf("Event invalidate: %v", err)
	}

	want := []string{"photos|cat.jpg", "photos|"}
	if got := sink.snapshot(); len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("sink events = %v, want %v", got, want)
	}
}

// TestCacheBroadcasterFansToPeers checks the notifier CacheBroadcaster builds
// delivers a node's events to every peer, off the caller's goroutine.
func TestCacheBroadcasterFansToPeers(t *testing.T) {
	mk := func(name string) (*recordingSink, string, func()) {
		sink := &recordingSink{}
		l, _ := local.New(t.TempDir())
		srv, err := cluster.NewServer(map[string]storage.StorageAPI{"/d": l}, lock.NewLocalLocker(name), sink)
		if err != nil {
			t.Fatalf("NewServer %s: %v", name, err)
		}
		hs := httptest.NewServer(srv.Handler())
		return sink, hs.URL, hs.Close
	}
	s1, u1, c1 := mk("peer-1")
	t.Cleanup(c1)
	s2, u2, c2 := mk("peer-2")
	t.Cleanup(c2)

	notify := cluster.CacheBroadcaster([]string{u1, u2}, nil)
	if notify == nil {
		t.Fatal("CacheBroadcaster returned nil for a non-empty peer list")
	}
	notify("b", "k")

	// Delivery is asynchronous; both peers must receive the event shortly.
	deadline := time.Now().Add(3 * time.Second)
	for len(s1.snapshot()) != 1 || len(s2.snapshot()) != 1 {
		if time.Now().After(deadline) {
			t.Fatalf("broadcast not delivered: peer1=%v peer2=%v", s1.snapshot(), s2.snapshot())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if got := s1.snapshot(); got[0] != "b|k" {
		t.Fatalf("peer1 event = %v, want [b|k]", got)
	}
	if got := s2.snapshot(); got[0] != "b|k" {
		t.Fatalf("peer2 event = %v, want [b|k]", got)
	}
}

// TestCacheBroadcasterNilWithoutPeers confirms a node with no peers installs no
// notifier, leaving the layer free of cross-node chatter.
func TestCacheBroadcasterNilWithoutPeers(t *testing.T) {
	if notify := cluster.CacheBroadcaster(nil, nil); notify != nil {
		t.Fatal("CacheBroadcaster must return nil when there are no peers")
	}
}

// BenchmarkCacheBroadcastDispatch measures the cost a write pays to fan a
// coherence event to a peer: the dispatch is non-blocking, so the actual RPC
// happens off this goroutine. It confirms the write path is charged only the
// cheap hand-off, not the round trip.
func BenchmarkCacheBroadcastDispatch(b *testing.B) {
	sink := &recordingSink{}
	l, _ := local.New(b.TempDir())
	srv, err := cluster.NewServer(map[string]storage.StorageAPI{"/d": l}, lock.NewLocalLocker("peer"), sink)
	if err != nil {
		b.Fatalf("NewServer: %v", err)
	}
	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()

	notify := cluster.CacheBroadcaster([]string{hs.URL}, hs.Client())
	for b.Loop() {
		notify("bucket", "key")
	}
}

// TestNewServerRejectsCachePathDrive confirms a drive endpoint may not collide
// with the reserved cache path.
func TestNewServerRejectsCachePathDrive(t *testing.T) {
	l, _ := local.New(t.TempDir())
	bad := map[string]storage.StorageAPI{cluster.CachePath: l}
	if _, err := cluster.NewServer(bad, lock.NewLocalLocker("n"), &recordingSink{}); err == nil {
		t.Fatal("a drive path colliding with the cache endpoint must be rejected")
	}
}
