// SPDX-License-Identifier: Apache-2.0

package ldapdir

import (
	"errors"
	"fmt"
	"testing"

	"github.com/go-ldap/ldap/v3"
	"github.com/tamnd/liteio/auth"
)

// fakeConn is an in-memory LDAP connection: Bind accepts the credentials in accept
// and rejects the rest with an invalid-credentials error, and Search returns the
// canned entries for the user or group subtree by base DN. It records every bind and
// search so a test can assert the bind sequence and the (escaped) filters. This lets
// the whole bind-and-search path run without a server.
type fakeConn struct {
	accept      map[string]string // dn -> password Bind accepts
	userBaseDN  string
	groupBaseDN string
	userResult  []*ldap.Entry
	groupResult []*ldap.Entry
	searchErr   error

	binds    [][2]string
	searches []*ldap.SearchRequest
	closed   bool
}

func (c *fakeConn) Bind(dn, password string) error {
	c.binds = append(c.binds, [2]string{dn, password})
	if want, ok := c.accept[dn]; !ok || want != password {
		return ldap.NewError(ldap.LDAPResultInvalidCredentials, fmt.Errorf("invalid credentials"))
	}
	return nil
}

func (c *fakeConn) Search(req *ldap.SearchRequest) (*ldap.SearchResult, error) {
	c.searches = append(c.searches, req)
	if c.searchErr != nil {
		return nil, c.searchErr
	}
	switch req.BaseDN {
	case c.userBaseDN:
		return &ldap.SearchResult{Entries: c.userResult}, nil
	case c.groupBaseDN:
		return &ldap.SearchResult{Entries: c.groupResult}, nil
	default:
		return &ldap.SearchResult{}, nil
	}
}

func (c *fakeConn) Close() error { c.closed = true; return nil }

// testConfig is the config every test builds on; individual tests vary the fake.
func testConfig() Config {
	return Config{
		URL:            "ldap://dir.test:389",
		LookupDN:       "cn=lookup,dc=corp",
		LookupPassword: "lookuppw",
		UserBaseDN:     "ou=people,dc=corp",
		UserFilter:     "(uid=%s)",
		GroupBaseDN:    "ou=groups,dc=corp",
		GroupFilter:    "(member=%s)",
	}
}

// withConn builds a Directory whose dial returns the given fake connection.
func withConn(t *testing.T, cfg Config, c *fakeConn) *Directory {
	t.Helper()
	d, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.dial = func() (conn, error) { return c, nil }
	return d
}

func TestAuthenticate(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:      map[string]string{"cn=lookup,dc=corp": "lookuppw", "uid=alice,ou=people,dc=corp": "alicepw"},
		userBaseDN:  cfg.UserBaseDN,
		groupBaseDN: cfg.GroupBaseDN,
		userResult:  []*ldap.Entry{{DN: "uid=alice,ou=people,dc=corp"}},
		groupResult: []*ldap.Entry{
			ldap.NewEntry("cn=data-reader,ou=groups,dc=corp", map[string][]string{"cn": {"data-reader"}}),
			ldap.NewEntry("cn=ops,ou=groups,dc=corp", map[string][]string{"cn": {"  ops  "}}),
		},
	}
	d := withConn(t, cfg, c)

	dn, groups, err := d.Authenticate("alice", "alicepw")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if dn != "uid=alice,ou=people,dc=corp" {
		t.Fatalf("dn = %q", dn)
	}
	if len(groups) != 2 || groups[0] != "data-reader" || groups[1] != "ops" {
		t.Fatalf("groups = %v, want [data-reader ops] (trimmed)", groups)
	}

	// The bind sequence is lookup, user, lookup-again-for-group-search.
	wantBinds := [][2]string{
		{"cn=lookup,dc=corp", "lookuppw"},
		{"uid=alice,ou=people,dc=corp", "alicepw"},
		{"cn=lookup,dc=corp", "lookuppw"},
	}
	if len(c.binds) != len(wantBinds) {
		t.Fatalf("binds = %v, want %v", c.binds, wantBinds)
	}
	for i := range wantBinds {
		if c.binds[i] != wantBinds[i] {
			t.Fatalf("bind %d = %v, want %v", i, c.binds[i], wantBinds[i])
		}
	}
	if !c.closed {
		t.Fatalf("connection not closed")
	}
}

