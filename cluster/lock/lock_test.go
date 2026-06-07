// SPDX-License-Identifier: Apache-2.0

package lock_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/cluster/rpc"
)

func ctx() context.Context { return context.Background() }

// fakeClock is a manually-advanced clock for driving lease expiry without
// sleeping. It is mutex-guarded so SetClock's reader and the test's advance never
// race under -race.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Unix(1_700_000_000, 0)}
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// --- LocalLocker --------------------------------------------------------------

func TestWriteLockIsExclusive(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	a1 := lock.Args{UID: "u1", Resources: []string{"bucket/key"}}
	a2 := lock.Args{UID: "u2", Resources: []string{"bucket/key"}}

	if ok, _ := l.Lock(ctx(), a1); !ok {
		t.Fatal("first write lock should be granted")
	}
	if ok, _ := l.Lock(ctx(), a2); ok {
		t.Fatal("second write lock on a held key must be denied")
	}
	if ok, _ := l.RLock(ctx(), a2); ok {
		t.Fatal("read lock on a write-held key must be denied")
	}
	if ok, _ := l.Unlock(ctx(), a1); !ok {
		t.Fatal("unlock should find the held lock")
	}
	if ok, _ := l.Lock(ctx(), a2); !ok {
		t.Fatal("write lock should be grantable after release")
	}
}

func TestReadLocksAreShared(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	r := []string{"bucket/key"}
	if ok, _ := l.RLock(ctx(), lock.Args{UID: "r1", Resources: r}); !ok {
		t.Fatal("first read lock denied")
	}
	if ok, _ := l.RLock(ctx(), lock.Args{UID: "r2", Resources: r}); !ok {
		t.Fatal("second read lock should share")
	}
	// A writer is excluded while any reader holds.
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "w1", Resources: r}); ok {
		t.Fatal("write lock must be denied while readers hold")
	}
	// Release both readers, then the writer fits.
	_, _ = l.RUnlock(ctx(), lock.Args{UID: "r1", Resources: r})
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "w1", Resources: r}); ok {
		t.Fatal("write lock must still be denied with one reader left")
	}
	_, _ = l.RUnlock(ctx(), lock.Args{UID: "r2", Resources: r})
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "w1", Resources: r}); !ok {
		t.Fatal("write lock should be granted once all readers release")
	}
}

func TestMultiResourceIsAllOrNothing(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u1", Resources: []string{"b/k2"}}); !ok {
		t.Fatal("setup lock denied")
	}
	// A batch that includes the already-held k2 must be denied entirely, leaving
	// k1 and k3 free.
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k1", "b/k2", "b/k3"}}); ok {
		t.Fatal("batch overlapping a held key must be denied")
	}
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u3", Resources: []string{"b/k1"}}); !ok {
		t.Fatal("k1 must be free after a denied batch (no partial grant)")
	}
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u4", Resources: []string{"b/k3"}}); !ok {
		t.Fatal("k3 must be free after a denied batch")
	}
}

func TestUnlockKindMustMatch(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	a := lock.Args{UID: "u1", Resources: []string{"b/k"}}
	_, _ = l.Lock(ctx(), a)
	// An RUnlock must not release a write hold.
	if ok, _ := l.RUnlock(ctx(), a); ok {
		t.Fatal("RUnlock should not find a write hold")
	}
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k"}}); ok {
		t.Fatal("write hold must survive a mismatched RUnlock")
	}
}

func TestLeaseExpiryReclaimsLock(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	clock := newFakeClock()
	lock.SetClock(l, clock.now)

	a := lock.Args{UID: "u1", Resources: []string{"b/k"}}
	if ok, _ := l.Lock(ctx(), a); !ok {
		t.Fatal("initial lock denied")
	}
	// Before the lease elapses, the key is still held.
	clock.advance(lock.DefaultLease - time.Second)
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k"}}); ok {
		t.Fatal("lock must still be held before its lease elapses")
	}
	// Past the lease, a crashed holder's lock is reclaimed.
	clock.advance(2 * time.Second)
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u3", Resources: []string{"b/k"}}); !ok {
		t.Fatal("expired lock must be reclaimable")
	}
}

