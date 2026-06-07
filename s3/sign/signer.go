// SPDX-License-Identifier: Apache-2.0

package sign

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/liteio/auth"
)

// UnsignedPayload is the body-hash sentinel for requests that decline to hash the
// payload (the common choice for streaming or large uploads under header auth).
const UnsignedPayload = unsignedBody

// SignHeader signs r in place with header-based SigV4, setting X-Amz-Date,
// X-Amz-Content-Sha256, and Authorization. payloadHash is the hex SHA256 of the
// body (use EmptyPayloadHash for an empty body, or UnsignedPayload for a body the
// client declines to hash). It is the inverse of verifyHeader and is what tests
// and the internode client use to produce valid requests.
func SignHeader(r *http.Request, cred auth.Credentials, region, payloadHash string, t time.Time) {
	signHeaderReturning(r, cred, region, payloadHash, t)
}

// signHeaderReturning signs r and returns the computed signature, which seeds the
// chunk chain for streaming bodies.
func signHeaderReturning(r *http.Request, cred auth.Credentials, region, payloadHash string, t time.Time) string {
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
	return sig
}

// SignStreaming signs r as an aws-chunked streaming upload and returns the framed
// request body to send: it sets the streaming headers, computes the header (seed)
// signature, and frames payload into signed chunks of chunkSize bytes (64 KiB
// when chunkSize <= 0) plus the terminating zero chunk. It is the client-side
// inverse of chunkedReader, used by tests and future streaming clients.
func SignStreaming(r *http.Request, cred auth.Credentials, region string, payload []byte, chunkSize int, t time.Time) []byte {
	if chunkSize <= 0 {
		chunkSize = 64 << 10
	}
	t = t.UTC()
	r.Header.Set("Content-Encoding", "aws-chunked")
	r.Header.Set("x-amz-decoded-content-length", strconv.Itoa(len(payload)))

	seed := signHeaderReturning(r, cred, region, streamingSigned, t)
	scope := credentialScope{accessKey: cred.AccessKey, date: t.Format(yyyymmdd), region: region, service: serviceS3}
	key := signingKey(cred.SecretKey, scope)
	amzDate := t.Format(iso8601)

	var body bytes.Buffer
	prev := seed
	writeChunk := func(data []byte) {
		dh := sha256.Sum256(data)
		sts := chunkStringToSignAlgo + "\n" + amzDate + "\n" + scope.scopeString() + "\n" +
			prev + "\n" + EmptyPayloadHash + "\n" + hex.EncodeToString(dh[:])
		sig := hex.EncodeToString(hmacSHA256(key, sts))
		body.WriteString(strconv.FormatInt(int64(len(data)), 16))
		body.WriteString(";chunk-signature=")
		body.WriteString(sig)
		body.WriteString("\r\n")
		body.Write(data)
		body.WriteString("\r\n")
		prev = sig
	}
	for off := 0; off < len(payload); off += chunkSize {
		writeChunk(payload[off:min(off+chunkSize, len(payload))])
	}
	writeChunk(nil) // terminating zero-length chunk

	framed := body.Bytes()
	r.ContentLength = int64(len(framed))
	r.Header.Set("Content-Length", strconv.Itoa(len(framed)))
	return framed
}

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
