// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"testing"
)

// --- unit: tag encoding --------------------------------------------------

func TestEncodeDecodeTagsRoundTrip(t *testing.T) {
	want := map[string]string{"env": "prod", "tier": "hot"}
	enc := EncodeTags(want)
	got, err := DecodeTags(enc)
	if err != nil {
		t.Fatalf("DecodeTags: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("key %q: got %q, want %q", k, got[k], v)
		}
	}
}

func TestEncodeTagsEmptyMap(t *testing.T) {
	if got := EncodeTags(nil); got != "" {
		t.Fatalf("nil = %q, want empty", got)
	}
	if got := EncodeTags(map[string]string{}); got != "" {
		t.Fatalf("empty = %q, want empty", got)
	}
}

func TestDecodeTagsEmpty(t *testing.T) {
	got, err := DecodeTags("")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got %v, want empty", got)
	}
}

func TestValidateTags(t *testing.T) {
	cases := []struct {
		name    string
		tags    map[string]string
		max     int
		wantErr bool
	}{
		{"ok", map[string]string{"env": "prod"}, 10, false},
		{"too many", map[string]string{"a": "1", "b": "2"}, 1, true},
		{"empty key", map[string]string{"": "v"}, 10, true},
		{"key too long", map[string]string{string(bytes.Repeat([]byte("k"), 129)): "v"}, 10, true},
		{"value too long", map[string]string{"k": string(bytes.Repeat([]byte("v"), 257))}, 10, true},
	}
	for _, tc := range cases {
		err := ValidateTags(tc.tags, tc.max)
		if (err != nil) != tc.wantErr {
			t.Errorf("%s: err=%v, wantErr=%v", tc.name, err, tc.wantErr)
		}
	}
}

// --- integration: object tags --------------------------------------------

func TestSetGetDeleteObjectTags(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	body := []byte("hello")
	_, err := sp.PutObject(ctx, "bkt", "obj.txt",
		NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	tags := map[string]string{"project": "alpha", "cost-center": "42"}
	if err := sp.SetObjectTags(ctx, "bkt", "obj.txt", "", tags); err != nil {
		t.Fatalf("SetObjectTags: %v", err)
	}

	got, err := sp.GetObjectTags(ctx, "bkt", "obj.txt", "")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	for k, v := range tags {
		if got[k] != v {
			t.Errorf("tag %q: got %q, want %q", k, got[k], v)
		}
	}

	// Delete clears all tags; the object remains.
	if err := sp.DeleteObjectTags(ctx, "bkt", "obj.txt", ""); err != nil {
		t.Fatalf("DeleteObjectTags: %v", err)
	}
	after, err := sp.GetObjectTags(ctx, "bkt", "obj.txt", "")
	if err != nil {
		t.Fatalf("GetObjectTags after delete: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("after delete: %v, want empty", after)
	}

	// Object bytes are intact.
	if got := getBytes(t, sp, "bkt", "obj.txt", ObjectOptions{}); !bytes.Equal(got, body) {
		t.Fatalf("body mismatch after tagging")
	}
}

func TestGetObjectTagsMissingObject(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	_, err := sp.GetObjectTags(ctx, "bkt", "missing.txt", "")
	if err == nil {
		t.Fatal("expected error for missing object")
	}
}

func TestSetObjectTagsMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	err := sp.SetObjectTags(ctx, "no-bucket", "obj.txt", "", map[string]string{"k": "v"})
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

// TestObjectTagsSurvivePutAndGet checks that tags written via the x-amz-tagging
// header on PutObject are readable through GetObjectTags.
func TestObjectTagsSurvivePutAndGet(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	body := []byte("data")
	_, err := sp.PutObject(ctx, "bkt", "tagged.txt",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{UserDefined: map[string]string{
			"x-amz-tagging": "color=blue&size=small",
		}})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	tags, err := sp.GetObjectTags(ctx, "bkt", "tagged.txt", "")
	if err != nil {
		t.Fatalf("GetObjectTags: %v", err)
	}
	if tags["color"] != "blue" {
		t.Errorf("color = %q, want blue", tags["color"])
	}
	if tags["size"] != "small" {
		t.Errorf("size = %q, want small", tags["size"])
	}
}

// --- integration: bucket tags --------------------------------------------

func TestSetGetDeleteBucketTagging(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	tags := map[string]string{"owner": "ops", "tier": "prod"}
	doc, err := BucketTagsToJSON(tags)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := sp.SetBucketTagging(ctx, "bkt", doc); err != nil {
		t.Fatalf("SetBucketTagging: %v", err)
	}

	raw, err := sp.GetBucketTagging(ctx, "bkt")
	if err != nil {
		t.Fatalf("GetBucketTagging: %v", err)
	}
	// The stored bytes must survive a JSON round-trip.
	got, err2 := bucketTagsFromJSON(raw)
	if err2 != nil {
		t.Fatalf("parse returned doc: %v", err2)
	}
	for k, v := range tags {
		if got[k] != v {
			t.Errorf("bucket tag %q: got %q, want %q", k, got[k], v)
		}
	}

	if err := sp.DeleteBucketTagging(ctx, "bkt"); err != nil {
		t.Fatalf("DeleteBucketTagging: %v", err)
	}
	_, err = sp.GetBucketTagging(ctx, "bkt")
	if err == nil {
		t.Fatal("expected ErrNoSuchBucketTagging after delete")
	}
}

func TestDeleteBucketTaggingIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	// Deleting tags from a bucket that was never tagged must succeed.
	if err := sp.DeleteBucketTagging(ctx, "bkt"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := sp.DeleteBucketTagging(ctx, "bkt"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
