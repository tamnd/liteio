// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"io"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/s3/sign"
	"github.com/tamnd/liteio/storage"
	"github.com/tamnd/liteio/storage/local"
)

// stsHarness wires the front door with one auth.Store serving three roles at once:
// the SigV4 credential store (so an STS-minted session can immediately sign), the
// authorizer, and the STS issuer. That single-authority wiring is the production
// shape, and it is what lets an assumed session's credentials work end to end.
type stsHarness struct {
	t     *testing.T
	srv   *httptest.Server
	store *auth.Store
}

func newSTSHarness(t *testing.T) *stsHarness {
	t.Helper()
	const n, parity = 6, 2
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	layer, err := object.NewSingleSet([16]byte{9, 9, 9}, drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}

	store := auth.NewStore(testCreds.AccessKey, testCreds.SecretKey)
	server := NewServer(layer, store, // the store is the credential store too
		WithAuthorizer(store),
		WithSTS(store),
		WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)
	return &stsHarness{t: t, srv: srv, store: store}
}

// doAs signs a request with the given credentials and drains the response.
func (h *stsHarness) doAs(c auth.Credentials, method, path string, body []byte) result {
	h.t.Helper()
	var rdr io.Reader
	hash := sign.EmptyPayloadHash
	if body != nil {
		rdr = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	sign.SignHeader(req, c, "us-east-1", hash, time.Now().UTC())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

// assumeRoleAs posts a form-encoded AssumeRole request signed by the given caller
// and returns the raw response.
func (h *stsHarness) assumeRoleAs(c auth.Credentials, form url.Values) result {
	h.t.Helper()
	body := []byte(form.Encode())
	sum := sha256.Sum256(body)
	req, err := http.NewRequest(http.MethodPost, h.srv.URL+"/", bytes.NewReader(body))
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(body))
	sign.SignHeader(req, c, "us-east-1", hex.EncodeToString(sum[:]), time.Now().UTC())
	resp, err := h.srv.Client().Do(req)
	if err != nil {
		h.t.Fatalf("assume role: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

// sessionCreds parses an AssumeRole success body into signable credentials.
func sessionCreds(t *testing.T, res result) auth.Credentials {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("AssumeRole status = %d; body=%s", res.status, res.body)
	}
	var parsed assumeRoleResponse
	if err := xml.Unmarshal(res.body, &parsed); err != nil {
		t.Fatalf("decode AssumeRole: %v; body=%s", err, res.body)
	}
	c := parsed.Result.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		t.Fatalf("incomplete credentials: %+v", c)
	}
	if _, err := time.Parse(time.RFC3339, c.Expiration); err != nil {
		t.Fatalf("bad expiration %q: %v", c.Expiration, err)
	}
	return auth.Credentials{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey}
}

func TestAssumeRoleSessionCanSign(t *testing.T) {
	h := newSTSHarness(t)
	// Root assumes a role; the resulting session inherits root and can list buckets.
	sess := sessionCreds(t, h.assumeRoleAs(testCreds, url.Values{"Action": {"AssumeRole"}}))
	mustStatus(t, h.doAs(sess, http.MethodGet, "/", nil), http.StatusOK)
}

func TestAssumeRoleInlinePolicyNarrows(t *testing.T) {
	h := newSTSHarness(t)
	// Set up a bucket with an object the admin owns.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b/k", []byte("hello")), http.StatusOK)

	// Assume a role whose inline policy allows only reading under b/.
	form := url.Values{
		"Action": {"AssumeRole"},
		"Policy": {`{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"s3:GetObject","Resource":"arn:aws:s3:::b/*"}]}`},
	}
	sess := sessionCreds(t, h.assumeRoleAs(testCreds, form))

	// The session reads the object but cannot write: the inline policy only narrows.
	mustStatus(t, h.doAs(sess, http.MethodGet, "/b/k", nil), http.StatusOK)
	mustStatus(t, h.doAs(sess, http.MethodPut, "/b/k2", []byte("nope")), http.StatusForbidden)
}

func TestAssumeRoleDurationParsed(t *testing.T) {
	h := newSTSHarness(t)
	res := h.assumeRoleAs(testCreds, url.Values{"Action": {"AssumeRole"}, "DurationSeconds": {"900"}})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d; body=%s", res.status, res.body)
	}
	var parsed assumeRoleResponse
	if err := xml.Unmarshal(res.body, &parsed); err != nil {
		t.Fatalf("decode: %v", err)
	}
	exp, err := time.Parse(time.RFC3339, parsed.Result.Credentials.Expiration)
	if err != nil {
		t.Fatal(err)
	}
	// 15 minutes out, within a generous skew for the round trip.
	if d := time.Until(exp); d < 14*time.Minute || d > 16*time.Minute {
		t.Fatalf("expiration %v is not ~15m out (got %v)", exp, d)
	}
}

func TestAssumeRoleRejectsBadInput(t *testing.T) {
	h := newSTSHarness(t)

	// A malformed inline policy is a 400 MalformedPolicyDocument.
	res := h.assumeRoleAs(testCreds, url.Values{"Action": {"AssumeRole"}, "Policy": {"not json"}})
	mustSTSError(t, res, http.StatusBadRequest, "MalformedPolicyDocument")

	// A bad duration is a 400 ValidationError.
	res = h.assumeRoleAs(testCreds, url.Values{"Action": {"AssumeRole"}, "DurationSeconds": {"-5"}})
	mustSTSError(t, res, http.StatusBadRequest, "ValidationError")

	// An unsupported action is a 400 InvalidAction.
	res = h.assumeRoleAs(testCreds, url.Values{"Action": {"GetSessionToken"}})
	mustSTSError(t, res, http.StatusBadRequest, "InvalidAction")

	// The remaining federated flows are advertised as not implemented.
	res = h.assumeRoleAs(testCreds, url.Values{"Action": {"AssumeRoleWithLDAPIdentity"}})
	mustSTSError(t, res, http.StatusNotImplemented, "NotImplemented")
}

func TestAssumeRoleServiceAccountDenied(t *testing.T) {
	h := newSTSHarness(t)
	// A service account cannot assume a role (no chaining); the store says ErrNotFound,
	// which the endpoint maps to AccessDenied.
	if err := h.store.AddUser(auth.User{AccessKey: "alice", SecretKey: "as", Policies: []string{"readwrite"}}); err != nil {
		t.Fatal(err)
	}
	svc := auth.Credentials{AccessKey: "appkey", SecretKey: "appsecret"}
	if err := h.store.AddServiceAccount(auth.ServiceAccount{AccessKey: svc.AccessKey, SecretKey: svc.SecretKey, ParentUser: "alice"}); err != nil {
		t.Fatal(err)
	}
	res := h.assumeRoleAs(svc, url.Values{"Action": {"AssumeRole"}})
	mustSTSError(t, res, http.StatusForbidden, "AccessDenied")
}

func TestSTSDisabledByDefault(t *testing.T) {
	// A server built without WithSTS rejects the endpoint.
	store := auth.NewStaticStore(testCreds)
	drives := make([]storage.StorageAPI, 6)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	layer, err := object.NewSingleSet([16]byte{7, 7, 7}, drives, 2)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}
	server := NewServer(layer, store, WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewServer(server)
	t.Cleanup(srv.Close)

	body := []byte(url.Values{"Action": {"AssumeRole"}}.Encode())
	sum := sha256.Sum256(body)
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.ContentLength = int64(len(body))
	sign.SignHeader(req, testCreds, "us-east-1", hex.EncodeToString(sum[:]), time.Now().UTC())
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501", resp.StatusCode)
	}
}

