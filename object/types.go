// SPDX-License-Identifier: Apache-2.0

// Package object is liteio's ObjectLayer: the abstraction that presents a set of
// drives as one object store (spec 2020, docs 03, 04, 07, 11.3). It composes the
// pure primitives in object/placement, object/erasure, and object/meta over the
// byte-oriented storage.StorageAPI, turning S3-shaped object operations into
// erasure-coded, quorum-checked reads and writes across a set of drives.
//
// The layer is built in two pieces. An erasureSet owns N drives and runs the
// per-object data path (encode, fan-out write, quorum, degraded read) for the
// objects that hash to it. erasureServerPools sits above a list of sets, routes
// each object to its set via placement, and is the ObjectLayer the S3 front door
// talks to.
package object

import (
	"context"
	"io"
	"time"

	"github.com/tamnd/liteio/object/meta"
)

// ObjectInfo is the engine/handler view of one object version. It is the value
// the S3 layer renders into headers and XML. The meta package's FileInfo is the
// per-drive on-disk projection; toObjectInfo converts between them.
type ObjectInfo struct {
	Bucket       string
	Name         string
	VersionID    string
	IsLatest     bool
	DeleteMarker bool
	Size         int64
	ModTime      time.Time
	ETag         string
	ContentType  string
	StorageClass string
	UserDefined  map[string]string
	Parts        []meta.ObjectPartInfo
	Erasure      meta.ErasureInfo
}

// ObjectOptions carries per-call inputs that do not belong in the positional
// arguments: the target version, conditional headers, and user metadata for a
// write.
type ObjectOptions struct {
	// VersionID selects a specific version for read/delete; empty means latest.
	VersionID string
	// UserDefined holds user metadata (x-amz-meta-*) and replayed system headers
	// (content-type, ...) on a write.
	UserDefined map[string]string
	// MIME shortcut for the common content-type header.
	ContentType string
}

// BucketInfo describes a bucket.
type BucketInfo struct {
	Name    string
	Created time.Time
}

// PutReader is the body of a write: a reader plus the caller-declared size (-1
// when unknown) and the precomputed wire ETag when the caller already has it.
type PutReader struct {
	Reader io.Reader
	Size   int64
}

// NewPutReader wraps r with a declared size (-1 for unknown/streaming).
func NewPutReader(r io.Reader, size int64) *PutReader {
	return &PutReader{Reader: r, Size: size}
}

// GetObjectReader streams an object's bytes to the caller and carries its
// metadata. Close releases the underlying buffers/handles.
type GetObjectReader struct {
	ObjectInfo ObjectInfo
	r          io.Reader
	closer     func() error
}

// Read implements io.Reader over the object's bytes.
func (g *GetObjectReader) Read(p []byte) (int, error) { return g.r.Read(p) }

// Close releases resources held by the reader.
func (g *GetObjectReader) Close() error {
	if g.closer != nil {
		return g.closer()
	}
	return nil
}

// ListObjectsV2Info is the result of a ListObjectsV2 call.
type ListObjectsV2Info struct {
	Objects               []ObjectInfo
	Prefixes              []string // common (delimiter-rolled) prefixes
	IsTruncated           bool
	ContinuationToken     string
	NextContinuationToken string
}

// ObjectToDelete names one target of a batch delete.
type ObjectToDelete struct {
	Name      string
	VersionID string
}

// DeletedObject is the per-key result of a successful batch delete.
type DeletedObject struct {
	Name                  string
	VersionID             string
	DeleteMarker          bool
	DeleteMarkerVersionID string
}

// MakeBucketOptions carries bucket-creation inputs (initial versioning state).
type MakeBucketOptions struct{ VersionedDefault bool }

// DeleteBucketOptions carries bucket-deletion inputs (force-delete non-empty).
type DeleteBucketOptions struct{ Force bool }

// toObjectInfo projects an on-disk FileInfo into the handler-facing ObjectInfo.
func toObjectInfo(bucket, object string, fi meta.FileInfo) ObjectInfo {
	oi := ObjectInfo{
		Bucket:       bucket,
		Name:         object,
		VersionID:    fi.VersionID,
		IsLatest:     fi.IsLatest,
		DeleteMarker: fi.Deleted,
		Size:         fi.Size,
		ModTime:      fi.ModTime,
		ETag:         fi.ETag,
		UserDefined:  fi.Metadata,
		Parts:        fi.Parts,
		Erasure:      fi.Erasure,
	}
	if fi.Metadata != nil {
		oi.ContentType = fi.Metadata["content-type"]
	}
	return oi
}

// ObjectLayer is the cluster-wide object store contract the S3 front door calls.
// The core (M1) operations are implemented by erasureServerPools; multipart,
// copy, versioning listing, and heal arrive with later milestones and currently
// return ErrNotImplemented.
type ObjectLayer interface {
	// buckets
	MakeBucket(ctx context.Context, bucket string, opts MakeBucketOptions) error
	GetBucketInfo(ctx context.Context, bucket string) (BucketInfo, error)
	ListBuckets(ctx context.Context) ([]BucketInfo, error)
	DeleteBucket(ctx context.Context, bucket string, opts DeleteBucketOptions) error

	// objects
	PutObject(ctx context.Context, bucket, object string, r *PutReader, opts ObjectOptions) (ObjectInfo, error)
	GetObject(ctx context.Context, bucket, object string, opts ObjectOptions) (*GetObjectReader, error)
	GetObjectInfo(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error)
	DeleteObject(ctx context.Context, bucket, object string, opts ObjectOptions) (ObjectInfo, error)
	DeleteObjects(ctx context.Context, bucket string, objs []ObjectToDelete, opts ObjectOptions) ([]DeletedObject, []error)

	// listing
	ListObjectsV2(ctx context.Context, bucket, prefix, token, startAfter, delim string, maxKeys int, fetchOwner bool) (ListObjectsV2Info, error)
}
