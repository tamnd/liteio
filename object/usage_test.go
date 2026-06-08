// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"testing"

	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// noDiskDrive wraps a real drive but refuses the statfs probe, standing in for an
// offline drive or one on a platform without DiskInfo support.
type noDiskDrive struct{ storage.StorageAPI }

func (noDiskDrive) DiskInfo(context.Context) (storage.DiskInfo, error) {
	return storage.DiskInfo{}, storage.ErrDiskInfoUnsupported
}

func TestDiskUsageAggregates(t *testing.T) {
	sp := newLayer(t, 6, 2)
	du := sp.DiskUsage(context.Background())

	if len(du.Sets) != 1 {
		t.Fatalf("sets = %d, want 1", len(du.Sets))
	}
	su := du.Sets[0]
	if su.DriveCount != 6 || su.DrivesReporting != 6 {
		t.Fatalf("drives = %d reporting %d, want 6/6", su.DriveCount, su.DrivesReporting)
	}
	if su.RawTotal == 0 {
		t.Fatal("RawTotal is zero on real filesystems")
	}
	// Usable is the data fraction K/N of raw: K=4, N=6. Allow a drive's worth of
	// rounding slack since usable is computed from the integer-divided raw sum.
	wantUsable := su.RawTotal / 6 * 4
	if su.UsableTotal != wantUsable {
		t.Errorf("UsableTotal = %d, want %d (raw*K/N)", su.UsableTotal, wantUsable)
	}
	if su.UsableTotal >= su.RawTotal {
		t.Errorf("usable %d should be below raw %d", su.UsableTotal, su.RawTotal)
	}
	// The deployment rollup is the sum of its one set.
	if du.RawTotal != su.RawTotal || du.UsableTotal != su.UsableTotal {
		t.Errorf("rollup %+v disagrees with the only set %+v", du, su)
	}
}

func TestDiskUsageSkipsNonReportingDrives(t *testing.T) {
	sp := newLayer(t, 4, 2)
	// Silence one drive's statfs; it must lower DrivesReporting without erroring out
	// the whole probe.
	sp.pools[0].sets[0].drives[1] = noDiskDrive{sp.pools[0].sets[0].drives[1]}

	su := sp.DiskUsage(context.Background()).Sets[0]
	if su.DriveCount != 4 {
		t.Errorf("DriveCount = %d, want 4", su.DriveCount)
	}
	if su.DrivesReporting != 3 {
		t.Errorf("DrivesReporting = %d, want 3 (one is silent)", su.DrivesReporting)
	}
	if su.RawTotal == 0 {
		t.Error("the three reporting drives should still sum to a non-zero raw total")
	}
}

func TestDiskUsageMultiSet(t *testing.T) {
	sp, err := NewServerPools(depID(11), []PoolConfig{{Sets: []SetConfig{
		{Drives: mkLocalDrives(t, 4), Parity: 2},
		{Drives: mkLocalDrives(t, 6), Parity: 3},
	}}})
	if err != nil {
		t.Fatalf("NewServerPools: %v", err)
	}
	du := sp.DiskUsage(context.Background())
	if len(du.Sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(du.Sets))
	}
	var sumRaw, sumUsable uint64
	for _, s := range du.Sets {
		sumRaw += s.RawTotal
		sumUsable += s.UsableTotal
	}
	if du.RawTotal != sumRaw || du.UsableTotal != sumUsable {
		t.Errorf("rollup %+v is not the sum of its sets", du)
	}
}

func mkLocalDrives(t *testing.T, n int) []storage.StorageAPI {
	t.Helper()
	ds := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		ds[i] = d
	}
	return ds
}

func BenchmarkDiskUsage(b *testing.B) {
	sp := newLayer(b, 8, 4)
	ctx := context.Background()
	for b.Loop() {
		_ = sp.DiskUsage(ctx)
	}
}
