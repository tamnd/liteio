// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"slices"
	"time"
)

// AssumeRoleWithLDAPIdentity is the LDAP federated flow (spec 2020, doc 08.4): a
// client presents a directory username and password, liteio binds to the directory
// to confirm the credential and read the user's group memberships, and maps those
// groups to the policies the session inherits. Like the other federated flows it
// needs no liteio credential up front — the directory credential is the credential —
// so the session has no parent in the store (sessionRecord.parent == ""): its base
// is the mapped policy set and its subject is the user's distinguished name.
//
// Unlike the OIDC and certificate flows, the directory protocol (LDAP, RFC 4511) is
// not in the standard library, so it sits behind the Directory seam below. The auth
// package stays stdlib-only and is fully testable against a fake directory; the
// concrete bind-and-search client that speaks LDAP to a real server lives in the
// ldapdir package, which an operator wires in via RegisterLDAPProvider.

// Directory authenticates a user against an external directory and reports the
// groups the user belongs to. It is the seam between the federated session flow,
// which is pure standard library, and the LDAP wire protocol, which is not.
//
// An implementation binds as the named user with the password and, on success,
// returns the user's distinguished name (the canonical identity, used as the
// session subject) and the names of the groups it belongs to (mapped to liteio
// policies the same way the OIDC flow maps a claim and the certificate flow maps
// subject organizations). It must return ErrInvalidToken for a rejected credential
// and ErrIDPCommunication when it cannot reach or parse the directory, so the flow
// and the STS endpoint can tell a bad password from an unreachable server.
type Directory interface {
	Authenticate(username, password string) (dn string, groups []string, err error)
}

// LDAPProvider configures the directory liteio binds to for the
// AssumeRoleWithLDAPIdentity flow.
//
// Group mapping mirrors the other federated flows: the names Directory.Authenticate
// returns are read as liteio policy names, and those known to the store form the
// session's base permissions. An operator names liteio policies to match the
// directory's group names, the same way a web-identity deployment names them to
// match a token claim.
type LDAPProvider struct {
	// Name is an operator-facing label.
	Name string
	// Directory binds to the external directory and resolves group memberships.
	// Required. The ldapdir package provides the production LDAP implementation.
	Directory Directory
}

// ldapProvider is the registered, internal form of a provider.
type ldapProvider struct {
	name string
	dir  Directory
}

// RegisterLDAPProvider registers the directory for the LDAP flow. The directory is
// required and only one provider may be registered, since a deployment binds to a
// single directory.
func (s *Store) RegisterLDAPProvider(p LDAPProvider) error {
	if p.Directory == nil {
		return fmt.Errorf("%w: LDAP provider has no directory", ErrInvalid)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ldapProvider != nil {
		return fmt.Errorf("%w: LDAP provider", ErrExists)
	}
	s.ldapProvider = &ldapProvider{name: p.Name, dir: p.Directory}
	return nil
}

// AssumeRoleWithLDAPIdentity binds the given username and password against the
// registered directory, maps the user's groups to a session, and returns the
// session and the user's distinguished name. The session duration is clamped to
// [Min, Max]. An optional session policy can only narrow, as with AssumeRole.
func (s *Store) AssumeRoleWithLDAPIdentity(username, password string, sessionPolicy *Policy, ttl time.Duration) (Session, string, error) {
	// A blank credential is rejected before the directory is touched: an empty
	// password to many directories is an unauthenticated (anonymous) bind that
	// "succeeds" without proving anything, so liteio never forwards one.
	if username == "" || password == "" {
		return Session{}, "", fmt.Errorf("%w: directory username and password are required", ErrInvalidToken)
	}

	s.mu.RLock()
	prov := s.ldapProvider
	s.mu.RUnlock()
	if prov == nil {
		return Session{}, "", fmt.Errorf("%w: LDAP identity is not configured", ErrInvalidToken)
	}

	// Bind outside the store lock: it is network I/O against the directory and must
	// not block the store. The directory reports a bad credential as ErrInvalidToken
	// and an unreachable server as ErrIDPCommunication, which propagate to the caller.
	dn, groups, err := prov.dir.Authenticate(username, password)
	if err != nil {
		return Session{}, "", err
	}
	subject := dn
	if subject == "" {
		subject = username
	}

	expiry := s.now().Add(clampDuration(ttl))

	s.mu.Lock()
	defer s.mu.Unlock()
	base := make([]Policy, 0, len(groups))
	for _, name := range slices.Clone(groups) {
		if p, ok := s.policies[name]; ok {
			base = append(base, p)
		}
	}
	if len(base) == 0 {
		return Session{}, "", fmt.Errorf("%w: directory groups name no known policy", ErrInvalidToken)
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
