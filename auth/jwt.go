// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"slices"
	"strings"
	"time"
)

// JWT (JWS) verification for the OIDC web-identity flow (spec 2020, doc 08.4).
// liteio verifies the identity token an external IdP issued before mapping its
// claims to a session, so this is the security boundary between a foreign identity
// provider and a liteio credential. It is deliberately strict: only asymmetric
// signatures from the provider's published keys are accepted.

// jwtLeeway absorbs small clock differences between the IdP and liteio when
// checking exp/nbf, the same small window AWS allows.
const jwtLeeway = 60 * time.Second

// jwsHeader is the protected header of a compact JWS: the signing algorithm and
// the key id that selects which JWKS key verifies it.
type jwsHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// verifiedToken is the validated content of an identity token: the subject the
// session is attributed to and the full claim set, so the provider's policy claim
// can be read after verification.
type verifiedToken struct {
	subject string
	claims  map[string]any
}

// keyByID resolves the verification key a JWS "kid" header names. A JWKS keyset
// implements it; tests can supply a fixed key.
type keyByID interface {
	publicKey(kid string) (crypto.PublicKey, error)
}

// jwtAlg pairs a JWS algorithm name with the hash it signs over and whether it is
// RSA or ECDSA. Only asymmetric algorithms are listed: a web-identity token must
// be verifiable against the provider's public JWKS, and accepting an HMAC ("HS*")
// algorithm would let an attacker sign a token with the public key as the shared
// secret (the classic JWT algorithm-confusion attack). "none" is likewise absent.
var jwtAlgs = map[string]struct {
	hash  crypto.Hash
	isEC  bool
	ecLen int // r || s component length for ECDSA, bytes
}{
	"RS256": {crypto.SHA256, false, 0},
	"RS384": {crypto.SHA384, false, 0},
	"RS512": {crypto.SHA512, false, 0},
	"ES256": {crypto.SHA256, true, 32},
	"ES384": {crypto.SHA384, true, 48},
	"ES512": {crypto.SHA512, true, 66},
}

// verifyJWT validates a compact JWS identity token: a known asymmetric algorithm,
// a signature that checks against the key the "kid" header selects, the expected
// issuer, an audience the token is intended for, and an unexpired validity window.
// It returns the subject and claims only when every check passes.
func verifyJWT(token string, keys keyByID, expectIssuer string, audiences []string, now time.Time) (*verifiedToken, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: token is not a compact JWS", ErrInvalidToken)
	}

	headerBytes, err := b64url(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header is not base64url", ErrInvalidToken)
	}
	var hdr jwsHeader
	if err := json.Unmarshal(headerBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header is not JSON", ErrInvalidToken)
	}
	spec, ok := jwtAlgs[hdr.Alg]
	if !ok {
		return nil, fmt.Errorf("%w: unsupported algorithm %q", ErrInvalidToken, hdr.Alg)
	}

	sig, err := b64url(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: signature is not base64url", ErrInvalidToken)
	}
	key, err := keys.publicKey(hdr.Kid)
	if err != nil {
		return nil, err
	}

	signed := []byte(parts[0] + "." + parts[1])
	if err := verifySignature(spec.hash, spec.isEC, spec.ecLen, key, signed, sig); err != nil {
		return nil, err
	}

	payloadBytes, err := b64url(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload is not base64url", ErrInvalidToken)
	}
	var claims map[string]any
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return nil, fmt.Errorf("%w: payload is not JSON", ErrInvalidToken)
	}

	if err := validateClaims(claims, expectIssuer, audiences, now); err != nil {
		return nil, err
	}

	sub, _ := claims["sub"].(string)
	if sub == "" {
		return nil, fmt.Errorf("%w: token has no subject", ErrInvalidToken)
	}
	return &verifiedToken{subject: sub, claims: claims}, nil
}

