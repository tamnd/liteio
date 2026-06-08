// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// signWithService signs r in place exactly as SignHeader does but for an arbitrary
// service name, so the test can produce a genuine sts-scoped signature (the public
// SignHeader always uses s3).
func signWithService(r *http.Request, service, region, payloadHash string, t time.Time) {
	t = t.UTC()
	r.Header.Set("X-Amz-Date", t.Format(iso8601))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)
	signed := signedHeaderList(r)
	scope := credentialScope{accessKey: testCred.AccessKey, date: t.Format(yyyymmdd), region: region, service: service}
	canonReq := canonicalRequest(r.Method, canonicalURIPath(r), canonicalQuery(r.URL.Query(), ""), canonicalHeaders(r, signed), strings.Join(signed, ";"), payloadHash)
	sts := stringToSign(t, scope, canonReq)
	sig := computeSignature(testCred.SecretKey, scope, sts)
	r.Header.Set("Authorization", algorithm+" Credential="+testCred.AccessKey+"/"+scope.scopeString()+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
}

func TestSTSServiceAccepted(t *testing.T) {
	// A request signed with the "sts" service verifies on the shared endpoint, since
	// liteio serves STS alongside S3 (allowedService).
	r := httptest.NewRequest(http.MethodPost, "http://s3.local/", strings.NewReader("Action=AssumeRole"))
	r.Host = "s3.local"
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	signWithService(r, "sts", "us-east-1", bodyHash("Action=AssumeRole"), fixedTime)

	vr, err := Verify(r, testStore, fixedTime)
	if err != nil {
		t.Fatalf("verify sts-signed request: %v", err)
	}
	if vr.AccessKey != testCred.AccessKey {
		t.Fatalf("access key = %q, want %q", vr.AccessKey, testCred.AccessKey)
	}
}

func TestForeignServiceRejected(t *testing.T) {
	// A service liteio does not serve (here "iam") is still rejected, so the
	// relaxation is scoped to s3 and sts only.
	r := httptest.NewRequest(http.MethodGet, "http://s3.local/", nil)
	r.Host = "s3.local"
	signWithService(r, "iam", "us-east-1", EmptyPayloadHash, fixedTime)
	if _, err := Verify(r, testStore, fixedTime); err == nil {
		t.Fatal("expected an iam-service signature to be rejected")
	}
}
