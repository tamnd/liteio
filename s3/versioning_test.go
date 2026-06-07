// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/xml"
	"net/http"
	"testing"
)

// enableVersioning turns on versioning for a bucket over the wire.
func (h *harness) enableVersioning(bucket string) {
	h.t.Helper()
	body, err := xml.Marshal(versioningConfiguration{Status: "Enabled"})
	if err != nil {
		h.t.Fatalf("marshal versioning config: %v", err)
	}
	mustStatus(h.t, h.do(http.MethodPut, "/"+bucket+"?versioning", body, nil), http.StatusOK)
}

func TestS3ListObjectVersions(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/v", nil, nil), http.StatusOK)
	h.enableVersioning("v")

	// Two versions of one key.
	r1 := h.do(http.MethodPut, "/v/doc.txt", []byte("one"), nil)
	mustStatus(t, r1, http.StatusOK)
	r2 := h.do(http.MethodPut, "/v/doc.txt", []byte("two two"), nil)
	mustStatus(t, r2, http.StatusOK)
	v1, v2 := r1.header.Get("x-amz-version-id"), r2.header.Get("x-amz-version-id")
	if v1 == "" || v2 == "" || v1 == v2 {
		t.Fatalf("version ids: %q %q", v1, v2)
	}

	// Delete creates a marker as the new latest.
	del := h.do(http.MethodDelete, "/v/doc.txt", nil, nil)
	mustStatus(t, del, http.StatusNoContent)
	if del.header.Get("x-amz-delete-marker") != "true" {
		t.Fatalf("delete in versioned bucket should set x-amz-delete-marker")
	}

	res := h.do(http.MethodGet, "/v?versions", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var out listVersionsResult
	if err := xml.Unmarshal(res.body, &out); err != nil {
		t.Fatalf("decode versions: %v; body=%s", err, res.body)
	}
	if len(out.Versions) != 2 {
		t.Fatalf("got %d versions, want 2: %+v", len(out.Versions), out.Versions)
	}
	if len(out.DeleteMarkers) != 1 {
		t.Fatalf("got %d delete markers, want 1", len(out.DeleteMarkers))
	}
	if !out.DeleteMarkers[0].IsLatest {
		t.Fatalf("delete marker should be the latest version")
	}
	// The delete marker is the current version, so no Version entry is latest;
	// v2 is merely the newest real version.
	if out.Versions[0].VersionID != v2 || out.Versions[0].IsLatest {
		t.Fatalf("Version[0] = %+v, want %s not latest", out.Versions[0], v2)
	}

	// A specific older version still reads back by versionId.
	get := h.do(http.MethodGet, "/v/doc.txt?versionId="+v1, nil, nil)
	mustStatus(t, get, http.StatusOK)
	if !bytes.Equal(get.body, []byte("one")) {
		t.Fatalf("GET versionId %s = %q, want one", v1, get.body)
	}
}

func TestS3ListObjectVersionsPagination(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/v", nil, nil), http.StatusOK)
	h.enableVersioning("v")
	mustStatus(t, h.do(http.MethodPut, "/v/a", []byte("a1"), nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/v/a", []byte("a2"), nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/v/c", []byte("c1"), nil), http.StatusOK)

	p1 := h.do(http.MethodGet, "/v?versions&max-keys=2", nil, nil)
	mustStatus(t, p1, http.StatusOK)
	var o1 listVersionsResult
	if err := xml.Unmarshal(p1.body, &o1); err != nil {
		t.Fatalf("decode page1: %v", err)
	}
	if !o1.IsTruncated || o1.NextKeyMarker == "" {
		t.Fatalf("page1 not truncated: %+v", o1)
	}

	p2 := h.do(http.MethodGet, "/v?versions&key-marker="+o1.NextKeyMarker+"&version-id-marker="+o1.NextVersionIDMarker, nil, nil)
	mustStatus(t, p2, http.StatusOK)
	var o2 listVersionsResult
	if err := xml.Unmarshal(p2.body, &o2); err != nil {
		t.Fatalf("decode page2: %v", err)
	}
	if len(o2.Versions) != 1 || o2.Versions[0].Key != "c" {
		t.Fatalf("page2 = %+v, want single c", o2.Versions)
	}
}
