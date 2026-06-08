// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"fmt"
	"slices"
)

// This file holds the read accessors and policy lifecycle the admin REST API
// (doc 10.2) needs on top of the create/delete mutators in identity.go: listing
// and reading identities, reading a policy document back, and replacing or
// removing a custom policy. They round out the IAM surface so the admin handlers
// stay thin translations of HTTP onto the store.

// Users returns the access keys of every user, sorted. Service accounts and STS
// sessions are not users and are listed by their own accessors.
func (s *Store) Users() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedKeys(s.users)
}

// User returns a copy of the named user with its policies and group memberships,
// and whether it exists. The secret key is never returned: the admin API has no
// reason to read a secret back, and signature verification uses Secret separately.
func (s *Store) User(accessKey string) (User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[accessKey]
	if !ok {
		return User{}, false
	}
	return User{
		AccessKey: u.AccessKey,
		Policies:  slices.Clone(u.Policies),
		Groups:    slices.Clone(u.Groups),
	}, true
}

// Groups returns the names of every group, sorted.
func (s *Store) Groups() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return sortedKeys(s.groups)
}

// Group returns a copy of the named group with its attached policies, and whether
// it exists.
func (s *Store) Group(name string) (Group, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	g, ok := s.groups[name]
	if !ok {
		return Group{}, false
	}
	return Group{Name: g.Name, Policies: slices.Clone(g.Policies)}, true
}

// GetPolicy returns the named policy document (its statements cloned so a caller
// cannot mutate the stored copy) and whether it exists. It serves both custom and
// canned policies, so the admin API can render any attachable policy.
func (s *Store) GetPolicy(name string) (Policy, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	p, ok := s.policies[name]
	if !ok {
		return Policy{}, false
	}
	p.Statements = slices.Clone(p.Statements)
	return p, true
}

// SetPolicy creates or replaces a custom policy. A canned policy name is rejected
// so a built-in (readonly, readwrite, consoleAdmin, ...) can never be redefined
// out from under the operators who rely on it; use a fresh name instead.
func (s *Store) SetPolicy(name string, p Policy) error {
	if isCanned(name) {
		return fmt.Errorf("%w: cannot redefine built-in policy %q", ErrExists, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.policies[name] = p
	return nil
}

// DeletePolicy removes a custom policy. A canned policy cannot be deleted, and an
// unknown name is ErrNotFound. A policy still attached to a user or group is
// allowed to go: userPolicies skips names it no longer knows, so the attachment
// simply stops granting rather than breaking evaluation.
func (s *Store) DeletePolicy(name string) error {
	if isCanned(name) {
		return fmt.Errorf("%w: cannot delete built-in policy %q", ErrInvalid, name)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[name]; !ok {
		return fmt.Errorf("%w: policy %q", ErrNotFound, name)
	}
	delete(s.policies, name)
	return nil
}

// ServiceAccountsFor returns the access keys of every service account derived from
// a parent user, sorted. An unknown parent is ErrNotFound, distinguishing it from
// a user that simply has no service accounts.
func (s *Store) ServiceAccountsFor(parentKey string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.users[parentKey]; !ok {
		return nil, fmt.Errorf("%w: user %q", ErrNotFound, parentKey)
	}
	var keys []string
	for ak, sa := range s.svc {
		if sa.ParentUser == parentKey {
			keys = append(keys, ak)
		}
	}
	slices.Sort(keys)
	return keys, nil
}

// sortedKeys returns the map keys in sorted order. The caller must hold the lock.
func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

// isCanned reports whether a name belongs to a built-in policy.
func isCanned(name string) bool {
	_, ok := CannedPolicy(name)
	return ok
}
