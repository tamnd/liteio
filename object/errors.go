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
)
