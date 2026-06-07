// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// stsStore builds a store with a user (alice/readwrite) and a controllable clock
// pinned at a fixed instant the test advances by hand.
func stsStore(t *testing.T) (*Store, *time.Time) {
	t.Helper()
	s := NewStore("root", "rootsecret")
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	clock := time.Unix(1_700_000_000, 0).UTC()
	SetClock(s, func() time.Time { return clock })
	return s, &clock
}

func TestAssumeRoleInheritsParent(t *testing.T) {
	s, _ := stsStore(t)
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(sess.AccessKey, "LTIO") {
		t.Fatalf("session access key %q lacks the LTIO prefix", sess.AccessKey)
	}
	// The credential verifies and the session has the parent's full rights.
	if sec, ok := s.Secret(sess.AccessKey); !ok || sec != sess.SecretKey {
		t.Fatalf("Secret(session) = %q,%v want %q", sec, ok, sess.SecretKey)
	}
	if !allowed(t, s, sess.AccessKey, put("b")) {
		t.Fatal("a session with no session policy inherits the parent's rights")
	}
}

func TestAssumeRoleSessionPolicyNarrows(t *testing.T) {
	s, _ := stsStore(t)
	// Session scoped to reads under one prefix.
	scoped := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"}]}`)
	sess, err := s.AssumeRole("alice", &scoped, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, sess.AccessKey, get("data")) {
		t.Fatal("session read on its prefix should be allowed")
	}
	if allowed(t, s, sess.AccessKey, put("data")) {
		t.Fatal("session policy must narrow: no write even though the parent allows")
	}
	if allowed(t, s, sess.AccessKey, get("other")) {
		t.Fatal("session scoped to data/* must not reach another bucket")
	}
}

func TestAssumeRoleSessionPolicyCannotWiden(t *testing.T) {
	s := NewStore("root", "rootsecret")
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		t.Fatal(err)
	}
	wide := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::*"}]}`)
	sess, err := s.AssumeRole("alice", &wide, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, sess.AccessKey, put("b")) {
		t.Fatal("a session policy must not widen past the assumed identity's rights")
	}
	if !allowed(t, s, sess.AccessKey, get("b")) {
		t.Fatal("a read both the parent and the session allow should pass")
	}
}

func TestAssumeRoleFromRoot(t *testing.T) {
	s, _ := stsStore(t)
	// Root can do everything; a session policy bounds that to reads.
	scoped := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::*"}]}`)
	sess, err := s.AssumeRole("root", &scoped, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, sess.AccessKey, get("b")) {
		t.Fatal("root session bounded to reads should read")
	}
	if allowed(t, s, sess.AccessKey, put("b")) {
		t.Fatal("the session policy bounds even a root session")
	}
}

func TestAssumeRoleRejectsBadParent(t *testing.T) {
	s, _ := stsStore(t)
	if _, err := s.AssumeRole("ghost", nil, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assume from unknown = %v, want ErrNotFound", err)
	}
	// A service account is not an assumable principal.
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AssumeRole("app", nil, time.Hour); !errors.Is(err, ErrNotFound) {
		t.Fatalf("assume from service account = %v, want ErrNotFound", err)
	}
}

func TestSessionExpiry(t *testing.T) {
	s, clock := stsStore(t)
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Expiration != clock.Add(time.Hour) {
		t.Fatalf("expiration = %v, want %v", sess.Expiration, clock.Add(time.Hour))
	}
	// One second before expiry: still valid.
	*clock = sess.Expiration.Add(-time.Second)
	if !allowed(t, s, sess.AccessKey, get("b")) {
		t.Fatal("session must be valid just before expiry")
	}
	// At expiry: rejected with ErrExpired, token no longer validates.
	*clock = sess.Expiration
	if ok, err := s.IsAllowed(sess.AccessKey, get("b")); ok || !errors.Is(err, ErrExpired) {
		t.Fatalf("at expiry IsAllowed = %v,%v want false,ErrExpired", ok, err)
	}
	if s.ValidateSessionToken(sess.AccessKey, sess.SessionToken) {
		t.Fatal("an expired session token must not validate")
	}
}

func TestValidateSessionToken(t *testing.T) {
	s, _ := stsStore(t)
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !s.ValidateSessionToken(sess.AccessKey, sess.SessionToken) {
		t.Fatal("the issued token must validate")
	}
	if s.ValidateSessionToken(sess.AccessKey, "wrong") {
		t.Fatal("a wrong token must not validate")
	}
	if s.ValidateSessionToken("alice", "anything") {
		t.Fatal("a non-session access key has no session token")
	}
}

func TestRevokeSession(t *testing.T) {
	s, _ := stsStore(t)
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.RevokeSession(sess.AccessKey); err != nil {
		t.Fatal(err)
	}
	if _, err := s.IsAllowed(sess.AccessKey, get("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoked session IsAllowed err = %v, want ErrNotFound", err)
	}
	if err := s.RevokeSession(sess.AccessKey); !errors.Is(err, ErrNotFound) {
		t.Fatalf("revoking a gone session = %v, want ErrNotFound", err)
	}
}

func TestExpireSessionsSweep(t *testing.T) {
	s, clock := stsStore(t)
	short, _ := s.AssumeRole("alice", nil, MinSTSDuration)
	long, _ := s.AssumeRole("alice", nil, MaxSTSDuration)
	// Advance past the short session but not the long one.
	*clock = clock.Add(MinSTSDuration)
	if n := s.ExpireSessions(); n != 1 {
		t.Fatalf("ExpireSessions removed %d, want 1", n)
	}
	if _, err := s.IsAllowed(short.AccessKey, get("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("swept session should be gone, err = %v", err)
	}
	if !allowed(t, s, long.AccessKey, get("b")) {
		t.Fatal("the long session must survive the sweep")
	}
}

func TestAssumeRoleClampsDuration(t *testing.T) {
	s, clock := stsStore(t)
	for _, tc := range []struct {
		name string
		ttl  time.Duration
		want time.Duration
	}{
		{"zero uses default", 0, DefaultSTSDuration},
		{"below min clamps up", time.Minute, MinSTSDuration},
		{"above max clamps down", 48 * time.Hour, MaxSTSDuration},
		{"in range kept", 2 * time.Hour, 2 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sess, err := s.AssumeRole("alice", nil, tc.ttl)
			if err != nil {
				t.Fatal(err)
			}
			if got := sess.Expiration.Sub(*clock); got != tc.want {
				t.Fatalf("duration = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestSessionKeyIsUnique(t *testing.T) {
	s, _ := stsStore(t)
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// A new user cannot reuse a live session's access key.
	if err := s.AddUser(User{AccessKey: sess.AccessKey, SecretKey: "x"}); !errors.Is(err, ErrExists) {
		t.Fatalf("adding a user on a session key = %v, want ErrExists", err)
	}
}

func BenchmarkAssumeRole(b *testing.B) {
	s := NewStore("root", "rootsecret")
	_ = s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}})
	for b.Loop() {
		sess, err := s.AssumeRole("alice", nil, time.Hour)
		if err != nil {
			b.Fatal(err)
		}
		_ = s.RevokeSession(sess.AccessKey)
	}
}
