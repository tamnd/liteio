// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"net/http"
	"net/url"
	"testing"
)

// tagXML builds a Tagging XML body from k/v pairs (alternating key, value).
func tagXML(kv ...string) []byte {
	type Tag struct {
		Key   string `xml:"Key"`
		Value string `xml:"Value"`
	}
	type Tagging struct {
		XMLName xml.Name `xml:"Tagging"`
		Tags    []Tag    `xml:"TagSet>Tag"`
	}
	var t Tagging
	for i := 0; i+1 < len(kv); i += 2 {
		t.Tags = append(t.Tags, Tag{Key: kv[i], Value: kv[i+1]})
	}
	b, _ := xml.Marshal(t)
	return b
}

// --- object tagging ------------------------------------------------------

func TestObjectTaggingLifecycle(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/photos", nil, nil)
	h.do("PUT", "/photos/cat.jpg", []byte("img data"), nil)

	// Set two tags via PUT ?tagging.
	res := h.do("PUT", "/photos/cat.jpg?tagging", tagXML("env", "prod", "team", "ml"), nil)
	mustStatus(t, res, http.StatusOK)

	// GET returns both tags; both key/value pairs are in the XML body.
	res = h.do("GET", "/photos/cat.jpg?tagging", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var got taggingResponse
	if err := xml.Unmarshal(res.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tags := tagsFromXML(got.TagSet)
	if tags["env"] != "prod" {
		t.Errorf("env = %q, want prod", tags["env"])
	}
	if tags["team"] != "ml" {
		t.Errorf("team = %q, want ml", tags["team"])
	}

	// DELETE removes all tags; subsequent GET returns an empty TagSet.
	res = h.do("DELETE", "/photos/cat.jpg?tagging", nil, nil)
	mustStatus(t, res, http.StatusNoContent)

	res = h.do("GET", "/photos/cat.jpg?tagging", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var empty taggingResponse
	if err := xml.Unmarshal(res.body, &empty); err != nil {
		t.Fatalf("unmarshal empty: %v", err)
	}
	if len(empty.TagSet) != 0 {
		t.Fatalf("after delete: got %v, want empty tagset", empty.TagSet)
	}

	// The object itself must still be readable after all tagging operations.
	res = h.do("GET", "/photos/cat.jpg", nil, nil)
	mustStatus(t, res, http.StatusOK)
}

func TestObjectTaggingMissingObject(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bucket", nil, nil)
	res := h.do("GET", "/bucket/no-such-key?tagging", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

func TestObjectTaggingInvalidTooMany(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)
	h.do("PUT", "/bkt/obj", []byte("x"), nil)

	// Build 11 tags, exceeding the per-object limit of 10.
	kv := make([]string, 0, 22)
	for i := range 11 {
		kv = append(kv, "k"+string(rune('0'+i)), "v")
	}
	res := h.do("PUT", "/bkt/obj?tagging", tagXML(kv...), nil)
	if res.status == http.StatusOK {
		t.Fatalf("expected error for 11 tags, got 200")
	}
}

// TestPutObjectWithTagHeader checks that x-amz-tagging on PutObject is stored
// and readable via GetObjectTagging.
func TestPutObjectWithTagHeader(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/bkt", nil, nil)
	h.do("PUT", "/bkt/obj", []byte("data"), map[string]string{
		"x-amz-tagging": "color=red&phase=m6",
	})

	res := h.do("GET", "/bkt/obj?tagging", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var resp taggingResponse
	if err := xml.Unmarshal(res.body, &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tags := tagsFromXML(resp.TagSet)
	if tags["color"] != "red" {
		t.Errorf("color = %q, want red", tags["color"])
	}
}

// --- bucket tagging -------------------------------------------------------

func TestBucketTaggingLifecycle(t *testing.T) {
	h := newHarness(t)
	h.do("PUT", "/mybucket", nil, nil)

	// PUT ?tagging
	res := h.do("PUT", "/mybucket?tagging", tagXML("owner", "ops"), nil)
	mustStatus(t, res, http.StatusOK)

	// GET returns the tag.
	res = h.do("GET", "/mybucket?tagging", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var got taggingResponse
	if err := xml.Unmarshal(res.body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	tags := tagsFromXML(got.TagSet)
	if tags["owner"] != "ops" {
		t.Errorf("owner = %q, want ops", tags["owner"])
	}

	// DELETE ?tagging
	res = h.do("DELETE", "/mybucket?tagging", nil, nil)
	mustStatus(t, res, http.StatusNoContent)

	// GET after delete returns NoSuchTagSet.
	res = h.do("GET", "/mybucket?tagging", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

func TestBucketTaggingMissingBucket(t *testing.T) {
	h := newHarness(t)
	res := h.do("GET", "/no-such-bucket?tagging", nil, nil)
	mustStatus(t, res, http.StatusNotFound)
}

// TestOperationNameTagging checks the metrics classifier returns the right labels
// for the six new tagging operations.
func TestOperationNameTagging(t *testing.T) {
	cases := []struct {
		method string
		path   string
		want   string
	}{
		{"PUT", "/bkt?tagging", "PutBucketTagging"},
		{"GET", "/bkt?tagging", "GetBucketTagging"},
		{"DELETE", "/bkt?tagging", "DeleteBucketTagging"},
		{"PUT", "/bkt/key?tagging", "PutObjectTagging"},
		{"GET", "/bkt/key?tagging", "GetObjectTagging"},
		{"DELETE", "/bkt/key?tagging", "DeleteObjectTagging"},
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