// BenchmarkAssumeRole measures a full signed AssumeRole round trip: SigV4
// verification, form parsing, session minting (three crypto/rand reads), and XML
// encoding.
func BenchmarkAssumeRole(b *testing.B) {
	store := auth.NewStore(testCreds.AccessKey, testCreds.SecretKey)
	// The STS endpoint never touches the object layer, so a nil layer is fine here.
	server := NewServer(nil, store, WithSTS(store), WithClock(func() time.Time { return time.Now().UTC() }))

	form := []byte(url.Values{"Action": {"AssumeRole"}}.Encode())
	sum := sha256.Sum256(form)
	hash := hex.EncodeToString(sum[:])

	b.ReportAllocs()
	for b.Loop() {
		req := httptest.NewRequest(http.MethodPost, "http://sts.local/", bytes.NewReader(form))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.ContentLength = int64(len(form))
		sign.SignHeader(req, testCreds, "us-east-1", hash, time.Now().UTC())
		rec := httptest.NewRecorder()
		server.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			b.Fatalf("status = %d", rec.Code)
		}
	}
}

// --- Web identity (OIDC) federated flow ---

const webIdentityKid = "k1"

// webIDP is an in-process OIDC provider for the web-identity tests: it signs RS256
// tokens and serves the matching JWKS, so the unsigned federated flow is exercised
// over a real fetch.
type webIDP struct {
	issuer string
	key    *rsa.PrivateKey
}

