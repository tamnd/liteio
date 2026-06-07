// SPDX-License-Identifier: Apache-2.0

package cluster_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/liteio/cluster"
	"github.com/tamnd/liteio/cluster/lock"
	"github.com/tamnd/liteio/cluster/mtls"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
	"github.com/tamnd/liteio/storage/remote"
)

// newServerNode builds a Server hosting n local drives under /driveK plus a lock
// authority, and returns it along with the lock authority it serves (so a quorum
// can use the very same locker as this node's local entry).
func newServerNode(t *testing.T, n int, lockerName string) (*cluster.Server, *lock.LocalLocker) {
	t.Helper()
	drives := make(map[string]storage.StorageAPI, n)
	for d := range n {
		l, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local drive: %v", err)
		}
		drives["/drive"+itoa(d)] = l
	}
	locker := lock.NewLocalLocker(lockerName)
	srv, err := cluster.NewServer(drives, locker)
	if err != nil {
		t.Fatalf("NewServer: %v", err)
	}
	return srv, locker
}

func itoa(i int) string { return string(rune('0' + i)) }

func TestServerServesDrivesAndIsolatesThem(t *testing.T) {
	srv, _ := newServerNode(t, 2, "node-a")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	d0 := remote.NewStorage(hs.URL+"/drive0", hs.Client())
	d1 := remote.NewStorage(hs.URL+"/drive1", hs.Client())
	ctx := context.Background()

	// A volume made on drive0 must not appear on drive1: the two paths address
	// two independent backing drives.
	if err := d0.MakeVol(ctx, "bucket"); err != nil {
		t.Fatalf("MakeVol on drive0: %v", err)
	}
	if _, err := d1.StatVol(ctx, "bucket"); err == nil {
		t.Fatal("volume leaked from drive0 to drive1")
	}

	// Round-trip obj.meta through drive0 to prove the served drive is functional.
	want := bytes.Repeat([]byte("liteio"), 4096)
	if err := d0.WriteMeta(ctx, "bucket", "obj", want); err != nil {
		t.Fatalf("WriteMeta: %v", err)
	}
	got, err := d0.ReadMeta(ctx, "bucket", "obj")
	if err != nil {
		t.Fatalf("ReadMeta: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("meta round trip mismatch: %d vs %d bytes", len(got), len(want))
	}
}

func TestServerServesLockEndpoint(t *testing.T) {
	srv, _ := newServerNode(t, 1, "node-a")
	hs := httptest.NewServer(srv.Handler())
	t.Cleanup(hs.Close)

	rl := lock.NewRemoteLocker(hs.URL+cluster.LockPath, hs.Client())
	if !rl.IsOnline() {
		t.Fatal("lock endpoint reports offline")
	}
	ctx := context.Background()
	args := lock.Args{UID: "u1", Resources: []string{"bucket/key"}, Owner: "node-a"}
	ok, err := rl.Lock(ctx, args)
	if err != nil || !ok {
		t.Fatalf("Lock = %v, %v; want true", ok, err)
	}
	// A conflicting writer must be refused while the first is held.
	if ok, _ := rl.Lock(ctx, lock.Args{UID: "u2", Resources: []string{"bucket/key"}, Owner: "node-b"}); ok {
		t.Fatal("second writer granted while the first holds the lock")
	}
	if ok, err := rl.Unlock(ctx, args); err != nil || !ok {
		t.Fatalf("Unlock = %v, %v", ok, err)
	}
}

func TestNewServerRejectsBadConfig(t *testing.T) {
	l, _ := local.New(t.TempDir())
	if _, err := cluster.NewServer(map[string]storage.StorageAPI{"/d": l}, nil); err == nil {
		t.Fatal("NewServer must require a lock authority")
	}
	bad := map[string]storage.StorageAPI{cluster.LockPath: l}
	if _, err := cluster.NewServer(bad, lock.NewLocalLocker("n")); err == nil {
		t.Fatal("a drive path that collides with the lock endpoint must be rejected")
	}
	empty := map[string]storage.StorageAPI{"": l}
	if _, err := cluster.NewServer(empty, lock.NewLocalLocker("n")); err == nil {
		t.Fatal("an empty drive path must be rejected")
	}
}

// TestServerOverMutualTLS confirms the node handler composes with the cluster's
// mutual-TLS configs: a peer presenting a cluster cert can call a drive, and the
// transport is the secured one.
func TestServerOverMutualTLS(t *testing.T) {
	dir := t.TempDir()
	caCert, caKey := newCA(t)
	srvCertPEM, srvKeyPEM := leaf(t, caCert, caKey, []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	cliCertPEM, cliKeyPEM := leaf(t, caCert, caKey, nil, nil)
	caFile := writePEM(t, dir, "ca.pem", caCert.Raw, "CERTIFICATE")
	srvCertFile := writeRaw(t, dir, "srv-cert.pem", srvCertPEM)
	srvKeyFile := writeRaw(t, dir, "srv-key.pem", srvKeyPEM)
	cliCertFile := writeRaw(t, dir, "cli-cert.pem", cliCertPEM)
	cliKeyFile := writeRaw(t, dir, "cli-key.pem", cliKeyPEM)

	srv, _ := newServerNode(t, 1, "node-a")
	srvCfg, err := mtls.ServerConfig(srvCertFile, srvKeyFile, caFile)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Discard the expected "bad certificate" handshake log from the no-cert
	// sub-case below so it does not clutter the test output.
	hsrv := &http.Server{Handler: srv.Handler(), TLSConfig: srvCfg, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = hsrv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = hsrv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	baseURL := "https://localhost:" + port

	client, err := mtls.NewClient(cliCertFile, cliKeyFile, caFile, "localhost")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	d := remote.NewStorage(baseURL+"/drive0", client)
	if err := d.MakeVol(context.Background(), "secure"); err != nil {
		t.Fatalf("MakeVol over mTLS: %v", err)
	}

	// A client with no certificate must be turned away at the handshake.
	plain := remote.NewStorage(baseURL+"/drive0", http.DefaultClient)
	if err := plain.MakeVol(context.Background(), "nope"); err == nil {
		t.Fatal("server accepted a client with no cluster certificate")
	}
}

// TestLockQuorumAcrossNodes builds a three-node deployment and proves a quorum
// assembled by LockQuorum grants a lock and enforces mutual exclusion.
func TestLockQuorumAcrossNodes(t *testing.T) {
	var bases []string
	var lockers []*lock.LocalLocker
	for range 3 {
		srv, locker := newServerNode(t, 1, "n")
		hs := httptest.NewServer(srv.Handler())
		t.Cleanup(hs.Close)
		bases = append(bases, hs.URL)
		lockers = append(lockers, locker)
	}
	client := http.DefaultClient

	// Every node spans the SAME three lock authorities; the difference is only
	// which one it reaches in-process and which two over the network. That shared
	// set is what makes any two majorities overlap on a common authority.
	//
	// Node A is node 0: its own locker plus nodes 1 and 2 as remotes.
	quorumA := cluster.LockQuorum(lockers[0], bases[1:], client)
	if len(quorumA) != 3 {
		t.Fatalf("quorum has %d lockers, want 3", len(quorumA))
	}

	ctx := context.Background()
	mu := lock.NewDRWMutex("A", quorumA, "bucket/key")
	if !mu.GetLock(ctx, "test") {
		t.Fatal("failed to acquire a lock with full quorum available")
	}

	// Node B is node 1: its own locker plus nodes 0 and 2 as remotes — the same
	// three authorities. It must not get the lock while A holds it.
	quorumB := cluster.LockQuorum(lockers[1], []string{bases[0], bases[2]}, client)
	muB := lock.NewDRWMutex("B", quorumB, "bucket/key")
	ctx2, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	if muB.GetLock(ctx2, "test") {
		muB.Unlock()
		t.Fatal("two holders acquired the same lock; mutual exclusion broken")
	}
	mu.Unlock()

	// Once A releases, B can take it.
	if !muB.GetLock(ctx, "test") {
		t.Fatal("B could not acquire the lock after A released it")
	}
	muB.Unlock()
}

// --- compact in-memory ECDSA PKI for the mTLS composition test ----------------

func newCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("ca cert: %v", err)
	}
	cert, _ := x509.ParseCertificate(der)
	return cert, key
}

func leaf(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns []string, ips []net.IP) (certPEM, keyPEM []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "node"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM
}

func writePEM(t *testing.T, dir, name string, der []byte, typ string) string {
	t.Helper()
	return writeRaw(t, dir, name, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}))
}

func writeRaw(t *testing.T, dir, name string, b []byte) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
	return path
}
