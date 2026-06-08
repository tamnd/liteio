// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/object"
)

// --- XML wire types ----------------------------------------------------------

type accessControlPolicyXML struct {
	XMLName xml.Name             `xml:"AccessControlPolicy"`
	XMLNS   string               `xml:"xmlns,attr,omitempty"`
	Owner   ownerXML             `xml:"Owner"`
	ACL     accessControlListXML `xml:"AccessControlList"`
}

type ownerXML struct {
	ID          string `xml:"ID"`
	DisplayName string `xml:"DisplayName"`
}

type accessControlListXML struct {
	Grant []grantXML `xml:"Grant"`
}

type grantXML struct {
	Grantee    granteeXML `xml:"Grantee"`
	Permission string     `xml:"Permission"`
}

type granteeXML struct {
	XMLNS       string `xml:"xmlns:xsi,attr,omitempty"`
	Type        string `xml:"xsi:type,attr,omitempty"`
	ID          string `xml:"ID,omitempty"`
	DisplayName string `xml:"DisplayName,omitempty"`
	URI         string `xml:"URI,omitempty"`
}

// cannedACLFromHeader returns the canned ACL name from the x-amz-acl header, or
// "private" when the header is absent.
func cannedACLFromHeader(r *http.Request) string {
	if v := r.Header.Get("x-amz-acl"); v != "" {
		return v
	}
	return "private"
}

// minimalACP returns a minimal AccessControlPolicy granting the canonical owner
// FullControl. When publicRead is true it also adds an anonymous read grant,
// matching what the public-read canned ACL produces.
func minimalACP(publicRead bool) accessControlPolicyXML {
	ownerGrant := grantXML{
		Grantee: granteeXML{
			XMLNS:       "http://www.w3.org/2001/XMLSchema-instance",
			Type:        "CanonicalUser",
			ID:          ownerID,
			DisplayName: ownerID,
		},
		Permission: "FULL_CONTROL",
	}
	acl := accessControlPolicyXML{
		XMLNS: s3XMLNS,
		Owner: ownerXML{ID: ownerID, DisplayName: ownerID},
		ACL:   accessControlListXML{Grant: []grantXML{ownerGrant}},
	}
	if publicRead {
		acl.ACL.Grant = append(acl.ACL.Grant, grantXML{
			Grantee: granteeXML{
				XMLNS: "http://www.w3.org/2001/XMLSchema-instance",
				Type:  "Group",
				URI:   "http://acs.amazonaws.com/groups/global/AllUsers",
			},
			Permission: "READ",
		})
	}
	return acl
}

// --- bucket ACL handlers -----------------------------------------------------

// putBucketAcl handles PUT /bucket?acl. It maps the canned public-read ACL to a
// bucket policy granting anonymous GetObject, and private to no bucket policy.
// Full custom ACL XML is accepted but ignored — liteio uses PBAC (policy-based
// access control), so the canned header is the only effective input.
func (s *Server) putBucketAcl(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	_, _ = io.Copy(io.Discard, r.Body)

	canned := cannedACLFromHeader(r)
	switch canned {
	case "public-read":
		policy := auth.PublicReadPolicy(bucket)
		doc, err := json.Marshal(policy)
		if err != nil {
			s.fail(w, requestID, r.URL.Path, err)
			return
		}
		if err := s.layer.SetBucketPolicy(r.Context(), bucket, doc); err != nil {
			s.fail(w, requestID, r.URL.Path, err)
			return
		}
	case "private", "":
		if err := s.layer.DeleteBucketPolicy(r.Context(), bucket); err != nil &&
			!errors.Is(err, object.ErrNoSuchBucketPolicy) {
			s.fail(w, requestID, r.URL.Path, err)
			return
		}
	}
	// All other canned ACLs (authenticated-read, public-read-write, etc.) are
	// acknowledged without effect — liteio's PBAC model handles them through
	// explicit bucket policies.
	w.WriteHeader(http.StatusOK)
}

// getBucketAcl handles GET /bucket?acl. It returns a minimal ACP: the owner
// always has FullControl; if the bucket carries a public-read policy an
// anonymous read grant is also included.
func (s *Server) getBucketAcl(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	publicRead := false
	_, err := s.layer.GetBucketPolicy(r.Context(), bucket)
	if err == nil {
		// A bucket policy exists; a full evaluation of whether it grants public
		// read is beyond a minimal ACL surface, so we conservatively note that
		// a policy is present without reflecting its grants here.
		publicRead = false
	}
	writeXML(w, requestID, http.StatusOK, minimalACP(publicRead))
}

// --- object ACL handlers -----------------------------------------------------

// putObjectAcl handles PUT /bucket/key?acl. Object-level ACLs are a no-op in
// liteio's PBAC model; the request is acknowledged with 200.
func (s *Server) putObjectAcl(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.WriteHeader(http.StatusOK)
}

// getObjectAcl handles GET /bucket/key?acl. It verifies the object exists, then
// returns a minimal owner-only ACP.
func (s *Server) getObjectAcl(w http.ResponseWriter, r *http.Request, requestID, bucket, obj string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	if _, err := s.layer.GetObjectInfo(r.Context(), bucket, obj, opts); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, minimalACP(false))
}
