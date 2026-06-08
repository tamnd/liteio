// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeS3 is a minimal in-memory S3 server for migration tests.
type fakeS3 struct {
	buckets map[string]map[string]string // bucket -> key -> body
}

func newFakeS3() *fakeS3 { return &fakeS3{buckets: make(map[string]map[string]string)} }

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	parts := strings.SplitN(path, "/", 2)
	if path == "" || path == "/" {
		// ListBuckets
		type bucket struct {
			Name string `xml:"Name"`
		}
		type buckets struct {
			Bucket []bucket `xml:"Bucket"`
		}
		type resp struct {
			XMLName xml.Name `xml:"ListAllMyBucketsResult"`
			Buckets buckets  `xml:"Buckets"`
		}
		var bs []bucket
		for name := range f.buckets {
			bs = append(bs, bucket{Name: name})
		}
		xml.NewEncoder(w).Encode(resp{Buckets: buckets{Bucket: bs}})
		return
	}
	bkt := parts[0]
	if len(parts) == 1 {
		// Bucket operation.
		switch r.Method {
		case "PUT":
			if f.buckets[bkt] == nil {
				f.buckets[bkt] = make(map[string]string)
			}
			w.WriteHeader(200)
		case "GET":
			// ListObjectsV2
			type content struct {
				Key string `xml:"Key"`
			}
			type resp struct {
				XMLName     xml.Name  `xml:"ListBucketResult"`
				Contents    []content `xml:"Contents"`
				IsTruncated bool      `xml:"IsTruncated"`
			}
			var cs []content
			for k := range f.buckets[bkt] {
				cs = append(cs, content{Key: k})
			}
			xml.NewEncoder(w).Encode(resp{Contents: cs})
		default:
			w.WriteHeader(405)
		}
		return
	}
	key := parts[1]
	switch r.Method {
	case "PUT":
		body, _ := io.ReadAll(r.Body)
		if f.buckets[bkt] == nil {
			f.buckets[bkt] = make(map[string]string)
		}
		f.buckets[bkt][key] = string(body)
		w.WriteHeader(200)
	case "GET":
		body, ok := f.buckets[bkt][key]
		if !ok {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(body)))
		w.WriteHeader(200)
		_, _ = io.WriteString(w, body)
	default:
		w.WriteHeader(405)
	}
}

// TestMigrateOneBucket verifies that all objects in a source bucket are
// copied to the destination.
func TestMigrateOneBucket(t *testing.T) {
	src := newFakeS3()
	src.buckets["photos"] = map[string]string{
		"a.jpg": "imgA",
		"b.jpg": "imgB",
	}
	srcSrv := httptest.NewServer(src)
	defer srcSrv.Close()

	dst := newFakeS3()
	dstSrv := httptest.NewServer(dst)
	defer dstSrv.Close()

	m := &migrator{
		cfg: cfg{
			srcEndpoint: srcSrv.URL,
			srcAK:       "ak", srcSK: "sk",
			dstEndpoint: dstSrv.URL,
			dstAK:       "ak", dstSK: "sk",
			bucket:  "photos",
			workers: 2,
			region:  "us-east-1",
		},
		hc: &http.Client{},
	}
	if err := m.migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	for _, k := range []string{"a.jpg", "b.jpg"} {
		got, ok := dst.buckets["photos"][k]
		if !ok {
			t.Errorf("key %s not found in dst", k)
			continue
		}
		want := src.buckets["photos"][k]
		if got != want {
			t.Errorf("key %s: got %q, want %q", k, got, want)
		}
	}
}

// TestMigrateAllBuckets verifies the migrator discovers buckets from ListBuckets.
func TestMigrateAllBuckets(t *testing.T) {
	src := newFakeS3()
	src.buckets["bkt1"] = map[string]string{"obj1": "v1"}
	src.buckets["bkt2"] = map[string]string{"obj2": "v2"}
	srcSrv := httptest.NewServer(src)
	defer srcSrv.Close()

	dst := newFakeS3()
	dstSrv := httptest.NewServer(dst)
	defer dstSrv.Close()

	m := &migrator{
		cfg: cfg{
			srcEndpoint: srcSrv.URL,
			srcAK:       "ak", srcSK: "sk",
			dstEndpoint: dstSrv.URL,
			dstAK:       "ak", dstSK: "sk",
			workers: 2,
			region:  "us-east-1",
		},
		hc: &http.Client{},
	}
	if err := m.migrate(context.Background()); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if dst.buckets["bkt1"]["obj1"] != "v1" {
		t.Error("bkt1/obj1 not migrated")
	}
	if dst.buckets["bkt2"]["obj2"] != "v2" {
		t.Error("bkt2/obj2 not migrated")
	}
}

// TestMigrateEmptyBucket verifies that an empty source bucket completes without error.
func TestMigrateEmptyBucket(t *testing.T) {
	src := newFakeS3()
	src.buckets["empty"] = map[string]string{}
	srcSrv := httptest.NewServer(src)
	defer srcSrv.Close()

	dst := newFakeS3()
	dstSrv := httptest.NewServer(dst)
	defer dstSrv.Close()

	m := &migrator{
		cfg: cfg{
			srcEndpoint: srcSrv.URL, srcAK: "ak", srcSK: "sk",
			dstEndpoint: dstSrv.URL, dstAK: "ak", dstSK: "sk",
			bucket: "empty", workers: 2, region: "us-east-1",
		},
		hc: &http.Client{},
	}
	if err := m.migrate(context.Background()); err != nil {
		t.Fatalf("empty bucket: %v", err)
	}
}

// TestRunMissingFlags verifies that run() returns an error when required flags are absent.
func TestRunMissingFlags(t *testing.T) {
	if err := run([]string{}); err == nil {
		t.Fatal("expected error for missing flags")
	}
}
