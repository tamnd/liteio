// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/metrics"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// newMeteredHarness wires a front door with metrics enabled and returns the harness
// and the registry it records to.
func newMeteredHarness(t *testing.T) (*harness, *metrics.Registry) {
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
	reg := metrics.NewRegistry()
	server := NewServer(layer, store,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithMetrics(reg))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &harness{t: t, srv: srv}, reg
}

func scrape(t *testing.T, reg *metrics.Registry) string {
	t.Helper()
	var b bytes.Buffer
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

// TestMetricsRecordsRequests drives a bucket and object lifecycle through the metered
// front door, then scrapes and asserts the per-API counters, the latency histogram,
// the byte counters, and the error counter by S3 code all reflect what happened.
func TestMetricsRecordsRequests(t *testing.T) {
	h, reg := newMeteredHarness(t)

	payload := []byte("the cat sat on the mat")
	mustStatus(t, h.do(http.MethodPut, "/photos", nil, nil), http.StatusOK)             // CreateBucket
	mustStatus(t, h.do(http.MethodPut, "/photos/cat.txt", payload, nil), http.StatusOK) // PutObject
	mustStatus(t, h.do(http.MethodGet, "/photos/cat.txt", nil, nil), http.StatusOK)     // GetObject
	// A GET of a missing key is a NoSuchKey error, counted by code.
	mustStatus(t, h.do(http.MethodGet, "/photos/missing.txt", nil, nil), http.StatusNotFound)

	out := scrape(t, reg)
	want := []string{
		`liteio_s3_requests_total{api="CreateBucket"} 1`,
		`liteio_s3_requests_total{api="PutObject"} 1`,
		`liteio_s3_requests_total{api="GetObject"} 2`,
		`liteio_s3_request_errors_total{api="GetObject",code="NoSuchKey"} 1`,
		`liteio_s3_request_duration_seconds_count{api="GetObject"} 2`,
		// PutObject read the body, so request bytes are recorded for it.
		`liteio_s3_request_bytes_total{api="PutObject"} 22`,
		// The successful GetObject wrote the object back, so response bytes are recorded.
		`liteio_s3_response_bytes_total{api="GetObject"}`,
		"# TYPE liteio_s3_requests_total counter",
		"# TYPE liteio_s3_request_duration_seconds histogram",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("scrape missing %q\n%s", w, out)
		}
	}
}

// TestMetricsInFlightSettles confirms the in-flight gauge returns to zero once every
// request has finished, so it is not leaking increments.
func TestMetricsInFlightSettles(t *testing.T) {
	h, reg := newMeteredHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/b", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodHead, "/b", nil, nil), http.StatusOK)
	out := scrape(t, reg)
	if !strings.Contains(out, "liteio_s3_requests_in_flight 0") {
		t.Errorf("in-flight gauge did not settle to 0\n%s", out)
	}
}

// TestUnmeteredServerHasNoOverhead confirms a server built without WithMetrics serves
// normally and never touches the metrics path (the nil set is the guard).
func TestUnmeteredServerHasNoOverhead(t *testing.T) {
	h := newHarness(t) // no WithMetrics
	mustStatus(t, h.do(http.MethodPut, "/b", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodGet, "/b", nil, nil), http.StatusOK)
}

// BenchmarkOperationName measures the per-request classification cost the metered
// front door adds on top of the metric updates, for a representative object GET.
func BenchmarkOperationName(b *testing.B) {
	s := &Server{}
	r := &http.Request{
		Method: http.MethodGet,
		URL:    &url.URL{Path: "/photos/cat.txt"},
		Header: http.Header{},
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = operationName(r, s.parseResource(r))
	}
}

func TestOperationName(t *testing.T) {
	cases := []struct {
		method, path, query, copySource string
		want                            string
	}{
		{http.MethodGet, "/", "", "", "ListBuckets"},
		{http.MethodPost, "/", "", "", "STS"},
		{http.MethodGet, "/bucket", "", "", "ListObjectsV2"},
		{http.MethodGet, "/bucket", "location=", "", "GetBucketLocation"},
		{http.MethodGet, "/bucket", "versioning=", "", "GetBucketVersioning"},
		{http.MethodGet, "/bucket", "policy=", "", "GetBucketPolicy"},
		{http.MethodGet, "/bucket", "versions=", "", "ListObjectVersions"},
		{http.MethodGet, "/bucket", "uploads=", "", "ListMultipartUploads"},
		{http.MethodPut, "/bucket", "", "", "CreateBucket"},
		{http.MethodPut, "/bucket", "versioning=", "", "PutBucketVersioning"},
		{http.MethodPut, "/bucket", "policy=", "", "PutBucketPolicy"},
		{http.MethodHead, "/bucket", "", "", "HeadBucket"},
		{http.MethodDelete, "/bucket", "", "", "DeleteBucket"},
		{http.MethodDelete, "/bucket", "policy=", "", "DeleteBucketPolicy"},
		{http.MethodPost, "/bucket", "delete=", "", "DeleteObjects"},
		{http.MethodPut, "/bucket/key", "", "", "PutObject"},
		{http.MethodPut, "/bucket/key", "", "/src/obj", "CopyObject"},
		{http.MethodPut, "/bucket/key", "uploadId=x&partNumber=1", "", "UploadPart"},
		{http.MethodPut, "/bucket/key", "uploadId=x&partNumber=1", "/src/obj", "UploadPartCopy"},
		{http.MethodGet, "/bucket/key", "", "", "GetObject"},
		{http.MethodGet, "/bucket/key", "uploadId=x", "", "ListParts"},
		{http.MethodHead, "/bucket/key", "", "", "HeadObject"},
		{http.MethodPost, "/bucket/key", "uploads=", "", "CreateMultipartUpload"},
		{http.MethodPost, "/bucket/key", "uploadId=x", "", "CompleteMultipartUpload"},
		{http.MethodDelete, "/bucket/key", "", "", "DeleteObject"},
		{http.MethodDelete, "/bucket/key", "uploadId=x", "", "AbortMultipartUpload"},
		{http.MethodDelete, "/bucket/key", "tagging=", "", "DeleteObjectTagging"},
		{http.MethodPut, "/bucket/key", "tagging=", "", "PutObjectTagging"},
		{http.MethodGet, "/bucket/key", "tagging=", "", "GetObjectTagging"},
		{http.MethodPut, "/bucket", "tagging=", "", "PutBucketTagging"},
		{http.MethodGet, "/bucket", "tagging=", "", "GetBucketTagging"},
		{http.MethodDelete, "/bucket", "tagging=", "", "DeleteBucketTagging"},
		{http.MethodPatch, "/bucket", "", "", "Unknown"},
	}
	s := &Server{}
	for _, c := range cases {
		r := &http.Request{
			Method: c.method,
			URL:    &url.URL{Path: c.path, RawQuery: c.query},
			Header: http.Header{},
		}
		if c.copySource != "" {
			r.Header.Set(copySourceHeader, c.copySource)
		}
		got := operationName(r, s.parseResource(r))
		if got != c.want {
			t.Errorf("%s %s?%s -> %q, want %q", c.method, c.path, c.query, got, c.want)
		}
	}
}