func TestAuthenticateEscapesFilters(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:      map[string]string{"cn=lookup,dc=corp": "lookuppw", "uid=a,ou=people,dc=corp": "pw"},
		userBaseDN:  cfg.UserBaseDN,
		groupBaseDN: cfg.GroupBaseDN,
		userResult:  []*ldap.Entry{{DN: "uid=a,ou=people,dc=corp"}},
	}
	d := withConn(t, cfg, c)

	// A username carrying filter metacharacters must be escaped, never injected.
	const evil = "a)(uid=*"
	if _, _, err := d.Authenticate(evil, "pw"); err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	wantUserFilter := fmt.Sprintf("(uid=%s)", ldap.EscapeFilter(evil))
	if c.searches[0].Filter != wantUserFilter {
		t.Fatalf("user filter = %q, want %q", c.searches[0].Filter, wantUserFilter)
	}
	// The group filter interpolates the (escaped) user DN.
	wantGroupFilter := fmt.Sprintf("(member=%s)", ldap.EscapeFilter("uid=a,ou=people,dc=corp"))
	if c.searches[1].Filter != wantGroupFilter {
		t.Fatalf("group filter = %q, want %q", c.searches[1].Filter, wantGroupFilter)
	}
}

func TestAuthenticateWrongPassword(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:      map[string]string{"cn=lookup,dc=corp": "lookuppw", "uid=alice,ou=people,dc=corp": "alicepw"},
		userBaseDN:  cfg.UserBaseDN,
		groupBaseDN: cfg.GroupBaseDN,
		userResult:  []*ldap.Entry{{DN: "uid=alice,ou=people,dc=corp"}},
	}
	d := withConn(t, cfg, c)

	// The user is found but the bind is rejected: a bad password is ErrInvalidToken.
	if _, _, err := d.Authenticate("alice", "wrong"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateUserNotFound(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:     map[string]string{"cn=lookup,dc=corp": "lookuppw"},
		userBaseDN: cfg.UserBaseDN,
		userResult: nil, // zero matches
	}
	d := withConn(t, cfg, c)

	if _, _, err := d.Authenticate("ghost", "pw"); !errors.Is(err, auth.ErrInvalidToken) {
		t.Fatalf("err = %v, want ErrInvalidToken", err)
	}
}

func TestAuthenticateAmbiguousUser(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:     map[string]string{"cn=lookup,dc=corp": "lookuppw"},
		userBaseDN: cfg.UserBaseDN,
		userResult: []*ldap.Entry{{DN: "uid=a,ou=x"}, {DN: "uid=a,ou=y"}},
	}
	d := withConn(t, cfg, c)

	// Two matches is an operator filter problem, not a bad credential, so it is a
	// communication-class error and never a silent pick of the first entry.
	if _, _, err := d.Authenticate("a", "pw"); !errors.Is(err, auth.ErrIDPCommunication) {
		t.Fatalf("err = %v, want ErrIDPCommunication", err)
	}
}

func TestAuthenticateLookupBindFails(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:     map[string]string{}, // lookup account rejected
		userBaseDN: cfg.UserBaseDN,
	}
	d := withConn(t, cfg, c)

	// A misconfigured service account is a server-side problem, not the caller's.
	if _, _, err := d.Authenticate("alice", "pw"); !errors.Is(err, auth.ErrIDPCommunication) {
		t.Fatalf("err = %v, want ErrIDPCommunication", err)
	}
}

func TestAuthenticateDialFails(t *testing.T) {
	d, err := New(testConfig())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d.dial = func() (conn, error) { return nil, errors.New("connection refused") }

	if _, _, err := d.Authenticate("alice", "pw"); !errors.Is(err, auth.ErrIDPCommunication) {
		t.Fatalf("err = %v, want ErrIDPCommunication", err)
	}
}

