// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"net/http"
	"strings"
	"testing"
)

func TestPutGetDeleteBucketCors(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/corstest", nil, nil), http.StatusOK)

	corsBody := []byte(`<CORSConfiguration>
		<CORSRule>
			<ID>r1</ID>
			<AllowedOrigin>https://example.com</AllowedOrigin>
			<AllowedMethod>GET</AllowedMethod>
			<AllowedMethod>PUT</AllowedMethod>
			<AllowedHeader>Content-Type</AllowedHeader>
			<MaxAgeSeconds>600</MaxAgeSeconds>
		</CORSRule>
	</CORSConfiguration>`)

	// PUT CORS config.
	put := h.do(http.MethodPut, "/corstest?cors", corsBody, map[string]string{"Content-Type": "application/xml"})
	mustStatus(t, put, http.StatusOK)

	// GET CORS config.
	get := h.do(http.MethodGet, "/corstest?cors", nil, nil)
	mustStatus(t, get, http.StatusOK)
	var got corsConfigurationXML
	if err := xml.Unmarshal(get.body, &got); err != nil {
		t.Fatalf("unmarshal CORS: %v", err)
	}
	if len(got.Rules) != 1 {
		t.Fatalf("rules = %d, want 1", len(got.Rules))
	}
	if got.Rules[0].ID != "r1" {
		t.Errorf("ID = %q, want r1", got.Rules[0].ID)
	}
	if got.Rules[0].MaxAgeSeconds != 600 {
		t.Errorf("MaxAgeSeconds = %d, want 600", got.Rules[0].MaxAgeSeconds)
	}

	// DELETE CORS config.
	del := h.do(http.MethodDelete, "/corstest?cors", nil, nil)
	mustStatus(t, del, http.StatusNoContent)

	// GET after delete returns 404.
	get2 := h.do(http.MethodGet, "/corstest?cors", nil, nil)
	mustStatus(t, get2, http.StatusNotFound)
	if !strings.Contains(string(get2.body), "NoSuchCORSConfiguration") {
		t.Fatalf("expected NoSuchCORSConfiguration, got %s", get2.body)
	}
}

func TestCORSPreflightHit(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/prefbkt", nil, nil), http.StatusOK)

	corsBody := []byte(`<CORSConfiguration>
		<CORSRule>
			<AllowedOrigin>*</AllowedOrigin>
			<AllowedMethod>GET</AllowedMethod>
			<AllowedHeader>Authorization</AllowedHeader>
			<MaxAgeSeconds>120</MaxAgeSeconds>
		</CORSRule>
	</CORSConfiguration>`)
	mustStatus(t, h.do(http.MethodPut, "/prefbkt?cors", corsBody, map[string]string{"Content-Type": "application/xml"}), http.StatusOK)

	// Send an unauthenticated OPTIONS preflight (no SigV4).
	req, err := http.NewRequest(http.MethodOptions, h.srv.URL+"/prefbkt/somefile.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://myapp.io")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("preflight status = %d, want 200", resp.StatusCode)
	}
	if resp.Header.Get("Access-Control-Allow-Origin") == "" {
		t.Error("expected Access-Control-Allow-Origin header")
	}
}

func TestCORSPreflightNoConfig(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/nocorsbkt", nil, nil), http.StatusOK)

	req, err := http.NewRequest(http.MethodOptions, h.srv.URL+"/nocorsbkt/file.txt", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", "https://anywhere.io")
	req.Header.Set("Access-Control-Request-Method", "GET")
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("no-config preflight status = %d, want 403", resp.StatusCode)
	}
}
