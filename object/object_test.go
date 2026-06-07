// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

func depID(b byte) [16]byte {
	var d [16]byte
	for i := range d {
		d[i] = b + byte(i)
	}
	return d
}

// newLayer builds a single-set ServerPools backed by n local drives in temp dirs.
func newLayer(t *testing.T, n, parity int) *ServerPools {
	t.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	sp, err := NewSingleSet(depID(7), drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}
	return sp
}

func putGet(t *testing.T, sp *ServerPools, bucket, key string, body []byte, opts ObjectOptions) ObjectInfo {
	t.Helper()
	ctx := context.Background()
	oi, err := sp.PutObject(ctx, bucket, key, NewPutReader(bytes.NewReader(body), int64(len(body))), opts)
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	got := getBytes(t, sp, bucket, key, ObjectOptions{})
	if !bytes.Equal(got, body) {
		t.Fatalf("GetObject %s: %d bytes back, want %d", key, len(got), len(body))
	}
	return oi
}

func getBytes(t *testing.T, sp *ServerPools, bucket, key string, opts ObjectOptions) []byte {
	t.Helper()
	r, err := sp.GetObject(context.Background(), bucket, key, opts)
	if err != nil {
		t.Fatalf("GetObject %s: %v", key, err)
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read object %s: %v", key, err)
	}
	return data
}

func TestBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)

	if err := sp.MakeBucket(ctx, "photos", MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	if err := sp.MakeBucket(ctx, "photos", MakeBucketOptions{}); !errors.Is(err, ErrBucketExists) {
		t.Fatalf("duplicate MakeBucket = %v want ErrBucketExists", err)
	}
	bi, err := sp.GetBucketInfo(ctx, "photos")
	if err != nil || bi.Name != "photos" {
		t.Fatalf("GetBucketInfo = %+v, %v", bi, err)
	}
	if _, err := sp.GetBucketInfo(ctx, "missing"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("GetBucketInfo missing = %v", err)
	}
	buckets, err := sp.ListBuckets(ctx)
	if err != nil || len(buckets) != 1 || buckets[0].Name != "photos" {
		t.Fatalf("ListBuckets = %+v, %v", buckets, err)
	}
	if err := sp.DeleteBucket(ctx, "photos", DeleteBucketOptions{}); err != nil {
		t.Fatalf("DeleteBucket: %v", err)
	}
	if _, err := sp.GetBucketInfo(ctx, "photos"); !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("after delete GetBucketInfo = %v", err)
	}
}

func TestPutGetInlineAndOutOfLine(t *testing.T) {
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	// Inline (small) and out-of-line (large) objects round-trip identically.
	small := bytes.Repeat([]byte("s"), 1000)
	large := bytes.Repeat([]byte("L"), 2<<20) // 2 MiB, above the inline threshold

	si := putGet(t, sp, "b", "small.txt", small, ObjectOptions{ContentType: "text/plain"})
	li := putGet(t, sp, "b", "big.bin", large, ObjectOptions{})

	if si.ETag == "" || li.ETag == "" {
		t.Fatal("missing ETag")
	}
	if si.Size != int64(len(small)) || li.Size != int64(len(large)) {
		t.Fatalf("sizes wrong: %d %d", si.Size, li.Size)
	}
	if si.ContentType != "text/plain" {
		t.Fatalf("content-type = %q", si.ContentType)
	}

	// HEAD returns metadata without reading data.
	hi, err := sp.GetObjectInfo(context.Background(), "b", "big.bin", ObjectOptions{})
	if err != nil || hi.Size != int64(len(large)) || hi.ETag != li.ETag {
		t.Fatalf("GetObjectInfo = %+v, %v", hi, err)
	}
}

func TestGetMissingObject(t *testing.T) {
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	if _, err := sp.GetObject(context.Background(), "b", "nope", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("GetObject missing = %v", err)
	}
	if _, err := sp.GetObjectInfo(context.Background(), "b", "nope", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("GetObjectInfo missing = %v", err)
	}
}

