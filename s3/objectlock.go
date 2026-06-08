// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"net/http"

	"github.com/tamnd/liteio/object"
)

// --- bucket-level Object Lock configuration --------------------------------

// putObjectLockConfiguration handles PUT /bucket?object-lock.
func (s *Server) putObjectLockConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	var req objectLockConfigurationXML
	if !decodeXMLBody(w, r, requestID, &req) {
		return
	}
	cfg := object.ObjectLockConfig{Enabled: req.ObjectLockEnabled == "Enabled"}
	if req.Rule != nil {
		dr := req.Rule.DefaultRetention
		cfg.Rule = &object.DefaultRetention{
			Mode:  dr.Mode,
			Days:  dr.Days,
			Years: dr.Years,
		}
	}
	if err := s.layer.SetObjectLockConfiguration(r.Context(), bucket, cfg); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getObjectLockConfiguration handles GET /bucket?object-lock.
func (s *Server) getObjectLockConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	cfg, err := s.layer.GetObjectLockConfiguration(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	resp := objectLockConfigurationXML{XMLNS: s3XMLNS}
	if cfg.Enabled {
		resp.ObjectLockEnabled = "Enabled"
	}
	if cfg.Rule != nil {
		resp.Rule = &objectLockRuleXML{
			DefaultRetention: defaultRetentionXML{
				Mode:  cfg.Rule.Mode,
				Days:  cfg.Rule.Days,
				Years: cfg.Rule.Years,
			},
		}
	}
	writeXML(w, requestID, http.StatusOK, resp)
}

// --- per-object retention --------------------------------------------------

// putObjectRetention handles PUT /bucket/key?retention.
func (s *Server) putObjectRetention(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	var req retentionXML
	if !decodeXMLBody(w, r, requestID, &req) {
		return
	}
	versionID := r.URL.Query().Get("versionId")
	if err := s.layer.SetObjectRetention(r.Context(), bucket, object, versionID, req.Mode, req.RetainUntilDate); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getObjectRetention handles GET /bucket/key?retention.
func (s *Server) getObjectRetention(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	versionID := r.URL.Query().Get("versionId")
	mode, until, err := s.layer.GetObjectRetention(r.Context(), bucket, object, versionID)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, retentionXML{
		XMLNS:           s3XMLNS,
		Mode:            mode,
		RetainUntilDate: until,
	})
}

// --- per-object legal hold -------------------------------------------------

// putObjectLegalHold handles PUT /bucket/key?legal-hold.
func (s *Server) putObjectLegalHold(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	var req legalHoldXML
	if !decodeXMLBody(w, r, requestID, &req) {
		return
	}
	versionID := r.URL.Query().Get("versionId")
	if err := s.layer.SetObjectLegalHold(r.Context(), bucket, object, versionID, req.Status); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getObjectLegalHold handles GET /bucket/key?legal-hold.
func (s *Server) getObjectLegalHold(w http.ResponseWriter, r *http.Request, requestID, bucket, object string) {
	versionID := r.URL.Query().Get("versionId")
	status, err := s.layer.GetObjectLegalHold(r.Context(), bucket, object, versionID)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, legalHoldXML{XMLNS: s3XMLNS, Status: status})
}

// decodeXMLBody reads and decodes an XML request body into v; on error it
// writes an S3 MalformedXML response and returns false.
func decodeXMLBody(w http.ResponseWriter, r *http.Request, requestID string, v any) bool {
	if err := xml.NewDecoder(r.Body).Decode(v); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return false
	}
	return true
}
