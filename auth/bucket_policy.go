// SPDX-License-Identifier: Apache-2.0

package auth

import "fmt"

// Bucket policies are resource-attached access-control documents (spec 2020, doc
// 08.3). They differ from identity policies in two ways: each statement carries a
// Principal naming who it applies to, and they are evaluated *alongside* identity
// policies — an allow in either path grants, subject to an explicit deny in either
// (doc 08.2 rule 4). Anonymous requests (no SigV4) are evaluated against the
// bucket policy alone, so a bucket policy granting s3:GetObject to Principal "*"
// is what makes objects publicly readable.

// ParseBucketPolicy decodes and validates a bucket policy. It applies the same
// document validation as ParsePolicy (version, statements, effect, action,
// resource) and additionally requires every statement to carry a Principal,
// decoding it into a matchable form. A statement without a Principal, or with an
// invalid one, is rejected — a resource policy that does not say who it applies to
// is meaningless.
func ParseBucketPolicy(doc []byte) (Policy, error) {
	p, err := ParsePolicy(doc)
	if err != nil {
		return Policy{}, err
	}
	for i := range p.Statements {
		pr, err := parsePrincipal(p.Statements[i].Principal)
		if err != nil {
			return Policy{}, fmt.Errorf("auth: statement %d: %w", i, err)
		}
		p.Statements[i].parsedPrincipal = pr
	}
	return p, nil
}

// EvaluateBucketPolicy decides a request against one bucket policy for a given
// principal, three-valued: DecisionDeny on the first matching Deny, else
// DecisionAllow if any statement matched and allowed, else DecisionNone. A
// statement matches only when its Principal covers the requester and its action
// and resource cover the request. The policy must have come from
// ParseBucketPolicy (so each statement's Principal is decoded); a statement with
// no decoded Principal never matches.
func EvaluateBucketPolicy(p Policy, id PrincipalID, req Request) Decision {
	d := DecisionNone
	for i := range p.Statements {
		st := &p.Statements[i]
		if !st.parsedPrincipal.matches(id) {
			continue
		}
		if !st.matches(req) {
			continue
		}
		if st.Effect == Deny {
			return DecisionDeny
		}
		d = DecisionAllow
	}
	return d
}

// Authorize combines an identity's effective policies with a resource-attached
// bucket policy per the AWS rule (doc 08.2 rule 4):
//
//   - an explicit Deny in *either* path is final;
//   - otherwise an Allow in *either* path grants;
//   - otherwise the request is denied (deny by default).
//
// id is the requesting principal: a real access key for an authenticated request
// (pass its effective identity policies), or Anonymous for an unauthenticated one
// (pass nil identity policies, so the decision rests on the bucket policy alone).
// bucketPolicy may be nil when the bucket has none.
func Authorize(identity []Policy, bucketPolicy *Policy, id PrincipalID, req Request) bool {
	idDecision := decide(identity, req)
	if idDecision == DecisionDeny {
		return false
	}
	bucket := DecisionNone
	if bucketPolicy != nil {
		bucket = EvaluateBucketPolicy(*bucketPolicy, id, req)
		if bucket == DecisionDeny {
			return false
		}
	}
	return idDecision == DecisionAllow || bucket == DecisionAllow
}

// PublicReadPolicy returns the bucket policy equivalent to the canned public-read
// ACL: anonymous and authenticated callers alike may read (s3:GetObject) every
// object in the bucket. It is the normalized form doc 08.3 refers to.
func PublicReadPolicy(bucket string) Policy {
	return Policy{
		Version: "2012-10-17",
		Statements: []Statement{{
			Effect:          Allow,
			Actions:         stringSet{"s3:GetObject"},
			Resources:       stringSet{ObjectARN(bucket, "*")},
			parsedPrincipal: &principal{anyone: true},
		}},
	}
}
