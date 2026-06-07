// SPDX-License-Identifier: Apache-2.0

package cluster_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"

	"github.com/tamnd/liteio/cluster"
	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
	"github.com/tamnd/liteio/storage/remote"
)

func newDeploymentID(t *testing.T) [16]byte {
	t.Helper()
	id, err := cluster.NewDeploymentID()
	if err != nil {
		t.Fatalf("NewDeploymentID: %v", err)
	}
	return id
}

func sampleRecord(t *testing.T) cluster.Format {
	t.Helper()
	id := newDeploymentID(t)
	layout := cluster.Layout{DeploymentID: id, SetSize: 16}
	layout.Sets = [][]cluster.Endpoint{make([]cluster.Endpoint, 1)}
	return layout.Formats(0)[0][0]
}

func TestFormatLocalDriveFirstWriteAndRejoin(t *testing.T) {
	dir := t.TempDir()
	want := sampleRecord(t)

	got, err := cluster.FormatLocalDrive(dir, want)
	if err != nil {
		t.Fatalf("first format: %v", err)
	}
	if got != want {
		t.Fatalf("first format returned %+v, want %+v", got, want)
	}
	// The record must now be on disk.
	if _, err := cluster.ParseFormat(readFile(t, filepath.Join(dir, cluster.FormatFile))); err != nil {
		t.Fatalf("format.json not written: %v", err)
	}
	// A second bring-up of the same drive with the same record is a no-op rejoin.
	again, err := cluster.FormatLocalDrive(dir, want)
	if err != nil {
		t.Fatalf("rejoin: %v", err)
	}
	if again != want {
		t.Fatalf("rejoin returned %+v, want %+v", again, want)
	}
}

func TestFormatLocalDriveRejectsForeignDeployment(t *testing.T) {
	dir := t.TempDir()
	first := sampleRecord(t)
	if _, err := cluster.FormatLocalDrive(dir, first); err != nil {
		t.Fatalf("first format: %v", err)
	}
	// Same drive, different deployment: must be refused, not re-formatted.
	other := first
	other.DeploymentID = sampleRecord(t).DeploymentID
	if _, err := cluster.FormatLocalDrive(dir, other); err == nil {
		t.Fatal("a drive from another deployment must be refused")
	}
}

func TestFormatLocalDriveRejectsMovedCoordinates(t *testing.T) {
	dir := t.TempDir()
	first := sampleRecord(t)
	if _, err := cluster.FormatLocalDrive(dir, first); err != nil {
		t.Fatalf("first format: %v", err)
	}
	moved := first
	moved.Set = first.Set + 1 // same deployment, but the layout puts it elsewhere
	if _, err := cluster.FormatLocalDrive(dir, moved); err == nil {
		t.Fatal("a drive that moved set must be refused")
	}
	mismatch := first
	mismatch.SetSize = first.SetSize + 1
	if _, err := cluster.FormatLocalDrive(dir, mismatch); err == nil {
		t.Fatal("a drive with a different set size must be refused")
	}
}

// TestBringUpSingleNodeLocal brings up an all-local pool and round-trips an
// object through the assembled object layer.
func TestBringUpSingleNodeLocal(t *testing.T) {
	base := t.TempDir()
	eps, err := cluster.ParsePattern([]string{filepath.Join(base, "disk{1...6}")})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	id := newDeploymentID(t)
	sp, layouts, err := cluster.BringUp(id, []cluster.PoolSpec{{Endpoints: eps, Parity: 2}}, cluster.Opener{}, cluster.Membership{})
	if err != nil {
		t.Fatalf("BringUp: %v", err)
	}
	if len(layouts) != 1 || len(layouts[0].Sets) != 1 || layouts[0].SetSize != 6 {
		t.Fatalf("layout = %+v, want one pool, one set of 6", layouts)
	}
	// Every local drive must now carry a format.json.
	for i := 1; i <= 6; i++ {
		path := filepath.Join(base, "disk"+strconv.Itoa(i), cluster.FormatFile)
		if _, err := cluster.ParseFormat(readFile(t, path)); err != nil {
			t.Fatalf("drive %d not formatted: %v", i, err)
		}
	}
	roundTrip(t, sp)
}

// TestBringUpOverRemoteDrives is the keystone: a pool whose every drive is a
// remote endpoint served by another "node" (an httptest server wrapping a local
// drive over cluster/rpc). BringUp dials them all, and an object round-trips
// through the assembled layer byte for byte, proving the bring-up path stitches
// a multi-node deployment together.
func TestBringUpOverRemoteDrives(t *testing.T) {
	const nodes, drivesPerNode = 2, 3 // 6 drives -> set size 6, 1 set
	var eps []cluster.Endpoint
	for range nodes {
		baseURL, prefixes := serveNode(t, drivesPerNode)
		for _, prefix := range prefixes {
			e, err := parseOne(t, baseURL+prefix)
			if err != nil {
				t.Fatalf("endpoint: %v", err)
			}
			eps = append(eps, e)
		}
	}

	id := newDeploymentID(t)
	// Every endpoint is remote to this node; dial with a plain client (no TLS in
	// the test). No local drive is formatted because none is owned here.
	opener := cluster.Opener{
		Local:  func(cluster.Endpoint) bool { return false },
		Client: http.DefaultClient,
	}
	sp, layouts, err := cluster.BringUp(id, []cluster.PoolSpec{{Endpoints: eps, Parity: 2}}, opener, cluster.Membership{})
	if err != nil {
		t.Fatalf("BringUp over remote drives: %v", err)
	}
	if len(layouts[0].Sets[0]) != 6 {
		t.Fatalf("set has %d drives, want 6", len(layouts[0].Sets[0]))
	}
	roundTrip(t, sp)
}

