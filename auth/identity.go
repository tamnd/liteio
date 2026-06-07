// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"
)

// The identity store holds liteio's identities and turns an access key into an
// authorization decision (spec 2020, doc 08.1). It models the four identity kinds
// the spec names — root, users, groups, service accounts — and resolves each into
// the effective policy set the PBAC evaluator (Evaluate) consumes.
//
// This store is the in-memory authority. Persisting identities as objects under
// .liteio.sys (doc 07) and serializing changes under the admin lock (doc 05) is a
// later wiring subsystem; the store is written so that layer only has to load and
// snapshot it, not reimplement the model or the resolution rules.

// Sentinel errors the store returns so callers can branch on the cause.
var (
	// ErrExists is returned when creating an identity whose access key or name is
	// already taken.
	ErrExists = errors.New("auth: identity already exists")
	// ErrNotFound is returned when an operation names an identity that is absent.
	ErrNotFound = errors.New("auth: identity not found")
	// ErrUnknownPolicy is returned when attaching a policy name the store does not
	// know (neither canned nor added).
	ErrUnknownPolicy = errors.New("auth: unknown policy")
	// ErrExpired is returned when an STS session credential has passed its TTL.
	ErrExpired = errors.New("auth: session expired")
)

// User is a long-lived credential with attached policies and group memberships
// (doc 08.1). Its effective permissions are the union of its own policies and
// those of every group it belongs to.
type User struct {
	AccessKey string
	SecretKey string
	Policies  []string // attached policy names
	Groups    []string // group memberships, the source of truth for the hot path
}

// Group is a named collection of policies that apply to its members (doc 08.1).
// Membership lives on the User (User.Groups) so resolving a user's effective
// policies never scans the whole group set; a group records only its policies.
type Group struct {
	Name     string
	Policies []string // attached policy names
}

// ServiceAccount is a child credential derived from a parent user (doc 08.1).
// Its permissions are the parent's, optionally narrowed by an inline policy that
// can only remove access, never add it — enforced by intersecting the parent's
// decision with the inline policy's (see IsAllowed).
type ServiceAccount struct {
	AccessKey  string
	SecretKey  string
	ParentUser string  // access key of the parent user
	Inline     *Policy // optional narrowing policy; nil means full parent rights
}

// Store is the identity authority: root, users, groups, service accounts, and the
// named policy documents they attach. It is safe for concurrent use.
type Store struct {
	mu       sync.RWMutex
	rootKey  string
	rootSec  string
	users    map[string]*User           // by access key
	groups   map[string]*Group          // by name
	svc      map[string]*ServiceAccount // by access key
	sessions map[string]*sessionRecord  // STS sessions by access key
	policies map[string]Policy          // by name; seeded with the canned set
	now      func() time.Time           // clock, injectable for tests
}

// NewStore builds a store with the given root credential and the canned policies
// preloaded by name (so readwrite, readonly, and the rest attach out of the box).
func NewStore(rootAccessKey, rootSecretKey string) *Store {
	s := &Store{
		rootKey:  rootAccessKey,
		rootSec:  rootSecretKey,
		users:    make(map[string]*User),
		groups:   make(map[string]*Group),
		svc:      make(map[string]*ServiceAccount),
		sessions: make(map[string]*sessionRecord),
		policies: make(map[string]Policy),
		now:      time.Now,
	}
	for _, name := range CannedPolicyNames() {
		p, _ := CannedPolicy(name)
		s.policies[name] = p
	}
	return s
}

// keyTaken reports whether an access key is already in use by any identity.
// The caller must hold the lock.
func (s *Store) keyTaken(accessKey string) bool {
	if accessKey == s.rootKey {
		return true
	}
	if _, ok := s.users[accessKey]; ok {
		return true
	}
	if _, ok := s.svc[accessKey]; ok {
		return true
	}
	_, ok := s.sessions[accessKey]
	return ok
}

// AddPolicy registers a custom policy document under a name so users and groups
// can attach it. A name already in use (including a canned name) is rejected, so
// a custom policy can never silently shadow a built-in one.
func (s *Store) AddPolicy(name string, p Policy) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.policies[name]; ok {
		return fmt.Errorf("%w: policy %q", ErrExists, name)
	}
	s.policies[name] = p
	return nil
}

// PolicyNames returns the names of every policy the store knows, sorted.
func (s *Store) PolicyNames() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	names := make([]string, 0, len(s.policies))
	for name := range s.policies {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// AddUser creates a user. Its access key must be free across root, users, and
// service accounts, and every attached policy name must be known.
func (s *Store) AddUser(u User) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyTaken(u.AccessKey) {
		return fmt.Errorf("%w: access key %q", ErrExists, u.AccessKey)
	}
	if err := s.policiesKnown(u.Policies); err != nil {
		return err
	}
	for _, g := range u.Groups {
		if _, ok := s.groups[g]; !ok {
			return fmt.Errorf("%w: group %q", ErrNotFound, g)
		}
	}
	cp := u
	cp.Policies = slices.Clone(u.Policies)
	cp.Groups = slices.Clone(u.Groups)
	s.users[u.AccessKey] = &cp
	return nil
}

