// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// Policy is an AWS-IAM-syntax access-control document (spec 2020, doc 08.2). The
// JSON form matches AWS exactly, so policies written for S3 port to liteio
// unchanged. A policy is a version tag and a list of statements; evaluation
// (Evaluate) is deny-by-default with explicit deny winning over any allow.
type Policy struct {
	Version    string      `json:"Version"`
	ID         string      `json:"Id,omitempty"`
	Statements []Statement `json:"Statement"`
}

// Effect is a statement's verdict when it matches a request: Allow or Deny.
type Effect string

const (
	// Allow grants the matched actions on the matched resources.
	Allow Effect = "Allow"
	// Deny refuses them, overriding any Allow (explicit deny wins).
	Deny Effect = "Deny"
)

// supportedPolicyVersions are the IAM document versions liteio accepts. The
// current version is 2012-10-17; the legacy 2008-10-17 is accepted for policies
// migrated from old deployments.
var supportedPolicyVersions = map[string]bool{
	"2012-10-17": true,
	"2008-10-17": true,
}

// Statement is one rule in a policy: an Effect plus the actions and resources it
// applies to. A statement uses either Action or NotAction (the complement) and
// either Resource or NotResource; the standard form is Action + Resource.
//
// Principal and Condition are parsed and retained so the document round-trips,
// but the identity-policy evaluator in this package does not yet interpret them:
// Principal matters only to bucket (resource-attached) policies, and condition
// evaluation is a later subsystem. They are kept on the type so adding that
// evaluation never changes the wire format.
type Statement struct {
	SID          string          `json:"Sid,omitempty"`
	Effect       Effect          `json:"Effect"`
	Actions      stringSet       `json:"Action,omitempty"`
	NotActions   stringSet       `json:"NotAction,omitempty"`
	Resources    stringSet       `json:"Resource,omitempty"`
	NotResources stringSet       `json:"NotResource,omitempty"`
	Principal    json.RawMessage `json:"Principal,omitempty"`
	Condition    json.RawMessage `json:"Condition,omitempty"`
}

// stringSet is a JSON field that AWS allows as either a single string or an array
// of strings ("Action": "s3:GetObject" and "Action": ["s3:GetObject", ...] are
// both valid). It always marshals back as an array for a canonical form.
type stringSet []string

// UnmarshalJSON accepts a bare string or an array of strings.
func (s *stringSet) UnmarshalJSON(data []byte) error {
	var one string
	if err := json.Unmarshal(data, &one); err == nil {
		*s = stringSet{one}
		return nil
	}
	var many []string
	if err := json.Unmarshal(data, &many); err != nil {
		return fmt.Errorf("auth: field must be a string or array of strings: %w", err)
	}
	*s = many
	return nil
}

// ParsePolicy decodes and validates an IAM policy document. It rejects an
// unknown Version, an empty statement list, a statement whose Effect is not
// Allow or Deny, and a statement that names neither an action nor a not-action.
func ParsePolicy(doc []byte) (Policy, error) {
	var p Policy
	dec := json.NewDecoder(bytes.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return Policy{}, fmt.Errorf("auth: parse policy: %w", err)
	}
	if !supportedPolicyVersions[p.Version] {
		return Policy{}, fmt.Errorf("auth: unsupported policy version %q", p.Version)
	}
	if len(p.Statements) == 0 {
		return Policy{}, fmt.Errorf("auth: policy has no statements")
	}
	for i := range p.Statements {
		st := &p.Statements[i]
		if st.Effect != Allow && st.Effect != Deny {
			return Policy{}, fmt.Errorf("auth: statement %d has invalid effect %q", i, st.Effect)
		}
		if len(st.Actions) == 0 && len(st.NotActions) == 0 {
			return Policy{}, fmt.Errorf("auth: statement %d names neither Action nor NotAction", i)
		}
		if len(st.Actions) > 0 && len(st.NotActions) > 0 {
			return Policy{}, fmt.Errorf("auth: statement %d sets both Action and NotAction", i)
		}
		if len(st.Resources) > 0 && len(st.NotResources) > 0 {
			return Policy{}, fmt.Errorf("auth: statement %d sets both Resource and NotResource", i)
		}
	}
	return p, nil
}
