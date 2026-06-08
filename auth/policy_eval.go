// SPDX-License-Identifier: Apache-2.0

package auth

import "strings"

// arnPrefix is the fixed S3 ARN namespace. liteio is single-region and
// single-account for ARN purposes, so the partition, region, and account fields
// are empty, matching how S3 bucket ARNs are written: arn:aws:s3:::bucket/key.
const arnPrefix = "arn:aws:s3:::"

// Request is one authorization question: may this action touch this resource?
// Action is an S3 or admin action key (for example "s3:GetObject"); Resource is
// the ARN of the bucket or object, as built by BucketARN or ObjectARN.
type Request struct {
	Action   string
	Resource string
}

// BucketARN returns the ARN naming a bucket: arn:aws:s3:::bucket.
func BucketARN(bucket string) string { return arnPrefix + bucket }

// ObjectARN returns the ARN naming an object: arn:aws:s3:::bucket/key.
func ObjectARN(bucket, key string) string { return arnPrefix + bucket + "/" + key }

// Decision is the three-valued outcome of evaluating a request against a set of
// statements: no statement matched, a matching Allow, or a matching Deny. The
// two-valued Evaluate collapses None to a denial; the three-valued form is what
// lets bucket policies and identity policies be combined (Authorize), where a
// matched Deny in either path must beat a matched Allow in the other.
type Decision int

const (
	// DecisionNone means no statement matched the request.
	DecisionNone Decision = iota
	// DecisionAllow means a matching Allow and no matching Deny.
	DecisionAllow
	// DecisionDeny means a matching Deny, which is final.
	DecisionDeny
)

// Evaluate decides a request against a set of policies using AWS rules:
//
//  1. Deny by default — with no matching Allow the request is denied.
//  2. Explicit deny wins — any matching Deny overrides every Allow.
//  3. Union of allows — a matching Allow in any policy grants, unless a Deny
//     also matches.
//
// The policies passed are the identity's effective set: its user policies, its
// groups' policies, and (for an STS session) the session policy. Because a Deny
// anywhere in that set wins and an Allow anywhere suffices, the caller composes
// an identity's permissions simply by passing every applicable policy.
func Evaluate(policies []Policy, req Request) bool {
	return decide(policies, req) == DecisionAllow
}

// decide is the three-valued core of Evaluate: it returns DecisionDeny on the
// first matching Deny, otherwise DecisionAllow if any statement allowed, else
// DecisionNone.
func decide(policies []Policy, req Request) Decision {
	d := DecisionNone
	for _, p := range policies {
		for i := range p.Statements {
			if !p.Statements[i].matches(req) {
				continue
			}
			if p.Statements[i].Effect == Deny {
				return DecisionDeny // explicit deny is final
			}
			d = DecisionAllow
		}
	}
	return d
}

// matches reports whether the statement applies to the request: its action side
// covers req.Action and its resource side covers req.Resource.
func (st *Statement) matches(req Request) bool {
	return st.actionMatches(req.Action) && st.resourceMatches(req.Resource)
}

// actionMatches applies the statement's Action or NotAction set to action.
// Action matching is case-insensitive, as in AWS.
func (st *Statement) actionMatches(action string) bool {
	if len(st.NotActions) > 0 {
		return !anyWildcardFold(st.NotActions, action)
	}
	return anyWildcardFold(st.Actions, action)
}

// resourceMatches applies the statement's Resource or NotResource set to
// resource. Resource matching is case-sensitive, since S3 keys are.
func (st *Statement) resourceMatches(resource string) bool {
	if len(st.NotResources) > 0 {
		return !anyWildcard(st.NotResources, resource)
	}
	// A statement with no resource clause matches no resource. (ParsePolicy
	// permits an action-only statement; such a statement grants nothing until a
	// resource is named, which is the safe reading.)
	return anyWildcard(st.Resources, resource)
}

// anyWildcard reports whether any pattern matches s case-sensitively.
func anyWildcard(patterns []string, s string) bool {
	for _, p := range patterns {
		if wildcardMatch(p, s) {
			return true
		}
	}
	return false
}

// anyWildcardFold reports whether any pattern matches s case-insensitively.
func anyWildcardFold(patterns []string, s string) bool {
	s = strings.ToLower(s)
	for _, p := range patterns {
		if wildcardMatch(strings.ToLower(p), s) {
			return true
		}
	}
	return false
}

// wildcardMatch reports whether the AWS IAM glob pattern matches s. The pattern
// language is two metacharacters: '*' matches any run of characters (including
// none) and '?' matches exactly one character; every other character is literal.
// It is implemented with linear-time backtracking, so a pattern of many '*' does
// not blow up.
func wildcardMatch(pattern, s string) bool {
	var (
		px, sx    int  // current pattern and string positions
		star      = -1 // position in pattern of the last '*'
		starMatch = 0  // position in s that '*' is currently consuming to
	)
	for sx < len(s) {
		switch {
		case px < len(pattern) && (pattern[px] == '?' || pattern[px] == s[sx]):
			px++
			sx++
		case px < len(pattern) && pattern[px] == '*':
			star = px
			starMatch = sx
			px++
		case star != -1:
			// Backtrack: let the last '*' swallow one more character of s.
			px = star + 1
			starMatch++
			sx = starMatch
		default:
			return false
		}
	}
	// Trailing '*'s in the pattern can match the empty remainder.
	for px < len(pattern) && pattern[px] == '*' {
		px++
	}
	return px == len(pattern)
}
