// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"strings"
	"testing"
)

func mustParse(t *testing.T, doc string) Policy {
	t.Helper()
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePolicy: %v\ndoc: %s", err, doc)
	}
	return p
}

func TestParsePolicyStringOrArray(t *testing.T) {
	// Action and Resource given as bare strings (the AWS single-value shorthand).
	p := mustParse(t, `{
		"Version": "2012-10-17",
		"Statement": [{"Effect": "Allow", "Action": "s3:GetObject", "Resource": "arn:aws:s3:::b/*"}]
	}`)
	st := p.Statements[0]
	if len(st.Actions) != 1 || st.Actions[0] != "s3:GetObject" {
		t.Fatalf("Action = %v, want [s3:GetObject]", st.Actions)
	}
	if len(st.Resources) != 1 || st.Resources[0] != "arn:aws:s3:::b/*" {
		t.Fatalf("Resource = %v, want [arn:aws:s3:::b/*]", st.Resources)
	}

	// And as arrays.
	p = mustParse(t, `{
		"Version": "2012-10-17",
		"Statement": [{"Effect": "Allow", "Action": ["s3:GetObject","s3:PutObject"], "Resource": ["arn:aws:s3:::b/*"]}]
	}`)
	if len(p.Statements[0].Actions) != 2 {
		t.Fatalf("Action array len = %d, want 2", len(p.Statements[0].Actions))
	}
}

func TestParsePolicyRejectsBadDocuments(t *testing.T) {
	cases := map[string]string{
		"unknown version":      `{"Version":"2020-01-01","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::*"}]}`,
		"no statements":        `{"Version":"2012-10-17","Statement":[]}`,
		"bad effect":           `{"Version":"2012-10-17","Statement":[{"Effect":"Permit","Action":"s3:*","Resource":"arn:aws:s3:::*"}]}`,
		"no action":            `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Resource":"arn:aws:s3:::*"}]}`,
		"action and notaction": `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","NotAction":"s3:DeleteObject","Resource":"arn:aws:s3:::*"}]}`,
		"unknown field":        `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::*","Bogus":1}]}`,
		"not json":             `{not json`,
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParsePolicy([]byte(doc)); err == nil {
				t.Fatalf("ParsePolicy accepted an invalid document (%s)", name)
			}
		})
	}
}

func TestEvaluateDenyByDefault(t *testing.T) {
	// An empty policy set denies everything.
	if Evaluate(nil, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "k")}) {
		t.Fatal("no policy must deny by default")
	}
	// A policy that grants an unrelated action denies the asked one.
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:PutObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "k")}) {
		t.Fatal("an unrelated allow must not grant the asked action")
	}
}

