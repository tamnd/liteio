// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"
)

// Conditions are the optional gate on a policy statement: a statement applies
// only when, on top of its action and resource matching, every condition in its
// Condition block is satisfied by the request's context (spec 2020, doc 08.2).
// This is what expresses prefix-scoped tenancy (s3:prefix), TLS-only access
// (aws:SecureTransport), source-IP restrictions (aws:SourceIp), pagination caps
// (s3:max-keys), and the like.
//
// The Condition block is the AWS shape — operator → key → value(s):
//
//	"Condition": {
//	  "StringEquals": {"s3:prefix": ["home/", "shared/"]},
//	  "IpAddress":    {"aws:SourceIp": "10.0.0.0/8"},
//	  "Bool":         {"aws:SecureTransport": "true"}
//	}
//
// The request supplies a flat context of key→value (Request.Context); the front
// door fills it from the HTTP request (the source IP, the TLS flag, the query
// parameters, and so on). All conditions in a statement must hold (AND).

// Well-known condition keys the front door populates. They are plain strings;
// these constants exist so producers and the front door agree on spelling.
const (
	CondSourceIP        = "aws:SourceIp"
	CondSecureTransport = "aws:SecureTransport"
	CondUsername        = "aws:username"
	CondCurrentTime     = "aws:CurrentTime"
	CondPrefix          = "s3:prefix"
	CondMaxKeys         = "s3:max-keys"
	CondDelimiter       = "s3:delimiter"
)

// condition is one parsed operator/key/values triple, with the negation and the
// IfExists qualifier lifted out of the operator name.
type condition struct {
	op       string // base operator, e.g. "StringEquals" (negation stripped)
	not      bool   // a *Not* / Not* operator: satisfied when nothing matches
	ifExists bool   // ...IfExists: a missing key satisfies the condition
	key      string
	values   []string
}

// conditionSet is every condition on a statement; all must hold.
type conditionSet []condition

// negatedOps maps the negated operator spellings to their positive base.
var negatedOps = map[string]string{
	"StringNotEquals":           "StringEquals",
	"StringNotEqualsIgnoreCase": "StringEqualsIgnoreCase",
	"StringNotLike":             "StringLike",
	"NumericNotEquals":          "NumericEquals",
	"DateNotEquals":             "DateEquals",
	"NotIpAddress":              "IpAddress",
}

// knownOps is the set of positive base operators liteio evaluates.
var knownOps = map[string]bool{
	"StringEquals": true, "StringEqualsIgnoreCase": true, "StringLike": true,
	"Bool":          true,
	"IpAddress":     true,
	"NumericEquals": true, "NumericLessThan": true, "NumericLessThanEquals": true,
	"NumericGreaterThan": true, "NumericGreaterThanEquals": true,
	"DateEquals": true, "DateLessThan": true, "DateLessThanEquals": true,
	"DateGreaterThan": true, "DateGreaterThanEquals": true,
}

// normalizeOp splits an operator spelling into its base, whether it is negated,
// and whether it carries the IfExists qualifier. ok is false for an operator
// liteio does not evaluate, so a typo is rejected at parse time rather than
// silently passing.
func normalizeOp(op string) (base string, not, ifExists bool, ok bool) {
	base = op
	if strings.HasSuffix(base, "IfExists") {
		ifExists = true
		base = strings.TrimSuffix(base, "IfExists")
	}
	if pos, isNeg := negatedOps[base]; isNeg {
		return pos, true, ifExists, true
	}
	return base, false, ifExists, knownOps[base]
}

// parseConditions decodes a Condition block. An empty block is nil (a statement
// with no conditions always passes the condition gate). An unknown operator is
// rejected.
func parseConditions(raw json.RawMessage) (conditionSet, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	var block map[string]map[string]stringSet
	if err := json.Unmarshal(raw, &block); err != nil {
		return nil, fmt.Errorf("auth: Condition must be operator->key->values: %w", err)
	}
	var set conditionSet
	for op, kv := range block {
		base, not, ifExists, ok := normalizeOp(op)
		if !ok {
			return nil, fmt.Errorf("auth: unknown condition operator %q", op)
		}
		for key, values := range kv {
			set = append(set, condition{
				op:       base,
				not:      not,
				ifExists: ifExists,
				key:      key,
				values:   values,
			})
		}
	}
	return set, nil
}

