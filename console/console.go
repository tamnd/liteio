// SPDX-License-Identifier: Apache-2.0

// Package console is liteio's free web console (spec 2020, doc 10.1): the
// management UI MinIO removed from its community edition, shipped here Apache-2.0
// and embedded in the server binary via go:embed so the "single binary" promise
// (doc 01) still holds.
//
// The console serves a small framework-free single-page app and a thin backend: a
// login that authenticates a user against the same IAM as the S3 front door, a
// server-side session so the browser never holds a secret key, and a signed bridge
// that forwards the SPA's calls to the in-process admin REST API (doc 10.2) with the
// session's credentials. The console has no backdoor of its own — every management
// action goes through the admin API and is authorized there per action, exactly as
// the CLI's calls are.
//
// This is the M5 foundation: the embedded server, the session lifecycle, the login,
// and the signed bridge. The bucket, identity, and cluster views are later
// subsystems that ride this frame and the admin routes it reaches.
package console

import (
	"embed"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"time"

	"github.com/tamnd/liteio/admin"
	"github.com/tamnd/liteio/auth"
)

// embedded is the single-page app baked into the binary. all: includes files an
// ordinary embed skips (none here begin with _ or ., but the directive is
// future-proof for assets that might).
//
//go:embed all:ui
var embedded embed.FS

// sessionTTL is how long a console login stays valid; a fresh login renews it.
const sessionTTL = 8 * time.Hour

// csrfHeader is the custom header the SPA sends on every API call. A browser cannot
// set it on a cross-origin request without a CORS preflight the console never
// answers, so requiring it blocks cross-site requests riding the session cookie.
const csrfHeader = "X-Liteio-Console"

// cookieName is the session cookie the login sets and the bridge reads.
const cookieName = "liteio_console"

// Authorizer is the slice of the IAM store the console needs to gate login: a
// successful sign-in must be both a valid credential and one with admin rights.
// auth.Store satisfies it.
type Authorizer interface {
	IsAllowed(accessKey string, req auth.Request) (bool, error)
}

// Server is the console HTTP handler: static assets, the auth endpoints, and the
// signed admin bridge, all under one mux served on the console's own port.
type Server struct {
	creds     auth.CredentialStore // verifies the login secret
	authz     Authorizer           // gates login on admin rights
	admin     http.Handler         // in-process admin API the bridge forwards to
	adminPath string               // admin route prefix the bridge targets
	s3        http.Handler         // in-process S3 API the bucket browser forwards to (nil disables it)
	region    string               // SigV4 region the bridge signs with
	sessions  *sessionStore
	now       func() time.Time
	secure    bool // mark the session cookie Secure (true in production over TLS)
	mux       *http.ServeMux
	staticH   http.Handler // serves the embedded SPA with index.html fallback
}

// Option configures a Server.
type Option func(*Server)

// WithClock overrides the clock (tests inject a fixed instant).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithInsecureCookie drops the Secure flag on the session cookie so the console
// works over plain HTTP in development. Production serves the console over TLS and
// leaves this off.
func WithInsecureCookie() Option { return func(s *Server) { s.secure = false } }

// WithRegion sets the SigV4 region the bridge signs admin calls with. The admin API
// derives the region from the signature's own scope, so any value verifies as long
// as the bridge is self-consistent; the default is "us-east-1".
func WithRegion(region string) Option { return func(s *Server) { s.region = region } }

// WithS3 wires the in-process S3 API the bucket browser forwards signed calls to.
// Without it the console serves identity and cluster views only and the /api/s3
// bridge route is not registered (a 404 the SPA reads as "no object browser here").
func WithS3(h http.Handler) Option { return func(s *Server) { s.s3 = h } }

// NewServer builds the console over the credential store and authorizer that gate
// login and the admin handler the bridge forwards signed calls to.
func NewServer(creds auth.CredentialStore, authz Authorizer, adminAPI http.Handler, opts ...Option) (*Server, error) {
	sub, err := fs.Sub(embedded, "ui")
	if err != nil {
		return nil, err
	}
	s := &Server{
		creds:     creds,
		authz:     authz,
		admin:     adminAPI,
		adminPath: admin.APIPrefix,
		region:    "us-east-1",
		now:       time.Now,
		secure:    true,
	}
	for _, o := range opts {
		o(s)
	}
	s.sessions = newSessionStore(s.now)
	s.staticH = spaHandler{fsys: sub}
	s.mux = s.routes()
	return s, nil
}

// Sweep removes expired sessions; an operator can call it on a ticker.
func (s *Server) Sweep() int { return s.sessions.Sweep() }

// routes wires the auth endpoints, the bridge, and the SPA fallback. Order matters:
// the API patterns are more specific than the "/" catch-all the SPA handler serves.
func (s *Server) routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/login", s.handleLogin)
	mux.HandleFunc("POST /api/logout", s.handleLogout)
	mux.HandleFunc("GET /api/session", s.handleSession)
	mux.HandleFunc("/api/admin/", s.handleBridge)
	// The S3 bridge backs the bucket browser; register it only when an S3 handler is
	// wired, so a node without an object layer simply does not advertise it.
	if s.s3 != nil {
		mux.HandleFunc("/api/s3/", s.handleS3Bridge)
	}
	// Anything the SPA's client-side router owns falls through to the assets,
	// which serve index.html for unknown paths.
	mux.Handle("/", s.staticH)
	return mux
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mux.ServeHTTP(w, r)
}

// spaHandler serves the embedded single-page app. A request for a real file is
// served as-is; anything else returns index.html so the client-side router can take
// over (deep links work on refresh). It never directory-lists.
type spaHandler struct{ fsys fs.FS }

func (h spaHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Path
	if name == "/" {
		h.serveIndex(w, r)
		return
	}
	clean := name[1:] // strip leading slash for fs.FS
	if !fs.ValidPath(clean) {
		// A path fs.FS rejects (a trailing slash, "..", and the like) is never a real
		// asset; serve the shell so the client-side router can resolve it.
		h.serveIndex(w, r)
		return
	}
	f, err := h.fsys.Open(clean)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			h.serveIndex(w, r)
			return
		}
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		h.serveIndex(w, r)
		return
	}
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, info.Name(), info.ModTime(), rs)
}

func (h spaHandler) serveIndex(w http.ResponseWriter, r *http.Request) {
	f, err := h.fsys.Open("index.html")
	if err != nil {
		http.Error(w, "console assets missing", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()
	info, _ := f.Stat()
	rs, ok := f.(io.ReadSeeker)
	if !ok {
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	http.ServeContent(w, r, "index.html", info.ModTime(), rs)
}