func TestRefreshKeepsLockAlive(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	clock := newFakeClock()
	lock.SetClock(l, clock.now)

	a := lock.Args{UID: "u1", Resources: []string{"b/k"}}
	_, _ = l.Lock(ctx(), a)
	clock.advance(lock.DefaultLease - time.Second)
	if ok, _ := l.Refresh(ctx(), a); !ok {
		t.Fatal("refresh should find a live hold")
	}
	// The refresh reset the lease, so after another near-lease span it still holds.
	clock.advance(lock.DefaultLease - time.Second)
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k"}}); ok {
		t.Fatal("refreshed lock must still be held")
	}
	// A refresh of an expired/unknown UID reports not-found.
	clock.advance(2 * lock.DefaultLease)
	if ok, _ := l.Refresh(ctx(), a); ok {
		t.Fatal("refresh of an expired hold should report not-found")
	}
}

func TestForceUnlock(t *testing.T) {
	l := lock.NewLocalLocker("n0")
	a := lock.Args{UID: "u1", Resources: []string{"b/k"}}
	_, _ = l.Lock(ctx(), a)
	if ok, _ := l.ForceUnlock(ctx(), lock.Args{Resources: []string{"b/k"}}); !ok {
		t.Fatal("force unlock should report done")
	}
	if ok, _ := l.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k"}}); !ok {
		t.Fatal("key must be free after a force unlock")
	}
}

// --- DRWMutex over a quorum ---------------------------------------------------

// newCluster returns n LocalLockers wrapped as a []lock.Locker.
func newCluster(n int) []lock.Locker {
	ls := make([]lock.Locker, n)
	for i := range n {
		ls[i] = lock.NewLocalLocker(fmt.Sprintf("n%d", i))
	}
	return ls
}

func TestDistributedMutualExclusion(t *testing.T) {
	ls := newCluster(3)
	m1 := lock.NewDRWMutex("owner", ls, "b/k")
	m2 := lock.NewDRWMutex("owner", ls, "b/k")

	if !m1.GetLock(ctx(), "t1") {
		t.Fatal("m1 should acquire the write lock")
	}
	// m2 cannot reach quorum while m1 holds it; a short deadline forces a quick fail.
	c, cancel := context.WithTimeout(ctx(), 200*time.Millisecond)
	defer cancel()
	if m2.GetLock(c, "t2") {
		m2.Unlock()
		t.Fatal("m2 must not acquire while m1 holds the write lock")
	}
	m1.Unlock()
	if !m2.GetLock(ctx(), "t2") {
		t.Fatal("m2 should acquire once m1 releases")
	}
	m2.Unlock()
}

func TestDistributedSharedReads(t *testing.T) {
	ls := newCluster(3)
	r1 := lock.NewDRWMutex("owner", ls, "b/k")
	r2 := lock.NewDRWMutex("owner", ls, "b/k")
	if !r1.GetRLock(ctx(), "r1") || !r2.GetRLock(ctx(), "r2") {
		t.Fatal("two readers should both acquire a shared lock")
	}
	w := lock.NewDRWMutex("owner", ls, "b/k")
	c, cancel := context.WithTimeout(ctx(), 200*time.Millisecond)
	defer cancel()
	if w.GetLock(c, "w") {
		w.Unlock()
		t.Fatal("a writer must be excluded while readers hold the lock")
	}
	r1.RUnlock()
	r2.RUnlock()
	if !w.GetLock(ctx(), "w") {
		t.Fatal("writer should acquire once readers release")
	}
	w.Unlock()
}

func TestQuorumToleratesMinorityFailure(t *testing.T) {
	// 2 healthy local lockers + 1 dead remote: a majority (2 of 3) still grants.
	dead := lock.NewRemoteLocker("http://127.0.0.1:1", nil)
	ls := []lock.Locker{lock.NewLocalLocker("n0"), lock.NewLocalLocker("n1"), dead}
	m := lock.NewDRWMutex("owner", ls, "b/k")
	c, cancel := context.WithTimeout(ctx(), 2*time.Second)
	defer cancel()
	if !m.GetLock(c, "t") {
		t.Fatal("a lock must be acquirable with a minority of servers down")
	}
	m.Unlock()
}

