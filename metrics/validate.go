// SPDX-License-Identifier: Apache-2.0

package metrics

import "strconv"

// validName reports whether s is a valid Prometheus metric name: it must be
// non-empty and match [a-zA-Z_:][a-zA-Z0-9_:]*.
func validName(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' || c == ':' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// validLabel reports whether s is a valid label name: non-empty, [a-zA-Z_][a-zA-Z0-9_]*,
// and not reserved (a leading "__" is reserved for internal use). A label may not use
// the colon a metric name allows.
func validLabel(s string) bool {
	if s == "" || (len(s) >= 2 && s[0] == '_' && s[1] == '_') {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		ok := c == '_' ||
			(c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') ||
			(i > 0 && c >= '0' && c <= '9')
		if !ok {
			return false
		}
	}
	return true
}

// mustValidName panics on an invalid metric name. An invalid name is a programmer
// error (a literal in the instrumentation), caught loudly at startup rather than
// emitting output a scraper would reject.
func mustValidName(name string) {
	if !validName(name) {
		panic("metrics: invalid metric name " + strconv.Quote(name))
	}
}

// mustValidLabels panics on any invalid or reserved label name, or a name "le" used
// with a histogram (reserved for the bucket bound). The histogram check is applied by
// the caller; here we reject the always-invalid cases.
func mustValidLabels(names []string) {
	for _, n := range names {
		if !validLabel(n) {
			panic("metrics: invalid label name " + strconv.Quote(n))
		}
	}
}
