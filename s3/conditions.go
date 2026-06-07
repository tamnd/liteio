// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/tamnd/liteio/object"
)

// parseRange parses an HTTP Range header into an object.HTTPRangeSpec. It honors
// the byte-range forms S3 supports: "bytes=start-end", "bytes=start-" (open), and
// "bytes=-N" (suffix). A header that is absent, uses a non-byte unit, lists more
// than one range, or is otherwise malformed yields nil — S3 ignores such a header
// and returns the whole object. Whether a syntactically valid range is actually
// satisfiable is decided later by HTTPRangeSpec.GetOffsetLength against the size.
func parseRange(v string) *object.HTTPRangeSpec {
	const prefix = "bytes="
	if !strings.HasPrefix(v, prefix) {
		return nil
	}
	spec := strings.TrimPrefix(v, prefix)
	if strings.Contains(spec, ",") {
		return nil // multiple ranges are unsupported; serve the full object
	}
	startStr, endStr, ok := strings.Cut(spec, "-")
	if !ok {
		return nil
	}
	startStr, endStr = strings.TrimSpace(startStr), strings.TrimSpace(endStr)
	switch {
	case startStr == "" && endStr == "":
		return nil
	case startStr == "": // suffix: bytes=-N
		n, err := strconv.ParseInt(endStr, 10, 64)
		if err != nil {
			return nil
		}
		return &object.HTTPRangeSpec{IsSuffix: true, Start: n}
	case endStr == "": // open: bytes=start-
		start, err := strconv.ParseInt(startStr, 10, 64)
		if err != nil {
			return nil
		}
		return &object.HTTPRangeSpec{Start: start, End: -1}
	default:
		start, err1 := strconv.ParseInt(startStr, 10, 64)
		end, err2 := strconv.ParseInt(endStr, 10, 64)
		if err1 != nil || err2 != nil {
			return nil
		}
		return &object.HTTPRangeSpec{Start: start, End: end}
	}
}

// checkPreconditions evaluates the RFC 7232 conditional headers (If-Match,
// If-None-Match, If-Modified-Since, If-Unmodified-Since) against the object. When
// a precondition decides the response it writes it and returns true, so the
// caller stops; otherwise it returns false and the request proceeds.
//
// Precedence follows S3/RFC 7232: If-Match wins over If-Unmodified-Since, and
// If-None-Match wins over If-Modified-Since. A satisfied If-None-Match (or
// If-Modified-Since) on a GET/HEAD yields 304 Not Modified; a failed If-Match (or
// If-Unmodified-Since) yields 412 Precondition Failed.
func (s *Server) checkPreconditions(w http.ResponseWriter, r *http.Request, requestID string, info object.ObjectInfo) bool {
	etag := quoteETag(info.ETag)
	modTime := info.ModTime.UTC().Truncate(time.Second)

	if im := r.Header.Get("If-Match"); im != "" {
		if !etagMatches(im, etag) {
			writeError(w, requestID, r.URL.Path, errPreconditionFailed)
			return true
		}
	} else if ius := r.Header.Get("If-Unmodified-Since"); ius != "" {
		if t, err := http.ParseTime(ius); err == nil && modTime.After(t) {
			writeError(w, requestID, r.URL.Path, errPreconditionFailed)
			return true
		}
	}

	if inm := r.Header.Get("If-None-Match"); inm != "" {
		if etagMatches(inm, etag) {
			writeNotModified(w, info)
			return true
		}
	} else if ims := r.Header.Get("If-Modified-Since"); ims != "" {
		if t, err := http.ParseTime(ims); err == nil && !modTime.After(t) {
			writeNotModified(w, info)
			return true
		}
	}
	return false
}

// etagMatches reports whether the quoted etag satisfies an If-Match/If-None-Match
// header value: "*" matches any existing object, otherwise any comma-separated
// candidate must equal the object's ETag (weak-comparison prefixes are stripped).
func etagMatches(header, etag string) bool {
	header = strings.TrimSpace(header)
	if header == "*" {
		return true
	}
	for cand := range strings.SplitSeq(header, ",") {
		cand = strings.TrimSpace(cand)
		cand = strings.TrimPrefix(cand, "W/")
		if cand == etag {
			return true
		}
	}
	return false
}

// writeNotModified emits a 304 with the validators a cache needs, and no body.
func writeNotModified(w http.ResponseWriter, info object.ObjectInfo) {
	w.Header().Set("ETag", quoteETag(info.ETag))
	w.Header().Set("Last-Modified", info.ModTime.UTC().Format(http.TimeFormat))
	w.WriteHeader(http.StatusNotModified)
}
