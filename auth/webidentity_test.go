// SPDX-License-Identifier: Apache-2.0

package auth

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// fakeIDP is an in-process OIDC provider: it signs identity tokens with an RSA and
// an EC key and serves the matching JWKS, so the web-identity flow can be exercised
// over a real fetch without an external IdP.
type fakeIDP struct {
	issuer string
	server *httptest.Server
	rsa    *rsa.PrivateKey
	ec     *ecdsa.PrivateKey
}

const (
	rsaKid = "rsa-1"
	ecKid  = "ec-1"
)

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	rsaKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	ecKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ec key: %v", err)
	}
	idp := &fakeIDP{rsa: rsaKey, ec: ecKey}
	idp.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(idp.jwks())
	}))
	idp.issuer = idp.server.URL
	t.Cleanup(idp.server.Close)
	return idp
}

func (idp *fakeIDP) jwks() []byte {
	pub := &idp.rsa.PublicKey
	x, y := make([]byte, 32), make([]byte, 32)
	idp.ec.X.FillBytes(x)
	idp.ec.Y.FillBytes(y)
	doc := map[string]any{"keys": []map[string]string{
		{"kty": "RSA", "kid": rsaKid, "alg": "RS256", "n": enc(pub.N.Bytes()), "e": enc(big.NewInt(int64(pub.E)).Bytes())},
		{"kty": "EC", "kid": ecKid, "crv": "P-256", "x": enc(x), "y": enc(y)},
	}}
	out, _ := json.Marshal(doc)
	return out
}

func enc(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// signRS256 produces a compact JWS over the claims with the IdP's RSA key.
func (idp *fakeIDP) signRS256(t *testing.T, claims map[string]any) string {
	return idp.sign(map[string]any{"alg": "RS256", "kid": rsaKid, "typ": "JWT"}, claims, func(signing string) []byte {
		sum := sha256.Sum256([]byte(signing))
		sig, err := rsa.SignPKCS1v15(rand.Reader, idp.rsa, crypto.SHA256, sum[:])
		if err != nil {
			t.Fatalf("rsa sign: %v", err)
		}
		return sig
	})
}

// signES256 produces a compact JWS with the IdP's EC key (raw r||s signature).
func (idp *fakeIDP) signES256(t *testing.T, claims map[string]any) string {
	return idp.sign(map[string]any{"alg": "ES256", "kid": ecKid, "typ": "JWT"}, claims, func(signing string) []byte {
		sum := sha256.Sum256([]byte(signing))
		r, s, err := ecdsa.Sign(rand.Reader, idp.ec, sum[:])
		if err != nil {
			t.Fatalf("ec sign: %v", err)
		}
		sig := make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
		return sig
	})
}

func (idp *fakeIDP) sign(header, claims map[string]any, sign func(string) []byte) string {
	h, _ := json.Marshal(header)
	p, _ := json.Marshal(claims)
	signing := enc(h) + "." + enc(p)
	return signing + "." + enc(sign(signing))
}

// claims builds a standard claim set for the IdP at the pinned test time.
func (idp *fakeIDP) claims(now time.Time, policy any) map[string]any {
	return map[string]any{
		"iss":    idp.issuer,
		"sub":    "user@example.com",
		"aud":    "liteio",
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Unix(),
		"policy": policy,
	}
}

// webIdentityStore builds a store with the IdP registered and a custom data-reader
// policy the tokens map to, with the clock pinned for deterministic expiry.
func webIdentityStore(t *testing.T, idp *fakeIDP, now time.Time) *Store {
	t.Helper()
	store := NewStore("root", "rootsecret")
	store.SetHTTPClient(idp.server.Client())
	SetClock(store, func() time.Time { return now })
	reader := Policy{Version: "2012-10-17", Statements: []Statement{{
		Effect: "Allow", Actions: stringSet{"s3:GetObject"}, Resources: stringSet{"arn:aws:s3:::data/*"},
	}}}
	if err := store.AddPolicy("data-reader", reader); err != nil {
		t.Fatalf("AddPolicy: %v", err)
	}
	if err := store.RegisterWebIdentityProvider(WebIdentityProvider{
		Name: "test-idp", Issuer: idp.issuer, Audiences: []string{"liteio"}, JWKSURL: idp.server.URL,
	}); err != nil {
		t.Fatalf("RegisterWebIdentityProvider: %v", err)
	}
	return store
}

func TestWebIdentityExchange(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	token := idp.signRS256(t, idp.claims(now, "data-reader"))
	sess, subject, err := store.AssumeRoleWithWebIdentity(token, nil, time.Hour)
	if err != nil {
		t.Fatalf("AssumeRoleWithWebIdentity: %v", err)
	}
	if subject != "user@example.com" {
		t.Fatalf("subject = %q, want user@example.com", subject)
	}
	if sess.AccessKey == "" || sess.SecretKey == "" || sess.SessionToken == "" {
		t.Fatalf("incomplete session: %+v", sess)
	}
	// The mapped data-reader policy allows reading data/* and nothing else.
	if ok, err := store.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::data/x"}); err != nil || !ok {
		t.Fatalf("GetObject on data/x: ok=%v err=%v, want allowed", ok, err)
	}
	if ok, _ := store.IsAllowed(sess.AccessKey, Request{Action: "s3:PutObject", Resource: "arn:aws:s3:::data/x"}); ok {
		t.Fatal("PutObject should be denied by the mapped policy")
	}
}

