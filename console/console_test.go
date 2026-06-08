// SPDX-License-Identifier: Apache-2.0

package console

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/admin"
	"github.com/tamnd/liteio/auth"
)

// newJar builds a cookie jar so a login cookie carries to later requests.
func newJar() http.CookieJar {
	jar, _ := cookiejar.New(nil)
	return jar
}

// hasCookie reports whether a response sets a cookie with the given name to a
// non-empty value (a cleared cookie has an empty value).
func hasCookie(resp *http.Response, name string) bool {
	for _, c := range resp.Cookies() {
		if c.Name == name && c.Value != "" {
			return true
		}
	}
	return false
}

// harness wires the console over an auth.Store that is at once the credential store,
// the authorizer, and the IAM behind the admin API, which is the production shape: a
// console login signs admin calls as the same identity the store knows.
type harness struct {
	t     *testing.T
	srv   *httptest.Server
	store *auth.Store
	clock *time.Time
}

const (
	adminKey = "ADMINKEY"
	adminSec = "adminsecret"
	plainKey = "PLAINKEY"
	plainSec = "plainsecret"
)

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := auth.NewStore("root", "rootsecret")
	// A console admin (consoleAdmin grants admin:*) and an ordinary S3 user with no
	// admin rights at all.
	if err := store.AddUser(auth.User{AccessKey: adminKey, SecretKey: adminSec, Policies: []string{"consoleAdmin"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddUser(auth.User{AccessKey: plainKey, SecretKey: plainSec, Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_700_000_000, 0).UTC()
	adminAPI := admin.NewServer(store, store, admin.WithClock(func() time.Time { return clock }))
	c, err := NewServer(store, store, adminAPI,
		WithInsecureCookie(),
		WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, store: store, clock: &clock}
}

// client returns an HTTP client with a cookie jar, so a login cookie carries to
// later calls the way a browser's would.
func (h *harness) client() *http.Client {
	jar := newJar()
	return &http.Client{Jar: jar}
}

// call issues a console API request carrying the CSRF header.
func (h *harness) call(cl *http.Client, method, path string, body any) *http.Response {
	h.t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatal(err)
	}
	req.Header.Set(csrfHeader, "1")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := cl.Do(req)
	if err != nil {
		h.t.Fatalf("%s %s: %v", method, path, err)
	}
	return resp
}

// login signs in and returns the cookie-bearing client.
func (h *harness) login(key, secret string) (*http.Client, *http.Response) {
	cl := h.client()
	resp := h.call(cl, http.MethodPost, "/api/login", loginRequest{AccessKey: key, SecretKey: secret})
	return cl, resp
}

func TestLoginSuccessAndSession(t *testing.T) {
	h := newHarness(t)
	cl, resp := h.login(adminKey, adminSec)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("login status = %d, want 200", resp.StatusCode)
	}
	// The session cookie is set, HttpOnly, and the SPA can ask who it is.
	if !hasCookie(resp, cookieName) {
		t.Fatal("login set no session cookie")
	}
	resp.Body.Close()

	who := h.call(cl, http.MethodGet, "/api/session", nil)
	defer who.Body.Close()
	if who.StatusCode != http.StatusOK {
		t.Fatalf("session status = %d, want 200", who.StatusCode)
	}
	var sr sessionResponse
	if err := json.NewDecoder(who.Body).Decode(&sr); err != nil {
		t.Fatal(err)
	}
	if sr.AccessKey != adminKey {
		t.Fatalf("whoami = %q, want %q", sr.AccessKey, adminKey)
	}
}

func TestLoginRejections(t *testing.T) {
	h := newHarness(t)
	cases := []struct {
		name, key, secret string
	}{
		{"wrong secret", adminKey, "nope"},
		{"unknown key", "GHOST", "whatever"},
		{"valid non-admin", plainKey, plainSec},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, resp := h.login(c.key, c.secret)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", resp.StatusCode)
			}
			if hasCookie(resp, cookieName) {
				t.Fatal("a failed login must not set a session cookie")
			}
		})
	}
}