func TestOverwriteUnversioned(t *testing.T) {
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	putGet(t, sp, "b", "k", []byte("first"), ObjectOptions{})
	putGet(t, sp, "b", "k", []byte("second-and-longer"), ObjectOptions{})
	// Only the null version exists; latest reads back the second write.
	got := getBytes(t, sp, "b", "k", ObjectOptions{})
	if string(got) != "second-and-longer" {
		t.Fatalf("overwrite read = %q", got)
	}
}

// Degraded read: an object survives the loss of up to M drives.
func TestDegradedRead(t *testing.T) {
	sp := newLayer(t, 8, 4) // K=4, M=4: tolerate 4 lost drives
	mustMakeBucket(t, sp, "b")
	body := bytes.Repeat([]byte("erasure!"), 300000) // ~2.3 MiB, out-of-line
	putGet(t, sp, "b", "obj", body, ObjectOptions{})

	set := sp.allSets()[0]
	// Replace 4 of the 8 drives with offline fakes.
	for i := 0; i < 4; i++ {
		set.drives[i] = offlineDrive{set.drives[i]}
	}
	got := getBytes(t, sp, "b", "obj", ObjectOptions{})
	if !bytes.Equal(got, body) {
		t.Fatalf("degraded read mismatch: %d bytes", len(got))
	}
}

// Losing more than M drives must fail the read, not return corrupt data.
func TestReadQuorumLost(t *testing.T) {
	sp := newLayer(t, 8, 4)
	mustMakeBucket(t, sp, "b")
	body := bytes.Repeat([]byte("data"), 300000)
	putGet(t, sp, "b", "obj", body, ObjectOptions{})

	set := sp.allSets()[0]
	for i := 0; i < 5; i++ { // drop 5 > M=4
		set.drives[i] = offlineDrive{set.drives[i]}
	}
	if _, err := sp.GetObject(context.Background(), "b", "obj", ObjectOptions{}); err == nil {
		t.Fatal("expected read failure with fewer than K drives")
	}
}

// Bitrot on one shard must be detected and routed around via reconstruction.
func TestBitrotReconstruction(t *testing.T) {
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")
	body := bytes.Repeat([]byte("rot"), 400000) // out-of-line
	putGet(t, sp, "b", "obj", body, ObjectOptions{})

	// Corrupt one drive's shard bytes; the read should still reconstruct.
	set := sp.allSets()[0]
	set.drives[0] = corruptDrive{set.drives[0]}
	got := getBytes(t, sp, "b", "obj", ObjectOptions{})
	if !bytes.Equal(got, body) {
		t.Fatalf("bitrot read mismatch: %d bytes", len(got))
	}
}

func TestWriteQuorumFailure(t *testing.T) {
	sp := newLayer(t, 4, 2) // K=2, write quorum = 3
	mustMakeBucket(t, sp, "b")
	set := sp.allSets()[0]
	// Knock out 2 of 4 drives; only 2 can accept the write, below quorum 3.
	set.drives[0] = offlineDrive{set.drives[0]}
	set.drives[1] = offlineDrive{set.drives[1]}
	_, err := sp.PutObject(context.Background(), "b", "k",
		NewPutReader(bytes.NewReader([]byte("x")), 1), ObjectOptions{})
	if !errors.Is(err, ErrWriteQuorum) {
		t.Fatalf("PutObject = %v want ErrWriteQuorum", err)
	}
}

