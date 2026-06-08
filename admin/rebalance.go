// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"net/http"
	"strconv"

	"github.com/tamnd/liteio/object"
)

// RebalanceLayer is the object-layer interface the admin server uses for
// rebalance and decommission operations. object.ServerPools satisfies it.
type RebalanceLayer interface {
	Rebalance(ctx context.Context) error
	RebalanceStatus() object.RebalanceStatus
	StopRebalance()

	Decommission(ctx context.Context, poolIdx int) error
	DecommissionStatus(poolIdx int) object.DecommissionStatus
	StopDecommission(poolIdx int)
}

// WithRebalancer wires the object layer for rebalance and decommission
// endpoints. Without it those routes return 501 Not Implemented.
func WithRebalancer(rl RebalanceLayer) Option {
	return func(s *Server) { s.rebalancer = rl }
}

// startRebalance handles POST /admin/v1/rebalance.
// It launches the rebalancer in the background and returns 202 immediately.
func (s *Server) startRebalance(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	go func() { _ = s.rebalancer.Rebalance(context.Background()) }()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started"})
}

// getRebalanceStatus handles GET /admin/v1/rebalance.
func (s *Server) getRebalanceStatus(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	writeJSON(w, http.StatusOK, s.rebalancer.RebalanceStatus())
}

// stopRebalance handles DELETE /admin/v1/rebalance.
func (s *Server) stopRebalance(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	s.rebalancer.StopRebalance()
	w.WriteHeader(http.StatusNoContent)
}

// startDecommission handles POST /admin/v1/decommission.
// It reads the pool index from the ?pool= query parameter and launches
// the decommissioner in the background.
func (s *Server) startDecommission(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	idx, err := parsePoolIdx(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "InvalidArgument", Message: err.Error()})
		return
	}
	go func() { _ = s.rebalancer.Decommission(context.Background(), idx) }()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "pool": strconv.Itoa(idx)})
}

// getDecommissionStatus handles GET /admin/v1/decommission.
func (s *Server) getDecommissionStatus(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	idx, err := parsePoolIdx(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "InvalidArgument", Message: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, s.rebalancer.DecommissionStatus(idx))
}

// stopDecommission handles DELETE /admin/v1/decommission.
func (s *Server) stopDecommission(w http.ResponseWriter, r *http.Request) {
	if s.rebalancer == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "rebalancer not configured"})
		return
	}
	idx, err := parsePoolIdx(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "InvalidArgument", Message: err.Error()})
		return
	}
	s.rebalancer.StopDecommission(idx)
	w.WriteHeader(http.StatusNoContent)
}

// parsePoolIdx reads ?pool=<n> from r and returns the integer pool index.
func parsePoolIdx(r *http.Request) (int, error) {
	s := r.URL.Query().Get("pool")
	if s == "" {
		return 0, strconv.ErrSyntax
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, strconv.ErrSyntax
	}
	return n, nil
}
