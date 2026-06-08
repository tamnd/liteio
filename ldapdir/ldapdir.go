// SPDX-License-Identifier: Apache-2.0

// Package ldapdir is the production LDAP directory behind the auth package's
// Directory seam (spec 2020, doc 08.4). It binds to an LDAP server, verifies a
// user's credential, and resolves the groups the user belongs to, so the federated
// AssumeRoleWithLDAPIdentity flow can map those groups to liteio policies.
//
// The auth package itself is standard-library only; the LDAP wire protocol (RFC
// 4511, BER over TCP) is not, so it is confined here behind a small connection
// interface. The flow logic and all of liteio's authorization stay testable without
// a live directory: auth tests use a fake Directory, and this package's tests use a
// fake connection, so the only code that ever talks to a real server is the thin
// adapter at the bottom of this file.
package ldapdir

import (
	"fmt"
	"strings"

	"github.com/go-ldap/ldap/v3"
	"github.com/tamnd/liteio/auth"
)

// Config describes how to reach a directory and find users and their groups. It
// follows the lookup-then-bind pattern: a service account binds first to search for
// the user's entry by username, the user's own credential is then verified by
// binding as that entry, and finally the user's groups are searched. This avoids
// trusting a username straight into a bind DN and works with directories whose user
// entries are not under a predictable DN.
type Config struct {
	// URL is the directory server, e.g. "ldap://dir.example.com:389" or
	// "ldaps://dir.example.com:636". Required.
	URL string

	// LookupDN and LookupPassword are the service account used to search for user
	// and group entries. Both required: liteio never searches over an anonymous bind.
	LookupDN       string
	LookupPassword string

	// UserBaseDN is the subtree searched for a user's entry. UserFilter is its
	// filter with a single %s placeholder for the (escaped) username, e.g.
	// "(uid=%s)" or "(sAMAccountName=%s)". Both required.
	UserBaseDN string
	UserFilter string

	// GroupBaseDN is the subtree searched for a user's groups. GroupFilter is its
	// filter with a single %s placeholder for the (escaped) user DN, e.g.
	// "(&(objectClass=groupOfNames)(member=%s))". Both required.
	GroupBaseDN string
	GroupFilter string

	// GroupNameAttr is the attribute on a group entry read as the group name that
	// maps to a liteio policy. Defaults to "cn".
	GroupNameAttr string
}

const defaultGroupNameAttr = "cn"

// conn is the slice of an LDAP connection this package uses: bind a credential,
// run a search, and close. *ldap.Conn satisfies it through the adapter at the
// bottom of the file, and tests substitute a fake so the bind-and-search logic runs
// without a server.
type conn interface {
	Bind(username, password string) error
	Search(req *ldap.SearchRequest) (*ldap.SearchResult, error)
	Close() error
}

// Directory is the production auth.Directory: it dials an LDAP server per request
// and resolves credentials and group memberships against it.
type Directory struct {
	cfg  Config
	dial func() (conn, error) // overridable in tests; defaults to a real dial
}

// Compile-time check that Directory satisfies the auth seam.
var _ auth.Directory = (*Directory)(nil)

// New builds a Directory from a validated config. It does no network I/O; each
// Authenticate call opens and closes its own connection, which keeps the directory
// stateless and free of idle-connection management for an endpoint that is hit only
// at session-exchange time.
func New(cfg Config) (*Directory, error) {
	switch {
	case cfg.URL == "":
		return nil, fmt.Errorf("ldapdir: URL is required")
	case cfg.LookupDN == "" || cfg.LookupPassword == "":
		return nil, fmt.Errorf("ldapdir: lookup DN and password are required")
	case cfg.UserBaseDN == "" || cfg.UserFilter == "":
		return nil, fmt.Errorf("ldapdir: user base DN and filter are required")
	case cfg.GroupBaseDN == "" || cfg.GroupFilter == "":
		return nil, fmt.Errorf("ldapdir: group base DN and filter are required")
	case !strings.Contains(cfg.UserFilter, "%s"):
		return nil, fmt.Errorf("ldapdir: user filter must contain a %%s placeholder")
	case !strings.Contains(cfg.GroupFilter, "%s"):
		return nil, fmt.Errorf("ldapdir: group filter must contain a %%s placeholder")
	}
	if cfg.GroupNameAttr == "" {
		cfg.GroupNameAttr = defaultGroupNameAttr
	}
	d := &Directory{cfg: cfg}
	d.dial = d.dialReal
	return d, nil
}

