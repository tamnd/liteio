// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"testing"
	"time"
)

// fakeDirectory is a stand-in for an LDAP directory: it answers Authenticate from an
// in-memory table, so the federated flow runs end to end without a server. A user's
// entry is its password and the groups it belongs to; an unknown user or wrong
// password is ErrInvalidToken, and a flagged-down directory is ErrIDPCommunication.
type fakeDirectory struct {
	users map[string]fakeUser
	down  bool // when true, every call reports the directory unreachable
}

type fakeUser struct {
	dn       string
	password string
	groups   []string
}

func (d *fakeDirectory) Authenticate(username, password string) (string, []string, error) {
	if d.down {
		return "", nil, fmt.Errorf("%w: directory unreachable", ErrIDPCommunication)
	}
	u, ok := d.users[username]
	if !ok || u.password != password {
		return "", nil, fmt.Errorf("%w: bad credential", ErrInvalidToken)
	}
	return u.dn, u.groups, nil
}

// ldapStore builds a store with the fake directory registered, a pinned clock, and a
// custom data-reader policy the directory's groups map to.
func ldapStore(t *testing.T, dir Directory, now time.Time) *Store {
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
	if err := s.RegisterLDAPProvider(LDAPProvider{Name: "test", Directory: dir}); err != nil {
		t.Fatalf("RegisterLDAPProvider: %v", err)
	}
	return s
}

func TestAssumeRoleWithLDAPIdentity(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		"alice": {dn: "uid=alice,ou=people,dc=corp", password: "pw", groups: []string{"data-reader"}},
	}}
	s := ldapStore(t, dir, now)

	sess, subject, err := s.AssumeRoleWithLDAPIdentity("alice", "pw", nil, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithLDAPIdentity: %v", err)
	}
	// The subject is the user's distinguished name, not the bare username.
	if subject != "uid=alice,ou=people,dc=corp" {
		t.Fatalf("subject = %q, want the user DN", subject)
	}

	// The session reads under data/ (the mapped group policy) but cannot write.
	get := Request{Action: "s3:GetObject", Resource: ObjectARN("data", "report.csv")}
	if ok, err := s.IsAllowed(sess.AccessKey, get); err != nil || !ok {
		t.Fatalf("GetObject = (%v, %v), want allowed", ok, err)
	}
	put := Request{Action: "s3:PutObject", Resource: ObjectARN("data", "report.csv")}
	if ok, _ := s.IsAllowed(sess.AccessKey, put); ok {
		t.Fatalf("PutObject allowed, want denied")
	}
}

func TestAssumeRoleWithLDAPIdentityMultipleGroups(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		// A group that names no known policy is ignored; data-reader still maps.
		"svc": {dn: "uid=svc", password: "pw", groups: []string{"unrelated-group", "data-reader"}},
	}}
	s := ldapStore(t, dir, now)

	sess, _, err := s.AssumeRoleWithLDAPIdentity("svc", "pw", nil, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithLDAPIdentity: %v", err)
	}
	get := Request{Action: "s3:GetObject", Resource: ObjectARN("data", "x")}
	if ok, err := s.IsAllowed(sess.AccessKey, get); err != nil || !ok {
		t.Fatalf("GetObject = (%v, %v), want allowed", ok, err)
	}
}

