// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
)

// decodedBody adapts a decoded streaming reader back into an io.ReadCloser, so
// the handler reads object bytes while Close still releases the original body.
type decodedBody struct {
	io.Reader
	closer io.Closer
}

func (d *decodedBody) Close() error { return d.closer.Close() }

// Server is the S3 HTTP front door. It authenticates each request with SigV4,
// resolves the bucket/object from the URL (path-style or virtual-host), and
// dispatches to the handler that calls the object layer.
type Server struct {
	layer    object.ObjectLayer
	creds    auth.CredentialStore
	authz    Authorizer       // policy decision (nil authenticates only)
	sts      STSIssuer        // temporary-credential issuer (nil disables the STS endpoint)
	sessions SessionValidator // X-Amz-Security-Token check (nil skips it)
	domain   string           // virtual-host base domain ("" disables vhost)
	now      func() time.Time // clock seam for signature skew (tests inject)
}

// SessionValidator checks the X-Amz-Security-Token a request presents against the
// access key its signature verified under (auth.Store satisfies it). It is the seam
// the front door consults after SigV4 authentication: a temporary credential must
// carry the token it was issued with, a long-lived one must not carry a token at
// all, and an expired session is refused here. A nil error admits the request.
type SessionValidator interface {
	ValidateSession(accessKey, token string) error
}

// WithSessionValidator turns on X-Amz-Security-Token validation: every
// signature-verified request has its presented token (header or presigned query)
// checked against the credential it signed under. Without it the server trusts the
// signature alone, which is correct only for a deployment that never issues
// temporary credentials.
func WithSessionValidator(v SessionValidator) Option { return func(s *Server) { s.sessions = v } }

// Option configures a Server.
type Option func(*Server)

// WithDomain enables virtual-host-style addressing for the given base domain
// (e.g. "s3.example.com" routes bucket.s3.example.com). Empty keeps path-style.
func WithDomain(domain string) Option { return func(s *Server) { s.domain = domain } }

// WithClock overrides the clock used for signature skew checks (for tests).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// NewServer builds the front door over an object layer and credential store.
func NewServer(layer object.ObjectLayer, creds auth.CredentialStore, opts ...Option) *Server {
	s := &Server{layer: layer, creds: creds, now: time.Now}
	for _, o := range opts {
		o(s)
	}
	return s
}

// resource holds the bucket/object a request addresses.
type resource struct {
	bucket string
	object string
}

// ServeHTTP implements http.Handler: authenticate, parse the resource, dispatch.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	requestID := newRequestID()
	setCommonHeaders(w, requestID)

	vr, serr := sign.Verify(r, s.creds, s.now())
	if serr != nil {
		// An unsigned request may still reach the STS endpoint's federated flows
		// (AssumeRoleWithWebIdentity and the rest), where the identity token is the
		// credential and no SigV4 caller exists. Everything else needs a signature.
		if s.sts != nil && r.Method == http.MethodPost && s.parseResource(r).bucket == "" {
			s.serveSTS(w, r, requestID, nil)
			return
		}
		writeError(w, requestID, r.URL.Path, APIError{Code: serr.Code, Description: serr.Message, HTTPStatus: signStatus(serr.Code)})
		return
	}
	// The signature verified the caller holds the secret key; if the credential is a
	// temporary session, also bind the request to its X-Amz-Security-Token. This runs
	// before STS and the action gate so a session must present its token whatever the
	// operation, and so an expired session is refused even with no authorizer wired.
	if s.sessions != nil {
		if err := s.sessions.ValidateSession(vr.AccessKey, securityToken(r)); err != nil {
			writeError(w, requestID, r.URL.Path, errAccessDenied)
			return
		}
	}
	// For an aws-chunked upload, hand handlers a reader over the decoded object
	// bytes (with verified chunk signatures) and the real content length.
	if vr.IsStreaming() {
		orig := r.Body
		r.Body = &decodedBody{Reader: vr.DecodeBody(orig), closer: orig}
		if dec := r.Header.Get("x-amz-decoded-content-length"); dec != "" {
			if n, err := strconv.ParseInt(dec, 10, 64); err == nil {
				r.ContentLength = n
			}
		}
	}

	res := s.parseResource(r)
	// The STS endpoint (doc 08.4) lives at the service root as a POST form. It
	// predates the S3 action gate — issuing a credential is not an S3 operation, and
	// AssumeRole authorizes intrinsically (only root or a user may assume).
	if res.bucket == "" && r.Method == http.MethodPost {
		s.serveSTS(w, r, requestID, vr)
		return
	}
	if aerr := s.authorize(r, vr, res); aerr.Code != "" {
		writeError(w, requestID, r.URL.Path, aerr)
		return
	}
	switch {
	case res.bucket == "":
		s.serveService(w, r, requestID)
	case res.object == "":
		s.serveBucket(w, r, requestID, res.bucket)
	default:
		s.serveObject(w, r, requestID, res.bucket, res.object)
	}
}

