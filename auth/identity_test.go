// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"slices"
	"testing"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	return NewStore("root", "rootsecret")
}

func get(b string) Request { return Request{Action: "s3:GetObject", Resource: ObjectARN(b, "k")} }
func put(b string) Request { return Request{Action: "s3:PutObject", Resource: ObjectARN(b, "k")} }
func del(b string) Request { return Request{Action: "s3:DeleteObject", Resource: ObjectARN(b, "k")} }

func allowed(t *testing.T, s *Store, key string, req Request) bool {
	t.Helper()
	ok, err := s.IsAllowed(key, req)
	if err != nil {
		t.Fatalf("IsAllowed(%q): %v", key, err)
	}
	return ok
}

func TestStoreSeedsCannedPolicies(t *testing.T) {
	s := newStore(t)
	got := s.PolicyNames()
	for _, name := range CannedPolicyNames() {
		if !slices.Contains(got, name) {
			t.Fatalf("canned policy %q not seeded; have %v", name, got)
		}
	}
}

func TestRootAllowedEverything(t *testing.T) {
	s := newStore(t)
	if !allowed(t, s, "root", del("anything")) {
		t.Fatal("root must be allowed everything")
	}
	if sec, ok := s.Secret("root"); !ok || sec != "rootsecret" {
		t.Fatalf("root secret = %q,%v", sec, ok)
	}
}

func TestUnknownAccessKeyDenied(t *testing.T) {
	s := newStore(t)
	ok, err := s.IsAllowed("ghost", get("b"))
	if ok {
		t.Fatal("unknown key must be denied")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestUserWithAttachedPolicy(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "alice", get("b")) {
		t.Fatal("readonly user should read")
	}
	if allowed(t, s, "alice", put("b")) {
		t.Fatal("readonly user must not write")
	}
}

func TestAddUserRejectsDuplicateAndRootCollision(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s2"}); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate user err = %v, want ErrExists", err)
	}
	if err := s.AddUser(User{AccessKey: "root", SecretKey: "s2"}); !errors.Is(err, ErrExists) {
		t.Fatalf("root-collision err = %v, want ErrExists", err)
	}
}

func TestAddUserRejectsUnknownPolicy(t *testing.T) {
	s := newStore(t)
	err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"nope"}})
	if !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("err = %v, want ErrUnknownPolicy", err)
	}
}

func TestAttachAndDetachUserPolicy(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	// No policy yet: deny.
	if allowed(t, s, "alice", get("b")) {
		t.Fatal("a user with no policy must be denied")
	}
	if err := s.AttachUserPolicy("alice", "readwrite"); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "alice", put("b")) {
		t.Fatal("after attaching readwrite the user should write")
	}
	if err := s.DetachUserPolicy("alice", "readwrite"); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "alice", put("b")) {
		t.Fatal("after detaching the user must be denied again")
	}
	if err := s.AttachUserPolicy("alice", "nope"); !errors.Is(err, ErrUnknownPolicy) {
		t.Fatalf("attaching unknown policy err = %v", err)
	}
}

func TestGroupPoliciesApplyToMembers(t *testing.T) {
	s := newStore(t)
	if err := s.AddGroup(Group{Name: "writers", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{AccessKey: "bob", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "bob", put("b")) {
		t.Fatal("non-member must not inherit group policy")
	}
	if err := s.AddUserToGroup("bob", "writers"); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "bob", put("b")) {
		t.Fatal("group member should inherit readwrite")
	}
	// Membership is a set: a repeat add does not double anything and stays valid.
	if err := s.AddUserToGroup("bob", "writers"); err != nil {
		t.Fatal(err)
	}
	members, err := s.GroupMembers("writers")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(members, []string{"bob"}) {
		t.Fatalf("members = %v, want [bob]", members)
	}
	if err := s.RemoveUserFromGroup("bob", "writers"); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "bob", put("b")) {
		t.Fatal("after leaving the group the member loses the policy")
	}
}