func TestVersioningAndDeleteMarker(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{VersionedDefault: true}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	v1 := putVersion(t, sp, "b", "k", []byte("one"))
	v2 := putVersion(t, sp, "b", "k", []byte("two"))
	if v1.VersionID == "" || v2.VersionID == "" || v1.VersionID == v2.VersionID {
		t.Fatalf("expected distinct version IDs: %q %q", v1.VersionID, v2.VersionID)
	}
	// Latest is v2; specific versions are independently readable.
	if got := getBytes(t, sp, "b", "k", ObjectOptions{}); string(got) != "two" {
		t.Fatalf("latest = %q", got)
	}
	if got := getBytes(t, sp, "b", "k", ObjectOptions{VersionID: v1.VersionID}); string(got) != "one" {
		t.Fatalf("v1 = %q", got)
	}

	// Delete without a version writes a delete marker; latest reads as gone.
	di, err := sp.DeleteObject(ctx, "b", "k", ObjectOptions{})
	if err != nil || !di.DeleteMarker {
		t.Fatalf("DeleteObject = %+v, %v", di, err)
	}
	if _, err := sp.GetObject(ctx, "b", "k", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("after delete marker GetObject = %v", err)
	}
	// The prior version is still retrievable by ID.
	if got := getBytes(t, sp, "b", "k", ObjectOptions{VersionID: v2.VersionID}); string(got) != "two" {
		t.Fatalf("v2 after marker = %q", got)
	}
}

