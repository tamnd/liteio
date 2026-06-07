// SPDX-License-Identifier: Apache-2.0

package local

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path"
	"strings"
	"testing"

	"github.com/tamnd/liteio/storage"
)

func newDrive(t *testing.T) *Local {
	t.Helper()
	d, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return d
}

func TestVolumeLifecycle(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)

	if err := d.MakeVol(ctx, "bucket"); err != nil {
		t.Fatalf("MakeVol: %v", err)
	}
	if err := d.MakeVol(ctx, "bucket"); !errors.Is(err, storage.ErrVolumeExists) {
		t.Fatalf("duplicate MakeVol = %v want ErrVolumeExists", err)
	}

	vi, err := d.StatVol(ctx, "bucket")
	if err != nil || vi.Name != "bucket" {
		t.Fatalf("StatVol = %+v, %v", vi, err)
	}
	if _, err := d.StatVol(ctx, "missing"); !errors.Is(err, storage.ErrVolumeNotFound) {
		t.Fatalf("StatVol missing = %v", err)
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

func TestDeleteNonEmptyVolume(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	if err := d.WriteMeta(ctx, "b", "obj", []byte("x")); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	if err := d.DeleteVol(ctx, "b", false); !errors.Is(err, storage.ErrVolumeNotEmpty) {
		t.Fatalf("DeleteVol non-empty = %v want ErrVolumeNotEmpty", err)
	}
	if err := d.DeleteVol(ctx, "b", true); err != nil {
		t.Fatalf("force DeleteVol: %v", err)
	}
}

func TestMetaRoundTrip(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")

	if _, err := d.ReadMeta(ctx, "b", "a/obj"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("ReadMeta missing = %v", err)
	}

	want := []byte("LIO2-meta-bytes")
	if err := d.WriteMeta(ctx, "b", "a/obj", want); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	got, err := d.ReadMeta(ctx, "b", "a/obj")
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("ReadMeta = %q, %v", got, err)
	}

	// Overwrite is atomic and visible.
	want2 := []byte("second-write")
	if err := d.WriteMeta(ctx, "b", "a/obj", want2); err != nil {
		t.Fatalf("WriteMeta overwrite: %v", err)
	}
	got, _ = d.ReadMeta(ctx, "b", "a/obj")
	if !bytes.Equal(got, want2) {
		t.Fatalf("overwrite ReadMeta = %q", got)
	}
}

func TestCreateReadAndCommit(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")

	payload := bytes.Repeat([]byte("shard"), 1000) // 5000 bytes
	staging := "tmp/upload-1/part.1"
	if err := d.CreateFile(ctx, "b", staging, int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}

	// Read it back through the streaming and buffered paths from the staging path.
	rc, err := d.ReadFileStream(ctx, "b", staging, 0, -1)
	if err != nil {
		t.Fatalf("ReadFileStream: %v", err)
	}
	full, _ := io.ReadAll(rc)
	_ = rc.Close()
	if !bytes.Equal(full, payload) {
		t.Fatalf("stream read mismatch: %d bytes", len(full))
	}

	// Ranged read.
	rc, err = d.ReadFileStream(ctx, "b", staging, 5, 5)
	if err != nil {
		t.Fatalf("ranged ReadFileStream: %v", err)
	}
	ranged, _ := io.ReadAll(rc)
	_ = rc.Close()
	if string(ranged) != "shard" {
		t.Fatalf("ranged read = %q", ranged)
	}

	// Commit the staging directory into place with RenameData.
	if err := d.RenameData(ctx, "b", "tmp/upload-1", "objects/file"); err != nil {
		t.Fatalf("RenameData: %v", err)
	}
	if _, err := d.StatFile(ctx, "b", "tmp/upload-1"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("staging should be gone: %v", err)
	}
	st, err := d.StatFile(ctx, "b", "objects/file/part.1")
	if err != nil || st.Size != int64(len(payload)) {
		t.Fatalf("committed StatFile = %+v, %v", st, err)
	}
}

