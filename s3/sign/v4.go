// SPDX-License-Identifier: Apache-2.0

// Package sign implements AWS Signature Version 4 for the S3 front door (spec
// 2020, doc 02 §2.3). It verifies inbound requests — header authorization and
// presigned query authorization — and offers a Sign helper used by tests and the
// (later) replication/internode clients. Streaming aws-chunked payloads are a
// documented follow-on; this cut handles the signed-header and presigned modes
// with either a hashed or UNSIGNED-PAYLOAD body.
package sign

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/tamnd/liteio/auth"
)

const (
	algorithm    = "AWS4-HMAC-SHA256"
	serviceS3    = "s3"
	terminator   = "aws4_request"
	iso8601      = "20060102T150405Z"
	yyyymmdd     = "20060102"
	maxSkew      = 15 * time.Minute
	maxPresign   = 7 * 24 * time.Hour
	unsignedBody = "UNSIGNED-PAYLOAD"
	// EmptyPayloadHash is SHA256("") — the body hash a request with no body uses.
	EmptyPayloadHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
)

// Error is a signing failure mapped to an S3 code by the caller. Code is the S3
// error code (e.g. SignatureDoesNotMatch); Message is human detail.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }

var (
	errMissingAuth = &Error{"MissingSecurityHeader", "missing Authorization"}
	errMalformed   = &Error{"AuthorizationHeaderMalformed", "the authorization header is malformed"}
	errBadAlgo     = &Error{"InvalidRequest", "unsupported signature algorithm"}
	errNoKey       = &Error{"InvalidAccessKeyId", "the access key does not exist"}
	errMismatch    = &Error{"SignatureDoesNotMatch", "the request signature does not match"}
	errSkew        = &Error{"RequestTimeTooSkewed", "the request time is too far from server time"}
	errExpired     = &Error{"AccessDenied", "request has expired"}
	errBadDate     = &Error{"AccessDenied", "invalid date"}
)

// credentialScope is the parsed Credential= value: AK/date/region/service/term.
type credentialScope struct {
	accessKey string
	date      string // yyyymmdd
	region    string
	service   string
}

func (c credentialScope) scopeString() string {
	return c.date + "/" + c.region + "/" + c.service + "/" + terminator
}

// Verify authenticates an inbound request against the credential store. On
// success it returns the access key whose secret signed the request. It picks the
// presigned path when the query carries X-Amz-Signature, otherwise the header
// path. now is the server's clock (injected for testability). liteio does not pin
// a region: the scope region is echoed back in the signing key, so any region a
// client signs with verifies as long as the secret matches (the MinIO-style lax
// default, doc 02 §2.3).
func Verify(r *http.Request, store auth.CredentialStore, now time.Time) (string, *Error) {
	if r.URL.Query().Get("X-Amz-Signature") != "" {
		return verifyPresigned(r, store, now)
	}
	return verifyHeader(r, store, now)
}

// verifyHeader handles Authorization-header SigV4.
func verifyHeader(r *http.Request, store auth.CredentialStore, now time.Time) (string, *Error) {
	authHdr := r.Header.Get("Authorization")
	if authHdr == "" {
		return "", errMissingAuth
	}
	scope, signedHeaders, providedSig, err := parseAuthHeader(authHdr)
	if err != nil {
		return "", err
	}
	if scope.service != serviceS3 {
		return "", errBadAlgo
	}

	t, terr := requestTime(r)
	if terr != nil {
		return "", terr
	}
	if absDur(now.Sub(t)) > maxSkew {
		return "", errSkew
	}

	cred, ok := store.Get(scope.accessKey)
	if !ok {
		return "", errNoKey
	}

	payloadHash := r.Header.Get("X-Amz-Content-Sha256")
	if payloadHash == "" {
		payloadHash = unsignedBody
	}

	canonReq := canonicalRequest(r.Method, canonicalURIPath(r), canonicalQuery(r.URL.Query(), ""), canonicalHeaders(r, signedHeaders), strings.Join(signedHeaders, ";"), payloadHash)
	sts := stringToSign(t, scope, canonReq)
	want := computeSignature(cred.SecretKey, scope, sts)
	if subtle.ConstantTimeCompare([]byte(want), []byte(providedSig)) != 1 {
		return "", errMismatch
	}
	return scope.accessKey, nil
}

