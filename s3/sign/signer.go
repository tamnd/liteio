// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/liteio/auth"
)

// SignHeader signs r in place with header-based SigV4, setting X-Amz-Date,
// X-Amz-Content-Sha256, and Authorization. payloadHash is the hex SHA256 of the
// body (use EmptyPayloadHash for an empty body, or UnsignedPayload for a body the
// client declines to hash). It is the inverse of verifyHeader and is what tests
// and the internode client use to produce valid requests.
func SignHeader(r *http.Request, cred auth.Credentials, region, payloadHash string, t time.Time) {
	if payloadHash == "" {
		payloadHash = unsignedBody
	}
	t = t.UTC()
	r.Header.Set("X-Amz-Date", t.Format(iso8601))
	r.Header.Set("X-Amz-Content-Sha256", payloadHash)

	signed := signedHeaderList(r)
	scope := credentialScope{accessKey: cred.AccessKey, date: t.Format(yyyymmdd), region: region, service: serviceS3}

	canonReq := canonicalRequest(r.Method, canonicalURIPath(r), canonicalQuery(r.URL.Query(), ""), canonicalHeaders(r, signed), strings.Join(signed, ";"), payloadHash)
	sts := stringToSign(t, scope, canonReq)
	sig := computeSignature(cred.SecretKey, scope, sts)

	r.Header.Set("Authorization", algorithm+" Credential="+cred.AccessKey+"/"+scope.scopeString()+", SignedHeaders="+strings.Join(signed, ";")+", Signature="+sig)
}

// UnsignedPayload is the body-hash sentinel for requests that decline to hash the
// payload (the common choice for streaming or large uploads under header auth).
const UnsignedPayload = unsignedBody

// signedHeaderList returns the lowercase, sorted set of headers to sign: host,
// every x-amz-*, and content-type if present.
func signedHeaderList(r *http.Request) []string {
	set := map[string]bool{"host": true}
	for k := range r.Header {
		lk := strings.ToLower(k)
		if lk == "content-type" || strings.HasPrefix(lk, "x-amz-") {
			set[lk] = true
		}
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
