// SPDX-License-Identifier: Apache-2.0

package object

import "errors"

// Object-layer errors. The S3 front door maps these to S3 error codes; they are
// sentinels compared with errors.Is.
var (
	// ErrBucketNotFound is returned for operations on a missing bucket.
	ErrBucketNotFound = errors.New("object: bucket not found")
	// ErrBucketExists is returned when creating a bucket that already exists.
	ErrBucketExists = errors.New("object: bucket already exists")
	// ErrBucketNotEmpty is returned when deleting a non-empty bucket without force.
	ErrBucketNotEmpty = errors.New("object: bucket not empty")
	// ErrObjectNotFound is returned for a missing object or version.
	ErrObjectNotFound = errors.New("object: object not found")
	// ErrNoSuchUpload is returned for a multipart upload ID that does not exist.
	ErrNoSuchUpload = errors.New("object: no such multipart upload")
	// ErrInvalidPart is returned when a completion part is missing or its ETag
	// does not match the uploaded part.
	ErrInvalidPart = errors.New("object: invalid part")
	// ErrInvalidPartOrder is returned when completion parts are not in ascending
	// part-number order.
	ErrInvalidPartOrder = errors.New("object: invalid part order")
	// ErrEntityTooSmall is returned when a non-final part is below the 5 MiB
	// multipart minimum.
	ErrEntityTooSmall = errors.New("object: part smaller than the 5 MiB minimum")
	// ErrInvalidRange is returned when a requested byte range is unsatisfiable.
	ErrInvalidRange = errors.New("object: invalid range")
	// ErrReadQuorum means fewer than K drives returned a consistent view.
	ErrReadQuorum = errors.New("object: read quorum not met")
	// ErrWriteQuorum means fewer than the write-quorum drives accepted the write.
	ErrWriteQuorum = errors.New("object: write quorum not met")
	// ErrInvalidArgument is returned for malformed bucket/object names or options.
	ErrInvalidArgument = errors.New("object: invalid argument")
	// ErrNotImplemented marks an operation deferred to a later milestone.
	ErrNotImplemented = errors.New("object: not implemented")
	// ErrOperationTimedOut means the namespace lock for a mutating operation
	// could not be acquired before the request context was done.
	ErrOperationTimedOut = errors.New("object: operation timed out acquiring the namespace lock")
	// ErrNoSuchBucketPolicy is returned by GetBucketPolicy when the bucket has no
	// policy attached.
	ErrNoSuchBucketPolicy = errors.New("object: no such bucket policy")
	// ErrNoSuchBucketTagging is returned by GetBucketTagging when the bucket has
	// no tags set.
	ErrNoSuchBucketTagging = errors.New("object: no such bucket tagging")
	// ErrInvalidTag is returned when a tag key or value violates the S3 limits.
	ErrInvalidTag = errors.New("object: invalid tag")
	// ErrTooManyTags is returned when the tag count exceeds the per-object or
	// per-bucket S3 limit.
	ErrTooManyTags = errors.New("object: too many tags")
	// ErrNoSuchBucketLifecycle is returned by GetBucketLifecycle when the bucket
	// has no lifecycle configuration set.
	ErrNoSuchBucketLifecycle = errors.New("object: no such bucket lifecycle configuration")
	// ErrSSECKeyRequired is returned when a GET/HEAD targets an SSE-C object but
	// the request carries no customer key.
	ErrSSECKeyRequired = errors.New("object: SSE-C customer key required")
	// ErrSSECKeyMismatch is returned when the supplied customer key does not match
	// the key's MD5 stored at write time.
	ErrSSECKeyMismatch = errors.New("object: SSE-C customer key does not match")
	// ErrSSECOnUnencrypted is returned when a request supplies an SSE-C key for an
	// object that was written without SSE-C.
	ErrSSECOnUnencrypted = errors.New("object: SSE-C key supplied for unencrypted object")
	// ErrObjectLocked is returned when a delete or overwrite targets a version
	// protected by Object Lock retention or a legal hold.
	ErrObjectLocked = errors.New("object: object is locked")
	// ErrObjectLockRequiresVersioning is returned when Object Lock is enabled on a
	// bucket that does not have versioning turned on.
	ErrObjectLockRequiresVersioning = errors.New("object: object lock requires versioning to be enabled")
	// ErrNoSuchObjectLockConfiguration is returned by GetObjectLockConfiguration
	// when the bucket has no lock configuration set.
	ErrNoSuchObjectLockConfiguration = errors.New("object: no such object lock configuration")
	// ErrComplianceRetentionCannotShorten is returned when a PUT Object Retention
	// request attempts to move a COMPLIANCE retain-until date earlier.
	ErrComplianceRetentionCannotShorten = errors.New("object: compliance retention date cannot be shortened")
	// ErrNoSuchObjectRetention is returned by GetObjectRetention when the version
	// has no retention set.
	ErrNoSuchObjectRetention = errors.New("object: no such object retention")
	// ErrSSES3NoKMS is returned when a GET targets an SSE-S3 object but the
	// ServerPools was constructed without a KMS backend.
	ErrSSES3NoKMS = errors.New("object: SSE-S3 object cannot be decrypted: no KMS configured")
	// ErrNoSuchBucketEncryption is returned by GetBucketEncryption when the
	// bucket has no default encryption configuration set.
	ErrNoSuchBucketEncryption = errors.New("object: no such bucket encryption configuration")
)
