// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

func TestListObjectsV1(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/v1list", nil, nil), http.StatusOK)
	for _, key := range []string{"a/1.txt", "a/2.txt", "b/3.txt", "top.txt"} {
		mustStatus(t, h.do(http.MethodPut, "/v1list/"+key, []byte("v"), nil), http.StatusOK)
	}

	// Plain v1 list (no list-type=2).
	resp := h.do(http.MethodGet, "/v1list", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var res listBucketV1Result
	if err := xml.Unmarshal(resp.body, &res); err != nil {
		t.Fatalf("unmarshal list v1: %v", err)
	}
	if len(res.Contents) != 4 {
		t.Fatalf("flat v1 list = %d keys, want 4", len(res.Contents))
	}

	// Delimiter rolls prefixes.
	resp = h.do(http.MethodGet, "/v1list?delimiter=/", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var res2 listBucketV1Result
	if err := xml.Unmarshal(resp.body, &res2); err != nil {
		t.Fatalf("unmarshal delimited v1: %v", err)
	}
	if len(res2.CommonPrefixes) != 2 {
		t.Fatalf("common prefixes = %d, want 2", len(res2.CommonPrefixes))
	}

	// Marker: start after a/2.txt to get only b/3.txt and top.txt.
	resp = h.do(http.MethodGet, "/v1list?marker=a%2F2.txt", nil, nil)
	mustStatus(t, resp, http.StatusOK)
	var res3 listBucketV1Result
	if err := xml.Unmarshal(resp.body, &res3); err != nil {
		t.Fatalf("unmarshal marker v1: %v", err)
	}
	if len(res3.Contents) != 2 {
		t.Fatalf("marker list = %d keys, want 2 (b/3.txt and top.txt)", len(res3.Contents))
	}
}

func TestGetObjectAttributes(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/attrbkt", nil, nil), http.StatusOK)
	payload := []byte("object attributes test payload")
	mustStatus(t, h.do(http.MethodPut, "/attrbkt/myobj", payload, nil), http.StatusOK)

	res := h.do(http.MethodGet, "/attrbkt/myobj?attributes", nil, map[string]string{
		"x-amz-object-attributes": "ETag,StorageClass,ObjectSize",
	})
	mustStatus(t, res, http.StatusOK)
	var got getObjectAttributesResponse
	if err := xml.Unmarshal(res.body, &got); err != nil {
		t.Fatalf("unmarshal GetObjectAttributes: %v", err)
	}
	if got.ETag == "" {
		t.Error("expected ETag to be set")
	}
	if got.StorageClass != "STANDARD" {
		t.Errorf("StorageClass = %q, want STANDARD", got.StorageClass)
	}
	if got.ObjectSize != int64(len(payload)) {
		t.Errorf("ObjectSize = %d, want %d", got.ObjectSize, len(payload))
	}
}

func TestContentMD5BadDigest(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/md5bkt", nil, nil), http.StatusOK)

	payload := []byte("hello md5")
	// Compute the correct MD5 then flip a byte so it is wrong.
	sum := md5.Sum(payload)
	sum[0] ^= 0xFF
	wrongMD5 := base64.StdEncoding.EncodeToString(sum[:])

	res := h.do(http.MethodPut, "/md5bkt/obj", payload, map[string]string{
		"Content-MD5": wrongMD5,
	})
	if res.status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 BadDigest; body=%s", res.status, res.body)
	}
	if !strings.Contains(string(res.body), "BadDigest") {
		t.Fatalf("expected BadDigest, got %s", res.body)
	}
}

func TestContentMD5GoodDigest(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/goodmd5bkt", nil, nil), http.StatusOK)

	payload := []byte("correct md5 payload")
	sum := md5.Sum(payload)
	correctMD5 := base64.StdEncoding.EncodeToString(sum[:])

	res := h.do(http.MethodPut, "/goodmd5bkt/obj", payload, map[string]string{
		"Content-MD5": correctMD5,
	})
	mustStatus(t, res, http.StatusOK)
}

func TestIfNoneMatchConditionalWrite(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/cndbkt", nil, nil), http.StatusOK)

	payload := []byte("some data")
	// First write with If-None-Match: * succeeds (object does not exist).
	res := h.do(http.MethodPut, "/cndbkt/key", payload, map[string]string{
		"If-None-Match": "*",
	})
	mustStatus(t, res, http.StatusOK)

	// Second write with If-None-Match: * must fail with 412 (object now exists).
	res2 := h.do(http.MethodPut, "/cndbkt/key", payload, map[string]string{
		"If-None-Match": "*",
	})
	if res2.status != http.StatusPreconditionFailed {
		t.Fatalf("status = %d, want 412 PreconditionFailed; body=%s", res2.status, res2.body)
	}
	if !strings.Contains(string(res2.body), "PreconditionFailed") {
		t.Fatalf("expected PreconditionFailed, got %s", res2.body)
	}
}