func TestWebIdentityES256(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	token := idp.signES256(t, idp.claims(now, "data-reader"))
	sess, _, err := store.AssumeRoleWithWebIdentity(token, nil, time.Hour)
	if err != nil {
		t.Fatalf("ES256 token: %v", err)
	}
	if ok, err := store.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::data/x"}); err != nil || !ok {
		t.Fatalf("EC session GetObject: ok=%v err=%v", ok, err)
	}
}

func TestWebIdentityMultiplePolicies(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	// A comma-separated claim and an array form both map to the union.
	token := idp.signRS256(t, idp.claims(now, "data-reader,writeonly"))
	sess, _, err := store.AssumeRoleWithWebIdentity(token, nil, time.Hour)
	if err != nil {
		t.Fatalf("multi-policy: %v", err)
	}
	if ok, _ := store.IsAllowed(sess.AccessKey, Request{Action: "s3:PutObject", Resource: "arn:aws:s3:::data/x"}); !ok {
		t.Fatal("writeonly should grant PutObject")
	}
	if ok, _ := store.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::data/x"}); !ok {
		t.Fatal("data-reader should grant GetObject")
	}
}

func TestWebIdentitySessionPolicyNarrows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	// data-reader grants data/*; a session policy narrows to data/public/* only.
	inline := Policy{Version: "2012-10-17", Statements: []Statement{{
		Effect: "Allow", Actions: stringSet{"s3:GetObject"}, Resources: stringSet{"arn:aws:s3:::data/public/*"},
	}}}
	token := idp.signRS256(t, idp.claims(now, "data-reader"))
	sess, _, err := store.AssumeRoleWithWebIdentity(token, &inline, time.Hour)
	if err != nil {
		t.Fatalf("narrowed session: %v", err)
	}
	if ok, _ := store.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::data/public/x"}); !ok {
		t.Fatal("data/public should be allowed")
	}
	if ok, _ := store.IsAllowed(sess.AccessKey, Request{Action: "s3:GetObject", Resource: "arn:aws:s3:::data/private/x"}); ok {
		t.Fatal("data/private should be narrowed away by the session policy")
	}
}

func TestWebIdentityExpiryBoundedByToken(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	// Token expires in 20 minutes; a 12h request must be clamped to the token.
	claims := idp.claims(now, "data-reader")
	claims["exp"] = now.Add(20 * time.Minute).Unix()
	token := idp.signRS256(t, claims)
	sess, _, err := store.AssumeRoleWithWebIdentity(token, nil, 12*time.Hour)
	if err != nil {
		t.Fatalf("exchange: %v", err)
	}
	if got := sess.Expiration; !got.Equal(now.Add(20 * time.Minute)) {
		t.Fatalf("expiration = %v, want token expiry %v", got, now.Add(20*time.Minute))
	}
}

