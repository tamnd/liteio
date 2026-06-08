// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"crypto/md5"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tamnd/liteio/auth"
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
	cfg, err := s.layer.GetBucketVersioning(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	out := versioningConfiguration{XMLNS: s3XMLNS}
	switch {
	case cfg.Enabled:
		out.Status = "Enabled"
	case cfg.Suspended:
		out.Status = "Suspended"
	}
	writeXML(w, requestID, http.StatusOK, out)
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
	vc := object.VersioningConfig{
		Enabled:   cfg.Status == "Enabled",
		Suspended: cfg.Status == "Suspended",
	}
	if err := s.layer.SetBucketVersioning(r.Context(), bucket, vc); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// --- bucket policy ----------------------------------------------------------

// maxBucketPolicySize caps the policy document a client may upload, matching the
// 20 KB limit S3 enforces and bounding the read before it touches storage.
const maxBucketPolicySize = 20 * 1024

func (s *Server) getBucketPolicy(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	doc, err := s.layer.GetBucketPolicy(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	// The stored document is the canonical JSON the client gave us; return it
	// verbatim rather than re-marshaling, so round-tripping is byte-exact.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(doc)
}

func (s *Server) putBucketPolicy(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	// A bucket policy can only be set on a bucket that exists; check first so a
	// policy for a missing bucket is NoSuchBucket, not a confusing parse error.
	if _, err := s.layer.GetBucketInfo(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	doc, err := io.ReadAll(io.LimitReader(r.Body, maxBucketPolicySize+1))
	if err != nil {
		writeError(w, requestID, r.URL.Path, errInvalidRequest)
		return
	}
	if len(doc) > maxBucketPolicySize {
		writeError(w, requestID, r.URL.Path, errMalformedPolicy)
		return
	}
	// Validate the document and its Principal clause before persisting; an invalid
	// policy never reaches storage. The parsed form is discarded — the object layer
	// stores the bytes — but parsing is what rejects a malformed policy.
	if _, err := auth.ParseBucketPolicy(doc); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedPolicy)
		return
	}
	if err := s.layer.SetBucketPolicy(r.Context(), bucket, doc); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) deleteBucketPolicy(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketPolicy(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// listObjectVersions handles GET /bucket?versions: it lists every version and
// delete marker of the objects in a bucket, newest-first within each key.
func (s *Server) listObjectVersions(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q := r.URL.Query()
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxKeys = n
		}
	}
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	keyMarker := q.Get("key-marker")
	versionIDMarker := q.Get("version-id-marker")

	res, err := s.layer.ListObjectVersions(r.Context(), bucket, prefix, keyMarker, versionIDMarker, delim, maxKeys)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	out := listVersionsResult{
		XMLNS:               s3XMLNS,
		Name:                bucket,
		Prefix:              prefix,
		KeyMarker:           keyMarker,
		VersionIDMarker:     versionIDMarker,
		NextKeyMarker:       res.NextKeyMarker,
		NextVersionIDMarker: res.NextVersionIDMarker,
		MaxKeys:             maxKeys,
		Delimiter:           delim,
		IsTruncated:         res.IsTruncated,
	}
	for _, o := range res.Objects {
		versionID := o.VersionID
		if versionID == "" {
			versionID = "null"
		}
		if o.DeleteMarker {
			out.DeleteMarkers = append(out.DeleteMarkers, deleteMarkerXML{
				Key:          o.Name,
				VersionID:    versionID,
				IsLatest:     o.IsLatest,
				LastModified: amzTime(o.ModTime),
			})
			continue
		}
		out.Versions = append(out.Versions, versionEntryXML{
			Key:          o.Name,
			VersionID:    versionID,
			IsLatest:     o.IsLatest,
			LastModified: amzTime(o.ModTime),
			ETag:         quoteETag(o.ETag),
			Size:         o.Size,
			StorageClass: "STANDARD",
		})
	}
	for _, p := range res.Prefixes {
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefix{Prefix: p})
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// listObjectsV1 handles GET /bucket (without ?list-type=2). It maps the v1
// ?marker query parameter to the V2 StartAfter, then renders the result in the
// ListBucketResult (v1) envelope. S3 v1 uses NextMarker instead of a
// continuation token, set to the last key when the result is truncated.
func (s *Server) listObjectsV1(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	q := r.URL.Query()
	maxKeys := 1000
	if v := q.Get("max-keys"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			maxKeys = n
		}
	}
	prefix := q.Get("prefix")
	delim := q.Get("delimiter")
	marker := q.Get("marker")

	// The v1 marker maps to v2's start-after: it names the key after which listing
	// begins (exclusive). The v2 token and fetch-owner are unused for v1.
	res, err := s.layer.ListObjectsV2(r.Context(), bucket, prefix, "", marker, delim, maxKeys, false)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	out := listBucketV1Result{
		XMLNS:       s3XMLNS,
		Name:        bucket,
		Prefix:      prefix,
		Marker:      marker,
		MaxKeys:     maxKeys,
		Delimiter:   delim,
		IsTruncated: res.IsTruncated,
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
		out.CommonPrefixes = append(out.CommonPrefixes, commonPrefixEntry{Prefix: p})
	}
	// NextMarker is set to the last key in the listing when the result is
	// truncated, so the caller knows where to continue.
	if res.IsTruncated && len(out.Contents) > 0 {
		out.NextMarker = out.Contents[len(out.Contents)-1].Key
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// getObjectAttributes handles GET /bucket/key?attributes. The
// x-amz-object-attributes header is a comma-separated list selecting which
// attribute groups to include: ETag, StorageClass, ObjectSize, ObjectParts.
func (s *Server) getObjectAttributes(w http.ResponseWriter, r *http.Request, requestID, bucket, obj string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	info, err := s.layer.GetObjectInfo(r.Context(), bucket, obj, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	attrs := make(map[string]bool)
	for _, a := range strings.Split(r.Header.Get("x-amz-object-attributes"), ",") {
		attrs[strings.TrimSpace(a)] = true
	}
	// If the header is absent or empty, return all attributes.
	all := len(attrs) == 0 || (len(attrs) == 1 && attrs[""])

	out := getObjectAttributesResponse{XMLNS: s3XMLNS}
	if all || attrs["ETag"] {
		out.ETag = strings.Trim(info.ETag, "\"")
	}
	if all || attrs["StorageClass"] {
		out.StorageClass = "STANDARD"
	}
	if all || attrs["ObjectSize"] {
		out.ObjectSize = info.Size
	}
	if (all || attrs["ObjectParts"]) && len(info.Parts) > 0 {
		parts := make([]objectPartAttr, 0, len(info.Parts))
		for _, p := range info.Parts {
			parts = append(parts, objectPartAttr{
				PartNumber: p.Number,
				Size:       p.Size,
			})
		}
		out.ObjectParts = &objectPartsResponse{
			TotalPartsCount: len(parts),
			Parts:           parts,
		}
	}
	writeXML(w, requestID, http.StatusOK, out)
}

// --- objects ---------------------------------------------------------------

func (s *Server) putObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	// Conditional write: If-None-Match: * means "fail if the object already exists."
	if r.Header.Get("If-None-Match") == "*" {
		_, err := s.layer.GetObjectInfo(r.Context(), bucket, object2, object.ObjectOptions{})
		if err == nil {
			// Object exists; precondition failed.
			writeError(w, requestID, r.URL.Path, errPreconditionFailed)
			return
		}
		// Any error other than not-found (bucket missing, quorum error, etc.) is a
		// real failure; only ErrObjectNotFound means the object is absent and the
		// write can proceed.
		if !errors.Is(err, object.ErrObjectNotFound) {
			s.fail(w, requestID, r.URL.Path, err)
			return
		}
	}

	size := r.ContentLength
	opts := object.ObjectOptions{
		ContentType:       r.Header.Get("Content-Type"),
		UserDefined:       userMetaFromHeader(r),
		SourceIP:          sourceIPFromRequest(r),
		ReplicationSource: isReplicationSource(r),
	}
	if !parseSSECKey(w, r, requestID, &opts) {
		return
	}
	parseSSES3Header(r, &opts)
	info, err := s.layer.PutObject(r.Context(), bucket, object2, object.NewPutReader(r.Body, size), opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	// Content-MD5 validation: if the client sent a base64-encoded MD5, compare it
	// against the hex ETag the object layer computed. A mismatch means the bytes
	// were corrupted in transit; delete the just-written object and return 400.
	if contentMD5 := r.Header.Get("Content-MD5"); contentMD5 != "" {
		rawMD5, decErr := base64.StdEncoding.DecodeString(contentMD5)
		if decErr != nil || len(rawMD5) != md5.Size {
			_, _ = s.layer.DeleteObject(r.Context(), bucket, object2, object.ObjectOptions{})
			writeError(w, requestID, r.URL.Path, errBadDigest)
			return
		}
		hexMD5 := hex.EncodeToString(rawMD5)
		// info.ETag may be quoted ("abc...") or bare (abc...); strip quotes.
		etag := strings.Trim(info.ETag, "\"")
		// For multipart objects the ETag contains a dash (etag-N); skip the check
		// because the ETag is not a plain MD5 in that case.
		if !strings.Contains(etag, "-") && etag != hexMD5 {
			_, _ = s.layer.DeleteObject(r.Context(), bucket, object2, object.ObjectOptions{})
			writeError(w, requestID, r.URL.Path, errBadDigest)
			return
		}
	}

	w.Header().Set("ETag", quoteETag(info.ETag))
	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	setSSECResponseHeaders(w, info.UserDefined)
	setSSES3ResponseHeaders(w, info.UserDefined)
	w.WriteHeader(http.StatusOK)
}

func (s *Server) getObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	if !parseSSECKey(w, r, requestID, &opts) {
		return
	}

	// Fetch metadata first so conditional headers and the Range can be resolved
	// before any bytes are read, exactly as S3 evaluates a GET.
	info, err := s.layer.GetObjectInfo(r.Context(), bucket, object2, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	if s.checkPreconditions(w, r, requestID, info) {
		return
	}

	rng := parseRange(r.Header.Get("Range"))
	start, length, rerr := rng.GetOffsetLength(info.Size)
	if rerr != nil {
		// An unsatisfiable range gets 416 with the object's full size echoed back.
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		writeError(w, requestID, r.URL.Path, errInvalidRange)
		return
	}

	opts.Range = rng
	gr, err := s.layer.GetObject(r.Context(), bucket, object2, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	defer func() { _ = gr.Close() }()

	writeObjectHeaders(w, info)
	setSSECResponseHeaders(w, info.UserDefined)
	setSSES3ResponseHeaders(w, info.UserDefined)
	s.addCORSHeaders(w, r, bucket)
	if rng != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", contentRange(start, length, info.Size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	_, _ = io.Copy(w, gr)
}

func (s *Server) headObject(w http.ResponseWriter, r *http.Request, requestID, bucket, object2 string) {
	opts := object.ObjectOptions{VersionID: r.URL.Query().Get("versionId")}
	if !parseSSECKey(w, r, requestID, &opts) {
		return
	}
	info, err := s.layer.GetObjectInfo(r.Context(), bucket, object2, opts)
	if err != nil {
		w.WriteHeader(toAPIError(err).HTTPStatus)
		return
	}
	setSSECResponseHeaders(w, info.UserDefined)
	setSSES3ResponseHeaders(w, info.UserDefined)
	if s.checkPreconditions(w, r, requestID, info) {
		return
	}

	rng := parseRange(r.Header.Get("Range"))
	start, length, rerr := rng.GetOffsetLength(info.Size)
	if rerr != nil {
		w.Header().Set("Content-Range", "bytes */"+strconv.FormatInt(info.Size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}

	writeObjectHeaders(w, info)
	s.addCORSHeaders(w, r, bucket)
	if rng != nil {
		w.Header().Set("Content-Length", strconv.FormatInt(length, 10))
		w.Header().Set("Content-Range", contentRange(start, length, info.Size))
		w.WriteHeader(http.StatusPartialContent)
		return
	}
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

// contentRange formats a Content-Range header value for a satisfied byte range:
// "bytes <start>-<end>/<total>".
func contentRange(start, length, total int64) string {
	return "bytes " + strconv.FormatInt(start, 10) + "-" +
		strconv.FormatInt(start+length-1, 10) + "/" + strconv.FormatInt(total, 10)
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

// userMetaFromHeader collects x-amz-meta-* headers, content-type, and the
// x-amz-tagging header (stored under its canonical key so Put/GetObjectTagging
// share the same storage path) into the UserDefined map the object layer persists.
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
	if tag := r.Header.Get("x-amz-tagging"); tag != "" {
		m["x-amz-tagging"] = tag
	}
	// Object Lock per-object headers: captured verbatim so the object layer can
	// apply them without the S3 layer needing to know the retention semantics.
	for _, k := range []string{
		"x-amz-object-lock-mode",
		"x-amz-object-lock-retain-until-date",
		"x-amz-object-lock-legal-hold",
	} {
		if v := r.Header.Get(k); v != "" {
			m[k] = v
		}
	}
	return m
}
