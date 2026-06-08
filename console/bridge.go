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

// maxBridgeBody caps a bridged request body. Admin calls carry small JSON (a user, a
// policy); a larger body is refused before it is read into memory to hash.
const maxBridgeBody = 1 << 20 // 1 MiB

// handleBridge forwards a SPA call to the in-process admin API, signed with the
// session's credentials. The browser holds only the session cookie; the secret key
// stays on the server, so the console never has to ship a signing key to JavaScript.
// The admin API authorizes the call per action exactly as it does for the CLI, so
// the bridge grants no rights of its own: a non-admin session reaches the admin API
// and is denied there.
func (s *Server) handleBridge(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}

	// Read and hash the body so the signature covers it, then hand the handler a
	// fresh reader over the same bytes.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBridgeBody))
	if err != nil {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body too large")
		return
	}
	sum := sha256.Sum256(body)
	payloadHash := hex.EncodeToString(sum[:])

	// Map /api/admin/<rest> onto the admin route prefix, preserving the query.
	rest := strings.TrimPrefix(r.URL.Path, "/api/admin")
	target := s.adminPath + rest
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, bytes.NewReader(body))
	if err != nil {
		writeJSONError(w, http.StatusBadGateway, "could not build the admin request")
		return
	}
	req.Host = "console.liteio"
	req.ContentLength = int64(len(body))
	if ct := r.Header.Get("Content-Type"); ct != "" {
		req.Header.Set("Content-Type", ct)
	}

	// Sign and serve in-process: the same request object is signed and dispatched, so
	// the signature is self-consistent whatever the synthetic host.
	sign.SignHeader(req, sess.creds, s.region, payloadHash, s.now().UTC())
	s.admin.ServeHTTP(w, req)
}
