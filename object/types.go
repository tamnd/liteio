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

	"github.com/tamnd/liteio/event"
	"github.com/tamnd/liteio/object/meta"
	"github.com/tamnd/liteio/replication"
	"github.com/tamnd/liteio/tier"
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

	// Tier is the name of the remote tier holding this object's data, or empty
	// for objects stored locally. Set from FileInfo.Metadata[tier.MetaName].
	Tier string
	// TierKey is the remote key under which the data is stored in the tier bucket.
	TierKey string
	// RestoreExpires is the expiry time of a completed RestoreObject operation.
	// Zero means no active restore.
	RestoreExpires time.Time
	// RestoreOngoing is true while a RestoreObject is in progress.
	RestoreOngoing bool
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
	// Range, when set, limits a GetObject to a byte range of the object.
	Range *HTTPRangeSpec
	// SSES3, when true, requests server-managed AES-256-GCM encryption (SSE-S3).
	// Requires a KMS to be configured on the ServerPools. Ignored when SSECKey
	// is also set (SSE-C takes precedence).
	SSES3 bool
	// SSECKey, when non-nil, is the 32-byte AES-256 customer key for SSE-C
	// (server-side encryption with customer-provided keys). On a write it triggers
	// AES-256-CTR encryption; on a read it decrypts and validates the key against
	// the MD5 stored at write time.
	SSECKey *[32]byte
	// SrcSSECKey, when non-nil, is the customer key for the copy source object in a
	// CopyObject or CopyObjectPart operation. It is used only for reading; the
	// destination is controlled by SSECKey.
	SrcSSECKey *[32]byte

	// SourceIP is the client IP address, propagated for inclusion in event records.
	SourceIP string

	// EventName, when non-empty, overrides the S3 event name fired on success.
	// The S3 front door sets this for CopyObject (ObjectCreated:Copy) and
	// CompleteMultipartUpload (ObjectCreated:CompleteMultipartUpload); regular
	// PutObject leaves it empty and the write path uses ObjectCreated:Put.
	EventName string

	// ReplicationSource, when true, marks the request as an incoming replica from
	// a peer cluster. The object layer stores StatusReplica in obj.meta so the
	// object is not re-replicated (loop prevention for active-active setups).
	ReplicationSource bool

	// BypassGovernanceRetention, when true, allows a DELETE to proceed on a
	// GOVERNANCE-mode locked version without waiting for the retain-until date.
	// The S3 front door sets this only after verifying that the caller holds
	// s3:BypassGovernanceRetention (spec 2020, doc 08, section 8.9).
	// Legal hold always blocks regardless of this flag.
	// COMPLIANCE-mode retention is never bypassed.
	BypassGovernanceRetention bool
}

// HTTPRangeSpec is a parsed HTTP Range request over a single object. It models
// the two forms S3 honors: a closed/open byte range (bytes=start-[end]) and a
// suffix range (bytes=-N, the trailing N bytes). The S3 front door parses the
// Range header into this; the object layer resolves it against the real size.
type HTTPRangeSpec struct {
	// IsSuffix selects the trailing Start bytes of the object (bytes=-N form).
	IsSuffix bool
	// Start is the first byte offset, or the suffix length when IsSuffix.
	Start int64
	// End is the last byte offset (inclusive); -1 means "to the end".
	End int64
}

