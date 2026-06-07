// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
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

var testCreds = auth.Credentials{AccessKey: "liteioadmin", SecretKey: "liteiosecret"}

// harness wires a real object layer over temp drives behind the S3 front door and
// an httptest server, plus a signing client.
type harness struct {
	t   *testing.T
	srv *httptest.Server
}

func newHarness(t *testing.T) *harness {
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
	store := auth.NewStaticStore(testCreds)
	server := NewServer(layer, store, WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}
}

// result is a fully-read response: the body is drained and closed so tests never
// leak a connection (and bodyclose stays quiet).
type result struct {
	status int
	header http.Header
	body   []byte
}

// do signs and sends a request, draining the response. body may be nil.
func (h *harness) do(method, path string, body []byte, headers map[string]string) result {
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
	sign.SignHeader(req, testCreds, "us-east-1", hash, time.Now().UTC())
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

func mustStatus(t *testing.T, res result, want int) {
	t.Helper()
	if res.status != want {
		t.Fatalf("status = %d, want %d; body=%s", res.status, want, res.body)
	}
}

func TestUnauthenticatedRejected(t *testing.T) {
	h := newHarness(t)
	resp, err := http.Get(h.srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest { // MissingSecurityHeader
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), "MissingSecurityHeader") {
		t.Fatalf("expected MissingSecurityHeader, got %s", body)
	}
}

func TestBucketLifecycle(t *testing.T) {
	h := newHarness(t)

	// Create.
	mustStatus(t, h.do(http.MethodPut, "/photos", nil, nil), http.StatusOK)

	// HEAD existing.
	mustStatus(t, h.do(http.MethodHead, "/photos", nil, nil), http.StatusOK)

	// HEAD missing.
	mustStatus(t, h.do(http.MethodHead, "/nope", nil, nil), http.StatusNotFound)

	// List buckets.
	resp := h.do(http.MethodGet, "/", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var lab listAllMyBucketsResult
	if err := xml.Unmarshal(resp.body, &lab); err != nil {
		t.Fatalf("unmarshal ListAllMyBuckets: %v", err)
	}
	if len(lab.Buckets.Bucket) != 1 || lab.Buckets.Bucket[0].Name != "photos" {
		t.Fatalf("buckets = %+v", lab.Buckets.Bucket)
	}

	// Duplicate create -> 409.
	mustStatus(t, h.do(http.MethodPut, "/photos", nil, nil), http.StatusConflict)

	// Delete.
	mustStatus(t, h.do(http.MethodDelete, "/photos", nil, nil), http.StatusNoContent)
	mustStatus(t, h.do(http.MethodHead, "/photos", nil, nil), http.StatusNotFound)
}

func TestObjectPutGetHeadDelete(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/data", nil, nil), http.StatusOK)

	payload := bytes.Repeat([]byte("liteio-"), 5000) // ~35 KiB, out-of-line
	put := h.do(http.MethodPut, "/data/big.txt", payload, map[string]string{"Content-Type": "text/plain"})
	mustStatus(t, put, http.StatusOK)
	etag := put.header.Get("ETag")
	if etag == "" || !strings.HasPrefix(etag, "\"") {
		t.Fatalf("missing quoted ETag: %q", etag)
	}

	// HEAD.
	head := h.do(http.MethodHead, "/data/big.txt", nil, nil)
	mustStatus(t, head, http.StatusOK)
	if head.header.Get("Content-Type") != "text/plain" {
		t.Fatalf("content-type = %q", head.header.Get("Content-Type"))
	}
	if head.header.Get("ETag") != etag {
		t.Fatalf("HEAD etag %q != PUT etag %q", head.header.Get("ETag"), etag)
	}

	// GET and compare bytes.
	get := h.do(http.MethodGet, "/data/big.txt", nil, nil)
	mustStatus(t, get, http.StatusOK)
	if got := get.body; !bytes.Equal(got, payload) {
		t.Fatalf("GET body mismatch: %d vs %d bytes", len(got), len(payload))
	}

	// GET missing.
	mustStatus(t, h.do(http.MethodGet, "/data/missing.txt", nil, nil), http.StatusNotFound)

	// DELETE.
	mustStatus(t, h.do(http.MethodDelete, "/data/big.txt", nil, nil), http.StatusNoContent)
	mustStatus(t, h.do(http.MethodGet, "/data/big.txt", nil, nil), http.StatusNotFound)
}

func TestPutToMissingBucket(t *testing.T) {
	h := newHarness(t)
	resp := h.do(http.MethodPut, "/ghost/key", []byte("x"), nil)
	mustStatus(t, resp, http.StatusNotFound)
	if !strings.Contains(string(resp.body), "NoSuchBucket") {
		t.Fatal("expected NoSuchBucket")
	}
}

func TestListObjectsV2(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/listing", nil, nil), http.StatusOK)
	for _, key := range []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"} {
		mustStatus(t, h.do(http.MethodPut, "/listing/"+key, []byte("v"), nil), http.StatusOK)
	}

	// Flat list.
	resp := h.do(http.MethodGet, "/listing?list-type=2", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var res listBucketV2Result
	if err := xml.Unmarshal(resp.body, &res); err != nil {
		t.Fatalf("unmarshal list: %v", err)
	}
	if len(res.Contents) != 4 {
		t.Fatalf("flat list = %d keys, want 4", len(res.Contents))
	}

	// Delimited list rolls a/ and b/ into common prefixes.
	resp = h.do(http.MethodGet, "/listing?list-type=2&delimiter=/", nil, nil)
	var res2 listBucketV2Result
	if err := xml.Unmarshal(resp.body, &res2); err != nil {
		t.Fatalf("unmarshal delimited list: %v", err)
	}
	if len(res2.CommonPrefixes) != 2 {
		t.Fatalf("common prefixes = %+v, want a/ and b/", res2.CommonPrefixes)
	}
	if len(res2.Contents) != 1 || res2.Contents[0].Key != "top.txt" {
		t.Fatalf("delimited contents = %+v, want only top.txt", res2.Contents)
	}

	// Prefix scope.
	resp = h.do(http.MethodGet, "/listing?list-type=2&prefix=a/", nil, nil)
	var res3 listBucketV2Result
	if err := xml.Unmarshal(resp.body, &res3); err != nil {
		t.Fatalf("unmarshal prefix list: %v", err)
	}
	if len(res3.Contents) != 2 {
		t.Fatalf("prefix a/ = %d keys, want 2", len(res3.Contents))
	}
}

func TestDeleteObjectsBatch(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/batch", nil, nil), http.StatusOK)
	for _, k := range []string{"x", "y", "z"} {
		mustStatus(t, h.do(http.MethodPut, "/batch/"+k, []byte("v"), nil), http.StatusOK)
	}

	body, err := xml.Marshal(deleteRequest{
		Objects: []deleteRequestEntry{{Key: "x"}, {Key: "y"}, {Key: "missing"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp := h.do(http.MethodPost, "/batch?delete", body, map[string]string{"Content-Type": "application/xml"})
	mustStatus(t, resp, http.StatusOK)
	var dr deleteResult
	if err := xml.Unmarshal(resp.body, &dr); err != nil {
		t.Fatalf("unmarshal delete result: %v", err)
	}
	// Unversioned deletes of x, y, and a missing key all succeed idempotently.
	if len(dr.Deleted) != 3 {
		t.Fatalf("deleted = %d, want 3 (idempotent missing): %+v / errs %+v", len(dr.Deleted), dr.Deleted, dr.Errors)
	}
	mustStatus(t, h.do(http.MethodGet, "/batch/z", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodGet, "/batch/x", nil, nil), http.StatusNotFound)
}

func TestInlineSmallObject(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/tiny", nil, nil), http.StatusOK)
	small := []byte("hello")
	mustStatus(t, h.do(http.MethodPut, "/tiny/hi.txt", small, nil), http.StatusOK)
	get := h.do(http.MethodGet, "/tiny/hi.txt", nil, nil)
	mustStatus(t, get, http.StatusOK)
	if got := get.body; !bytes.Equal(got, small) {
		t.Fatalf("inline round-trip mismatch: %q", got)
	}
}

func TestGetBucketLocation(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/loc", nil, nil), http.StatusOK)
	resp := h.do(http.MethodGet, "/loc?location=", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var lc locationConstraint
	if err := xml.Unmarshal(resp.body, &lc); err != nil {
		t.Fatalf("unmarshal location: %v", err)
	}
}
