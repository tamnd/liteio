// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"testing"
)

// mustConditions parses a Condition block (a JSON object) and fails the test if
// it does not parse. The argument is the value of a statement's "Condition"
// field, e.g. `{"StringEquals": {"s3:prefix": "home/"}}`.
func mustConditions(t *testing.T, doc string) conditionSet {
	t.Helper()
	cs, err := parseConditions(json.RawMessage(doc))
	if err != nil {
		t.Fatalf("parseConditions(%s): %v", doc, err)
	}
	return cs
}

func TestParseConditionsEmptyIsNil(t *testing.T) {
	for _, raw := range []json.RawMessage{nil, json.RawMessage("")} {
		cs, err := parseConditions(raw)
		if err != nil {
			t.Fatalf("parseConditions(empty): %v", err)
		}
		if cs != nil {
			t.Fatalf("parseConditions(empty) = %v, want nil", cs)
		}
	}
}

func TestParseConditionsRejectsUnknownOperator(t *testing.T) {
	bad := []string{
		`{"StringMatches": {"s3:prefix": "home/"}}`,  // not an operator
		`{"Stringequals": {"s3:prefix": "home/"}}`,   // wrong case
		`{"IpAddressLike": {"aws:SourceIp": "x"}}`,   // not an operator
		`{"NumericBetween": {"s3:max-keys": "100"}}`, // not an operator
	}
	for _, doc := range bad {
		if _, err := parseConditions(json.RawMessage(doc)); err == nil {
			t.Errorf("parseConditions(%s): want error, got nil", doc)
		}
	}
}

func TestParseConditionsRejectsMalformedShape(t *testing.T) {
	bad := []string{
		`{"StringEquals": "home/"}`,         // value must be key->values, not a string
		`{"StringEquals": ["home/"]}`,       // value must be an object
		`"StringEquals"`,                    // block must be an object
		`{"StringEquals": {"k": {"x": 1}}}`, // values must be string-or-array
	}
	for _, doc := range bad {
		if _, err := parseConditions(json.RawMessage(doc)); err == nil {
			t.Errorf("parseConditions(%s): want error, got nil", doc)
		}
	}
}

func TestParseConditionsNormalizesOperators(t *testing.T) {
	cs := mustConditions(t, `{
		"StringNotEquals":   {"s3:prefix": "secret/"},
		"StringLikeIfExists": {"aws:username": "admin*"}
	}`)
	byKey := map[string]condition{}
	for _, c := range cs {
		byKey[c.key] = c
	}
	if c := byKey["s3:prefix"]; c.op != "StringEquals" || !c.not || c.ifExists {
		t.Errorf("StringNotEquals normalized to %+v", c)
	}
	if c := byKey["aws:username"]; c.op != "StringLike" || c.not || !c.ifExists {
		t.Errorf("StringLikeIfExists normalized to %+v", c)
	}
}

func TestConditionStringEquals(t *testing.T) {
	cs := mustConditions(t, `{"StringEquals": {"s3:prefix": ["home/", "shared/"]}}`)
	if !cs.satisfied(map[string]string{"s3:prefix": "home/"}) {
		t.Error("home/ should match listed value")
	}
	if !cs.satisfied(map[string]string{"s3:prefix": "shared/"}) {
		t.Error("shared/ should match listed value")
	}
	if cs.satisfied(map[string]string{"s3:prefix": "other/"}) {
		t.Error("other/ should not match")
	}
	if cs.satisfied(map[string]string{"s3:prefix": "Home/"}) {
		t.Error("StringEquals is case-sensitive")
	}
}

func TestConditionStringEqualsIgnoreCase(t *testing.T) {
	cs := mustConditions(t, `{"StringEqualsIgnoreCase": {"aws:username": "Admin"}}`)
	if !cs.satisfied(map[string]string{"aws:username": "admin"}) {
		t.Error("admin should match Admin ignoring case")
	}
	if cs.satisfied(map[string]string{"aws:username": "root"}) {
		t.Error("root should not match")
	}
}

func TestConditionStringLike(t *testing.T) {
	cs := mustConditions(t, `{"StringLike": {"s3:prefix": "tenant-*/data/?"}}`)
	if !cs.satisfied(map[string]string{"s3:prefix": "tenant-7/data/x"}) {
		t.Error("glob should match")
	}
	if cs.satisfied(map[string]string{"s3:prefix": "tenant-7/data/xy"}) {
		t.Error("? matches exactly one character")
	}
}

func TestConditionStringNotEquals(t *testing.T) {
	cs := mustConditions(t, `{"StringNotEquals": {"s3:prefix": "secret/"}}`)
	if !cs.satisfied(map[string]string{"s3:prefix": "public/"}) {
		t.Error("public/ should satisfy NotEquals secret/")
	}
	if cs.satisfied(map[string]string{"s3:prefix": "secret/"}) {
		t.Error("secret/ should fail NotEquals secret/")
	}
}

