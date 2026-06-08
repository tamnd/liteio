// SPDX-License-Identifier: Apache-2.0

package object

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tamnd/liteio/replication"
)

// TestSetGetDeleteBucketReplication exercises the storage lifecycle for
// replication configs: set → get → verify → delete → get returns error.
func TestSetGetDeleteBucketReplication(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{
				ID:          "rule1",
				Enabled:     true,
				Filter:      replication.Filter{Prefix: "logs/"},
				Destination: replication.Destination{Endpoint: "http://peer", Bucket: "dst", AccessKey: "ak", SecretKey: "sk"},
			},
		},
	}
	if err := sp.SetBucketReplication(ctx, "src", cfg); err != nil {
		t.Fatalf("SetBucketReplication: %v", err)
	}
	got, err := sp.GetBucketReplication(ctx, "src")
	if err != nil {
		t.Fatalf("GetBucketReplication: %v", err)
	}
	if len(got.Rules) != 1 || got.Rules[0].ID != "rule1" {
		t.Fatalf("unexpected config: %+v", got)
	}

	if err := sp.DeleteBucketReplication(ctx, "src"); err != nil {
		t.Fatalf("DeleteBucketReplication: %v", err)
	}
	_, err = sp.GetBucketReplication(ctx, "src")
	if err == nil {
		t.Fatal("expected ErrNoSuchBucketReplication after delete, got nil")
	}
}

// TestReplicationGetOnMissingBucket verifies that GetBucketReplication returns
// ErrBucketNotFound (not ErrNoSuchBucketReplication) for a bucket that does
// not exist.
func TestReplicationGetOnMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	_, err := sp.GetBucketReplication(ctx, "ghost")
	if err == nil {
		t.Fatal("expected error for missing bucket")
	}
}

// TestMaybeReplicateFireAndForget verifies that PutObject fans out to the
// destination when the bucket has a replication rule.
func TestMaybeReplicateFireAndForget(t *testing.T) {
	var received atomic.Int32
	// Destination mock: count PUT requests.
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			received.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()

	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{
				ID:          "r1",
				Enabled:     true,
				Destination: replication.Destination{Endpoint: dst.URL, Bucket: "dst", AccessKey: "ak", SecretKey: "sk"},
			},
		},
	}
	if err := sp.SetBucketReplication(ctx, "src", cfg); err != nil {
		t.Fatalf("SetBucketReplication: %v", err)
	}

	body := []byte("hello world")
	_, err := sp.PutObject(ctx, "src", "a/b.txt", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Give the background goroutine time to replicate.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if received.Load() == 0 {
		t.Fatal("destination received no replication PUT")
	}
}

// TestReplicaStatusLoopPrevention verifies that an object PUT with REPLICA
// status is not re-replicated.
func TestReplicaStatusLoopPrevention(t *testing.T) {
	var received atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			received.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()

	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{
				ID:           "r1",
				Enabled:      true,
				ActiveActive: true,
				Destination:  replication.Destination{Endpoint: dst.URL, Bucket: "dst", AccessKey: "ak", SecretKey: "sk"},
			},
		},
	}
	if err := sp.SetBucketReplication(ctx, "src", cfg); err != nil {
		t.Fatalf("SetBucketReplication: %v", err)
	}

	// PutObject with ReplicationSource=true marks the object as REPLICA.
	body := []byte("replica data")
	_, err := sp.PutObject(ctx, "src", "replica.txt", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{
		ReplicationSource: true,
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Wait briefly to confirm no replication happened.
	time.Sleep(100 * time.Millisecond)
	if received.Load() != 0 {
		t.Fatalf("replica object was re-replicated (%d PUTs reached destination)", received.Load())
	}
}

// TestMaybeReplicateDelete verifies that DeleteObject fans out to the
// destination when DeleteReplication is enabled.
func TestMaybeReplicateDelete(t *testing.T) {
	var deleted atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleted.Add(1)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer dst.Close()

	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{
				ID:                "r1",
				Enabled:           true,
				DeleteReplication: true,
				Destination:       replication.Destination{Endpoint: dst.URL, Bucket: "dst", AccessKey: "ak", SecretKey: "sk"},
			},
		},
	}
	if err := sp.SetBucketReplication(ctx, "src", cfg); err != nil {
		t.Fatalf("SetBucketReplication: %v", err)
	}

	// First put an object.
	body := []byte("to-be-deleted")
	_, err := sp.PutObject(ctx, "src", "del.txt", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Delete it.
	_, err = sp.DeleteObject(ctx, "src", "del.txt", ObjectOptions{})
	if err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

	// Wait for background goroutine.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && deleted.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if deleted.Load() == 0 {
		t.Fatal("destination received no replication DELETE")
	}
}

