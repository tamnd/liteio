// SPDX-License-Identifier: Apache-2.0

// Package cluster models a liteio deployment's physical topology: the set of
// drive endpoints an operator declares, how those endpoints expand from a
// compact pattern, how they are grouped into erasure sets that spread across
// failure domains, and the format.json identity each drive carries (spec 2020,
// doc 04 and doc 11).
//
// A deployment is described to liteio as one or more arguments, each a path or
// URL that may carry a numeric ellipsis pattern. For example a single argument
//
//	https://node{1...4}.lan/mnt/disk{1...8}
//
// expands to the 32 drives node1..node4 each holding disk1..disk8. Expansion is
// pure and deterministic: the same arguments always yield the same ordered
// endpoint list, which is what lets placement be lookup-free.
package cluster

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"
)

// Endpoint identifies one drive in the deployment. A local endpoint (one served
// by this process) has an empty Scheme and Host and a filesystem Path. A remote
// endpoint carries the peer's Scheme and Host and the drive's Path on that peer.
type Endpoint struct {
	// Scheme is "http" or "https" for a remote endpoint, empty for a local path.
	Scheme string
	// Host is "host" or "host:port" for a remote endpoint, empty for a local path.
	Host string
	// Path is the drive's directory, always present.
	Path string
}

// IsLocal reports whether the endpoint is a bare filesystem path served by this
// process rather than a drive reached over the network.
func (e Endpoint) IsLocal() bool { return e.Host == "" }

// String renders the endpoint back to its canonical textual form: a bare path
// when local, or scheme://host/path when remote.
func (e Endpoint) String() string {
	if e.IsLocal() {
		return e.Path
	}
	return e.Scheme + "://" + e.Host + e.Path
}

// ParsePattern expands a list of deployment arguments into the full ordered list
// of drive endpoints. Each argument may be a plain path, a URL, or either with
// one or more numeric ellipsis ranges of the form {lo...hi}. Ranges expand as a
// cartesian product in left-to-right order, and a range written with leading
// zeros (such as {01...12}) keeps that zero-padding in every expansion so it
// matches zero-padded directory names.
//
// The arguments are expanded independently and concatenated, so an operator can
// describe a heterogeneous deployment as several arguments. The result preserves
// declaration order, which the layout step relies on to spread sets across hosts.
func ParsePattern(args []string) ([]Endpoint, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("cluster: no endpoint arguments given")
	}
	var out []Endpoint
	for _, arg := range args {
		expanded, err := expandArg(arg)
		if err != nil {
			return nil, err
		}
		out = append(out, expanded...)
	}
	return out, nil
}

// expandArg expands one argument: it first expands any ellipsis ranges in the
// raw string, then parses each resulting string into an Endpoint.
func expandArg(arg string) ([]Endpoint, error) {
	strs, err := expandEllipsis(arg)
	if err != nil {
		return nil, err
	}
	out := make([]Endpoint, 0, len(strs))
	for _, s := range strs {
		e, err := parseEndpoint(s)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

// parseEndpoint turns one fully-expanded string (no ellipsis left) into an
// Endpoint. A string with a scheme is treated as a remote URL; anything else is
// a local filesystem path.
func parseEndpoint(s string) (Endpoint, error) {
	if !strings.Contains(s, "://") {
		if s == "" {
			return Endpoint{}, fmt.Errorf("cluster: empty endpoint path")
		}
		return Endpoint{Path: s}, nil
	}
	u, err := url.Parse(s)
	if err != nil {
		return Endpoint{}, fmt.Errorf("cluster: parse endpoint %q: %w", s, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return Endpoint{}, fmt.Errorf("cluster: endpoint %q has unsupported scheme %q", s, u.Scheme)
	}
	if u.Host == "" {
		return Endpoint{}, fmt.Errorf("cluster: endpoint %q has no host", s)
	}
	path := u.Path
	if path == "" || path == "/" {
		return Endpoint{}, fmt.Errorf("cluster: endpoint %q has no drive path", s)
	}
	return Endpoint{Scheme: u.Scheme, Host: u.Host, Path: path}, nil
}

// expandEllipsis expands every {lo...hi} range in s as a cartesian product,
// returning the full list of concrete strings in row-major order (the leftmost
// range varies slowest). A string with no range returns itself unchanged.
func expandEllipsis(s string) ([]string, error) {
	open := strings.IndexByte(s, '{')
	if open < 0 {
		if strings.IndexByte(s, '}') >= 0 {
			return nil, fmt.Errorf("cluster: unmatched '}' in %q", s)
		}
		return []string{s}, nil
	}
	close := strings.IndexByte(s[open:], '}')
	if close < 0 {
		return nil, fmt.Errorf("cluster: unmatched '{' in %q", s)
	}
	close += open

	prefix := s[:open]
	body := s[open+1 : close]
	rest := s[close+1:]

	values, err := rangeValues(body)
	if err != nil {
		return nil, err
	}
	// Expand the remainder (which may hold further ranges) once, then prepend each
	// of this range's values to every tail.
	tails, err := expandEllipsis(rest)
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(values)*len(tails))
	for _, v := range values {
		for _, tail := range tails {
			out = append(out, prefix+v+tail)
		}
	}
	return out, nil
}

// rangeValues turns a range body "lo...hi" into its inclusive list of values,
// preserving zero-padding when lo or hi is written with a leading zero.
func rangeValues(body string) ([]string, error) {
	loStr, hiStr, ok := strings.Cut(body, "...")
	if !ok {
		return nil, fmt.Errorf("cluster: range %q must be of the form {lo...hi}", body)
	}
	lo, err := strconv.Atoi(loStr)
	if err != nil {
		return nil, fmt.Errorf("cluster: range bound %q is not a number", loStr)
	}
	hi, err := strconv.Atoi(hiStr)
	if err != nil {
		return nil, fmt.Errorf("cluster: range bound %q is not a number", hiStr)
	}
	if lo < 0 || hi < 0 {
		return nil, fmt.Errorf("cluster: range %q bounds must be non-negative", body)
	}
	if lo > hi {
		return nil, fmt.Errorf("cluster: range %q has lo greater than hi", body)
	}
	// Zero-pad the output when either bound was written padded, to the width of the
	// widest bound, so {01...12} yields 01,02,...,12 to match padded directories.
	width := 0
	if (len(loStr) > 1 && loStr[0] == '0') || (len(hiStr) > 1 && hiStr[0] == '0') {
		width = max(len(loStr), len(hiStr))
	}
	out := make([]string, 0, hi-lo+1)
	for n := lo; n <= hi; n++ {
		if width > 0 {
			out = append(out, fmt.Sprintf("%0*d", width, n))
		} else {
			out = append(out, strconv.Itoa(n))
		}
	}
	return out, nil
}
