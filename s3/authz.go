// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"errors"
	"net"
	"net/http"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
)

// Authorizer decides whether an authenticated access key may perform a request,
// combining the caller's identity policies with the bucket's resource policy
// (auth.Store satisfies it). The front door consults it after SigV4
// authentication; a nil Authorizer leaves the server authenticating only.
type Authorizer interface {
	AuthorizeS3(accessKey string, bucketPolicy *auth.Policy, req auth.Request) (bool, error)
}

// WithAuthorizer turns on policy-based authorization: every authenticated request
// is mapped to an action and resource ARN, combined with the bucket policy, and
// denied with AccessDenied unless the identity (and any resource policy) grants
// it. Without it the server authenticates the signature but authorizes nothing.
func WithAuthorizer(a Authorizer) Option { return func(s *Server) { s.authz = a } }

// authorize maps a request to its S3 action and resource, loads the bucket policy,
// and asks the Authorizer for a verdict. It returns a non-nil APIError to deny;
// the zero value means proceed. With no Authorizer configured it always proceeds.
func (s *Server) authorize(r *http.Request, vr *sign.VerifiedRequest, res resource) APIError {
	if s.authz == nil {
		return APIError{}
	}
	action, resourceARN := s.actionFor(r, res)
	req := auth.Request{
		Action:   action,
		Resource: resourceARN,
		Context:  requestContext(r, vr.AccessKey),
	}
	var bucketPolicy *auth.Policy
	if res.bucket != "" {
		if p, err := s.loadBucketPolicy(r, res.bucket); err != nil {
			return toAPIError(err)
		} else if p != nil {
			bucketPolicy = p
		}
	}
	ok, err := s.authz.AuthorizeS3(vr.AccessKey, bucketPolicy, req)
	if err != nil {
		// An unknown or expired key reaching authorization (the signature already
		// verified) is an access failure, not an internal one.
		if errors.Is(err, auth.ErrNotFound) || errors.Is(err, auth.ErrExpired) {
			return errAccessDenied
		}
		return errInternalError
	}
	if !ok {
		return errAccessDenied
	}
	return APIError{}
}

// loadBucketPolicy fetches and parses the bucket's attached policy. A bucket with
// no policy (or one that does not yet exist, as on CreateBucket) yields a nil
// policy and no error, so authorization rests on the identity path; the handler
// then performs the real existence check.
func (s *Server) loadBucketPolicy(r *http.Request, bucket string) (*auth.Policy, error) {
	doc, err := s.layer.GetBucketPolicy(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, object.ErrNoSuchBucketPolicy) || errors.Is(err, object.ErrBucketNotFound) {
			return nil, nil
		}
		return nil, err
	}
	p, err := auth.ParseBucketPolicy(doc)
	if err != nil {
		// A stored policy that no longer parses must not silently grant; fail closed.
		return nil, err
	}
	return &p, nil
}

// requestContext fills the condition-key context from the HTTP request: the source
// IP, whether the transport was TLS, the caller's access key, the current time,
// and the list parameters a statement may scope on.
func requestContext(r *http.Request, accessKey string) map[string]string {
	ctx := map[string]string{
		auth.CondUsername:        accessKey,
		auth.CondSecureTransport: boolString(r.TLS != nil),
		auth.CondCurrentTime:     time.Now().UTC().Format(time.RFC3339),
	}
	if ip := sourceIP(r); ip != "" {
		ctx[auth.CondSourceIP] = ip
	}
	q := r.URL.Query()
	for param, key := range map[string]string{
		"prefix":    auth.CondPrefix,
		"max-keys":  auth.CondMaxKeys,
		"delimiter": auth.CondDelimiter,
	} {
		if q.Has(param) {
			ctx[key] = q.Get(param)
		}
	}
	return ctx
}

// sourceIP extracts the caller's IP from RemoteAddr, dropping the port. An address
// that does not parse yields the empty string, so the aws:SourceIp key is absent
// rather than wrong.
func sourceIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// actionFor maps an HTTP request to the S3 action it performs and the ARN it acts
// on, mirroring the routing in serveService/serveBucket/serveObject. The mapping
// follows the AWS action catalog: HeadBucket is s3:ListBucket, HeadObject is
// s3:GetObject, and the multipart and copy operations resolve to the put/get/list
// actions AWS authorizes them under.
func (s *Server) actionFor(r *http.Request, res resource) (action, arn string) {
	q := r.URL.Query()
	switch {
	case res.bucket == "":
		// Service level: only ListAllMyBuckets is reachable.
		return "s3:ListAllMyBuckets", auth.BucketARN("*")
	case res.object == "":
		return bucketAction(r.Method, q), auth.BucketARN(res.bucket)
	default:
		return objectAction(r.Method, q), auth.ObjectARN(res.bucket, res.object)
	}
}

// bucketAction resolves a bucket-level request to its action.
func bucketAction(method string, q queryHas) string {
	switch method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return "s3:GetBucketLocation"
		case q.Has("versioning"):
			return "s3:GetBucketVersioning"
		case q.Has("policy"):
			return "s3:GetBucketPolicy"
		case q.Has("versions"):
			return "s3:ListBucketVersions"
		case q.Has("uploads"):
			return "s3:ListBucketMultipartUploads"
		default:
			return "s3:ListBucket"
		}
	case http.MethodPut:
		switch {
		case q.Has("versioning"):
			return "s3:PutBucketVersioning"
		case q.Has("policy"):
			return "s3:PutBucketPolicy"
		default:
			return "s3:CreateBucket"
		}
	case http.MethodHead:
		return "s3:ListBucket"
	case http.MethodDelete:
		if q.Has("policy") {
			return "s3:DeleteBucketPolicy"
		}
		return "s3:DeleteBucket"
	case http.MethodPost:
		if q.Has("delete") {
			// Bulk delete: authorized coarsely as DeleteObject over the bucket; the
			// handler still resolves each key.
			return "s3:DeleteObject"
		}
	}
	return ""
}

// objectAction resolves an object-level request to its action.
func objectAction(method string, q queryHas) string {
	switch method {
	case http.MethodPut:
		// PutObject, UploadPart, UploadPartCopy and CopyObject all authorize as
		// s3:PutObject on the destination.
		return "s3:PutObject"
	case http.MethodGet:
		if q.Get("uploadId") != "" {
			return "s3:ListMultipartUploadParts"
		}
		return "s3:GetObject"
	case http.MethodHead:
		return "s3:GetObject"
	case http.MethodPost:
		// CreateMultipartUpload and CompleteMultipartUpload write the object.
		return "s3:PutObject"
	case http.MethodDelete:
		if q.Get("uploadId") != "" {
			return "s3:AbortMultipartUpload"
		}
		return "s3:DeleteObject"
	}
	return ""
}

// queryHas is the slice of url.Values methods the action mapping needs, so the
// mappers can be unit-tested without constructing a full request.
type queryHas interface {
	Has(string) bool
	Get(string) string
}
