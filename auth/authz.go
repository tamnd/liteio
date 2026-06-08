// SPDX-License-Identifier: Apache-2.0

package auth

import "fmt"

// AuthorizeS3 is the front-door authorization decision: it resolves an access key
// to its identity, evaluates that identity's effective policies, combines them
// with the bucket's resource policy, and applies any narrowing layer the identity
// carries. It is what the S3 server calls once per request, after SigV4
// authentication has established who the caller is.
//
// The combination follows doc 08.2 rule 4: an explicit Deny in either the identity
// path or the bucket policy is final, otherwise an Allow in either path grants,
// otherwise the request is denied (deny by default). On top of that, a service
// account's inline policy or an STS session's session policy acts as a ceiling:
// it can only subtract permissions, so the request must fall within the ceiling
// for any grant — identity or bucket — to stand. This matches IsAllowed's rule
// that such a layer narrows, never widens, and errs toward least privilege when
// the ceiling and a resource policy disagree.
//
// The requester presents its durable principal to the bucket policy: a user (or
// root) by its own access key, and a service account or STS session by the parent
// user it was derived from, since that is the identity an operator names in a
// bucket policy.
//
// Special cases:
//
//   - An empty access key is the anonymous caller. The decision rests on the
//     bucket policy alone, evaluated against the Anonymous principal — a wildcard
//     Principal grants it, a named one does not.
//   - Root is allowed everything and bypasses the bucket policy, mirroring
//     IsAllowed: root is the cluster superuser, not an identity an operator can
//     lock out with a resource policy.
//   - An unknown access key returns ErrNotFound and an expired session returns
//     ErrExpired, so the caller can tell a missing identity from a denied one.
//
// bucketPolicy may be nil when the bucket has no policy attached, in which case an
// authenticated decision rests on the identity path alone and an anonymous request
// is denied.
func (s *Store) AuthorizeS3(accessKey string, bucketPolicy *Policy, req Request) (bool, error) {
	if accessKey == "" {
		return Authorize(nil, bucketPolicy, Anonymous, req), nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if accessKey == s.rootKey {
		return true, nil
	}
	if u, ok := s.users[accessKey]; ok {
		idDec := decide(s.userPolicies(u), req)
		return combineGrant(idDec, bucketPolicy, PrincipalID(accessKey), req, nil), nil
	}
	if sa, ok := s.svc[accessKey]; ok {
		base, pid, ok := s.parentDecision(sa.ParentUser, req)
		if !ok {
			// The parent was deleted without the child; deny on a dangling reference.
			return false, fmt.Errorf("%w: parent user %q", ErrNotFound, sa.ParentUser)
		}
		return combineGrant(base, bucketPolicy, pid, req, sa.Inline), nil
	}
	if sess, ok := s.sessions[accessKey]; ok {
		if !s.now().Before(sess.expiry) {
			return false, fmt.Errorf("%w: access key %q", ErrExpired, accessKey)
		}
		base, pid, ok := s.sessionDecision(sess, req)
		if !ok {
			return false, fmt.Errorf("%w: session principal %q", ErrNotFound, sess.parent)
		}
		return combineGrant(base, bucketPolicy, pid, req, sess.policy), nil
	}
	return false, fmt.Errorf("%w: access key %q", ErrNotFound, accessKey)
}

// sessionDecision resolves the base three-valued decision for an STS session and the
// principal it presents to a bucket policy. An AssumeRole session defers to the
// parent it was assumed from; a federated session (parent == "") has no store
// identity, so its base is the policy set the provider's claims mapped to and the
// principal it presents is the token subject. The caller must hold the lock.
func (s *Store) sessionDecision(sess *sessionRecord, req Request) (Decision, PrincipalID, bool) {
	if sess.parent == "" {
		return decide(sess.basePolicies, req), PrincipalID(sess.subject), true
	}
	return s.parentDecision(sess.parent, req)
}

// parentDecision resolves the three-valued decision of the identity named by a
// parent access key (root or a user) together with the principal that identity
// presents to a bucket policy. ok is false when the key names no such principal.
// The caller must hold the lock.
func (s *Store) parentDecision(parentKey string, req Request) (Decision, PrincipalID, bool) {
	if parentKey == s.rootKey {
		return DecisionAllow, PrincipalID(parentKey), true
	}
	if u, ok := s.users[parentKey]; ok {
		return decide(s.userPolicies(u), req), PrincipalID(parentKey), true
	}
	return DecisionNone, Anonymous, false
}

// combineGrant merges a three-valued identity decision with a bucket policy and an
// optional narrowing ceiling, returning the final allow/deny. An explicit Deny in
// the identity path or the bucket policy is final; otherwise an Allow in either
// grants; and when a ceiling is present the request must fall within it for that
// grant to stand. pid is the principal presented to the bucket policy.
func combineGrant(idDec Decision, bucketPolicy *Policy, pid PrincipalID, req Request, ceiling *Policy) bool {
	if idDec == DecisionDeny {
		return false
	}
	bucket := DecisionNone
	if bucketPolicy != nil {
		bucket = EvaluateBucketPolicy(*bucketPolicy, pid, req)
		if bucket == DecisionDeny {
			return false
		}
	}
	granted := idDec == DecisionAllow || bucket == DecisionAllow
	if ceiling != nil && decide([]Policy{*ceiling}, req) != DecisionAllow {
		// A ceiling that does not explicitly allow — whether it denies or simply
		// does not match — puts the request outside the granted scope.
		return false
	}
	return granted
}
