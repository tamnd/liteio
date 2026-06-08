// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"crypto/x509"
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

// The XML namespace on every STS response body (the AssumeRoleResponse and
// ErrorResponse XMLName tags) is the AWS STS 2011-06-15 namespace, which clients
// that parse the body expect verbatim.

// STSIssuer mints temporary session credentials. AssumeRole exchanges a long-lived
// identity (the signed caller) for a session; AssumeRoleWithWebIdentity exchanges
// an external OIDC token (the unsigned federated flow) for one, returning the token
// subject alongside the session. It is the slice of the IAM store the STS endpoint
// drives; *auth.Store satisfies it. The interface keeps the s3 package depending on
// auth for types only, and lets the endpoint be tested against a fake.
type STSIssuer interface {
	AssumeRole(parentKey string, sessionPolicy *auth.Policy, ttl time.Duration) (auth.Session, error)
	AssumeRoleWithWebIdentity(token string, sessionPolicy *auth.Policy, ttl time.Duration) (auth.Session, string, error)
	AssumeRoleWithCertificate(chain []*x509.Certificate, sessionPolicy *auth.Policy, ttl time.Duration) (auth.Session, string, error)
	AssumeRoleWithLDAPIdentity(username, password string, sessionPolicy *auth.Policy, ttl time.Duration) (auth.Session, string, error)
}

// WithSTS enables the STS endpoint at the service root, issuing sessions from the
// given store. Without it a POST to the root is MethodNotAllowed, as before.
func WithSTS(issuer STSIssuer) Option { return func(s *Server) { s.sts = issuer } }

// serveSTS handles a POST to the service root: the internal STS (doc 08.4). The
// request is already SigV4-authenticated; the signing identity (vr.AccessKey) is
// the principal assuming the role. Authorization is intrinsic — AssumeRole only
// lets root or an existing user assume — so the S3 action gate does not apply here.
func (s *Server) serveSTS(w http.ResponseWriter, r *http.Request, requestID string, vr *sign.VerifiedRequest) {
	if s.sts == nil {
		writeSTSError(w, requestID, errSTSNotImplemented)
		return
	}
	if err := r.ParseForm(); err != nil {
		writeSTSError(w, requestID, stsError{http.StatusBadRequest, "InvalidParameterValue", "the request form could not be parsed"})
		return
	}
	switch r.Form.Get("Action") {
	case "AssumeRole":
		// AssumeRole exchanges the caller's own credentials, so it requires a signed
		// request; the unsigned STS path (vr == nil) reaches only the federated flows.
		if vr == nil {
			writeSTSError(w, requestID, stsError{http.StatusForbidden, "AccessDenied", "AssumeRole requires a signed request"})
			return
		}
		s.assumeRole(w, r, requestID, vr)
	case "AssumeRoleWithWebIdentity":
		s.assumeRoleWithWebIdentity(w, r, requestID)
	case "AssumeRoleWithCertificate":
		s.assumeRoleWithCertificate(w, r, requestID)
	case "AssumeRoleWithLDAPIdentity":
		s.assumeRoleWithLDAPIdentity(w, r, requestID)
	default:
		writeSTSError(w, requestID, stsError{http.StatusBadRequest, "InvalidAction", "the STS Action is missing or not supported"})
	}
}

// sessionParams parses the DurationSeconds and inline Policy a session request may
// carry, shared by AssumeRole and the federated flows. A non-nil *stsError means the
// input was rejected and the caller should stop.
func sessionParams(r *http.Request) (time.Duration, *auth.Policy, *stsError) {
	var ttl time.Duration
	if v := r.Form.Get("DurationSeconds"); v != "" {
		secs, err := strconv.Atoi(v)
		if err != nil || secs < 0 {
			return 0, nil, &stsError{http.StatusBadRequest, "ValidationError", "DurationSeconds must be a non-negative integer"}
		}
		ttl = time.Duration(secs) * time.Second
	}
	var policy *auth.Policy
	if doc := r.Form.Get("Policy"); doc != "" {
		p, err := auth.ParsePolicy([]byte(doc))
		if err != nil {
			return 0, nil, &stsError{http.StatusBadRequest, "MalformedPolicyDocument", err.Error()}
		}
		policy = &p
	}
	return ttl, policy, nil
}

