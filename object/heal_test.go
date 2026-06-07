// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// healBytes returns n deterministic, non-trivial bytes for heal round trips.
func healBytes(n int) []byte {
	b := make([]byte, n)
	for i := range b {
		b[i] = byte(i*31 + 7)
	}
	return b
}

// faultDrive wraps a StorageAPI with a toggle that makes the drive appear down:
// while offline every operation returns ErrDriveOffline, modelling a drive that
// was unreachable during a write and later came back to be healed.
type faultDrive struct {
	storage.StorageAPI
	offline atomic.Bool
}

func (f *faultDrive) down() { f.offline.Store(true) }
func (f *faultDrive) up()   { f.offline.Store(false) }

func (f *faultDrive) MakeVol(ctx context.Context, v string) error {
	if f.offline.Load() {
		return storage.ErrDriveOffline
	}
	return f.StorageAPI.MakeVol(ctx, v)
}

func (f *faultDrive) ReadMeta(ctx context.Context, v, p string) ([]byte, error) {
	if f.offline.Load() {
		return nil, storage.ErrDriveOffline
	}
	return f.StorageAPI.ReadMeta(ctx, v, p)
}

func (f *faultDrive) WriteMeta(ctx context.Context, v, p string, b []byte) error {
	if f.offline.Load() {
		return storage.ErrDriveOffline
	}
	return f.StorageAPI.WriteMeta(ctx, v, p, b)
}

func (f *faultDrive) CreateFile(ctx context.Context, v, p string, size int64, r io.Reader) error {
	if f.offline.Load() {
		return storage.ErrDriveOffline
	}
	return f.StorageAPI.CreateFile(ctx, v, p, size, r)
}

func (f *faultDrive) ReadFileStream(ctx context.Context, v, p string, off, n int64) (io.ReadCloser, error) {
	if f.offline.Load() {
		return nil, storage.ErrDriveOffline
	}
	return f.StorageAPI.ReadFileStream(ctx, v, p, off, n)
}

func (f *faultDrive) RenameData(ctx context.Context, v, src, dst string) error {
	if f.offline.Load() {
		return storage.ErrDriveOffline
	}
	return f.StorageAPI.RenameData(ctx, v, src, dst)
}

func (f *faultDrive) RenameFile(ctx context.Context, sv, sp, dv, dp string) error {
	if f.offline.Load() {
		return storage.ErrDriveOffline
	}
	return f.StorageAPI.RenameFile(ctx, sv, sp, dv, dp)
}

// healLayer builds a single set of n local drives whose last drive is a
// faultDrive, returning the layer and that drive's toggle. Parity 2 over 4 drives
// gives a write quorum of 3, so the layer commits with one drive down and has a
// laggard to heal.
func healLayer(tb testing.TB, n, parity int) (*ServerPools, *faultDrive) {
	tb.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(tb.TempDir())
		if err != nil {
			tb.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	fault := &faultDrive{StorageAPI: drives[n-1]}
	drives[n-1] = fault
	sp, err := NewSingleSet(depID(9), drives, parity)
	if err != nil {
		tb.Fatalf("NewSingleSet: %v", err)
	}
	return sp, fault
}

// drivesWithVersion counts how many of the set's drives carry version versionID
// of bucket/object in their obj.meta.
func drivesWithVersion(tb testing.TB, drives []storage.StorageAPI, bucket, object, versionID string) int {
	tb.Helper()
	ctx := context.Background()
	count := 0
	for _, d := range drives {
		raw, err := d.ReadMeta(ctx, bucket, object)
		if err != nil {
			continue
		}
		vers, err := meta.Unmarshal(raw)
		if err != nil {
			continue
		}
		for _, fi := range vers {
			if fi.VersionID == versionID && !fi.Deleted {
				count++
				break
			}
		}
	}
	return count
}

func TestReactiveHealRepairsDownDrive(t *testing.T) {
	for _, tc := range []struct {
		name string
		size int
	}{
		{"inline", 4 << 10},      // <= 128 KiB: shard rides in obj.meta
		{"outofline", 300 << 10}, // > 128 KiB: shards are part files
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			sp, fault := healLayer(t, 4, 2)
			set := sp.route("k")
			if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
				t.Fatalf("MakeBucket: %v", err)
			}

			body := healBytes(tc.size)
			fault.down() // a drive is unreachable for the whole write
			oi, err := sp.PutObject(ctx, "b", "k", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
			if err != nil {
				t.Fatalf("PutObject (one drive down): %v", err)
			}

			fault.up() // the drive comes back
			if n := drivesWithVersion(t, set.drives, "b", "k", oi.VersionID); n != 3 {
				t.Fatalf("before heal: %d/4 drives carry the version, want 3", n)
			}

			if err := set.healObject(ctx, "b", "k", oi.VersionID); err != nil {
				t.Fatalf("healObject: %v", err)
			}
			if n := drivesWithVersion(t, set.drives, "b", "k", oi.VersionID); n != 4 {
				t.Fatalf("after heal: %d/4 drives carry the version, want 4", n)
			}

			// The healed object still reads back byte for byte.
			if got := getBytes(t, sp, "b", "k", ObjectOptions{}); !bytes.Equal(got, body) {
				t.Fatalf("after heal: %d bytes back, want %d", len(got), len(body))
			}
		})
	}
}

