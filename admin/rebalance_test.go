// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
)

// fakeRebalancer is a minimal RebalanceLayer for admin handler tests.
type fakeRebalancer struct {
	started     bool
	stopped     bool
	dcStarted   int
	dcStopped   int
	rebalStatus object.RebalanceStatus
	dcStatus    object.DecommissionStatus
}

func (f *fakeRebalancer) Rebalance(_ context.Context) error {
	f.started = true
	return nil
}
func (f *fakeRebalancer) RebalanceStatus() object.RebalanceStatus { return f.rebalStatus }
func (f *fakeRebalancer) StopRebalance()                          { f.stopped = true }
func (f *fakeRebalancer) Decommission(_ context.Context, idx int) error {
	f.dcStarted = idx
	return nil
}
func (f *fakeRebalancer) DecommissionStatus(idx int) object.DecommissionStatus {
	f.dcStatus.PoolIndex = idx
	return f.dcStatus
}
func (f *fakeRebalancer) StopDecommission(idx int) { f.dcStopped = idx }

// newRBHarness builds an admin.Server with a fakeRebalancer and returns the
// test server plus a helper that issues signed requests as root.
func newRBHarness(t *testing.T, fr *fakeRebalancer) (func(method, path string) (int, []byte), *httptest.Server) {
	t.Helper()
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }), WithRebalancer(fr))
	ts := httptest.NewServer(s)
	t.Cleanup(ts.Close)
	do := func(method, path string) (int, []byte) {
		req, _ := http.NewRequest(method, ts.URL+APIPrefix+path, nil)
		sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("do %s %s: %v", method, path, err)
		}
		defer resp.Body.Close()
		var buf [4096]byte
		n, _ := resp.Body.Read(buf[:])
		return resp.StatusCode, buf[:n]
	}
	return do, ts
}

// TestStartRebalanceReturns202 verifies POST /rebalance responds 202 Accepted.
func TestStartRebalanceReturns202(t *testing.T) {
	fr := &fakeRebalancer{}
	do, _ := newRBHarness(t, fr)
	status, body := do("POST", "/rebalance")
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", status, body)
	}
	var m map[string]string
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if m["status"] != "started" {
		t.Fatalf("status field = %q, want started", m["status"])
	}
}

// TestGetRebalanceStatus verifies GET /rebalance returns a JSON RebalanceStatus.
func TestGetRebalanceStatus(t *testing.T) {
	fr := &fakeRebalancer{rebalStatus: object.RebalanceStatus{ObjectsMoved: 7, BytesMoved: 2048}}
	do, _ := newRBHarness(t, fr)
	status, body := do("GET", "/rebalance")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var s object.RebalanceStatus
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if s.ObjectsMoved != 7 || s.BytesMoved != 2048 {
		t.Fatalf("unexpected status: %+v", s)
	}
}

// TestStopRebalanceReturns204 verifies DELETE /rebalance returns 204.
func TestStopRebalanceReturns204(t *testing.T) {
	fr := &fakeRebalancer{}
	do, _ := newRBHarness(t, fr)
	status, _ := do("DELETE", "/rebalance")
	if status != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", status)
	}
}

// TestStartDecommissionReturns202 verifies POST /decommission?pool=1 responds 202.
func TestStartDecommissionReturns202(t *testing.T) {
	fr := &fakeRebalancer{}
	do, _ := newRBHarness(t, fr)
	status, body := do("POST", "/decommission?pool=1")
	if status != http.StatusAccepted {
		t.Fatalf("expected 202, got %d: %s", status, body)
	}
}

// TestGetDecommissionStatus verifies GET /decommission?pool=0 returns JSON.
func TestGetDecommissionStatus(t *testing.T) {
	fr := &fakeRebalancer{dcStatus: object.DecommissionStatus{Done: true, ObjectsMoved: 10}}
	do, _ := newRBHarness(t, fr)
	status, body := do("GET", "/decommission?pool=0")
	if status != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", status, body)
	}
	var s object.DecommissionStatus
	if err := json.Unmarshal(body, &s); err != nil {
		t.Fatalf("parse body: %v", err)
	}
	if !s.Done || s.ObjectsMoved != 10 {
		t.Fatalf("unexpected status: %+v", s)
	}
}

// TestStopDecommissionReturns204 verifies DELETE /decommission?pool=0 returns 204.
func TestStopDecommissionReturns204(t *testing.T) {
	fr := &fakeRebalancer{}
	do, _ := newRBHarness(t, fr)
	status, _ := do("DELETE", "/decommission?pool=0")
	if status != http.StatusNoContent {
		t.Fatalf("expected 204, got %d", status)
	}
}

// TestDecommissionMissingPool verifies that a missing ?pool param returns 400.
func TestDecommissionMissingPool(t *testing.T) {
	fr := &fakeRebalancer{}
	do, _ := newRBHarness(t, fr)
	status, body := do("POST", "/decommission")
	if status != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d: %s", status, body)
	}
}

// TestRebalancerNotConfigured verifies that a server with no rebalancer returns 501.
func TestRebalancerNotConfigured(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }))
	ts := httptest.NewServer(s)
	defer ts.Close()

	for _, tc := range []struct{ method, path string }{
		{"GET", "/rebalance"},
		{"GET", "/decommission?pool=0"},
	} {
		req, _ := http.NewRequest(tc.method, ts.URL+APIPrefix+tc.path, nil)
		sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", tc.method, tc.path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusNotImplemented {
			t.Errorf("%s %s: expected 501, got %d", tc.method, tc.path, resp.StatusCode)
		}
	}
}
