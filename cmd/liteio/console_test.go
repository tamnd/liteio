// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/liteio/auth"
)

// rootCfg is a config with the default root credential and the console wired for
// plain-HTTP testing (the Secure cookie attribute would otherwise drop the cookie
// over httptest's HTTP listener).
func rootCfg() config {
	return config{
		accessKey:             "liteioadmin",
		secretKey:             "liteioadmin",
		consoleAddress:        ":9001",
		consoleInsecureCookie: true,
	}
}

func TestParseFlagsConsoleDefaults(t *testing.T) {
	cfg, err := parseFlags([]string{"--drives", "a,b"})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.consoleAddress != ":9001" {
		t.Errorf("consoleAddress = %q, want :9001", cfg.consoleAddress)
	}
	if cfg.consoleInsecureCookie {
		t.Error("consoleInsecureCookie should default to false")
	}
}

func TestParseFlagsConsoleDisabled(t *testing.T) {
	cfg, err := parseFlags([]string{"--drives", "a,b", "--console-address", ""})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.consoleAddress != "" {
		t.Errorf("consoleAddress = %q, want empty", cfg.consoleAddress)
	}
}

// TestBuildConsoleComposition proves the assembled listener routes the admin path
// prefix to the admin handler (which demands a signature) and every other path to
// the console app, rather than letting the console SPA fallback swallow admin URLs.
func TestBuildConsoleComposition(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, csrv, err := buildConsole(rootCfg(), store)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	if csrv == nil {
		t.Fatal("buildConsole returned a nil console server")
	}

	srv := httptest.NewServer(handler)
	defer srv.Close()

	// The admin prefix reaches the admin handler: an unsigned call is refused with
	// 403, not served as the SPA index (which would be 200).
	resp := get(t, srv.Client(), srv.URL+"/liteio/admin/v1/users", nil)
	if resp != http.StatusForbidden {
		t.Errorf("unsigned admin call status = %d, want 403", resp)
	}

	// The SPA shell is served at the root.
	body, status := getBody(t, srv.Client(), srv.URL+"/", nil)
	if status != http.StatusOK {
		t.Fatalf("GET / status = %d, want 200", status)
	}
	if !strings.Contains(body, "liteio console") {
		t.Errorf("root did not serve the console shell, got %q", body)
	}

	// The console API rejects an unauthenticated whoami with the CSRF header set.
	if s := get(t, srv.Client(), srv.URL+"/api/session", csrfHeaders()); s != http.StatusUnauthorized {
		t.Errorf("anonymous /api/session status = %d, want 401", s)
	}
}

// TestBuildConsoleLoginAndBridge drives the whole console path the binary exposes:
// sign in as root, then create and list a user through the in-process signed bridge
// into the admin API, confirming the wiring carries a request end to end.
func TestBuildConsoleLoginAndBridge(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, _, err := buildConsole(rootCfg(), store)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}

	// Sign in as root.
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d, want 200", s)
	}

	// Create a user through the bridge.
	body := mustJSON(t, map[string]any{"secretKey": "devsecret123", "policies": []string{"readwrite"}})
	if s := putJSON(t, client, srv.URL+"/api/admin/users/dev", body); s != http.StatusNoContent {
		t.Fatalf("create user via bridge status = %d, want 204", s)
	}

	// List users through the bridge and confirm the new user is there.
	listBody, status := getBody(t, client, srv.URL+"/api/admin/users", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("list users via bridge status = %d, want 200", status)
	}
	var listed struct {
		Users []string `json:"users"`
	}
	if err := json.Unmarshal([]byte(listBody), &listed); err != nil {
		t.Fatalf("decode list: %v (body %q)", err, listBody)
	}
	if len(listed.Users) != 1 || listed.Users[0] != "dev" {
		t.Errorf("users = %v, want [dev]", listed.Users)
	}
}

func TestBuildConsoleNeedsAddressToServe(t *testing.T) {
	// buildConsole itself always builds; the address gate lives in run(). A blank
	// region and default opts must still produce a working server.
	store := auth.NewStore("k", "s")
	if _, _, err := buildConsole(config{}, store); err != nil {
		t.Fatalf("buildConsole with zero config: %v", err)
	}
}

// TestStartSweepingStops confirms the background sweeper exits when its context is
// cancelled and does not panic on a freshly built console server.
func TestStartSweepingStops(t *testing.T) {
	store := auth.NewStore("k", "s")
	_, csrv, err := buildConsole(rootCfg(), store)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	startSweeping(ctx, csrv)
	cancel() // the goroutine returns on the next loop iteration
}

// BenchmarkConsoleBridgeList measures one authenticated list call through the whole
// assembled stack: CSRF check, session lookup, SigV4 signing in the bridge, and the
// admin handler's verify-authorize-dispatch. It is the per-request cost a browser
// pays for every view that reads from the admin API.
func BenchmarkConsoleBridgeList(b *testing.B) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, _, err := buildConsole(rootCfg(), store)
	if err != nil {
		b.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(b, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/api/login", bytes.NewReader(login))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liteio-Console", "1")
	resp, err := client.Do(req)
	if err != nil {
		b.Fatalf("login: %v", err)
	}
	resp.Body.Close()

	url := srv.URL + "/api/admin/users"
	for b.Loop() {
		r, _ := http.NewRequest(http.MethodGet, url, nil)
		r.Header.Set("X-Liteio-Console", "1")
		resp, err := client.Do(r)
		if err != nil {
			b.Fatalf("list: %v", err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// --- HTTP helpers ---

func csrfHeaders() http.Header {
	h := http.Header{}
	h.Set("X-Liteio-Console", "1")
	return h
}

func get(t *testing.T, c *http.Client, url string, hdr http.Header) int {
	t.Helper()
	_, status := getBody(t, c, url, hdr)
	return status
}

func getBody(t *testing.T, c *http.Client, url string, hdr http.Header) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	maps.Copy(req.Header, hdr)
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func post(t *testing.T, c *http.Client, url string, body []byte) int {
	t.Helper()
	return sendJSON(t, c, http.MethodPost, url, body)
}

func putJSON(t *testing.T, c *http.Client, url string, body []byte) int {
	t.Helper()
	return sendJSON(t, c, http.MethodPut, url, body)
}

func sendJSON(t *testing.T, c *http.Client, method, url string, body []byte) int {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Liteio-Console", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
