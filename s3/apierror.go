// SPDX-License-Identifier: Apache-2.0

// Package s3 is liteio's S3 REST front door (spec 2020, doc 02). It maps HTTP
// requests onto the object.ObjectLayer: SigV4 authentication (s3/sign), the
// request router (path-style and virtual-host addressing), the per-operation
// handlers, and the XML/error envelope clients expect. The package speaks the S3
// error catalog exactly, since real clients branch on the Code string.
package s3

import (
	"encoding/xml"
	"errors"
	"net/http"

	"github.com/tamnd/liteio/object"
)

// APIError is one entry in the S3 error catalog: the wire Code clients branch on,
// a human Message, and the HTTP status it travels with (doc 02 §2.8).
type APIError struct {
	Code        string
	Description string
	HTTPStatus  int
}

// Error implements the error interface so an APIError can flow as a normal error.
func (e APIError) Error() string { return e.Code + ": " + e.Description }

// errorResponse is the S3 REST error envelope (doc 02 §2.8). Resource is the
// request path; RequestID/HostID echo the per-response trace headers.
type errorResponse struct {
	XMLName   xml.Name `xml:"Error"`
	Code      string   `xml:"Code"`
	Message   string   `xml:"Message"`
	Resource  string   `xml:"Resource"`
	RequestID string   `xml:"RequestId"`
	HostID    string   `xml:"HostId"`
}

// The catalog. Only the codes liteio can actually return at M1 are spelled out
// here; the table in doc 02 §2.8 is the full target and entries are added as the
// operations that raise them land.
var (
	errNoSuchBucket            = APIError{"NoSuchBucket", "The specified bucket does not exist.", http.StatusNotFound}
	errNoSuchKey               = APIError{"NoSuchKey", "The specified key does not exist.", http.StatusNotFound}
	errNoSuchVersion           = APIError{"NoSuchVersion", "The specified version does not exist.", http.StatusNotFound}
	errBucketNotEmpty          = APIError{"BucketNotEmpty", "The bucket you tried to delete is not empty.", http.StatusConflict}
	errBucketAlreadyOwnedByYou = APIError{"BucketAlreadyOwnedByYou", "Your previous request to create the named bucket succeeded and you already own it.", http.StatusConflict}
	errInvalidBucketName       = APIError{"InvalidBucketName", "The specified bucket is not valid.", http.StatusBadRequest}
	errInvalidArgument         = APIError{"InvalidArgument", "Invalid Argument.", http.StatusBadRequest}
	errInvalidRange            = APIError{"InvalidRange", "The requested range is not satisfiable.", http.StatusRequestedRangeNotSatisfiable}
	errPreconditionFailed      = APIError{"PreconditionFailed", "At least one of the preconditions you specified did not hold.", http.StatusPreconditionFailed}
	errNoSuchUpload            = APIError{"NoSuchUpload", "The specified multipart upload does not exist. The upload ID may be invalid, or the upload may have been aborted or completed.", http.StatusNotFound}
	errInvalidPart             = APIError{"InvalidPart", "One or more of the specified parts could not be found. The part may not have been uploaded, or the specified ETag may not match the part's ETag.", http.StatusBadRequest}
	errInvalidPartOrder        = APIError{"InvalidPartOrder", "The list of parts was not in ascending order. Parts must be ordered by part number.", http.StatusBadRequest}
	errEntityTooSmall          = APIError{"EntityTooSmall", "Your proposed upload is smaller than the minimum allowed object size. Each part but the last must be at least 5 MiB.", http.StatusBadRequest}
	errInvalidRequest          = APIError{"InvalidRequest", "Invalid Request.", http.StatusBadRequest}
	errMalformedXML            = APIError{"MalformedXML", "The XML you provided was not well-formed or did not validate against our published schema.", http.StatusBadRequest}
	errMissingContentLength    = APIError{"MissingContentLength", "You must provide the Content-Length HTTP header.", http.StatusBadRequest}
	errBadDigest               = APIError{"BadDigest", "The Content-MD5 you specified did not match what we received.", http.StatusBadRequest}
	errAccessDenied            = APIError{"AccessDenied", "Access Denied.", http.StatusForbidden}
	errSignatureDoesNotMatch   = APIError{"SignatureDoesNotMatch", "The request signature we calculated does not match the signature you provided.", http.StatusForbidden}
	errInvalidAccessKeyID      = APIError{"InvalidAccessKeyId", "The access key Id you provided does not exist in our records.", http.StatusForbidden}
	errRequestTimeTooSkewed    = APIError{"RequestTimeTooSkewed", "The difference between the request time and the server's time is too large.", http.StatusForbidden}
	errMethodNotAllowed        = APIError{"MethodNotAllowed", "The specified method is not allowed against this resource.", http.StatusMethodNotAllowed}
	errMissingSecurityHeader   = APIError{"MissingSecurityHeader", "Your request is missing a required header.", http.StatusBadRequest}
	errSlowDown                = APIError{"SlowDown", "Please reduce your request rate.", http.StatusServiceUnavailable}
	errInternalError           = APIError{"InternalError", "We encountered an internal error. Please try again.", http.StatusInternalServerError}
	errNotImplemented          = APIError{"NotImplemented", "A header or operation you provided implies functionality that is not implemented.", http.StatusNotImplemented}
)

// toAPIError maps an object-layer (or lower) error to its S3 catalog entry. A nil
// error maps to a zero APIError; callers check err != nil first.
func toAPIError(err error) APIError {
	switch {
	case err == nil:
		return APIError{}
	case errors.Is(err, object.ErrBucketNotFound):
		return errNoSuchBucket
	case errors.Is(err, object.ErrObjectNotFound):
		return errNoSuchKey
	case errors.Is(err, object.ErrBucketExists):
		return errBucketAlreadyOwnedByYou
	case errors.Is(err, object.ErrBucketNotEmpty):
		return errBucketNotEmpty
	case errors.Is(err, object.ErrInvalidArgument):
		return errInvalidArgument
	case errors.Is(err, object.ErrInvalidRange):
		return errInvalidRange
	case errors.Is(err, object.ErrNoSuchUpload):
		return errNoSuchUpload
	case errors.Is(err, object.ErrInvalidPart):
		return errInvalidPart
	case errors.Is(err, object.ErrInvalidPartOrder):
		return errInvalidPartOrder
	case errors.Is(err, object.ErrEntityTooSmall):
		return errEntityTooSmall
	case errors.Is(err, object.ErrNotImplemented):
		return errNotImplemented
	case errors.Is(err, object.ErrReadQuorum), errors.Is(err, object.ErrWriteQuorum):
		// A set without quorum is asking the client to back off and retry.
		return errSlowDown
	default:
		if ae, ok := errors.AsType[APIError](err); ok {
			return ae
		}
		return errInternalError
	}
}
