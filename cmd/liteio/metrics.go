// SPDX-License-Identifier: Apache-2.0

package main

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/tamnd/liteio/metrics"
)

// metricsHandler wraps the registry's exposition handler with bearer-token auth, so
// the metrics endpoint (doc 10.4) is reachable only by a scraper that presents the
// token. The token is compared in constant time. An empty presented token, a wrong
// token, or a missing Authorization header is refused with 401.
func metricsHandler(reg *metrics.Registry, token string) http.Handler {
	exposition := metrics.Handler(reg)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !bearerTokenOK(r, token) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		exposition.ServeHTTP(w, r)
	})
}

// bearerTokenOK reports whether the request carries the expected token as a Bearer
// credential. The comparison is constant-time to avoid leaking the token by timing.
func bearerTokenOK(r *http.Request, token string) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}