// verifyPresigned handles query-string (presigned URL) SigV4.
func verifyPresigned(r *http.Request, store auth.CredentialStore, now time.Time) (string, *Error) {
	q := r.URL.Query()
	if q.Get("X-Amz-Algorithm") != algorithm {
		return "", errBadAlgo
	}
	scope, perr := parseCredential(q.Get("X-Amz-Credential"))
	if perr != nil {
		return "", perr
	}
	if scope.service != serviceS3 {
		return "", errBadAlgo
	}
	t, terr := time.Parse(iso8601, q.Get("X-Amz-Date"))
	if terr != nil {
		return "", errBadDate
	}
	expires, eerr := time.ParseDuration(q.Get("X-Amz-Expires") + "s")
	if eerr != nil || expires <= 0 || expires > maxPresign {
		return "", errMalformed
	}
	if now.After(t.Add(expires)) {
		return "", errExpired
	}
	if absDur(now.Sub(t)) > maxSkew+expires {
		return "", errSkew
	}

	cred, ok := store.Get(scope.accessKey)
	if !ok {
		return "", errNoKey
	}

	signedHeaders := strings.Split(q.Get("X-Amz-SignedHeaders"), ";")
	sort.Strings(signedHeaders)
	providedSig := q.Get("X-Amz-Signature")

	// The canonical query omits X-Amz-Signature; everything else is signed.
	canonReq := canonicalRequest(r.Method, canonicalURIPath(r), canonicalQuery(q, "X-Amz-Signature"), canonicalHeaders(r, signedHeaders), strings.Join(signedHeaders, ";"), unsignedBody)
	sts := stringToSign(t, scope, canonReq)
	want := computeSignature(cred.SecretKey, scope, sts)
	if subtle.ConstantTimeCompare([]byte(want), []byte(providedSig)) != 1 {
		return "", errMismatch
	}
	return scope.accessKey, nil
}

// --- canonicalization -----------------------------------------------------

// canonicalRequest assembles the SigV4 canonical request string.
func canonicalRequest(method, uri, query, headers, signedHeaders, payloadHash string) string {
	return method + "\n" + uri + "\n" + query + "\n" + headers + "\n" + signedHeaders + "\n" + payloadHash
}

// canonicalURIPath is the URI-encoded path. S3 encodes each segment once and
// keeps the slashes; an empty path is "/".
func canonicalURIPath(r *http.Request) string {
	p := r.URL.Path
	if p == "" {
		return "/"
	}
	segs := strings.Split(p, "/")
	for i, s := range segs {
		segs[i] = uriEncode(s, false)
	}
	return strings.Join(segs, "/")
}

// canonicalQuery is the sorted, encoded query string, optionally excluding one
// key (X-Amz-Signature for presigned requests).
func canonicalQuery(q url.Values, exclude string) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		if k == exclude {
			continue
		}
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for i, k := range keys {
		vals := append([]string(nil), q[k]...)
		sort.Strings(vals)
		for j, v := range vals {
			if i > 0 || j > 0 {
				b.WriteByte('&')
			}
			b.WriteString(uriEncode(k, true))
			b.WriteByte('=')
			b.WriteString(uriEncode(v, true))
		}
	}
	return b.String()
}

// canonicalHeaders builds the canonical-headers block for the signed-header set.
// Each is lowercased name, trimmed value, newline-terminated, in sorted order.
func canonicalHeaders(r *http.Request, signedHeaders []string) string {
	var b strings.Builder
	for _, h := range signedHeaders {
		b.WriteString(h)
		b.WriteByte(':')
		b.WriteString(trimAll(headerValue(r, h)))
		b.WriteByte('\n')
	}
	return b.String()
}