func TestAuthenticateSearchFails(t *testing.T) {
	cfg := testConfig()
	c := &fakeConn{
		accept:     map[string]string{"cn=lookup,dc=corp": "lookuppw"},
		userBaseDN: cfg.UserBaseDN,
		searchErr:  errors.New("server closed the connection"),
	}
	d := withConn(t, cfg, c)

	if _, _, err := d.Authenticate("alice", "pw"); !errors.Is(err, auth.ErrIDPCommunication) {
		t.Fatalf("err = %v, want ErrIDPCommunication", err)
	}
}

func TestAuthenticateCustomGroupAttr(t *testing.T) {
	cfg := testConfig()
	cfg.GroupNameAttr = "ou"
	c := &fakeConn{
		accept:      map[string]string{"cn=lookup,dc=corp": "lookuppw", "uid=alice,ou=people,dc=corp": "pw"},
		userBaseDN:  cfg.UserBaseDN,
		groupBaseDN: cfg.GroupBaseDN,
		userResult:  []*ldap.Entry{{DN: "uid=alice,ou=people,dc=corp"}},
		groupResult: []*ldap.Entry{ldap.NewEntry("x", map[string][]string{"ou": {"team-a"}, "cn": {"ignored"}})},
	}
	d := withConn(t, cfg, c)

	_, groups, err := d.Authenticate("alice", "pw")
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if len(groups) != 1 || groups[0] != "team-a" {
		t.Fatalf("groups = %v, want [team-a] from the ou attribute", groups)
	}
}

func TestNewValidation(t *testing.T) {
	base := testConfig()
	cases := []struct {
		name  string
		mutTo func(c *Config)
	}{
		{"no URL", func(c *Config) { c.URL = "" }},
		{"no lookup DN", func(c *Config) { c.LookupDN = "" }},
		{"no lookup password", func(c *Config) { c.LookupPassword = "" }},
		{"no user base", func(c *Config) { c.UserBaseDN = "" }},
		{"no user filter", func(c *Config) { c.UserFilter = "" }},
		{"no group base", func(c *Config) { c.GroupBaseDN = "" }},
		{"no group filter", func(c *Config) { c.GroupFilter = "" }},
		{"user filter without placeholder", func(c *Config) { c.UserFilter = "(uid=fixed)" }},
		{"group filter without placeholder", func(c *Config) { c.GroupFilter = "(member=fixed)" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutTo(&cfg)
			if _, err := New(cfg); err == nil {
				t.Fatalf("New(%s) = nil error, want failure", tc.name)
			}
		})
	}

	// A valid config defaults the group name attribute to cn.
	d, err := New(base)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if d.cfg.GroupNameAttr != "cn" {
		t.Fatalf("GroupNameAttr = %q, want cn", d.cfg.GroupNameAttr)
	}
}

// BenchmarkAuthenticate measures the bind-and-search orchestration against the fake
// connection, so it reports the filter building and result handling cost without a
// network round trip.
func BenchmarkAuthenticate(b *testing.B) {
	cfg := testConfig()
	d, err := New(cfg)
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	// A fresh connection per call, as a real dial would hand back, so the recorded
	// bind/search slices do not grow across iterations and skew the allocation count.
	d.dial = func() (conn, error) {
		return &fakeConn{
			accept:      map[string]string{"cn=lookup,dc=corp": "lookuppw", "uid=alice,ou=people,dc=corp": "pw"},
			userBaseDN:  cfg.UserBaseDN,
			groupBaseDN: cfg.GroupBaseDN,
			userResult:  []*ldap.Entry{{DN: "uid=alice,ou=people,dc=corp"}},
			groupResult: []*ldap.Entry{ldap.NewEntry("g", map[string][]string{"cn": {"readonly"}})},
		}, nil
	}

	b.ReportAllocs()
	for b.Loop() {
		if _, _, err := d.Authenticate("alice", "pw"); err != nil {
			b.Fatalf("Authenticate: %v", err)
		}
	}
}
