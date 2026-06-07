// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"

	"github.com/tamnd/liteio/object"
)

// newMultipartUpload handles POST /bucket/key?uploads: it begins a multipart
// upload and returns the opaque UploadId clients pass to subsequent calls.
func (s *Server) newMultipartUpload(w http.ResponseWriter, r *http.Request, requestID, bucket, key string) {
	opts := object.ObjectOptions{
		ContentType: r.Header.Get("Content-Type"),
		UserDefined: userMetaFromHeader(r),
	}
	uploadID, err := s.layer.NewMultipartUpload(r.Context(), bucket, key, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, initiateMultipartUploadResult{
		XMLNS:    s3XMLNS,
		Bucket:   bucket,
		Key:      key,
		UploadID: uploadID,
	})
}

// uploadPart handles PUT /bucket/key?partNumber=N&uploadId=...: it stores one
// part and returns its ETag, which the client echoes back at completion.
func (s *Server) uploadPart(w http.ResponseWriter, r *http.Request, requestID, bucket, key, uploadID string) {
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 {
		writeError(w, requestID, r.URL.Path, errInvalidArgument)
		return
	}
	info, err := s.layer.PutObjectPart(r.Context(), bucket, key, uploadID, partNumber,
		object.NewPutReader(r.Body, r.ContentLength), object.ObjectOptions{})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.WriteHeader(http.StatusOK)
}

// completeMultipartUpload handles POST /bucket/key?uploadId=...: it assembles the
// uploaded parts into a finished object.
func (s *Server) completeMultipartUpload(w http.ResponseWriter, r *http.Request, requestID, bucket, key, uploadID string) {
	var req completeMultipartUpload
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	parts := make([]object.CompletePart, len(req.Parts))
	for i, p := range req.Parts {
		parts[i] = object.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag}
	}
	info, err := s.layer.CompleteMultipartUpload(r.Context(), bucket, key, uploadID, parts, object.ObjectOptions{})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	writeXML(w, requestID, http.StatusOK, completeMultipartUploadResult{
		XMLNS:    s3XMLNS,
		Location: "/" + bucket + "/" + key,
		Bucket:   bucket,
		Key:      key,
		ETag:     quoteETag(info.ETag),
	})
}

// abortMultipartUpload handles DELETE /bucket/key?uploadId=...: it discards an
// upload and its staged parts.
func (s *Server) abortMultipartUpload(w http.ResponseWriter, r *http.Request, requestID, bucket, key, uploadID string) {
	if err := s.layer.AbortMultipartUpload(r.Context(), bucket, key, uploadID, object.ObjectOptions{}); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// listObjectParts handles GET /bucket/key?uploadId=...: it lists the parts
// uploaded so far for an in-progress upload.
func (s *Server) listObjectParts(w http.ResponseWriter, r *http.Request, requestID, bucket, key, uploadID string) {
	q := r.URL.Query()
	maxParts := 1000
	if v := q.Get("max-parts"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxParts = n
		}
	}
	marker := 0
	if v := q.Get("part-number-marker"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			marker = n
		}
	}
	res, err := s.layer.ListObjectParts(r.Context(), bucket, key, uploadID, marker, maxParts, object.ObjectOptions{})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	out := listPartsResult{
		XMLNS:                s3XMLNS,
		Bucket:               res.Bucket,
		Key:                  res.Object,
		UploadID:             res.UploadID,
		PartNumberMarker:     res.PartNumberMarker,
		NextPartNumberMarker: res.NextPartNumberMarker,
		MaxParts:             res.MaxParts,
		IsTruncated:          res.IsTruncated,
		StorageClass:         "STANDARD",
		Initiator:            canonicalUser{ID: ownerID, DisplayName: ownerID},
		Owner:                canonicalUser{ID: ownerID, DisplayName: ownerID},
	}
	for _, p := range res.Parts {
		out.Parts = append(out.Parts, partXML{
			PartNumber:   p.PartNumber,
			LastModified: amzTime(p.LastModified),
			ETag:         quoteETag(p.ETag),
			Size:         p.Size,
		})
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// listMultipartUploads handles GET /bucket?uploads: it lists in-progress
// multipart uploads in a bucket.
func (s *Server) listMultipartUploads(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q := r.URL.Query()
	maxUploads := 1000
	if v := q.Get("max-uploads"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxUploads = n
		}
	}
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	keyMarker := q.Get("key-marker")
	uploadIDMarker := q.Get("upload-id-marker")

	res, err := s.layer.ListMultipartUploads(r.Context(), bucket, prefix, keyMarker, uploadIDMarker, delim, maxUploads)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	out := listMultipartUploadsResult{
		XMLNS:              s3XMLNS,
		Bucket:             res.Bucket,
		Prefix:             res.Prefix,
		Delimiter:          res.Delimiter,
		KeyMarker:          res.KeyMarker,
		UploadIDMarker:     res.UploadIDMarker,
		NextKeyMarker:      res.NextKeyMarker,
		NextUploadIDMarker: res.NextUploadIDMarker,
		MaxUploads:         res.MaxUploads,
		IsTruncated:        res.IsTruncated,
	}
	for _, u := range res.Uploads {
		out.Uploads = append(out.Uploads, uploadXML{
			Key:          u.Object,
			UploadID:     u.UploadID,
			StorageClass: "STANDARD",
			Initiated:    amzTime(u.Initiated),
			Initiator:    canonicalUser{ID: ownerID, DisplayName: ownerID},
			Owner:        canonicalUser{ID: ownerID, DisplayName: ownerID},
		})
	}
	for _, p := range res.CommonPrefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: p})
	}
	writeXML(w, requestID, http.StatusOK, out)
}
