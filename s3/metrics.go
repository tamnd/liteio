// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"io"
	"net/http"
	"net/url"

	"github.com/tamnd/liteio/metrics"
)

// metricsSet holds the S3 front-door metric families (doc 10.4): per-API request
// counts, error counts by S3 code, request latency, and bytes in and out. It is built
// once over a registry and shared across every request; the families themselves are
// concurrency-safe.
type metricsSet struct {
	requests *metrics.CounterVec   // by api
	errors   *metrics.CounterVec   // by api, S3 error code
	duration *metrics.HistogramVec // seconds, by api
	bytesIn  *metrics.CounterVec   // request body bytes read, by api
	bytesOut *metrics.CounterVec   // response body bytes written, by api
	inFlight *metrics.Gauge        // requests currently being served
}

// newMetricsSet registers the families on reg. The names follow the
// liteio_s3_<subsystem>_<unit> convention so a scraper groups them under one service.
func newMetricsSet(reg *metrics.Registry) *metricsSet {
	return &metricsSet{
		requests: reg.NewCounterVec("liteio_s3_requests_total",
			"S3 requests received, by API operation.", []string{"api"}),
		errors: reg.NewCounterVec("liteio_s3_request_errors_total",
			"S3 requests that returned an error, by API operation and S3 error code.", []string{"api", "code"}),
		duration: reg.NewHistogramVec("liteio_s3_request_duration_seconds",
			"S3 request latency in seconds, by API operation.", []string{"api"}, metrics.DefBuckets),
		bytesIn: reg.NewCounterVec("liteio_s3_request_bytes_total",
			"Request body bytes read, by API operation.", []string{"api"}),
		bytesOut: reg.NewCounterVec("liteio_s3_response_bytes_total",
			"Response body bytes written, by API operation.", []string{"api"}),
		inFlight: reg.NewGauge("liteio_s3_requests_in_flight",
			"S3 requests currently being served."),
	}
}

// observe records one finished request: its count, latency, bytes, and, when it
// returned an S3 error, the error count by code.
func (m *metricsSet) observe(api string, rec *metricsRecorder, bytesIn int64, seconds float64) {
	m.requests.With(api).Inc()
	m.duration.With(api).Observe(seconds)
	if bytesIn > 0 {
		m.bytesIn.With(api).Add(float64(bytesIn))
	}
	if rec.written > 0 {
		m.bytesOut.With(api).Add(float64(rec.written))
	}
	if rec.errorCode != "" {
		m.errors.With(api, rec.errorCode).Inc()
	}
}

// metricsRecorder wraps the response writer to record the bytes written and the S3
// error code (set by writeError through the codeRecorder seam). It forwards the
// optional http.Flusher so a streamed GET still flushes.
type metricsRecorder struct {
	http.ResponseWriter
	written   int64
	errorCode string
}

func newMetricsRecorder(w http.ResponseWriter) *metricsRecorder {
	return &metricsRecorder{ResponseWriter: w}
}

func (r *metricsRecorder) Write(p []byte) (int, error) {
	n, err := r.ResponseWriter.Write(p)
	r.written += int64(n)
	return n, err
}

