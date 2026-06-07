// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"strconv"
	"testing"
)

func TestCopyObjectAcrossKeys(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "src")
	mustMakeBucket(t, sp, "dst")

	body := bytes.Repeat([]byte("payload"), 200_000) // ~1.4 MiB, out of line
	src, err := sp.PutObject(ctx, "src", "a/orig.bin",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{ContentType: "application/octet-stream", UserDefined: map[string]string{"content-type": "application/octet-stream", "x-amz-meta-team": "blue"}})
	if err != nil {
		t.Fatalf("PutObject src: %v", err)
	}

	// COPY keeps the source metadata; the bytes and ETag match the source.
	out, err := sp.CopyObject(ctx, "src", "a/orig.bin", "dst", "b/copy.bin", src,
		ObjectOptions{ContentType: src.ContentType, UserDefined: src.UserDefined})
	if err != nil {
		t.Fatalf("CopyObject: %v", err)
	}
	if out.ETag != src.ETag {
		t.Fatalf("copy ETag = %q, want %q", out.ETag, src.ETag)
	}
	if got := getBytes(t, sp, "dst", "b/copy.bin", ObjectOptions{}); !bytes.Equal(got, body) {
		t.Fatalf("copied bytes differ: %d back, want %d", len(got), len(body))
	}
	info, err := sp.GetObjectInfo(ctx, "dst", "b/copy.bin", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo dst: %v", err)
	}
	if info.UserDefined["x-amz-meta-team"] != "blue" {
		t.Fatalf("copied metadata = %+v, want team=blue", info.UserDefined)
	}
}

func TestCopyObjectReplaceMetadata(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	body := []byte("small body")
	src, err := sp.PutObject(ctx, "b", "k",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{UserDefined: map[string]string{"x-amz-meta-old": "1"}})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// REPLACE onto the same key swaps the metadata while the bytes survive.
	if _, err := sp.CopyObject(ctx, "b", "k", "b", "k", src,
		ObjectOptions{UserDefined: map[string]string{"x-amz-meta-new": "2"}}); err != nil {
		t.Fatalf("CopyObject replace: %v", err)
	}
	info, err := sp.GetObjectInfo(ctx, "b", "k", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo: %v", err)
	}
	if info.UserDefined["x-amz-meta-new"] != "2" || info.UserDefined["x-amz-meta-old"] != "" {
		t.Fatalf("metadata after replace = %+v", info.UserDefined)
	}
	if got := getBytes(t, sp, "b", "k", ObjectOptions{}); !bytes.Equal(got, body) {
		t.Fatalf("bytes after replace differ")
	}
}

func TestCopyObjectPartRange(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	// A 6 MiB source; the multipart object copies its first 5 MiB as part 1 and a
	// small ranged tail as part 2.
	source := make([]byte, 6<<20)
	for i := range source {
		source[i] = byte(i)
	}
	src, err := sp.PutObject(ctx, "b", "source.bin",
		NewPutReader(bytes.NewReader(source), int64(len(source))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject source: %v", err)
	}

	uploadID, err := sp.NewMultipartUpload(ctx, "b", "joined.bin", ObjectOptions{})
	if err != nil {
		t.Fatalf("NewMultipartUpload: %v", err)
	}

	p1, err := sp.CopyObjectPart(ctx, "b", "source.bin", "b", "joined.bin", uploadID, 1, src,
		&HTTPRangeSpec{Start: 0, End: MinPartSize - 1}, ObjectOptions{})
	if err != nil {
		t.Fatalf("CopyObjectPart 1: %v", err)
	}
	p2, err := sp.CopyObjectPart(ctx, "b", "source.bin", "b", "joined.bin", uploadID, 2, src,
		&HTTPRangeSpec{Start: MinPartSize, End: int64(len(source)) - 1}, ObjectOptions{})
	if err != nil {
		t.Fatalf("CopyObjectPart 2: %v", err)
	}

	if _, err := sp.CompleteMultipartUpload(ctx, "b", "joined.bin", uploadID,
		[]CompletePart{{PartNumber: 1, ETag: p1.ETag}, {PartNumber: 2, ETag: p2.ETag}}, ObjectOptions{}); err != nil {
		t.Fatalf("CompleteMultipartUpload: %v", err)
	}

	if got := getBytes(t, sp, "b", "joined.bin", ObjectOptions{}); !bytes.Equal(got, source) {
		t.Fatalf("reassembled %d bytes, want %d", len(got), len(source))
	}
}

func TestCopyObjectMissingSource(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	if _, err := sp.CopyObject(ctx, "b", "ghost", "b", "dst", ObjectInfo{}, ObjectOptions{}); err == nil {
		t.Fatal("CopyObject of missing source: want error, got nil")
	}
}

// BenchmarkCopyObject measures the read-then-write copy of a 4 MiB object. The
// cost is dominated by erasure-decoding the source and re-encoding the
// destination; a same-set shard-level copy is a documented later optimization.
func BenchmarkCopyObject(b *testing.B) {
	ctx := context.Background()
	sp := benchLayer(b, 6, 3)
	body := bytes.Repeat([]byte{0xab}, 4<<20)
	if _, err := sp.PutObject(ctx, "b", "src", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{}); err != nil {
		b.Fatal(err)
	}
	src, err := sp.GetObjectInfo(ctx, "b", "src", ObjectOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for i := 0; b.Loop(); i++ {
		if _, err := sp.CopyObject(ctx, "b", "src", "b", "dst-"+strconv.Itoa(i), src, ObjectOptions{}); err != nil {
			b.Fatal(err)
		}
	}
}
