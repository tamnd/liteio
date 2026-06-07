// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"testing"
)

func TestParseCopySource(t *testing.T) {
	cases := []struct {
		in               string
		bucket, key, ver string
		wantErr          bool
	}{
		{in: "/b/k", bucket: "b", key: "k"},
		{in: "b/k", bucket: "b", key: "k"},
		{in: "/b/dir/sub/file.txt", bucket: "b", key: "dir/sub/file.txt"},
		{in: "/b/a%20b.txt", bucket: "b", key: "a b.txt"},
		{in: "/b/k%2Fwith%2Fslash", bucket: "b", key: "k/with/slash"},
		{in: "/b/k?versionId=v9", bucket: "b", key: "k", ver: "v9"},
		{in: "/b", wantErr: true},
		{in: "", wantErr: true},
		{in: "/b/", wantErr: true},
	}
	for _, c := range cases {
		b, k, v, err := parseCopySource(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseCopySource(%q): want error", c.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseCopySource(%q): %v", c.in, err)
			continue
		}
		if b != c.bucket || k != c.key || v != c.ver {
			t.Errorf("parseCopySource(%q) = %q,%q,%q want %q,%q,%q", c.in, b, k, v, c.bucket, c.key, c.ver)
		}
	}
}

func TestS3CopyObject(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/src", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/dst", nil, nil), http.StatusOK)

	body := bytes.Repeat([]byte("z"), 3<<20)
	mustStatus(t, h.do(http.MethodPut, "/src/orig.bin", body, map[string]string{"x-amz-meta-team": "blue"}), http.StatusOK)

	// COPY across buckets carries the source metadata forward.
	res := h.do(http.MethodPut, "/dst/copy.bin", nil, map[string]string{copySourceHeader: "/src/orig.bin"})
	mustStatus(t, res, http.StatusOK)
	var out copyObjectResult
	if err := xml.Unmarshal(res.body, &out); err != nil {
		t.Fatalf("decode copy result: %v; body=%s", err, res.body)
	}
	if out.ETag == "" {
		t.Fatalf("copy result missing ETag: %+v", out)
	}

	get := h.do(http.MethodGet, "/dst/copy.bin", nil, nil)
	mustStatus(t, get, http.StatusOK)
	if !bytes.Equal(get.body, body) {
		t.Fatalf("copied object: %d bytes, want %d", len(get.body), len(body))
	}
	if got := get.header.Get("x-amz-meta-team"); got != "blue" {
		t.Fatalf("copied metadata team = %q, want blue", got)
	}
}

func TestS3CopyObjectSelfIllegal(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/b", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/b/k", []byte("hi"), nil), http.StatusOK)

	// Copy onto self with the default COPY directive is rejected.
	res := h.do(http.MethodPut, "/b/k", nil, map[string]string{copySourceHeader: "/b/k"})
	mustStatus(t, res, http.StatusBadRequest)
	if !bytes.Contains(res.body, []byte("InvalidRequest")) {
		t.Fatalf("self-copy = %s", res.body)
	}

	// Same copy with REPLACE is allowed and rewrites the metadata.
	res = h.do(http.MethodPut, "/b/k", nil, map[string]string{
		copySourceHeader:     "/b/k",
		metadataDirectiveStr: "REPLACE",
		"x-amz-meta-fresh":   "yes",
	})
	mustStatus(t, res, http.StatusOK)
	head := h.do(http.MethodHead, "/b/k", nil, nil)
	mustStatus(t, head, http.StatusOK)
	if head.header.Get("x-amz-meta-fresh") != "yes" {
		t.Fatalf("replace metadata missing: %v", head.header)
	}
}

func TestS3CopyObjectMissingSource(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/b", nil, nil), http.StatusOK)
	res := h.do(http.MethodPut, "/b/dst", nil, map[string]string{copySourceHeader: "/b/ghost"})
	mustStatus(t, res, http.StatusNotFound)
	if !bytes.Contains(res.body, []byte("NoSuchKey")) {
		t.Fatalf("copy of missing source = %s", res.body)
	}
}

func TestS3UploadPartCopy(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/b", nil, nil), http.StatusOK)

	source := make([]byte, 6<<20)
	for i := range source {
		source[i] = byte(i * 7)
	}
	mustStatus(t, h.do(http.MethodPut, "/b/source.bin", source, nil), http.StatusOK)

	uploadID := h.initiateUpload("b", "joined.bin")

	// Part 1: first 5 MiB by range. Part 2: the small tail by range.
	r1 := h.do(http.MethodPut, "/b/joined.bin?partNumber=1&uploadId="+uploadID, nil, map[string]string{
		copySourceHeader:      "/b/source.bin",
		copySourceRangeHeader: "bytes=0-5242879",
	})
	mustStatus(t, r1, http.StatusOK)
	r2 := h.do(http.MethodPut, "/b/joined.bin?partNumber=2&uploadId="+uploadID, nil, map[string]string{
		copySourceHeader:      "/b/source.bin",
		copySourceRangeHeader: "bytes=5242880-6291455",
	})
	mustStatus(t, r2, http.StatusOK)

	var cp1, cp2 copyPartResult
	if err := xml.Unmarshal(r1.body, &cp1); err != nil {
		t.Fatalf("decode copy part 1: %v; body=%s", err, r1.body)
	}
	if err := xml.Unmarshal(r2.body, &cp2); err != nil {
		t.Fatalf("decode copy part 2: %v; body=%s", err, r2.body)
	}

	body, err := xml.Marshal(completeMultipartUpload{Parts: []completePartXML{
		{PartNumber: 1, ETag: cp1.ETag},
		{PartNumber: 2, ETag: cp2.ETag},
	}})
	if err != nil {
		t.Fatalf("marshal complete: %v", err)
	}
	mustStatus(t, h.do(http.MethodPost, "/b/joined.bin?uploadId="+uploadID, body, nil), http.StatusOK)

	get := h.do(http.MethodGet, "/b/joined.bin", nil, nil)
	mustStatus(t, get, http.StatusOK)
	if !bytes.Equal(get.body, source) {
		t.Fatalf("UploadPartCopy reassembled %d bytes, want %d", len(get.body), len(source))
	}
}