func TestEvaluateAllow(t *testing.T) {
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::data/*"]}]}`)
	if !Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "reports/q1.csv")}) {
		t.Fatal("matching allow must grant")
	}
	// Resource outside the prefix is denied.
	if Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("other", "x")}) {
		t.Fatal("allow scoped to data/* must not reach another bucket")
	}
}

func TestEvaluateExplicitDenyWins(t *testing.T) {
	allow := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:*","Resource":"arn:aws:s3:::data/*"}]}`)
	deny := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::data/*"}]}`)
	req := Request{Action: "s3:DeleteObject", Resource: ObjectARN("data", "k")}
	// Allow alone grants; the deny in a second policy overrides it.
	if !Evaluate([]Policy{allow}, req) {
		t.Fatal("wildcard allow should grant delete")
	}
	if Evaluate([]Policy{allow, deny}, req) {
		t.Fatal("explicit deny must override the allow")
	}
	// Order must not matter.
	if Evaluate([]Policy{deny, allow}, req) {
		t.Fatal("explicit deny must override regardless of policy order")
	}
	// A non-denied action under the same allow still works.
	if !Evaluate([]Policy{allow, deny}, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "k")}) {
		t.Fatal("the deny must not bleed onto other actions")
	}
}

func TestEvaluateNotAction(t *testing.T) {
	// Allow everything except deletes.
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","NotAction":"s3:DeleteObject","Resource":"arn:aws:s3:::b/*"}]}`)
	if !Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "k")}) {
		t.Fatal("NotAction allow should grant a non-excluded action")
	}
	if Evaluate([]Policy{p}, Request{Action: "s3:DeleteObject", Resource: ObjectARN("b", "k")}) {
		t.Fatal("NotAction allow must not grant the excluded action")
	}
}

func TestEvaluateNotResource(t *testing.T) {
	// Allow reads on every bucket except the secret one.
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","NotResource":"arn:aws:s3:::secret/*"}]}`)
	if !Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("public", "k")}) {
		t.Fatal("NotResource allow should grant outside the excluded prefix")
	}
	if Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("secret", "k")}) {
		t.Fatal("NotResource allow must not reach the excluded prefix")
	}
}

func TestEvaluateActionCaseInsensitive(t *testing.T) {
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"S3:GETOBJECT","Resource":"arn:aws:s3:::b/*"}]}`)
	if !Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("b", "k")}) {
		t.Fatal("action matching must be case-insensitive")
	}
}

func TestEvaluateResourceCaseSensitive(t *testing.T) {
	p := mustParse(t, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::Data/*"}]}`)
	if Evaluate([]Policy{p}, Request{Action: "s3:GetObject", Resource: ObjectARN("data", "k")}) {
		t.Fatal("resource matching must be case-sensitive (Data != data)")
	}
}

func TestWildcardMatch(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "", true},
		{"*", "anything", true},
		{"s3:*", "s3:getobject", true},
		{"s3:get*", "s3:getobject", true},
		{"s3:get*", "s3:putobject", false},
		{"data*", "data", true},
		{"data*", "data_private", true},
		{"data*", "datum", false},
		{"arn:aws:s3:::data/*", "arn:aws:s3:::data/reports/x", true},
		{"arn:aws:s3:::data/*", "arn:aws:s3:::data", false},
		{"a?c", "abc", true},
		{"a?c", "ac", false},
		{"a?c", "abbc", false},
		{"*.*", "x.y", true},
		{"**a**", "ba", true},
		{"abc", "abc", true},
		{"abc", "abcd", false},
	}
	for _, c := range cases {
		if got := wildcardMatch(c.pattern, c.s); got != c.want {
			t.Errorf("wildcardMatch(%q, %q) = %v, want %v", c.pattern, c.s, got, c.want)
		}
	}
}

func TestCannedPoliciesParse(t *testing.T) {
	names := CannedPolicyNames()
	want := []string{"consoleAdmin", "diagnostics", "readonly", "readwrite", "writeonly"}
	if strings.Join(names, ",") != strings.Join(want, ",") {
		t.Fatalf("CannedPolicyNames = %v, want %v", names, want)
	}
	for _, name := range names {
		if _, ok := CannedPolicy(name); !ok {
			t.Fatalf("CannedPolicy(%q) not found", name)
		}
	}
	if _, ok := CannedPolicy("nope"); ok {
		t.Fatal("CannedPolicy must report a missing policy")
	}
}

func TestCannedPolicySemantics(t *testing.T) {
	get := Request{Action: "s3:GetObject", Resource: ObjectARN("b", "k")}
	put := Request{Action: "s3:PutObject", Resource: ObjectARN("b", "k")}
	del := Request{Action: "s3:DeleteObject", Resource: ObjectARN("b", "k")}
	list := Request{Action: "s3:ListBucket", Resource: BucketARN("b")}
	admin := Request{Action: "admin:ServerInfo", Resource: BucketARN("anything")}

	for _, tc := range []struct {
		policy             string
		get, put, del, adm bool
	}{
		{"readwrite", true, true, true, false},
		{"readonly", true, false, false, false},
		{"writeonly", false, true, false, false},
		{"diagnostics", false, false, false, true},
		{"consoleAdmin", true, true, true, true},
	} {
		t.Run(tc.policy, func(t *testing.T) {
			p, _ := CannedPolicy(tc.policy)
			ps := []Policy{p}
			if got := Evaluate(ps, get); got != tc.get {
				t.Errorf("%s GetObject = %v, want %v", tc.policy, got, tc.get)
			}
			if got := Evaluate(ps, put); got != tc.put {
				t.Errorf("%s PutObject = %v, want %v", tc.policy, got, tc.put)
			}
			if got := Evaluate(ps, del); got != tc.del {
				t.Errorf("%s DeleteObject = %v, want %v", tc.policy, got, tc.del)
			}
			if got := Evaluate(ps, admin); got != tc.adm {
				t.Errorf("%s admin:ServerInfo = %v, want %v", tc.policy, got, tc.adm)
			}
		})
	}

	// readonly grants ListBucket so a read-only client can browse.
	if ro, _ := CannedPolicy("readonly"); !Evaluate([]Policy{ro}, list) {
		t.Fatal("readonly should grant ListBucket")
	}
}

func BenchmarkEvaluate(b *testing.B) {
	// A realistic identity: a canned readwrite allow plus a couple of scoped denies.
	rw, _ := CannedPolicy("readwrite")
	deny := mustParseB(b, `{"Version":"2012-10-17","Statement":[
		{"Effect":"Deny","Action":"s3:DeleteObject","Resource":"arn:aws:s3:::locked/*"},
		{"Effect":"Deny","Action":"s3:*","Resource":"arn:aws:s3:::quarantine/*"}]}`)
	policies := []Policy{rw, deny}
	req := Request{Action: "s3:GetObject", Resource: ObjectARN("data", "reports/2026/q2/summary.csv")}
	for b.Loop() {
		_ = Evaluate(policies, req)
	}
}

func mustParseB(b *testing.B, doc string) Policy {
	b.Helper()
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		b.Fatalf("ParsePolicy: %v", err)
	}
	return p
}
