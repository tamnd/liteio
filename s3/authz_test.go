// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// authzHarness wires the front door with a real IAM store as the authorizer and a
// credential store that can sign for both the admin (root) key and a scoped user.
type authzHarness struct {
	t   *testing.T
	srv *httptest.Server
}

var readerCreds = auth.Credentials{AccessKey: "reader", SecretKey: "readersecret"}

func newAuthzHarness(t *testing.T) *authzHarness {
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

	// The IAM store: admin is root (allowed everything); reader is a scoped user.
	store := auth.NewStore(testCreds.AccessKey, testCreds.SecretKey)
	if err := store.AddUser(auth.User{AccessKey: readerCreds.AccessKey, SecretKey: readerCreds.SecretKey, Policies: []string{"readonly"}}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	// Signing seam: a static store with both keys, since the SigV4 verifier needs
	// the secret for each access key that signs a request.
	creds := auth.NewStaticStore(testCreds, readerCreds)

	server := NewServer(layer, creds,
		WithAuthorizer(store),
		WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &authzHarness{t: t, srv: srv}
}

// doAs signs a request with the given credentials and drains the response.
func (h *authzHarness) doAs(c auth.Credentials, method, path string, body []byte) result {
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
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

func TestAuthzScopedUserDeniedWrite(t *testing.T) {
	h := newAuthzHarness(t)
	// Admin (root) creates the bucket and an object.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b/k", []byte("hi")), http.StatusOK)

	// The readonly user may read.
	get := h.doAs(readerCreds, http.MethodGet, "/b/k", nil)
	mustStatus(t, get, http.StatusOK)
	if !bytes.Equal(get.body, []byte("hi")) {
		t.Fatalf("read body = %q", get.body)
	}

	// The readonly user may not write: AccessDenied.
	put := h.doAs(readerCreds, http.MethodPut, "/b/k2", []byte("nope"))
	mustStatus(t, put, http.StatusForbidden)
	if code := errorCode(t, put.body); code != "AccessDenied" {
		t.Fatalf("write code = %s, want AccessDenied", code)
	}

	// Nor delete.
	del := h.doAs(readerCreds, http.MethodDelete, "/b/k", nil)
	mustStatus(t, del, http.StatusForbidden)
	if code := errorCode(t, del.body); code != "AccessDenied" {
		t.Fatalf("delete code = %s, want AccessDenied", code)
	}
}

func TestAuthzBucketPolicyGrantsWrite(t *testing.T) {
	h := newAuthzHarness(t)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)

	// Without a policy the reader cannot write.
	mustStatus(t, h.doAs(readerCreds, http.MethodPut, "/b/k", []byte("x")), http.StatusForbidden)

	// Attach a bucket policy that grants the reader PutObject.
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":"reader"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b?policy", []byte(policy)), http.StatusNoContent)

	// Now the reader's write is granted by the resource policy.
	mustStatus(t, h.doAs(readerCreds, http.MethodPut, "/b/k", []byte("x")), http.StatusOK)
}

func TestAuthzAdminAllowed(t *testing.T) {
	h := newAuthzHarness(t)
	// Root is allowed every operation the front door maps.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b/k", []byte("v")), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodGet, "/b/k", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodGet, "/", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodDelete, "/b/k", nil), http.StatusNoContent)
}

func TestActionFor(t *testing.T) {
	cases := []struct {
		name       string
		method     string
		query      string
		bucket     string
		object     string
		wantAction string
		wantARN    string
	}{
		{"list buckets", http.MethodGet, "", "", "", "s3:ListAllMyBuckets", "arn:aws:s3:::*"},
		{"list objects", http.MethodGet, "", "b", "", "s3:ListBucket", "arn:aws:s3:::b"},
		{"head bucket", http.MethodHead, "", "b", "", "s3:ListBucket", "arn:aws:s3:::b"},
		{"create bucket", http.MethodPut, "", "b", "", "s3:CreateBucket", "arn:aws:s3:::b"},
		{"delete bucket", http.MethodDelete, "", "b", "", "s3:DeleteBucket", "arn:aws:s3:::b"},
		{"get policy", http.MethodGet, "policy=", "b", "", "s3:GetBucketPolicy", "arn:aws:s3:::b"},
		{"put policy", http.MethodPut, "policy=", "b", "", "s3:PutBucketPolicy", "arn:aws:s3:::b"},
		{"delete policy", http.MethodDelete, "policy=", "b", "", "s3:DeleteBucketPolicy", "arn:aws:s3:::b"},
		{"get versioning", http.MethodGet, "versioning=", "b", "", "s3:GetBucketVersioning", "arn:aws:s3:::b"},
		{"put versioning", http.MethodPut, "versioning=", "b", "", "s3:PutBucketVersioning", "arn:aws:s3:::b"},
		{"get location", http.MethodGet, "location=", "b", "", "s3:GetBucketLocation", "arn:aws:s3:::b"},
		{"list versions", http.MethodGet, "versions=", "b", "", "s3:ListBucketVersions", "arn:aws:s3:::b"},
		{"list uploads", http.MethodGet, "uploads=", "b", "", "s3:ListBucketMultipartUploads", "arn:aws:s3:::b"},
		{"bulk delete", http.MethodPost, "delete=", "b", "", "s3:DeleteObject", "arn:aws:s3:::b"},
		{"get object", http.MethodGet, "", "b", "k", "s3:GetObject", "arn:aws:s3:::b/k"},
		{"head object", http.MethodHead, "", "b", "k", "s3:GetObject", "arn:aws:s3:::b/k"},
		{"put object", http.MethodPut, "", "b", "k", "s3:PutObject", "arn:aws:s3:::b/k"},
		{"delete object", http.MethodDelete, "", "b", "k", "s3:DeleteObject", "arn:aws:s3:::b/k"},
		{"list parts", http.MethodGet, "uploadId=u", "b", "k", "s3:ListMultipartUploadParts", "arn:aws:s3:::b/k"},
		{"abort upload", http.MethodDelete, "uploadId=u", "b", "k", "s3:AbortMultipartUpload", "arn:aws:s3:::b/k"},
		{"create upload", http.MethodPost, "uploads=", "b", "k", "s3:PutObject", "arn:aws:s3:::b/k"},
		{"complete upload", http.MethodPost, "uploadId=u", "b", "k", "s3:PutObject", "arn:aws:s3:::b/k"},
	}
	s := &Server{}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u := &url.URL{Path: "/" + c.bucket + "/" + c.object, RawQuery: c.query}
			r := &http.Request{Method: c.method, URL: u}
			action, arn := s.actionFor(r, resource{bucket: c.bucket, object: c.object})
			if action != c.wantAction || arn != c.wantARN {
				t.Fatalf("actionFor = %q,%q want %q,%q", action, arn, c.wantAction, c.wantARN)
			}
		})
	}
}
