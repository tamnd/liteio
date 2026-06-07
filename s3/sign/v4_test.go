// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
)

var (
	testCred  = auth.Credentials{AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY"}
	testStore = auth.NewStaticStore(testCred)
	fixedTime = time.Date(2026, 6, 7, 12, 0, 0, 0, time.UTC)
)

func bodyHash(b string) string {
	h := sha256.Sum256([]byte(b))
	return hex.EncodeToString(h[:])
}

func TestSigningKeyDerivationVector(t *testing.T) {
	// AWS-documented signing-key example ("Examples of how to derive a signing
	// key", service=iam, 20150830/us-east-1). Pins the HMAC chain.
	scope := credentialScope{date: "20150830", region: "us-east-1", service: "iam"}
	kDate := hmacSHA256([]byte("AWS4wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY"), scope.date)
	kRegion := hmacSHA256(kDate, scope.region)
	kService := hmacSHA256(kRegion, scope.service)
	kSigning := hmacSHA256(kService, terminator)
	got := hex.EncodeToString(kSigning)
	const want = "c4afb1cc5771d871763a393e44b703571b55cc28424d1a5e86da6ed3c154a4b9"
	if got != want {
		t.Fatalf("signing key = %s, want %s", got, want)
	}
}

func TestHeaderSignRoundTrip(t *testing.T) {
	body := "hello world"
	r := httptest.NewRequest(http.MethodPut, "http://s3.local/bucket/key.txt", strings.NewReader(body))
	r.Host = "s3.local"
	r.Header.Set("Content-Type", "text/plain")
	SignHeader(r, testCred, "us-east-1", bodyHash(body), fixedTime)

	ak, err := Verify(r, testStore, fixedTime)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if ak != testCred.AccessKey {
		t.Fatalf("access key = %q, want %q", ak, testCred.AccessKey)
	}
}

func TestHeaderSignRoundTripWithQuery(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket?list-type=2&prefix=a/b&max-keys=10", nil)
	r.Host = "s3.local"
	SignHeader(r, testCred, "us-east-1", EmptyPayloadHash, fixedTime)
	if _, err := Verify(r, testStore, fixedTime); err != nil {
		t.Fatalf("verify with query: %v", err)
	}
}

func TestTamperedSignatureRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key", nil)
	r.Host = "s3.local"
	SignHeader(r, testCred, "us-east-1", EmptyPayloadHash, fixedTime)

	// Flip the path after signing: the signature must no longer match.
	r.URL.Path = "/bucket/other"
	if _, err := Verify(r, testStore, fixedTime); err == nil || err.Code != "SignatureDoesNotMatch" {
		t.Fatalf("expected SignatureDoesNotMatch, got %v", err)
	}
}

func TestWrongSecretRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key", nil)
	r.Host = "s3.local"
	bad := auth.Credentials{AccessKey: testCred.AccessKey, SecretKey: "not-the-secret"}
	SignHeader(r, bad, "us-east-1", EmptyPayloadHash, fixedTime)
	if _, err := Verify(r, testStore, fixedTime); err == nil || err.Code != "SignatureDoesNotMatch" {
		t.Fatalf("expected SignatureDoesNotMatch, got %v", err)
	}
}

func TestUnknownAccessKeyRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key", nil)
	r.Host = "s3.local"
	other := auth.Credentials{AccessKey: "AKIAUNKNOWN", SecretKey: "x"}
	SignHeader(r, other, "us-east-1", EmptyPayloadHash, fixedTime)
	if _, err := Verify(r, testStore, fixedTime); err == nil || err.Code != "InvalidAccessKeyId" {
		t.Fatalf("expected InvalidAccessKeyId, got %v", err)
	}
}

func TestClockSkewRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key", nil)
	r.Host = "s3.local"
	SignHeader(r, testCred, "us-east-1", EmptyPayloadHash, fixedTime)
	if _, err := Verify(r, testStore, fixedTime.Add(30*time.Minute)); err == nil || err.Code != "RequestTimeTooSkewed" {
		t.Fatalf("expected RequestTimeTooSkewed, got %v", err)
	}
}

func TestMissingAuthRejected(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key", nil)
	r.Host = "s3.local"
	if _, err := Verify(r, testStore, fixedTime); err == nil {
		t.Fatal("expected error for unsigned request")
	}
}

func TestPresignRoundTrip(t *testing.T) {
	// Build a presigned GET by hand and verify it.
	q := r2q()
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key?"+q.encodeFor(testCred, fixedTime, 3600), nil)
	r.Host = "s3.local"
	if _, err := Verify(r, testStore, fixedTime.Add(10*time.Minute)); err != nil {
		t.Fatalf("verify presigned: %v", err)
	}
}

func TestPresignExpired(t *testing.T) {
	q := r2q()
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/bucket/key?"+q.encodeFor(testCred, fixedTime, 60), nil)
	r.Host = "s3.local"
	if _, err := Verify(r, testStore, fixedTime.Add(2*time.Minute)); err == nil || err.Code != "AccessDenied" {
		t.Fatalf("expected AccessDenied (expired), got %v", err)
	}
}

// presignBuilder is a tiny presigned-URL constructor used only by the tests; it
// mirrors what an SDK does so the verifier is exercised against an independent
// implementation rather than its own SignHeader.
type presignBuilder struct{ host, path string }

func r2q() presignBuilder { return presignBuilder{host: "s3.local", path: "/bucket/key"} }

func (p presignBuilder) encodeFor(cred auth.Credentials, t time.Time, expires int) string {
	t = t.UTC()
	scope := credentialScope{accessKey: cred.AccessKey, date: t.Format(yyyymmdd), region: "us-east-1", service: serviceS3}
	q := qmap(map[string]string{
		"X-Amz-Algorithm":     algorithm,
		"X-Amz-Credential":    cred.AccessKey + "/" + scope.scopeString(),
		"X-Amz-Date":          t.Format(iso8601),
		"X-Amz-Expires":       strconv.Itoa(expires),
		"X-Amz-SignedHeaders": "host",
	})
	canonReq := canonicalRequest(http.MethodGet, p.path, q.canonical(), "host:"+p.host+"\n", "host", unsignedBody)
	sts := stringToSign(t, scope, canonReq)
	sig := computeSignature(cred.SecretKey, scope, sts)
	return q.canonical() + "&X-Amz-Signature=" + sig
}

// qmap is a minimal ordered query helper for the test presigner.
type qmap map[string]string

func (u qmap) canonical() string {
	keys := make([]string, 0, len(u))
	for k := range u {
		keys = append(keys, k)
	}
	// canonical order
	for i := 0; i < len(keys); i++ {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	var b strings.Builder
	for i, k := range keys {
		if i > 0 {
			b.WriteByte('&')
		}
		b.WriteString(uriEncode(k, true))
		b.WriteByte('=')
		b.WriteString(uriEncode(u[k], true))
	}
	return b.String()
}
