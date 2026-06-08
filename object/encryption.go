// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"path"
)

// BucketEncryptionConfig is the bucket-level default server-side encryption
// rule. When set, every PUT Object that carries no explicit SSE header is
// encrypted with the specified algorithm.
type BucketEncryptionConfig struct {
	// Algorithm is "AES256" for SSE-S3 or "aws:kms" for SSE-KMS. An empty
	// string means encryption is not configured.
	Algorithm string `json:"algorithm"`
	// KMSKeyID is the operator-assigned KMS key identifier used for SSE-KMS.
	// Unused for AES256.
	KMSKeyID string `json:"kmsKeyId,omitempty"`
}

// SetBucketEncryption implements ObjectLayer: stores the bucket default
// encryption rule under .liteio.sys/encryption on all sets.
func (sp *ServerPools) SetBucketEncryption(ctx context.Context, bucket string, cfg BucketEncryptionConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketEncryption(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketEncryption implements ObjectLayer: returns the bucket default
// encryption rule, or ErrNoSuchBucketEncryption when none is set.
func (sp *ServerPools) GetBucketEncryption(ctx context.Context, bucket string) (BucketEncryptionConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return BucketEncryptionConfig{}, err
	}
	doc, err := sp.allSets()[0].bucketEncryption(ctx, bucket)
	if err != nil {
		return BucketEncryptionConfig{}, err
	}
	var cfg BucketEncryptionConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return BucketEncryptionConfig{}, ErrNoSuchBucketEncryption
	}
	return cfg, nil
}

// DeleteBucketEncryption implements ObjectLayer: removes the bucket default
// encryption rule. Idempotent: succeeds even when no rule was set.
func (sp *ServerPools) DeleteBucketEncryption(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketEncryption(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// --- erasureSet helpers --------------------------------------------------

func (s *erasureSet) encryptionPath() string { return path.Join(reserved, "encryption") }

func (s *erasureSet) setBucketEncryption(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.encryptionPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketEncryption(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.encryptionPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketEncryption
}

func (s *erasureSet) deleteBucketEncryption(ctx context.Context, bucket string) error {
	if _, err := s.bucketEncryption(ctx, bucket); err != nil {
		return nil // not set; idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.encryptionPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}
