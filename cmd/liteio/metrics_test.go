// SPDX-License-Identifier: Apache-2.0

package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/metrics"
	"github.com/tamnd/liteio/s3"
)

// metricsCfg is rootCfg with a metrics token, so buildConsole mounts the endpoint.
func metricsCfg(token string) config {
	cfg := rootCfg()
	cfg.metricsToken = token
	return cfg
}

// TestMetricsEndpointTokenGated confirms the console listener serves /metrics only to
// a request carrying the configured bearer token: no header is 401, a wrong token is
// 401, and the right token returns the exposition with the S3 request families.
func TestMetricsEndpointTokenGated(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	layer := newDriveLayer(t, 4, 2)
	registry := metrics.NewRegistry()
	s3Handler := s3.NewServer(layer, store, s3.WithMetrics(registry))
	handler, _, err := buildConsole(metricsCfg("scrape-secret"), store, layer, s3Handler, registry)
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	// Drive one request through the S3 handler so a metric exists to scrape. The
	// console listener does not serve S3, so go to the handler directly here.
	s3srv := httptest.NewServer(s3Handler)
	defer s3srv.Close()
	resp, err := http.Get(s3srv.URL + "/") // unsigned ListBuckets, recorded then refused
	if err != nil {
		t.Fatalf("seed request: %v", err)
	}
	resp.Body.Close()

	// No Authorization header.
	if status := metricsGET(t, srv.URL, ""); status != http.StatusUnauthorized {
		t.Errorf("no token: status = %d, want 401", status)
	}
	// Wrong token.
	if status := metricsGET(t, srv.URL, "wrong"); status != http.StatusUnauthorized {
		t.Errorf("wrong token: status = %d, want 401", status)
	}
	// Right token returns the exposition.
	body, status := metricsGETBody(t, srv.URL, "scrape-secret")
	if status != http.StatusOK {
		t.Fatalf("right token: status = %d, want 200", status)
	}
	if !strings.Contains(body, "liteio_s3_requests_total") {
		t.Errorf("exposition missing S3 metrics\n%s", body)
	}
}

// TestMetricsEndpointDisabledWithoutToken confirms the endpoint is not mounted when no
// token is configured: the path falls through to the SPA shell rather than exposing
// metrics unauthenticated.
func TestMetricsEndpointDisabledWithoutToken(t *testing.T) {
	store := auth.NewStore("liteioadmin", "liteioadmin")
	registry := metrics.NewRegistry()
	handler, _, err := buildConsole(rootCfg(), store, nil, nil, registry) // no metrics token
	if err != nil {
		t.Fatalf("buildConsole: %v", err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()

	body, status := metricsGETBody(t, srv.URL, "anything")
	if status != http.StatusOK || !strings.Contains(body, "liteio console") {
		t.Errorf("without a token, /metrics status = %d, want the SPA shell; body=%q", status, body)
	}
}

func metricsGET(t *testing.T, base, token string) int {
	t.Helper()
	_, status := metricsGETBody(t, base, token)
	return status
}

func metricsGETBody(t *testing.T, base, token string) (string, int) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, base+"/metrics", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(b), resp.StatusCode
}