func TestLoginBadJSON(t *testing.T) {
	h := newHarness(t)
	cl := h.client()
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/login", strings.NewReader("{not json"))
	req.Header.Set(csrfHeader, "1")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestCSRFHeaderRequired(t *testing.T) {
	h := newHarness(t)
	cl := h.client()
	// No CSRF header: forbidden, whatever the body.
	b, _ := json.Marshal(loginRequest{AccessKey: adminKey, SecretKey: adminSec})
	req, _ := http.NewRequest(http.MethodPost, h.srv.URL+"/api/login", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestBridgeSignsAdminCall(t *testing.T) {
	h := newHarness(t)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()

	// List users through the bridge: the console signs as the admin and the admin API
	// answers. The two seeded users plus root appear.
	lr := h.call(cl, http.MethodGet, "/api/admin/users", nil)
	defer lr.Body.Close()
	if lr.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(lr.Body)
		t.Fatalf("bridge list users = %d, want 200; body=%s", lr.StatusCode, body)
	}
	body, _ := io.ReadAll(lr.Body)
	if !strings.Contains(string(body), adminKey) {
		t.Fatalf("user list missing %q: %s", adminKey, body)
	}

	// Create a user through the bridge (a signed PUT with a body), then confirm it.
	put := h.call(cl, http.MethodPut, "/api/admin/users/NEWUSER", map[string]any{
		"secretKey": "newsecret",
		"policies":  []string{"readonly"},
	})
	put.Body.Close()
	if put.StatusCode/100 != 2 {
		t.Fatalf("bridge create user = %d, want 2xx", put.StatusCode)
	}
	if _, ok := h.store.User("NEWUSER"); !ok {
		t.Fatal("the bridged PUT did not create the user in the store")
	}
}

// captureHandler records the request it received so a test can assert what the bridge
// signed and forwarded, and returns a canned 200.
type captureHandler struct {
	method string
	path   string
	host   string
	auth   string
	body   string
	hit    bool
}

func (c *captureHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.hit = true
	c.method = r.Method
	c.path = r.URL.Path
	c.host = r.Host
	c.auth = r.Header.Get("Authorization")
	b, _ := io.ReadAll(r.Body)
	c.body = string(b)
	w.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(w, "ok")
}

// newHarnessWithS3 is newHarness with an S3 handler wired into the bucket-browser
// bridge.
func newHarnessWithS3(t *testing.T, s3 http.Handler) *harness {
	t.Helper()
	store := auth.NewStore("root", "rootsecret")
	if err := store.AddUser(auth.User{AccessKey: adminKey, SecretKey: adminSec, Policies: []string{"consoleAdmin"}}); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_700_000_000, 0).UTC()
	adminAPI := admin.NewServer(store, store, admin.WithClock(func() time.Time { return clock }))
	c, err := NewServer(store, store, adminAPI,
		WithS3(s3),
		WithInsecureCookie(),
		WithClock(func() time.Time { return clock }))
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(c)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv, store: store, clock: &clock}
}

func TestS3BridgeSignsAndMapsPath(t *testing.T) {
	rec := &captureHandler{}
	h := newHarnessWithS3(t, rec)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()

	// A list-objects call: /api/s3/<bucket>?list-type=2 maps to /<bucket> with the
	// query preserved, signed as the admin, under the synthetic S3 host.
	r := h.call(cl, http.MethodGet, "/api/s3/photos?list-type=2", nil)
	r.Body.Close()
	if !rec.hit {
		t.Fatal("the S3 bridge did not reach the S3 handler")
	}
	if rec.path != "/photos" {
		t.Errorf("forwarded path = %q, want /photos", rec.path)
	}
	if rec.host != s3BridgeHost {
		t.Errorf("forwarded host = %q, want %q", rec.host, s3BridgeHost)
	}
	if !strings.HasPrefix(rec.auth, "AWS4-HMAC-SHA256 ") {
		t.Errorf("forwarded call was not SigV4-signed, Authorization = %q", rec.auth)
	}
}

func TestS3BridgeListBucketsHitsRoot(t *testing.T) {
	rec := &captureHandler{}
	h := newHarnessWithS3(t, rec)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()

	// /api/s3/ is the service root (ListBuckets): it maps to "/".
	r := h.call(cl, http.MethodGet, "/api/s3/", nil)
	r.Body.Close()
	if !rec.hit || rec.path != "/" {
		t.Errorf("ListBuckets mapped to %q (hit=%v), want /", rec.path, rec.hit)
	}
}

func TestS3BridgeNeedsSession(t *testing.T) {
	rec := &captureHandler{}
	h := newHarnessWithS3(t, rec)
	cl := h.client() // never logs in
	resp := h.call(cl, http.MethodGet, "/api/s3/photos", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated S3 bridge = %d, want 401", resp.StatusCode)
	}
	if rec.hit {
		t.Error("an unauthenticated call must not reach the S3 handler")
	}
}

func TestS3BridgeAbsentWithoutHandler(t *testing.T) {
	// The default harness wires no S3 handler, so /api/s3/ is not a bridge route and
	// the SPA fallback serves the shell rather than a 401 or a nil-handler panic.
	h := newHarness(t)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()
	r := h.call(cl, http.MethodGet, "/api/s3/photos", nil)
	defer r.Body.Close()
	body, _ := io.ReadAll(r.Body)
	if r.StatusCode != http.StatusOK || !strings.Contains(string(body), "liteio console") {
		t.Errorf("without S3 handler, /api/s3/ = %d, want the SPA shell", r.StatusCode)
	}
}

func TestBridgeNeedsSession(t *testing.T) {
	h := newHarness(t)
	cl := h.client() // never logs in
	resp := h.call(cl, http.MethodGet, "/api/admin/users", nil)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated bridge = %d, want 401", resp.StatusCode)
	}
}