// assumeRole exchanges the caller's long-lived credentials for a session, honoring
// an optional DurationSeconds and an optional inline Policy that can only narrow.
func (s *Server) assumeRole(w http.ResponseWriter, r *http.Request, requestID string, vr *sign.VerifiedRequest) {
	ttl, policy, perr := sessionParams(r)
	if perr != nil {
		writeSTSError(w, requestID, *perr)
		return
	}

	sess, err := s.sts.AssumeRole(vr.AccessKey, policy, ttl)
	if err != nil {
		// The signature already verified, so the only expected failure is that the
		// caller is not an assumable identity (a service account or another session
		// trying to chain). Anything else is internal.
		if errors.Is(err, auth.ErrNotFound) {
			writeSTSError(w, requestID, stsError{http.StatusForbidden, "AccessDenied", "the caller is not permitted to assume a role"})
			return
		}
		writeSTSError(w, requestID, stsError{http.StatusInternalServerError, "InternalFailure", "could not issue session credentials"})
		return
	}

	// AWS echoes an assumed-role identity; liteio synthesizes one from the session
	// name (when given) and the assuming principal, since it does not model roles
	// as separate resources. Clients read Credentials, not this.
	name := r.Form.Get("RoleSessionName")
	if name == "" {
		name = vr.AccessKey
	}
	resp := assumeRoleResponse{
		Result: assumeRoleResult{
			Credentials: stsCredentials{
				AccessKeyID:     sess.AccessKey,
				SecretAccessKey: sess.SecretKey,
				SessionToken:    sess.SessionToken,
				Expiration:      sess.Expiration.UTC().Format(time.RFC3339),
			},
			AssumedRoleUser: assumedRoleUser{
				Arn:           "arn:aws:sts:::assumed-role/" + vr.AccessKey + "/" + name,
				AssumedRoleID: sess.AccessKey,
			},
		},
		Metadata: stsResponseMetadata{RequestID: requestID},
	}
	writeSTSXML(w, requestID, http.StatusOK, resp)
}

// assumeRoleWithWebIdentity exchanges an external OIDC identity token for a session.
// It is the unsigned federated flow: the WebIdentityToken is the credential, so no
// SigV4 caller is required. The store verifies the token against the provider its
// issuer names and maps the configured policy claim to the session's permissions.
func (s *Server) assumeRoleWithWebIdentity(w http.ResponseWriter, r *http.Request, requestID string) {
	token := r.Form.Get("WebIdentityToken")
	if token == "" {
		writeSTSError(w, requestID, stsError{http.StatusBadRequest, "ValidationError", "WebIdentityToken is required"})
		return
	}
	ttl, policy, perr := sessionParams(r)
	if perr != nil {
		writeSTSError(w, requestID, *perr)
		return
	}

	sess, subject, err := s.sts.AssumeRoleWithWebIdentity(token, policy, ttl)
	if err != nil {
		writeSTSError(w, requestID, webIdentityError(err))
		return
	}

	name := r.Form.Get("RoleSessionName")
	if name == "" {
		name = subject
	}
	resp := assumeRoleWithWebIdentityResponse{
		Result: assumeRoleWithWebIdentityResult{
			Credentials: stsCredentials{
				AccessKeyID:     sess.AccessKey,
				SecretAccessKey: sess.SecretKey,
				SessionToken:    sess.SessionToken,
				Expiration:      sess.Expiration.UTC().Format(time.RFC3339),
			},
			SubjectFromWebIdentityToken: subject,
			AssumedRoleUser: assumedRoleUser{
				Arn:           "arn:aws:sts:::assumed-role/" + subject + "/" + name,
				AssumedRoleID: sess.AccessKey,
			},
		},
		Metadata: stsResponseMetadata{RequestID: requestID},
	}
	writeSTSXML(w, requestID, http.StatusOK, resp)
}

// assumeRoleWithCertificate exchanges a client certificate for a session. Like the
// web-identity flow it is unsigned: the certificate the client presented on the TLS
// connection is the credential. The store verifies the chain against its configured
// trust roots and maps the certificate's subject to the session's permissions. The
// TLS listener only needs to request a client certificate; liteio does the trust
// decision here against its own roots, so it need not be the listener's client CA.
func (s *Server) assumeRoleWithCertificate(w http.ResponseWriter, r *http.Request, requestID string) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		writeSTSError(w, requestID, stsError{http.StatusBadRequest, "ValidationError", "a client certificate is required"})
		return
	}
	ttl, policy, perr := sessionParams(r)
	if perr != nil {
		writeSTSError(w, requestID, *perr)
		return
	}

	sess, subject, err := s.sts.AssumeRoleWithCertificate(r.TLS.PeerCertificates, policy, ttl)
	if err != nil {
		writeSTSError(w, requestID, certificateError(err))
		return
	}

	name := r.Form.Get("RoleSessionName")
	if name == "" {
		name = subject
	}
	resp := assumeRoleWithCertificateResponse{
		Result: assumeRoleWithCertificateResult{
			Credentials: stsCredentials{
				AccessKeyID:     sess.AccessKey,
				SecretAccessKey: sess.SecretKey,
				SessionToken:    sess.SessionToken,
				Expiration:      sess.Expiration.UTC().Format(time.RFC3339),
			},
			AssumedRoleUser: assumedRoleUser{
				Arn:           "arn:aws:sts:::assumed-role/" + subject + "/" + name,
				AssumedRoleID: sess.AccessKey,
			},
		},
		Metadata: stsResponseMetadata{RequestID: requestID},
	}
	writeSTSXML(w, requestID, http.StatusOK, resp)
}

