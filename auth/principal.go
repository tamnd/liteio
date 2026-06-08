// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/json"
	"fmt"
	"slices"
)

// PrincipalID identifies who is making a request, for matching the Principal
// clause of a bucket policy. The empty value is the anonymous principal: a
// request with no credentials, which matches only a wildcard ("*") principal.
type PrincipalID string

// Anonymous is the principal of an unauthenticated request.
const Anonymous PrincipalID = ""

// principal is a bucket-policy Principal clause decoded into a matchable form:
// either everyone (the "*" wildcard, which includes anonymous requests) or a set
// of named AWS principals (liteio access keys; the deployment is single-account,
// so a bare access key identifies a principal).
type principal struct {
	anyone bool
	aws    []string
}

// matches reports whether the request's principal is covered by this clause. The
// wildcard matches everyone including anonymous; a named set matches only a
// non-anonymous principal whose access key is listed.
func (p *principal) matches(id PrincipalID) bool {
	if p == nil {
		return false
	}
	if p.anyone {
		return true
	}
	if id == Anonymous {
		return false
	}
	return slices.Contains(p.aws, string(id))
}

// parsePrincipal decodes a bucket-policy Principal clause. AWS allows the bare
// string "*" (everyone) or an object keyed by principal type; liteio reads the
// "AWS" key, whose value is a string or array of access keys, with "*" anywhere
// in it meaning everyone. An empty clause is an error: a bucket-policy statement
// must name who it applies to.
func parsePrincipal(raw json.RawMessage) (*principal, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("auth: bucket-policy statement has no Principal")
	}

	// The bare-string form: only "*" is valid.
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		if one == "*" {
			return &principal{anyone: true}, nil
		}
		return nil, fmt.Errorf("auth: invalid Principal %q (only \"*\" is valid as a bare string)", one)
	}

	// The object form: {"AWS": "*"} or {"AWS": ["key", ...]}.
	var obj map[string]stringSet
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("auth: Principal must be \"*\" or an object: %w", err)
	}
	aws, ok := obj["AWS"]
	if !ok {
		return nil, fmt.Errorf("auth: Principal object must have an \"AWS\" key")
	}
	if slices.Contains(aws, "*") {
		return &principal{anyone: true}, nil
	}
	if len(aws) == 0 {
		return nil, fmt.Errorf("auth: Principal AWS list is empty")
	}
	return &principal{aws: aws}, nil
}
