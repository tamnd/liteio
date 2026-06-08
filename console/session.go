// SPDX-License-Identifier: Apache-2.0

package console

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"sync"
	"time"

	"github.com/tamnd/liteio/auth"
)

// session is one logged-in console browser. It holds the credentials the user
// proved at login so the signed bridge can sign admin calls on their behalf; the
// browser never sees the secret again. The token is the opaque cookie value.
type session struct {
	creds  auth.Credentials
	expiry time.Time
}

// sessionStore keeps live console sessions keyed by their opaque token. Tokens are
// high-entropy random strings, so a map lookup is enough to find one; nothing but
// holding the token grants access. Expired sessions are rejected on lookup and
// swept by Sweep, the same belt-and-braces pairing the STS store uses.
type sessionStore struct {
	mu  sync.Mutex
	m   map[string]session
	now func() time.Time
}

func newSessionStore(now func() time.Time) *sessionStore {
	return &sessionStore{m: make(map[string]session), now: now}
}

// create mints a token for verified credentials and stores the session with the
// given lifetime, returning the token to set as the cookie.
func (s *sessionStore) create(creds auth.Credentials, ttl time.Duration) (string, error) {
	token, err := randToken()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	s.m[token] = session{creds: creds, expiry: s.now().Add(ttl)}
	s.mu.Unlock()
	return token, nil
}

// lookup returns the live session for a token. A missing or expired token returns
// false; an expired one is dropped in passing so it cannot be reused.
func (s *sessionStore) lookup(token string) (session, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.m[token]
	if !ok {
		return session{}, false
	}
	if !s.now().Before(sess.expiry) {
		delete(s.m, token)
		return session{}, false
	}
	return sess, true
}

// destroy drops a session so its cookie can no longer be used (logout).
func (s *sessionStore) destroy(token string) {
	s.mu.Lock()
	delete(s.m, token)
	s.mu.Unlock()
}

// Sweep deletes every expired session and returns how many it removed. Sessions are
// also rejected lazily on lookup, so this only bounds the map.
func (s *sessionStore) Sweep() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	var n int
	for token, sess := range s.m {
		if !now.Before(sess.expiry) {
			delete(s.m, token)
			n++
		}
	}
	return n
}

// randToken returns a 32-byte URL-safe random session token.
func randToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("console: read random: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