// Flush forwards to the underlying writer when it supports flushing, so a large GET
// streams rather than buffering once the recorder is in the chain.
func (r *metricsRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// recordError captures the S3 error code. writeError calls it through the codeRecorder
// interface so the front door learns the code without parsing the XML body.
func (r *metricsRecorder) recordError(code string) { r.errorCode = code }

// codeRecorder is the seam writeError uses to report the S3 error code to the metrics
// recorder, if one is wrapping the response. A plain ResponseWriter does not implement
// it, so unmetered serving is unaffected.
type codeRecorder interface {
	recordError(code string)
}

// countingReader counts the bytes read through it, so the front door can record the
// request-body size a handler actually consumed.
type countingReader struct {
	r io.ReadCloser
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

func (c *countingReader) Close() error { return c.r.Close() }

// operationName classifies a request into its S3 API operation for the metrics label,
// mirroring the routing in serveService/serveBucket/serveObject. It is derived purely
// from the request, so it labels even a request that failed authentication. The label
// set is bounded by the operations the router knows, so it cannot blow up cardinality.
func operationName(r *http.Request, res resource) string {
	q := r.URL.Query()
	switch {
	case res.bucket == "":
		if r.Method == http.MethodPost {
			return "STS"
		}
		if r.Method == http.MethodGet {
			return "ListBuckets"
		}
		return "Unknown"
	case res.object == "":
		return bucketOperation(r, q)
	default:
		return objectOperation(r, q)
	}
}

func bucketOperation(r *http.Request, q url.Values) string {
	switch r.Method {
	case http.MethodGet:
		switch {
		case q.Has("location"):
			return "GetBucketLocation"
		case q.Has("versioning"):
			return "GetBucketVersioning"
		case q.Has("policy"):
			return "GetBucketPolicy"
		case q.Has("tagging"):
			return "GetBucketTagging"
		case q.Has("lifecycle"):
			return "GetBucketLifecycleConfiguration"
		case q.Has("object-lock"):
			return "GetObjectLockConfiguration"
		case q.Has("versions"):
			return "ListObjectVersions"
		case q.Has("uploads"):
			return "ListMultipartUploads"
		default:
			return "ListObjectsV2"
		}
	case http.MethodPut:
		switch {
		case q.Has("versioning"):
			return "PutBucketVersioning"
		case q.Has("policy"):
			return "PutBucketPolicy"
		case q.Has("tagging"):
			return "PutBucketTagging"
		case q.Has("lifecycle"):
			return "PutBucketLifecycleConfiguration"
		case q.Has("object-lock"):
			return "PutObjectLockConfiguration"
		default:
			return "CreateBucket"
		}
	case http.MethodHead:
		return "HeadBucket"
	case http.MethodDelete:
		switch {
		case q.Has("policy"):
			return "DeleteBucketPolicy"
		case q.Has("tagging"):
			return "DeleteBucketTagging"
		case q.Has("lifecycle"):
			return "DeleteBucketLifecycleConfiguration"
		}
		return "DeleteBucket"
	case http.MethodPost:
		if q.Has("delete") {
			return "DeleteObjects"
		}
	}
	return "Unknown"
}

func objectOperation(r *http.Request, q url.Values) string {
	uploadID := q.Get("uploadId")
	copySource := r.Header.Get(copySourceHeader)
	switch r.Method {
	case http.MethodPut:
		switch {
		case uploadID != "" && q.Has("partNumber"):
			if copySource != "" {
				return "UploadPartCopy"
			}
			return "UploadPart"
		case copySource != "":
			return "CopyObject"
		case q.Has("tagging"):
			return "PutObjectTagging"
		case q.Has("retention"):
			return "PutObjectRetention"
		case q.Has("legal-hold"):
			return "PutObjectLegalHold"
		default:
			return "PutObject"
		}
	case http.MethodGet:
		if uploadID != "" {
			return "ListParts"
		}
		if q.Has("tagging") {
			return "GetObjectTagging"
		}
		if q.Has("retention") {
			return "GetObjectRetention"
		}
		if q.Has("legal-hold") {
			return "GetObjectLegalHold"
		}
		return "GetObject"
	case http.MethodHead:
		return "HeadObject"
	case http.MethodPost:
		switch {
		case q.Has("uploads"):
			return "CreateMultipartUpload"
		case uploadID != "":
			return "CompleteMultipartUpload"
		}
	case http.MethodDelete:
		if uploadID != "" {
			return "AbortMultipartUpload"
		}
		if q.Has("tagging") {
			return "DeleteObjectTagging"
		}
		return "DeleteObject"
	}
	return "Unknown"
}
