// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// A JWKS keyset holds the signing keys an OIDC provider publishes at its JWKS URL
// (spec 2020, doc 08.4). It resolves the key a token's "kid" header names, fetching
// the document on first use and refetching when a token presents an unknown kid
// (the provider rotated keys), bounded by a minimum interval so a flood of bogus
// kids cannot turn into a fetch storm against the IdP.

// jwksRefetchInterval is the shortest gap between JWKS fetches; an unknown kid seen
// sooner is rejected without refetching, so unknown-kid tokens cannot DoS the IdP.
const jwksRefetchInterval = time.Minute

// jwksMaxBytes caps the JWKS document liteio will read, so a hostile or broken IdP
// cannot exhaust memory.
const jwksMaxBytes = 1 << 20 // 1 MiB

// keySet caches a provider's verification keys by kid and refetches on demand.
type keySet struct {
	url    string
	client *http.Client
	now    func() time.Time

	mu        sync.RWMutex
	keys      map[string]crypto.PublicKey
	lastFetch time.Time
}

// newKeySet builds a keyset for a JWKS URL. A nil client uses http.DefaultClient.
func newKeySet(url string, client *http.Client, now func() time.Time) *keySet {
	if client == nil {
		client = http.DefaultClient
	}
	return &keySet{url: url, client: client, now: now, keys: map[string]crypto.PublicKey{}}
}

// publicKey resolves the key a token's kid names, fetching or refetching the JWKS
// when the kid is not cached. An empty kid is allowed only when the document holds
// exactly one key (a provider that publishes a single unnamed key).
func (k *keySet) publicKey(kid string) (crypto.PublicKey, error) {
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	// Unknown kid: the provider may have rotated. Refetch, rate-limited.
	k.mu.Lock()
	stale := k.now().Sub(k.lastFetch) >= jwksRefetchInterval
	k.mu.Unlock()
	if stale {
		if err := k.fetch(); err != nil {
			return nil, err
		}
	}
	if key, ok := k.lookup(kid); ok {
		return key, nil
	}
	return nil, fmt.Errorf("%w: no signing key for kid %q", ErrInvalidToken, kid)
}

// lookup returns a cached key. An empty kid matches the sole key when the set has
// exactly one, mirroring how single-key providers omit the header field.
func (k *keySet) lookup(kid string) (crypto.PublicKey, bool) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if kid == "" && len(k.keys) == 1 {
		for _, key := range k.keys {
			return key, true
		}
	}
	key, ok := k.keys[kid]
	return key, ok
}

// fetch downloads and parses the JWKS document, replacing the cache. It is called
// under demand, not on a timer, so an offline IdP only fails the assume that needed
// a fresh key, not background work.
func (k *keySet) fetch() error {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, k.url, nil)
	if err != nil {
		return fmt.Errorf("%w: build JWKS request: %v", ErrIDPCommunication, err)
	}
	resp, err := k.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: fetch JWKS: %v", ErrIDPCommunication, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%w: JWKS endpoint returned %d", ErrIDPCommunication, resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, jwksMaxBytes))
	if err != nil {
		return fmt.Errorf("%w: read JWKS: %v", ErrIDPCommunication, err)
	}
	keys, err := parseJWKS(body)
	if err != nil {
		return err
	}

	k.mu.Lock()
	k.keys = keys
	k.lastFetch = k.now()
	k.mu.Unlock()
	return nil
}

// jwk is one key in a JWKS document. Only the fields liteio verifies with are read;
// "kty" selects RSA vs EC and which of the others apply.
type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"` // RSA modulus (base64url, big-endian)
	E   string `json:"e"` // RSA exponent (base64url, big-endian)
	Crv string `json:"crv"`
	X   string `json:"x"` // EC x coordinate
	Y   string `json:"y"` // EC y coordinate
}

// parseJWKS turns a JWKS document into public keys by kid, skipping keys whose type
// or curve liteio does not support rather than failing the whole set.
func parseJWKS(body []byte) (map[string]crypto.PublicKey, error) {
	var doc struct {
		Keys []jwk `json:"keys"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("%w: JWKS is not JSON", ErrIDPCommunication)
	}
	out := make(map[string]crypto.PublicKey, len(doc.Keys))
	for _, key := range doc.Keys {
		pub, err := key.publicKey()
		if err != nil {
			continue // unsupported key type/curve; ignore, others may verify
		}
		out[key.Kid] = pub
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: JWKS has no usable keys", ErrIDPCommunication)
	}
	return out, nil
}

// publicKey builds the crypto.PublicKey for a single JWK.
func (j jwk) publicKey() (crypto.PublicKey, error) {
	switch j.Kty {
	case "RSA":
		n, err := b64url(j.N)
		if err != nil {
			return nil, err
		}
		e, err := b64url(j.E)
		if err != nil {
			return nil, err
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		curve, err := curveFor(j.Crv)
		if err != nil {
			return nil, err
		}
		x, err := b64url(j.X)
		if err != nil {
			return nil, err
		}
		y, err := b64url(j.Y)
		if err != nil {
			return nil, err
		}
		return &ecdsa.PublicKey{Curve: curve, X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported key type %q", ErrInvalidToken, j.Kty)
	}
}

func curveFor(crv string) (elliptic.Curve, error) {
	switch crv {
	case "P-256":
		return elliptic.P256(), nil
	case "P-384":
		return elliptic.P384(), nil
	case "P-521":
		return elliptic.P521(), nil
	default:
		return nil, fmt.Errorf("%w: unsupported curve %q", ErrInvalidToken, crv)
	}
}
