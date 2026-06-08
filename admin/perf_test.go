// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// buildTestPerfLayer creates a minimal ServerPools backed by temp-dir drives
// so the perf test handler exercises real object I/O.
func buildTestPerfLayer(t *testing.T) object.ObjectLayer {
	t.Helper()
	var id [16]byte
	drives := make([]storage.StorageAPI, 2)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	sp, err := object.NewSingleSet(id, drives, 1)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}
	return sp
}

// TestPerfEndpointReturnsResult verifies that POST /perf returns 200 with valid
// throughput numbers (non-negative, all fields present).
func TestPerfEndpointReturnsResult(t *testing.T) {
	pl := buildTestPerfLayer(t)
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithPerfLayer(pl),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Short run so the test is fast.
	req, _ := http.NewRequest("POST", ts.URL+APIPrefix+"/perf?duration=200ms&size=512&workers=2", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	var result PerfResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if result.PutMBs < 0 || result.GetMBs < 0 {
		t.Errorf("negative throughput: put=%.2f get=%.2f", result.PutMBs, result.GetMBs)
	}
	if result.Workers != 2 {
		t.Errorf("workers = %d, want 2", result.Workers)
	}
	if result.ObjectSize != 512 {
		t.Errorf("object_size = %d, want 512", result.ObjectSize)
	}
}

// TestPerfNotConfiguredReturns501 verifies the handler returns 501 without a perf layer.
func TestPerfNotConfiguredReturns501(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }))
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest("POST", ts.URL+APIPrefix+"/perf", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, _ := ts.Client().Do(req)
	if resp != nil {
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("expected 501, got %d", resp.StatusCode)
		}
	}
}

// TestParseDuration verifies the helper for all edge cases.
func TestParseDuration(t *testing.T) {
	for _, tc := range []struct {
		s    string
		def  time.Duration
		want time.Duration
	}{
		{"", 5 * time.Second, 5 * time.Second},
		{"bad", 5 * time.Second, 5 * time.Second},
		{"10s", 5 * time.Second, 10 * time.Second},
		{"0s", 5 * time.Second, 5 * time.Second},  // zero treated as bad
		{"-1s", 5 * time.Second, 5 * time.Second}, // negative treated as bad
	} {
		if got := parseDuration(tc.s, tc.def); got != tc.want {
			t.Errorf("parseDuration(%q) = %v, want %v", tc.s, got, tc.want)
		}
	}
}

// TestRunPhase verifies the counters from a trivial phase that always succeeds.
func TestRunPhase(t *testing.T) {
	ops, byts := runPhase(context.Background(), 2, 100*time.Millisecond, func(i int) int64 {
		// Simulate a 128-byte operation.
		_ = bytes.Repeat([]byte{0}, 128)
		return 128
	})
	if ops <= 0 {
		t.Errorf("expected >0 ops, got %d", ops)
	}
	if byts != ops*128 {
		t.Errorf("bytes = %d, want ops*128 = %d", byts, ops*128)
	}
}
