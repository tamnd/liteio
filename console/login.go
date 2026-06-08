// SPDX-License-Identifier: Apache-2.0

package console

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"

	"github.com/tamnd/liteio/auth"
)

// adminResource is the ARN admin actions authorize against, matching the admin API
// and the consoleAdmin canned policy (doc 08).
const adminResource = "arn:aws:s3:::*"

// loginGateAction is the admin action a sign-in must be allowed before the console
// admits it. ServerInfo is the dashboard's landing call, so granting it is what
// "can see the console" means; consoleAdmin (admin:*), the diagnostics policy, and
// root all carry it. A credential without it is a valid identity that simply has no
// console to see, and is turned away at the door rather than into an empty UI.
const loginGateAction = "admin:ServerInfo"

// loginRequest is the JSON the SPA posts to sign in.
type loginRequest struct {
	AccessKey string `json:"accessKey"`
	SecretKey string `json:"secretKey"`
}

// sessionResponse is the whoami payload: who the browser is signed in as.
type sessionResponse struct {
	AccessKey string `json:"accessKey"`
}

// handleLogin verifies the posted credentials against the credential store and the
// authorizer, then issues a session cookie. It rejects with one 401 whether the key
// is unknown, the secret is wrong, or the identity lacks admin rights, so a probe
// cannot tell which: the console is not an oracle for which keys exist.
func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	var req loginRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<10)).Decode(&req); err != nil {
		writeJSONError(w, http.StatusBadRequest, "the request body is not valid JSON")
		return
	}
	if !s.validLogin(req) {
		writeJSONError(w, http.StatusUnauthorized, "sign-in failed")
		return
	}
	token, err := s.sessions.create(auth.Credentials{AccessKey: req.AccessKey, SecretKey: req.SecretKey}, sessionTTL)
	if err != nil {
		writeJSONError(w, http.StatusInternalServerError, "could not start a session")
		return
	}
	http.SetCookie(w, s.cookie(token, int(sessionTTL.Seconds())))
	writeJSON(w, http.StatusOK, sessionResponse{AccessKey: req.AccessKey})
}

// validLogin reports whether the credentials are correct and carry admin rights.
// The secret is compared in constant time; the admin gate is the authorizer's call.
func (s *Server) validLogin(req loginRequest) bool {
	if req.AccessKey == "" || req.SecretKey == "" {
		return false
	}
	creds, ok := s.creds.Get(req.AccessKey)
	if !ok {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(creds.SecretKey), []byte(req.SecretKey)) != 1 {
		return false
	}
	allowed, err := s.authz.IsAllowed(req.AccessKey, auth.Request{
		Action:   loginGateAction,
		Resource: adminResource,
		Context:  map[string]string{auth.CondUsername: req.AccessKey},
	})
	return err == nil && allowed
}

// handleLogout drops the session and clears the cookie. It is idempotent: a request
// with no or an unknown cookie still returns 200 with the cookie cleared.
func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	if c, err := r.Cookie(cookieName); err == nil {
		s.sessions.destroy(c.Value)
	}
	http.SetCookie(w, s.cookie("", -1))
	w.WriteHeader(http.StatusNoContent)
}

// handleSession is whoami: it returns who the live session belongs to, or 401 if
// there is none. The SPA calls it on load to decide between the login and the app.
func (s *Server) handleSession(w http.ResponseWriter, r *http.Request) {
	if !s.checkCSRF(w, r) {
		return
	}
	sess, ok := s.currentSession(r)
	if !ok {
		writeJSONError(w, http.StatusUnauthorized, "not signed in")
		return
	}
	writeJSON(w, http.StatusOK, sessionResponse{AccessKey: sess.creds.AccessKey})
}

// currentSession resolves the live session a request's cookie names.
func (s *Server) currentSession(r *http.Request) (session, bool) {
	c, err := r.Cookie(cookieName)
	if err != nil {
		return session{}, false
	}
	return s.sessions.lookup(c.Value)
}

// checkCSRF enforces the custom-header guard on every API call. A browser cannot set
// X-Liteio-Console on a cross-origin request without a preflight the console never
// answers, so its presence proves the call came from the console's own origin.
func (s *Server) checkCSRF(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get(csrfHeader) == "" {
		writeJSONError(w, http.StatusForbidden, "missing "+csrfHeader+" header")
		return false
	}
	return true
}

// cookie builds the session cookie with the security attributes the console relies
// on: HttpOnly so script cannot read the token, SameSite=Strict and the CSRF header
// so it does not ride cross-site requests, and Secure in production.
func (s *Server) cookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{
		Name:     cookieName,
		Value:    value,
		Path:     "/",
		MaxAge:   maxAge,
		HttpOnly: true,
		Secure:   s.secure,
		SameSite: http.SameSiteStrictMode,
	}
}

// writeJSON encodes v as a JSON response with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a {"error": msg} body with the given status.
func writeJSONError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
