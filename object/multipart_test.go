// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"strconv"
	"testing"
)

// uploadPart uploads one part and returns its ETag.
func uploadPart(t *testing.T, sp *ServerPools, bucket, key, uploadID string, num int, body []byte) string {
	t.Helper()
	pi, err := sp.PutObjectPart(context.Background(), bucket, key, uploadID, num,
		NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObjectPart %d: %v", num, err)
	}
	if pi.Size != int64(len(body)) {
		t.Fatalf("part %d size = %d, want %d", num, pi.Size, len(body))
	}
	return pi.ETag
}

// compositeETag computes the expected MD5(part-MD5s)-N wire ETag for parts.
func compositeETag(parts [][]byte) string {
	var buf []byte
	for _, p := range parts {
		sum := md5.Sum(p)
		buf = append(buf, sum[:]...)
	}
	sum := md5.Sum(buf)
	return hex.EncodeToString(sum[:]) + "-" + strconv.Itoa(len(parts))
}

func TestMultipartRoundTrip(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	uploadID, err := sp.NewMultipartUpload(ctx, "b", "big.bin", ObjectOptions{ContentType: "application/zip"})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	if uploadID == "" {
		t.Fatal("empty upload id")
	}

	// Two 5 MiB parts plus a small final part; only the last may be under 5 MiB.
	p1 := bytes.Repeat([]byte("A"), MinPartSize)
	p2 := bytes.Repeat([]byte("B"), MinPartSize)
	p3 := bytes.Repeat([]byte("C"), 1024)
	e1 := uploadPart(t, sp, "b", "big.bin", uploadID, 1, p1)
	e2 := uploadPart(t, sp, "b", "big.bin", uploadID, 2, p2)
	e3 := uploadPart(t, sp, "b", "big.bin", uploadID, 3, p3)

	// ListParts reflects the uploaded parts in order.
	lp, err := sp.ListObjectParts(ctx, "b", "big.bin", uploadID, 0, 1000, ObjectOptions{})
	if err != nil {
		t.Fatalf("ListObjectParts: %v", err)
	}
	if len(lp.Parts) != 3 || lp.Parts[0].PartNumber != 1 || lp.Parts[2].PartNumber != 3 {
		t.Fatalf("ListObjectParts = %+v", lp.Parts)
	}

	oi, err := sp.CompleteMultipartUpload(ctx, "b", "big.bin", uploadID, []CompletePart{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
		{PartNumber: 3, ETag: e3},
	}, ObjectOptions{})
	if err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	want := compositeETag([][]byte{p1, p2, p3})
	if oi.ETag != want {
		t.Fatalf("composite ETag = %q, want %q", oi.ETag, want)
	}
	wantSize := int64(len(p1) + len(p2) + len(p3))
	if oi.Size != wantSize {
		t.Fatalf("size = %d, want %d", oi.Size, wantSize)
	}

	// The assembled object reads back as the concatenation of its parts.
	full := append(append(append([]byte{}, p1...), p2...), p3...)
	got := getBytes(t, sp, "b", "big.bin", ObjectOptions{})
	if !bytes.Equal(got, full) {
		t.Fatalf("assembled object: %d bytes, want %d", len(got), len(full))
	}

	// A ranged read spanning the part boundary returns the right window.
	window := getBytes(t, sp, "b", "big.bin", ObjectOptions{Range: &HTTPRangeSpec{Start: int64(MinPartSize - 2), End: int64(MinPartSize + 1)}})
	if !bytes.Equal(window, full[MinPartSize-2:MinPartSize+2]) {
		t.Fatalf("ranged read across boundary = %q", window)
	}

	// The staging tree is gone after completion.
	if _, err := sp.ListObjectParts(ctx, "b", "big.bin", uploadID, 0, 1000, ObjectOptions{}); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("ListObjectParts after complete = %v, want ErrNoSuchUpload", err)
	}
	if oi.ContentType != "application/zip" {
		t.Fatalf("content-type = %q", oi.ContentType)
	}
}