// assumeRoleWithLDAPIdentity exchanges a directory username and password for a
// session. Like the other federated flows it is unsigned: the directory credential
// is the credential. The store binds to the directory, verifies the credential, and
// maps the user's groups to the session's permissions. The form carries the
// credential the way the AWS and MinIO LDAP flows do, as LDAPUsername/LDAPPassword.
func (s *Server) assumeRoleWithLDAPIdentity(w http.ResponseWriter, r *http.Request, requestID string) {
	username := r.Form.Get("LDAPUsername")
	password := r.Form.Get("LDAPPassword")
	if username == "" || password == "" {
		writeSTSError(w, requestID, stsError{http.StatusBadRequest, "ValidationError", "LDAPUsername and LDAPPassword are required"})
		return
	}
	ttl, policy, perr := sessionParams(r)
	if perr != nil {
		writeSTSError(w, requestID, *perr)
		return
	}

	sess, subject, err := s.sts.AssumeRoleWithLDAPIdentity(username, password, policy, ttl)
	if err != nil {
		writeSTSError(w, requestID, ldapError(err))
		return
	}

	name := r.Form.Get("RoleSessionName")
	if name == "" {
		name = subject
	}
	resp := assumeRoleWithLDAPIdentityResponse{
		Result: assumeRoleWithLDAPIdentityResult{
			Credentials: stsCredentials{
				AccessKeyID:     sess.AccessKey,
				SecretAccessKey: sess.SecretKey,
				SessionToken:    sess.SessionToken,
				Expiration:      sess.Expiration.UTC().Format(time.RFC3339),
			},
			AssumedRoleUser: assumedRoleUser{
				Arn:           "arn:aws:sts:::assumed-role/" + subject + "/" + name,
				AssumedRoleID: sess.AccessKey,
			},
		},
		Metadata: stsResponseMetadata{RequestID: requestID},
	}
	writeSTSXML(w, requestID, http.StatusOK, resp)
}

// ldapError maps a directory-exchange failure to its STS wire error. A directory
// liteio cannot reach is a server-side fault (IDPCommunicationError, 500); a rejected
// or unmapped credential is the caller's fault and not a usable identity, so it is
// AccessDenied (403).
func ldapError(err error) stsError {
	switch {
	case errors.Is(err, auth.ErrIDPCommunication):
		return stsError{http.StatusInternalServerError, "IDPCommunicationError", "could not reach the directory"}
	case errors.Is(err, auth.ErrInvalidToken):
		return stsError{http.StatusForbidden, "AccessDenied", "the directory credential is not accepted"}
	default:
		return stsError{http.StatusInternalServerError, "InternalFailure", "could not issue session credentials"}
	}
}

// certificateError maps a certificate-exchange failure to its STS wire error. A
// certificate that is untrusted, expired, or grants no policy is the caller's fault
// and not a usable identity, so it is AccessDenied rather than a malformed-input
// error.
func certificateError(err error) stsError {
	switch {
	case errors.Is(err, auth.ErrInvalidToken):
		return stsError{http.StatusForbidden, "AccessDenied", "the client certificate is not accepted"}
	default:
		return stsError{http.StatusInternalServerError, "InternalFailure", "could not issue session credentials"}
	}
}

// webIdentityError maps a token-exchange failure to its STS wire error, mirroring
// the codes AWS uses so SDK error handling behaves the same.
func webIdentityError(err error) stsError {
	switch {
	case errors.Is(err, auth.ErrExpired):
		return stsError{http.StatusBadRequest, "ExpiredToken", "the web identity token has expired"}
	case errors.Is(err, auth.ErrIDPCommunication):
		return stsError{http.StatusInternalServerError, "IDPCommunicationError", "could not reach the identity provider"}
	case errors.Is(err, auth.ErrInvalidToken):
		return stsError{http.StatusBadRequest, "InvalidIdentityToken", "the web identity token is not valid"}
	default:
		return stsError{http.StatusInternalServerError, "InternalFailure", "could not issue session credentials"}
	}
}

