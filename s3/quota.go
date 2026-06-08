// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tamnd/liteio/object"
)

// getBucketQuota handles GET /bucket?quota.
func (s *Server) getBucketQuota(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q, err := s.layer.GetBucketQuota(r.Context(), bucket)
	if err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		if errors.Is(err, object.ErrNoSuchBucketQuota) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucketQuota)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	b, _ := json.Marshal(q)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(b) //nolint:errcheck
}

// putBucketQuota handles PUT /bucket?quota.
func (s *Server) putBucketQuota(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	var q object.BucketQuota
	if !decodeJSONBody(w, r, requestID, &q) {
		return
	}
	if err := s.layer.SetBucketQuota(r.Context(), bucket, q); err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// deleteBucketQuota handles DELETE /bucket?quota.
func (s *Server) deleteBucketQuota(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketQuota(r.Context(), bucket); err != nil {
		if errors.Is(err, object.ErrBucketNotFound) {
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
			return
		}
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// decodeJSONBody decodes the request body as JSON into v. On error it writes an
// S3 MalformedXML error (reusing that code for JSON parse errors) and returns false.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, requestID string, v any) bool {
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return false
	}
	return true
}
