// SPDX-License-Identifier: Apache-2.0

package console

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"

	"github.com/tamnd/liteio/s3/sign"
)

// maxBridgeBody caps a buffered bridged request body. Admin calls carry small JSON (a
// user, a policy) and the S3 management calls the console buffers are short (create a
// bucket, copy headers), so the body is read into memory to hash and a larger one is
// refused before that read. Object uploads do not take this path: the S3 bridge streams
// PUT and POST bodies straight through (see forwardStream), so an upload is bounded by
// the object store, not by this cap.
const maxBridgeBody = 1 << 20 // 1 MiB

// adminBridgeHost is the synthetic Host the admin bridge signs and dispatches under.
// The admin API derives the signing scope from the request itself, so any stable host
// verifies as long as the signed and served request are the same object.
const adminBridgeHost = "console.liteio"

// s3BridgeHost is the synthetic Host the S3 bridge signs under. Path-style addressing
// means the bucket comes from the path, not the host, so this host is never parsed as
// a bucket; the S3 server resolves /<bucket>/<key> from the path.
const s3BridgeHost = "s3.liteio"

// handleBridge forwards a SPA call to the in-process admin API, signed with the
// session's credentials. The browser holds only the session cookie; the secret key
// stays on the server, so the console never has to ship a signing key to JavaScript.
// The admin API authorizes the call per action exactly as it does for the CLI, so
// the bridge grants no rights of its own: a non-admin session reaches the admin API
// and is denied there.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.bridgePreflight(w, r)
	if !ok {
		return
	}
	// Map /api/admin/<rest> onto the admin route prefix.
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/api/admin")
	s.forward(w, r, sess, s.admin, adminBridgeHost, s.adminPath+rest)
}

// handleS3Bridge forwards a SPA call to the in-process S3 API, signed with the
// session's credentials, so the console can list buckets, browse objects, and run
// bucket and object management against the same data plane the S3 clients use. The
// S3 server verifies the signature and authorizes the operation by IAM policy exactly
// as it does for an external client, so the bridge again grants nothing of its own.
func (s *Server) handleS3Bridge(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.bridgePreflight(w, r)
	if !ok {
		return
	}
	// Map /api/s3/<rest> onto the S3 service path. An empty rest is the service root
	// (ListBuckets); a bucket or bucket/key follows the prefix verbatim.
	rest := strings.TrimPrefix(r.URL.EscapedPath(), "/api/s3")
	if rest == "" {
		rest = "/"
	}
	// An upload (PutObject, multipart parts) can be arbitrarily large, so stream the
	// body straight to the S3 server rather than buffering it to hash. Browse, head,
	// and delete carry no body and take the buffered path with the rest of the bridge.
	if r.Method == http.MethodPut || r.Method == http.MethodPost {
		s.forwardStream(w, r, sess, s.s3, s3BridgeHost, rest)
		return
	}
	s.forward(w, r, sess, s.s3, s3BridgeHost, rest)
}

// bridgePreflight runs the two checks every bridged call shares: the CSRF header and a
// live session. It returns the session and true when the call may proceed.
func (s *Server) bridgePreflight(w http.ResponseWriter, r *http.Request) (session, bool) {
	if !s.checkCSRF(w, r) {
		return session{}, false
	}
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return session{}, false
	}
	return sess, true
}

// forward reads the SPA request body, builds a fresh request to target under the
// given synthetic host, signs it with the session's credentials, and dispatches it to
// handler in-process. Signing and serving the same request object keeps the signature
// self-consistent whatever the synthetic host. It is the shared core of the admin and
// S3 bridges.
func (s *Server) forward(w http.ResponseWriter, r *http.Request, sess session, handler http.Handler, host, target string) {
	// Read and hash the body so the signature covers it, then hand the handler a
	// fresh reader over the same bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBridgeBody))
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])
	s.dispatch(w, r, sess, handler, host, target, bytes.NewReader(body), int64(len(body)), payloadHash)
}

// forwardStream forwards a SPA request without buffering its body. It signs with an
// unsigned-payload hash so the signature does not depend on the bytes, then hands the
// handler the request body directly. An object upload can be far larger than memory, so
// streaming it keeps the console's footprint flat and lifts the buffered body cap; the
// S3 server still enforces its own object size limits. The content length is forwarded
// verbatim, so a client that omits it (a chunked upload) reaches the handler as such.
func (s *Server) forwardStream(w http.ResponseWriter, r *http.Request, sess session, handler http.Handler, host, target string) {
	s.dispatch(w, r, sess, handler, host, target, r.Body, r.ContentLength, sign.UnsignedPayload)
}

// dispatch builds a fresh request to target under the given synthetic host over the
// supplied body, signs it with the session's credentials for the given payload hash,
// and serves it to handler in-process. Signing and serving the same request object
// keeps the signature self-consistent whatever the synthetic host. It is the shared
// core of the buffered and streaming bridges.
func (s *Server) dispatch(w http.ResponseWriter, r *http.Request, sess session, handler http.Handler, host, target string, body io.Reader, length int64, payloadHash string) {
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "could not build the bridged request")
		return
	}
	req.Host = host
	req.ContentLength = length
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	sign.SignHeader(req, sess.creds, s.region, payloadHash, s.now().UTC())
	handler.ServeHTTP(w, req)
}
