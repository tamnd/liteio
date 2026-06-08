// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"slices"
	"testing"
)

func TestUsersAndUser(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "bob", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	if got := s.Users(); !slices.Equal(got, []string{"alice", "bob"}) {
		t.Fatalf("Users() = %v, want sorted [alice bob]", got)
	}
	u, ok := s.User("bob")
	if !ok {
		t.Fatal("User(bob) missing")
	}
	if u.AccessKey != "bob" || !slices.Equal(u.Policies, []string{"readonly"}) {
		t.Fatalf("User(bob) = %+v", u)
	}
	if u.SecretKey != "" {
		t.Fatal("User must not return the secret key")
	}
	// Mutating the returned slice must not touch the store.
	u.Policies[0] = "tampered"
	again, _ := s.User("bob")
	if again.Policies[0] != "readonly" {
		t.Fatal("returned policies must be a copy")
	}
	if _, ok := s.User("ghost"); ok {
		t.Fatal("User(ghost) should be missing")
	}
}

func TestGroupsAndGroup(t *testing.T) {
	s := newStore(t)
	if err := s.AddGroup(Group{Name: "ops", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if got := s.Groups(); !slices.Equal(got, []string{"ops"}) {
		t.Fatalf("Groups() = %v", got)
	}
	g, ok := s.Group("ops")
	if !ok || !slices.Equal(g.Policies, []string{"readwrite"}) {
		t.Fatalf("Group(ops) = %+v ok=%v", g, ok)
	}
	if _, ok := s.Group("none"); ok {
		t.Fatal("Group(none) should be missing")
	}
}

func TestGetPolicy(t *testing.T) {
	s := newStore(t)
	// A canned policy is readable.
	if _, ok := s.GetPolicy("readonly"); !ok {
		t.Fatal("canned readonly should be readable")
	}
	custom := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := s.AddPolicy("mine", custom); err != nil {
		t.Fatal(err)
	}
	p, ok := s.GetPolicy("mine")
	if !ok || len(p.Statements) != 1 {
		t.Fatalf("GetPolicy(mine) = %+v ok=%v", p, ok)
	}
	if _, ok := s.GetPolicy("nope"); ok {
		t.Fatal("GetPolicy(nope) should be missing")
	}
}

func TestSetPolicyCreatesAndReplaces(t *testing.T) {
	s := newStore(t)
	v1 := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := s.SetPolicy("p", v1); err != nil {
		t.Fatal(err)
	}
	// Replace with a different document under the same name.
	v2 := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := s.SetPolicy("p", v2); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if err := s.AddUser(User{AccessKey: "u", SecretKey: "s", Policies: []string{"p"}}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "u", put("b")) {
		t.Fatal("after replacing p with s3:* the user should write")
	}
	// A canned name cannot be redefined.
	if err := s.SetPolicy("readonly", v2); !errors.Is(err, ErrExists) {
		t.Fatalf("SetPolicy(readonly) = %v, want ErrExists", err)
	}
}

func TestDeletePolicy(t *testing.T) {
	s := newStore(t)
	custom := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := s.AddPolicy("mine", custom); err != nil {
		t.Fatal(err)
	}
	// A policy still attached can be deleted; the attachment just stops granting.
	if err := s.AddUser(User{AccessKey: "u", SecretKey: "s", Policies: []string{"mine"}}); err != nil {
		t.Fatal(err)
	}
	if !allowed(t, s, "u", get("b")) {
		t.Fatal("user should read before deletion")
	}
	if err := s.DeletePolicy("mine"); err != nil {
		t.Fatal(err)
	}
	if allowed(t, s, "u", get("b")) {
		t.Fatal("after deleting the policy the attachment must stop granting")
	}
	if _, ok := s.GetPolicy("mine"); ok {
		t.Fatal("deleted policy should be gone")
	}
	if err := s.DeletePolicy("mine"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete absent = %v, want ErrNotFound", err)
	}
	if err := s.DeletePolicy("readonly"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delete canned = %v, want ErrInvalid", err)
	}
}

func TestServiceAccountsFor(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app2", SecretKey: "x", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app1", SecretKey: "x", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	keys, err := s.ServiceAccountsFor("alice")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(keys, []string{"app1", "app2"}) {
		t.Fatalf("ServiceAccountsFor(alice) = %v, want sorted [app1 app2]", keys)
	}
	if _, err := s.ServiceAccountsFor("ghost"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ServiceAccountsFor(ghost) = %v, want ErrNotFound", err)
	}
}