// Authenticate implements auth.Directory: it verifies the username and password
// against the directory and returns the user's DN and group names. A rejected
// credential is auth.ErrInvalidToken; an unreachable or unparseable directory is
// auth.ErrIDPCommunication, matching the contract the federated flow maps to STS
// wire errors.
func (d *Directory) Authenticate(username, password string) (string, []string, error) {
	c, err := d.dial()
	if err != nil {
		return "", nil, fmt.Errorf("%w: dial directory: %v", auth.ErrIDPCommunication, err)
	}
	defer func() { _ = c.Close() }()

	// Bind as the lookup service account, then find the user's entry by username.
	if err := c.Bind(d.cfg.LookupDN, d.cfg.LookupPassword); err != nil {
		return "", nil, fmt.Errorf("%w: lookup bind: %v", auth.ErrIDPCommunication, err)
	}
	userDN, err := d.findUserDN(c, username)
	if err != nil {
		return "", nil, err
	}

	// Verify the user's own credential by binding as the found entry. A wrong
	// password is the directory rejecting the bind, which is ErrInvalidToken.
	if err := c.Bind(userDN, password); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultInvalidCredentials) {
			return "", nil, fmt.Errorf("%w: directory rejected the credential", auth.ErrInvalidToken)
		}
		return "", nil, fmt.Errorf("%w: user bind: %v", auth.ErrIDPCommunication, err)
	}

	// Re-bind as the lookup account to search for groups: the user entry may not
	// have permission to read the group subtree, but the service account does.
	if err := c.Bind(d.cfg.LookupDN, d.cfg.LookupPassword); err != nil {
		return "", nil, fmt.Errorf("%w: rebind for group search: %v", auth.ErrIDPCommunication, err)
	}
	groups, err := d.findGroups(c, userDN)
	if err != nil {
		return "", nil, err
	}
	return userDN, groups, nil
}

// findUserDN searches the user subtree for exactly one entry matching the username
// and returns its DN. Zero matches is a credential that names no user
// (ErrInvalidToken); more than one is an ambiguous filter the operator must fix
// (ErrIDPCommunication), never a silent pick of the first.
func (d *Directory) findUserDN(c conn, username string) (string, error) {
	filter := fmt.Sprintf(d.cfg.UserFilter, ldap.EscapeFilter(username))
	req := ldap.NewSearchRequest(
		d.cfg.UserBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		2, 0, false, // size limit 2: enough to detect an ambiguous match
		filter, []string{"dn"}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return "", fmt.Errorf("%w: user search: %v", auth.ErrIDPCommunication, err)
	}
	switch len(res.Entries) {
	case 0:
		return "", fmt.Errorf("%w: no directory user matches %q", auth.ErrInvalidToken, username)
	case 1:
		return res.Entries[0].DN, nil
	default:
		return "", fmt.Errorf("%w: user filter matched %d entries for %q", auth.ErrIDPCommunication, len(res.Entries), username)
	}
}

// findGroups searches the group subtree for the user DN's memberships and returns
// the configured name attribute of each. No groups is not an error here; the flow
// decides whether the resulting (possibly empty) policy set is usable.
func (d *Directory) findGroups(c conn, userDN string) ([]string, error) {
	filter := fmt.Sprintf(d.cfg.GroupFilter, ldap.EscapeFilter(userDN))
	req := ldap.NewSearchRequest(
		d.cfg.GroupBaseDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
		0, 0, false,
		filter, []string{d.cfg.GroupNameAttr}, nil,
	)
	res, err := c.Search(req)
	if err != nil {
		return nil, fmt.Errorf("%w: group search: %v", auth.ErrIDPCommunication, err)
	}
	var groups []string
	for _, e := range res.Entries {
		for _, name := range e.GetAttributeValues(d.cfg.GroupNameAttr) {
			if name = strings.TrimSpace(name); name != "" {
				groups = append(groups, name)
			}
		}
	}
	return groups, nil
}

// dialReal opens a real LDAP connection and wraps it as a conn. It is the only code
// in this package that touches the network; everything above is exercised in tests
// against a fake conn.
func (d *Directory) dialReal() (conn, error) {
	l, err := ldap.DialURL(d.cfg.URL)
	if err != nil {
		return nil, err
	}
	return l, nil
}

// *ldap.Conn already has Bind, Search, and Close with the signatures conn needs, so
// it satisfies the interface directly; this assertion pins that to the build.
var _ conn = (*ldap.Conn)(nil)
