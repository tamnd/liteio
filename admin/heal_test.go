// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
)

// fakeHealer is a stub HealLayer for tests.
type fakeHealer struct {
	stats object.MRFStats
}

func (f *fakeHealer) MRFStats() object.MRFStats { return f.stats }

// TestGetHealStatus verifies the handler returns the correct JSON counters.
func TestGetHealStatus(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	healer := &fakeHealer{stats: object.MRFStats{Pending: 3, Dropped: 1, Healed: 42, Failed: 2}}
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithHealer(healer),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/heal", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var got healStatusResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Pending != 3 || got.Dropped != 1 || got.Healed != 42 || got.Failed != 2 {
		t.Errorf("unexpected stats: %+v", got)
	}
}

// TestHealNotConfigured501 verifies the handler returns 501 when no healer is wired.
func TestHealNotConfigured501(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }))
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/heal", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
