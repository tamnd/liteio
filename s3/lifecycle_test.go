// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"net/http"
	"net/url"
	"testing"
)

// lifecycleXML is a minimal S3 LifecycleConfiguration document.
const lifecycleXML = `<LifecycleConfiguration xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Rule><ID>expire-old</ID><Filter><Prefix>logs/</Prefix></Filter><Status>Enabled</Status><Expiration><Days>30</Days></Expiration></Rule></LifecycleConfiguration>`

func TestBucketLifecycleLifecycle(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/mybucket", nil, nil)

	// PUT the configuration.
	res := h.do("PUT", "/mybucket?lifecycle", []byte(lifecycleXML), nil)
	mustStatus(t, res, http.StatusOK)

	// GET returns the exact bytes we put.
	res = h.do("GET", "/mybucket?lifecycle", nil, nil)
	mustStatus(t, res, http.StatusOK)
	if string(res.body) != lifecycleXML {
		t.Errorf("got %q, want %q", res.body, lifecycleXML)
	}

	// DELETE removes it.
	res = h.do("DELETE", "/mybucket?lifecycle", nil, nil)
	mustStatus(t, res, http.StatusNoContent)

	// GET after delete returns NoSuchLifecycleConfiguration.
	res = h.do("GET", "/mybucket?lifecycle", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

func TestBucketLifecycleMissingBucket(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/no-such-bucket?lifecycle", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

func TestBucketLifecycleEmptyBody(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)
	res := h.do("PUT", "/bkt?lifecycle", nil, nil)
	if res.status == http.StatusOK {
		t.Fatal("expected error for empty lifecycle body, got 200")
	}
}

func TestBucketLifecycleDeleteIdempotent(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)
	// First delete on a bucket with no lifecycle must still succeed.
	res := h.do("DELETE", "/bkt?lifecycle", nil, nil)
	mustStatus(t, res, http.StatusNoContent)
}

// TestOperationNameLifecycle checks the metrics classifier for the three
// new lifecycle operations.
func TestOperationNameLifecycle(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"PUT", "/bkt?lifecycle", "PutBucketLifecycleConfiguration"},
		{"GET", "/bkt?lifecycle", "GetBucketLifecycleConfiguration"},
		{"DELETE", "/bkt?lifecycle", "DeleteBucketLifecycleConfiguration"},
	}
	s := &Server{}
	for _, tc := range cases {
		u, _ := url.Parse(tc.path)
		req := &http.Request{
			Method: tc.method,
			URL:    u,
			Header: http.Header{},
		}
		got := operationName(req, s.parseResource(req))
		if got != tc.want {
			t.Errorf("%s %s = %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
