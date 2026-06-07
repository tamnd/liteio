// SPDX-License-Identifier: Apache-2.0

package lock

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"sort"
	"sync"
	"time"
)

// DefaultRefresh is how often a held DRWMutex re-extends its lease on the lock
// servers. It must be comfortably shorter than the lease so a transient refresh
// failure does not lose a still-wanted lock.
const DefaultRefresh = 10 * time.Second

// quorum returns the majority of n lock servers. A write and a read each need a
// majority, so any two grants overlap on at least one server, which is what gives
// mutual exclusion across the cluster without a central coordinator.
func quorum(n int) int { return n/2 + 1 }

// newUID returns a random, unguessable acquisition identifier.
func newUID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// DRWMutex is a distributed read/write lock over a fixed set of Lockers. A write
// (GetLock) or read (GetRLock) is held only when a majority of the lockers grant
// it; on failure every partial grant is released before retrying, so the protocol
// holds no lock while waiting and cannot deadlock. While held, a background loop
// refreshes the lease on every locker until Unlock.
type DRWMutex struct {
	Names   []string // the resources, canonicalized to sorted order
	lockers []Locker
	owner   string
	refresh time.Duration

	mu       sync.Mutex
	uid      string
	writer   bool
	cancelRf context.CancelFunc
}

// NewDRWMutex returns a mutex over the given lockers guarding names. Names are
// sorted so every caller acquires a set of resources in the same order, which
// removes lock-ordering deadlocks between batch operations.
func NewDRWMutex(owner string, lockers []Locker, names ...string) *DRWMutex {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	return &DRWMutex{
		Names:   sorted,
		lockers: lockers,
		owner:   owner,
		refresh: DefaultRefresh,
	}
}

// GetLock blocks until it holds the write lock or ctx is done. It returns true on
// success; on ctx expiry it returns false with no lock held.
func (d *DRWMutex) GetLock(ctx context.Context, source string) bool {
	return d.lock(ctx, source, true)
}

// GetRLock blocks until it holds a shared read lock or ctx is done.
func (d *DRWMutex) GetRLock(ctx context.Context, source string) bool {
	return d.lock(ctx, source, false)
}

func (d *DRWMutex) lock(ctx context.Context, source string, writer bool) bool {
	uid := newUID()
	backoff := 10 * time.Millisecond
	for {
		if d.tryLock(ctx, uid, source, writer) {
			d.mu.Lock()
			d.uid, d.writer = uid, writer
			rctx, cancel := context.WithCancel(context.Background())
			d.cancelRf = cancel
			d.mu.Unlock()
			go d.refreshLoop(rctx, uid)
			return true
		}
		// A fresh UID each attempt keeps a partially-granted-then-released
		// attempt from being confused with the next one.
		uid = newUID()
		select {
		case <-ctx.Done():
			return false
		case <-time.After(backoff):
			if backoff < time.Second {
				backoff *= 2
			}
		}
	}
}

// tryLock makes one acquisition attempt across all lockers and reports whether a
// quorum granted. On anything short of quorum it releases every grant it did get,
// so a failed attempt leaves no trace.
func (d *DRWMutex) tryLock(ctx context.Context, uid, source string, writer bool) bool {
	args := Args{UID: uid, Resources: d.Names, Owner: d.owner, Source: source}
	granted := make([]bool, len(d.lockers))
	var wg sync.WaitGroup
	for i, lk := range d.lockers {
		wg.Add(1)
		go func(i int, lk Locker) {
			defer wg.Done()
			var ok bool
			var err error
			if writer {
				ok, err = lk.Lock(ctx, args)
			} else {
				ok, err = lk.RLock(ctx, args)
			}
			granted[i] = err == nil && ok
		}(i, lk)
	}
	wg.Wait()

	count := 0
	for _, g := range granted {
		if g {
			count++
		}
	}
	if count >= quorum(len(d.lockers)) {
		return true
	}
	d.releaseGranted(args, writer, granted)
	return false
}

// releaseGranted unlocks exactly the lockers that granted, best-effort.
func (d *DRWMutex) releaseGranted(args Args, writer bool, granted []bool) {
	var wg sync.WaitGroup
	for i, lk := range d.lockers {
		if !granted[i] {
			continue
		}
		wg.Add(1)
		go func(lk Locker) {
			defer wg.Done()
			if writer {
				_, _ = lk.Unlock(context.Background(), args)
			} else {
				_, _ = lk.RUnlock(context.Background(), args)
			}
		}(lk)
	}
	wg.Wait()
}

// refreshLoop keeps the held lock's lease alive on every locker until the lock is
// released. Refresh is best-effort per server; the lease on the servers is the
// real backstop, so a missed tick does not by itself drop the lock.
func (d *DRWMutex) refreshLoop(ctx context.Context, uid string) {
	t := time.NewTicker(d.refresh)
	defer t.Stop()
	args := Args{UID: uid, Resources: d.Names, Owner: d.owner}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			for _, lk := range d.lockers {
				_, _ = lk.Refresh(ctx, args)
			}
		}
	}
}

// Unlock releases the write lock on every locker. It is safe to call once per
// successful GetLock.
func (d *DRWMutex) Unlock() { d.unlock(true) }

// RUnlock releases the read lock on every locker.
func (d *DRWMutex) RUnlock() { d.unlock(false) }

func (d *DRWMutex) unlock(writer bool) {
	d.mu.Lock()
	uid := d.uid
	cancel := d.cancelRf
	d.uid, d.cancelRf = "", nil
	d.mu.Unlock()
	if uid == "" {
		return
	}
	if cancel != nil {
		cancel()
	}
	args := Args{UID: uid, Resources: d.Names, Owner: d.owner}
	var wg sync.WaitGroup
	for _, lk := range d.lockers {
		wg.Add(1)
		go func(lk Locker) {
			defer wg.Done()
			if writer {
				_, _ = lk.Unlock(context.Background(), args)
			} else {
				_, _ = lk.RUnlock(context.Background(), args)
			}
		}(lk)
	}
	wg.Wait()
}
