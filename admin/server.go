// SPDX-License-Identifier: Apache-2.0

// Package admin is liteio's signed admin REST API (spec 2020, doc 10.2). It
// surfaces the IAM authority — users, groups, policies, and service accounts —
// over HTTP for the console and the CLI, so automation has parity with the UI.
// Every request is SigV4-authenticated (the same scheme as the S3 front door) and
// authorized against the policy engine under an admin: action, so only an identity
// granted admin rights (the consoleAdmin canned policy, doc 08) can manage IAM.
//
// This is the IAM slice of the admin surface; info/health, config, heal, topology,
// and perf (doc 10.2) are separate subsystems that land with the machinery they
// drive.
package admin

import (
	"net"
	"net/http"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
)

// apiPrefix is the version-pinned root every admin route hangs under. APIPrefix
// exports it so an in-process caller (the web console's signed bridge, doc 10.1)
// can target the same routes without hardcoding the string.
const apiPrefix = "/liteio/admin/v1"

// APIPrefix is the version-pinned root every admin route hangs under.
const APIPrefix = apiPrefix

// adminResource is the ARN admin actions authorize against; it matches the
// Resource of the consoleAdmin canned policy (doc 08), so that policy grants the
// whole admin surface.
const adminResource = "arn:aws:s3:::*"

// IAM is the slice of the identity store the admin handlers drive. auth.Store
// satisfies it; the interface keeps the dependency explicit and the handlers
// testable against a fake.
type IAM interface {
	IsAllowed(accessKey string, req auth.Request) (bool, error)

	Users() []string
	User(accessKey string) (auth.User, bool)
	AddUser(u auth.User) error
	DeleteUser(accessKey string) error

	Groups() []string
	Group(name string) (auth.Group, bool)
	AddGroup(g auth.Group) error
	DeleteGroup(name string) error
	AddUserToGroup(accessKey, group string) error
	RemoveUserFromGroup(accessKey, group string) error
	GroupMembers(group string) ([]string, error)

	PolicyNames() []string
	GetPolicy(name string) (auth.Policy, bool)
	SetPolicy(name string, p auth.Policy) error
	DeletePolicy(name string) error
	AttachUserPolicy(accessKey, policy string) error
	DetachUserPolicy(accessKey, policy string) error

	AddServiceAccount(sa auth.ServiceAccount) error
	DeleteServiceAccount(accessKey string) error
	ServiceAccountsFor(parentKey string) ([]string, error)
}

// auth.Store is the production IAM implementation behind the admin API.
var _ IAM = (*auth.Store)(nil)

// Server is the admin REST API HTTP handler. It authenticates and authorizes every
// request, then dispatches to the IAM handlers.
type Server struct {
	iam        IAM
	creds      auth.CredentialStore // SigV4 secret lookup
	info       InfoSource           // deployment topology for info/health (nil omits those routes)
	tiers      TierLayer            // tier config management (nil omits those routes)
	rebalancer RebalanceLayer       // rebalance/decommission (nil omits those routes)
	version    string               // build version reported by the info endpoint
	now        func() time.Time     // clock seam for signature skew (tests inject)
	mux        *http.ServeMux
	actions    map[string]string // ServeMux pattern -> required admin action
}

// Option configures a Server.
type Option func(*Server)

// WithClock overrides the clock used for signature skew checks (for tests).
func WithClock(now func() time.Time) Option { return func(s *Server) { s.now = now } }

// WithInfo wires the deployment topology source the info and health endpoints
// report (doc 10.2). Without it those routes are not registered, so a node with no
// object layer (a pure IAM control node) simply does not serve them.
func WithInfo(src InfoSource) Option { return func(s *Server) { s.info = src } }

// WithVersion sets the build version string the info endpoint reports.
func WithVersion(v string) Option { return func(s *Server) { s.version = v } }

// NewServer builds the admin API over an IAM store and the credential store the
// signature verifier looks secrets up in.
func NewServer(iam IAM, creds auth.CredentialStore, opts ...Option) *Server {
	s := &Server{iam: iam, creds: creds, version: "dev", now: time.Now}
	for _, o := range opts {
		o(s)
	}
	s.mux = s.routes()
	return s
}

// route binds a method+path pattern to the admin action it requires and the
// handler that serves it.
type route struct {
	pattern string // ServeMux "METHOD /path" pattern
	action  string // admin: action authorized before dispatch
	handler http.HandlerFunc
}

