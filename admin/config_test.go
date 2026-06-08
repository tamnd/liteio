// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

func defaultTestConfig() ClusterConfig {
	return ClusterConfig{
		Region:                "us-east-1",
		MaxConcurrentRequests: 0,
		HealWorkers:           4,
		ScannerInterval:       "1h",
	}
}

// bodyHash returns the hex SHA-256 of b for SigV4 signing.
func bodyHash(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// TestGetConfig verifies GET /config returns the seeded defaults.
func TestGetConfig(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	cs := NewInMemoryConfigStore(defaultTestConfig())
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithConfig(cs),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/config", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got ClusterConfig
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Region != "us-east-1" || got.HealWorkers != 4 || got.ScannerInterval != "1h" {
		t.Errorf("unexpected config: %+v", got)
	}
}

// TestSetConfig verifies PUT /config round-trips a new value through the store.
func TestSetConfig(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	cs := NewInMemoryConfigStore(defaultTestConfig())
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithConfig(cs),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	newCfg := ClusterConfig{
		Region:                "eu-west-1",
		MaxConcurrentRequests: 100,
		HealWorkers:           8,
		ScannerInterval:       "30m",
	}
	body, _ := json.Marshal(newCfg)

	req, _ := http.NewRequest(http.MethodPut, ts.URL+APIPrefix+"/config", bytes.NewReader(body))
	req.ContentLength = int64(len(body))
	sign.SignHeader(req, adminCreds, "us-east-1", bodyHash(body), time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("PUT status = %d, want 204", resp.StatusCode)
	}

	// Confirm the store now holds the new value.
	got := cs.GetConfig()
	if got.Region != "eu-west-1" || got.HealWorkers != 8 {
		t.Errorf("after PUT, store holds %+v", got)
	}
}

// TestSetConfigInvalid verifies PUT /config rejects a malformed body with 400.
func TestSetConfigInvalid(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	cs := NewInMemoryConfigStore(defaultTestConfig())
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithConfig(cs),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	bad := []byte(`not json`)
	req, _ := http.NewRequest(http.MethodPut, ts.URL+APIPrefix+"/config", bytes.NewReader(bad))
	req.ContentLength = int64(len(bad))
	sign.SignHeader(req, adminCreds, "us-east-1", bodyHash(bad), time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}
