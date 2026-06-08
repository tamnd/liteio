// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tamnd/liteio/object/placement"
)

// RebalanceStatus is a snapshot of a rebalance run.
type RebalanceStatus struct {
	Running      bool
	ObjectsMoved int64
	BytesMoved   int64
	Errors       int64
}

// DecommissionStatus is a snapshot of a pool-decommission run.
type DecommissionStatus struct {
	PoolIndex    int
	Running      bool
	Done         bool
	ObjectsMoved int64
	BytesMoved   int64
	Errors       int64
}

// rbState holds atomic counters for an active rebalance or decommission.
type rbState struct {
	running atomic.Bool
	moved   atomic.Int64
	bytes   atomic.Int64
	errors  atomic.Int64
	cancel  context.CancelFunc // nil when not running
	mu      sync.Mutex         // guards cancel
}

func (s *rbState) stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
}

// poolFreeCache caches per-pool free-space info so route() pays one DiskUsage
// fan-out per TTL period instead of one per request.
type poolFreeCache struct {
	mu  sync.RWMutex
	at  time.Time
	ttl time.Duration
	inf []placement.PoolFreeInfo
}

func newPoolFreeCache() *poolFreeCache { return &poolFreeCache{ttl: 30 * time.Second} }

// get returns the cached info; the bool is false when the cache is stale.
func (fc *poolFreeCache) get() ([]placement.PoolFreeInfo, bool) {
	fc.mu.RLock()
	defer fc.mu.RUnlock()
	if fc.inf == nil || time.Since(fc.at) > fc.ttl {
		return nil, false
	}
	return fc.inf, true
}

// set replaces the cached info. A nil or empty inf invalidates the cache.
func (fc *poolFreeCache) set(inf []placement.PoolFreeInfo) {
	fc.mu.Lock()
	defer fc.mu.Unlock()
	if len(inf) == 0 {
		fc.inf = nil
		return
	}
	fc.inf = inf
	fc.at = time.Now()
}

// poolFreeInfo reads current free space from all pools (one DiskUsage call) and
// caches the result. It aggregates per-set figures up to per-pool totals and
// excludes draining pools from the routing slice.
func (sp *ServerPools) poolFreeInfo(ctx context.Context) []placement.PoolFreeInfo {
	if info, ok := sp.fc.get(); ok {
		return info
	}
	usage := sp.DiskUsage(ctx)
	// Aggregate set-level free bytes into pool-level totals. DiskUsage.Sets is
	// ordered sets-across-pools in the same pool × set traversal order as allSets.
	setIdx := 0
	pfi := make([]placement.PoolFreeInfo, len(sp.pools))
	for i, p := range sp.pools {
		pfi[i] = placement.PoolFreeInfo{Index: i, Free: 1}
		var free uint64
		for range p.sets {
			if setIdx < len(usage.Sets) {
				free += usage.Sets[setIdx].RawFree
				setIdx++
			}
		}
		if free > 0 {
			pfi[i].Free = free
		}
	}
	// Remove draining pools so they receive no new writes.
	sp.dcMu.RLock()
	draining := sp.draining
	sp.dcMu.RUnlock()
	if len(draining) > 0 {
		var active []placement.PoolFreeInfo
		for _, p := range pfi {
			if !draining[p.Index] {
				active = append(active, p)
			}
		}
		if len(active) > 0 {
			pfi = active
		}
	}
	sp.fc.set(pfi)
	return pfi
}

// routeWithFree resolves the owning set using real per-pool free-space weights.
// PoolIndex already returns the pool's Index field (its position in sp.pools),
// not a slice offset into pfi, so we use it directly.
func (sp *ServerPools) routeWithFree(ctx context.Context, object string) *erasureSet {
	pfi := sp.poolFreeInfo(ctx)
	poolIdx := placement.PoolIndex(object, pfi)
	if poolIdx < 0 || poolIdx >= len(sp.pools) {
		poolIdx = 0
	}
	p := sp.pools[poolIdx]
	si := placement.SetIndex(object, sp.deploymentID, len(p.sets))
	return p.sets[si]
}

// Rebalance walks every object in the cluster and migrates any whose current set
// differs from the set free-space-weighted placement assigns today. Call it after
// adding a new pool so free-space ratios converge.
func (sp *ServerPools) Rebalance(ctx context.Context) error {
	if !sp.rbSt.running.CompareAndSwap(false, true) {
		return nil // already running
	}
	ctx, cancel := context.WithCancel(ctx)
	sp.rbSt.mu.Lock()
	sp.rbSt.cancel = cancel
	sp.rbSt.mu.Unlock()
	sp.rbSt.moved.Store(0)
	sp.rbSt.bytes.Store(0)
	sp.rbSt.errors.Store(0)
	defer func() {
		sp.rbSt.running.Store(false)
		cancel()
	}()

	buckets, _ := sp.ListBuckets(ctx)
	for _, p := range sp.pools {
		for _, set := range p.sets {
			for _, bi := range buckets {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				keys, err := set.walkObjects(ctx, bi.Name)
				if err != nil {
					continue
				}
				for _, key := range keys {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					target := sp.routeWithFree(ctx, key)
					if target == set {
						continue
					}
					if err := sp.migrateObject(ctx, &sp.rbSt, set, target, bi.Name, key); err != nil {
						sp.rbSt.errors.Add(1)
					}
				}
			}
		}
	}
	return nil
}

