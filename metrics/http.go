// SPDX-License-Identifier: Apache-2.0

package metrics

import "net/http"

// textContentType is the content type for the Prometheus text exposition format,
// including the version the format documents.
const textContentType = "text/plain; version=0.0.4; charset=utf-8"

// Handler returns an http.Handler that writes the registry in the Prometheus text
// exposition format on a GET. It performs no authentication; the caller wraps it with
// whatever access control the deployment wants (liteio gates it behind a token, doc
// 10.4). Mount it at the metrics path.
func Handler(r *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method != http.MethodGet && req.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", textContentType)
		_ = r.WriteText(w)
	})
}
