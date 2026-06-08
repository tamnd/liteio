// SPDX-License-Identifier: Apache-2.0

package object

import "context"

// SetBucketPolicy implements ObjectLayer: it attaches a policy document to a
// bucket, persisting it on every set the bucket spans. The document is stored as
// opaque bytes; the S3 front door validates it (auth.ParseBucketPolicy) before
// calling, so the object layer stays free of the auth package, exactly as it
// treats versioning state and obj.meta as opaque to the storage layer below it.
func (sp *ServerPools) SetBucketPolicy(ctx context.Context, bucket string, doc []byte) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketPolicy(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketPolicy implements ObjectLayer: it returns the bucket's attached policy
// document, or ErrNoSuchBucketPolicy when none is set. Every set carries the same
// document, so the first set is authoritative.
func (sp *ServerPools) GetBucketPolicy(ctx context.Context, bucket string) ([]byte, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return nil, err
	}
	return sp.allSets()[0].bucketPolicy(ctx, bucket)
}

// DeleteBucketPolicy implements ObjectLayer: it removes the bucket's attached
// policy from every set. It is idempotent — deleting when no policy is set
// succeeds, matching S3's DeleteBucketPolicy.
func (sp *ServerPools) DeleteBucketPolicy(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketPolicy(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}
