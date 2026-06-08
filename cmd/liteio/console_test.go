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
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
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
	handler, csrv, err := buildConsole(rootCfg(), store, nil, nil)
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
	handler, _, err := buildConsole(rootCfg(), store, nil, nil)
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

// TestBuildConsoleInfoThroughBridge confirms the admin info endpoint is wired when a
// real object layer is passed, and that it is reachable through the console bridge:
// the dashboard's landing call returns the live topology.
func TestBuildConsoleInfoThroughBridge(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	layer := newDriveLayer(t, 4, 2)
	handler, _, err := buildConsole(rootCfg(), store, layer, nil)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}

	body, status := getBody(t, client, srv.URL+"/api/admin/info", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("info via bridge status = %d, want 200; body %q", status, body)
	}
	var info struct {
		DriveCount   int `json:"driveCount"`
		OnlineDrives int `json:"onlineDriveCount"`
		SetCount     int `json:"setCount"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("decode info: %v (%q)", err, body)
	}
	if info.DriveCount != 4 || info.OnlineDrives != 4 || info.SetCount != 1 {
		t.Errorf("info = %+v, want 4 drives all online in 1 set", info)
	}
}

// TestBuildConsoleS3Browse drives the bucket browser end to end: sign in as root,
// create a bucket through the S3 bridge, then list buckets and confirm it is there.
// It proves the console's signed in-process path reaches the S3 data plane, not just
// the admin API.
func TestBuildConsoleS3Browse(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	layer := newDriveLayer(t, 4, 2)
	s3Handler := s3.NewServer(layer, store)
	handler, _, err := buildConsole(rootCfg(), store, layer, s3Handler)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}

	// Create a bucket through the S3 bridge (PUT /api/s3/<bucket>).
	if s := sendJSON(t, client, http.MethodPut, srv.URL+"/api/s3/photos", nil); s != http.StatusOK {
		t.Fatalf("create bucket via S3 bridge status = %d, want 200", s)
	}

	// List buckets through the bridge and confirm the new bucket is in the XML.
	body, status := getBody(t, client, srv.URL+"/api/s3/", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("list buckets via bridge status = %d, want 200; body %q", status, body)
	}
	if !strings.Contains(body, "<Name>photos</Name>") {
		t.Errorf("ListBuckets did not include the new bucket, got %q", body)
	}
}

// TestBuildConsoleS3ObjectRoundTrip drives the object actions the browser exposes
// over the S3 bridge: sign in, create a bucket, upload a small object, download it
// back byte-for-byte, delete it, and confirm it is gone. It proves the buffered
// signed bridge carries a PUT body and a GET response body, not just empty calls.
func TestBuildConsoleS3ObjectRoundTrip(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	layer := newDriveLayer(t, 4, 2)
	s3Handler := s3.NewServer(layer, store)
	handler, _, err := buildConsole(rootCfg(), store, layer, s3Handler)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}

	if s := sendJSON(t, client, http.MethodPut, srv.URL+"/api/s3/photos", nil); s != http.StatusOK {
		t.Fatalf("create bucket status = %d, want 200", s)
	}

	// Upload a small object through the bridge.
	payload := []byte("meow, the cat says")
	if _, s := sendRaw(t, client, http.MethodPut, srv.URL+"/api/s3/photos/cat.txt", payload); s != http.StatusOK {
		t.Fatalf("upload object status = %d, want 200", s)
	}

	// Download it and confirm the bytes round-trip.
	body, status := sendRaw(t, client, http.MethodGet, srv.URL+"/api/s3/photos/cat.txt", nil)
	if status != http.StatusOK {
		t.Fatalf("download status = %d, want 200", status)
	}
	if body != string(payload) {
		t.Errorf("downloaded body = %q, want %q", body, payload)
	}

	// Delete it, then confirm a fetch is a 404.
	if _, s := sendRaw(t, client, http.MethodDelete, srv.URL+"/api/s3/photos/cat.txt", nil); s != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", s)
	}
	if _, s := sendRaw(t, client, http.MethodGet, srv.URL+"/api/s3/photos/cat.txt", nil); s != http.StatusNotFound {
		t.Errorf("get after delete status = %d, want 404", s)
	}
}

// TestBuildConsoleStreamingUpload proves the S3 bridge streams an upload rather than
// buffering it: it PUTs an object several times larger than the buffered body cap and
// reads it back byte-for-byte. A buffered bridge would refuse the body with a 413, so
// a clean round-trip is the evidence the streaming path carries the upload.
func TestBuildConsoleStreamingUpload(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	layer := newDriveLayer(t, 4, 2)
	s3Handler := s3.NewServer(layer, store)
	handler, _, err := buildConsole(rootCfg(), store, layer, s3Handler)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}
	if s := sendJSON(t, client, http.MethodPut, srv.URL+"/api/s3/backups", nil); s != http.StatusOK {
		t.Fatalf("create bucket status = %d, want 200", s)
	}

	// A body well past the buffered cap, with a recognizable pattern so a truncated or
	// reordered read shows up in the comparison rather than only in the length.
	payload := bytes.Repeat([]byte("liteio-streaming-upload-"), 200000) // ~4.8 MiB
	const bufferedCap = 1 << 20                                         // console.maxBridgeBody, the cap the streaming path lifts
	if len(payload) <= bufferedCap {
		t.Fatalf("payload %d bytes does not exceed the buffered cap %d", len(payload), bufferedCap)
	}
	if _, s := sendRaw(t, client, http.MethodPut, srv.URL+"/api/s3/backups/archive.bin", payload); s != http.StatusOK {
		t.Fatalf("streaming upload status = %d, want 200", s)
	}

	body, status := sendRaw(t, client, http.MethodGet, srv.URL+"/api/s3/backups/archive.bin", nil)
	if status != http.StatusOK {
		t.Fatalf("download status = %d, want 200", status)
	}
	if body != string(payload) {
		t.Errorf("downloaded %d bytes, want %d; round-trip mismatch", len(body), len(payload))
	}
}

// TestBuildConsoleS3BridgeAbsentWithoutHandler confirms the S3 bridge route is not
// registered when no S3 handler is wired: the call falls through to the SPA shell
// (200 HTML) rather than reaching a nil handler.
func TestBuildConsoleS3BridgeAbsentWithoutHandler(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, _, err := buildConsole(rootCfg(), store, nil, nil)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}
	// With no S3 handler the /api/s3/ path is not a registered bridge route, so the
	// SPA fallback serves index.html.
	body, status := getBody(t, client, srv.URL+"/api/s3/", csrfHeaders())
	if status != http.StatusOK || !strings.Contains(body, "liteio console") {
		t.Errorf("without S3 handler, /api/s3/ status = %d body = %q, want the SPA shell", status, body)
	}
}

// TestBuildConsoleIdentityManagement drives the IAM screens' admin-bridge flow the
// SPA depends on: create a custom policy, create a user carrying it, list users,
// attach a second policy and detach it, read the user back, then delete the user.
func TestBuildConsoleIdentityManagement(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, _, err := buildConsole(rootCfg(), store, nil, nil)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}

	// Create a custom policy.
	policyDoc := mustJSON(t, map[string]any{
		"Version": "2012-10-17",
		"Statement": []map[string]any{
			{"Effect": "Allow", "Action": []string{"s3:GetObject"}, "Resource": []string{"arn:aws:s3:::*"}},
		},
	})
	if s := putJSON(t, client, srv.URL+"/api/admin/policies/reader", policyDoc); s != http.StatusNoContent {
		t.Fatalf("create policy status = %d, want 204", s)
	}

	// Create a user carrying that policy.
	userBody := mustJSON(t, map[string]any{"secretKey": "devsecret123", "policies": []string{"reader"}})
	if s := putJSON(t, client, srv.URL+"/api/admin/users/dev", userBody); s != http.StatusNoContent {
		t.Fatalf("create user status = %d, want 204", s)
	}

	// Attach a second (canned) policy, then detach it.
	attach := srv.URL + "/api/admin/users/dev/policies/readwrite"
	if s := sendJSON(t, client, http.MethodPut, attach, nil); s != http.StatusNoContent {
		t.Fatalf("attach policy status = %d, want 204", s)
	}
	if s := sendJSON(t, client, http.MethodDelete, attach, nil); s != http.StatusNoContent {
		t.Fatalf("detach policy status = %d, want 204", s)
	}

	// Read the user back and confirm it carries exactly the custom policy.
	body, status := getBody(t, client, srv.URL+"/api/admin/users/dev", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("get user status = %d; body %q", status, body)
	}
	var u struct {
		AccessKey string   `json:"accessKey"`
		Policies  []string `json:"policies"`
	}
	if err := json.Unmarshal([]byte(body), &u); err != nil {
		t.Fatalf("decode user: %v (%q)", err, body)
	}
	if u.AccessKey != "dev" || len(u.Policies) != 1 || u.Policies[0] != "reader" {
		t.Errorf("user = %+v, want dev with [reader]", u)
	}

	// Delete the user.
	if s := sendJSON(t, client, http.MethodDelete, srv.URL+"/api/admin/users/dev", nil); s != http.StatusNoContent {
		t.Fatalf("delete user status = %d, want 204", s)
	}
}

// TestBuildConsoleGroupsAndServiceAccounts drives the rest of the IAM screens' flow:
// create a group, add and remove a member (confirming the group GET reports members),
// and create, list, and delete a service account under a parent user.
func TestBuildConsoleGroupsAndServiceAccounts(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	handler, _, err := buildConsole(rootCfg(), store, nil, nil)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	jar, _ := cookiejar.New(nil)
	client := &http.Client{Jar: jar}
	login := mustJSON(t, map[string]string{"accessKey": "liteioadmin", "secretKey": "liteioadmin"})
	if s := post(t, client, srv.URL+"/api/login", login); s != http.StatusOK {
		t.Fatalf("login status = %d", s)
	}

	// A user to be a member and a service-account parent.
	if s := putJSON(t, client, srv.URL+"/api/admin/users/alice", mustJSON(t, map[string]any{"secretKey": "alicesecret1"})); s != http.StatusNoContent {
		t.Fatalf("create user status = %d", s)
	}

	// Create a group and add alice to it.
	if s := putJSON(t, client, srv.URL+"/api/admin/groups/ops", mustJSON(t, map[string]any{"policies": []string{"readwrite"}})); s != http.StatusNoContent {
		t.Fatalf("create group status = %d", s)
	}
	if s := sendJSON(t, client, http.MethodPut, srv.URL+"/api/admin/groups/ops/members/alice", nil); s != http.StatusNoContent {
		t.Fatalf("add member status = %d", s)
	}

	// The group GET reports the member the console renders.
	body, status := getBody(t, client, srv.URL+"/api/admin/groups/ops", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("get group status = %d; body %q", status, body)
	}
	var g struct {
		Name    string   `json:"name"`
		Members []string `json:"members"`
	}
	if err := json.Unmarshal([]byte(body), &g); err != nil {
		t.Fatalf("decode group: %v (%q)", err, body)
	}
	if len(g.Members) != 1 || g.Members[0] != "alice" {
		t.Errorf("group members = %v, want [alice]", g.Members)
	}

	// Remove the member.
	if s := sendJSON(t, client, http.MethodDelete, srv.URL+"/api/admin/groups/ops/members/alice", nil); s != http.StatusNoContent {
		t.Fatalf("remove member status = %d", s)
	}

	// Create a service account under alice, list it, then delete it.
	saBody := mustJSON(t, map[string]any{"accessKey": "alice-svc", "secretKey": "svcsecret123", "parentUser": "alice"})
	if s := putJSON(t, client, srv.URL+"/api/admin/service-accounts", saBody); s != http.StatusNoContent {
		t.Fatalf("create service account status = %d", s)
	}
	listBody, status := getBody(t, client, srv.URL+"/api/admin/service-accounts?user=alice", csrfHeaders())
	if status != http.StatusOK {
		t.Fatalf("list service accounts status = %d; body %q", status, listBody)
	}
	if !strings.Contains(listBody, "alice-svc") {
		t.Errorf("service account list missing the new account, got %q", listBody)
	}
	if s := sendJSON(t, client, http.MethodDelete, srv.URL+"/api/admin/service-accounts/alice-svc", nil); s != http.StatusNoContent {
		t.Fatalf("delete service account status = %d", s)
	}
}

// newDriveLayer builds a single-set object layer over n local drives in temp dirs.
func newDriveLayer(t *testing.T, n, parity int) object.ObjectLayer {
	t.Helper()
	drives := make([]storage.StorageAPI, n)
	for i := range n {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	layer, err := object.NewSingleSet(deploymentSalt("test"), drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}
	return layer
}

func TestBuildConsoleNeedsAddressToServe(t *testing.T) {
	// buildConsole itself always builds; the address gate lives in run(). A blank
	// region and default opts must still produce a working server.
	store := auth.NewStore("k", "s")
	if _, _, err := buildConsole(config{}, store, nil, nil); err != nil {
		t.Fatalf("buildConsole with zero config: %v", err)
	}
}

// TestStartSweepingStops confirms the background sweeper exits when its context is
// cancelled and does not panic on a freshly built console server.
func TestStartSweepingStops(t *testing.T) {
	store := auth.NewStore("k", "s")
	_, csrv, err := buildConsole(rootCfg(), store, nil, nil)
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
	handler, _, err := buildConsole(rootCfg(), store, nil, nil)
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

// sendRaw sends a request with a raw body (no JSON content type, as the S3 bridge
// expects) and returns the response body and status. A nil body is a bodyless call.
func sendRaw(t *testing.T, c *http.Client, method, url string, body []byte) (string, int) {
	t.Helper()
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, url, r)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("X-Liteio-Console", "1")
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return string(b), resp.StatusCode
}

func mustJSON(t testing.TB, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