// --- STS response and error envelopes ---

type assumeRoleResponse struct {
	XMLName  xml.Name            `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleResponse"`
	Result   assumeRoleResult    `xml:"AssumeRoleResult"`
	Metadata stsResponseMetadata `xml:"ResponseMetadata"`
}

type assumeRoleResult struct {
	Credentials     stsCredentials  `xml:"Credentials"`
	AssumedRoleUser assumedRoleUser `xml:"AssumedRoleUser"`
}

type assumeRoleWithWebIdentityResponse struct {
	XMLName  xml.Name                        `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleWithWebIdentityResponse"`
	Result   assumeRoleWithWebIdentityResult `xml:"AssumeRoleWithWebIdentityResult"`
	Metadata stsResponseMetadata             `xml:"ResponseMetadata"`
}

type assumeRoleWithWebIdentityResult struct {
	Credentials                 stsCredentials  `xml:"Credentials"`
	SubjectFromWebIdentityToken string          `xml:"SubjectFromWebIdentityToken"`
	AssumedRoleUser             assumedRoleUser `xml:"AssumedRoleUser"`
}

type assumeRoleWithCertificateResponse struct {
	XMLName  xml.Name                        `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleWithCertificateResponse"`
	Result   assumeRoleWithCertificateResult `xml:"AssumeRoleWithCertificateResult"`
	Metadata stsResponseMetadata             `xml:"ResponseMetadata"`
}

type assumeRoleWithCertificateResult struct {
	Credentials     stsCredentials  `xml:"Credentials"`
	AssumedRoleUser assumedRoleUser `xml:"AssumedRoleUser"`
}

type assumeRoleWithLDAPIdentityResponse struct {
	XMLName  xml.Name                         `xml:"https://sts.amazonaws.com/doc/2011-06-15/ AssumeRoleWithLDAPIdentityResponse"`
	Result   assumeRoleWithLDAPIdentityResult `xml:"AssumeRoleWithLDAPIdentityResult"`
	Metadata stsResponseMetadata              `xml:"ResponseMetadata"`
}

type assumeRoleWithLDAPIdentityResult struct {
	Credentials     stsCredentials  `xml:"Credentials"`
	AssumedRoleUser assumedRoleUser `xml:"AssumedRoleUser"`
}

type stsCredentials struct {
	AccessKeyID     string `xml:"AccessKeyId"`
	SecretAccessKey string `xml:"SecretAccessKey"`
	SessionToken    string `xml:"SessionToken"`
	Expiration      string `xml:"Expiration"`
}

type assumedRoleUser struct {
	Arn           string `xml:"Arn"`
	AssumedRoleID string `xml:"AssumedRoleId"`
}

type stsResponseMetadata struct {
	RequestID string `xml:"RequestId"`
}

// stsError is the STS analog of APIError: the wire Code and the HTTP status. STS
// uses a different envelope and namespace from the S3 error catalog, so it does
// not reuse writeError.
type stsError struct {
	status  int
	code    string
	message string
}

var errSTSNotImplemented = stsError{http.StatusNotImplemented, "NotImplemented", "this STS action is not implemented"}

type stsErrorResponse struct {
	XMLName   xml.Name `xml:"https://sts.amazonaws.com/doc/2011-06-15/ ErrorResponse"`
	Type      string   `xml:"Error>Type"`
	Code      string   `xml:"Error>Code"`
	Message   string   `xml:"Error>Message"`
	RequestID string   `xml:"RequestId"`
}

// writeSTSXML marshals an STS success body with the declaration prepended.
func writeSTSXML(w http.ResponseWriter, requestID string, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		writeSTSError(w, requestID, stsError{http.StatusInternalServerError, "InternalFailure", "could not encode the response"})
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// writeSTSError renders the STS error envelope. Every STS error is a Sender fault
// here (the caller's request was at fault) except the 5xx ones.
func writeSTSError(w http.ResponseWriter, requestID string, e stsError) {
	faultType := "Sender"
	if e.status >= http.StatusInternalServerError {
		faultType = "Receiver"
	}
	body, err := xml.Marshal(stsErrorResponse{Type: faultType, Code: e.code, Message: e.message, RequestID: requestID})
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(e.status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