// headerValue returns the value of header h, with the host pseudo-header pulled
// from r.Host (Go strips Host from r.Header).
func headerValue(r *http.Request, h string) string {
	if h == "host" {
		return r.Host
	}
	return strings.Join(r.Header.Values(http.CanonicalHeaderKey(h)), ",")
}

// trimAll collapses internal runs of spaces and trims the ends, per the SigV4
// header-value normalization rule.
func trimAll(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// stringToSign builds the SigV4 string-to-sign from the canonical request.
func stringToSign(t time.Time, scope credentialScope, canonReq string) string {
	h := sha256.Sum256([]byte(canonReq))
	return algorithm + "\n" + t.UTC().Format(iso8601) + "\n" + scope.scopeString() + "\n" + hex.EncodeToString(h[:])
}

// computeSignature derives the signing key and signs the string-to-sign.
func computeSignature(secret string, scope credentialScope, sts string) string {
	kDate := hmacSHA256([]byte("AWS4"+secret), scope.date)
	kRegion := hmacSHA256(kDate, scope.region)
	kService := hmacSHA256(kRegion, scope.service)
	kSigning := hmacSHA256(kService, terminator)
	return hex.EncodeToString(hmacSHA256(kSigning, sts))
}

func hmacSHA256(key []byte, data string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(data))
	return m.Sum(nil)
}

// --- parsing --------------------------------------------------------------

// parseAuthHeader splits an Authorization header into scope, signed headers, and
// the provided signature.
func parseAuthHeader(h string) (credentialScope, []string, string, *Error) {
	if !strings.HasPrefix(h, algorithm+" ") {
		return credentialScope{}, nil, "", errBadAlgo
	}
	var cred, signed, sig string
	for part := range strings.SplitSeq(strings.TrimPrefix(h, algorithm+" "), ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.HasPrefix(part, "Credential="):
			cred = strings.TrimPrefix(part, "Credential=")
		case strings.HasPrefix(part, "SignedHeaders="):
			signed = strings.TrimPrefix(part, "SignedHeaders=")
		case strings.HasPrefix(part, "Signature="):
			sig = strings.TrimPrefix(part, "Signature=")
		}
	}
	if cred == "" || signed == "" || sig == "" {
		return credentialScope{}, nil, "", errMalformed
	}
	scope, err := parseCredential(cred)
	if err != nil {
		return credentialScope{}, nil, "", err
	}
	headers := strings.Split(signed, ";")
	sort.Strings(headers)
	return scope, headers, sig, nil
}

// parseCredential parses AK/date/region/service/aws4_request.
func parseCredential(cred string) (credentialScope, *Error) {
	parts := strings.Split(cred, "/")
	if len(parts) != 5 || parts[4] != terminator {
		return credentialScope{}, errMalformed
	}
	return credentialScope{accessKey: parts[0], date: parts[1], region: parts[2], service: parts[3]}, nil
}

// requestTime reads the request timestamp from X-Amz-Date (preferred) or Date.
func requestTime(r *http.Request) (time.Time, *Error) {
	if v := r.Header.Get("X-Amz-Date"); v != "" {
		t, err := time.Parse(iso8601, v)
		if err != nil {
			return time.Time{}, errBadDate
		}
		return t, nil
	}
	if v := r.Header.Get("Date"); v != "" {
		t, err := http.ParseTime(v)
		if err != nil {
			return time.Time{}, errBadDate
		}
		return t, nil
	}
	return time.Time{}, errBadDate
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// uriEncode percent-encodes per the AWS SigV4 rules: unreserved characters
// (A-Z a-z 0-9 - _ . ~) pass through, '/' passes through only when encodeSlash
// is false, and everything else is %XX with uppercase hex.
func uriEncode(s string, encodeSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'A' && c <= 'Z', c >= 'a' && c <= 'z', c >= '0' && c <= '9',
			c == '-', c == '_', c == '.', c == '~':
			b.WriteByte(c)
		case c == '/' && !encodeSlash:
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		}
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"