func TestMultipartValidation(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")

	uploadID, err := sp.NewMultipartUpload(ctx, "b", "k", ObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	p1 := bytes.Repeat([]byte("A"), MinPartSize)
	p2 := bytes.Repeat([]byte("B"), 4<<20) // below 5 MiB
	e1 := uploadPart(t, sp, "b", "k", uploadID, 1, p1)
	e2 := uploadPart(t, sp, "b", "k", uploadID, 2, p2)

	// Out-of-order parts are rejected.
	if _, err := sp.CompleteMultipartUpload(ctx, "b", "k", uploadID, []CompletePart{
		{PartNumber: 2, ETag: e2}, {PartNumber: 1, ETag: e1},
	}, ObjectOptions{}); !errors.Is(err, ErrInvalidPartOrder) {
		t.Fatalf("out-of-order complete = %v, want ErrInvalidPartOrder", err)
	}

	// A wrong ETag is an invalid part.
	if _, err := sp.CompleteMultipartUpload(ctx, "b", "k", uploadID, []CompletePart{
		{PartNumber: 1, ETag: "deadbeef"},
	}, ObjectOptions{}); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("bad-etag complete = %v, want ErrInvalidPart", err)
	}

	// A missing part number is an invalid part.
	if _, err := sp.CompleteMultipartUpload(ctx, "b", "k", uploadID, []CompletePart{
		{PartNumber: 9, ETag: e1},
	}, ObjectOptions{}); !errors.Is(err, ErrInvalidPart) {
		t.Fatalf("missing-part complete = %v, want ErrInvalidPart", err)
	}

	// A non-final part below 5 MiB is too small. p2 (4 MiB) is fine as the last
	// part, so add a small final part to push it into the middle.
	p3 := bytes.Repeat([]byte("C"), 1024)
	e3 := uploadPart(t, sp, "b", "k", uploadID, 3, p3)
	if _, err := sp.CompleteMultipartUpload(ctx, "b", "k", uploadID, []CompletePart{
		{PartNumber: 1, ETag: e1}, {PartNumber: 2, ETag: e2}, {PartNumber: 3, ETag: e3},
	}, ObjectOptions{}); !errors.Is(err, ErrEntityTooSmall) {
		t.Fatalf("small middle part complete = %v, want ErrEntityTooSmall", err)
	}
}

func TestMultipartAbort(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")

	uploadID, err := sp.NewMultipartUpload(ctx, "b", "k", ObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	uploadPart(t, sp, "b", "k", uploadID, 1, bytes.Repeat([]byte("A"), MinPartSize))

	if err := sp.AbortMultipartUpload(ctx, "b", "k", uploadID, ObjectOptions{}); err != nil {
		t.Fatalf("AbortMultipartUpload: %v", err)
	}
	if err := sp.AbortMultipartUpload(ctx, "b", "k", uploadID, ObjectOptions{}); !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("double abort = %v, want ErrNoSuchUpload", err)
	}
	// The key never became a real object.
	if _, err := sp.GetObjectInfo(ctx, "b", "k", ObjectOptions{}); !errors.Is(err, ErrObjectNotFound) {
		t.Fatalf("GetObjectInfo after abort = %v, want ErrObjectNotFound", err)
	}
}

func TestMultipartUnknownUpload(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")

	_, err := sp.PutObjectPart(ctx, "b", "k", "no-such-id", 1,
		NewPutReader(bytes.NewReader([]byte("x")), 1), ObjectOptions{})
	if !errors.Is(err, ErrNoSuchUpload) {
		t.Fatalf("PutObjectPart unknown upload = %v, want ErrNoSuchUpload", err)
	}
}

