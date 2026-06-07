// SPDX-License-Identifier: Apache-2.0

// Package mtls builds the matching TLS configurations that secure liteio's
// inter-node transport (spec 2020, docs 06.7, 11). Every node both serves and
// dials the cluster/rpc endpoint, so the two ends must agree: each presents a
// certificate signed by the cluster CA and verifies that its peer's certificate
// is signed by the same CA (mutual TLS). A node with no cluster certificate
// cannot join the data path, which keeps the storage and lock RPCs — which move
// raw object bytes and grant exclusive locks — off untrusted connections.
//
// ECDSA certificates are recommended: RSA verification is comparatively slow in
// Go, and the transport is on the hot path of every distributed read and write.
package mtls

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/http"
	"os"
)

// minVersion pins the floor to TLS 1.3 for both ends: every liteio node is part
// of the same deployment, so there is no legacy peer to accommodate, and 1.3
// removes the weak cipher and renegotiation options entirely.
const minVersion = uint16(tls.VersionTLS13)

// loadCAPool reads a PEM bundle and returns a pool to verify peers against.
func loadCAPool(caFile string) (*x509.CertPool, error) {
	pem, err := os.ReadFile(caFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: read CA %q: %w", caFile, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("mtls: CA %q held no usable certificates", caFile)
	}
	return pool, nil
}

// ServerConfig builds the tls.Config a node uses to serve its cluster/rpc
// endpoint. It presents the node certificate and requires every client to
// present a certificate signed by the cluster CA, so an unauthenticated or
// foreign-CA peer is rejected during the handshake.
func ServerConfig(certFile, keyFile, caFile string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load server keypair: %w", err)
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		ClientCAs:    pool,
		ClientAuth:   tls.RequireAndVerifyClientCert,
		MinVersion:   minVersion,
	}, nil
}

// ClientConfig builds the tls.Config a node uses to dial a peer. It presents the
// node certificate and verifies the peer's certificate against the cluster CA.
// serverName is the name the peer's certificate must carry (its SAN); it must be
// the host the client connects to.
func ClientConfig(certFile, keyFile, caFile, serverName string) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, fmt.Errorf("mtls: load client keypair: %w", err)
	}
	pool, err := loadCAPool(caFile)
	if err != nil {
		return nil, err
	}
	return &tls.Config{
		Certificates: []tls.Certificate{cert},
		RootCAs:      pool,
		ServerName:   serverName,
		MinVersion:   minVersion,
	}, nil
}

// NewClient builds an *http.Client whose transport presents the node certificate
// and verifies peers against the cluster CA, ready to hand to rpc.NewClient. The
// transport keeps connection pooling and HTTP/2 from the default transport.
func NewClient(certFile, keyFile, caFile, serverName string) (*http.Client, error) {
	cfg, err := ClientConfig(certFile, keyFile, caFile, serverName)
	if err != nil {
		return nil, err
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.TLSClientConfig = cfg
	return &http.Client{Transport: tr}, nil
}