func TestConditionBool(t *testing.T) {
	cs := mustConditions(t, `{"Bool": {"aws:SecureTransport": "true"}}`)
	if !cs.satisfied(map[string]string{"aws:SecureTransport": "true"}) {
		t.Error("true should match")
	}
	if cs.satisfied(map[string]string{"aws:SecureTransport": "false"}) {
		t.Error("false should not match true")
	}
	if cs.satisfied(map[string]string{"aws:SecureTransport": "notabool"}) {
		t.Error("unparseable bool should not match")
	}
}

func TestConditionIpAddress(t *testing.T) {
	cs := mustConditions(t, `{"IpAddress": {"aws:SourceIp": ["10.0.0.0/8", "192.168.1.5"]}}`)
	if !cs.satisfied(map[string]string{"aws:SourceIp": "10.1.2.3"}) {
		t.Error("10.1.2.3 is in 10.0.0.0/8")
	}
	if !cs.satisfied(map[string]string{"aws:SourceIp": "192.168.1.5"}) {
		t.Error("exact address should match")
	}
	if cs.satisfied(map[string]string{"aws:SourceIp": "172.16.0.1"}) {
		t.Error("172.16.0.1 is outside both")
	}
	if cs.satisfied(map[string]string{"aws:SourceIp": "not-an-ip"}) {
		t.Error("unparseable IP should not match")
	}
}

func TestConditionNotIpAddress(t *testing.T) {
	cs := mustConditions(t, `{"NotIpAddress": {"aws:SourceIp": "10.0.0.0/8"}}`)
	if !cs.satisfied(map[string]string{"aws:SourceIp": "8.8.8.8"}) {
		t.Error("8.8.8.8 is outside 10.0.0.0/8, NotIpAddress holds")
	}
	if cs.satisfied(map[string]string{"aws:SourceIp": "10.1.1.1"}) {
		t.Error("10.1.1.1 is inside, NotIpAddress fails")
	}
}

func TestConditionNumeric(t *testing.T) {
	cases := []struct {
		op   string
		val  string
		want bool
	}{
		{"NumericEquals", "100", true},
		{"NumericEquals", "99", false},
		{"NumericLessThan", "99", true},
		{"NumericLessThan", "100", false},
		{"NumericLessThanEquals", "100", true},
		{"NumericLessThanEquals", "101", false},
		{"NumericGreaterThan", "101", true},
		{"NumericGreaterThan", "100", false},
		{"NumericGreaterThanEquals", "100", true},
		{"NumericGreaterThanEquals", "99", false},
	}
	for _, tc := range cases {
		cs := mustConditions(t, `{"`+tc.op+`": {"s3:max-keys": "100"}}`)
		got := cs.satisfied(map[string]string{"s3:max-keys": tc.val})
		if got != tc.want {
			t.Errorf("%s 100 vs %s = %v, want %v", tc.op, tc.val, got, tc.want)
		}
	}
	cs := mustConditions(t, `{"NumericEquals": {"s3:max-keys": "100"}}`)
	if cs.satisfied(map[string]string{"s3:max-keys": "lots"}) {
		t.Error("unparseable number should not match")
	}
}

func TestConditionDate(t *testing.T) {
	const ref = "2026-06-08T00:00:00Z"
	cases := []struct {
		op   string
		val  string
		want bool
	}{
		{"DateEquals", "2026-06-08T00:00:00Z", true},
		{"DateEquals", "2026-06-09T00:00:00Z", false},
		{"DateLessThan", "2026-06-07T00:00:00Z", true},
		{"DateLessThan", "2026-06-08T00:00:00Z", false},
		{"DateLessThanEquals", "2026-06-08T00:00:00Z", true},
		{"DateGreaterThan", "2026-06-09T00:00:00Z", true},
		{"DateGreaterThan", "2026-06-08T00:00:00Z", false},
		{"DateGreaterThanEquals", "2026-06-08T00:00:00Z", true},
	}
	for _, tc := range cases {
		cs := mustConditions(t, `{"`+tc.op+`": {"aws:CurrentTime": "`+ref+`"}}`)
		got := cs.satisfied(map[string]string{"aws:CurrentTime": tc.val})
		if got != tc.want {
			t.Errorf("%s %s vs %s = %v, want %v", tc.op, tc.val, ref, got, tc.want)
		}
	}
	cs := mustConditions(t, `{"DateLessThan": {"aws:CurrentTime": "`+ref+`"}}`)
	if cs.satisfied(map[string]string{"aws:CurrentTime": "soon"}) {
		t.Error("unparseable date should not match")
	}
}

