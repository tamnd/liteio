// SPDX-License-Identifier: Apache-2.0

package remote_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http/httptest"
	"testing"

	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
	"github.com/tamnd/liteio/storage/remote"
)

// newRemoteDrive builds a local drive, exposes it over an httptest RPC server, and
// returns a remote.Storage client for it. The drive behaves identically to the
// local one but every call crosses the transport.
func newRemoteDrive(t testing.TB) storage.StorageAPI {
	t.Helper()
	l, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local.New: %v", err)
	}
	mux := rpc.NewMux()
	remote.Register(mux, l)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return remote.NewStorage(srv.URL, nil)
}

func TestRemoteVolumeLifecycle(t *testing.T) {
	ctx := context.Background()
	d := newRemoteDrive(t)

	if err := d.MakeVol(ctx, "bucket"); err != nil {
		t.Fatalf("MakeVol: %v", err)
	}
	if err := d.MakeVol(ctx, "bucket"); !errors.Is(err, storage.ErrVolumeExists) {
		t.Fatalf("duplicate MakeVol = %v, want ErrVolumeExists across the wire", err)
	}
	vi, err := d.StatVol(ctx, "bucket")
	if err != nil || vi.Name != "bucket" {
		t.Fatalf("StatVol = %+v, %v", vi, err)
	}
	if _, err := d.StatVol(ctx, "missing"); !errors.Is(err, storage.ErrVolumeNotFound) {
		t.Fatalf("StatVol missing = %v, want ErrVolumeNotFound", err)
	}
	vols, err := d.ListVols(ctx)
	if err != nil || len(vols) != 1 || vols[0].Name != "bucket" {
		t.Fatalf("ListVols = %+v, %v", vols, err)
	}
	if err := d.DeleteVol(ctx, "bucket", false); err != nil {
		t.Fatalf("DeleteVol: %v", err)
	}
	if _, err := d.StatVol(ctx, "bucket"); !errors.Is(err, storage.ErrVolumeNotFound) {
		t.Fatalf("after delete StatVol = %v", err)
	}
}

func TestRemoteMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := newRemoteDrive(t)
	if err := d.MakeVol(ctx, "b"); err != nil {
		t.Fatalf("MakeVol: %v", err)
	}

	// A sizeable obj.meta exercises body framing (it would blow a header limit).
	meta := bytes.Repeat([]byte("m"), 200_000)
	if err := d.WriteMeta(ctx, "b", "obj/obj.meta", meta); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	got, err := d.ReadMeta(ctx, "b", "obj/obj.meta")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if !bytes.Equal(got, meta) {
		t.Fatalf("ReadMeta returned %d bytes, want %d identical", len(got), len(meta))
	}
	if _, err := d.ReadMeta(ctx, "b", "absent/obj.meta"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("ReadMeta absent = %v, want ErrFileNotFound", err)
	}
}

func TestRemoteFileRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := newRemoteDrive(t)
	if err := d.MakeVol(ctx, "b"); err != nil {
		t.Fatalf("MakeVol: %v", err)
	}

	payload := bytes.Repeat([]byte("abcdefgh"), 4096) // 32 KiB
	if err := d.CreateFile(ctx, "b", "staged/data", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := d.RenameData(ctx, "b", "staged", "obj"); err != nil {
		t.Fatalf("RenameData: %v", err)
	}

	// Full read via ReadFileStream.
	rc, err := d.ReadFileStream(ctx, "b", "obj/data", 0, -1)
	if err != nil {
		t.Fatalf("ReadFileStream: %v", err)
	}
	whole, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || !bytes.Equal(whole, payload) {
		t.Fatalf("stream read %d bytes (err %v), want %d identical", len(whole), err, len(payload))
	}

	// Ranged read via ReadFile into a fixed buffer.
	buf := make([]byte, 8)
	n, err := d.ReadFile(ctx, "b", "obj/data", 8, buf)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if n != 8 || !bytes.Equal(buf, []byte("abcdefgh")) {
		t.Fatalf("ReadFile at 8 = %q (n=%d)", buf[:n], n)
	}

	// ListDir sees the committed object directory.
	entries, err := d.ListDir(ctx, "b", "", -1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(entries) != 1 || entries[0] != "obj/" {
		t.Fatalf("ListDir = %v, want [obj/]", entries)
	}

	if err := d.Delete(ctx, "b", "obj", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := d.ReadFileStream(ctx, "b", "obj/data", 0, -1); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("after delete read = %v, want ErrFileNotFound", err)
	}
}

func TestRemoteIsOnline(t *testing.T) {
	d := newRemoteDrive(t)
	if !d.IsOnline() {
		t.Fatal("a reachable remote drive should report online")
	}
	// A drive whose peer is gone reports offline rather than hanging.
	dead := remote.NewStorage("http://127.0.0.1:1", nil)
	if dead.IsOnline() {
		t.Fatal("an unreachable remote drive should report offline")
	}
}

func TestRemoteDiskInfo(t *testing.T) {
	d := newRemoteDrive(t)
	di, err := d.DiskInfo(context.Background())
	if err != nil {
		t.Fatalf("DiskInfo: %v", err)
	}
	// The figures the peer read off its real filesystem must survive the round
	// trip intact: a non-zero total with consistent parts.
	if di.Total == 0 {
		t.Fatal("Total is zero across the wire")
	}
	if di.Free > di.Total || di.Used > di.Total {
		t.Fatalf("inconsistent DiskInfo %+v", di)
	}
}

// TestObjectLayerOverRemoteDrives is the headline: an erasure set built entirely
// from remote drives stores and serves an object through the unchanged object
// layer, proving local and remote drives are interchangeable.
func TestObjectLayerOverRemoteDrives(t *testing.T) {
	ctx := context.Background()
	const n, parity = 6, 3
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		drives[i] = newRemoteDrive(t)
	}
	var depID [16]byte
	for i := range depID {
		depID[i] = byte(i)
	}
	sp, err := object.NewSingleSet(depID, drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}

	if err := sp.MakeBucket(ctx, "b", object.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	body := bytes.Repeat([]byte("liteio-distributed"), 10_000) // ~180 KiB, multi-shard
	if _, err := sp.PutObject(ctx, "b", "k", object.NewPutReader(bytes.NewReader(body), int64(len(body))), object.ObjectOptions{}); err != nil {
		t.Fatalf("PutObject over remote drives: %v", err)
	}
	gr, err := sp.GetObject(ctx, "b", "k", object.ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	got, err := io.ReadAll(gr)
	_ = gr.Close()
	if err != nil || !bytes.Equal(got, body) {
		t.Fatalf("round trip over remote drives: %d bytes (err %v), want %d identical", len(got), err, len(body))
	}
}
