// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"errors"
	"net/http"
	"strconv"

	"github.com/tamnd/liteio/object"
)

// restoreObject handles POST /bucket/key?restore. It triggers a RestoreObject
// operation for a tiered (transitioned) object. The optional Days element in the
// body sets the local-copy expiry; when absent a default of 1 day is used.
func (s *Server) restoreObject(w http.ResponseWriter, r *http.Request, requestID, bucket, key string) {
	days := 1
	if r.ContentLength > 0 {
		var req restoreRequestXML
		if err := xml.NewDecoder(r.Body).Decode(&req); err == nil && req.Days > 0 {
			days = req.Days
		}
	}
	versionID := r.URL.Query().Get("versionId")
	if err := s.layer.RestoreObject(r.Context(), bucket, key, versionID, days); err != nil {
		switch {
		case errors.Is(err, object.ErrBucketNotFound):
			writeError(w, requestID, r.URL.Path, errNoSuchBucket)
		case errors.Is(err, object.ErrObjectNotFound):
			writeError(w, requestID, r.URL.Path, errNoSuchKey)
		default:
			writeError(w, requestID, r.URL.Path, toAPIError(err))
		}
		return
	}
	// 202 Accepted: restore queued (S3 semantics — restore is asynchronous).
	w.WriteHeader(http.StatusAccepted)
}

// --- XML shapes ---------------------------------------------------------

// restoreRequestXML is the optional body of a POST ?restore request.
type restoreRequestXML struct {
	XMLName xml.Name `xml:"RestoreRequest"`
	Days    int      `xml:"Days"`
}

// restoreStatusHeader returns the x-amz-restore header value for an object's
// restore state. An empty string means the object is not tiered.
func restoreStatusHeader(tier string, ongoing bool, expiry string) string {
	if tier == "" {
		return ""
	}
	if ongoing {
		return `ongoing-request="true"`
	}
	if expiry != "" {
		return `ongoing-request="false", expiry-date="` + expiry + `"`
	}
	return ""
}

// storageClassHeader returns the x-amz-storage-class header for a tiered object.
// For locally-stored objects it returns the empty string (no header set).
func storageClassHeader(tierName string) string {
	if tierName == "" {
		return ""
	}
	return tierName
}

// tierDaysHeader formats a RestoreDays count for the x-liteio-restore-days header
// (informational, non-standard).
func tierDaysHeader(days int) string {
	if days <= 0 {
		return ""
	}
	return strconv.Itoa(days)
}
