// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"io"
	"net/http"
)

const maxLifecycleBody = 256 << 10 // 256 KiB — lifecycle XML can be large

// putBucketLifecycleConfiguration handles PUT /bucket?lifecycle.
func (s *Server) putBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxLifecycleBody+1))
	if err != nil || len(body) > maxLifecycleBody {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	if len(body) == 0 {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	if err := s.layer.SetBucketLifecycle(r.Context(), bucket, body); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketLifecycleConfiguration handles GET /bucket?lifecycle.
func (s *Server) getBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	doc, err := s.layer.GetBucketLifecycle(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	setCommonHeaders(w, requestID)
	w.WriteHeader(http.StatusOK)
	w.Write(doc) //nolint:errcheck
}

// deleteBucketLifecycleConfiguration handles DELETE /bucket?lifecycle.
func (s *Server) deleteBucketLifecycleConfiguration(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketLifecycle(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
