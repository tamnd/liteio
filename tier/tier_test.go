// SPDX-License-Identifier: Apache-2.0

package tier_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tamnd/liteio/tier"
)

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		cfg     tier.TierConfig
		wantErr bool
	}{
		{
			name: "valid S3",
			cfg:  tier.TierConfig{Name: "cold", Type: tier.TierTypeS3, S3: &tier.S3Config{Endpoint: "http://s3.local", Bucket: "tier-bucket"}},
		},
		{
			name:    "missing name",
			cfg:     tier.TierConfig{Type: tier.TierTypeS3, S3: &tier.S3Config{Endpoint: "http://s3.local", Bucket: "b"}},
			wantErr: true,
		},
		{
			name:    "S3 missing bucket",
			cfg:     tier.TierConfig{Name: "x", Type: tier.TierTypeS3, S3: &tier.S3Config{Endpoint: "http://s3.local"}},
			wantErr: true,
		},
		{
			name:    "unknown type",
			cfg:     tier.TierConfig{Name: "x", Type: "redis"},
			wantErr: true,
		},
		{
			name:    "azure not supported yet",
			cfg:     tier.TierConfig{Name: "x", Type: tier.TierTypeAzure},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tier.Validate(tt.cfg)
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestRemoteKeyFormat(t *testing.T) {
	tests := []struct {
		prefix, bucket, object, versionID string
		want                              string
	}{
		{"", "b", "k.txt", "", "b/k.txt"},
		{"", "b", "k.txt", "v1", "b/v1/k.txt"},
		{"pfx/", "b", "deep/k.txt", "v2", "pfx/b/v2/deep/k.txt"},
		{"pfx", "b", "k.txt", "", "pfx/b/k.txt"},
	}
	for _, tt := range tests {
		got := tier.RemoteKey(tt.prefix, tt.bucket, tt.object, tt.versionID)
		if got != tt.want {
			t.Errorf("RemoteKey(%q,%q,%q,%q) = %q, want %q", tt.prefix, tt.bucket, tt.object, tt.versionID, got, tt.want)
		}
	}
}

func TestS3ClientRoundTrip(t *testing.T) {
	const body = "hello tier content"
	var receivedKey, receivedBody string
	var received bool

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPut:
			receivedKey = strings.TrimPrefix(r.URL.Path, "/tier-bucket/")
			b, _ := io.ReadAll(r.Body)
			receivedBody = string(b)
			received = true
			w.WriteHeader(http.StatusOK)
		case http.MethodGet:
			if !received {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Length", "18")
			w.WriteHeader(http.StatusOK)
			w.Write([]byte(receivedBody)) //nolint:errcheck
		case http.MethodDelete:
			received = false
			w.WriteHeader(http.StatusNoContent)
		}
	}))
	defer srv.Close()

	cfg := tier.TierConfig{
		Name: "test",
		Type: tier.TierTypeS3,
		S3: &tier.S3Config{
			Endpoint:  srv.URL,
			Region:    "us-east-1",
			Bucket:    "tier-bucket",
			AccessKey: "test",
			SecretKey: "test1234",
		},
	}
	client, err := tier.NewClient(cfg)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	ctx := context.Background()

	// PUT
	if err := client.Put(ctx, "foo/bar.txt", strings.NewReader(body), int64(len(body))); err != nil {
		t.Fatalf("Put: %v", err)
	}
	if receivedKey != "foo/bar.txt" {
		t.Errorf("PUT key = %q, want %q", receivedKey, "foo/bar.txt")
	}
	if receivedBody != body {
		t.Errorf("PUT body = %q, want %q", receivedBody, body)
	}

	// GET
	rc, size, err := client.Get(ctx, "foo/bar.txt")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close() //nolint:errcheck
	got, _ := io.ReadAll(rc)
	if string(got) != body {
		t.Errorf("GET body = %q, want %q", got, body)
	}
	_ = size

	// DELETE
	if err := client.Remove(ctx, "foo/bar.txt"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
}
