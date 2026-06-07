// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tamnd/liteio/object"
)

// ownerID is the canonical-user identity liteio reports until IAM lands; a single
// account owns every bucket/object (doc 02 leaves ACLs to a later milestone).
const ownerID = "liteio"

// fail renders err through the S3 catalog mapper.
func (s *Server) fail(w http.ResponseWriter, requestID, resource string, err error) {
	writeError(w, requestID, resource, toAPIError(err))
}

// --- service ---------------------------------------------------------------

func (s *Server) listBuckets(w http.ResponseWriter, r *http.Request, requestID string) {
	buckets, err := s.layer.ListBuckets(r.Context())
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	out := listAllMyBucketsResult{
		XMLNS: s3XMLNS,
		Owner: canonicalUser{ID: ownerID, DisplayName: ownerID},
	}
	for _, b := range buckets {
		out.Buckets.Bucket = append(out.Buckets.Bucket, bucketEntry{
			Name:         b.Name,
			CreationDate: amzTime(b.Created),
		})
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// --- buckets ---------------------------------------------------------------

func (s *Server) createBucket(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	// The optional CreateBucketConfiguration body only carries a region we ignore;
	// reading it keeps strict clients happy and drains the connection.
	_, _ = io.Copy(io.Discard, r.Body)
	if err := s.layer.MakeBucket(r.Context(), bucket, object.MakeBucketOptions{}); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.Header().Set("Location", "/"+bucket)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) headBucket(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		// HEAD carries no body; the status alone signals the error.
		w.WriteHeader(toAPIError(err).HTTPStatus)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteBucket(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucket(r.Context(), bucket, object.DeleteBucketOptions{}); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) getBucketLocation(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	// us-east-1 is rendered as an empty LocationConstraint, per S3.
	writeXML(w, requestID, http.StatusOK, locationConstraint{XMLNS: s3XMLNS})
}

func (s *Server) getBucketVersioning(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	// Versioning state is not yet surfaced through ObjectLayer; report the
	// unconfigured state (empty body) until the bucket-config API lands.
	writeXML(w, requestID, http.StatusOK, versioningConfiguration{})
}

func (s *Server) putBucketVersioning(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	var cfg versioningConfiguration
	if err := xml.NewDecoder(r.Body).Decode(&cfg); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	if cfg.Status != "Enabled" && cfg.Status != "Suspended" {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	// MakeBucket already created the bucket; enabling versioning end-to-end waits
	// on the bucket-config subsystem. Accept and ack the well-formed request.
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- listing ---------------------------------------------------------------

func (s *Server) listObjectsV2(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q := r.URL.Query()
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxKeys = n
		}
	}
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	token := q.Get("continuation-token")
	startAfter := q.Get("start-after")

	res, err := s.layer.ListObjectsV2(r.Context(), bucket, prefix, token, startAfter, delim, maxKeys, false)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	out := listBucketV2Result{
		XMLNS:                 s3XMLNS,
		Name:                  bucket,
		Prefix:                prefix,
		StartAfter:            startAfter,
		ContinuationToken:     token,
		NextContinuationToken: res.NextContinuationToken,
		MaxKeys:               maxKeys,
		Delimiter:             delim,
		IsTruncated:           res.IsTruncated,
	}
	for _, o := range res.Objects {
		out.Contents = append(out.Contents, objectEntry{
			Key:          o.Name,
			LastModified: amzTime(o.ModTime),
			ETag:         quoteETag(o.ETag),
			Size:         o.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, p := range res.Prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: p})
	}
	out.KeyCount = len(out.Contents) + len(out.CommonPrefixes)
	writeXML(w, requestID, http.StatusOK, out)
}

// --- objects ---------------------------------------------------------------

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	size := r.ContentLength
	opts := object.ObjectOptions{
		ContentType: r.Header.Get("Content-Type"),
		UserDefined: userMetaFromHeader(r),
	}
	info, err := s.layer.PutObject(r.Context(), bucket, object2, object.NewPutReader(r.Body, size), opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.Header().Set("ETag", quoteETag(info.ETag))
	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	gr, err := s.layer.GetObject(r.Context(), bucket, object2, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	defer func() { _ = gr.Close() }()
	writeObjectHeaders(w, gr.ObjectInfo)
	w.WriteHeader(http.StatusOK)
	_, _ = io.Copy(w, gr)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	info, err := s.layer.GetObjectInfo(r.Context(), bucket, object2, opts)
	if err != nil {
		w.WriteHeader(toAPIError(err).HTTPStatus)
		return
	}
	writeObjectHeaders(w, info)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) deleteObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	info, err := s.layer.DeleteObject(r.Context(), bucket, object2, opts)
	// S3 DELETE is idempotent: deleting a key that does not exist still succeeds.
	if err != nil && !errors.Is(err, object.ErrObjectNotFound) {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	if info.DeleteMarker {
		w.Header().Set("x-amz-delete-marker", "true")
		if info.VersionID != "" {
			w.Header().Set("x-amz-version-id", info.VersionID)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteObjects(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	var req deleteRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	targets := make([]object.ObjectToDelete, len(req.Objects))
	for i, o := range req.Objects {
		targets[i] = object.ObjectToDelete{Name: o.Key, VersionID: o.VersionID}
	}
	deleted, errs := s.layer.DeleteObjects(r.Context(), bucket, targets, object.ObjectOptions{})

	out := deleteResult{XMLNS: s3XMLNS}
	for i := range targets {
		// A missing key is an idempotent success, not a per-key error (S3 semantics).
		if errs[i] != nil && !errors.Is(errs[i], object.ErrObjectNotFound) {
			ae := toAPIError(errs[i])
			out.Errors = append(out.Errors, deleteErrorXML{Key: targets[i].Name, Code: ae.Code, Message: ae.Description})
			continue
		}
		if req.Quiet && !deleted[i].DeleteMarker {
			continue
		}
		out.Deleted = append(out.Deleted, deletedEntry{
			Key:                   targets[i].Name,
			VersionID:             deleted[i].VersionID,
			DeleteMarker:          deleted[i].DeleteMarker,
			DeleteMarkerVersionID: deleted[i].DeleteMarkerVersionID,
		})
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// --- helpers ---------------------------------------------------------------

// quoteETag wraps a bare hex ETag in the double quotes S3 sends on the wire,
// leaving an already-quoted value untouched.
func quoteETag(etag string) string {
	if etag == "" {
		return etag
	}
	if strings.HasPrefix(etag, "\"") {
		return etag
	}
	return "\"" + etag + "\""
}

// writeObjectHeaders stamps the standard object response headers shared by GET
// and HEAD.
func writeObjectHeaders(w http.ResponseWriter, info object.ObjectInfo) {
	w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	if info.ContentType != "" {
		w.Header().Set("Content-Type", info.ContentType)
	} else {
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	for k, v := range info.UserDefined {
		if strings.HasPrefix(k, "x-amz-meta-") {
			w.Header().Set(k, v)
		}
	}
}

// userMetaFromHeader collects x-amz-meta-* headers and content-type into the
// UserDefined map the object layer persists.
func userMetaFromHeader(r *http.Request) map[string]string {
	m := map[string]string{}
	for k, vals := range r.Header {
		lk := strings.ToLower(k)
		if strings.HasPrefix(lk, "x-amz-meta-") && len(vals) > 0 {
			m[lk] = vals[0]
		}
	}
	if ct := r.Header.Get("Content-Type"); ct != "" {
		m["content-type"] = ct
	}
	return m
}
