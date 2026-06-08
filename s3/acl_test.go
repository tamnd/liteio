// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"net/http"
	"testing"
)

func TestGetBucketAcl200(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/aclbkt", nil, nil), http.StatusOK)

	res := h.do(http.MethodGet, "/aclbkt?acl", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var acp accessControlPolicyXML
	if err := xml.Unmarshal(res.body, &acp); err != nil {
		t.Fatalf("unmarshal ACP: %v", err)
	}
	if acp.Owner.ID != ownerID {
		t.Errorf("Owner.ID = %q, want %q", acp.Owner.ID, ownerID)
	}
	if len(acp.ACL.Grant) == 0 {
		t.Error("expected at least one grant")
	}
	if acp.ACL.Grant[0].Permission != "FULL_CONTROL" {
		t.Errorf("first grant permission = %q, want FULL_CONTROL", acp.ACL.Grant[0].Permission)
	}
}

func TestPutBucketAclPrivate(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/privbkt", nil, nil), http.StatusOK)

	// Set public-read, then switch back to private.
	mustStatus(t, h.do(http.MethodPut, "/privbkt?acl", nil, map[string]string{"x-amz-acl": "public-read"}), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/privbkt?acl", nil, map[string]string{"x-amz-acl": "private"}), http.StatusOK)

	// GET ACL should succeed and return the owner grant.
	res := h.do(http.MethodGet, "/privbkt?acl", nil, nil)
	mustStatus(t, res, http.StatusOK)
}

func TestGetObjectAcl200(t *testing.T) {
	h := newHarness(t)
	mustStatus(t, h.do(http.MethodPut, "/objaclbkt", nil, nil), http.StatusOK)
	mustStatus(t, h.do(http.MethodPut, "/objaclbkt/myobj", []byte("hello"), nil), http.StatusOK)

	res := h.do(http.MethodGet, "/objaclbkt/myobj?acl", nil, nil)
	mustStatus(t, res, http.StatusOK)
	var acp accessControlPolicyXML
	if err := xml.Unmarshal(res.body, &acp); err != nil {
		t.Fatalf("unmarshal object ACP: %v", err)
	}
	if acp.Owner.ID != ownerID {
		t.Errorf("Owner.ID = %q, want %q", acp.Owner.ID, ownerID)
	}
}