// DeleteUser removes a user and every service account derived from it, so a
// deleted parent leaves no orphaned child credentials behind.
func (s *Store) DeleteUser(accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.users[accessKey]; !ok {
		return fmt.Errorf("%w: user %q", ErrNotFound, accessKey)
	}
	delete(s.users, accessKey)
	for k, sa := range s.svc {
		if sa.ParentUser == accessKey {
			delete(s.svc, k)
		}
	}
	return nil
}

// AddGroup creates an empty-membership group with the given attached policies.
func (s *Store) AddGroup(g Group) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[g.Name]; ok {
		return fmt.Errorf("%w: group %q", ErrExists, g.Name)
	}
	if err := s.policiesKnown(g.Policies); err != nil {
		return err
	}
	cp := g
	cp.Policies = slices.Clone(g.Policies)
	s.groups[g.Name] = &cp
	return nil
}

// DeleteGroup removes a group and drops it from the membership of every user, so
// no user is left pointing at a group that no longer exists.
func (s *Store) DeleteGroup(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.groups[name]; !ok {
		return fmt.Errorf("%w: group %q", ErrNotFound, name)
	}
	delete(s.groups, name)
	for _, u := range s.users {
		if i := slices.Index(u.Groups, name); i >= 0 {
			u.Groups = slices.Delete(u.Groups, i, i+1)
		}
	}
	return nil
}

// AddUserToGroup makes a user a member of a group. Both must exist; a repeat call
// is a no-op so membership stays a set.
func (s *Store) AddUserToGroup(accessKey, group string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[accessKey]
	if !ok {
		return fmt.Errorf("%w: user %q", ErrNotFound, accessKey)
	}
	if _, ok := s.groups[group]; !ok {
		return fmt.Errorf("%w: group %q", ErrNotFound, group)
	}
	if !slices.Contains(u.Groups, group) {
		u.Groups = append(u.Groups, group)
	}
	return nil
}

// RemoveUserFromGroup drops a membership. A user not in the group is a no-op.
func (s *Store) RemoveUserFromGroup(accessKey, group string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[accessKey]
	if !ok {
		return fmt.Errorf("%w: user %q", ErrNotFound, accessKey)
	}
	if i := slices.Index(u.Groups, group); i >= 0 {
		u.Groups = slices.Delete(u.Groups, i, i+1)
	}
	return nil
}

// GroupMembers returns the access keys of a group's members, sorted. It scans the
// users (membership lives on the user), which is off the authorization hot path.
func (s *Store) GroupMembers(group string) ([]string, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.groups[group]; !ok {
		return nil, fmt.Errorf("%w: group %q", ErrNotFound, group)
	}
	var members []string
	for key, u := range s.users {
		if slices.Contains(u.Groups, group) {
			members = append(members, key)
		}
	}
	slices.Sort(members)
	return members, nil
}

// AttachUserPolicy adds a policy to a user. The policy must be known and is not
// duplicated if already attached.
func (s *Store) AttachUserPolicy(accessKey, policy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[accessKey]
	if !ok {
		return fmt.Errorf("%w: user %q", ErrNotFound, accessKey)
	}
	if _, ok := s.policies[policy]; !ok {
		return fmt.Errorf("%w: %q", ErrUnknownPolicy, policy)
	}
	if !slices.Contains(u.Policies, policy) {
		u.Policies = append(u.Policies, policy)
	}
	return nil
}

// DetachUserPolicy removes a policy from a user; not attached is a no-op.
func (s *Store) DetachUserPolicy(accessKey, policy string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[accessKey]
	if !ok {
		return fmt.Errorf("%w: user %q", ErrNotFound, accessKey)
	}
	if i := slices.Index(u.Policies, policy); i >= 0 {
		u.Policies = slices.Delete(u.Policies, i, i+1)
	}
	return nil
}

// AddServiceAccount creates a child credential under a parent user. The parent
// must exist, the access key must be free, and an inline policy (if any) is stored
// as the narrowing boundary.
func (s *Store) AddServiceAccount(sa ServiceAccount) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyTaken(sa.AccessKey) {
		return fmt.Errorf("%w: access key %q", ErrExists, sa.AccessKey)
	}
	if _, ok := s.users[sa.ParentUser]; !ok {
		return fmt.Errorf("%w: parent user %q", ErrNotFound, sa.ParentUser)
	}
	cp := sa
	if sa.Inline != nil {
		inline := *sa.Inline
		inline.Statements = slices.Clone(sa.Inline.Statements)
		cp.Inline = &inline
	}
	s.svc[sa.AccessKey] = &cp
	return nil
}

