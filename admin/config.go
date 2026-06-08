// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"net/http"
	"sync"
)

// ClusterConfig holds the cluster-wide tunables operators read and write through
// the config endpoint (spec doc 10.2).
type ClusterConfig struct {
	Region                string `json:"region"`
	MaxConcurrentRequests int    `json:"maxConcurrentRequests"`
	HealWorkers           int    `json:"healWorkers"`
	ScannerInterval       string `json:"scannerInterval"`
}

// ConfigStore is the backing store the config endpoints drive.
type ConfigStore interface {
	GetConfig() ClusterConfig
	SetConfig(ClusterConfig) error
}

// InMemoryConfigStore is a thread-safe in-memory ConfigStore. It is the default
// store used when no persistent backend is wired; an operator change survives
// only until the process restarts.
type InMemoryConfigStore struct {
	mu  sync.RWMutex
	cfg ClusterConfig
}

// NewInMemoryConfigStore returns a store seeded with defaults.
func NewInMemoryConfigStore(defaults ClusterConfig) *InMemoryConfigStore {
	return &InMemoryConfigStore{cfg: defaults}
}

// GetConfig returns the current configuration under a read lock.
func (cs *InMemoryConfigStore) GetConfig() ClusterConfig {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.cfg
}

// SetConfig replaces the current configuration under a write lock.
func (cs *InMemoryConfigStore) SetConfig(cfg ClusterConfig) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.cfg = cfg
	return nil
}

// WithConfig wires the cluster config store. Without it the config endpoints
// return 501.
func WithConfig(cs ConfigStore) Option {
	return func(s *Server) { s.configStore = cs }
}

// getConfig handles GET /liteio/admin/v1/config. It returns the current cluster
// config as JSON (spec doc 10.2).
func (s *Server) getConfig(w http.ResponseWriter, _ *http.Request) {
	if s.configStore == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "config store not configured")
		return
	}
	writeJSON(w, http.StatusOK, s.configStore.GetConfig())
}

// setConfig handles PUT /liteio/admin/v1/config. It decodes a ClusterConfig
// from the request body and persists it via the config store.
func (s *Server) setConfig(w http.ResponseWriter, r *http.Request) {
	if s.configStore == nil {
		writeError(w, http.StatusNotImplemented, "NotImplemented", "config store not configured")
		return
	}
	var cfg ClusterConfig
	if !decodeJSON(w, r, &cfg) {
		return
	}
	if err := s.configStore.SetConfig(cfg); err != nil {
		writeError(w, http.StatusInternalServerError, "InternalError", err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