func TestConditionAbsentKey(t *testing.T) {
	empty := map[string]string{}

	// Positive operator: a missing key fails.
	pos := mustConditions(t, `{"StringEquals": {"s3:prefix": "home/"}}`)
	if pos.satisfied(empty) {
		t.Error("positive condition should fail when key is absent")
	}

	// Negated operator: a missing key passes (nothing to violate).
	neg := mustConditions(t, `{"StringNotEquals": {"s3:prefix": "secret/"}}`)
	if !neg.satisfied(empty) {
		t.Error("negated condition should pass when key is absent")
	}

	// IfExists: a missing key passes regardless of polarity.
	ifx := mustConditions(t, `{"StringEqualsIfExists": {"s3:prefix": "home/"}}`)
	if !ifx.satisfied(empty) {
		t.Error("IfExists condition should pass when key is absent")
	}
	// ...but when the key is present, IfExists evaluates normally.
	if ifx.satisfied(map[string]string{"s3:prefix": "other/"}) {
		t.Error("IfExists should still evaluate when key is present")
	}
}

func TestConditionSetAllMustHold(t *testing.T) {
	cs := mustConditions(t, `{
		"Bool":      {"aws:SecureTransport": "true"},
		"IpAddress": {"aws:SourceIp": "10.0.0.0/8"}
	}`)
	full := map[string]string{"aws:SecureTransport": "true", "aws:SourceIp": "10.1.2.3"}
	if !cs.satisfied(full) {
		t.Error("both conditions hold, set is satisfied")
	}
	if cs.satisfied(map[string]string{"aws:SecureTransport": "true", "aws:SourceIp": "8.8.8.8"}) {
		t.Error("one condition failing should fail the whole set")
	}
}

func TestNilConditionSetSatisfied(t *testing.T) {
	var cs conditionSet
	if !cs.satisfied(nil) {
		t.Error("a statement with no conditions should always pass the gate")
	}
}

// TestEvaluateWithCondition is the integration check: a Condition block on a
// statement gates the Evaluate decision, and the request context supplies the
// keys.
func TestEvaluateWithCondition(t *testing.T) {
	doc := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::reports/*",
			"Condition": {"IpAddress": {"aws:SourceIp": "10.0.0.0/8"}}
		}]
	}`
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		t.Fatalf("ParsePolicy: %v", err)
	}
	policies := []Policy{p}
	req := Request{
		Action:   "s3:GetObject",
		Resource: ObjectARN("reports", "q1.csv"),
	}

	// From an allowed network: granted.
	req.Context = map[string]string{CondSourceIP: "10.9.9.9"}
	if !Evaluate(policies, req) {
		t.Error("request from 10.0.0.0/8 should be allowed")
	}
	// From outside: the condition fails, so the statement does not match and the
	// request is denied by default.
	req.Context = map[string]string{CondSourceIP: "8.8.8.8"}
	if Evaluate(policies, req) {
		t.Error("request from outside 10.0.0.0/8 should be denied")
	}
	// With no context at all: the present-key condition cannot hold, so denied.
	req.Context = nil
	if Evaluate(policies, req) {
		t.Error("request with no context should be denied")
	}
}

// TestParsePolicyRejectsBadCondition confirms that a malformed Condition block is
// caught when the whole policy is parsed, not only by parseConditions directly.
func TestParsePolicyRejectsBadCondition(t *testing.T) {
	doc := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::b/*",
			"Condition": {"Bogus": {"k": "v"}}
		}]
	}`
	if _, err := ParsePolicy([]byte(doc)); err == nil {
		t.Error("ParsePolicy should reject an unknown condition operator")
	}
}

func BenchmarkEvaluateWithCondition(b *testing.B) {
	doc := `{
		"Version": "2012-10-17",
		"Statement": [{
			"Effect": "Allow",
			"Action": "s3:GetObject",
			"Resource": "arn:aws:s3:::reports/*",
			"Condition": {
				"IpAddress": {"aws:SourceIp": "10.0.0.0/8"},
				"Bool":      {"aws:SecureTransport": "true"}
			}
		}]
	}`
	p, err := ParsePolicy([]byte(doc))
	if err != nil {
		b.Fatalf("ParsePolicy: %v", err)
	}
	policies := []Policy{p}
	req := Request{
		Action:   "s3:GetObject",
		Resource: ObjectARN("reports", "q1.csv"),
		Context: map[string]string{
			CondSourceIP:        "10.9.9.9",
			CondSecureTransport: "true",
		},
	}
	b.ReportAllocs()
	for b.Loop() {
		if !Evaluate(policies, req) {
			b.Fatal("expected allow")
		}
	}
}