// DeleteServiceAccount removes a service account.
func (s *Store) DeleteServiceAccount(accessKey string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.svc[accessKey]; !ok {
		return fmt.Errorf("%w: service account %q", ErrNotFound, accessKey)
	}
	delete(s.svc, accessKey)
	return nil
}

// policiesKnown reports an error for the first name the store does not know.
// The caller must hold the lock.
func (s *Store) policiesKnown(names []string) error {
	for _, name := range names {
		if _, ok := s.policies[name]; !ok {
			return fmt.Errorf("%w: %q", ErrUnknownPolicy, name)
		}
	}
	return nil
}

// Secret returns the secret key for an access key (root, user, or service
// account) for signature verification, and whether the key exists.
func (s *Store) Secret(accessKey string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if accessKey == s.rootKey {
		return s.rootSec, true
	}
	if u, ok := s.users[accessKey]; ok {
		return u.SecretKey, true
	}
	if sa, ok := s.svc[accessKey]; ok {
		return sa.SecretKey, true
	}
	if sess, ok := s.sessions[accessKey]; ok {
		return sess.secretKey, true
	}
	return "", false
}

// userPolicies resolves a user's effective policy set: its own attached policies
// plus those of every group it belongs to, by name. The caller must hold the
// lock. Unknown names are skipped (attach-time validation keeps them out, and a
// policy deleted out from under an attachment must not deny the rest).
func (s *Store) userPolicies(u *User) []Policy {
	var out []Policy
	for _, name := range u.Policies {
		if p, ok := s.policies[name]; ok {
			out = append(out, p)
		}
	}
	for _, g := range u.Groups {
		grp, ok := s.groups[g]
		if !ok {
			continue
		}
		for _, name := range grp.Policies {
			if p, ok := s.policies[name]; ok {
				out = append(out, p)
			}
		}
	}
	return out
}

// principalAllows reports whether the identity named by a parent access key (root
// or a user) allows the request, ignoring any narrowing layer. It is the base of
// the intersection a service account or STS session restricts. The caller must
// hold the lock. ok is false when the parent access key names no such principal.
func (s *Store) principalAllows(parentKey string, req Request) (allowed, ok bool) {
	if parentKey == s.rootKey {
		return true, true
	}
	if u, found := s.users[parentKey]; found {
		return Evaluate(s.userPolicies(u), req), true
	}
	return false, false
}

// IsAllowed decides a request for the identity behind an access key, applying the
// right composition for each kind:
//
//   - Root is allowed everything.
//   - A user is allowed when its effective policy set (own + groups) allows.
//   - A service account is allowed when its parent's set allows AND, if it carries
//     an inline policy, that policy also allows — the intersection that makes an
//     inline policy able only to narrow, never widen, the parent's rights.
//   - An STS session is allowed when the identity it was assumed from allows AND,
//     if it carries a session policy, that policy also allows; an expired session
//     is denied with ErrExpired.
//
// An unknown access key is denied (false) with ErrNotFound, so a caller can tell
// "no such identity" from a known identity that was simply not granted the action.
func (s *Store) IsAllowed(accessKey string, req Request) (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if accessKey == s.rootKey {
		return true, nil
	}
	if u, ok := s.users[accessKey]; ok {
		return Evaluate(s.userPolicies(u), req), nil
	}
	if sa, ok := s.svc[accessKey]; ok {
		base, ok := s.principalAllows(sa.ParentUser, req)
		if !ok {
			// The parent was deleted without the child; deny rather than grant on a
			// dangling reference. DeleteUser removes children, so this is defensive.
			return false, fmt.Errorf("%w: parent user %q", ErrNotFound, sa.ParentUser)
		}
		if !base {
			return false, nil
		}
		if sa.Inline != nil {
			return Evaluate([]Policy{*sa.Inline}, req), nil
		}
		return true, nil
	}
	if sess, ok := s.sessions[accessKey]; ok {
		if !s.now().Before(sess.expiry) {
			return false, fmt.Errorf("%w: access key %q", ErrExpired, accessKey)
		}
		base, ok := s.principalAllows(sess.parent, req)
		if !ok {
			return false, fmt.Errorf("%w: session principal %q", ErrNotFound, sess.parent)
		}
		if !base {
			return false, nil
		}
		if sess.policy != nil {
			return Evaluate([]Policy{*sess.policy}, req), nil
		}
		return true, nil
	}
	return false, fmt.Errorf("%w: access key %q", ErrNotFound, accessKey)
}