// parseResource resolves bucket and object key from the request, honoring
// virtual-host addressing when a domain is configured.
func (s *Server) parseResource(r *http.Request) resource {
	host := r.Host
	if i := strings.IndexByte(host, ':'); i >= 0 {
		host = host[:i]
	}
	// Virtual-host style: <bucket>.<domain>.
	if s.domain != "" && host != s.domain && strings.HasSuffix(host, "."+s.domain) {
		bucket := strings.TrimSuffix(host, "."+s.domain)
		return resource{bucket: bucket, object: strings.TrimPrefix(r.URL.Path, "/")}
	}
	// Path style: /<bucket>/<key...>.
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" {
		return resource{}
	}
	if bucket, key, ok := strings.Cut(p, "/"); ok {
		return resource{bucket: bucket, object: key}
	}
	return resource{bucket: p}
}

// serveService handles service-level requests (the bucket-less root).
func (s *Server) serveService(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.Method == http.MethodGet {
		s.listBuckets(w, r, requestID)
		return
	}
	writeError(w, requestID, r.URL.Path, errMethodNotAllowed)
}

// serveBucket handles bucket-level requests and bucket subresources.
func (s *Server) serveBucket(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q := r.URL.Query()
	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			s.getBucketLocation(w, r, requestID, bucket)
		case q.Has("versioning"):
			s.getBucketVersioning(w, r, requestID, bucket)
		case q.Has("policy"):
			s.getBucketPolicy(w, r, requestID, bucket)
		case q.Has("versions"):
			s.listObjectVersions(w, r, requestID, bucket)
		case q.Has("uploads"):
			s.listMultipartUploads(w, r, requestID, bucket)
		default:
			s.listObjectsV2(w, r, requestID, bucket)
		}
	case http.MethodPut:
		switch {
		case q.Has("versioning"):
			s.putBucketVersioning(w, r, requestID, bucket)
		case q.Has("policy"):
			s.putBucketPolicy(w, r, requestID, bucket)
		default:
			s.createBucket(w, r, requestID, bucket)
		}
	case http.MethodHead:
		s.headBucket(w, r, requestID, bucket)
	case http.MethodDelete:
		if q.Has("policy") {
			s.deleteBucketPolicy(w, r, requestID, bucket)
			return
		}
		s.deleteBucket(w, r, requestID, bucket)
	case http.MethodPost:
		if q.Has("delete") {
			s.deleteObjects(w, r, requestID, bucket)
			return
		}
		writeError(w, requestID, r.URL.Path, errMethodNotAllowed)
	default:
		writeError(w, requestID, r.URL.Path, errMethodNotAllowed)
	}
}

// serveObject handles object-level requests, including the multipart-upload
// subresources keyed by the ?uploads / ?uploadId query parameters.
func (s *Server) serveObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	q := r.URL.Query()
	uploadID := q.Get("uploadId")
	switch r.Method {
	case http.MethodPut:
		copySource := r.Header.Get(copySourceHeader)
		if uploadID != "" && q.Has("partNumber") {
			if copySource != "" {
				s.uploadPartCopy(w, r, requestID, bucket, object, uploadID, copySource)
				return
			}
			s.uploadPart(w, r, requestID, bucket, object, uploadID)
			return
		}
		if copySource != "" {
			s.copyObject(w, r, requestID, bucket, object, copySource)
			return
		}
		s.putObject(w, r, requestID, bucket, object)
	case http.MethodGet:
		if uploadID != "" {
			s.listObjectParts(w, r, requestID, bucket, object, uploadID)
			return
		}
		s.getObject(w, r, requestID, bucket, object)
	case http.MethodHead:
		s.headObject(w, r, requestID, bucket, object)
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			s.newMultipartUpload(w, r, requestID, bucket, object)
		case uploadID != "":
			s.completeMultipartUpload(w, r, requestID, bucket, object, uploadID)
		default:
			writeError(w, requestID, r.URL.Path, errMethodNotAllowed)
		}
	case http.MethodDelete:
		if uploadID != "" {
			s.abortMultipartUpload(w, r, requestID, bucket, object, uploadID)
			return
		}
		s.deleteObject(w, r, requestID, bucket, object)
	default:
		writeError(w, requestID, r.URL.Path, errMethodNotAllowed)
	}
}

// securityToken returns the X-Amz-Security-Token a request presents. A header-signed
// request carries it in the header; a presigned URL carries it as a query parameter,
// where it is part of the signed query string. The empty string means none was sent.
func securityToken(r *http.Request) string {
	if t := r.Header.Get("X-Amz-Security-Token"); t != "" {
		return t
	}
	return r.URL.Query().Get("X-Amz-Security-Token")
}

// signStatus maps a SigV4 error code to its HTTP status.
func signStatus(code string) int {
	switch code {
	case "SignatureDoesNotMatch", "InvalidAccessKeyId", "AccessDenied":
		return http.StatusForbidden
	case "RequestTimeTooSkewed":
		return http.StatusForbidden
	case "MissingSecurityHeader", "AuthorizationHeaderMalformed", "InvalidRequest":
		return http.StatusBadRequest
	default:
		return http.StatusForbidden
	}
}
