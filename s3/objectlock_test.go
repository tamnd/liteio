// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// bypassCreds is a second user that has s3:BypassGovernanceRetention via a
// custom policy.  It does NOT have s3:DeleteObject on its own; the bypass
// action check happens at the handler level and is distinct from the normal
// DeleteObject gate.  For the tests below both users need DeleteObject, so we
// give bypassUser the "readwrite" canned policy (s3:*) which covers both.
var bypassCreds = auth.Credentials{AccessKey: "bypassuser", SecretKey: "bypasssecret"}

// nopBypassCreds is a user that can delete objects but does NOT hold
// s3:BypassGovernanceRetention.
var nopBypassCreds = auth.Credentials{AccessKey: "nopbypass", SecretKey: "nopbypasssecret"}

// lockHarness is an authz-aware test harness for Object Lock / bypass tests.
type lockHarness struct {
	t     *testing.T
	srv   *httptest.Server
	layer object.ObjectLayer
}

func newLockHarness(t *testing.T) *lockHarness {
	t.Helper()
	const n, parity = 6, 2
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	layer, err := object.NewSingleSet([16]byte{1, 2, 3}, drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}

	// Root is testCreds; bypassUser gets readwrite (s3:*), nopBypassUser gets a
	// policy with DeleteObject but NOT BypassGovernanceRetention.
	store := auth.NewStore(testCreds.AccessKey, testCreds.SecretKey)

	if err := store.AddUser(auth.User{
		AccessKey: bypassCreds.AccessKey,
		SecretKey: bypassCreds.SecretKey,
		Policies:  []string{"readwrite"},
	}); err != nil {
		t.Fatalf("AddUser bypassUser: %v", err)
	}

	deleteOnlyPolicyName := "deleteonly"
	deleteOnly, parseErr := auth.ParsePolicy([]byte(`{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": ["s3:DeleteObject", "s3:PutObject", "s3:GetObject", "s3:ListBucket",
			           "s3:CreateBucket", "s3:PutBucketVersioning"],
			"Resource": ["arn:aws:s3:::*"]
		}]
	}`))
	if parseErr != nil {
		t.Fatalf("ParsePolicy deleteonly: %v", parseErr)
	}
	if err := store.AddPolicy(deleteOnlyPolicyName, deleteOnly); err != nil {
		t.Fatalf("AddPolicy deleteonly: %v", err)
	}
	if err := store.AddUser(auth.User{
		AccessKey: nopBypassCreds.AccessKey,
		SecretKey: nopBypassCreds.SecretKey,
		Policies:  []string{deleteOnlyPolicyName},
	}); err != nil {
		t.Fatalf("AddUser nopBypassUser: %v", err)
	}

	creds := auth.NewStaticStore(testCreds, bypassCreds, nopBypassCreds)
	server := NewServer(layer, creds,
		WithAuthorizer(store),
		WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &lockHarness{t: t, srv: srv, layer: layer}
}

// doAsWithHeaders signs a request with the given credentials and extra headers.
func (h *lockHarness) doAsWithHeaders(c auth.Credentials, method, path string, body []byte, headers map[string]string) result {
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
	for k, v := range headers {
		req.Header.Set(k, v)
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
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

// doAs signs a request without extra headers.
func (h *lockHarness) doAs(c auth.Credentials, method, path string, body []byte) result {
	return h.doAsWithHeaders(c, method, path, body, nil)
}

// enableVersioningLH turns on versioning for a bucket, returning on success.
func (h *lockHarness) enableVersioningLH(bucket string) {
	h.t.Helper()
	body, err := xml.Marshal(versioningConfiguration{Status: "Enabled"})
	if err != nil {
		h.t.Fatalf("marshal versioning config: %v", err)
	}
	mustStatus(h.t, h.doAs(testCreds, http.MethodPut, "/"+bucket+"?versioning", body), http.StatusOK)
}

// setRetentionViaLayer applies GOVERNANCE retention directly through the
// object layer (bypasses the S3 API, so no need to also route through it in
// the test setup).
func (h *lockHarness) setRetentionViaLayer(bucket, key, versionID, mode string, future time.Time) {
	h.t.Helper()
	until := future.UTC().Format(time.RFC3339Nano)
	if err := h.layer.SetObjectRetention(context.Background(), bucket, key, versionID, mode, until); err != nil {
		h.t.Fatalf("SetObjectRetention %s/%s: %v", key, versionID, err)
	}
}

// mustPutObject puts an object via the S3 API as testCreds, returning its versionId.
func (h *lockHarness) mustPutObject(bucket, key string, body []byte) string {
	h.t.Helper()
	r := h.doAs(testCreds, http.MethodPut, "/"+bucket+"/"+key, body)
	mustStatus(h.t, r, http.StatusOK)
	return r.header.Get("x-amz-version-id")
}

// TestBypassGovernanceHeaderPassesThrough confirms that a caller with
// s3:BypassGovernanceRetention (via the readwrite canned policy) can delete a
// GOVERNANCE-locked version by sending x-amz-bypass-governance-retention: true.
func TestBypassGovernanceHeaderPassesThrough(t *testing.T) {
	h := newLockHarness(t)

	// Setup: bucket + versioning.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/bkt", nil), http.StatusOK)
	h.enableVersioningLH("bkt")

	versionID := h.mustPutObject("bkt", "obj", []byte("data"))

	// Lock the version under GOVERNANCE for 24 h.
	h.setRetentionViaLayer("bkt", "obj", versionID, object.LockModeGov, time.Now().UTC().Add(24*time.Hour))

	// A regular delete (no bypass header) must fail with AccessForbidden/Locked.
	del := h.doAs(bypassCreds, http.MethodDelete, "/bkt/obj?versionId="+versionID, nil)
	if del.status == http.StatusNoContent {
		t.Fatal("expected delete to fail under GOVERNANCE lock, but got 204")
	}

	// Delete WITH the bypass header from a caller who holds BypassGovernanceRetention.
	bypassDel := h.doAsWithHeaders(bypassCreds, http.MethodDelete,
		"/bkt/obj?versionId="+versionID,
		nil,
		map[string]string{"x-amz-bypass-governance-retention": "true"})
	mustStatus(t, bypassDel, http.StatusNoContent)
}

// TestBypassHeaderWithoutPermissionDenied confirms that presenting the bypass
// header without holding s3:BypassGovernanceRetention returns AccessDenied.
func TestBypassHeaderWithoutPermissionDenied(t *testing.T) {
	h := newLockHarness(t)

	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/bkt", nil), http.StatusOK)
	h.enableVersioningLH("bkt")

	versionID := h.mustPutObject("bkt", "obj", []byte("data"))

	// Lock under GOVERNANCE.
	h.setRetentionViaLayer("bkt", "obj", versionID, object.LockModeGov, time.Now().UTC().Add(24*time.Hour))

	// nopBypassCreds can delete objects but lacks BypassGovernanceRetention.
	del := h.doAsWithHeaders(nopBypassCreds, http.MethodDelete,
		"/bkt/obj?versionId="+versionID,
		nil,
		map[string]string{"x-amz-bypass-governance-retention": "true"})
	mustStatus(t, del, http.StatusForbidden)
	if code := errorCode(t, del.body); !strings.Contains(code, "AccessDenied") {
		t.Fatalf("expected AccessDenied, got code %q; body=%s", code, del.body)
	}
}
