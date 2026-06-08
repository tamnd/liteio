// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

var (
	adminCreds  = auth.Credentials{AccessKey: "admin", SecretKey: "adminsecret"}   // root, allowed everything
	opsCreds    = auth.Credentials{AccessKey: "ops", SecretKey: "opssecret"}       // consoleAdmin user
	viewerCreds = auth.Credentials{AccessKey: "viewer", SecretKey: "viewersecret"} // readonly, no admin rights
)

type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	if err := store.AddUser(auth.User{AccessKey: opsCreds.AccessKey, SecretKey: opsCreds.SecretKey, Policies: []string{"consoleAdmin"}}); err != nil {
		t.Fatalf("add ops: %v", err)
	}
	if err := store.AddUser(auth.User{AccessKey: viewerCreds.AccessKey, SecretKey: viewerCreds.SecretKey, Policies: []string{"readonly"}}); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	creds := auth.NewStaticStore(adminCreds, opsCreds, viewerCreds)
	server := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}
}

type result struct {
	status int
	body   []byte
}

// do signs a request with the given credentials and drains the response.
func (h *harness) do(c auth.Credentials, method, path string, body []byte) result {
	h.t.Helper()
	var rdr io.Reader
	hash := sign.EmptyPayloadHash
	if body != nil {
		rdr = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	sign.SignHeader(req, c, "us-east-1", hash, time.Now().UTC())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, body: rb}
}

// admin signs as the root admin, the common case for the lifecycle tests.
func (h *harness) admin(method, path string, body []byte) result {
	return h.do(adminCreds, method, path, body)
}

func mustStatus(t *testing.T, res result, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("status = %d, want %d; body=%s", res.status, want, res.body)
	}
}

func decode[T any](t *testing.T, res result) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(res.body, &v); err != nil {
		t.Fatalf("decode: %v; body=%s", err, res.body)
	}
	return v
}

func errCode(t *testing.T, res result) string {
	t.Helper()
	return decode[errorResponse](t, res).Code
}

func TestUserLifecycle(t *testing.T) {
	h := newHarness(t)

	// Create.
	body := []byte(`{"secretKey":"s3cret","policies":["readonly"]}`)
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/users/alice", body), http.StatusNoContent)

	// List includes the new user (alongside the seeded ops/viewer).
	list := decode[map[string][]string](t, h.admin(http.MethodGet, "/liteio/admin/v1/users", nil))
	if !slices.Contains(list["users"], "alice") {
		t.Fatalf("users = %v, want alice present", list["users"])
	}

	// Get shows policies, never the secret.
	info := decode[userInfo](t, h.admin(http.MethodGet, "/liteio/admin/v1/users/alice", nil))
	if info.AccessKey != "alice" || !slices.Equal(info.Policies, []string{"readonly"}) {
		t.Fatalf("user info = %+v", info)
	}

	// Attach a second policy, then confirm it shows up.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/users/alice/policies/readwrite", nil), http.StatusNoContent)
	info = decode[userInfo](t, h.admin(http.MethodGet, "/liteio/admin/v1/users/alice", nil))
	if !slices.Contains(info.Policies, "readwrite") {
		t.Fatalf("after attach, policies = %v", info.Policies)
	}

	// Detach it again.
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/users/alice/policies/readwrite", nil), http.StatusNoContent)

	// Delete the user; a subsequent GET is 404.
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/users/alice", nil), http.StatusNoContent)
	mustStatus(t, h.admin(http.MethodGet, "/liteio/admin/v1/users/alice", nil), http.StatusNotFound)
}

func TestCreateUserErrors(t *testing.T) {
	h := newHarness(t)
	// Missing secret.
	res := h.admin(http.MethodPut, "/liteio/admin/v1/users/bob", []byte(`{"policies":["readonly"]}`))
	mustStatus(t, res, http.StatusBadRequest)
	if errCode(t, res) != "MalformedRequest" {
		t.Fatalf("code = %s", errCode(t, res))
	}
	// Unknown policy.
	res = h.admin(http.MethodPut, "/liteio/admin/v1/users/bob", []byte(`{"secretKey":"s","policies":["nope"]}`))
	mustStatus(t, res, http.StatusBadRequest)
	if errCode(t, res) != "UnknownPolicy" {
		t.Fatalf("code = %s", errCode(t, res))
	}
	// Duplicate.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/users/bob", []byte(`{"secretKey":"s"}`)), http.StatusNoContent)
	res = h.admin(http.MethodPut, "/liteio/admin/v1/users/bob", []byte(`{"secretKey":"s"}`))
	mustStatus(t, res, http.StatusConflict)
	if errCode(t, res) != "AlreadyExists" {
		t.Fatalf("code = %s", errCode(t, res))
	}
	// Unknown field rejected by the strict decoder.
	res = h.admin(http.MethodPut, "/liteio/admin/v1/users/carol", []byte(`{"secretKey":"s","bogus":1}`))
	mustStatus(t, res, http.StatusBadRequest)
}