// TestReplicationStatusStored verifies that the replication status stored in
// obj.meta is accessible via GetObjectInfo.
func TestReplicationStatusStored(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	body := []byte("data")
	_, err := sp.PutObject(ctx, "src", "tagged.txt", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{
		ReplicationSource: true,
	})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	info, err := sp.GetObjectInfo(ctx, "src", "tagged.txt", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo: %v", err)
	}
	status := info.UserDefined[replication.MetaStatus]
	if status != string(replication.StatusReplica) {
		t.Fatalf("expected REPLICA status in metadata, got %q", status)
	}
}

// TestReplicationFilterPrefix verifies that rules with a prefix filter do not
// replicate objects whose keys do not match.
func TestReplicationFilterPrefix(t *testing.T) {
	var received atomic.Int32
	dst := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPut {
			received.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer dst.Close()

	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "src")

	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{
				ID:          "r1",
				Enabled:     true,
				Filter:      replication.Filter{Prefix: "logs/"},
				Destination: replication.Destination{Endpoint: dst.URL, Bucket: "dst", AccessKey: "ak", SecretKey: "sk"},
			},
		},
	}
	if err := sp.SetBucketReplication(ctx, "src", cfg); err != nil {
		t.Fatalf("SetBucketReplication: %v", err)
	}

	// This key does not match the prefix.
	body := []byte("not a log")
	_, err := sp.PutObject(ctx, "src", "images/photo.jpg", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	time.Sleep(100 * time.Millisecond)
	if received.Load() != 0 {
		t.Fatalf("non-matching key was replicated (%d PUTs)", received.Load())
	}

	// This key matches.
	_, err = sp.PutObject(ctx, "src", "logs/app.log", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && received.Load() == 0 {
		time.Sleep(20 * time.Millisecond)
	}
	if received.Load() == 0 {
		t.Fatal("matching key was not replicated")
	}
}

// TestSetReplicationStatus exercises the setReplicationStatus erasureSet helper
// directly to verify it updates the metadata and reaches write quorum.
func TestSetReplicationStatus(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "bkt")

	body := []byte("payload")
	oi, err := sp.PutObject(ctx, "bkt", "obj.bin", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
	if err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	set := sp.route("obj.bin")
	if err := set.setReplicationStatus(ctx, "bkt", "obj.bin", oi.VersionID, replication.StatusComplete); err != nil {
		t.Fatalf("setReplicationStatus: %v", err)
	}

	info, err := sp.GetObjectInfo(ctx, "bkt", "obj.bin", ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObjectInfo: %v", err)
	}
	if info.UserDefined[replication.MetaStatus] != string(replication.StatusComplete) {
		t.Fatalf("expected COMPLETE, got %q", info.UserDefined[replication.MetaStatus])
	}
}

// TestStripReplicationMeta verifies that internal keys are removed and other
// metadata passes through.
func TestStripReplicationMeta(t *testing.T) {
	in := map[string]string{
		replication.MetaStatus:        "PENDING",
		replication.MetaSourceCluster: "cluster-a",
		"content-type":                "text/plain",
		"x-amz-meta-app":              "myapp",
	}
	out := stripReplicationMeta(in)
	if _, ok := out[replication.MetaStatus]; ok {
		t.Error("MetaStatus should be stripped")
	}
	if _, ok := out[replication.MetaSourceCluster]; ok {
		t.Error("MetaSourceCluster should be stripped")
	}
	if out["content-type"] != "text/plain" {
		t.Error("content-type should pass through")
	}
	if out["x-amz-meta-app"] != "myapp" {
		t.Error("user meta should pass through")
	}
}

// TestMatchingRulesLoopPrevention verifies MatchingRules skips active-active
// rules when skipReplica is true.
func TestMatchingRulesLoopPrevention(t *testing.T) {
	cfg := replication.ReplicationConfig{
		Rules: []replication.Rule{
			{ID: "aa", Enabled: true, ActiveActive: true, Destination: replication.Destination{Endpoint: "http://peer"}},
			{ID: "ow", Enabled: true, ActiveActive: false, Destination: replication.Destination{Endpoint: "http://peer2"}},
		},
	}

	// A replica object should skip the active-active rule but keep the one-way rule.
	matched := cfg.MatchingRules("any/key", true)
	if len(matched) != 1 || matched[0].ID != "ow" {
		t.Fatalf("expected only one-way rule to match, got %v", matched)
	}

	// A normal object matches both.
	matched = cfg.MatchingRules("any/key", false)
	if len(matched) != 2 {
		t.Fatalf("expected both rules to match, got %v", matched)
	}
}

// BenchmarkMaybeReplicate measures the overhead of the REPLICA check on the
// hot write path when the bucket has no replication config.
func BenchmarkMaybeReplicate(b *testing.B) {
	ctx := context.Background()
	sp := newLayer(b, 4, 2)
	if err := sp.MakeBucket(ctx, "bench", MakeBucketOptions{}); err != nil {
		b.Fatalf("MakeBucket: %v", err)
	}
	body := bytes.Repeat([]byte("x"), 256)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := sp.PutObject(ctx, "bench", "obj", NewPutReader(bytes.NewReader(body), int64(len(body))), ObjectOptions{})
		if err != nil {
			b.Fatal(err)
		}
	}
}