func TestReactiveHealIsNoOpWhenFullyReplicated(t *testing.T) {
	ctx := context.Background()
	sp, _ := healLayer(t, 4, 2)
	set := sp.route("k")
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	oi := putGet(t, sp, "b", "k", healBytes(8<<10), ObjectOptions{})
	// All four drives already have it; heal must succeed and change nothing.
	if err := set.healObject(ctx, "b", "k", oi.VersionID); err != nil {
		t.Fatalf("healObject on a clean object: %v", err)
	}
	if n := drivesWithVersion(t, set.drives, "b", "k", oi.VersionID); n != 4 {
		t.Fatalf("%d/4 drives carry the version, want 4", n)
	}
}

// TestReactiveHealViaQueue drives the full reactive path: a write with a drive
// down enqueues a heal task, the background worker drains it, and the laggard is
// repaired without any direct healObject call.
func TestReactiveHealViaQueue(t *testing.T) {
	ctx := t.Context()
	sp, fault := healLayer(t, 4, 2)
	set := sp.route("k")
	sp.StartHealing(ctx)
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}

	body := healBytes(64 << 10)
	fault.down()
	oi, err := sp.PutObject(ctx, "b", "k", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject (one drive down): %v", err)
	}
	fault.up()

	// Wait for the worker to finish a heal task (it increments the counter only
	// after the repair completes, so this also orders the assertions below).
	deadline := time.Now().Add(5 * time.Second)
	for sp.MRFStats().Healed+sp.MRFStats().Failed == 0 {
		if time.Now().After(deadline) {
			t.Fatalf("reactive heal worker ran no task in time (stats %+v)", sp.MRFStats())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if st := sp.MRFStats(); st.Healed != 1 || st.Failed != 0 {
		t.Fatalf("heal stats = %+v, want one healed and none failed", st)
	}
	if n := drivesWithVersion(t, set.drives, "b", "k", oi.VersionID); n != 4 {
		t.Fatalf("after reactive heal: %d/4 drives carry the version, want 4", n)
	}
}

func TestMRFDropsWhenFull(t *testing.T) {
	// A queue of depth 1 with no worker draining it: the first task is buffered,
	// the rest are dropped and counted.
	q := newMRF(func(context.Context, healTask) error { return nil }, 1)
	for range 5 {
		q.enqueue(healTask{bucket: "b", object: "k"})
	}
	if got := q.dropped.Load(); got != 4 {
		t.Fatalf("dropped = %d, want 4", got)
	}
}

func BenchmarkHealObject(b *testing.B) {
	ctx := context.Background()
	body := healBytes(256 << 10)
	for b.Loop() {
		b.StopTimer()
		sp, fault := healLayer(b, 4, 2)
		set := sp.route("k")
		if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
			b.Fatalf("MakeBucket: %v", err)
		}
		fault.down()
		oi, err := sp.PutObject(ctx, "b", "k", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
		if err != nil {
			b.Fatalf("PutObject: %v", err)
		}
		fault.up()
		b.StartTimer()

		if err := set.healObject(ctx, "b", "k", oi.VersionID); err != nil {
			b.Fatalf("healObject: %v", err)
		}
	}
}
