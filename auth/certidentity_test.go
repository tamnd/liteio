// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"testing"
	"time"
)

// certPKI is a throwaway certificate authority for the certificate-identity tests:
// it signs client certificates with chosen subject organizations and validity
// windows, so the real crypto/x509 verification path runs.
type certPKI struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func newCertPKI(t *testing.T) *certPKI {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "liteio test CA"},
		NotBefore:             time.Unix(0, 0),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return &certPKI{caCert: caCert, caKey: key}
}

// roots returns a pool trusting this CA, for a CertificateProvider.
func (p *certPKI) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(p.caCert)
	return pool
}

// issue signs a client certificate with the given common name, organizations, and
// expiry. The returned slice is the leaf, the shape AssumeRoleWithCertificate takes.
func (p *certPKI) issue(t *testing.T, cn string, orgs []string, notAfter time.Time) []*x509.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore:    time.Unix(0, 0),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, p.caCert, &key.PublicKey, p.caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse leaf: %v", err)
	}
	return []*x509.Certificate{leaf}
}

// certStore builds a store with a registered certificate provider trusting the PKI,
// a pinned clock, and a custom data-reader policy the certificate maps to.
func certStore(t *testing.T, p *certPKI, now time.Time) *Store {
	t.Helper()
	s := NewStore("root", "rootsecret")
	SetClock(s, func() time.Time { return now })
	reader, err := ParsePolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"}]}`))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	if err := s.AddPolicy("data-reader", reader); err != nil {
		t.Fatalf("AddPolicy: %v", err)
	}
	if err := s.RegisterCertificateProvider(CertificateProvider{Name: "test", Roots: p.roots()}); err != nil {
		t.Fatalf("RegisterCertificateProvider: %v", err)
	}
	return s
}

func TestAssumeRoleWithCertificate(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	pki := newCertPKI(t)
	s := certStore(t, pki, now)

	chain := pki.issue(t, "alice@corp", []string{"data-reader"}, now.Add(24*time.Hour))
	sess, subject, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithCertificate: %v", err)
	}
	if subject != "alice@corp" {
		t.Fatalf("subject = %q, want alice@corp", subject)
	}

	// The session reads under data/ (the mapped policy) but cannot write.
	get := Request{Action: "s3:GetObject", Resource: ObjectARN("data", "report.csv")}
	if ok, err := s.IsAllowed(sess.AccessKey, get); err != nil || !ok {
		t.Fatalf("GetObject = (%v, %v), want allowed", ok, err)
	}
	put := Request{Action: "s3:PutObject", Resource: ObjectARN("data", "report.csv")}
	if ok, _ := s.IsAllowed(sess.AccessKey, put); ok {
		t.Fatalf("PutObject allowed, want denied")
	}
}

func TestAssumeRoleWithCertificateMultipleOrgs(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	pki := newCertPKI(t)
	s := certStore(t, pki, now)

	// An organization that names no known policy is ignored; data-reader still maps.
	chain := pki.issue(t, "svc", []string{"unrelated-ou", "data-reader"}, now.Add(24*time.Hour))
	sess, _, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithCertificate: %v", err)
	}
	get := Request{Action: "s3:GetObject", Resource: ObjectARN("data", "x")}
	if ok, err := s.IsAllowed(sess.AccessKey, get); err != nil || !ok {
		t.Fatalf("GetObject = (%v, %v), want allowed", ok, err)
	}
}

func TestAssumeRoleWithCertificateSessionPolicyNarrows(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	pki := newCertPKI(t)
	s := certStore(t, pki, now)

	// The mapped base reads all of data/; the session policy narrows to data/pub/.
	narrow, err := ParsePolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/pub/*"}]}`))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	chain := pki.issue(t, "alice", []string{"data-reader"}, now.Add(24*time.Hour))
	sess, _, err := s.AssumeRoleWithCertificate(chain, &narrow, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithCertificate: %v", err)
	}
	if ok, _ := s.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "pub/x")}); !ok {
		t.Fatalf("read under data/pub denied, want allowed")
	}
	if ok, _ := s.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "priv/x")}); ok {
		t.Fatalf("read under data/priv allowed, want denied by narrowing policy")
	}
}

func TestAssumeRoleWithCertificateExpiryBoundedByCert(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	pki := newCertPKI(t)
	s := certStore(t, pki, now)

	// The certificate expires in 20 minutes; a 12h request is bounded to the cert.
	certExp := now.Add(20 * time.Minute)
	chain := pki.issue(t, "alice", []string{"data-reader"}, certExp)
	sess, _, err := s.AssumeRoleWithCertificate(chain, nil, 12*time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithCertificate: %v", err)
	}
	if !sess.Expiration.Equal(certExp) {
		t.Fatalf("expiry = %v, want %v (bounded by cert)", sess.Expiration, certExp)
	}
}

func TestAssumeRoleWithCertificateRejections(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	pki := newCertPKI(t)
	s := certStore(t, pki, now)

	t.Run("untrusted CA", func(t *testing.T) {
		foreign := newCertPKI(t)
		chain := foreign.issue(t, "mallory", []string{"data-reader"}, now.Add(time.Hour))
		if _, _, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("expired cert", func(t *testing.T) {
		chain := pki.issue(t, "alice", []string{"data-reader"}, now.Add(-time.Minute))
		if _, _, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("no matching policy", func(t *testing.T) {
		chain := pki.issue(t, "alice", []string{"some-other-ou"}, now.Add(time.Hour))
		if _, _, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("no certificate", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithCertificate(nil, nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		bare := NewStore("root", "rootsecret")
		SetClock(bare, func() time.Time { return now })
		chain := pki.issue(t, "alice", []string{"readonly"}, now.Add(time.Hour))
		if _, _, err := bare.AssumeRoleWithCertificate(chain, nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})
}

func TestRegisterCertificateProviderValidation(t *testing.T) {
	s := NewStore("root", "rootsecret")
	if err := s.RegisterCertificateProvider(CertificateProvider{Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil roots err = %v, want ErrInvalid", err)
	}
	pool := x509.NewCertPool()
	if err := s.RegisterCertificateProvider(CertificateProvider{Name: "x", Roots: pool}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := s.RegisterCertificateProvider(CertificateProvider{Name: "y", Roots: pool}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate err = %v, want ErrExists", err)
	}
}

// BenchmarkAssumeRoleWithCertificate measures the verify-and-mint path: an ECDSA
// chain verification against the trust pool plus session minting.
func BenchmarkAssumeRoleWithCertificate(b *testing.B) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	key, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "ca"},
		NotBefore: time.Unix(0, 0), NotAfter: now.Add(10 * 365 * 24 * time.Hour),
		IsCA: true, KeyUsage: x509.KeyUsageCertSign, BasicConstraintsValid: true,
	}
	caDER, _ := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &key.PublicKey, key)
	caCert, _ := x509.ParseCertificate(caDER)
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	leafKey, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "alice", Organization: []string{"readonly"}},
		NotBefore: time.Unix(0, 0), NotAfter: now.Add(24 * time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, _ := x509.CreateCertificate(rand.Reader, leafTmpl, caCert, &leafKey.PublicKey, key)
	leaf, _ := x509.ParseCertificate(leafDER)
	chain := []*x509.Certificate{leaf}

	s := NewStore("root", "rootsecret")
	SetClock(s, func() time.Time { return now })
	_ = s.RegisterCertificateProvider(CertificateProvider{Roots: pool})

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := s.AssumeRoleWithCertificate(chain, nil, time.Hour); err != nil {
			b.Fatalf("AssumeRoleWithCertificate: %v", err)
		}
	}
}
