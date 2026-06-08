// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"sync"
	"time"

	"github.com/tamnd/liteio/metrics"
	"github.com/tamnd/liteio/object"
)

// clusterSource is the slice of the object layer the capacity and health metrics read:
// the cheap topology-and-reachability snapshot, the statfs-backed capacity rollup, and
// the reactive-heal queue counters. *object.ServerPools satisfies it.
type clusterSource interface {
	StorageInfo() object.StorageInfo
	DiskUsage(ctx context.Context) object.DiskUsage
	MRFStats() object.MRFStats
	ReplicationStats() (replicated int64, failed int64)
}

// clusterCollector exposes the deployment's capacity and health as the cluster metric
// families (doc 10.4): drive reachability, set availability against read quorum, raw and
// usable capacity, and the reactive-heal queue. A single scrape reads a dozen of these
// gauges, and computing each independently would walk the pools and statfs every drive
// once per gauge. The collector instead computes one snapshot per scrape and memoizes it
// for a short window, so a scrape probes the drives once and every series it emits comes
// from one consistent view.
type clusterCollector struct {
	src     clusterSource
	now     func() time.Time
	timeout time.Duration // bounds the per-scrape statfs sweep
	ttl     time.Duration // how long one snapshot is reused within a scrape

	mu     sync.Mutex
	snap   clusterSnapshot
	expiry time.Time
	valid  bool
}

// clusterSnapshot is the derived view one scrape reports, every field already a float64
// so a gauge or counter func returns it directly.
type clusterSnapshot struct {
	drivesTotal     float64
	drivesOnline    float64
	setsTotal       float64
	setsDegraded    float64
	setsUnavailable float64
	rawTotal        float64
	rawFree         float64
	usableTotal     float64
	usableFree      float64
	healPending     float64
	healDropped     float64
	healHealed      float64
	healFailed      float64
	replReplicated  float64
	replFailed      float64
}

// newClusterCollector builds a collector with the production probe timeout and snapshot
// window. The window is far shorter than any sane scrape interval, so consecutive
// scrapes each recompute while the many gauges within one scrape share a snapshot.
func newClusterCollector(src clusterSource) *clusterCollector {
	return &clusterCollector{src: src, now: time.Now, timeout: 5 * time.Second, ttl: time.Second}
}

// registerClusterMetrics wires the cluster capacity and health families onto reg, each
// reading its field from the shared per-scrape snapshot. The three heal totals are
// monotonic, so they register as counters; everything else is a point-in-time gauge.
// It also calls RegisterReplicationMetrics.
func registerClusterMetrics(reg *metrics.Registry, src clusterSource) {
	c := newClusterCollector(src)
	reg.NewGaugeFunc("liteio_cluster_drives_total",
		"Drives configured across the deployment.",
		func() float64 { return c.snapshot().drivesTotal })
	reg.NewGaugeFunc("liteio_cluster_drives_online",
		"Drives currently reachable.",
		func() float64 { return c.snapshot().drivesOnline })
	reg.NewGaugeFunc("liteio_cluster_sets_total",
		"Erasure sets across the deployment.",
		func() float64 { return c.snapshot().setsTotal })
	reg.NewGaugeFunc("liteio_cluster_sets_degraded",
		"Erasure sets with a drive down but still at or above read quorum.",
		func() float64 { return c.snapshot().setsDegraded })
	reg.NewGaugeFunc("liteio_cluster_sets_unavailable",
		"Erasure sets below read quorum.",
		func() float64 { return c.snapshot().setsUnavailable })
	reg.NewGaugeFunc("liteio_cluster_capacity_raw_bytes",
		"Raw filesystem capacity backing the drives.",
		func() float64 { return c.snapshot().rawTotal })
	reg.NewGaugeFunc("liteio_cluster_capacity_raw_free_bytes",
		"Raw filesystem capacity still free.",
		func() float64 { return c.snapshot().rawFree })
	reg.NewGaugeFunc("liteio_cluster_capacity_usable_bytes",
		"Usable capacity after the erasure ratio.",
		func() float64 { return c.snapshot().usableTotal })
	reg.NewGaugeFunc("liteio_cluster_capacity_usable_free_bytes",
		"Usable capacity still free after the erasure ratio.",
		func() float64 { return c.snapshot().usableFree })
	reg.NewGaugeFunc("liteio_cluster_heal_queue_depth",
		"Reactive-heal tasks waiting in the queue.",
		func() float64 { return c.snapshot().healPending })
	reg.NewCounterFunc("liteio_cluster_heal_dropped_total",
		"Reactive-heal tasks dropped because the queue was full.",
		func() float64 { return c.snapshot().healDropped })
	reg.NewCounterFunc("liteio_cluster_heal_healed_total",
		"Objects repaired by reactive heal.",
		func() float64 { return c.snapshot().healHealed })
	reg.NewCounterFunc("liteio_cluster_heal_failed_total",
		"Reactive-heal tasks the worker could not repair.",
		func() float64 { return c.snapshot().healFailed })
	RegisterReplicationMetrics(reg, c)
}

// RegisterReplicationMetrics adds replication counters to reg. It is split out
// so tests can call it independently from the cluster health families.
func RegisterReplicationMetrics(reg *metrics.Registry, c *clusterCollector) {
	reg.NewCounterFunc("liteio_replication_replicated_total",
		"Objects successfully replicated to a destination rule.",
		func() float64 { return c.snapshot().replReplicated })
	reg.NewCounterFunc("liteio_replication_failed_total",
		"Replication attempts that failed.",
		func() float64 { return c.snapshot().replFailed })
}

// snapshot returns the current derived view, recomputing it when the memoized one has
// expired. The lock serializes the recompute so a burst of concurrent scrapes probes the
// drives once, not once per scrape.
func (c *clusterCollector) snapshot() clusterSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.valid && c.now().Before(c.expiry) {
		return c.snap
	}
	c.snap = c.compute()
	c.expiry = c.now().Add(c.ttl)
	c.valid = true
	return c.snap
}

// compute walks the topology to derive drive and set health, then probes capacity and
// reads the heal counters. Read quorum is K = N - M; a set is unavailable below K online
// drives and degraded when a drive is down but K or more remain.
func (c *clusterCollector) compute() clusterSnapshot {
	var s clusterSnapshot
	for _, p := range c.src.StorageInfo().Pools {
		for _, set := range p.Sets {
			s.setsTotal++
			n := len(set.Drives)
			quorum := n - set.Parity
			online := 0
			for _, d := range set.Drives {
				if d.Online {
					online++
				}
			}
			s.drivesTotal += float64(n)
			s.drivesOnline += float64(online)
			switch {
			case online < quorum:
				s.setsUnavailable++
			case online < n:
				s.setsDegraded++
			}
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), c.timeout)
	defer cancel()
	usage := c.src.DiskUsage(ctx)
	s.rawTotal = float64(usage.RawTotal)
	s.rawFree = float64(usage.RawFree)
	s.usableTotal = float64(usage.UsableTotal)
	s.usableFree = float64(usage.UsableFree)

	heal := c.src.MRFStats()
	s.healPending = float64(heal.Pending)
	s.healDropped = float64(heal.Dropped)
	s.healHealed = float64(heal.Healed)
	s.healFailed = float64(heal.Failed)

	replicated, replFailed := c.src.ReplicationStats()
	s.replReplicated = float64(replicated)
	s.replFailed = float64(replFailed)
	return s
}
