// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"testing"
)

// putVersioned writes one version of a key into a versioned bucket and returns it.
func putVersioned(t *testing.T, sp *ServerPools, bucket, key string, body []byte) ObjectInfo {
	t.Helper()
	oi, err := sp.PutObject(context.Background(), bucket, key,
		NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject %s: %v", key, err)
	}
	if oi.VersionID == "" {
		t.Fatalf("PutObject %s in versioned bucket returned empty version id", key)
	}
	return oi
}

func makeVersionedBucket(t *testing.T, sp *ServerPools, name string) {
	t.Helper()
	if err := sp.MakeBucket(context.Background(), name, MakeBucketOptions{VersionedDefault: true}); err != nil {
		t.Fatalf("MakeBucket %s: %v", name, err)
	}
}

func TestListObjectVersions(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	makeVersionedBucket(t, sp, "b")

	// Three versions of one key, plus a single version of another.
	v1 := putVersioned(t, sp, "b", "doc.txt", []byte("one"))
	v2 := putVersioned(t, sp, "b", "doc.txt", []byte("two two"))
	v3 := putVersioned(t, sp, "b", "doc.txt", []byte("three three three"))
	other := putVersioned(t, sp, "b", "img.bin", []byte("img"))

	res, err := sp.ListObjectVersions(ctx, "b", "", "", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if len(res.Objects) != 4 {
		t.Fatalf("listed %d versions, want 4", len(res.Objects))
	}

	// doc.txt comes first (key order); within it, newest-first with IsLatest set.
	doc := res.Objects[:3]
	if doc[0].VersionID != v3.VersionID || doc[1].VersionID != v2.VersionID || doc[2].VersionID != v1.VersionID {
		t.Fatalf("doc.txt version order = %s,%s,%s", doc[0].VersionID, doc[1].VersionID, doc[2].VersionID)
	}
	if !doc[0].IsLatest || doc[1].IsLatest || doc[2].IsLatest {
		t.Fatalf("doc.txt IsLatest flags wrong")
	}
	if res.Objects[3].Name != "img.bin" || res.Objects[3].VersionID != other.VersionID {
		t.Fatalf("last entry = %+v, want img.bin latest", res.Objects[3])
	}
}

func TestListObjectVersionsWithDeleteMarker(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	makeVersionedBucket(t, sp, "b")

	putVersioned(t, sp, "b", "k", []byte("data"))
	// A versioned delete adds a delete marker as the new latest version.
	dm, err := sp.DeleteObject(ctx, "b", "k", ObjectOptions{})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}
	if !dm.DeleteMarker {
		t.Fatal("DeleteObject in versioned bucket should create a delete marker")
	}

	res, err := sp.ListObjectVersions(ctx, "b", "", "", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectVersions: %v", err)
	}
	if len(res.Objects) != 2 {
		t.Fatalf("listed %d entries, want 2 (version + marker)", len(res.Objects))
	}
	if !res.Objects[0].DeleteMarker || !res.Objects[0].IsLatest {
		t.Fatalf("latest entry should be the delete marker: %+v", res.Objects[0])
	}
	if res.Objects[1].DeleteMarker {
		t.Fatalf("older entry should be the real version: %+v", res.Objects[1])
	}
}

func TestListObjectVersionsPagination(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	makeVersionedBucket(t, sp, "b")

	putVersioned(t, sp, "b", "a", []byte("a1"))
	putVersioned(t, sp, "b", "a", []byte("a2"))
	putVersioned(t, sp, "b", "b", []byte("b1"))

	// First page of 2 truncates and reports a cursor.
	p1, err := sp.ListObjectVersions(ctx, "b", "", "", "", "", 2)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(p1.Objects) != 2 || !p1.IsTruncated || p1.NextKeyMarker == "" {
		t.Fatalf("page 1 = %+v", p1)
	}

	// Second page resumes after the cursor and returns the rest.
	p2, err := sp.ListObjectVersions(ctx, "b", "", p1.NextKeyMarker, p1.NextVersionIDMarker, "", 1000)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(p2.Objects) != 1 || p2.Objects[0].Name != "b" {
		t.Fatalf("page 2 = %+v, want single b", p2.Objects)
	}
}

func TestListObjectVersionsPrefixAndDelimiter(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	makeVersionedBucket(t, sp, "b")

	putVersioned(t, sp, "b", "photos/2024/jan.jpg", []byte("j"))
	putVersioned(t, sp, "b", "photos/2024/feb.jpg", []byte("f"))
	putVersioned(t, sp, "b", "notes.txt", []byte("n"))

	// Delimiter rolls the photos/ keys into one common prefix.
	res, err := sp.ListObjectVersions(ctx, "b", "", "", "", "/", 1000)
	if err != nil {
		t.Fatalf("ListObjectVersions delimiter: %v", err)
	}
	if len(res.Prefixes) != 1 || res.Prefixes[0] != "photos/" {
		t.Fatalf("prefixes = %v, want [photos/]", res.Prefixes)
	}
	// notes.txt has no delimiter past the prefix, so it lists as a version.
	if len(res.Objects) != 1 || res.Objects[0].Name != "notes.txt" {
		t.Fatalf("objects = %+v, want [notes.txt]", res.Objects)
	}

	// Prefix narrows to the photos tree.
	pf, err := sp.ListObjectVersions(ctx, "b", "photos/2024/", "", "", "", 1000)
	if err != nil {
		t.Fatalf("ListObjectVersions prefix: %v", err)
	}
	if len(pf.Objects) != 2 {
		t.Fatalf("prefix listed %d, want 2", len(pf.Objects))
	}
}
