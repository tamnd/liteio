// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"context"
	"errors"
	"net/http"

	"github.com/tamnd/liteio/object"
	tierpkg "github.com/tamnd/liteio/tier"
)

// TierLayer is the object-layer interface the admin server uses for tier management.
// Using a narrower interface keeps the admin server independent of the full object
// layer so control nodes that only run IAM can omit it.
type TierLayer interface {
	SetTierConfig(ctx context.Context, cfg tierpkg.TierConfig) error
	GetTierConfig(ctx context.Context, name string) (tierpkg.TierConfig, error)
	ListTierConfigs(ctx context.Context) ([]tierpkg.TierConfig, error)
	DeleteTierConfig(ctx context.Context, name string) error
}

// WithTiers wires the object layer for tier management endpoints. Without it the
// tier routes return 501 Not Implemented.
func WithTiers(tl TierLayer) Option {
	return func(s *Server) { s.tiers = tl }
}

// listTiers handles GET /admin/v1/tiers.
func (s *Server) listTiers(w http.ResponseWriter, r *http.Request) {
	if s.tiers == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "tier management not configured"})
		return
	}
	cfgs, err := s.tiers.ListTierConfigs(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Code: "InternalError", Message: err.Error()})
		return
	}
	if cfgs == nil {
		cfgs = []tierpkg.TierConfig{}
	}
	writeJSON(w, http.StatusOK, cfgs)
}

// putTier handles PUT /admin/v1/tiers/{name}.
func (s *Server) putTier(w http.ResponseWriter, r *http.Request) {
	if s.tiers == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "tier management not configured"})
		return
	}
	name := r.PathValue("name")
	var cfg tierpkg.TierConfig
	if !decodeJSON(w, r, &cfg) {
		return
	}
	// The name in the path is authoritative.
	cfg.Name = name
	if err := tierpkg.Validate(cfg); err != nil {
		writeJSON(w, http.StatusBadRequest, apiError{Code: "InvalidTierConfig", Message: err.Error()})
		return
	}
	if err := s.tiers.SetTierConfig(r.Context(), cfg); err != nil {
		writeJSON(w, http.StatusInternalServerError, apiError{Code: "InternalError", Message: err.Error()})
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getTier handles GET /admin/v1/tiers/{name}.
func (s *Server) getTier(w http.ResponseWriter, r *http.Request) {
	if s.tiers == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "tier management not configured"})
		return
	}
	name := r.PathValue("name")
	cfg, err := s.tiers.GetTierConfig(r.Context(), name)
	if err != nil {
		if errors.Is(err, object.ErrNoSuchTierConfig) {
			writeJSON(w, http.StatusNotFound, apiError{Code: "NoSuchTierConfiguration", Message: "tier not found: " + name})
			return
		}
		writeJSON(w, http.StatusInternalServerError, apiError{Code: "InternalError", Message: err.Error()})
		return
	}
	// Redact credentials in the response.
	redact(&cfg)
	writeJSON(w, http.StatusOK, cfg)
}

// deleteTier handles DELETE /admin/v1/tiers/{name}.
func (s *Server) deleteTier(w http.ResponseWriter, r *http.Request) {
	if s.tiers == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "tier management not configured"})
		return
	}
	name := r.PathValue("name")
	if err := s.tiers.DeleteTierConfig(r.Context(), name); err != nil {
		if errors.Is(err, object.ErrNoSuchTierConfig) {
			writeJSON(w, http.StatusNotFound, apiError{Code: "NoSuchTierConfiguration", Message: "tier not found: " + name})
			return
		}
		writeJSON(w, http.StatusInternalServerError, apiError{Code: "InternalError", Message: err.Error()})
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// redact removes credentials from a TierConfig before returning it to the client.
func redact(cfg *tierpkg.TierConfig) {
	if cfg.S3 != nil {
		cfg.S3.SecretKey = ""
	}
}

// apiError is a simple JSON error envelope for the admin API.
type apiError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