// routes registers every IAM route and records the action each requires, keyed by
// the ServeMux pattern so the authorization step can look it up after matching.
func (s *Server) routes() *http.ServeMux {
	rs := []route{
		{"GET " + apiPrefix + "/users", "admin:ListUsers", s.listUsers},
		{"PUT " + apiPrefix + "/users/{key}", "admin:CreateUser", s.putUser},
		{"GET " + apiPrefix + "/users/{key}", "admin:GetUser", s.getUser},
		{"DELETE " + apiPrefix + "/users/{key}", "admin:DeleteUser", s.deleteUser},
		{"PUT " + apiPrefix + "/users/{key}/policies/{policy}", "admin:AttachUserOrGroupPolicy", s.attachUserPolicy},
		{"DELETE " + apiPrefix + "/users/{key}/policies/{policy}", "admin:DetachUserOrGroupPolicy", s.detachUserPolicy},

		{"GET " + apiPrefix + "/groups", "admin:ListGroups", s.listGroups},
		{"PUT " + apiPrefix + "/groups/{name}", "admin:AddUserToGroup", s.putGroup},
		{"GET " + apiPrefix + "/groups/{name}", "admin:GetGroup", s.getGroup},
		{"DELETE " + apiPrefix + "/groups/{name}", "admin:RemoveUserFromGroup", s.deleteGroup},
		{"PUT " + apiPrefix + "/groups/{name}/members/{key}", "admin:AddUserToGroup", s.addGroupMember},
		{"DELETE " + apiPrefix + "/groups/{name}/members/{key}", "admin:RemoveUserFromGroup", s.removeGroupMember},

		{"GET " + apiPrefix + "/policies", "admin:ListUserPolicies", s.listPolicies},
		{"PUT " + apiPrefix + "/policies/{name}", "admin:CreatePolicy", s.putPolicy},
		{"GET " + apiPrefix + "/policies/{name}", "admin:GetPolicy", s.getPolicy},
		{"DELETE " + apiPrefix + "/policies/{name}", "admin:DeletePolicy", s.deletePolicy},

		{"GET " + apiPrefix + "/service-accounts", "admin:ListServiceAccounts", s.listServiceAccounts},
		{"PUT " + apiPrefix + "/service-accounts", "admin:CreateServiceAccount", s.putServiceAccount},
		{"DELETE " + apiPrefix + "/service-accounts/{key}", "admin:RemoveServiceAccount", s.deleteServiceAccount},
	}
	// Info and health report on the object layer; register them only when a topology
	// source is wired, so a control node without one does not advertise empty routes.
	if s.info != nil {
		rs = append(rs,
			route{"GET " + apiPrefix + "/info", "admin:ServerInfo", s.serverInfo},
			route{"GET " + apiPrefix + "/health", "admin:HealthInfo", s.health},
		)
	}
	// Tier management routes are registered regardless of whether tiers is wired;
	// the handlers return 501 when s.tiers is nil.
	rs = append(rs,
		route{"GET " + apiPrefix + "/tiers", "admin:ListTiers", s.listTiers},
		route{"GET " + apiPrefix + "/tiers/{name}", "admin:GetTier", s.getTier},
		route{"PUT " + apiPrefix + "/tiers/{name}", "admin:SetTier", s.putTier},
		route{"DELETE " + apiPrefix + "/tiers/{name}", "admin:DeleteTier", s.deleteTier},
	)
	// Rebalance and decommission routes; handlers return 501 when rebalancer is nil.
	rs = append(rs,
		route{"POST " + apiPrefix + "/rebalance", "admin:Rebalance", s.startRebalance},
		route{"GET " + apiPrefix + "/rebalance", "admin:Rebalance", s.getRebalanceStatus},
		route{"DELETE " + apiPrefix + "/rebalance", "admin:Rebalance", s.stopRebalance},
		route{"POST " + apiPrefix + "/decommission", "admin:Decommission", s.startDecommission},
		route{"GET " + apiPrefix + "/decommission", "admin:Decommission", s.getDecommissionStatus},
		route{"DELETE " + apiPrefix + "/decommission", "admin:Decommission", s.stopDecommission},
	)
	mux := http.NewServeMux()
	s.actions = make(map[string]string, len(rs))
	for _, r := range rs {
		mux.HandleFunc(r.pattern, r.handler)
		s.actions[r.pattern] = r.action
	}
	return mux
}

// ServeHTTP authenticates the signature, authorizes the matched route's admin
// action, then dispatches. An unmatched route is 404 before any IAM work.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	vr, serr := sign.Verify(r, s.creds, s.now())
	if serr != nil {
		writeError(w, http.StatusForbidden, serr.Code, serr.Message)
		return
	}
	_, pattern := s.mux.Handler(r)
	action, ok := s.actions[pattern]
	if !ok {
		// No admin route matched (or a CONNECT/OPTIONS the mux rejects): not found.
		writeError(w, http.StatusNotFound, "NotFound", "no such admin resource")
		return
	}
	allowed, err := s.iam.IsAllowed(vr.AccessKey, auth.Request{
		Action:   action,
		Resource: adminResource,
		Context:  requestContext(r, vr.AccessKey),
	})
	if err != nil || !allowed {
		writeError(w, http.StatusForbidden, "AccessDenied", "the request is not authorized for "+action)
		return
	}
	// Dispatch through the mux (not the handler from Handler above) so the matched
	// route's path wildcards are populated on the request for r.PathValue.
	s.mux.ServeHTTP(w, r)
}

// requestContext fills the condition-key context an admin policy may scope on: the
// caller, the transport, the time, and the source IP.
func requestContext(r *http.Request, accessKey string) map[string]string {
	ctx := map[string]string{
		auth.CondUsername:        accessKey,
		auth.CondSecureTransport: boolString(r.TLS != nil),
		auth.CondCurrentTime:     time.Now().UTC().Format(time.RFC3339),
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		ctx[auth.CondSourceIP] = host
	} else if r.RemoteAddr != "" {
		ctx[auth.CondSourceIP] = r.RemoteAddr
	}
	return ctx
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}