func TestDeleteGroupDropsMembership(t *testing.T) {
	s := newStore(t)
	if err := s.AddGroup(Group{Name: "g", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{AccessKey: "u", SecretKey: "s", Groups: []string{"g"}}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "u", put("b")) {
		t.Fatal("member should have the group policy")
	}
	if err := s.DeleteGroup("g"); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "u", put("b")) {
		t.Fatal("deleting the group must drop the inherited policy")
	}
	// The user must not be left referencing the dead group.
	if _, err := s.GroupMembers("g"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("group should be gone, err = %v", err)
	}
}

func TestServiceAccountInheritsParent(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "app", put("b")) {
		t.Fatal("a service account with no inline policy inherits the parent's full rights")
	}
	if sec, ok := s.Secret("app"); !ok || sec != "as" {
		t.Fatalf("service account secret = %q,%v", sec, ok)
	}
}

func TestServiceAccountInlineCanOnlyNarrow(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	// Inline policy grants only reads on one bucket.
	inline := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"}]}`)
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice", Inline: &inline}); err != nil {
		t.Fatal(err)
	}
	// Reads on data are in both the parent (readwrite) and the inline: allowed.
	if !allowed(t, s, "app", get("data")) {
		t.Fatal("inline read on data should be allowed (parent also allows)")
	}
	// Writes are in the parent but NOT the inline: the intersection denies.
	if allowed(t, s, "app", put("data")) {
		t.Fatal("inline policy must narrow: no write even though parent allows")
	}
	// Reads on another bucket are not in the inline: denied.
	if allowed(t, s, "app", get("other")) {
		t.Fatal("inline scoped to data/* must not reach another bucket")
	}
}

func TestServiceAccountInlineCannotWidenBeyondParent(t *testing.T) {
	s := newStore(t)
	// Parent can only read.
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		t.Fatal(err)
	}
	// Inline tries to grant writes the parent never had.
	inline := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::*"}]}`)
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice", Inline: &inline}); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "app", put("b")) {
		t.Fatal("an inline allow must not widen past the parent's rights")
	}
	if !allowed(t, s, "app", get("b")) {
		t.Fatal("a read both parent and inline allow should still pass")
	}
}

func TestDeleteUserRemovesServiceAccounts(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteUser("alice"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Secret("app"); ok {
		t.Fatal("deleting the parent must remove its service accounts")
	}
	if _, err := s.IsAllowed("app", get("b")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("orphaned service account should be gone, err = %v", err)
	}
}

func TestAddServiceAccountRejectsMissingParent(t *testing.T) {
	s := newStore(t)
	err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "ghost"})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAddPolicyRejectsCannedNameCollision(t *testing.T) {
	s := newStore(t)
	custom := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::*"}]}`)
	if err := s.AddPolicy("readonly", custom); !errors.Is(err, ErrExists) {
		t.Fatalf("err = %v, want ErrExists (must not shadow a canned policy)", err)
	}
	if err := s.AddPolicy("mine", custom); err != nil {
		t.Fatalf("adding a fresh-named policy: %v", err)
	}
	if err := s.AddUser(User{AccessKey: "u", SecretKey: "s", Policies: []string{"mine"}}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "u", get("b")) {
		t.Fatal("a user with the custom policy should read")
	}
}

func TestStoreConcurrentAccess(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		for i := range 1000 {
			name := "g" + string(rune('a'+i%26))
			_ = s.AddGroup(Group{Name: name})
			_ = s.AddUserToGroup("alice", name)
			_ = s.RemoveUserFromGroup("alice", name)
		}
		close(done)
	}()
	for range 1000 {
		_, _ = s.IsAllowed("alice", get("b"))
		_, _ = s.Secret("alice")
	}
	<-done
}

func BenchmarkIsAllowedUser(b *testing.B) {
	s := NewStore("root", "rootsecret")
	_ = s.AddGroup(Group{Name: "writers", Policies: []string{"readwrite"}})
	_ = s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}, Groups: []string{"writers"}})
	req := get("data")
	for b.Loop() {
		_, _ = s.IsAllowed("alice", req)
	}
}