func TestGroupLifecycle(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/users/alice", []byte(`{"secretKey":"s"}`)), http.StatusNoContent)
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/groups/ops", []byte(`{"policies":["readwrite"]}`)), http.StatusNoContent)

	// Add alice to the group.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/groups/ops/members/alice", nil), http.StatusNoContent)
	info := decode[userInfo](t, h.admin(http.MethodGet, "/liteio/admin/v1/users/alice", nil))
	if !slices.Contains(info.Groups, "ops") {
		t.Fatalf("alice groups = %v, want ops", info.Groups)
	}

	g := decode[groupInfo](t, h.admin(http.MethodGet, "/liteio/admin/v1/groups/ops", nil))
	if !slices.Equal(g.Policies, []string{"readwrite"}) {
		t.Fatalf("group = %+v", g)
	}
	groups := decode[map[string][]string](t, h.admin(http.MethodGet, "/liteio/admin/v1/groups", nil))
	if !slices.Contains(groups["groups"], "ops") {
		t.Fatalf("groups = %v", groups["groups"])
	}

	// Remove the member, then delete the empty group.
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/groups/ops/members/alice", nil), http.StatusNoContent)
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/groups/ops", nil), http.StatusNoContent)
	mustStatus(t, h.admin(http.MethodGet, "/liteio/admin/v1/groups/ops", nil), http.StatusNotFound)
}

func TestPolicyLifecycle(t *testing.T) {
	h := newHarness(t)
	doc := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`

	// Create.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/policies/mine", []byte(doc)), http.StatusNoContent)

	// Read it back: it parses as a policy with one statement.
	got := decode[auth.Policy](t, h.admin(http.MethodGet, "/liteio/admin/v1/policies/mine", nil))
	if got.Version != "2012-10-17" || len(got.Statements) != 1 {
		t.Fatalf("policy = %+v", got)
	}

	// List contains both the custom and the canned names.
	names := decode[map[string][]string](t, h.admin(http.MethodGet, "/liteio/admin/v1/policies", nil))
	if !slices.Contains(names["policies"], "mine") || !slices.Contains(names["policies"], "readonly") {
		t.Fatalf("policies = %v", names["policies"])
	}

	// Malformed document is rejected.
	res := h.admin(http.MethodPut, "/liteio/admin/v1/policies/bad", []byte(`not a policy`))
	mustStatus(t, res, http.StatusBadRequest)
	if errCode(t, res) != "MalformedPolicy" {
		t.Fatalf("code = %s", errCode(t, res))
	}

	// A built-in cannot be redefined or deleted.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/policies/readonly", []byte(doc)), http.StatusConflict)
	res = h.admin(http.MethodDelete, "/liteio/admin/v1/policies/readonly", nil)
	mustStatus(t, res, http.StatusBadRequest)
	if errCode(t, res) != "InvalidOperation" {
		t.Fatalf("delete-canned code = %s", errCode(t, res))
	}

	// Delete the custom one.
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/policies/mine", nil), http.StatusNoContent)
	mustStatus(t, h.admin(http.MethodGet, "/liteio/admin/v1/policies/mine", nil), http.StatusNotFound)
}

func TestServiceAccountLifecycle(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/users/alice", []byte(`{"secretKey":"s","policies":["readwrite"]}`)), http.StatusNoContent)

	// Create a service account under alice with a narrowing inline policy.
	body := `{"accessKey":"app","secretKey":"appsecret","parentUser":"alice","policy":{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}}`
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/service-accounts", []byte(body)), http.StatusNoContent)

	list := decode[map[string][]string](t, h.admin(http.MethodGet, "/liteio/admin/v1/service-accounts?user=alice", nil))
	if !slices.Equal(list["serviceAccounts"], []string{"app"}) {
		t.Fatalf("service accounts = %v", list["serviceAccounts"])
	}

	// Listing without the user parameter is a 400.
	mustStatus(t, h.admin(http.MethodGet, "/liteio/admin/v1/service-accounts", nil), http.StatusBadRequest)

	// Missing required fields is a 400.
	mustStatus(t, h.admin(http.MethodPut, "/liteio/admin/v1/service-accounts", []byte(`{"accessKey":"x"}`)), http.StatusBadRequest)

	// Delete it.
	mustStatus(t, h.admin(http.MethodDelete, "/liteio/admin/v1/service-accounts/app", nil), http.StatusNoContent)
}

func TestAuthorizationGating(t *testing.T) {
	h := newHarness(t)

	// A consoleAdmin user (not root) may manage IAM.
	mustStatus(t, h.do(opsCreds, http.MethodPut, "/liteio/admin/v1/users/alice", []byte(`{"secretKey":"s"}`)), http.StatusNoContent)

	// A readonly user may not: the admin: action is denied.
	res := h.do(viewerCreds, http.MethodPut, "/liteio/admin/v1/users/bob", []byte(`{"secretKey":"s"}`))
	mustStatus(t, res, http.StatusForbidden)
	if errCode(t, res) != "AccessDenied" {
		t.Fatalf("code = %s, want AccessDenied", errCode(t, res))
	}
	// A read is also denied for the viewer (no admin:ListUsers).
	mustStatus(t, h.do(viewerCreds, http.MethodGet, "/liteio/admin/v1/users", nil), http.StatusForbidden)
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/liteio/admin/v1/users")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", resp.StatusCode)
	}
}

func TestUnknownRouteNotFound(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.admin(http.MethodGet, "/liteio/admin/v1/nope", nil), http.StatusNotFound)
}

// BenchmarkGetUser measures a full signed request: SigV4 verification, the admin
// action authorization, the mux dispatch, and the handler. It runs the server
// in-process (no httptest socket) to keep the measurement on the server's own work.
func BenchmarkGetUser(b *testing.B) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	if err := store.AddUser(auth.User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		b.Fatal(err)
	}
	now := time.Now().UTC()
	server := NewServer(store, auth.NewStaticStore(adminCreds), WithClock(func() time.Time { return now }))

	newSigned := func() *http.Request {
		req, err := http.NewRequest(http.MethodGet, "http://admin.local/liteio/admin/v1/users/alice", nil)
		if err != nil {
			b.Fatal(err)
		}
		sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, now)
		return req
	}

	b.ReportAllocs()
	for b.Loop() {
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, newSigned())
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}
