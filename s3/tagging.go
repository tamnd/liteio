// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/json"
	"encoding/xml"
	"io"
	"net/http"

	"github.com/tamnd/liteio/object"
)

func unmarshalBucketTags(doc []byte, out *map[string]string) error {
	return json.Unmarshal(doc, out)
}

const maxTaggingBody = 16 << 10 // 16 KiB — tags are tiny

// --- object tagging -------------------------------------------------------

// putObjectTagging handles PUT /bucket/key?tagging.
func (s *Server) putObjectTagging(w http.ResponseWriter, r *http.Request, requestID, bucket, obj string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTaggingBody+1))
	if err != nil || len(body) > maxTaggingBody {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	var req taggingRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	tags := tagsFromXML(req.TagSet)
	if err := object.ValidateTags(tags, 10); err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	versionID := r.URL.Query().Get("versionId")
	if err := s.layer.SetObjectTags(r.Context(), bucket, obj, versionID, tags); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getObjectTagging handles GET /bucket/key?tagging.
func (s *Server) getObjectTagging(w http.ResponseWriter, r *http.Request, requestID, bucket, obj string) {
	versionID := r.URL.Query().Get("versionId")
	tags, err := s.layer.GetObjectTags(r.Context(), bucket, obj, versionID)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, taggingResponse{
		XMLNS:  s3XMLNS,
		TagSet: tagsToXML(tags),
	})
}

// deleteObjectTagging handles DELETE /bucket/key?tagging.
func (s *Server) deleteObjectTagging(w http.ResponseWriter, r *http.Request, requestID, bucket, obj string) {
	versionID := r.URL.Query().Get("versionId")
	if err := s.layer.DeleteObjectTags(r.Context(), bucket, obj, versionID); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- bucket tagging -------------------------------------------------------

// putBucketTagging handles PUT /bucket?tagging.
func (s *Server) putBucketTagging(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxTaggingBody+1))
	if err != nil || len(body) > maxTaggingBody {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	var req taggingRequest
	if err := xml.Unmarshal(body, &req); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	tags := tagsFromXML(req.TagSet)
	if err := object.ValidateTags(tags, 50); err != nil {
		writeError(w, requestID, r.URL.Path, toAPIError(err))
		return
	}
	doc, err := object.BucketTagsToJSON(tags)
	if err != nil {
		writeError(w, requestID, r.URL.Path, errInternalError)
		return
	}
	if err := s.layer.SetBucketTagging(r.Context(), bucket, doc); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketTagging handles GET /bucket?tagging.
func (s *Server) getBucketTagging(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	doc, err := s.layer.GetBucketTagging(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	// doc is JSON; convert to wire XML.
	tags := map[string]string{}
	if len(doc) > 0 {
		if err := unmarshalBucketTags(doc, &tags); err != nil {
			writeError(w, requestID, r.URL.Path, errInternalError)
			return
		}
	}
	writeXML(w, requestID, http.StatusOK, taggingResponse{
		XMLNS:  s3XMLNS,
		TagSet: tagsToXML(tags),
	})
}

// deleteBucketTagging handles DELETE /bucket?tagging.
func (s *Server) deleteBucketTagging(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketTagging(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