// newWebIDP starts a JWKS server, registers the provider on the harness store, and
// returns the IdP for minting tokens.
func (h *stsHarness) newWebIDP(audience string) *webIDP {
	h.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		h.t.Fatalf("rsa key: %v", err)
	}
	idp := &webIDP{key: key}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		pub := &key.PublicKey
		doc := map[string]any{"keys": []map[string]string{{
			"kty": "RSA", "kid": webIdentityKid, "alg": "RS256",
			"n": b64u(pub.N.Bytes()), "e": b64u(big.NewInt(int64(pub.E)).Bytes()),
		}}}
		out, _ := json.Marshal(doc)
		_, _ = w.Write(out)
	}))
	h.t.Cleanup(srv.Close)
	idp.issuer = srv.URL
	h.store.SetHTTPClient(srv.Client())
	if err := h.store.RegisterWebIdentityProvider(auth.WebIdentityProvider{
		Name: "test", Issuer: srv.URL, Audiences: []string{audience}, JWKSURL: srv.URL,
	}); err != nil {
		h.t.Fatalf("RegisterWebIdentityProvider: %v", err)
	}
	return idp
}

// token mints a signed RS256 identity token for the given subject, audience, and
// policy claim, valid for an hour.
func (idp *webIDP) token(t *testing.T, sub, aud, policy string) string {
	t.Helper()
	now := time.Now().UTC()
	header, _ := json.Marshal(map[string]any{"alg": "RS256", "kid": webIdentityKid, "typ": "JWT"})
	payload, _ := json.Marshal(map[string]any{
		"iss": idp.issuer, "sub": sub, "aud": aud,
		"exp": now.Add(time.Hour).Unix(), "iat": now.Unix(), "policy": policy,
	})
	signing := b64u(header) + "." + b64u(payload)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, idp.key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign token: %v", err)
	}
	return signing + "." + b64u(sig)
}