func TestMultipartList(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")

	ids := map[string]string{}
	for _, key := range []string{"a/1", "a/2", "b/1"} {
		id, err := sp.NewMultipartUpload(ctx, "b", key, ObjectOptions{})
		if err != nil {
			t.Fatalf("NewMultipartUpload %s: %v", key, err)
		}
		ids[key] = id
	}

	all, err := sp.ListMultipartUploads(ctx, "b", "", "", "", "", 1000)
	if err != nil {
		t.Fatalf("ListMultipartUploads: %v", err)
	}
	if len(all.Uploads) != 3 {
		t.Fatalf("listed %d uploads, want 3", len(all.Uploads))
	}

	// Prefix filtering narrows to the a/ keys.
	pref, err := sp.ListMultipartUploads(ctx, "b", "a/", "", "", "", 1000)
	if err != nil {
		t.Fatalf("ListMultipartUploads prefix: %v", err)
	}
	if len(pref.Uploads) != 2 {
		t.Fatalf("prefix a/ listed %d, want 2", len(pref.Uploads))
	}

	// Pagination caps the page and reports a cursor.
	page, err := sp.ListMultipartUploads(ctx, "b", "", "", "", "", 1)
	if err != nil {
		t.Fatalf("ListMultipartUploads page: %v", err)
	}
	if len(page.Uploads) != 1 || !page.IsTruncated || page.NextKeyMarker == "" {
		t.Fatalf("first page = %+v", page)
	}
	page2, err := sp.ListMultipartUploads(ctx, "b", "", page.NextKeyMarker, page.NextUploadIDMarker, "", 1000)
	if err != nil {
		t.Fatalf("ListMultipartUploads page2: %v", err)
	}
	if len(page2.Uploads) != 2 {
		t.Fatalf("second page listed %d, want 2", len(page2.Uploads))
	}
}

// BenchmarkCompleteMultipartUpload measures assembling a 4-part object: the
// completion path is pure metadata plus renames (no data rewrite), so it should
// stay cheap regardless of part size.
func BenchmarkCompleteMultipartUpload(b *testing.B) {
	ctx := context.Background()
	sp := benchLayer(b, 6, 3)
	part := bytes.Repeat([]byte{0xab}, MinPartSize)
	last := bytes.Repeat([]byte{0xcd}, 4096)
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		key := "obj-" + strconv.Itoa(i)
		uploadID, err := sp.NewMultipartUpload(ctx, "b", key, ObjectOptions{})
		if err != nil {
			b.Fatal(err)
		}
		cps := make([]CompletePart, 0, 4)
		for n := 1; n <= 4; n++ {
			body := part
			if n == 4 {
				body = last
			}
			pi, err := sp.PutObjectPart(ctx, "b", key, uploadID, n,
				NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
			if err != nil {
				b.Fatal(err)
			}
			cps = append(cps, CompletePart{PartNumber: n, ETag: pi.ETag})
		}
		if _, err := sp.CompleteMultipartUpload(ctx, "b", key, uploadID, cps, ObjectOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}

// A completed multipart object survives the loss of up to M drives, decoding
// each part from the surviving shards.
func TestMultipartDegradedRead(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 8, 4) // K=4, M=4
	mustMakeBucket(t, sp, "b")

	uploadID, err := sp.NewMultipartUpload(ctx, "b", "k", ObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}
	p1 := bytes.Repeat([]byte("X"), MinPartSize)
	p2 := bytes.Repeat([]byte("Y"), 2048)
	e1 := uploadPart(t, sp, "b", "k", uploadID, 1, p1)
	e2 := uploadPart(t, sp, "b", "k", uploadID, 2, p2)
	if _, err := sp.CompleteMultipartUpload(ctx, "b", "k", uploadID, []CompletePart{
		{PartNumber: 1, ETag: e1}, {PartNumber: 2, ETag: e2},
	}, ObjectOptions{}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	set := sp.allSets()[0]
	for i := range 4 { // drop M=4 drives
		set.drives[i] = offlineDrive{set.drives[i]}
	}

	full := append(append([]byte{}, p1...), p2...)
	got := getBytes(t, sp, "b", "k", ObjectOptions{})
	if !bytes.Equal(got, full) {
		t.Fatalf("degraded read back %d bytes, want %d", len(got), len(full))
	}
}
