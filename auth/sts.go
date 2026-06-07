// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"fmt"
	"time"
)

// STS issues short-lived credentials a caller exchanges long-lived ones for
// (spec 2020, doc 08.4). A session's permissions are the intersection of the
// identity it was assumed from and an optional session policy: the session policy
// can only narrow, never widen, which is the safety property that makes a session
// safe to hand to untrusted code (scoping an application to a prefix within a
// shared bucket). This file covers AssumeRole and the session lifecycle; the
// federated flows (AssumeRoleWithWebIdentity/OIDC and the rest) layer their token
// validation on top of issueSession and are separate subsystems.

// STS session duration bounds, matching the AWS defaults clients expect.
const (
	// MinSTSDuration is the shortest session liteio issues (15 minutes).
	MinSTSDuration = 15 * time.Minute
	// MaxSTSDuration is the longest (12 hours); a longer request is clamped.
	MaxSTSDuration = 12 * time.Hour
	// DefaultSTSDuration is used when AssumeRole is given a zero duration (1 hour).
	DefaultSTSDuration = time.Hour
)

// sessionRecord is the stored state of an STS session: the credential to verify
// signatures against, the principal it was assumed from (root or a user access
// key), the optional narrowing policy, and when it stops being valid.
type sessionRecord struct {
	secretKey    string
	sessionToken string
	parent       string
	policy       *Policy
	expiry       time.Time
}

// Session is the temporary credential AssumeRole returns: the triple a client
// signs requests with, plus when it expires.
type Session struct {
	AccessKey    string
	SecretKey    string
	SessionToken string
	Expiration   time.Time
}

// clampDuration brings a requested session duration within [Min, Max], treating
// zero as the default.
func clampDuration(d time.Duration) time.Duration {
	switch {
	case d == 0:
		return DefaultSTSDuration
	case d < MinSTSDuration:
		return MinSTSDuration
	case d > MaxSTSDuration:
		return MaxSTSDuration
	default:
		return d
	}
}

// AssumeRole exchanges a long-lived identity (root or a user) for a temporary
// session. An optional session policy narrows the result: the session is allowed
// an action only when both the assumed identity and the session policy allow it
// (see IsAllowed), so a session can only restrict the identity's permissions. The
// duration is clamped to [MinSTSDuration, MaxSTSDuration]; a zero duration uses
// DefaultSTSDuration. The parent must be root or an existing user — a service
// account or another session cannot be assumed from.
func (s *Store) AssumeRole(parentKey string, sessionPolicy *Policy, ttl time.Duration) (Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if parentKey != s.rootKey {
		if _, ok := s.users[parentKey]; !ok {
			return Session{}, fmt.Errorf("%w: cannot assume from %q", ErrNotFound, parentKey)
		}
	}

	accessKey, err := s.freshAccessKey()
	if err != nil {
		return Session{}, err
	}
	secretKey, err := randSecret()
	if err != nil {
		return Session{}, err
	}
	token, err := randToken()
	if err != nil {
		return Session{}, err
	}

	expiry := s.now().Add(clampDuration(ttl))
	rec := &sessionRecord{
		secretKey:    secretKey,
		sessionToken: token,
		parent:       parentKey,
		expiry:       expiry,
	}
	if sessionPolicy != nil {
		p := *sessionPolicy
		p.Statements = append([]Statement(nil), sessionPolicy.Statements...)
		rec.policy = &p
	}
	s.sessions[accessKey] = rec

	return Session{
		AccessKey:    accessKey,
		SecretKey:    secretKey,
		SessionToken: token,
		Expiration:   expiry,
	}, nil
}

// ValidateSessionToken reports whether the access key names a live session whose
// token matches and which has not expired. The S3 front door calls this after
// verifying the request signature, to confirm the presented X-Amz-Security-Token
// belongs to the session. A non-session access key returns false.
func (s *Store) ValidateSessionToken(accessKey, token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	rec, ok := s.sessions[accessKey]
	if !ok {
		return false
	}
	if !s.now().Before(rec.expiry) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(rec.sessionToken), []byte(token)) == 1
}

// RevokeSession deletes a session before its TTL, so a leaked credential can be
// cut off. A missing access key returns ErrNotFound.
func (s *Store) RevokeSession(accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.sessions[accessKey]; !ok {
		return fmt.Errorf("%w: session %q", ErrNotFound, accessKey)
	}
	delete(s.sessions, accessKey)
	return nil
}

// ExpireSessions drops every session past its TTL and returns how many it removed.
// Sessions are also rejected lazily on use (IsAllowed, ValidateSessionToken), so
// this is a housekeeping sweep to bound the map, not a correctness requirement.
func (s *Store) ExpireSessions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var n int
	for key, rec := range s.sessions {
		if !now.Before(rec.expiry) {
			delete(s.sessions, key)
			n++
		}
	}
	return n
}

// freshAccessKey returns a random access key not already in use. The caller must
// hold the lock. Collisions are astronomically unlikely; the retry is belt and
// braces against ever minting a duplicate key.
func (s *Store) freshAccessKey() (string, error) {
	for range 8 {
		key, err := randAccessKey()
		if err != nil {
			return "", err
		}
		if !s.keyTaken(key) {
			return key, nil
		}
	}
	return "", fmt.Errorf("auth: could not mint a unique access key")
}

// Credential encodings mirror AWS shapes: an access key is uppercase base32 (the
// alphabet AWS keys use), a secret and a token are base64. Lengths are generous so
// the random space dwarfs any session count.
var (
	b32   = base32.StdEncoding.WithPadding(base32.NoPadding)
	b64   = base64.RawURLEncoding
	stsAK = "LTIO" // liteio STS access-key prefix, like AWS's ASIA
)

func randAccessKey() (string, error) {
	b := make([]byte, 10) // 16 base32 chars
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return stsAK + b32.EncodeToString(b), nil
}

func randSecret() (string, error) {
	b := make([]byte, 30) // 40 base64 chars
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return b64.EncodeToString(b), nil
}

func randToken() (string, error) {
	b := make([]byte, 48)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("auth: read random: %w", err)
	}
	return b64.EncodeToString(b), nil
}