// TestBringUpWithDistributedLock wires a real lock quorum into the object layer
// through Membership and proves the namespace lock is taken across it: a burst of
// concurrent writers to one key leaves a clean, untorn object, which can only
// hold if every PutObject acquired the cluster lock (over RPC to the peer
// lockers) before committing.
func TestBringUpWithDistributedLock(t *testing.T) {
	// Three lock authorities; node 0 is us, nodes 1 and 2 are peers we reach over
	// RPC. Each node's Server also serves a throwaway drive (NewServer needs one).
	var bases []string
	var self *lock.LocalLocker
	for i := range 3 {
		srv, locker := serveLockNode(t)
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		if i == 0 {
			self = locker
		} else {
			bases = append(bases, hs.URL)
		}
	}
	quorum := cluster.LockQuorum(self, bases, http.DefaultClient)

	base := t.TempDir()
	eps, err := cluster.ParsePattern([]string{filepath.Join(base, "disk{1...4}")})
	if err != nil {
		t.Fatalf("ParsePattern: %v", err)
	}
	id := newDeploymentID(t)
	sp, _, err := cluster.BringUp(id, []cluster.PoolSpec{{Endpoints: eps, Parity: 1}},
		cluster.Opener{}, cluster.Membership{NodeID: "node0", Lockers: quorum})
	if err != nil {
		t.Fatalf("BringUp: %v", err)
	}

	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "b", object.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	const writers = 12
	bodies := make([][]byte, writers)
	var wg sync.WaitGroup
	for w := range writers {
		body := bytes.Repeat([]byte{byte('A' + w)}, 70*1024)
		bodies[w] = body
		wg.Go(func() {
			_, _ = sp.PutObject(ctx, "b", "hot",
				object.NewPutReader(bytes.NewReader(body), int64(len(body))), object.ObjectOptions{})
		})
	}
	wg.Wait()

	got := getOne(t, sp)
	for _, body := range bodies {
		if bytes.Equal(got, body) {
			return // the final object is exactly one writer's payload, never a mix
		}
	}
	t.Fatalf("final object (%d bytes, first byte %q) matched no single writer's body", len(got), got[0])
}

// serveLockNode is a node whose Server exists mainly for its lock endpoint; it
// carries one throwaway drive because NewServer needs a valid drive config.
func serveLockNode(t *testing.T) (*cluster.Server, *lock.LocalLocker) {
	t.Helper()
	d, err := local.New(t.TempDir())
	if err != nil {
		t.Fatalf("local: %v", err)
	}
	locker := lock.NewLocalLocker("n")
	srv, err := cluster.NewServer(map[string]storage.StorageAPI{"/d0": d}, locker)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, locker
}

func getOne(t *testing.T, sp *object.ServerPools) []byte {
	t.Helper()
	r, err := sp.GetObject(context.Background(), "b", "hot", object.ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return got
}

// serveNode stands up one node: a parent mux hosting n local drives, each under
// its own /driveK prefix over cluster/rpc. It returns the server base URL and
// the drive prefixes mounted on it. Prefixes are local to the node; the host:port
// in the base URL keeps endpoints across nodes distinct.
func serveNode(t *testing.T, n int) (string, []string) {
	t.Helper()
	parent := http.NewServeMux()
	prefixes := make([]string, n)
	for d := range n {
		l, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local drive: %v", err)
		}
		mux := rpc.NewMux()
		remote.Register(mux, l)
		// The endpoint path is the drive prefix; strip it so the drive's mux sees
		// the bare rpc path it registered under.
		prefix := "/drive" + strconv.Itoa(d)
		prefixes[d] = prefix
		parent.Handle(prefix+"/", http.StripPrefix(prefix, mux))
	}
	srv := httptest.NewServer(parent)
	t.Cleanup(srv.Close)
	return srv.URL, prefixes
}

func roundTrip(t *testing.T, sp *object.ServerPools) {
	t.Helper()
	ctx := context.Background()
	if err := sp.MakeBucket(ctx, "photos", object.MakeBucketOptions{}); err != nil {
		t.Fatalf("MakeBucket: %v", err)
	}
	body := make([]byte, 180*1024) // multi-shard
	if _, err := rand.Read(body); err != nil {
		t.Fatalf("rand: %v", err)
	}
	if _, err := sp.PutObject(ctx, "photos", "cat.jpg",
		object.NewPutReader(bytes.NewReader(body), int64(len(body))), object.ObjectOptions{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	r, err := sp.GetObject(ctx, "photos", "cat.jpg", object.ObjectOptions{})
	if err != nil {
		t.Fatalf("GetObject: %v", err)
	}
	defer func() { _ = r.Close() }()
	got, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("read object: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("round trip mismatch: got %d bytes, want %d", len(got), len(body))
	}
}

// --- small helpers ------------------------------------------------------------

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func parseOne(t *testing.T, s string) (cluster.Endpoint, error) {
	t.Helper()
	eps, err := cluster.ParsePattern([]string{s})
	if err != nil {
		return cluster.Endpoint{}, err
	}
	return eps[0], nil
}