func TestReadFileBuffered(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	payload := []byte("0123456789")
	if err := d.CreateFile(ctx, "b", "f", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	buf := make([]byte, 4)
	n, err := d.ReadFile(ctx, "b", "f", 3, buf)
	if err != nil || n != 4 || string(buf) != "3456" {
		t.Fatalf("ReadFile = %q n=%d err=%v", buf, n, err)
	}
}

func TestCreateFileStreamUntilEOF(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	payload := []byte("unknown-length")
	if err := d.CreateFile(ctx, "b", "f", -1, bytes.NewReader(payload)); err != nil {
		t.Fatalf("CreateFile EOF mode: %v", err)
	}
	st, err := d.StatFile(ctx, "b", "f")
	if err != nil || st.Size != int64(len(payload)) {
		t.Fatalf("StatFile = %+v, %v", st, err)
	}
}

func TestCreateFileShortReader(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	// Declare 100 bytes but supply 10: must fail and leave no file behind.
	err := d.CreateFile(ctx, "b", "f", 100, strings.NewReader("ten-bytes!"))
	if err == nil {
		t.Fatal("expected error on short reader")
	}
	if _, err := d.StatFile(ctx, "b", "f"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("partial file should be removed: %v", err)
	}
}

func TestDeleteAndListDir(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	for _, name := range []string{"dir/a", "dir/b", "dir/c"} {
		if err := d.CreateFile(ctx, "b", name, 1, strings.NewReader("x")); err != nil {
			t.Fatalf("CreateFile %s: %v", name, err)
		}
	}
	names, err := d.ListDir(ctx, "b", "dir", -1)
	if err != nil {
		t.Fatalf("ListDir: %v", err)
	}
	if len(names) != 3 {
		t.Fatalf("ListDir = %v", names)
	}

	// Deleting an absent path is a no-op.
	if err := d.Delete(ctx, "b", "dir/missing", false); err != nil {
		t.Fatalf("delete absent: %v", err)
	}
	if err := d.Delete(ctx, "b", "dir/a", false); err != nil {
		t.Fatalf("delete file: %v", err)
	}
	if err := d.Delete(ctx, "b", "dir", true); err != nil {
		t.Fatalf("recursive delete: %v", err)
	}
	if _, err := d.ListDir(ctx, "b", "dir", -1); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("ListDir after delete = %v", err)
	}
}

func TestPathEscapeRejected(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "b")
	for _, p := range []string{"../escape", "a/../../escape", "../../etc/passwd"} {
		if err := d.WriteMeta(ctx, "b", p, []byte("x")); !errors.Is(err, storage.ErrPathEscapes) {
			t.Fatalf("WriteMeta %q = %v want ErrPathEscapes", p, err)
		}
	}
	// A path that cleans to within the volume is fine.
	if err := d.WriteMeta(ctx, "b", "a/b/../c", []byte("x")); err != nil {
		t.Fatalf("clean inner path rejected: %v", err)
	}
	if _, err := d.ReadMeta(ctx, "b", "a/c"); err != nil {
		t.Fatalf("cleaned path not readable as a/c: %v", err)
	}
}

func TestRenameFile(t *testing.T) {
	ctx := context.Background()
	d := newDrive(t)
	mustMakeVol(t, d, "src")
	mustMakeVol(t, d, "dst")
	if err := d.CreateFile(ctx, "src", "a", 3, strings.NewReader("abc")); err != nil {
		t.Fatalf("CreateFile: %v", err)
	}
	if err := d.RenameFile(ctx, "src", "a", "dst", path.Join("nested", "b")); err != nil {
		t.Fatalf("RenameFile: %v", err)
	}
	if _, err := d.StatFile(ctx, "src", "a"); !errors.Is(err, storage.ErrFileNotFound) {
		t.Fatalf("source should be gone: %v", err)
	}
	st, err := d.StatFile(ctx, "dst", "nested/b")
	if err != nil || st.Size != 3 {
		t.Fatalf("dst StatFile = %+v, %v", st, err)
	}
}

func TestIsOnline(t *testing.T) {
	if d := newDrive(t); !d.IsOnline() {
		t.Fatal("fresh drive should be online")
	}
}

func mustMakeVol(t *testing.T, d *Local, name string) {
	t.Helper()
	if err := d.MakeVol(context.Background(), name); err != nil {
		t.Fatalf("MakeVol %s: %v", name, err)
	}
}

func BenchmarkCreateFile1MiB(b *testing.B) {
	ctx := context.Background()
	d, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if err := d.MakeVol(ctx, "b"); err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xab}, 1<<20)
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if err := d.CreateFile(ctx, "b", "bench/part", int64(len(payload)), bytes.NewReader(payload)); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkReadFileStream1MiB(b *testing.B) {
	ctx := context.Background()
	d, err := New(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	if err := d.MakeVol(ctx, "b"); err != nil {
		b.Fatal(err)
	}
	payload := bytes.Repeat([]byte{0xcd}, 1<<20)
	if err := d.CreateFile(ctx, "b", "f", int64(len(payload)), bytes.NewReader(payload)); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(payload)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		rc, err := d.ReadFileStream(ctx, "b", "f", 0, -1)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, rc); err != nil {
			b.Fatal(err)
		}
		_ = rc.Close()
	}
}
