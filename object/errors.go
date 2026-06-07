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
)
