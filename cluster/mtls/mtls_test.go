// SPDX-License-Identifier: Apache-2.0

package mtls_test

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tamnd/liteio/cluster/mtls"
	"github.com/tamnd/liteio/cluster/rpc"
	"github.com/vmihailenco/msgpack/v5"
)

// --- in-memory PKI for the tests ---------------------------------------------

type ca struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	pemFile string
}

// newCA mints a self-signed ECDSA CA and writes its certificate to a temp file.
func newCA(t *testing.T, dir, name string) *ca {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: name},
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
	file := filepath.Join(dir, name+"-ca.pem")
	writePEM(t, file, "CERTIFICATE", der)
	return &ca{cert: cert, key: key, pemFile: file}
}

// leaf signs a server/client certificate under the CA and writes the cert and key
// to temp files, returning their paths.
func (c *ca) leaf(t *testing.T, dir, name string, dns []string, ips []net.IP) (certFile, keyFile string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		DNSNames:     dns,
		IPAddresses:  ips,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		t.Fatalf("leaf cert: %v", err)
	}
	certFile = filepath.Join(dir, name+"-cert.pem")
	keyFile = filepath.Join(dir, name+"-key.pem")
	writePEM(t, certFile, "CERTIFICATE", der)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatalf("marshal key: %v", err)
	}
	writePEM(t, keyFile, "EC PRIVATE KEY", keyDER)
	return certFile, keyFile
}

func writePEM(t *testing.T, file, typ string, der []byte) {
	t.Helper()
	if err := os.WriteFile(file, pem.EncodeToMemory(&pem.Block{Type: typ, Bytes: der}), 0o600); err != nil {
		t.Fatalf("write %s: %v", file, err)
	}
}

// --- a TLS-served RPC mux -----------------------------------------------------

// serveTLS starts mux over TLS with serverCfg on a loopback listener and returns
// its https URL.
func serveTLS(t *testing.T, mux *rpc.Mux, serverCfg *tls.Config) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: mux, TLSConfig: serverCfg}
	go func() { _ = srv.ServeTLS(ln, "", "") }()
	t.Cleanup(func() { _ = srv.Close() })
	_, port, _ := net.SplitHostPort(ln.Addr().String())
	return "https://localhost:" + port
}

func pingMux(t *testing.T) *rpc.Mux {
	t.Helper()
	mux := rpc.NewMux()
	mux.Register("ping", func(_ context.Context, _ msgpack.RawMessage, _ io.Reader, resp *rpc.Response) error {
		resp.SetResult("pong")
		return nil
	})
	return mux
}

// --- tests --------------------------------------------------------------------

func TestMutualTLSHappyPath(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, dir, "cluster")
	srvCert, srvKey := root.leaf(t, dir, "server", []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	cliCert, cliKey := root.leaf(t, dir, "client", nil, nil)

	srvCfg, err := mtls.ServerConfig(srvCert, srvKey, root.pemFile)
	if err != nil {
		t.Fatalf("ServerConfig: %v", err)
	}
	if srvCfg.ClientAuth != tls.RequireAndVerifyClientCert {
		t.Fatalf("server must require and verify a client cert, got %v", srvCfg.ClientAuth)
	}
	if srvCfg.MinVersion != tls.VersionTLS13 {
		t.Fatalf("server MinVersion = %x, want TLS 1.3", srvCfg.MinVersion)
	}
	url := serveTLS(t, pingMux(t), srvCfg)

	hc, err := mtls.NewClient(cliCert, cliKey, root.pemFile, "localhost")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client := rpc.NewClient(url, hc)
	reply, err := client.Call(context.Background(), "ping", struct{}{}, nil)
	if err != nil {
		t.Fatalf("Call over mTLS: %v", err)
	}
	defer reply.Close()
	var got string
	if err := reply.Unmarshal(&got); err != nil || got != "pong" {
		t.Fatalf("reply = %q, %v; want pong", got, err)
	}
}

func TestServerRejectsClientWithoutCert(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, dir, "cluster")
	srvCert, srvKey := root.leaf(t, dir, "server", []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	srvCfg, _ := mtls.ServerConfig(srvCert, srvKey, root.pemFile)
	url := serveTLS(t, pingMux(t), srvCfg)

	// A client that trusts the CA but presents no certificate must be rejected.
	pool := x509.NewCertPool()
	pem, _ := os.ReadFile(root.pemFile)
	pool.AppendCertsFromPEM(pem)
	hc := &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: pool, ServerName: "localhost", MinVersion: tls.VersionTLS13},
	}}
	client := rpc.NewClient(url, hc)
	if _, err := client.Call(context.Background(), "ping", struct{}{}, nil); err == nil {
		t.Fatal("server must reject a client presenting no certificate")
	}
}

func TestServerRejectsForeignCA(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, dir, "cluster")
	other := newCA(t, dir, "intruder")
	srvCert, srvKey := root.leaf(t, dir, "server", []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	srvCfg, _ := mtls.ServerConfig(srvCert, srvKey, root.pemFile)
	url := serveTLS(t, pingMux(t), srvCfg)

	// A client cert signed by a different CA must not be accepted, even though the
	// client trusts the real server CA.
	cliCert, cliKey := other.leaf(t, dir, "rogue", nil, nil)
	hc, err := mtls.NewClient(cliCert, cliKey, root.pemFile, "localhost")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client := rpc.NewClient(url, hc)
	if _, err := client.Call(context.Background(), "ping", struct{}{}, nil); err == nil {
		t.Fatal("server must reject a client cert from a foreign CA")
	}
}

func TestClientRejectsUntrustedServer(t *testing.T) {
	dir := t.TempDir()
	root := newCA(t, dir, "cluster")
	other := newCA(t, dir, "rogue-server-ca")
	// The server presents a cert from a CA the client does not trust.
	srvCert, srvKey := other.leaf(t, dir, "server", []string{"localhost"}, []net.IP{net.IPv4(127, 0, 0, 1)})
	srvCfg := &tls.Config{
		MinVersion: tls.VersionTLS13,
	}
	cert, err := tls.LoadX509KeyPair(srvCert, srvKey)
	if err != nil {
		t.Fatalf("load server cert: %v", err)
	}
	srvCfg.Certificates = []tls.Certificate{cert}
	url := serveTLS(t, pingMux(t), srvCfg)

	cliCert, cliKey := root.leaf(t, dir, "client", nil, nil)
	hc, err := mtls.NewClient(cliCert, cliKey, root.pemFile, "localhost")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client := rpc.NewClient(url, hc)
	if _, err := client.Call(context.Background(), "ping", struct{}{}, nil); err == nil {
		t.Fatal("client must reject a server cert from an untrusted CA")
	}
}

func TestConfigErrorsOnBadFiles(t *testing.T) {
	if _, err := mtls.ServerConfig("/no/cert", "/no/key", "/no/ca"); err == nil {
		t.Fatal("ServerConfig must error on missing files")
	}
	if _, err := mtls.ClientConfig("/no/cert", "/no/key", "/no/ca", "host"); err == nil {
		t.Fatal("ClientConfig must error on missing files")
	}
	dir := t.TempDir()
	bad := filepath.Join(dir, "empty-ca.pem")
	_ = os.WriteFile(bad, []byte("not a pem"), 0o600)
	root := newCA(t, dir, "cluster")
	cert, key := root.leaf(t, dir, "n", nil, nil)
	if _, err := mtls.ClientConfig(cert, key, bad, "host"); err == nil {
		t.Fatal("ClientConfig must error on a CA file with no certificates")
	}
}
