// SPDX-License-Identifier: Apache-2.0

package object

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	tierpkg "github.com/tamnd/liteio/tier"
)

// mustMakeTierSP builds a 4-drive/parity-2 ServerPools for tier tests.
func mustMakeTierSP(t *testing.T) *ServerPools {
	t.Helper()
	return newLayer(t, 4, 2)
}

// newTierServer returns an httptest Server that acts as a minimal S3-compatible
// tier backend. It stores a single object so round-trip tests can verify
// TransitionObject + GetObject (tiered).
func newTierServer(t *testing.T) (*httptest.Server, *sync.Map) {
	t.Helper()
	var store sync.Map // key -> []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := strings.TrimPrefix(r.URL.Path, "/tier-bucket/")
		switch r.Method {
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			store.Store(key, b)
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			v, ok := store.Load(key)
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			data := v.([]byte)
			w.WriteHeader(http.StatusOK)
			w.Write(data) //nolint:errcheck
		case http.MethodDelete:
			store.Delete(key)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	}))
	return srv, &store
}

func TestTierConfigCRUD(t *testing.T) {
	sp := mustMakeTierSP(t)
	ctx := context.Background()

	cfg := tierpkg.TierConfig{
		Name: "cold",
		Type: tierpkg.TierTypeS3,
		S3: &tierpkg.S3Config{
			Endpoint:  "http://s3.local",
			Region:    "us-east-1",
			Bucket:    "cold-bucket",
			AccessKey: "ak",
			SecretKey: "sk",
		},
	}

	// Set
	if err := sp.SetTierConfig(ctx, cfg); err != nil {
		t.Fatalf("SetTierConfig: %v", err)
	}

	// Get
	got, err := sp.GetTierConfig(ctx, "cold")
	if err != nil {
		t.Fatalf("GetTierConfig: %v", err)
	}
	if got.Name != "cold" || got.S3.Bucket != "cold-bucket" {
		t.Errorf("GetTierConfig round-trip failed: %+v", got)
	}

	// List
	cfgs, err := sp.ListTierConfigs(ctx)
	if err != nil {
		t.Fatalf("ListTierConfigs: %v", err)
	}
	if len(cfgs) != 1 || cfgs[0].Name != "cold" {
		t.Errorf("ListTierConfigs: got %+v", cfgs)
	}

	// Replace
	cfg.S3.Bucket = "cold-bucket-2"
	if err := sp.SetTierConfig(ctx, cfg); err != nil {
		t.Fatalf("SetTierConfig replace: %v", err)
	}
	got, _ = sp.GetTierConfig(ctx, "cold")
	if got.S3.Bucket != "cold-bucket-2" {
		t.Errorf("replace: bucket = %q, want %q", got.S3.Bucket, "cold-bucket-2")
	}

	// Delete
	if err := sp.DeleteTierConfig(ctx, "cold"); err != nil {
		t.Fatalf("DeleteTierConfig: %v", err)
	}
	if _, err := sp.GetTierConfig(ctx, "cold"); err != ErrNoSuchTierConfig {
		t.Errorf("after delete: want ErrNoSuchTierConfig, got %v", err)
	}

	// List after delete returns empty (not error)
	cfgs, err = sp.ListTierConfigs(ctx)
	if err != nil {
		t.Fatalf("ListTierConfigs after delete: %v", err)
	}
	if len(cfgs) != 0 {
		t.Errorf("ListTierConfigs after delete: want 0, got %d", len(cfgs))
	}
}

func TestTransitionObjectAndTransparentGet(t *testing.T) {
	srv, _ := newTierServer(t)
	defer srv.Close()

	sp := mustMakeTierSP(t)
	ctx := context.Background()

	// Register the tier using the httptest server URL.
	tierCfg := tierpkg.TierConfig{
		Name: "warm",
		Type: tierpkg.TierTypeS3,
		S3: &tierpkg.S3Config{
			Endpoint:  srv.URL,
			Region:    "us-east-1",
			Bucket:    "tier-bucket",
			AccessKey: "ak",
			SecretKey: "sk",
		},
	}
	if err := sp.SetTierConfig(ctx, tierCfg); err != nil {
		t.Fatalf("SetTierConfig: %v", err)
	}

	// Write a local object.
	bucket := "testbucket"
	mustMakeBucket(t, sp, bucket)
	const content = "cold object content"
	oi := mustPut(t, sp, bucket, "cold/obj.txt", content)
	_ = oi

	// Transition to tier.
	if err := sp.TransitionObject(ctx, bucket, "cold/obj.txt", "warm", ObjectOptions{}); err != nil {
		t.Fatalf("TransitionObject: %v", err)
	}

	// GetObjectInfo should show Tier set.
	info, err := sp.GetObjectInfo(ctx, bucket, "cold/obj.txt", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo after transition: %v", err)
	}
	if info.Tier != "warm" {
		t.Errorf("Tier = %q, want %q", info.Tier, "warm")
	}
	if info.TierKey == "" {
		t.Errorf("TierKey empty after transition")
	}

	// GetObject should transparently fetch from tier.
	reader, err := sp.GetObject(ctx, bucket, "cold/obj.txt", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject (tiered): %v", err)
	}
	defer reader.Close() //nolint:errcheck
	got, _ := io.ReadAll(reader)
	if string(got) != content {
		t.Errorf("GetObject (tiered): body = %q, want %q", got, content)
	}
}

func TestGetObject_NonTieredUnchanged(t *testing.T) {
	sp := mustMakeTierSP(t)
	ctx := context.Background()
	bucket := "testbucket"
	mustMakeBucket(t, sp, bucket)

	const content = "local only content"
	mustPut(t, sp, bucket, "local.txt", content)

	reader, err := sp.GetObject(ctx, bucket, "local.txt", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer reader.Close() //nolint:errcheck
	got, _ := io.ReadAll(reader)
	if string(got) != content {
		t.Errorf("body = %q, want %q", got, content)
	}
}

func TestRestoreObject_NotTiered(t *testing.T) {
	sp := mustMakeTierSP(t)
	ctx := context.Background()
	bucket := "testbucket"
	mustMakeBucket(t, sp, bucket)
	mustPut(t, sp, bucket, "obj.txt", "data")

	err := sp.RestoreObject(ctx, bucket, "obj.txt", "", 7)
	if err != ErrNotTiered {
		t.Errorf("RestoreObject on local object: want ErrNotTiered, got %v", err)
	}
}

// mustPut writes content to bucket/key and returns the ObjectInfo.
func mustPut(t *testing.T, sp *ServerPools, bucket, key, content string) ObjectInfo {
	t.Helper()
	pr := NewPutReader(strings.NewReader(content), int64(len(content)))
	oi, err := sp.PutObject(context.Background(), bucket, key, pr, ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject(%q): %v", key, err)
	}
	return oi
}
