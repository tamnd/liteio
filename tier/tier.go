// SPDX-License-Identifier: Apache-2.0

// Package tier implements remote-tier configuration and client access for liteio's
// ILM tiering feature (spec 2020, doc 09 §9.4). A tier is a remote object-store
// backend (S3-compatible, Azure Blob, or GCS) that holds the data of objects whose
// lifecycle rule has fired a Transition action. The object remains addressable
// through liteio; data is fetched transparently from the remote tier on GET and
// re-hydrated locally by RestoreObject.
package tier

import (
	"context"
	"errors"
	"io"
	"strings"
)

// TierType identifies the backend technology for a remote tier.
type TierType string

const (
	TierTypeS3    TierType = "s3"
	TierTypeAzure TierType = "azure"
	TierTypeGCS   TierType = "gcs"
)

// Tier-stub metadata keys written into FileInfo.Metadata when an object is
// transitioned. Their presence (MetaName non-empty) signals that the object's
// data lives on the remote tier, not on local drives.
const (
	// MetaName is the name of the tier that holds the object's data.
	MetaName = "x-liteio-tier-name"
	// MetaKey is the remote key under which the data is stored in the tier bucket.
	MetaKey = "x-liteio-tier-key"
	// MetaRestoreExpires is the RFC3339 expiry time of a completed RestoreObject
	// operation. Present only after a successful restore.
	MetaRestoreExpires = "x-liteio-restore-expires"
	// MetaRestoreOngoing is "true" while a RestoreObject operation is in progress.
	MetaRestoreOngoing = "x-liteio-restore-ongoing"
)

// TierConfig is the configuration for one remote tier. Exactly one of S3, Azure,
// or GCS must be non-nil; it identifies the backend type and supplies credentials.
type TierConfig struct {
	Name string    `json:"Name"`
	Type TierType  `json:"Type"`
	S3   *S3Config `json:"S3,omitempty"`
}

// S3Config holds connection parameters for an S3-compatible remote tier.
type S3Config struct {
	Endpoint  string `json:"Endpoint"` // base URL, e.g. "https://s3.amazonaws.com"
	Region    string `json:"Region"`   // e.g. "us-east-1"
	Bucket    string `json:"Bucket"`
	Prefix    string `json:"Prefix,omitempty"` // optional key prefix within the bucket
	AccessKey string `json:"AccessKey"`
	SecretKey string `json:"SecretKey"`
}

// RemoteClient is the interface a tier backend driver must implement.
type RemoteClient interface {
	// Put writes body (size bytes) to the tier under key.
	Put(ctx context.Context, key string, body io.Reader, size int64) error
	// Get retrieves the object at key. The caller is responsible for closing the reader.
	Get(ctx context.Context, key string) (io.ReadCloser, int64, error)
	// Remove deletes the object at key. It is idempotent.
	Remove(ctx context.Context, key string) error
}

// NewClient returns a RemoteClient for cfg. It validates the config and returns an
// error if the type is unknown or the config is incomplete.
func NewClient(cfg TierConfig) (RemoteClient, error) {
	switch cfg.Type {
	case TierTypeS3:
		if cfg.S3 == nil {
			return nil, errors.New("tier: S3 config required for type s3")
		}
		if cfg.S3.Endpoint == "" || cfg.S3.Bucket == "" {
			return nil, errors.New("tier: S3 endpoint and bucket are required")
		}
		return newS3Client(*cfg.S3), nil
	default:
		return nil, errors.New("tier: unsupported tier type: " + string(cfg.Type))
	}
}

// RemoteKey returns the object key used in the remote tier for the given liteio
// object (bucket + key + versionID). The prefix from the S3Config is prepended
// so multiple liteio clusters can share one remote bucket safely.
func RemoteKey(prefix, bucket, object, versionID string) string {
	var b strings.Builder
	if prefix != "" {
		b.WriteString(strings.TrimSuffix(prefix, "/"))
		b.WriteByte('/')
	}
	b.WriteString(bucket)
	b.WriteByte('/')
	if versionID != "" {
		b.WriteString(versionID)
		b.WriteByte('/')
	}
	b.WriteString(object)
	return b.String()
}

// Validate returns an error if cfg is incomplete or inconsistent.
func Validate(cfg TierConfig) error {
	if cfg.Name == "" {
		return errors.New("tier: name is required")
	}
	switch cfg.Type {
	case TierTypeS3:
		if cfg.S3 == nil || cfg.S3.Endpoint == "" || cfg.S3.Bucket == "" {
			return errors.New("tier: S3 config must have endpoint and bucket")
		}
	case TierTypeAzure, TierTypeGCS:
		return errors.New("tier: azure and gcs tiers are not yet supported")
	default:
		return errors.New("tier: unknown type " + string(cfg.Type))
	}
	return nil
}