// verifySignature checks the JWS signature against the resolved key, dispatching
// on the algorithm family. The key type must match the algorithm family, so an
// RS* token cannot be verified against an EC key or vice versa.
func verifySignature(hash crypto.Hash, isEC bool, ecLen int, key crypto.PublicKey, signed, sig []byte) error {
	sum := hash.New()
	sum.Write(signed)
	digest := sum.Sum(nil)

	if isEC {
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: EC algorithm with non-EC key", ErrInvalidToken)
		}
		// A JWS ECDSA signature is the fixed-length concatenation r || s, not ASN.1.
		if len(sig) != 2*ecLen {
			return fmt.Errorf("%w: malformed ECDSA signature", ErrInvalidToken)
		}
		r := new(big.Int).SetBytes(sig[:ecLen])
		ss := new(big.Int).SetBytes(sig[ecLen:])
		if !ecdsa.Verify(pub, digest, r, ss) {
			return fmt.Errorf("%w: signature does not verify", ErrInvalidToken)
		}
		return nil
	}

	pub, ok := key.(*rsa.PublicKey)
	if !ok {
		return fmt.Errorf("%w: RSA algorithm with non-RSA key", ErrInvalidToken)
	}
	if err := rsa.VerifyPKCS1v15(pub, hash, digest, sig); err != nil {
		return fmt.Errorf("%w: signature does not verify", ErrInvalidToken)
	}
	return nil
}

// validateClaims enforces the registered claims: issuer match, audience match (when
// the provider pins audiences), and an unexpired window with a small leeway.
func validateClaims(claims map[string]any, expectIssuer string, audiences []string, now time.Time) error {
	if iss, _ := claims["iss"].(string); iss != expectIssuer {
		return fmt.Errorf("%w: issuer %q is not the configured provider", ErrInvalidToken, iss)
	}
	if len(audiences) > 0 && !audienceMatches(claims["aud"], audiences) {
		return fmt.Errorf("%w: token audience is not accepted", ErrInvalidToken)
	}
	if exp, ok := numericClaim(claims["exp"]); ok {
		if !now.Add(-jwtLeeway).Before(time.Unix(exp, 0)) {
			return fmt.Errorf("%w: token has expired", ErrExpired)
		}
	} else {
		return fmt.Errorf("%w: token has no expiry", ErrInvalidToken)
	}
	if nbf, ok := numericClaim(claims["nbf"]); ok {
		if now.Add(jwtLeeway).Before(time.Unix(nbf, 0)) {
			return fmt.Errorf("%w: token is not yet valid", ErrInvalidToken)
		}
	}
	return nil
}

// tokenExpiry reads the validated token's expiry as a time, for bounding the
// session duration so a session never outlives the identity token behind it.
func (t *verifiedToken) expiry() (time.Time, bool) {
	exp, ok := numericClaim(t.claims["exp"])
	if !ok {
		return time.Time{}, false
	}
	return time.Unix(exp, 0), true
}

// audienceMatches reports whether the token's "aud" (a string or an array of
// strings) intersects the provider's accepted audiences.
func audienceMatches(aud any, accepted []string) bool {
	switch v := aud.(type) {
	case string:
		return slices.Contains(accepted, v)
	case []any:
		for _, a := range v {
			if s, ok := a.(string); ok && slices.Contains(accepted, s) {
				return true
			}
		}
	}
	return false
}

// numericClaim reads a JSON number claim (exp, nbf, iat) as a Unix second. JSON
// numbers decode to float64, which holds whole second values exactly.
func numericClaim(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok {
		return 0, false
	}
	return int64(f), true
}

// b64url decodes a base64url segment, accepting both padded and unpadded forms
// (JWS uses unpadded; tolerate padding defensively).
func b64url(s string) ([]byte, error) {
	if strings.ContainsAny(s, "=") {
		return base64.URLEncoding.DecodeString(s)
	}
	return base64.RawURLEncoding.DecodeString(s)
}
