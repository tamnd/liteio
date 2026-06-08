// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/tamnd/liteio/trace"
)

// TraceSource is the subscriber-facing slice of a ring buffer. The admin
// streaming endpoint calls Subscribe to get a channel of TraceEvents plus a
// cancel function it defers.
type TraceSource interface {
	Subscribe() (<-chan trace.TraceEvent, func())
}

// WithTraceSource wires the live trace event source. Without it the trace
// streaming endpoint returns 501.
func WithTraceSource(ts TraceSource) Option {
	return func(s *Server) { s.traceSource = ts }
}

// streamTrace handles GET /liteio/admin/v1/trace. It opens an SSE stream and
// writes each TraceEvent as a JSON-encoded "data:" line until the client
// disconnects. The handler returns 501 when no trace source is wired.
func (s *Server) streamTrace(w http.ResponseWriter, r *http.Request) {
	if s.traceSource == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "trace source not configured")
		return
	}

	// Subscribe before sending the response headers so that any events emitted
	// concurrently after the client sees the 200 are not missed.
	ch, cancel := s.traceSource.Subscribe()
	defer cancel()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, ok := w.(http.Flusher)
	if !ok {
		return
	}
	flusher.Flush()

	for {
		select {
		case ev, open := <-ch:
			if !open {
				return
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}
