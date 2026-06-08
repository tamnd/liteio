// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"net/http"

	"github.com/tamnd/liteio/object"
)

// HealLayer is the slice of the object layer the heal status endpoint reads.
// *object.ServerPools satisfies it via its MRFStats method.
type HealLayer interface {
	MRFStats() object.MRFStats
}

// WithHealer wires the heal status source. Without it the heal endpoint returns
// 501 so the route is always registered but a node without an object layer does
// not pretend to have heal counters.
func WithHealer(hl HealLayer) Option {
	return func(s *Server) { s.healer = hl }
}

// healStatusResponse is the wire shape of the heal counters.
type healStatusResponse struct {
	Pending int64 `json:"pending"`
	Dropped int64 `json:"dropped"`
	Healed  int64 `json:"healed"`
	Failed  int64 `json:"failed"`
}

// healStatus handles GET /liteio/admin/v1/heal. It returns the reactive-heal
// queue counters (spec doc 10.2). The counters are monotonic except Pending,
// which is a point-in-time snapshot of the buffer depth.
func (s *Server) healStatus(w http.ResponseWriter, _ *http.Request) {
	if s.healer == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "healer not configured")
		return
	}
	st := s.healer.MRFStats()
	writeJSON(w, http.StatusOK, healStatusResponse{
		Pending: st.Pending,
		Dropped: st.Dropped,
		Healed:  st.Healed,
		Failed:  st.Failed,
	})
}