func TestBridgeAuthorizesPerAction(t *testing.T) {
	// A user who can see the console (admin:ServerInfo via the diagnostics policy) but
	// lacks IAM rights logs in, yet the admin API denies the IAM call. The bridge
	// grants nothing on its own.
	h := newHarness(t)
	if err := h.store.AddUser(auth.User{AccessKey: "DIAG", SecretKey: "diagsecret", Policies: []string{"diagnostics"}}); err != nil {
		t.Fatal(err)
	}
	cl, resp := h.login("DIAG", "diagsecret")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("diagnostics login = %d, want 200 (it has admin:ServerInfo)", resp.StatusCode)
	}
	resp.Body.Close()
	denied := h.call(cl, http.MethodGet, "/api/admin/users", nil)
	defer denied.Body.Close()
	if denied.StatusCode != http.StatusForbidden {
		t.Fatalf("diagnostics listing users = %d, want 403", denied.StatusCode)
	}
}

func TestLogout(t *testing.T) {
	h := newHarness(t)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()

	out := h.call(cl, http.MethodPost, "/api/logout", nil)
	out.Body.Close()
	if out.StatusCode != http.StatusNoContent {
		t.Fatalf("logout = %d, want 204", out.StatusCode)
	}
	// The session is dead: whoami is now 401.
	who := h.call(cl, http.MethodGet, "/api/session", nil)
	defer who.Body.Close()
	if who.StatusCode != http.StatusUnauthorized {
		t.Fatalf("post-logout session = %d, want 401", who.StatusCode)
	}
}

func TestSessionExpiry(t *testing.T) {
	h := newHarness(t)
	cl, resp := h.login(adminKey, adminSec)
	resp.Body.Close()

	// Just before the TTL: still signed in.
	*h.clock = h.clock.Add(sessionTTL - time.Second)
	if who := h.call(cl, http.MethodGet, "/api/session", nil); who.StatusCode != http.StatusOK {
		who.Body.Close()
		t.Fatalf("before TTL session = %d, want 200", who.StatusCode)
	} else {
		who.Body.Close()
	}
	// At the TTL: rejected, and Sweep collects it.
	*h.clock = h.clock.Add(time.Second)
	who := h.call(cl, http.MethodGet, "/api/session", nil)
	who.Body.Close()
	if who.StatusCode != http.StatusUnauthorized {
		t.Fatalf("at TTL session = %d, want 401", who.StatusCode)
	}
}

func TestStaticAssetsAndSPAFallback(t *testing.T) {
	h := newHarness(t)
	cl := h.client()

	// The root serves the SPA shell.
	root, err := cl.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := io.ReadAll(root.Body)
	root.Body.Close()
	if root.StatusCode != http.StatusOK || !strings.Contains(string(rb), "liteio console") {
		t.Fatalf("root = %d, body lacks title: %s", root.StatusCode, rb)
	}

	// A real asset is served as itself.
	js, err := cl.Get(h.srv.URL + "/app.js")
	if err != nil {
		t.Fatal(err)
	}
	jb, _ := io.ReadAll(js.Body)
	js.Body.Close()
	if js.StatusCode != http.StatusOK || !strings.Contains(string(jb), "liteio") {
		t.Fatalf("app.js = %d", js.StatusCode)
	}

	// An unknown client-side route falls back to index.html (deep links survive a
	// refresh) rather than 404.
	deep, err := cl.Get(h.srv.URL + "/buckets/some/deep/link")
	if err != nil {
		t.Fatal(err)
	}
	db, _ := io.ReadAll(deep.Body)
	deep.Body.Close()
	if deep.StatusCode != http.StatusOK || !strings.Contains(string(db), "liteio console") {
		t.Fatalf("SPA fallback = %d", deep.StatusCode)
	}
}

func TestSecureCookieDefault(t *testing.T) {
	// Without WithInsecureCookie the session cookie is marked Secure.
	store := auth.NewStore("root", "rootsecret")
	if err := store.AddUser(auth.User{AccessKey: adminKey, SecretKey: adminSec, Policies: []string{"consoleAdmin"}}); err != nil {
		t.Fatal(err)
	}
	adminAPI := admin.NewServer(store, store)
	c, err := NewServer(store, store, adminAPI)
	if err != nil {
		t.Fatal(err)
	}
	ck := c.cookie("tok", 100)
	if !ck.Secure || !ck.HttpOnly || ck.SameSite != http.SameSiteStrictMode {
		t.Fatalf("cookie attrs = secure:%v httponly:%v samesite:%v", ck.Secure, ck.HttpOnly, ck.SameSite)
	}
}

func BenchmarkSessionLookup(b *testing.B) {
	store := newSessionStore(func() time.Time { return time.Unix(1_700_000_000, 0).UTC() })
	tok, err := store.create(auth.Credentials{AccessKey: "K", SecretKey: "S"}, time.Hour)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, ok := store.lookup(tok); !ok {
			b.Fatal("lookup miss")
		}
	}
}
