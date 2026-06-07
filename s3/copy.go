// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/tamnd/liteio/object"
)

// Copy-related request headers.
const (
	copySourceHeader      = "x-amz-copy-source"
	copySourceRangeHeader = "x-amz-copy-source-range"
	metadataDirectiveStr  = "x-amz-metadata-directive"
)

// parseCopySource splits an x-amz-copy-source header into its bucket, key, and
// optional version id. The header is "/bucket/key" or "bucket/key" with the key
// URL-encoded and an optional "?versionId=..." suffix. The bucket is split off
// the first raw slash before the key is unescaped, so a key containing an encoded
// slash (%2F) is not mistaken for the bucket boundary.
func parseCopySource(h string) (bucket, key, versionID string, err error) {
	h = strings.TrimPrefix(h, "/")
	if rest, query, ok := strings.Cut(h, "?"); ok {
		h = rest
		if vals, perr := url.ParseQuery(query); perr == nil {
			versionID = vals.Get("versionId")
		}
	}
	b, rawKey, ok := strings.Cut(h, "/")
	if !ok || b == "" || rawKey == "" {
		return "", "", "", errInvalidCopySource
	}
	k, derr := url.QueryUnescape(rawKey)
	if derr != nil {
		return "", "", "", errInvalidCopySource
	}
	return b, k, versionID, nil
}

// copyObject handles PUT /bucket/key with an x-amz-copy-source header: it copies a
// source object onto the destination key. The metadata directive (COPY, the
// default, or REPLACE) decides whether the source's user metadata carries forward
// or the request's headers replace it.
func (s *Server) copyObject(w http.ResponseWriter, r *http.Request, requestID, dstBucket, dstObject, copySource string) {
	srcBucket, srcObject, srcVersion, err := parseCopySource(copySource)
	if err != nil {
		writeError(w, requestID, r.URL.Path, errInvalidCopySource)
		return
	}

	srcInfo, err := s.layer.GetObjectInfo(r.Context(), srcBucket, srcObject, object.ObjectOptions{VersionID: srcVersion})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	replace := strings.EqualFold(r.Header.Get(metadataDirectiveStr), "REPLACE")
	sameObject := srcBucket == dstBucket && srcObject == dstObject
	if sameObject && !replace {
		// Copying an object onto itself is only legal when the metadata changes.
		writeError(w, requestID, r.URL.Path, errInvalidCopyDest)
		return
	}

	opts := object.ObjectOptions{}
	if replace {
		opts.ContentType = r.Header.Get("Content-Type")
		opts.UserDefined = userMetaFromHeader(r)
	} else {
		opts.ContentType = srcInfo.ContentType
		opts.UserDefined = srcInfo.UserDefined
	}

	info, err := s.layer.CopyObject(r.Context(), srcBucket, srcObject, dstBucket, dstObject, srcInfo, opts)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	if info.VersionID != "" {
		w.Header().Set("x-amz-version-id", info.VersionID)
	}
	if srcVersion != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcVersion)
	}
	writeXML(w, requestID, http.StatusOK, copyObjectResult{
		XMLNS:        s3XMLNS,
		LastModified: amzTime(info.ModTime),
		ETag:         quoteETag(info.ETag),
	})
}

// uploadPartCopy handles PUT /bucket/key?partNumber=N&uploadId=... with an
// x-amz-copy-source header: it uploads a part whose bytes are read from a source
// object, optionally limited to the range named by x-amz-copy-source-range.
func (s *Server) uploadPartCopy(w http.ResponseWriter, r *http.Request, requestID, dstBucket, dstObject, uploadID, copySource string) {
	partNumber, err := strconv.Atoi(r.URL.Query().Get("partNumber"))
	if err != nil || partNumber < 1 {
		writeError(w, requestID, r.URL.Path, errInvalidArgument)
		return
	}
	srcBucket, srcObject, srcVersion, err := parseCopySource(copySource)
	if err != nil {
		writeError(w, requestID, r.URL.Path, errInvalidCopySource)
		return
	}

	srcInfo, err := s.layer.GetObjectInfo(r.Context(), srcBucket, srcObject, object.ObjectOptions{VersionID: srcVersion})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}

	var rng *object.HTTPRangeSpec
	if h := r.Header.Get(copySourceRangeHeader); h != "" {
		rng = parseRange(h)
		if rng == nil {
			writeError(w, requestID, r.URL.Path, errInvalidArgument)
			return
		}
	}

	part, err := s.layer.CopyObjectPart(r.Context(), srcBucket, srcObject, dstBucket, dstObject, uploadID, partNumber, srcInfo, rng, object.ObjectOptions{})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	if srcVersion != "" {
		w.Header().Set("x-amz-copy-source-version-id", srcVersion)
	}
	writeXML(w, requestID, http.StatusOK, copyPartResult{
		XMLNS:        s3XMLNS,
		LastModified: amzTime(part.LastModified),
		ETag:         quoteETag(part.ETag),
	})
}
