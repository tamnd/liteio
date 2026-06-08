// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// samplePolicy is a minimal valid-shaped bucket policy document. The object layer
// stores it opaquely, so its content only has to round-trip byte-for-byte.
var samplePolicy = []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)

func TestSetGetBucketPolicy(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	// No policy yet.
	if _, err := sp.GetBucketPolicy(ctx, "b"); !errors.Is(err, ErrNoSuchBucketPolicy) {
		t.Fatalf("GetBucketPolicy before set: got %v, want ErrNoSuchBucketPolicy", err)
	}

	if err := sp.SetBucketPolicy(ctx, "b", samplePolicy); err != nil {
		t.Fatalf("SetBucketPolicy: %v", err)
	}
	got, err := sp.GetBucketPolicy(ctx, "b")
	if err != nil {
		t.Fatalf("GetBucketPolicy after set: %v", err)
	}
	if !bytes.Equal(got, samplePolicy) {
		t.Fatalf("policy round-trip mismatch:\n got %s\nwant %s", got, samplePolicy)
	}
}

func TestSetBucketPolicyOverwrite(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	if err := sp.SetBucketPolicy(ctx, "b", samplePolicy); err != nil {
		t.Fatalf("first SetBucketPolicy: %v", err)
	}
	replacement := []byte(`{"Version":"2012-10-17","Statement":[{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := sp.SetBucketPolicy(ctx, "b", replacement); err != nil {
		t.Fatalf("overwrite SetBucketPolicy: %v", err)
	}
	got, err := sp.GetBucketPolicy(ctx, "b")
	if err != nil {
		t.Fatalf("GetBucketPolicy: %v", err)
	}
	if !bytes.Equal(got, replacement) {
		t.Fatalf("overwrite did not replace: got %s", got)
	}
}

func TestDeleteBucketPolicy(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	if err := sp.SetBucketPolicy(ctx, "b", samplePolicy); err != nil {
		t.Fatalf("SetBucketPolicy: %v", err)
	}
	if err := sp.DeleteBucketPolicy(ctx, "b"); err != nil {
		t.Fatalf("DeleteBucketPolicy: %v", err)
	}
	if _, err := sp.GetBucketPolicy(ctx, "b"); !errors.Is(err, ErrNoSuchBucketPolicy) {
		t.Fatalf("GetBucketPolicy after delete: got %v, want ErrNoSuchBucketPolicy", err)
	}
}

func TestDeleteBucketPolicyIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)
	mustMakeBucket(t, sp, "b")

	// Deleting when no policy is set succeeds, matching S3's DeleteBucketPolicy.
	if err := sp.DeleteBucketPolicy(ctx, "b"); err != nil {
		t.Fatalf("DeleteBucketPolicy with no policy: %v", err)
	}
}

func TestBucketPolicyMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 6, 3)

	if err := sp.SetBucketPolicy(ctx, "ghost", samplePolicy); !errors.Is(err, ErrBucketNotFound) {
		t.Errorf("SetBucketPolicy on missing bucket: got %v, want ErrBucketNotFound", err)
	}
	if _, err := sp.GetBucketPolicy(ctx, "ghost"); !errors.Is(err, ErrBucketNotFound) {
		t.Errorf("GetBucketPolicy on missing bucket: got %v, want ErrBucketNotFound", err)
	}
	if err := sp.DeleteBucketPolicy(ctx, "ghost"); !errors.Is(err, ErrBucketNotFound) {
		t.Errorf("DeleteBucketPolicy on missing bucket: got %v, want ErrBucketNotFound", err)
	}
}

func BenchmarkGetBucketPolicy(b *testing.B) {
	ctx := context.Background()
	sp := newLayer(b, 6, 3)
	if err := sp.MakeBucket(ctx, "b", MakeBucketOptions{}); err != nil {
		b.Fatalf("MakeBucket: %v", err)
	}
	if err := sp.SetBucketPolicy(ctx, "b", samplePolicy); err != nil {
		b.Fatalf("SetBucketPolicy: %v", err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := sp.GetBucketPolicy(ctx, "b"); err != nil {
			b.Fatalf("GetBucketPolicy: %v", err)
		}
	}
}
