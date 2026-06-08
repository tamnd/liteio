// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"time"
)

// AssumeRoleWithWebIdentity is the OIDC federated flow (spec 2020, doc 08.4): a
// client presents an identity token an external provider signed, and liteio
// exchanges it for a session whose permissions come from the policies the token's
// claims map to. Unlike AssumeRole, no liteio credential is needed up front — the
// token is the credential — so the session has no parent identity; its base is the
// mapped policy set (see sessionRecord.basePolicies). This file holds the provider
// registry and the exchange; JWT verification lives in jwt.go, key resolution in
// jwks.go.

// defaultPolicyClaim is the claim liteio reads policy names from when a provider
// does not name one, matching the common OIDC deployment convention.
const defaultPolicyClaim = "policy"

// WebIdentityProvider configures an OIDC identity provider liteio trusts for the
// AssumeRoleWithWebIdentity flow.
type WebIdentityProvider struct {
	// Name is an operator-facing label for the provider.
	Name string
	// Issuer is the expected "iss" claim and the key the provider is looked up by
	// when a token arrives.
	Issuer string
	// Audiences are the accepted "aud" values. Empty means the audience is not
	// checked (acceptable only when the issuer is single-tenant).
	Audiences []string
	// PolicyClaim names the claim whose value lists the liteio policies the session
	// inherits. Empty defaults to "policy".
	PolicyClaim string
	// JWKSURL is where the provider publishes its signing keys.
	JWKSURL string
}

// webIdentityProvider is the registered, internal form of a provider: the validated
// config plus the live keyset that fetches and caches its signing keys.
type webIdentityProvider struct {
	name        string
	issuer      string
	audiences   []string
	policyClaim string
	keys        *keySet
}

// SetHTTPClient sets the HTTP client used to fetch provider JWKS documents (for a
// custom CA pool or timeout). It must be called before registering providers; a
// nil client is ignored. The default is http.DefaultClient.
func (s *Store) SetHTTPClient(c *http.Client) {
	if c == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.client = c
}

// RegisterWebIdentityProvider registers an OIDC provider. The issuer and JWKS URL
// are required; the issuer must be unique. The provider's keys are fetched lazily
// on the first token that names it, so registration does no network I/O.
func (s *Store) RegisterWebIdentityProvider(p WebIdentityProvider) error {
	if p.Issuer == "" {
		return fmt.Errorf("%w: provider issuer is empty", ErrInvalid)
	}
	if p.JWKSURL == "" {
		return fmt.Errorf("%w: provider %q has no JWKS URL", ErrInvalid, p.Issuer)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.providers[p.Issuer]; ok {
		return fmt.Errorf("%w: provider for issuer %q", ErrExists, p.Issuer)
	}
	claim := p.PolicyClaim
	if claim == "" {
		claim = defaultPolicyClaim
	}
	s.providers[p.Issuer] = &webIdentityProvider{
		name:        p.Name,
		issuer:      p.Issuer,
		audiences:   slices.Clone(p.Audiences),
		policyClaim: claim,
		keys:        newKeySet(p.JWKSURL, s.client, s.now),
	}
	return nil
}

// AssumeRoleWithWebIdentity verifies an OIDC identity token against the provider its
// issuer names, maps the configured policy claim to a session, and returns the
// session and the token subject (for the response and aws:username). The session
// duration is clamped to [Min, Max] and further bounded so it never outlives the
// token. An optional session policy can only narrow, as with AssumeRole.
func (s *Store) AssumeRoleWithWebIdentity(token string, sessionPolicy *Policy, ttl time.Duration) (Session, string, error) {
	issuer, err := unverifiedIssuer(token)
	if err != nil {
		return Session{}, "", err
	}

	// Resolve the provider under the lock, then verify outside it: verification may
	// fetch the JWKS over the network, which must not block the store.
	s.mu.RLock()
	prov := s.providers[issuer]
	s.mu.RUnlock()
	if prov == nil {
		return Session{}, "", fmt.Errorf("%w: no provider for issuer %q", ErrInvalidToken, issuer)
	}

	vt, err := verifyJWT(token, prov.keys, prov.issuer, prov.audiences, s.now())
	if err != nil {
		return Session{}, "", err
	}

	names := policyNames(vt.claims[prov.policyClaim])
	if len(names) == 0 {
		return Session{}, "", fmt.Errorf("%w: token claim %q names no policy", ErrInvalidToken, prov.policyClaim)
	}

	expiry := s.now().Add(clampDuration(ttl))
	if tokenExp, ok := vt.expiry(); ok && tokenExp.Before(expiry) {
		expiry = tokenExp
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	base := make([]Policy, 0, len(names))
	for _, name := range names {
		if p, ok := s.policies[name]; ok {
			base = append(base, p)
		}
	}
	if len(base) == 0 {
		return Session{}, "", fmt.Errorf("%w: none of the token's policies are known to the store", ErrInvalidToken)
	}

	rec := &sessionRecord{
		basePolicies: base,
		subject:      vt.subject,
		expiry:       expiry,
	}
	sess, err := s.mintSession(rec, sessionPolicy)
	if err != nil {
		return Session{}, "", err
	}
	return sess, vt.subject, nil
}

// unverifiedIssuer reads the "iss" claim from a token without verifying it, so the
// right provider (and its keys) can be selected before verification. The signature
// is checked afterward against that provider, so reading the issuer first is safe.
func unverifiedIssuer(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return "", fmt.Errorf("%w: token is not a compact JWS", ErrInvalidToken)
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		if payload, err = base64.URLEncoding.DecodeString(parts[1]); err != nil {
			return "", fmt.Errorf("%w: payload is not base64url", ErrInvalidToken)
		}
	}
	var claims struct {
		Iss string `json:"iss"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return "", fmt.Errorf("%w: payload is not JSON", ErrInvalidToken)
	}
	if claims.Iss == "" {
		return "", fmt.Errorf("%w: token has no issuer", ErrInvalidToken)
	}
	return claims.Iss, nil
}

// policyNames extracts policy names from a claim value. A provider may carry them
// as a single string, a comma-separated string, or an array of strings.
func policyNames(claim any) []string {
	switch v := claim.(type) {
	case string:
		var out []string
		for name := range strings.SplitSeq(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				out = append(out, name)
			}
		}
		return out
	case []any:
		var out []string
		for _, item := range v {
			if name, ok := item.(string); ok {
				if name = strings.TrimSpace(name); name != "" {
					out = append(out, name)
				}
			}
		}
		return out
	default:
		return nil
	}
}
