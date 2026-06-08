// SPDX-License-Identifier: Apache-2.0

package object

import (
	"testing"

	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

func TestStorageInfoTopology(t *testing.T) {
	sp := newLayer(t, 6, 2)
	si := sp.StorageInfo()

	if si.DeploymentID == "" {
		t.Error("DeploymentID is empty")
	}
	if len(si.Pools) != 1 {
		t.Fatalf("pools = %d, want 1", len(si.Pools))
	}
	if len(si.Pools[0].Sets) != 1 {
		t.Fatalf("sets = %d, want 1", len(si.Pools[0].Sets))
	}
	set := si.Pools[0].Sets[0]
	if set.Parity != 2 {
		t.Errorf("parity = %d, want 2", set.Parity)
	}
	if len(set.Drives) != 6 {
		t.Fatalf("drives = %d, want 6", len(set.Drives))
	}
	for i, d := range set.Drives {
		if d.Endpoint == "" {
			t.Errorf("drive %d has empty endpoint", i)
		}
		if !d.Online {
			t.Errorf("drive %d should be online (local drive)", i)
		}
	}
}

func TestStorageInfoReflectsOffline(t *testing.T) {
	// Build a set where one drive reports offline, and confirm the snapshot shows
	// it. The offlineDrive wraps a real local drive so String still resolves.
	drives := make([]storage.StorageAPI, 4)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		if i == 2 {
			drives[i] = offlineDrive{d}
		} else {
			drives[i] = d
		}
	}
	sp, err := NewSingleSet(depID(3), drives, 2)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}

	set := sp.StorageInfo().Pools[0].Sets[0]
	online := 0
	for _, d := range set.Drives {
		if d.Online {
			online++
		}
	}
	if online != 3 {
		t.Errorf("online drives = %d, want 3 (one is offline)", online)
	}
	if set.Drives[2].Online {
		t.Error("drive 2 should report offline")
	}
}

func TestStorageInfoMultiSet(t *testing.T) {
	// Two sets in one pool, so the snapshot must report both with their own drives.
	mk := func(n int) []storage.StorageAPI {
		ds := make([]storage.StorageAPI, n)
		for i := range ds {
			d, err := local.New(t.TempDir())
			if err != nil {
				t.Fatalf("local.New: %v", err)
			}
			ds[i] = d
		}
		return ds
	}
	sp, err := NewServerPools(depID(9), []PoolConfig{{Sets: []SetConfig{
		{Drives: mk(4), Parity: 2},
		{Drives: mk(6), Parity: 3},
	}}})
	if err != nil {
		t.Fatalf("NewServerPools: %v", err)
	}

	sets := sp.StorageInfo().Pools[0].Sets
	if len(sets) != 2 {
		t.Fatalf("sets = %d, want 2", len(sets))
	}
	if len(sets[0].Drives) != 4 || sets[0].Parity != 2 {
		t.Errorf("set 0 = %d drives parity %d, want 4/2", len(sets[0].Drives), sets[0].Parity)
	}
	if len(sets[1].Drives) != 6 || sets[1].Parity != 3 {
		t.Errorf("set 1 = %d drives parity %d, want 6/3", len(sets[1].Drives), sets[1].Parity)
	}
}

func BenchmarkStorageInfo(b *testing.B) {
	sp := newLayer(b, 8, 4)
	for b.Loop() {
		_ = sp.StorageInfo()
	}
}
