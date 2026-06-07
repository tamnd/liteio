// SPDX-License-Identifier: Apache-2.0

// Package auth holds liteio's credential model and (later) the policy-based
// access-control evaluator (spec 2020, doc 08). For the M1 front door it provides
// the access-key/secret-key pairs the SigV4 verifier looks up.
package auth

import "crypto/subtle"

// Credentials is one access-key/secret-key pair.
type Credentials struct {
	AccessKey string
	SecretKey string
}

// CredentialStore resolves an access key to its credentials. It is the seam the
// SigV4 verifier uses; a full IAM store (users, service accounts, STS) implements
// the same interface later.
type CredentialStore interface {
	// Get returns the credentials for an access key and whether it is known.
	Get(accessKey string) (Credentials, bool)
}

// StaticStore is a fixed set of credentials, the root/admin key configured at
// startup. Lookups are constant-time on the access key to avoid leaking which
// keys exist through timing.
type StaticStore struct {
	creds map[string]Credentials
}

// NewStaticStore builds a store from a set of credentials.
func NewStaticStore(creds ...Credentials) *StaticStore {
	m := make(map[string]Credentials, len(creds))
	for _, c := range creds {
		m[c.AccessKey] = c
	}
	return &StaticStore{creds: m}
}

// Get implements CredentialStore.
func (s *StaticStore) Get(accessKey string) (Credentials, bool) {
	// Compare against every key in constant time so a hit and a miss take the
	// same path; the map lookup alone would short-circuit.
	var found Credentials
	ok := false
	for ak, c := range s.creds {
		if subtle.ConstantTimeCompare([]byte(ak), []byte(accessKey)) == 1 {
			found = c
			ok = true
		}
	}
	return found, ok
}
