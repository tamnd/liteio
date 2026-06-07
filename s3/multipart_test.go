// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

// initiateUpload starts a multipart upload over the wire and returns its id.
func (h *harness) initiateUpload(bucket, key string) string {
	h.t.Helper()
	res := h.do(http.MethodPost, "/"+bucket+"/"+key+"?uploads", nil, nil)
	mustStatus(h.t, res, http.StatusOK)
	var out initiateMultipartUploadResult
	if err := xml.Unmarshal(res.body, &out); err != nil {
		h.t.Fatalf("decode initiate: %v; body=%s", err, res.body)
	}
	if out.UploadID == "" || out.Bucket != bucket || out.Key != key {
		h.t.Fatalf("initiate result = %+v", out)
	}
	return out.UploadID
}

func TestS3MultipartRoundTrip(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/mp", nil, nil), http.StatusOK)

	uploadID := h.initiateUpload("mp", "big.bin")

	p1 := bytes.Repeat([]byte("A"), 5<<20) // 5 MiB, satisfies the part minimum
	p2 := bytes.Repeat([]byte("B"), 4096)  // small final part

	r1 := h.do(http.MethodPut, "/mp/big.bin?partNumber=1&uploadId="+uploadID, p1, nil)
	mustStatus(t, r1, http.StatusOK)
	r2 := h.do(http.MethodPut, "/mp/big.bin?partNumber=2&uploadId="+uploadID, p2, nil)
	mustStatus(t, r2, http.StatusOK)
	e1, e2 := r1.header.Get("ETag"), r2.header.Get("ETag")
	if e1 == "" || e2 == "" {
		t.Fatalf("missing part ETags: %q %q", e1, e2)
	}

	// ListParts shows both parts.
	lp := h.do(http.MethodGet, "/mp/big.bin?uploadId="+uploadID, nil, nil)
	mustStatus(t, lp, http.StatusOK)
	var parts listPartsResult
	if err := xml.Unmarshal(lp.body, &parts); err != nil {
		t.Fatalf("decode list parts: %v", err)
	}
	if len(parts.Parts) != 2 {
		t.Fatalf("listed %d parts, want 2", len(parts.Parts))
	}

	// ListMultipartUploads shows the in-progress upload.
	lu := h.do(http.MethodGet, "/mp?uploads", nil, nil)
	mustStatus(t, lu, http.StatusOK)
	var uploads listMultipartUploadsResult
	if err := xml.Unmarshal(lu.body, &uploads); err != nil {
		t.Fatalf("decode list uploads: %v", err)
	}
	if len(uploads.Uploads) != 1 || uploads.Uploads[0].UploadID != uploadID {
		t.Fatalf("list uploads = %+v", uploads.Uploads)
	}

	// Complete the upload.
	body, err := xml.Marshal(completeMultipartUpload{Parts: []completePartXML{
		{PartNumber: 1, ETag: e1},
		{PartNumber: 2, ETag: e2},
	}})
	if err != nil {
		t.Fatalf("marshal complete: %v", err)
	}
	cm := h.do(http.MethodPost, "/mp/big.bin?uploadId="+uploadID, body, nil)
	mustStatus(t, cm, http.StatusOK)
	var done completeMultipartUploadResult
	if err := xml.Unmarshal(cm.body, &done); err != nil {
		t.Fatalf("decode complete: %v", err)
	}
	if !strings.HasSuffix(done.ETag, `-2"`) {
		t.Fatalf("composite ETag = %q, want ...-2", done.ETag)
	}

	// The assembled object reads back whole.
	get := h.do(http.MethodGet, "/mp/big.bin", nil, nil)
	mustStatus(t, get, http.StatusOK)
	want := append(append([]byte{}, p1...), p2...)
	if !bytes.Equal(get.body, want) {
		t.Fatalf("GET assembled object: %d bytes, want %d", len(get.body), len(want))
	}

	// A second ranged GET across the part boundary returns the right window.
	rng := h.do(http.MethodGet, "/mp/big.bin", nil, map[string]string{"Range": "bytes=5242878-5242881"})
	mustStatus(t, rng, http.StatusPartialContent)
	if !bytes.Equal(rng.body, want[5242878:5242882]) {
		t.Fatalf("ranged GET = %q", rng.body)
	}
}

func TestS3MultipartAbort(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/mp", nil, nil), http.StatusOK)
	uploadID := h.initiateUpload("mp", "k")
	mustStatus(t, h.do(http.MethodPut, "/mp/k?partNumber=1&uploadId="+uploadID, bytes.Repeat([]byte("A"), 5<<20), nil), http.StatusOK)

	mustStatus(t, h.do(http.MethodDelete, "/mp/k?uploadId="+uploadID, nil, nil), http.StatusNoContent)

	// The upload is gone; listing its parts is NoSuchUpload.
	res := h.do(http.MethodGet, "/mp/k?uploadId="+uploadID, nil, nil)
	mustStatus(t, res, http.StatusNotFound)
	if !strings.Contains(string(res.body), "NoSuchUpload") {
		t.Fatalf("abort then list = %s", res.body)
	}
}

func TestS3MultipartCompleteErrors(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/mp", nil, nil), http.StatusOK)
	uploadID := h.initiateUpload("mp", "k")
	r1 := h.do(http.MethodPut, "/mp/k?partNumber=1&uploadId="+uploadID, bytes.Repeat([]byte("A"), 5<<20), nil)
	mustStatus(t, r1, http.StatusOK)

	// Completing with a wrong ETag yields InvalidPart.
	body, _ := xml.Marshal(completeMultipartUpload{Parts: []completePartXML{{PartNumber: 1, ETag: `"deadbeef"`}}})
	res := h.do(http.MethodPost, "/mp/k?uploadId="+uploadID, body, nil)
	mustStatus(t, res, http.StatusBadRequest)
	if !strings.Contains(string(res.body), "InvalidPart") {
		t.Fatalf("bad-etag complete = %s", res.body)
	}

	// Uploading a part to an unknown upload yields NoSuchUpload.
	res = h.do(http.MethodPut, "/mp/k?partNumber=1&uploadId=nope", []byte("x"), nil)
	mustStatus(t, res, http.StatusNotFound)
	if !strings.Contains(string(res.body), "NoSuchUpload") {
		t.Fatalf("unknown upload part = %s", res.body)
	}
}