func TestDeleteUnversioned(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	putGet(t, sp, "b", "k", []byte("bye"), ObjectOptions{})
	if _, err := sp.DeleteObject(ctx, "b", "k", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if _, err := sp.GetObject(ctx, "b", "k", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("after delete = %v", err)
	}
}

func TestListObjectsV2(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	keys := []string{"a.txt", "photos/1.jpg", "photos/2.jpg", "photos/sub/3.jpg", "z.txt"}
	for _, k := range keys {
		putGet(t, sp, "b", k, []byte(k), ObjectOptions{})
	}

	// Flat listing returns every object key, sorted.
	flat, err := sp.ListObjectsV2(ctx, "b", "", "", "", "", 1000, false)
	if err != nil {
		t.Fatalf("ListObjectsV2: %v", err)
	}
	if len(flat.Objects) != len(keys) {
		t.Fatalf("flat list = %d objects want %d", len(flat.Objects), len(keys))
	}

	// Delimiter rolls photos/* into a common prefix.
	rolled, err := sp.ListObjectsV2(ctx, "b", "", "", "", "/", 1000, false)
	if err != nil {
		t.Fatalf("delim list: %v", err)
	}
	if len(rolled.Prefixes) != 1 || rolled.Prefixes[0] != "photos/" {
		t.Fatalf("prefixes = %v", rolled.Prefixes)
	}
	// Top-level objects only: a.txt and z.txt.
	if len(rolled.Objects) != 2 {
		t.Fatalf("top-level objects = %d want 2", len(rolled.Objects))
	}

	// Prefix scoping.
	scoped, err := sp.ListObjectsV2(ctx, "b", "photos/", "", "", "/", 1000, false)
	if err != nil {
		t.Fatalf("prefix list: %v", err)
	}
	if len(scoped.Objects) != 2 || len(scoped.Prefixes) != 1 || scoped.Prefixes[0] != "photos/sub/" {
		t.Fatalf("scoped = objects %d prefixes %v", len(scoped.Objects), scoped.Prefixes)
	}
}

func TestListObjectsV2Pagination(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	for i := 0; i < 5; i++ {
		k := fmt.Sprintf("k%02d", i)
		putGet(t, sp, "b", k, []byte(k), ObjectOptions{})
	}
	page1, err := sp.ListObjectsV2(ctx, "b", "", "", "", "", 2, false)
	if err != nil {
		t.Fatalf("page1: %v", err)
	}
	if len(page1.Objects) != 2 || !page1.IsTruncated || page1.NextContinuationToken == "" {
		t.Fatalf("page1 = %+v", page1)
	}
	page2, err := sp.ListObjectsV2(ctx, "b", "", page1.NextContinuationToken, "", "", 2, false)
	if err != nil {
		t.Fatalf("page2: %v", err)
	}
	if len(page2.Objects) != 2 || !page2.IsTruncated {
		t.Fatalf("page2 = %+v", page2)
	}
	if page2.Objects[0].Name <= page1.Objects[1].Name {
		t.Fatalf("pagination not advancing: %q then %q", page1.Objects[1].Name, page2.Objects[0].Name)
	}
}

func TestDeleteObjectsBatch(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	for _, k := range []string{"a", "b", "c"} {
		putGet(t, sp, "b", k, []byte(k), ObjectOptions{})
	}
	deleted, errs := sp.DeleteObjects(ctx, "b", []ObjectToDelete{{Name: "a"}, {Name: "c"}, {Name: "ghost"}}, ObjectOptions{})
	if len(deleted) != 3 {
		t.Fatalf("deleted len = %d", len(deleted))
	}
	if errs[0] != nil || errs[1] != nil {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !errors.Is(errs[2], ErrObjectNotFound) {
		t.Fatalf("ghost err = %v", errs[2])
	}
	if _, err := sp.GetObject(ctx, "b", "a", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("a should be gone: %v", err)
	}
	if got := getBytes(t, sp, "b", "b", ObjectOptions{}); string(got) != "b" {
		t.Fatalf("b should survive: %q", got)
	}
}

func TestMultiPoolRouting(t *testing.T) {
	ctx := context.Background()
	// Two pools, each one set of 4 drives. Objects spread across both; every
	// object must round-trip regardless of which pool/set it routes to.
	mk := func() []storage.StorageAPI {
		ds := make([]storage.StorageAPI, 4)
		for i := range ds {
			d, err := local.New(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			ds[i] = d
		}
		return ds
	}
	sp, err := NewServerPools(depID(3), []PoolConfig{
		{Sets: []SetConfig{{Drives: mk(), Parity: 2}}},
		{Sets: []SetConfig{{Drives: mk(), Parity: 2}}},
	})
	if err != nil {
		t.Fatalf("NewServerPools: %v", err)
	}
	mustMakeBucket(t, sp, "b")
	for i := 0; i < 20; i++ {
		k := fmt.Sprintf("obj-%02d", i)
		putGet(t, sp, "b", k, []byte(k+"-payload"), ObjectOptions{})
	}
	list, err := sp.ListObjectsV2(ctx, "b", "", "", "", "", 1000, false)
	if err != nil || len(list.Objects) != 20 {
		t.Fatalf("ListObjectsV2 = %d objects, %v", len(list.Objects), err)
	}
}

func TestHTTPRangeSpecGetOffsetLength(t *testing.T) {
	const size = 100
	cases := []struct {
		name              string
		spec              *HTTPRangeSpec
		wantStart, wantLn int64
		wantErr           bool
	}{
		{"nil covers whole object", nil, 0, size, false},
		{"closed range", &HTTPRangeSpec{Start: 10, End: 19}, 10, 10, false},
		{"open range to end", &HTTPRangeSpec{Start: 90, End: -1}, 90, 10, false},
		{"end past size clamps", &HTTPRangeSpec{Start: 95, End: 1000}, 95, 5, false},
		{"single byte", &HTTPRangeSpec{Start: 0, End: 0}, 0, 1, false},
		{"suffix", &HTTPRangeSpec{IsSuffix: true, Start: 20}, 80, 20, false},
		{"suffix larger than size clamps", &HTTPRangeSpec{IsSuffix: true, Start: 1000}, 0, size, false},
		{"start past end of object", &HTTPRangeSpec{Start: 100, End: -1}, 0, 0, true},
		{"inverted bounds", &HTTPRangeSpec{Start: 50, End: 40}, 0, 0, true},
		{"empty suffix", &HTTPRangeSpec{IsSuffix: true, Start: 0}, 0, 0, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			start, ln, err := c.spec.GetOffsetLength(size)
			if c.wantErr {
				if !errors.Is(err, ErrInvalidRange) {
					t.Fatalf("err = %v, want ErrInvalidRange", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
			if start != c.wantStart || ln != c.wantLn {
				t.Fatalf("offset/length = %d/%d, want %d/%d", start, ln, c.wantStart, c.wantLn)
			}
		})
	}
}

func TestGetObjectRange(t *testing.T) {
	sp := newLayer(t, 6, 2)
	mustMakeBucket(t, sp, "b")
	// An out-of-line object so the range slices erasure-decoded bytes.
	body := make([]byte, 50<<10)
	for i := range body {
		body[i] = byte(i)
	}
	if _, err := sp.PutObject(context.Background(), "b", "k",
		NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
		t.Fatal(err)
	}

	// Mid-object closed range.
	got := getBytes(t, sp, "b", "k", ObjectOptions{Range: &HTTPRangeSpec{Start: 1000, End: 1999}})
	if !bytes.Equal(got, body[1000:2000]) {
		t.Fatalf("closed range mismatch: %d bytes", len(got))
	}
	// Suffix range.
	got = getBytes(t, sp, "b", "k", ObjectOptions{Range: &HTTPRangeSpec{IsSuffix: true, Start: 256}})
	if !bytes.Equal(got, body[len(body)-256:]) {
		t.Fatalf("suffix range mismatch: %d bytes", len(got))
	}
	// Unsatisfiable range surfaces ErrInvalidRange.
	_, err := sp.GetObject(context.Background(), "b", "k", ObjectOptions{Range: &HTTPRangeSpec{Start: 1 << 30, End: -1}})
	if !errors.Is(err, ErrInvalidRange) {
		t.Fatalf("err = %v, want ErrInvalidRange", err)
	}
}

// --- helpers and fault-injection drives ----------------------------------

func mustMakeBucket(t *testing.T, sp *ServerPools, name string) {
	t.Helper()
	if err := sp.MakeBucket(context.Background(), name, MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket %s: %v", name, err)
	}
}

func putVersion(t *testing.T, sp *ServerPools, bucket, key string, body []byte) ObjectInfo {
	t.Helper()
	oi, err := sp.PutObject(context.Background(), bucket, key, NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	return oi
}

// offlineDrive simulates a downed drive: every operation fails and it reports
// offline, so the set treats it as missing.
type offlineDrive struct{ storage.StorageAPI }

func (offlineDrive) IsOnline() bool { return false }
func (o offlineDrive) ReadMeta(context.Context, string, string) ([]byte, error) {
	return nil, storage.ErrDriveOffline
}
func (o offlineDrive) ReadFileStream(context.Context, string, string, int64, int64) (io.ReadCloser, error) {
	return nil, storage.ErrDriveOffline
}
func (o offlineDrive) WriteMeta(context.Context, string, string, []byte) error {
	return storage.ErrDriveOffline
}
func (o offlineDrive) CreateFile(context.Context, string, string, int64, io.Reader) error {
	return storage.ErrDriveOffline
}
func (o offlineDrive) RenameData(context.Context, string, string, string) error {
	return storage.ErrDriveOffline
}

// corruptDrive flips bytes in any shard it serves, exercising bitrot detection.
type corruptDrive struct{ storage.StorageAPI }

func (c corruptDrive) ReadFileStream(ctx context.Context, vol, p string, off, length int64) (io.ReadCloser, error) {
	rc, err := c.StorageAPI.ReadFileStream(ctx, vol, p, off, length)
	if err != nil {
		return nil, err
	}
	data, _ := io.ReadAll(rc)
	_ = rc.Close()
	if len(data) > 0 {
		data[0] ^= 0xff
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func BenchmarkPutInline(b *testing.B) {
	sp := benchLayer(b, 6, 3)
	body := bytes.Repeat([]byte{0xab}, 64<<10)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := sp.PutObject(context.Background(), "b", "k",
			NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkGetOutOfLine(b *testing.B) {
	sp := benchLayer(b, 6, 3)
	body := bytes.Repeat([]byte{0xcd}, 4<<20)
	if _, err := sp.PutObject(context.Background(), "b", "k",
		NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		r, err := sp.GetObject(context.Background(), "b", "k", ObjectOptions{})
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, r); err != nil {
			b.Fatal(err)
		}
		_ = r.Close()
	}
}

func benchLayer(b *testing.B, n, parity int) *ServerPools {
	b.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(b.TempDir())
		if err != nil {
			b.Fatal(err)
		}
		drives[i] = d
	}
	sp, err := NewSingleSet(depID(7), drives, parity)
	if err != nil {
		b.Fatal(err)
	}
	if err := sp.MakeBucket(context.Background(), "b", MakeBucketOptions{}); err != nil {
		b.Fatal(err)
	}
	return sp
}