func TestWebIdentityRejections(t *testing.T) {
	now := time.Unix(1_700_000_000, 0).UTC()
	idp := newFakeIDP(t)
	store := webIdentityStore(t, idp, now)

	cases := []struct {
		name  string
		token func() string
		want  error
	}{
		{"tampered signature", func() string {
			return idp.signRS256(t, idp.claims(now, "data-reader")) + "x"
		}, ErrInvalidToken},
		{"wrong audience", func() string {
			c := idp.claims(now, "data-reader")
			c["aud"] = "someone-else"
			return idp.signRS256(t, c)
		}, ErrInvalidToken},
		{"expired", func() string {
			c := idp.claims(now, "data-reader")
			c["exp"] = now.Add(-2 * time.Hour).Unix()
			return idp.signRS256(t, c)
		}, ErrExpired},
		{"no policy claim", func() string {
			c := idp.claims(now, "data-reader")
			delete(c, "policy")
			return idp.signRS256(t, c)
		}, ErrInvalidToken},
		{"unknown policy name", func() string {
			return idp.signRS256(t, idp.claims(now, "no-such-policy"))
		}, ErrInvalidToken},
		{"no subject", func() string {
			c := idp.claims(now, "data-reader")
			delete(c, "sub")
			return idp.signRS256(t, c)
		}, ErrInvalidToken},
		{"unknown issuer", func() string {
			c := idp.claims(now, "data-reader")
			c["iss"] = "https://evil.example"
			return idp.signRS256(t, c)
		}, ErrInvalidToken},
		{"alg confusion HS256", func() string {
			// Sign with HMAC using the RSA public modulus as the secret: the classic
			// confusion attack, which must be refused because HS* is not accepted.
			header := map[string]any{"alg": "HS256", "kid": rsaKid, "typ": "JWT"}
			h, _ := json.Marshal(header)
			p, _ := json.Marshal(idp.claims(now, "data-reader"))
			signing := enc(h) + "." + enc(p)
			mac := hmac.New(sha256.New, idp.rsa.N.Bytes())
			mac.Write([]byte(signing))
			return signing + "." + enc(mac.Sum(nil))
		}, ErrInvalidToken},
		{"alg none", func() string {
			header := map[string]any{"alg": "none", "typ": "JWT"}
			h, _ := json.Marshal(header)
			p, _ := json.Marshal(idp.claims(now, "data-reader"))
			return enc(h) + "." + enc(p) + "."
		}, ErrInvalidToken},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := store.AssumeRoleWithWebIdentity(tc.token(), nil, time.Hour)
			if !errors.Is(err, tc.want) {
				t.Fatalf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestRegisterWebIdentityProviderValidation(t *testing.T) {
	store := NewStore("root", "rootsecret")
	if err := store.RegisterWebIdentityProvider(WebIdentityProvider{JWKSURL: "https://x/jwks"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty issuer: err = %v, want ErrInvalid", err)
	}
	if err := store.RegisterWebIdentityProvider(WebIdentityProvider{Issuer: "https://x"}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty JWKS URL: err = %v, want ErrInvalid", err)
	}
	p := WebIdentityProvider{Issuer: "https://x", JWKSURL: "https://x/jwks"}
	if err := store.RegisterWebIdentityProvider(p); err != nil {
		t.Fatalf("first register: %v", err)
	}
	if err := store.RegisterWebIdentityProvider(p); !errors.Is(err, ErrExists) {
		t.Fatalf("duplicate issuer: err = %v, want ErrExists", err)
	}
}

func BenchmarkAssumeRoleWithWebIdentity(b *testing.B) {
	now := time.Unix(1_700_000_000, 0).UTC()
	rsaKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	idp := &fakeIDP{rsa: rsaKey, ec: mustEC(b)}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(idp.jwks())
	}))
	defer srv.Close()
	idp.server = srv
	idp.issuer = srv.URL

	store := NewStore("root", "rootsecret")
	store.SetHTTPClient(srv.Client())
	SetClock(store, func() time.Time { return now })
	_ = store.AddPolicy("data-reader", Policy{Version: "2012-10-17", Statements: []Statement{{
		Effect: "Allow", Actions: stringSet{"s3:GetObject"}, Resources: stringSet{"arn:aws:s3:::data/*"},
	}}})
	_ = store.RegisterWebIdentityProvider(WebIdentityProvider{Issuer: srv.URL, Audiences: []string{"liteio"}, JWKSURL: srv.URL})

	// Sign once; the keyset is cached after the first exchange, so the benchmark
	// measures verification, not the JWKS fetch.
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": rsaKid, "typ": "JWT"})
	payload, _ := json.Marshal(idp.claims(now, "data-reader"))
	signing := enc(header) + "." + enc(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, _ := rsa.SignPKCS1v15(rand.Reader, rsaKey, crypto.SHA256, sum[:])
	token := signing + "." + enc(sig)

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := store.AssumeRoleWithWebIdentity(token, nil, time.Hour); err != nil {
			b.Fatal(err)
		}
	}
}

func mustEC(b *testing.B) *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		b.Fatal(err)
	}
	return k
}
