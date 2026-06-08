// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"encoding/json"
	"path"
)

// CORSRule is one entry in a bucket's CORS configuration. AllowedOrigins,
// AllowedMethods, AllowedHeaders, and ExposeHeaders follow the S3 field names
// (doc 02 §2.9). The wildcard "*" in AllowedOrigins matches any origin.
type CORSRule struct {
	ID             string
	AllowedOrigins []string
	AllowedMethods []string
	AllowedHeaders []string
	ExposeHeaders  []string
	MaxAgeSeconds  int
}

// CORSConfig is the full CORS configuration attached to a bucket.
type CORSConfig struct {
	Rules []CORSRule
}

// SetBucketCORS implements ObjectLayer: it persists the CORS configuration on
// every set the bucket spans. The encoding mirrors bucket policy and lifecycle —
// the object layer stores and serves the bytes without interpreting them.
func (sp *ServerPools) SetBucketCORS(ctx context.Context, bucket string, cfg CORSConfig) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	doc, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.setBucketCORS(ctx, bucket, doc); err != nil {
			return err
		}
	}
	return nil
}

// GetBucketCORS implements ObjectLayer: it returns the bucket's CORS
// configuration, or ErrNoSuchBucketCORS when none is set. The first online set
// is authoritative — all sets carry the same document.
func (sp *ServerPools) GetBucketCORS(ctx context.Context, bucket string) (CORSConfig, error) {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return CORSConfig{}, err
	}
	doc, err := sp.allSets()[0].bucketCORS(ctx, bucket)
	if err != nil {
		return CORSConfig{}, err
	}
	var cfg CORSConfig
	if err := json.Unmarshal(doc, &cfg); err != nil {
		return CORSConfig{}, err
	}
	return cfg, nil
}

// DeleteBucketCORS implements ObjectLayer: it removes the CORS configuration
// from every set. Idempotent — deleting when nothing is set succeeds.
func (sp *ServerPools) DeleteBucketCORS(ctx context.Context, bucket string) error {
	if _, err := sp.GetBucketInfo(ctx, bucket); err != nil {
		return err
	}
	for _, set := range sp.allSets() {
		if err := set.deleteBucketCORS(ctx, bucket); err != nil {
			return err
		}
	}
	return nil
}

// --- set-level helpers -------------------------------------------------------

func (s *erasureSet) corsPath() string { return path.Join(reserved, "cors") }

func (s *erasureSet) setBucketCORS(ctx context.Context, bucket string, doc []byte) error {
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].WriteMeta(ctx, bucket, s.corsPath(), doc)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}

func (s *erasureSet) bucketCORS(ctx context.Context, bucket string) ([]byte, error) {
	for _, d := range s.drives {
		if !d.IsOnline() {
			continue
		}
		data, err := d.ReadMeta(ctx, bucket, s.corsPath())
		if err != nil {
			continue
		}
		return data, nil
	}
	return nil, ErrNoSuchBucketCORS
}

func (s *erasureSet) deleteBucketCORS(ctx context.Context, bucket string) error {
	if _, err := s.bucketCORS(ctx, bucket); err != nil {
		return nil // not set; idempotent
	}
	res := fanOut(ctx, len(s.drives), func(ctx context.Context, i int) (struct{}, error) {
		return struct{}{}, s.drives[i].Delete(ctx, bucket, s.corsPath(), true)
	})
	if countOK(res) < s.writeQuorum() {
		return ErrWriteQuorum
	}
	return nil
}
