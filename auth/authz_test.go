// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"errors"
	"testing"
	"time"
)

// authz checks AuthorizeS3 and fails on an unexpected error, returning the grant.
func authz(t *testing.T, s *Store, key string, bp *Policy, req Request) bool {
	t.Helper()
	ok, err := s.AuthorizeS3(key, bp, req)
	if err != nil {
		t.Fatalf("AuthorizeS3(%q): %v", key, err)
	}
	return ok
}

func TestAuthorizeS3RootBypassesBucketPolicy(t *testing.T) {
	s := newStore(t)
	// A bucket policy that denies everyone must not lock out the root superuser.
	deny := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/*"}]}`)
	if !authz(t, s, "root", &deny, get("b")) {
		t.Fatal("root must be allowed everything, even past a deny-all bucket policy")
	}
	if !authz(t, s, "root", nil, del("anything")) {
		t.Fatal("root must be allowed everything with no bucket policy")
	}
}

func TestAuthorizeS3UnknownKey(t *testing.T) {
	s := newStore(t)
	ok, err := s.AuthorizeS3("ghost", nil, get("b"))
	if ok {
		t.Fatal("unknown key must be denied")
	}
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestAuthorizeS3UserIdentityPath(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readonly"}}); err != nil {
		t.Fatal(err)
	}
	// No bucket policy: the identity decides.
	if !authz(t, s, "alice", nil, get("b")) {
		t.Fatal("readonly user should read with no bucket policy")
	}
	if authz(t, s, "alice", nil, put("b")) {
		t.Fatal("readonly user must not write")
	}
}

func TestAuthorizeS3BucketPolicyGrantsBeyondIdentity(t *testing.T) {
	s := newStore(t)
	// The user has no identity policy at all.
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	// A bucket policy naming alice grants the write the identity never would.
	bp := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !authz(t, s, "alice", &bp, put("b")) {
		t.Fatal("a bucket policy granting alice should allow the write")
	}
	// An action the bucket policy does not cover is still denied.
	if authz(t, s, "alice", &bp, del("b")) {
		t.Fatal("the bucket policy grants only PutObject; delete must be denied")
	}
}

func TestAuthorizeS3ExplicitDenyWins(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	// Identity allows reads, bucket policy explicitly denies them: deny wins.
	bucketDeny := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if authz(t, s, "alice", &bucketDeny, get("b")) {
		t.Fatal("an explicit bucket Deny must override an identity Allow")
	}

	// The mirror: identity denies, bucket allows. An identity Deny is final too.
	denyPol := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if err := s.AddPolicy("denyget", denyPol); err != nil {
		t.Fatal(err)
	}
	if err := s.AddUser(User{AccessKey: "bob", SecretKey: "s", Policies: []string{"readwrite", "denyget"}}); err != nil {
		t.Fatal(err)
	}
	bucketAllow := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if authz(t, s, "bob", &bucketAllow, get("b")) {
		t.Fatal("an explicit identity Deny must override a bucket Allow")
	}
}

func TestAuthorizeS3Anonymous(t *testing.T) {
	s := newStore(t)
	// No bucket policy: anonymous is denied.
	if authz(t, s, "", nil, get("b")) {
		t.Fatal("anonymous must be denied with no bucket policy")
	}
	// A wildcard bucket policy grants the anonymous caller.
	public := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !authz(t, s, "", &public, get("b")) {
		t.Fatal("a wildcard bucket policy should grant the anonymous caller")
	}
	if authz(t, s, "", &public, put("b")) {
		t.Fatal("the public policy grants only reads; anonymous write must be denied")
	}
	// A bucket policy naming a real principal does not match anonymous.
	named := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if authz(t, s, "", &named, get("b")) {
		t.Fatal("a named-principal policy must not grant anonymous")
	}
}

func TestAuthorizeS3ServiceAccountPresentsParentToBucketPolicy(t *testing.T) {
	s := newStore(t)
	// Parent has no identity policy; a bucket policy names the parent user.
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s"}); err != nil {
		t.Fatal(err)
	}
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	bp := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	// The service account presents alice to the bucket policy, so the grant applies.
	if !authz(t, s, "app", &bp, get("b")) {
		t.Fatal("a service account should present its parent to the bucket policy")
	}
}

func TestAuthorizeS3ServiceAccountInlineCapsBucketGrant(t *testing.T) {
	s := newStore(t)
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	// The inline ceiling allows reads on data/* only.
	inline := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"}]}`)
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice", Inline: &inline}); err != nil {
		t.Fatal(err)
	}
	// A bucket policy grants writes on data, but the inline ceiling has no write:
	// the request falls outside the ceiling, so even the bucket grant is denied.
	bp := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:PutObject","Resource":"arn:aws:s3:::data/*"}]}`)
	if authz(t, s, "app", &bp, put("data")) {
		t.Fatal("the inline ceiling must cap a bucket grant: no write outside the ceiling")
	}
	// A read on data is within the ceiling and within the parent: it stands.
	if !authz(t, s, "app", &bp, get("data")) {
		t.Fatal("a read inside the inline ceiling should be allowed")
	}
	// A read on another bucket is outside the ceiling: denied.
	if authz(t, s, "app", nil, get("other")) {
		t.Fatal("a read outside the inline ceiling must be denied")
	}
}

func TestAuthorizeS3SessionCeilingAndExpiry(t *testing.T) {
	s, clock := stsStore(t) // alice/readwrite, controllable clock
	scoped := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::data/*"}]}`)
	sess, err := s.AssumeRole("alice", &scoped, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	// Within the session policy and within the parent: allowed.
	if !authz(t, s, sess.AccessKey, nil, get("data")) {
		t.Fatal("a read inside the session policy should be allowed")
	}
	// A bucket policy granting a write cannot exceed the session ceiling.
	bp := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::data/*"}]}`)
	if authz(t, s, sess.AccessKey, &bp, put("data")) {
		t.Fatal("the session ceiling must cap a bucket grant")
	}
	// Past expiry: ErrExpired.
	*clock = clock.Add(2 * time.Hour)
	ok, err := s.AuthorizeS3(sess.AccessKey, nil, get("data"))
	if ok || !errors.Is(err, ErrExpired) {
		t.Fatalf("expired session = %v,%v want false,ErrExpired", ok, err)
	}
}

func TestAuthorizeS3MatchesIsAllowedWithoutBucketPolicy(t *testing.T) {
	// With no bucket policy, AuthorizeS3 must agree with IsAllowed for every kind
	// of identity: the front-door combiner only adds the resource-policy path.
	s, _ := stsStore(t)
	if err := s.AddServiceAccount(ServiceAccount{AccessKey: "app", SecretKey: "as", ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	sess, err := s.AssumeRole("alice", nil, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{"root", "alice", "app", sess.AccessKey}
	reqs := []Request{get("b"), put("b"), del("b")}
	for _, k := range keys {
		for _, req := range reqs {
			want, errW := s.IsAllowed(k, req)
			got, errG := s.AuthorizeS3(k, nil, req)
			if want != got || (errW == nil) != (errG == nil) {
				t.Fatalf("key %q action %q: IsAllowed=%v,%v AuthorizeS3=%v,%v", k, req.Action, want, errW, got, errG)
			}
		}
	}
}

func BenchmarkAuthorizeS3User(b *testing.B) {
	s := NewStore("root", "rootsecret")
	if err := s.AddUser(User{AccessKey: "alice", SecretKey: "s", Policies: []string{"readwrite"}}); err != nil {
		b.Fatal(err)
	}
	bp, err := ParseBucketPolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`))
	if err != nil {
		b.Fatal(err)
	}
	req := get("b")
	b.ReportAllocs()
	for b.Loop() {
		if ok, err := s.AuthorizeS3("alice", &bp, req); !ok || err != nil {
			b.Fatalf("AuthorizeS3 = %v,%v", ok, err)
		}
	}
}