// GetOffsetLength resolves the range against an object of resourceSize bytes,
// returning the read offset and length. A nil spec covers the whole object. An
// unsatisfiable range (offset past the end, inverted bounds, empty suffix)
// returns ErrInvalidRange, which the front door renders as 416.
func (h *HTTPRangeSpec) GetOffsetLength(resourceSize int64) (start, length int64, err error) {
	if h == nil {
		return 0, resourceSize, nil
	}
	if h.IsSuffix {
		n := h.Start
		if n <= 0 {
			return 0, 0, ErrInvalidRange
		}
		if n > resourceSize {
			n = resourceSize
		}
		return resourceSize - n, n, nil
	}
	if h.Start < 0 || h.Start >= resourceSize {
		return 0, 0, ErrInvalidRange
	}
	end := h.End
	if end < 0 || end >= resourceSize {
		end = resourceSize - 1
	}
	if h.Start > end {
		return 0, 0, ErrInvalidRange
	}
	return h.Start, end - h.Start + 1, nil
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

// ListObjectVersionsInfo is the result of a ListObjectVersions call. Objects holds
// versions and delete markers interleaved, ordered by key ascending and, within a
// key, newest-first (IsLatest and DeleteMarker on each entry tell the front door
// which XML element to render).
type ListObjectVersionsInfo struct {
	Objects             []ObjectInfo
	Prefixes            []string // common (delimiter-rolled) prefixes
	IsTruncated         bool
	NextKeyMarker       string
	NextVersionIDMarker string
}

// MultipartUpload identifies one in-progress multipart upload in a listing.
type MultipartUpload struct {
	Bucket    string
	Object    string
	UploadID  string
	Initiated time.Time
}

// PartInfo describes one uploaded part as ListParts and CompleteMultipartUpload
// report it.
type PartInfo struct {
	PartNumber   int
	LastModified time.Time
	ETag         string
	Size         int64
}

// CompletePart is one entry of a CompleteMultipartUpload request: the part number
// and the ETag the client received from UploadPart.
type CompletePart struct {
	PartNumber int
	ETag       string
}

// ListPartsInfo is the result of ListObjectParts.
type ListPartsInfo struct {
	Bucket               string
	Object               string
	UploadID             string
	PartNumberMarker     int
	NextPartNumberMarker int
	MaxParts             int
	IsTruncated          bool
	Parts                []PartInfo
	UserDefined          map[string]string
}

// ListMultipartsInfo is the result of ListMultipartUploads.
type ListMultipartsInfo struct {
	Bucket             string
	Prefix             string
	Delimiter          string
	KeyMarker          string
	UploadIDMarker     string
	NextKeyMarker      string
	NextUploadIDMarker string
	MaxUploads         int
	IsTruncated        bool
	Uploads            []MultipartUpload
	CommonPrefixes     []string
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

// VersioningConfig is a bucket's versioning state as the S3 VersioningConfiguration
// subresource models it. Enabled keeps every version; Suspended retains existing
// versions but writes the unversioned "null" version going forward; the zero value
// (neither set) means versioning was never configured.
type VersioningConfig struct {
	Enabled   bool
	Suspended bool
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
		oi.Tier = fi.Metadata[tier.MetaName]
		oi.TierKey = fi.Metadata[tier.MetaKey]
		if exp := fi.Metadata[tier.MetaRestoreExpires]; exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err == nil {
				oi.RestoreExpires = t
			}
		}
		oi.RestoreOngoing = fi.Metadata[tier.MetaRestoreOngoing] == "true"
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
	ListObjectVersions(ctx context.Context, bucket, prefix, keyMarker, versionIDMarker, delim string, maxKeys int) (ListObjectVersionsInfo, error)

	// versioning
	SetBucketVersioning(ctx context.Context, bucket string, cfg VersioningConfig) error
	GetBucketVersioning(ctx context.Context, bucket string) (VersioningConfig, error)

	// bucket policy
	SetBucketPolicy(ctx context.Context, bucket string, doc []byte) error
	GetBucketPolicy(ctx context.Context, bucket string) ([]byte, error)
	DeleteBucketPolicy(ctx context.Context, bucket string) error

	// object tagging
	SetObjectTags(ctx context.Context, bucket, object, versionID string, tags map[string]string) error
	GetObjectTags(ctx context.Context, bucket, object, versionID string) (map[string]string, error)
	DeleteObjectTags(ctx context.Context, bucket, object, versionID string) error

	// bucket tagging
	SetBucketTagging(ctx context.Context, bucket string, doc []byte) error
	GetBucketTagging(ctx context.Context, bucket string) ([]byte, error)
	DeleteBucketTagging(ctx context.Context, bucket string) error

	// bucket lifecycle configuration (storage only; scanner execution is deferred)
	SetBucketLifecycle(ctx context.Context, bucket string, doc []byte) error
	GetBucketLifecycle(ctx context.Context, bucket string) ([]byte, error)
	DeleteBucketLifecycle(ctx context.Context, bucket string) error

	// bucket default encryption (SSE-S3 / SSE-KMS)
	SetBucketEncryption(ctx context.Context, bucket string, cfg BucketEncryptionConfig) error
	GetBucketEncryption(ctx context.Context, bucket string) (BucketEncryptionConfig, error)
	DeleteBucketEncryption(ctx context.Context, bucket string) error

	// bucket quotas: hard/soft limits by size and object count
	SetBucketQuota(ctx context.Context, bucket string, q BucketQuota) error
	GetBucketQuota(ctx context.Context, bucket string) (BucketQuota, error)
	DeleteBucketQuota(ctx context.Context, bucket string) error

	// bucket notification configuration (event notifications, doc 09 §9.5)
	SetBucketNotification(ctx context.Context, bucket string, cfg event.NotificationConfig) error
	GetBucketNotification(ctx context.Context, bucket string) (event.NotificationConfig, error)
	DeleteBucketNotification(ctx context.Context, bucket string) error

	// object lock: bucket-level configuration and per-version retention / legal hold
	SetObjectLockConfiguration(ctx context.Context, bucket string, cfg ObjectLockConfig) error
	GetObjectLockConfiguration(ctx context.Context, bucket string) (ObjectLockConfig, error)
	SetObjectRetention(ctx context.Context, bucket, object, versionID, mode, retainUntil string) error
	GetObjectRetention(ctx context.Context, bucket, object, versionID string) (mode, retainUntil string, err error)
	SetObjectLegalHold(ctx context.Context, bucket, object, versionID, status string) error
	GetObjectLegalHold(ctx context.Context, bucket, object, versionID string) (string, error)

	// copy
	CopyObject(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject string, srcInfo ObjectInfo, opts ObjectOptions) (ObjectInfo, error)
	CopyObjectPart(ctx context.Context, srcBucket, srcObject, dstBucket, dstObject, uploadID string, partID int, srcInfo ObjectInfo, rng *HTTPRangeSpec, opts ObjectOptions) (PartInfo, error)

	// multipart upload
	NewMultipartUpload(ctx context.Context, bucket, object string, opts ObjectOptions) (uploadID string, err error)
	PutObjectPart(ctx context.Context, bucket, object, uploadID string, partID int, r *PutReader, opts ObjectOptions) (PartInfo, error)
	CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []CompletePart, opts ObjectOptions) (ObjectInfo, error)
	AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string, opts ObjectOptions) error
	ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumberMarker, maxParts int, opts ObjectOptions) (ListPartsInfo, error)
	ListMultipartUploads(ctx context.Context, bucket, prefix, keyMarker, uploadIDMarker, delimiter string, maxUploads int) (ListMultipartsInfo, error)

	// tiering: remote-tier config management and object lifecycle transitions
	SetTierConfig(ctx context.Context, cfg tier.TierConfig) error
	GetTierConfig(ctx context.Context, name string) (tier.TierConfig, error)
	ListTierConfigs(ctx context.Context) ([]tier.TierConfig, error)
	DeleteTierConfig(ctx context.Context, name string) error
	TransitionObject(ctx context.Context, bucket, object, tierName string, opts ObjectOptions) error
	RestoreObject(ctx context.Context, bucket, object, versionID string, days int) error

	// replication: bucket replication config management
	SetBucketReplication(ctx context.Context, bucket string, cfg replication.ReplicationConfig) error
	GetBucketReplication(ctx context.Context, bucket string) (replication.ReplicationConfig, error)
	DeleteBucketReplication(ctx context.Context, bucket string) error
}
