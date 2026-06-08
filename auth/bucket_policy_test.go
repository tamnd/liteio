// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"
	"testing"
)

func mustParseBucket(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := ParseBucketPolicy([]byte(doc))
	if err != nil {
		t.Fatalf("ParseBucketPolicy: %v\ndoc: %s", err, doc)
	}
	return p
}

func TestParseBucketPolicyRequiresPrincipal(t *testing.T) {
	// A statement with no Principal is rejected.
	_, err := ParseBucketPolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`))
	if err == nil {
		t.Fatal("a bucket-policy statement without a Principal must be rejected")
	}
}

func TestParsePrincipalForms(t *testing.T) {
	// Bare "*".
	p := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !p.Statements[0].parsedPrincipal.anyone {
		t.Fatal(`Principal "*" should be anyone`)
	}
	// {"AWS":"*"}.
	p = mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"*"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !p.Statements[0].parsedPrincipal.anyone {
		t.Fatal(`Principal {"AWS":"*"} should be anyone`)
	}
	// {"AWS":["alice","bob"]}.
	p = mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":["alice","bob"]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	pr := p.Statements[0].parsedPrincipal
	if pr.anyone || len(pr.aws) != 2 {
		t.Fatalf("named principal parsed wrong: %+v", pr)
	}
}

func TestParsePrincipalRejectsBadForms(t *testing.T) {
	cases := map[string]string{
		"bare non-star":  `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"alice","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"no AWS key":     `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"CanonicalUser":"x"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
		"empty AWS list": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"AWS":[]},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseBucketPolicy([]byte(doc)); err == nil {
				t.Fatalf("expected rejection of %s", name)
			}
		})
	}
}

func TestEvaluateBucketPolicyAnonymous(t *testing.T) {
	pub := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	// Anonymous read is granted by the wildcard principal.
	if EvaluateBucketPolicy(pub, Anonymous, get("b")) != DecisionAllow {
		t.Fatal("anonymous read should be allowed by a public-read bucket policy")
	}
	// Anonymous write is not in the policy.
	if EvaluateBucketPolicy(pub, Anonymous, put("b")) != DecisionNone {
		t.Fatal("anonymous write should not match")
	}
}

func TestEvaluateBucketPolicyNamedPrincipal(t *testing.T) {
	p := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if EvaluateBucketPolicy(p, "alice", get("b")) != DecisionAllow {
		t.Fatal("named principal alice should be allowed")
	}
	if EvaluateBucketPolicy(p, "bob", get("b")) != DecisionNone {
		t.Fatal("a different principal must not match a named grant")
	}
	// A named grant never matches anonymous.
	if EvaluateBucketPolicy(p, Anonymous, get("b")) != DecisionNone {
		t.Fatal("anonymous must not match a named (non-wildcard) principal")
	}
}

func TestEvaluateBucketPolicyExplicitDeny(t *testing.T) {
	p := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/secret/*"}]}`)
	if EvaluateBucketPolicy(p, Anonymous, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "public/x")}) != DecisionAllow {
		t.Fatal("public object should be readable")
	}
	if EvaluateBucketPolicy(p, Anonymous, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "secret/x")}) != DecisionDeny {
		t.Fatal("the explicit deny must protect the secret prefix")
	}
}

func TestAuthorizeCombinesIdentityAndBucket(t *testing.T) {
	bucket := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	ro, _ := CannedPolicy("readonly")

	// Anonymous: no identity, granted by the bucket policy alone.
	if !Authorize(nil, &bucket, Anonymous, get("b")) {
		t.Fatal("anonymous read should be granted by the public bucket policy")
	}
	if Authorize(nil, &bucket, Anonymous, put("b")) {
		t.Fatal("anonymous write is in neither path")
	}
	// Anonymous with no bucket policy is denied (deny by default).
	if Authorize(nil, nil, Anonymous, get("b")) {
		t.Fatal("anonymous with no bucket policy must be denied")
	}

	// Authenticated: an allow in the identity path grants even with no bucket grant.
	if !Authorize([]Policy{ro}, nil, "alice", get("other")) {
		t.Fatal("identity readonly should grant a read with no bucket policy")
	}
	// An allow in the bucket path grants even when the identity does not.
	writeReq := put("b")
	if Authorize([]Policy{ro}, &bucket, "alice", writeReq) {
		t.Fatal("neither path allows a write")
	}
	bucketWrite := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":{"AWS":"alice"},"Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !Authorize([]Policy{ro}, &bucketWrite, "alice", writeReq) {
		t.Fatal("a bucket-policy allow should grant a write the identity lacks")
	}
}

func TestAuthorizeExplicitDenyWinsAcrossPaths(t *testing.T) {
	rw, _ := CannedPolicy("readwrite")
	// The identity allows everything; the bucket policy denies one prefix.
	bucketDeny := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/locked/*"}]}`)
	if !Authorize([]Policy{rw}, &bucketDeny, "alice", Request{Action: "s3:GetObject", Resource: ObjectARN("b", "open/x")}) {
		t.Fatal("readwrite identity should still read outside the denied prefix")
	}
	if Authorize([]Policy{rw}, &bucketDeny, "alice", Request{Action: "s3:GetObject", Resource: ObjectARN("b", "locked/x")}) {
		t.Fatal("a bucket-policy deny must override the identity allow")
	}

	// And the reverse: an identity deny overrides a bucket allow.
	bucketAllow := mustParseBucket(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	idDeny := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if Authorize([]Policy{idDeny}, &bucketAllow, "alice", get("b")) {
		t.Fatal("an identity deny must override a bucket-policy allow")
	}
}

func TestPublicReadPolicy(t *testing.T) {
	p := PublicReadPolicy("media")
	if EvaluateBucketPolicy(p, Anonymous, Request{Action: "s3:GetObject", Resource: ObjectARN("media", "logo.png")}) != DecisionAllow {
		t.Fatal("public-read should grant anonymous GetObject")
	}
	if EvaluateBucketPolicy(p, Anonymous, Request{Action: "s3:PutObject", Resource: ObjectARN("media", "x")}) != DecisionNone {
		t.Fatal("public-read must not grant writes")
	}
	if EvaluateBucketPolicy(p, Anonymous, Request{Action: "s3:GetObject", Resource: ObjectARN("other", "x")}) != DecisionNone {
		t.Fatal("public-read must be scoped to its bucket")
	}
}

func TestBucketPolicyValidationReusesIdentityRules(t *testing.T) {
	// A bad version is rejected by the shared ParsePolicy validation, with the
	// Principal still present so we know it reached that check.
	_, err := ParseBucketPolicy([]byte(`{"Version":"2020-01-01","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`))
	if err == nil || !strings.Contains(err.Error(), "version") {
		t.Fatalf("expected a version error, got %v", err)
	}
}

func BenchmarkAuthorize(b *testing.B) {
	rw, _ := CannedPolicy("readwrite")
	bucket, err := ParseBucketPolicy([]byte(`{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Principal":"*","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"},
		{"Effect":"Deny","Principal":"*","Action":"s3:*","Resource":"arn:aws:s3:::b/locked/*"}]}`))
	if err != nil {
		b.Fatal(err)
	}
	req := Request{Action: "s3:GetObject", Resource: ObjectARN("b", "open/file.txt")}
	for b.Loop() {
		_ = Authorize([]Policy{rw}, &bucket, "alice", req)
	}
}