func TestAssumeRoleWithLDAPIdentitySessionPolicyNarrows(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		"alice": {dn: "uid=alice", password: "pw", groups: []string{"data-reader"}},
	}}
	s := ldapStore(t, dir, now)

	// The mapped base reads all of data/; the session policy narrows to data/pub/.
	narrow, err := ParsePolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/pub/*"}]}`))
	if err != nil {
		t.Fatalf("parse policy: %v", err)
	}
	sess, _, err := s.AssumeRoleWithLDAPIdentity("alice", "pw", &narrow, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithLDAPIdentity: %v", err)
	}
	if ok, _ := s.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "pub/x")}); !ok {
		t.Fatalf("read under data/pub denied, want allowed")
	}
	if ok, _ := s.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "priv/x")}); ok {
		t.Fatalf("read under data/priv allowed, want denied by narrowing policy")
	}
}

func TestAssumeRoleWithLDAPIdentityDuration(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		"alice": {dn: "uid=alice", password: "pw", groups: []string{"data-reader"}},
	}}
	s := ldapStore(t, dir, now)

	// A bind has no expiry of its own, so the session lives exactly the clamped TTL.
	sess, _, err := s.AssumeRoleWithLDAPIdentity("alice", "pw", nil, 2*time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithLDAPIdentity: %v", err)
	}
	if want := now.Add(2 * time.Hour); !sess.Expiration.Equal(want) {
		t.Fatalf("expiry = %v, want %v", sess.Expiration, want)
	}
}

func TestAssumeRoleWithLDAPIdentityRejections(t *testing.T) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		"alice":  {dn: "uid=alice", password: "pw", groups: []string{"data-reader"}},
		"nogrp":  {dn: "uid=nogrp", password: "pw", groups: []string{"some-other-group"}},
		"groupc": {dn: "uid=groupc", password: "pw", groups: nil},
	}}
	s := ldapStore(t, dir, now)

	t.Run("wrong password", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("alice", "nope", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("unknown user", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("ghost", "pw", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("empty username", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("", "pw", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("empty password rejected before bind", func(t *testing.T) {
		// An empty password must never reach the directory (anonymous-bind footgun).
		if _, _, err := s.AssumeRoleWithLDAPIdentity("alice", "", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("no matching policy", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("nogrp", "pw", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("no groups at all", func(t *testing.T) {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("groupc", "pw", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})

	t.Run("directory unreachable", func(t *testing.T) {
		// An unreachable directory propagates as ErrIDPCommunication, not as a bad
		// credential, so the endpoint can answer 500 rather than 403.
		down := ldapStore(t, &fakeDirectory{down: true}, now)
		if _, _, err := down.AssumeRoleWithLDAPIdentity("alice", "pw", nil, time.Hour); !errors.Is(err, ErrIDPCommunication) {
			t.Fatalf("err = %v, want ErrIDPCommunication", err)
		}
	})

	t.Run("not configured", func(t *testing.T) {
		bare := NewStore("root", "rootsecret")
		SetClock(bare, func() time.Time { return now })
		if _, _, err := bare.AssumeRoleWithLDAPIdentity("alice", "pw", nil, time.Hour); !errors.Is(err, ErrInvalidToken) {
			t.Fatalf("err = %v, want ErrInvalidToken", err)
		}
	})
}

func TestRegisterLDAPProviderValidation(t *testing.T) {
	s := NewStore("root", "rootsecret")
	if err := s.RegisterLDAPProvider(LDAPProvider{Name: "x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil directory err = %v, want ErrInvalid", err)
	}
	dir := &fakeDirectory{}
	if err := s.RegisterLDAPProvider(LDAPProvider{Name: "x", Directory: dir}); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := s.RegisterLDAPProvider(LDAPProvider{Name: "y", Directory: dir}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate err = %v, want ErrExists", err)
	}
}

// BenchmarkAssumeRoleWithLDAPIdentity measures the map-and-mint path with the
// directory bind stubbed out, so it reports liteio's own per-exchange cost (group
// mapping plus session minting) without a network round trip.
func BenchmarkAssumeRoleWithLDAPIdentity(b *testing.B) {
	now := time.Date(2026, 6, 8, 12, 0, 0, 0, time.UTC)
	dir := &fakeDirectory{users: map[string]fakeUser{
		"alice": {dn: "uid=alice", password: "pw", groups: []string{"readonly"}},
	}}
	s := NewStore("root", "rootsecret")
	SetClock(s, func() time.Time { return now })
	if err := s.RegisterLDAPProvider(LDAPProvider{Directory: dir}); err != nil {
		b.Fatalf("RegisterLDAPProvider: %v", err)
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := s.AssumeRoleWithLDAPIdentity("alice", "pw", nil, time.Hour); err != nil {
			b.Fatalf("AssumeRoleWithLDAPIdentity: %v", err)
		}
	}
}