func TestConcurrentWritersSerialize(t *testing.T) {
	ls := newCluster(3)
	const goroutines = 8
	var inCrit atomic.Int32
	var maxSeen atomic.Int32
	var wg sync.WaitGroup
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			m := lock.NewDRWMutex("owner", ls, "b/hot")
			if !m.GetLock(ctx(), fmt.Sprintf("g%d", i)) {
				t.Errorf("goroutine %d failed to acquire", i)
				return
			}
			n := inCrit.Add(1)
			for {
				old := maxSeen.Load()
				if n <= old || maxSeen.CompareAndSwap(old, n) {
					break
				}
			}
			time.Sleep(time.Millisecond)
			inCrit.Add(-1)
			m.Unlock()
		}(i)
	}
	wg.Wait()
	if got := maxSeen.Load(); got != 1 {
		t.Fatalf("max concurrent holders = %d, want 1 (mutual exclusion violated)", got)
	}
}

// --- RemoteLocker over cluster/rpc --------------------------------------------

// newRemoteCluster builds n lock servers, each a LocalLocker exposed over its own
// httptest RPC server, and returns RemoteLocker clients for them.
func newRemoteCluster(t testing.TB, n int) []lock.Locker {
	t.Helper()
	ls := make([]lock.Locker, n)
	for i := range n {
		mux := rpc.NewMux()
		lock.Register(mux, lock.NewLocalLocker(fmt.Sprintf("n%d", i)))
		srv := httptest.NewServer(mux)
		t.Cleanup(srv.Close)
		ls[i] = lock.NewRemoteLocker(srv.URL, nil)
	}
	return ls
}

func TestRemoteLockerRoundTrip(t *testing.T) {
	ls := newRemoteCluster(t, 1)
	r := ls[0]
	a := lock.Args{UID: "u1", Resources: []string{"b/k"}, Owner: "node-a"}
	if ok, err := r.Lock(ctx(), a); err != nil || !ok {
		t.Fatalf("remote Lock = %v, %v; want true, nil", ok, err)
	}
	if ok, err := r.Lock(ctx(), lock.Args{UID: "u2", Resources: []string{"b/k"}}); err != nil || ok {
		t.Fatalf("remote second Lock = %v, %v; want false, nil", ok, err)
	}
	if ok, err := r.Unlock(ctx(), a); err != nil || !ok {
		t.Fatalf("remote Unlock = %v, %v; want true, nil", ok, err)
	}
	if !r.IsOnline() {
		t.Fatal("a reachable remote locker should be online")
	}
}

func TestRemoteLockerOffline(t *testing.T) {
	dead := lock.NewRemoteLocker("http://127.0.0.1:1", nil)
	if dead.IsOnline() {
		t.Fatal("an unreachable remote locker should report offline")
	}
}

func TestDistributedMutualExclusionOverRPC(t *testing.T) {
	ls := newRemoteCluster(t, 3)
	m1 := lock.NewDRWMutex("owner", ls, "b/k")
	m2 := lock.NewDRWMutex("owner", ls, "b/k")
	if !m1.GetLock(ctx(), "t1") {
		t.Fatal("m1 should acquire over RPC")
	}
	c, cancel := context.WithTimeout(ctx(), 300*time.Millisecond)
	defer cancel()
	if m2.GetLock(c, "t2") {
		m2.Unlock()
		t.Fatal("m2 must be excluded over RPC while m1 holds")
	}
	m1.Unlock()
	if !m2.GetLock(ctx(), "t2") {
		t.Fatal("m2 should acquire over RPC once m1 releases")
	}
	m2.Unlock()
}

// --- benchmarks ---------------------------------------------------------------

func BenchmarkLocalLockUnlock(b *testing.B) {
	l := lock.NewLocalLocker("n0")
	a := lock.Args{UID: "u", Resources: []string{"b/k"}}
	b.ReportAllocs()
	for b.Loop() {
		_, _ = l.Lock(ctx(), a)
		_, _ = l.Unlock(ctx(), a)
	}
}

func BenchmarkDistributedLockUnlock(b *testing.B) {
	ls := newCluster(3)
	b.ReportAllocs()
	for b.Loop() {
		m := lock.NewDRWMutex("owner", ls, "b/k")
		if !m.GetLock(ctx(), "bench") {
			b.Fatal("acquire failed")
		}
		m.Unlock()
	}
}
