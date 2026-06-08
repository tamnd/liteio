// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/x509"
	"fmt"
	"slices"
	"time"
)

// AssumeRoleWithCertificate is the X.509 client-certificate federated flow (spec
// 2020, doc 08.4): a client presents a certificate, liteio confirms it chains to a
// configured trust anchor, and maps the certificate's subject to the policies the
// session inherits. Like AssumeRoleWithWebIdentity it needs no liteio credential up
// front — the certificate is the credential — so the session is federated
// (sessionRecord.parent == ""): its base is the mapped policy set and its subject is
// the certificate's common name. Validation is stdlib crypto/x509, so no new
// dependency and no hand-rolled protocol. This file holds the provider and the
// exchange.

// CertificateProvider configures the trust liteio places in client certificates for
// the AssumeRoleWithCertificate flow.
//
// Policy mapping mirrors the OIDC flow's claim mapping: the certificate's subject
// Organization (O) values are read as liteio policy names, and those known to the
// store form the session's base permissions. An operator names liteio policies to
// match the organizations its issuing CA stamps into certificates, the same way a
// web-identity deployment names them to match a token claim.
type CertificateProvider struct {
	// Name is an operator-facing label.
	Name string
	// Roots are the trust anchors a presented certificate must chain to. Required.
	// This is liteio's own trust decision and need not match the TLS listener's
	// client-CA configuration: the listener may merely request a certificate while
	// liteio verifies it here against these roots.
	Roots *x509.CertPool
}

// certificateProvider is the registered, internal form of a provider.
type certificateProvider struct {
	name  string
	roots *x509.CertPool
}

// RegisterCertificateProvider registers the trust anchors for the certificate flow.
// Roots is required and only one provider may be registered, since a deployment
// has a single client-certificate trust domain.
func (s *Store) RegisterCertificateProvider(p CertificateProvider) error {
	if p.Roots == nil {
		return fmt.Errorf("%w: certificate provider has no trust roots", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.certProvider != nil {
		return fmt.Errorf("%w: certificate provider", ErrExists)
	}
	s.certProvider = &certificateProvider{name: p.Name, roots: p.Roots}
	return nil
}

// AssumeRoleWithCertificate verifies a client certificate chain against the
// registered trust roots, maps the leaf's subject Organization values to a session,
// and returns the session and the leaf's subject common name. The chain is the
// leaf followed by any intermediates the client presented. The session duration is
// clamped to [Min, Max] and further bounded so it never outlives the certificate.
// An optional session policy can only narrow, as with AssumeRole.
func (s *Store) AssumeRoleWithCertificate(chain []*x509.Certificate, sessionPolicy *Policy, ttl time.Duration) (Session, string, error) {
	if len(chain) == 0 {
		return Session{}, "", fmt.Errorf("%w: no client certificate presented", ErrInvalidToken)
	}

	s.mu.RLock()
	prov := s.certProvider
	s.mu.RUnlock()
	if prov == nil {
		return Session{}, "", fmt.Errorf("%w: certificate identity is not configured", ErrInvalidToken)
	}

	leaf := chain[0]
	intermediates := x509.NewCertPool()
	for _, c := range chain[1:] {
		intermediates.AddCert(c)
	}
	now := s.now()
	// Verify checks the chain to a trusted root and, via CurrentTime, the validity
	// window — so an expired or not-yet-valid certificate is rejected here. The
	// ExtKeyUsage requirement keeps a server-only certificate from authenticating.
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots:         prov.roots,
		Intermediates: intermediates,
		CurrentTime:   now,
		KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return Session{}, "", fmt.Errorf("%w: certificate is not trusted: %v", ErrInvalidToken, err)
	}

	subject := leaf.Subject.CommonName
	if subject == "" {
		subject = leaf.Subject.String()
	}

	// Bound the session so it never outlives the certificate, mirroring how the OIDC
	// flow bounds a session by the token expiry.
	expiry := now.Add(clampDuration(ttl))
	if leaf.NotAfter.Before(expiry) {
		expiry = leaf.NotAfter
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	base := make([]Policy, 0, len(leaf.Subject.Organization))
	for _, name := range slices.Clone(leaf.Subject.Organization) {
		if p, ok := s.policies[name]; ok {
			base = append(base, p)
		}
	}
	if len(base) == 0 {
		return Session{}, "", fmt.Errorf("%w: certificate subject names no known policy", ErrInvalidToken)
	}

	rec := &sessionRecord{
		basePolicies: base,
		subject:      subject,
		expiry:       expiry,
	}
	sess, err := s.mintSession(rec, sessionPolicy)
	if err != nil {
		return Session{}, "", err
	}
	return sess, subject, nil
}