// RebalanceStatus returns a snapshot of the running or last-completed rebalance.
func (sp *ServerPools) RebalanceStatus() RebalanceStatus {
	return RebalanceStatus{
		Running:      sp.rbSt.running.Load(),
		ObjectsMoved: sp.rbSt.moved.Load(),
		BytesMoved:   sp.rbSt.bytes.Load(),
		Errors:       sp.rbSt.errors.Load(),
	}
}

// StopRebalance cancels any running rebalance and returns immediately.
func (sp *ServerPools) StopRebalance() { sp.rbSt.stop() }

// Decommission marks poolIdx as draining, migrates all its objects to the
// remaining pools, and leaves the pool empty. Call it before retiring hardware.
func (sp *ServerPools) Decommission(ctx context.Context, poolIdx int) error {
	if poolIdx < 0 || poolIdx >= len(sp.pools) {
		return ErrInvalidArgument
	}
	sp.dcMu.Lock()
	sp.draining[poolIdx] = true
	st := sp.dcState[poolIdx]
	if st == nil {
		st = &rbState{}
		sp.dcState[poolIdx] = st
	}
	sp.dcMu.Unlock()

	// Invalidate the free-space cache so the next routeWithFree skips the pool.
	sp.fc.set(nil)

	if !st.running.CompareAndSwap(false, true) {
		return nil // already running
	}
	ctx, cancel := context.WithCancel(ctx)
	st.mu.Lock()
	st.cancel = cancel
	st.mu.Unlock()
	st.moved.Store(0)
	st.bytes.Store(0)
	st.errors.Store(0)
	defer func() {
		st.running.Store(false)
		cancel()
	}()

	p := sp.pools[poolIdx]
	buckets, _ := sp.ListBuckets(ctx)
	for _, set := range p.sets {
		for _, bi := range buckets {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			keys, err := set.walkObjects(ctx, bi.Name)
			if err != nil {
				continue
			}
			for _, key := range keys {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				target := sp.routeWithFree(ctx, key)
				if target == set {
					// routeWithFree excluded draining pools but fell back to the same set.
					st.errors.Add(1)
					continue
				}
				if err := sp.migrateObject(ctx, st, set, target, bi.Name, key); err != nil {
					st.errors.Add(1)
					continue
				}
			}
		}
	}
	return nil
}

// DecommissionStatus returns a snapshot of the decommission run for poolIdx.
func (sp *ServerPools) DecommissionStatus(poolIdx int) DecommissionStatus {
	sp.dcMu.RLock()
	defer sp.dcMu.RUnlock()
	s := DecommissionStatus{PoolIndex: poolIdx}
	if st, ok := sp.dcState[poolIdx]; ok {
		s.Running = st.running.Load()
		s.ObjectsMoved = st.moved.Load()
		s.BytesMoved = st.bytes.Load()
		s.Errors = st.errors.Load()
		s.Done = !s.Running && s.ObjectsMoved > 0
	}
	return s
}

// StopDecommission cancels any running decommission for poolIdx.
func (sp *ServerPools) StopDecommission(poolIdx int) {
	sp.dcMu.RLock()
	st := sp.dcState[poolIdx]
	sp.dcMu.RUnlock()
	if st != nil {
		st.stop()
	}
}

// migrateObject copies the latest version of bucket/key from src to dst and
// deletes it from src. Used by both Rebalance and Decommission.
func (sp *ServerPools) migrateObject(ctx context.Context, st *rbState, src, dst *erasureSet, bucket, key string) error {
	info, err := src.getObjectInfo(ctx, bucket, key, ObjectOptions{})
	if err != nil {
		return err
	}
	reader, err := src.getObject(ctx, bucket, key, ObjectOptions{})
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()

	opts := ObjectOptions{
		ContentType: info.ContentType,
		UserDefined: cloneMap(info.UserDefined),
	}
	oi, err := dst.putObject(ctx, bucket, key, NewPutReader(reader, info.Size), opts)
	if err != nil {
		return err
	}
	st.bytes.Add(oi.Size)
	st.moved.Add(1)

	_, err = src.deleteObject(ctx, bucket, key, ObjectOptions{})
	return err
}
