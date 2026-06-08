// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// errorCode extracts the S3 error Code from a response body, failing if absent.
func errorCode(t *testing.T, body []byte) string {
	t.Helper()
	var e errorResponse
	if err := xml.Unmarshal(body, &e); err != nil {
		t.Fatalf("decode error response: %v; body=%s", err, body)
	}
	return e.Code
}

// validPolicy is a well-formed bucket policy with a Principal, as the front door
// requires.
const validPolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::p/*"}]}`

func TestS3BucketPolicyLifecycle(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/p", nil, nil), http.StatusOK)

	// No policy yet.
	res := h.do(http.MethodGet, "/p?policy", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
	if code := errorCode(t, res.body); code != "NoSuchBucketPolicy" {
		t.Fatalf("GET no-policy code = %s, want NoSuchBucketPolicy", code)
	}

	// Put a policy.
	mustStatus(t, h.do(http.MethodPut, "/p?policy", []byte(validPolicy), nil), http.StatusNoContent)

	// Read it back byte-for-byte, as JSON.
	got := h.do(http.MethodGet, "/p?policy", nil, nil)
	mustStatus(t, got, http.StatusOK)
	if !bytes.Equal(got.body, []byte(validPolicy)) {
		t.Fatalf("policy round-trip mismatch:\n got %s\nwant %s", got.body, validPolicy)
	}
	if ct := got.header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", ct)
	}

	// Delete it; subsequent GET is NoSuchBucketPolicy again.
	mustStatus(t, h.do(http.MethodDelete, "/p?policy", nil, nil), http.StatusNoContent)
	after := h.do(http.MethodGet, "/p?policy", nil, nil)
	mustStatus(t, after, http.StatusNotFound)

	// Delete is idempotent.
	mustStatus(t, h.do(http.MethodDelete, "/p?policy", nil, nil), http.StatusNoContent)
}

func TestS3PutBucketPolicyRejectsMalformed(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/p", nil, nil), http.StatusOK)

	cases := []struct {
		name string
		doc  string
	}{
		{"not json", `not json at all`},
		{"missing principal", `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::p/*"}]}`},
		{"bad version", `{"Version":"1999-01-01","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::p/*"}]}`},
		{"empty statements", `{"Version":"2012-10-17","Statement":[]}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := h.do(http.MethodPut, "/p?policy", []byte(c.doc), nil)
			mustStatus(t, res, http.StatusBadRequest)
			if code := errorCode(t, res.body); code != "MalformedPolicy" {
				t.Fatalf("code = %s, want MalformedPolicy", code)
			}
		})
	}

	// A rejected policy is never stored: GET still reports no policy.
	res := h.do(http.MethodGet, "/p?policy", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

func TestS3PutBucketPolicyOversize(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/p", nil, nil), http.StatusOK)

	// A document past the 20 KB cap is rejected before parsing.
	big := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::p/` +
		strings.Repeat("a", 21*1024) + `"}]}`
	res := h.do(http.MethodPut, "/p?policy", []byte(big), nil)
	mustStatus(t, res, http.StatusBadRequest)
	if code := errorCode(t, res.body); code != "MalformedPolicy" {
		t.Fatalf("oversize code = %s, want MalformedPolicy", code)
	}
}

func TestS3BucketPolicyMissingBucket(t *testing.T) {
	h := newHarness(t)

	put := h.do(http.MethodPut, "/ghost?policy", []byte(validPolicy), nil)
	mustStatus(t, put, http.StatusNotFound)
	if code := errorCode(t, put.body); code != "NoSuchBucket" {
		t.Fatalf("PUT missing-bucket code = %s, want NoSuchBucket", code)
	}

	get := h.do(http.MethodGet, "/ghost?policy", nil, nil)
	mustStatus(t, get, http.StatusNotFound)
	if code := errorCode(t, get.body); code != "NoSuchBucket" {
		t.Fatalf("GET missing-bucket code = %s, want NoSuchBucket", code)
	}
}
