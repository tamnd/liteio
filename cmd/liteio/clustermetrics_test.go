// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/metrics"
	"github.com/tamnd/liteio/object"
)

// fakeClusterSource is a hand-built clusterSource: the topology and capacity are fixed,
// and diskCalls counts how often the statfs-backed rollup is probed so a test can prove
// the collector memoizes it across the gauges of one scrape.
type fakeClusterSource struct {
	info       object.StorageInfo
	usage      object.DiskUsage
	heal       object.MRFStats
	diskCalls  int
	replicated int64
	replFailed int64
}

func (f *fakeClusterSource) StorageInfo() object.StorageInfo { return f.info }

func (f *fakeClusterSource) DiskUsage(context.Context) object.DiskUsage {
	f.diskCalls++
	return f.usage
}

func (f *fakeClusterSource) MRFStats() object.MRFStats { return f.heal }

func (f *fakeClusterSource) ReplicationStats() (int64, int64) {
	return f.replicated, f.replFailed
}

// drives builds a slice with the first online of them up reachable and the rest down.
func drives(total, up int) []object.DriveInfo {
	d := make([]object.DriveInfo, total)
	for i := range d {
		d[i] = object.DriveInfo{Endpoint: "d", Online: i < up}
	}
	return d
}

// sampleSource returns a two-set deployment: one healthy set (4/4 up, parity 2) and one
// degraded set (3/4 up, parity 2, still at quorum K=2), plus capacity and heal counters.
func sampleSource() *fakeClusterSource {
	return &fakeClusterSource{
		info: object.StorageInfo{Pools: []object.PoolInfo{{Sets: []object.SetInfo{
			{Drives: drives(4, 4), Parity: 2},
			{Drives: drives(4, 3), Parity: 2},
		}}}},
		usage: object.DiskUsage{RawTotal: 4000, RawFree: 1000, UsableTotal: 2000, UsableFree: 500},
		heal:  object.MRFStats{Pending: 3, Dropped: 7, Healed: 40, Failed: 2},
	}
}

func TestClusterCollectorCompute(t *testing.T) {
	c := newClusterCollector(sampleSource())
	s := c.snapshot()

	cases := []struct {
		name string
		got  float64
		want float64
	}{
		{"drivesTotal", s.drivesTotal, 8},
		{"drivesOnline", s.drivesOnline, 7},
		{"setsTotal", s.setsTotal, 2},
		{"setsDegraded", s.setsDegraded, 1},
		{"setsUnavailable", s.setsUnavailable, 0},
		{"rawTotal", s.rawTotal, 4000},
		{"rawFree", s.rawFree, 1000},
		{"usableTotal", s.usableTotal, 2000},
		{"usableFree", s.usableFree, 500},
		{"healPending", s.healPending, 3},
		{"healDropped", s.healDropped, 7},
		{"healHealed", s.healHealed, 40},
		{"healFailed", s.healFailed, 2},
	}
	for _, tc := range cases {
		if tc.got != tc.want {
			t.Errorf("%s = %v, want %v", tc.name, tc.got, tc.want)
		}
	}
}

// TestClusterCollectorUnavailableSet drops a set below read quorum and checks it counts
// as unavailable, not degraded.
func TestClusterCollectorUnavailableSet(t *testing.T) {
	src := &fakeClusterSource{info: object.StorageInfo{Pools: []object.PoolInfo{{Sets: []object.SetInfo{
		{Drives: drives(4, 1), Parity: 2}, // K=2, only 1 up -> unavailable
	}}}}}
	s := newClusterCollector(src).snapshot()
	if s.setsUnavailable != 1 || s.setsDegraded != 0 {
		t.Errorf("unavailable=%v degraded=%v, want 1 and 0", s.setsUnavailable, s.setsDegraded)
	}
}

// TestClusterCollectorMemoizes proves a scrape probes the drives once: the many gauges
// read within one window share a snapshot, and the next window recomputes.
func TestClusterCollectorMemoizes(t *testing.T) {
	src := sampleSource()
	now := time.Unix(1000, 0)
	c := newClusterCollector(src)
	c.now = func() time.Time { return now }

	for range 14 { // one read per registered family
		c.snapshot()
	}
	if src.diskCalls != 1 {
		t.Fatalf("diskCalls = %d within one window, want 1", src.diskCalls)
	}

	now = now.Add(2 * time.Second) // past the 1s ttl
	c.snapshot()
	if src.diskCalls != 2 {
		t.Fatalf("diskCalls = %d after window, want 2", src.diskCalls)
	}
}

// TestRegisterClusterMetrics renders the registry and checks every family is present
// with the right type and value, and that the heal totals are counters.
func TestRegisterClusterMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	registerClusterMetrics(reg, sampleSource())

	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()

	want := []string{
		"liteio_cluster_drives_total 8",
		"liteio_cluster_drives_online 7",
		"liteio_cluster_sets_total 2",
		"liteio_cluster_sets_degraded 1",
		"liteio_cluster_sets_unavailable 0",
		"liteio_cluster_capacity_raw_bytes 4000",
		"liteio_cluster_capacity_raw_free_bytes 1000",
		"liteio_cluster_capacity_usable_bytes 2000",
		"liteio_cluster_capacity_usable_free_bytes 500",
		"liteio_cluster_heal_queue_depth 3",
		"liteio_cluster_heal_dropped_total 7",
		"liteio_cluster_heal_healed_total 40",
		"liteio_cluster_heal_failed_total 2",
		"# TYPE liteio_cluster_drives_online gauge",
		"# TYPE liteio_cluster_heal_queue_depth gauge",
		"# TYPE liteio_cluster_heal_dropped_total counter",
		"# TYPE liteio_cluster_heal_healed_total counter",
		"# TYPE liteio_cluster_heal_failed_total counter",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n%s", w, out)
		}
	}
}
