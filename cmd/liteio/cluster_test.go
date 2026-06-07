// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tamnd/liteio/cluster"
	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/object"
)

// TestBuildLayerSingleNode is the no-cluster path: a comma list of drive dirs
// builds one erasure set and no inter-node server.
func TestBuildLayerSingleNode(t *testing.T) {
	base := t.TempDir()
	dirs := make([]string, 4)
	for i := range dirs {
		dirs[i] = filepath.Join(base, "disk"+strconv.Itoa(i+1))
	}
	cfg := config{drives: strings.Join(dirs, ","), parity: 2, deploymentID: "t"}

	layer, srv, err := buildLayer(cfg)
	if err != nil {
		t.Fatalf("buildLayer: %v", err)
	}
	if srv != nil {
		t.Fatal("single-node mode must not build an inter-node server")
	}
	roundTrip(t, layer)
}

// TestBuildClusterLocalOnly exercises cluster mode with an all-local drive set
// and no peers: bring-up still routes through cluster.BringUp, the object layer
// round-trips, and the inter-node server actually serves this node's lock
// authority — a conflicting writer is refused while a lock is held.
func TestBuildClusterLocalOnly(t *testing.T) {
	base := t.TempDir()
	cfg := config{
		drives:         filepath.Join(base, "disk{1...4}"),
		parity:         1,
		deploymentID:   "t",
		clusterAddress: ":0", // enables cluster mode; the test serves the handler itself
		nodeHost:       "node-a",
	}

	layer, srv, err := buildLayer(cfg)
	if err != nil {
		t.Fatalf("buildLayer cluster: %v", err)
	}
	if srv == nil {
		t.Fatal("cluster mode must build an inter-node server")
	}
	roundTrip(t, layer)

	hs := httptest.NewServer(srv.Handler())
	defer hs.Close()
	rl := lock.NewRemoteLocker(hs.URL+cluster.LockPath, http.DefaultClient)
	if !rl.IsOnline() {
		t.Fatal("served lock endpoint reports offline")
	}
	ctx := context.Background()
	held, err := rl.Lock(ctx, lock.Args{UID: "1", Resources: []string{"b/k"}, Owner: "a"})
	if err != nil || !held {
		t.Fatalf("first lock not granted: held=%v err=%v", held, err)
	}
	conflict, err := rl.Lock(ctx, lock.Args{UID: "2", Resources: []string{"b/k"}, Owner: "b"})
	if err != nil {
		t.Fatalf("conflicting lock errored: %v", err)
	}
	if conflict {
		t.Fatal("a second writer was granted a held lock over the served endpoint")
	}
	if ok, err := rl.Unlock(ctx, lock.Args{UID: "1", Resources: []string{"b/k"}, Owner: "a"}); err != nil || !ok {
		t.Fatalf("unlock failed: ok=%v err=%v", ok, err)
	}
}

func TestBuildClusterRejectsMissingNodeHost(t *testing.T) {
	cfg := config{drives: "/d{1...4}", parity: 1, clusterAddress: ":0"}
	if _, _, err := buildLayer(cfg); err == nil {
		t.Fatal("cluster mode without --node-host must be rejected")
	}
}

func TestBuildClusterRejectsNoLocalDrives(t *testing.T) {
	// Every drive lives on another host, so this node owns none. The error must
	// fire before any peer is dialed.
	cfg := config{
		drives:         "http://other.host:9100/mnt/disk{1...4}",
		parity:         1,
		clusterAddress: ":0",
		nodeHost:       "node-a",
	}
	if _, _, err := buildLayer(cfg); err == nil {
		t.Fatal("a node owning no local drive must be rejected")
	}
}

func TestBuildClusterRejectsBadParity(t *testing.T) {
	cfg := config{drives: "/d{1...4}", parity: 0, clusterAddress: ":0", nodeHost: "node-a"}
	if _, _, err := buildLayer(cfg); err == nil {
		t.Fatal("parity below 1 must be rejected in cluster mode")
	}
}

func TestParseFlagsClusterDefaults(t *testing.T) {
	cfg, err := parseFlags([]string{
		"--drives", "/d1,/d2", "--parity", "1",
		"--cluster-address", ":9100", "--node-host", "n1.lan",
		"--peers", "https://n2.lan:9100,https://n3.lan:9100",
	})
	if err != nil {
		t.Fatalf("parseFlags: %v", err)
	}
	if cfg.clusterAddress != ":9100" || cfg.nodeHost != "n1.lan" {
		t.Fatalf("cluster flags not parsed: %+v", cfg)
	}
	if peers := splitNonEmpty(cfg.peers); len(peers) != 2 {
		t.Fatalf("peers = %v, want 2", peers)
	}
}

// BenchmarkBuildClusterLocalOnly measures the cost of assembling a cluster-mode
// node over four local drives: parse the endpoint pattern, format the drives,
// build the lock quorum, and stand up the object layer plus inter-node server.
func BenchmarkBuildClusterLocalOnly(b *testing.B) {
	for b.Loop() {
		dir := b.TempDir() // fresh drives each iteration so format.json is written cold
		cfg := config{
			drives:         filepath.Join(dir, "disk{1...4}"),
			parity:         1,
			deploymentID:   "bench",
			clusterAddress: ":0",
			nodeHost:       "node-a",
		}
		if _, _, err := buildLayer(cfg); err != nil {
			b.Fatalf("buildLayer: %v", err)
		}
	}
}

// roundTrip puts and gets one object through layer, asserting it comes back byte
// for byte.
func roundTrip(t *testing.T, layer object.ObjectLayer) {
	t.Helper()
	ctx := context.Background()
	if err := layer.MakeBucket(ctx, "photos", object.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	body := bytes.Repeat([]byte("liteio"), 20*1024) // ~120 KiB, multi-shard
	if _, err := layer.PutObject(ctx, "photos", "cat.jpg",
		object.NewPutReader(bytes.NewReader(body), int64(len(body))), object.ObjectOptions{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	r, err := layer.GetObject(ctx, "photos", "cat.jpg", object.ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = r.Close() }()
	var got bytes.Buffer
	if _, err := got.ReadFrom(r); err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(got.Bytes(), body) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", got.Len(), len(body))
	}
}
