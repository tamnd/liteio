// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"path"
)

// SetBucketLifecycle implements ObjectLayer: it stores the lifecycle configuration
// document (opaque bytes, validated by the caller) on every set the bucket spans.
// The storage model mirrors bucket policy: one file per set under .liteio.sys,
// opaque bytes that the object layer stores and serves but never interprets.
func (sp *ServerPools) SetBucketLifecycle(ctx context.Context, bucket string, doc []byte) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketLifecycle(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketLifecycle implements ObjectLayer: it returns the lifecycle configuration
// document, or ErrNoSuchBucketLifecycle when none is configured.
func (sp *ServerPools) GetBucketLifecycle(ctx context.Context, bucket string) ([]byte, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return nil, err
	}
	return sp.allSets()[0].bucketLifecycle(ctx, bucket)
}

// DeleteBucketLifecycle implements ObjectLayer: it removes the lifecycle
// configuration from every set. It is idempotent.
func (sp *ServerPools) DeleteBucketLifecycle(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketLifecycle(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

func (s *erasureSet) lifecyclePath() string { return path.Join(reserved, "lifecycle") }

func (s *erasureSet) setBucketLifecycle(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.lifecyclePath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketLifecycle(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.lifecyclePath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketLifecycle
}

func (s *erasureSet) deleteBucketLifecycle(ctx context.Context, bucket string) error {
	if _, err := s.bucketLifecycle(ctx, bucket); err != nil {
		return nil // not set; idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.lifecyclePath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}