// satisfied reports whether every condition holds against the request context.
// A nil set (no conditions) is trivially satisfied.
func (cs conditionSet) satisfied(ctx map[string]string) bool {
	for i := range cs {
		if !cs[i].satisfied(ctx) {
			return false
		}
	}
	return true
}

// satisfied evaluates one condition against the context. AWS semantics for a key
// absent from the context: a positive operator fails, a negated operator passes,
// and an IfExists operator passes either way. When the key is present, the result
// is whether any listed value matches (negated: whether none do).
func (c *condition) satisfied(ctx map[string]string) bool {
	val, present := ctx[c.key]
	if !present {
		if c.ifExists {
			return true
		}
		return c.not
	}
	matched := c.anyMatch(val)
	if c.not {
		return !matched
	}
	return matched
}

// anyMatch reports whether the context value matches at least one listed value
// under this condition's operator.
func (c *condition) anyMatch(val string) bool {
	for _, want := range c.values {
		if c.predicate(val, want) {
			return true
		}
	}
	return false
}

// predicate is the per-operator comparison of a context value against one listed
// value. A value that fails to parse for a typed operator (number, bool, IP,
// date) is treated as not matching, never as an error.
func (c *condition) predicate(val, want string) bool {
	switch c.op {
	case "StringEquals":
		return val == want
	case "StringEqualsIgnoreCase":
		return strings.EqualFold(val, want)
	case "StringLike":
		return wildcardMatch(want, val)
	case "Bool":
		return boolMatch(val, want)
	case "IpAddress":
		return ipMatch(val, want)
	case "NumericEquals", "NumericLessThan", "NumericLessThanEquals",
		"NumericGreaterThan", "NumericGreaterThanEquals":
		return numericMatch(c.op, val, want)
	case "DateEquals", "DateLessThan", "DateLessThanEquals",
		"DateGreaterThan", "DateGreaterThanEquals":
		return dateMatch(c.op, val, want)
	}
	return false
}

func boolMatch(val, want string) bool {
	bv, err1 := strconv.ParseBool(val)
	bw, err2 := strconv.ParseBool(want)
	return err1 == nil && err2 == nil && bv == bw
}

func ipMatch(val, want string) bool {
	addr, err := netip.ParseAddr(val)
	if err != nil {
		return false
	}
	if strings.Contains(want, "/") {
		p, err := netip.ParsePrefix(want)
		if err != nil {
			return false
		}
		return p.Contains(addr)
	}
	w, err := netip.ParseAddr(want)
	if err != nil {
		return false
	}
	return addr == w
}

func numericMatch(op, val, want string) bool {
	a, err1 := strconv.ParseFloat(val, 64)
	b, err2 := strconv.ParseFloat(want, 64)
	if err1 != nil || err2 != nil {
		return false
	}
	switch op {
	case "NumericEquals":
		return a == b
	case "NumericLessThan":
		return a < b
	case "NumericLessThanEquals":
		return a <= b
	case "NumericGreaterThan":
		return a > b
	case "NumericGreaterThanEquals":
		return a >= b
	}
	return false
}

func dateMatch(op, val, want string) bool {
	a, err1 := time.Parse(time.RFC3339, val)
	b, err2 := time.Parse(time.RFC3339, want)
	if err1 != nil || err2 != nil {
		return false
	}
	switch op {
	case "DateEquals":
		return a.Equal(b)
	case "DateLessThan":
		return a.Before(b)
	case "DateLessThanEquals":
		return !a.After(b)
	case "DateGreaterThan":
		return a.After(b)
	case "DateGreaterThanEquals":
		return !a.Before(b)
	}
	return false
}
