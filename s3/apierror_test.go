// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"errors"
	"net/http"
	"testing"

	"github.com/tamnd/liteio/object"
)

// catalog is the full M1 error set. The test pins each entry's wire Code and HTTP
// status, since real S3 clients branch on the Code string (doc 02 §2.8) — and it
// is the single place every catalog var is referenced, so the set stays live.
var catalog = []APIError{
	errNoSuchBucket, errNoSuchKey, errNoSuchVersion,
	errBucketNotEmpty, errBucketAlreadyOwnedByYou,
	errInvalidBucketName, errInvalidArgument, errInvalidRequest,
	errMalformedXML, errMissingContentLength, errBadDigest,
	errAccessDenied, errSignatureDoesNotMatch, errInvalidAccessKeyID,
	errRequestTimeTooSkewed, errMethodNotAllowed, errMissingSecurityHeader,
	errSlowDown, errInternalError, errNotImplemented,
}

func TestCatalogWellFormed(t *testing.T) {
	seen := map[string]bool{}
	for _, e := range catalog {
		if e.Code == "" {
			t.Errorf("catalog entry has empty Code: %+v", e)
		}
		if e.HTTPStatus < 400 || e.HTTPStatus > 599 {
			t.Errorf("%s: HTTP status %d is not a 4xx/5xx", e.Code, e.HTTPStatus)
		}
		if e.Description == "" {
			t.Errorf("%s: empty Description", e.Code)
		}
		if seen[e.Code] {
			t.Errorf("duplicate catalog code %s", e.Code)
		}
		seen[e.Code] = true
	}
}

func TestCatalogStatuses(t *testing.T) {
	want := map[string]int{
		"NoSuchBucket":            http.StatusNotFound,
		"NoSuchKey":               http.StatusNotFound,
		"BucketNotEmpty":          http.StatusConflict,
		"BucketAlreadyOwnedByYou": http.StatusConflict,
		"SignatureDoesNotMatch":   http.StatusForbidden,
		"InvalidAccessKeyId":      http.StatusForbidden,
		"SlowDown":                http.StatusServiceUnavailable,
		"NotImplemented":          http.StatusNotImplemented,
	}
	got := map[string]int{}
	for _, e := range catalog {
		got[e.Code] = e.HTTPStatus
	}
	for code, status := range want {
		if got[code] != status {
			t.Errorf("%s: status = %d, want %d", code, got[code], status)
		}
	}
}

func TestToAPIErrorMapping(t *testing.T) {
	cases := []struct {
		err  error
		code string
	}{
		{object.ErrBucketNotFound, "NoSuchBucket"},
		{object.ErrObjectNotFound, "NoSuchKey"},
		{object.ErrBucketExists, "BucketAlreadyOwnedByYou"},
		{object.ErrBucketNotEmpty, "BucketNotEmpty"},
		{object.ErrInvalidArgument, "InvalidArgument"},
		{object.ErrNotImplemented, "NotImplemented"},
		{object.ErrReadQuorum, "SlowDown"},
		{object.ErrWriteQuorum, "SlowDown"},
		{errors.New("some other failure"), "InternalError"},
	}
	for _, c := range cases {
		if got := toAPIError(c.err); got.Code != c.code {
			t.Errorf("toAPIError(%v) = %s, want %s", c.err, got.Code, c.code)
		}
	}
}
