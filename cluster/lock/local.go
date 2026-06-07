// SPDX-License-Identifier: Apache-2.0

// Package lock is liteio's distributed read/write lock service (spec 2020, doc
// 5.2): the dsync analog that coordinates mutating operations on the same
// namespace key across nodes without etcd, ZooKeeper, or any external consensus
// system. It has three layers that mirror the storage stack: a Locker contract
// (the lock server interface), a LocalLocker that holds locks in process, and a
// quorum DRWMutex that holds a logical lock against a majority of Lockers — some
// local, some reached over cluster/rpc — so a minority of node failures cannot
// break mutual exclusion. A lock that the holder stops refreshing expires on its
// lease, so a crashed node never wedges a key forever.
package lock

import (
	"context"
	"sync"
	"time"
)

// DefaultLease is how long a granted hold survives without a refresh. It is sized
// so a normal mutating operation finishes well inside it while a crashed holder's
// lock is reclaimed promptly.
const DefaultLease = 30 * time.Second

// Args identifies one lock acquisition. A single Args may name several Resources,
// which a Locker grants or denies all-or-nothing so a batch operation locks its
// keys atomically on each server. UID is unique per acquisition attempt and ties
// an Unlock or Refresh back to the hold it created.
type Args struct {
	UID       string   `msgpack:"u"`
	Resources []string `msgpack:"r"`
	Owner     string   `msgpack:"o"` // node identity, for diagnostics and ForceUnlock
	Source    string   `msgpack:"s"` // caller site, for diagnostics
}

// Locker is one lock server: the contract a DRWMutex holds a quorum of. A
// LocalLocker serves in process; a remote locker (see remote.go) forwards each
// call to a peer over cluster/rpc. Lock/RLock report whether the grant was made;
// Unlock/RUnlock/Refresh report whether a matching hold was found.
type Locker interface {
	Lock(ctx context.Context, args Args) (bool, error)
	Unlock(ctx context.Context, args Args) (bool, error)
	RLock(ctx context.Context, args Args) (bool, error)
	RUnlock(ctx context.Context, args Args) (bool, error)
	Refresh(ctx context.Context, args Args) (bool, error)
	ForceUnlock(ctx context.Context, args Args) (bool, error)
	String() string
	IsOnline() bool
}

// holder is one grant on one resource: a writer (exclusive) or a reader (shared),
// identified by the acquisition UID and valid until expiry.
type holder struct {
	uid    string
	owner  string
	writer bool
	expiry time.Time
}

// LocalLocker serves locks from an in-process table keyed by resource name. It is
// the lock server every node runs for the resources it hosts, and the leaf a
// remote locker's handlers call. The clock is injectable so lease expiry is
// testable without sleeping.
type LocalLocker struct {
	mu    sync.Mutex
	locks map[string][]holder
	lease time.Duration
	now   func() time.Time
	name  string
}

// NewLocalLocker returns a LocalLocker with the default lease. name is reported by
// String() to identify this server in a DRWMutex's locker set.
func NewLocalLocker(name string) *LocalLocker {
	return &LocalLocker{
		locks: map[string][]holder{},
		lease: DefaultLease,
		now:   time.Now,
		name:  name,
	}
}

// String identifies the server (its name).
func (l *LocalLocker) String() string { return l.name }

// IsOnline is always true for an in-process locker.
func (l *LocalLocker) IsOnline() bool { return true }

// reapLocked drops expired holders from one resource. The caller holds l.mu. A
// fresh acquisition sweeps the resources it touches, so a crashed holder's grant
// is reclaimed the next time someone contends for the same key.
func (l *LocalLocker) reapLocked(resource string, now time.Time) []holder {
	hs := l.locks[resource]
	live := hs[:0]
	for _, h := range hs {
		if h.expiry.After(now) {
			live = append(live, h)
		}
	}
	if len(live) == 0 {
		delete(l.locks, resource)
		return nil
	}
	l.locks[resource] = live
	return live
}

// canGrant reports whether a write (exclusive) or read (shared) hold can be added
// to resource given its live holders. The caller holds l.mu.
func (l *LocalLocker) canGrant(resource string, writer bool, now time.Time) bool {
	for _, h := range l.reapLocked(resource, now) {
		if writer || h.writer {
			// A writer conflicts with anyone; a reader conflicts with a writer.
			return false
		}
	}
	return true
}

// acquire grants writer or shared holds on all of args.Resources, or none. It is
// the shared body of Lock and RLock.
func (l *LocalLocker) acquire(args Args, writer bool) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	for _, r := range args.Resources {
		if !l.canGrant(r, writer, now) {
			return false, nil
		}
	}
	exp := now.Add(l.lease)
	for _, r := range args.Resources {
		l.locks[r] = append(l.locks[r], holder{uid: args.UID, owner: args.Owner, writer: writer, expiry: exp})
	}
	return true, nil
}

// release drops holds created by args.UID on all of args.Resources. writer selects
// which kind to remove so an Unlock cannot release a read hold or vice versa. It
// reports whether any matching hold was found.
func (l *LocalLocker) release(args Args, writer bool) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	found := false
	for _, r := range args.Resources {
		hs := l.locks[r]
		kept := hs[:0]
		for _, h := range hs {
			if h.uid == args.UID && h.writer == writer {
				found = true
				continue
			}
			kept = append(kept, h)
		}
		if len(kept) == 0 {
			delete(l.locks, r)
		} else {
			l.locks[r] = kept
		}
	}
	return found, nil
}

// Lock acquires an exclusive hold on every resource, all-or-nothing.
func (l *LocalLocker) Lock(_ context.Context, args Args) (bool, error) {
	return l.acquire(args, true)
}

// RLock acquires a shared hold on every resource, all-or-nothing.
func (l *LocalLocker) RLock(_ context.Context, args Args) (bool, error) {
	return l.acquire(args, false)
}

// Unlock releases the exclusive holds created by args.UID.
func (l *LocalLocker) Unlock(_ context.Context, args Args) (bool, error) {
	return l.release(args, true)
}

// RUnlock releases the shared holds created by args.UID.
func (l *LocalLocker) RUnlock(_ context.Context, args Args) (bool, error) {
	return l.release(args, false)
}

// Refresh extends the lease on every hold created by args.UID, reporting whether
// any was found still live. A holder calls this periodically to keep its lock from
// expiring; once it stops, the lease lapses and the lock is reclaimed.
func (l *LocalLocker) Refresh(_ context.Context, args Args) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	exp := now.Add(l.lease)
	found := false
	for _, r := range args.Resources {
		for i := range l.reapLocked(r, now) {
			if l.locks[r][i].uid == args.UID {
				l.locks[r][i].expiry = exp
				found = true
			}
		}
	}
	return found, nil
}

// ForceUnlock drops every hold on the named resources regardless of UID. It is the
// admin escape hatch for a key wedged by a misbehaving holder before its lease
// lapses.
func (l *LocalLocker) ForceUnlock(_ context.Context, args Args) (bool, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, r := range args.Resources {
		delete(l.locks, r)
	}
	return true, nil
}