func b64u(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// postForm sends an unsigned form POST to the service root, the way a web-identity
// client (which holds no liteio credentials) calls the STS endpoint.
func (h *stsHarness) postForm(form url.Values) result {
	h.t.Helper()
	resp, err := h.srv.Client().PostForm(h.srv.URL+"/", form)
	if err != nil {
		h.t.Fatalf("post form: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

// webIdentityCreds parses an AssumeRoleWithWebIdentity success body into signable
// credentials and returns the subject.
func webIdentityCreds(t *testing.T, res result) (auth.Credentials, string) {
	t.Helper()
	if res.status != http.StatusOK {
		t.Fatalf("AssumeRoleWithWebIdentity status = %d; body=%s", res.status, res.body)
	}
	var parsed assumeRoleWithWebIdentityResponse
	if err := xml.Unmarshal(res.body, &parsed); err != nil {
		t.Fatalf("decode: %v; body=%s", err, res.body)
	}
	c := parsed.Result.Credentials
	if c.AccessKeyID == "" || c.SecretAccessKey == "" || c.SessionToken == "" {
		t.Fatalf("incomplete credentials: %+v", c)
	}
	return auth.Credentials{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey}, parsed.Result.SubjectFromWebIdentityToken
}

func TestAssumeRoleWithWebIdentityUnsigned(t *testing.T) {
	h := newSTSHarness(t)
	idp := h.newWebIDP("liteio")

	// Admin seeds a bucket and object the federated session will read.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b/k", []byte("hello")), http.StatusOK)

	// An unsigned web-identity exchange (the token is the credential) maps to the
	// readonly policy and yields a usable session.
	token := idp.token(t, "alice@corp", "liteio", "readonly")
	creds, subject := webIdentityCreds(t, h.postForm(url.Values{
		"Action": {"AssumeRoleWithWebIdentity"}, "WebIdentityToken": {token},
	}))
	if subject != "alice@corp" {
		t.Fatalf("subject = %q, want alice@corp", subject)
	}
	// The readonly session reads but cannot write.
	mustStatus(t, h.doAs(creds, http.MethodGet, "/b/k", nil), http.StatusOK)
	mustStatus(t, h.doAs(creds, http.MethodPut, "/b/k2", []byte("nope")), http.StatusForbidden)
}

func TestAssumeRoleWithWebIdentityRejectsBadToken(t *testing.T) {
	h := newSTSHarness(t)
	h.newWebIDP("liteio")

	// A missing token is a ValidationError.
	mustSTSError(t, h.postForm(url.Values{"Action": {"AssumeRoleWithWebIdentity"}}), http.StatusBadRequest, "ValidationError")

	// A garbage token is an InvalidIdentityToken.
	mustSTSError(t, h.postForm(url.Values{
		"Action": {"AssumeRoleWithWebIdentity"}, "WebIdentityToken": {"not.a.jwt"},
	}), http.StatusBadRequest, "InvalidIdentityToken")
}

func TestAssumeRoleUnsignedRejected(t *testing.T) {
	h := newSTSHarness(t)
	// AssumeRole exchanges the caller's own credentials, so an unsigned request to
	// it is refused even though the unsigned STS path is open for federated flows.
	mustSTSError(t, h.postForm(url.Values{"Action": {"AssumeRole"}}), http.StatusForbidden, "AccessDenied")
}

// --- Certificate (X.509) federated flow ---

// certHarness wires the front door over TLS so a client certificate reaches the STS
// endpoint. The listener only requests a certificate; liteio verifies it against the
// store's own trust roots in AssumeRoleWithCertificate.
type certHarness struct {
	t      *testing.T
	srv    *httptest.Server
	store  *auth.Store
	client *http.Client
}

func newCertHarness(t *testing.T) (*certHarness, *certIssuer) {
	t.Helper()
	const n, parity = 6, 2
	drives := make([]storage.StorageAPI, n)
	for i := range drives {
		d, err := local.New(t.TempDir())
		if err != nil {
			t.Fatalf("local.New: %v", err)
		}
		drives[i] = d
	}
	layer, err := object.NewSingleSet([16]byte{5, 5, 5}, drives, parity)
	if err != nil {
		t.Fatalf("NewSingleSet: %v", err)
	}

	store := auth.NewStore(testCreds.AccessKey, testCreds.SecretKey)
	ca := newCertIssuer(t)
	if err := store.RegisterCertificateProvider(auth.CertificateProvider{Name: "test", Roots: ca.roots()}); err != nil {
		t.Fatalf("RegisterCertificateProvider: %v", err)
	}

	server := NewServer(layer, store,
		WithAuthorizer(store),
		WithSTS(store),
		WithClock(func() time.Time { return time.Now().UTC() }))
	srv := httptest.NewUnstartedServer(server)
	// Request a client certificate but do not verify it at the TLS layer: liteio
	// makes the trust decision against its own roots in the STS handler.
	srv.TLS = &tls.Config{ClientAuth: tls.RequestClientCert}
	srv.StartTLS()
	t.Cleanup(srv.Close)

	h := &certHarness{t: t, srv: srv, store: store, client: srv.Client()}
	return h, ca
}

// withClientCert returns an HTTP client that trusts the test server and presents the
// given client certificate on the TLS handshake.
func (h *certHarness) withClientCert(cert *tls.Certificate) *http.Client {
	base := h.srv.Client().Transport.(*http.Transport).Clone()
	if cert != nil {
		base.TLSClientConfig.Certificates = []tls.Certificate{*cert}
	}
	return &http.Client{Transport: base}
}

// postFormCert sends an unsigned form POST over TLS with the given client (which may
// carry a client certificate), the way a certificate client calls the STS endpoint.
func (h *certHarness) postFormCert(client *http.Client, form url.Values) result {
	h.t.Helper()
	resp, err := client.PostForm(h.srv.URL+"/", form)
	if err != nil {
		h.t.Fatalf("post form: %v", err)
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(resp.Body)
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

func TestAssumeRoleWithCertificate(t *testing.T) {
	h, ca := newCertHarness(t)

	// Admin seeds a bucket and object the federated session will read.
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b", nil), http.StatusOK)
	mustStatus(t, h.doAs(testCreds, http.MethodPut, "/b/k", []byte("hello")), http.StatusOK)

	// A client cert whose subject organization names the readonly policy yields a
	// usable readonly session over the unsigned certificate flow.
	cert := ca.issue(t, "alice@corp", []string{"readonly"})
	res := h.postFormCert(h.withClientCert(cert), url.Values{"Action": {"AssumeRoleWithCertificate"}})
	if res.status != http.StatusOK {
		t.Fatalf("status = %d; body=%s", res.status, res.body)
	}
	var parsed assumeRoleWithCertificateResponse
	if err := xml.Unmarshal(res.body, &parsed); err != nil {
		t.Fatalf("decode: %v; body=%s", err, res.body)
	}
	c := parsed.Result.Credentials
	creds := auth.Credentials{AccessKey: c.AccessKeyID, SecretKey: c.SecretAccessKey}
	if creds.AccessKey == "" || creds.SecretKey == "" {
		t.Fatalf("incomplete credentials: %+v", c)
	}
	// The readonly session reads but cannot write.
	mustStatus(t, h.doAs(creds, http.MethodGet, "/b/k", nil), http.StatusOK)
	mustStatus(t, h.doAs(creds, http.MethodPut, "/b/k2", []byte("nope")), http.StatusForbidden)
}

func (h *certHarness) doAs(c auth.Credentials, method, path string, body []byte) result {
	h.t.Helper()
	var rdr io.Reader
	hash := sign.EmptyPayloadHash
	if body != nil {
		rdr = bytes.NewReader(body)
		sum := sha256.Sum256(body)
		hash = hex.EncodeToString(sum[:])
	}
	req, err := http.NewRequest(method, h.srv.URL+path, rdr)
	if err != nil {
		h.t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.ContentLength = int64(len(body))
	}
	sign.SignHeader(req, c, "us-east-1", hash, time.Now().UTC())
	resp, err := h.client.Do(req)
	if err != nil {
		h.t.Fatalf("do %s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		h.t.Fatalf("read body: %v", err)
	}
	return result{status: resp.StatusCode, header: resp.Header, body: rb}
}

func TestAssumeRoleWithCertificateRejections(t *testing.T) {
	h, ca := newCertHarness(t)

	// No client certificate at all is a ValidationError.
	mustSTSError(t, h.postFormCert(h.withClientCert(nil), url.Values{"Action": {"AssumeRoleWithCertificate"}}),
		http.StatusBadRequest, "ValidationError")

	// A certificate from an untrusted CA is refused as AccessDenied.
	foreign := newCertIssuer(t)
	bad := foreign.issue(t, "mallory", []string{"readonly"})
	mustSTSError(t, h.postFormCert(h.withClientCert(bad), url.Values{"Action": {"AssumeRoleWithCertificate"}}),
		http.StatusForbidden, "AccessDenied")

	// A trusted certificate that names no known policy is also AccessDenied.
	noPolicy := ca.issue(t, "nobody", []string{"unknown-ou"})
	mustSTSError(t, h.postFormCert(h.withClientCert(noPolicy), url.Values{"Action": {"AssumeRoleWithCertificate"}}),
		http.StatusForbidden, "AccessDenied")
}

// certIssuer signs client certificates for the certificate-flow tests.
type certIssuer struct {
	caCert *x509.Certificate
	caKey  *ecdsa.PrivateKey
}

func newCertIssuer(t *testing.T) *certIssuer {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ca key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create CA: %v", err)
	}
	caCert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("parse CA: %v", err)
	}
	return &certIssuer{caCert: caCert, caKey: key}
}

func (c *certIssuer) roots() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(c.caCert)
	return pool
}

// issue signs a client certificate and packages it as a tls.Certificate (leaf plus
// private key) the client presents on the handshake.
func (c *certIssuer) issue(t *testing.T, cn string, orgs []string) *tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("leaf key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn, Organization: orgs},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.caCert, &key.PublicKey, c.caKey)
	if err != nil {
		t.Fatalf("create leaf: %v", err)
	}
	return &tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

// mustSTSError asserts the response is an STS error envelope with the wanted status
// and code.
func mustSTSError(t *testing.T, res result, status int, code string) {
	t.Helper()
	if res.status != status {
		t.Fatalf("status = %d, want %d; body=%s", res.status, status, res.body)
	}
	var parsed stsErrorResponse
	if err := xml.Unmarshal(res.body, &parsed); err != nil {
		t.Fatalf("decode STS error: %v; body=%s", err, res.body)
	}
	if parsed.Code != code {
		t.Fatalf("code = %q, want %q", parsed.Code, code)
	}
	if !strings.Contains(string(res.body), "ErrorResponse") {
		t.Fatalf("not an STS ErrorResponse envelope: %s", res.body)
	}
}
