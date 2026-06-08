// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/tamnd/liteio/auth"
)

// The IAM handlers are thin translations of HTTP onto the IAM store: decode the
// path and body, call the store, map the error or render the result. Authentication
// and authorization happen in ServeHTTP before any of these run.

// --- Users ---

// userInfo is the JSON view of a user: never the secret, only what it can do.
type userInfo struct {
	AccessKey string   `json:"accessKey"`
	Policies  []string `json:"policies,omitempty"`
	Groups    []string `json:"groups,omitempty"`
}

// createUserRequest is the body of PUT /users/{key}.
type createUserRequest struct {
	SecretKey string   `json:"secretKey"`
	Policies  []string `json:"policies,omitempty"`
	Groups    []string `json:"groups,omitempty"`
}

func (s *Server) listUsers(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"users": s.iam.Users()})
}

func (s *Server) putUser(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var req createUserRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.SecretKey == "" {
		writeError(w, http.StatusBadRequest, "MalformedRequest", "secretKey is required")
		return
	}
	if err := s.iam.AddUser(auth.User{
		AccessKey: key,
		SecretKey: req.SecretKey,
		Policies:  req.Policies,
		Groups:    req.Groups,
	}); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getUser(w http.ResponseWriter, r *http.Request) {
	u, ok := s.iam.User(r.PathValue("key"))
	if !ok {
		writeError(w, http.StatusNotFound, "NotFound", "no such user")
		return
	}
	writeJSON(w, http.StatusOK, userInfo{AccessKey: u.AccessKey, Policies: u.Policies, Groups: u.Groups})
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.DeleteUser(r.PathValue("key")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) attachUserPolicy(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.AttachUserPolicy(r.PathValue("key"), r.PathValue("policy")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) detachUserPolicy(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.DetachUserPolicy(r.PathValue("key"), r.PathValue("policy")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Groups ---

type groupInfo struct {
	Name     string   `json:"name"`
	Policies []string `json:"policies,omitempty"`
}

type createGroupRequest struct {
	Policies []string `json:"policies,omitempty"`
}

func (s *Server) listGroups(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"groups": s.iam.Groups()})
}

func (s *Server) putGroup(w http.ResponseWriter, r *http.Request) {
	var req createGroupRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if err := s.iam.AddGroup(auth.Group{Name: r.PathValue("name"), Policies: req.Policies}); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getGroup(w http.ResponseWriter, r *http.Request) {
	g, ok := s.iam.Group(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "NotFound", "no such group")
		return
	}
	writeJSON(w, http.StatusOK, groupInfo{Name: g.Name, Policies: g.Policies})
}

func (s *Server) deleteGroup(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.DeleteGroup(r.PathValue("name")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) addGroupMember(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.AddUserToGroup(r.PathValue("key"), r.PathValue("name")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) removeGroupMember(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.RemoveUserFromGroup(r.PathValue("key"), r.PathValue("name")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Policies ---

func (s *Server) listPolicies(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string][]string{"policies": s.iam.PolicyNames()})
}

// putPolicy creates or replaces a custom policy. The body is the IAM policy
// document itself (not an envelope), parsed and validated by auth.ParsePolicy.
func (s *Server) putPolicy(w http.ResponseWriter, r *http.Request) {
	const maxPolicy = 20 * 1024 // match the S3 bucket-policy cap
	body := http.MaxBytesReader(w, r.Body, maxPolicy)
	doc, err := io.ReadAll(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "MalformedPolicy", "policy document too large or unreadable")
		return
	}
	p, err := auth.ParsePolicy(doc)
	if err != nil {
		writeError(w, http.StatusBadRequest, "MalformedPolicy", err.Error())
		return
	}
	if err := s.iam.SetPolicy(r.PathValue("name"), p); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// getPolicy returns the named policy document (canned or custom) verbatim as JSON.
func (s *Server) getPolicy(w http.ResponseWriter, r *http.Request) {
	p, ok := s.iam.GetPolicy(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "NotFound", "no such policy")
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *Server) deletePolicy(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.DeletePolicy(r.PathValue("name")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Service accounts ---

// createServiceAccountRequest is the body of PUT /service-accounts. An inline
// policy, when present, may only narrow the parent's rights (enforced by the
// evaluator); omitting it gives the service account the parent's full rights.
type createServiceAccountRequest struct {
	AccessKey  string          `json:"accessKey"`
	SecretKey  string          `json:"secretKey"`
	ParentUser string          `json:"parentUser"`
	Policy     json.RawMessage `json:"policy,omitempty"`
}

func (s *Server) listServiceAccounts(w http.ResponseWriter, r *http.Request) {
	parent := r.URL.Query().Get("user")
	if parent == "" {
		writeError(w, http.StatusBadRequest, "MalformedRequest", "the user query parameter is required")
		return
	}
	keys, err := s.iam.ServiceAccountsFor(parent)
	if err != nil {
		writeIAMError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string][]string{"serviceAccounts": keys})
}

func (s *Server) putServiceAccount(w http.ResponseWriter, r *http.Request) {
	var req createServiceAccountRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.AccessKey == "" || req.SecretKey == "" || req.ParentUser == "" {
		writeError(w, http.StatusBadRequest, "MalformedRequest", "accessKey, secretKey and parentUser are required")
		return
	}
	sa := auth.ServiceAccount{AccessKey: req.AccessKey, SecretKey: req.SecretKey, ParentUser: req.ParentUser}
	if len(req.Policy) > 0 {
		p, err := auth.ParsePolicy(req.Policy)
		if err != nil {
			writeError(w, http.StatusBadRequest, "MalformedPolicy", err.Error())
			return
		}
		sa.Inline = &p
	}
	if err := s.iam.AddServiceAccount(sa); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteServiceAccount(w http.ResponseWriter, r *http.Request) {
	if err := s.iam.DeleteServiceAccount(r.PathValue("key")); err != nil {
		writeIAMError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
