// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"errors"
	"testing"
)

func TestSetGetDeleteBucketLifecycle(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	doc := []byte(`<LifecycleConfiguration><Rule><ID>expire-old</ID><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`)
	if err := sp.SetBucketLifecycle(ctx, "bkt", doc); err != nil {
		t.Fatalf("SetBucketLifecycle: %v", err)
	}

	got, err := sp.GetBucketLifecycle(ctx, "bkt")
	if err != nil {
		t.Fatalf("GetBucketLifecycle: %v", err)
	}
	if string(got) != string(doc) {
		t.Errorf("got %q, want %q", got, doc)
	}

	if err := sp.DeleteBucketLifecycle(ctx, "bkt"); err != nil {
		t.Fatalf("DeleteBucketLifecycle: %v", err)
	}
	_, err = sp.GetBucketLifecycle(ctx, "bkt")
	if !errors.Is(err, ErrNoSuchBucketLifecycle) {
		t.Fatalf("expected ErrNoSuchBucketLifecycle after delete, got %v", err)
	}
}

func TestDeleteBucketLifecycleIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")
	// Deleting lifecycle from a bucket that never had one must succeed.
	if err := sp.DeleteBucketLifecycle(ctx, "bkt"); err != nil {
		t.Fatalf("first delete: %v", err)
	}
	if err := sp.DeleteBucketLifecycle(ctx, "bkt"); err != nil {
		t.Fatalf("second delete: %v", err)
	}
}

func TestGetBucketLifecycleMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	_, err := sp.GetBucketLifecycle(ctx, "no-such-bucket")
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("expected ErrBucketNotFound, got %v", err)
	}
}

func TestSetBucketLifecycleMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	err := sp.SetBucketLifecycle(ctx, "no-such-bucket", []byte("<LifecycleConfiguration/>"))
	if !errors.Is(err, ErrBucketNotFound) {
		t.Fatalf("expected ErrBucketNotFound, got %v", err)
	}
}

func TestSetBucketLifecycleOverwrites(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	first := []byte(`<LifecycleConfiguration><Rule><ID>r1</ID><Status>Enabled</Status><Expiration><Days>10</Days></Expiration></Rule></LifecycleConfiguration>`)
	second := []byte(`<LifecycleConfiguration><Rule><ID>r2</ID><Status>Enabled</Status><Expiration><Days>90</Days></Expiration></Rule></LifecycleConfiguration>`)

	if err := sp.SetBucketLifecycle(ctx, "bkt", first); err != nil {
		t.Fatalf("first put: %v", err)
	}
	if err := sp.SetBucketLifecycle(ctx, "bkt", second); err != nil {
		t.Fatalf("second put: %v", err)
	}

	got, err := sp.GetBucketLifecycle(ctx, "bkt")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(second) {
		t.Errorf("got %q, want second doc %q", got, second)
	}
}
