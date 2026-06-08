// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"testing"
)

func TestSetGetDeleteBucketCORS(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	cfg := CORSConfig{
		Rules: []CORSRule{{
			ID:             "r1",
			AllowedOrigins: []string{"https://example.com"},
			AllowedMethods: []string{"GET", "PUT"},
			AllowedHeaders: []string{"Content-Type"},
			ExposeHeaders:  []string{"ETag"},
			MaxAgeSeconds:  300,
		}},
	}

	if err := sp.SetBucketCORS(ctx, "bkt", cfg); err != nil {
		t.Fatalf("SetBucketCORS: %v", err)
	}

	got, err := sp.GetBucketCORS(ctx, "bkt")
	if err != nil {
		t.Fatalf("GetBucketCORS: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("got %d rules, want 1", len(got.Rules))
	}
	r := got.Rules[0]
	if r.ID != "r1" {
		t.Errorf("ID = %q, want r1", r.ID)
	}
	if len(r.AllowedOrigins) != 1 || r.AllowedOrigins[0] != "https://example.com" {
		t.Errorf("AllowedOrigins = %v", r.AllowedOrigins)
	}
	if r.MaxAgeSeconds != 300 {
		t.Errorf("MaxAgeSeconds = %d, want 300", r.MaxAgeSeconds)
	}

	if err := sp.DeleteBucketCORS(ctx, "bkt"); err != nil {
		t.Fatalf("DeleteBucketCORS: %v", err)
	}
	_, err = sp.GetBucketCORS(ctx, "bkt")
	if !errors.Is(err, ErrNoSuchBucketCORS) {
		t.Fatalf("expected ErrNoSuchBucketCORS after delete, got %v", err)
	}
}

func TestGetCORSMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	_, err := sp.GetBucketCORS(ctx, "no-such-bucket")
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("expected ErrBucketNotFound, got %v", err)
	}
}

func TestGetCORSNotSet(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	_, err := sp.GetBucketCORS(ctx, "bkt")
	if !errors.Is(err, ErrNoSuchBucketCORS) {
		t.Fatalf("expected ErrNoSuchBucketCORS, got %v", err)
	}
}

func TestDeleteBucketCORSIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	// Delete when nothing is set must succeed twice.
	if err := sp.DeleteBucketCORS(ctx, "bkt"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := sp.DeleteBucketCORS(ctx, "bkt"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}
