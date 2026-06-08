// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"net/http"
)

// newRequestID returns a random hex token used for x-amz-request-id. It is opaque
// to clients and only needs to be unique enough to correlate logs.
func newRequestID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// setCommonHeaders stamps the trace headers every S3 response carries.
func setCommonHeaders(w http.ResponseWriter, requestID string) {
	w.Header().Set("x-amz-request-id", requestID)
	w.Header().Set("x-amz-id-2", requestID)
	w.Header().Set("Server", "liteio")
	w.Header().Set("Accept-Ranges", "bytes")
}

// writeXML marshals v as an S3 XML body with the declaration prepended and the
// given status. A marshal failure degrades to a 500 InternalError.
func writeXML(w http.ResponseWriter, requestID string, status int, v any) {
	body, err := xml.Marshal(v)
	if err != nil {
		writeError(w, requestID, "", errInternalError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}

// writeError renders the S3 error envelope with the catalog status. resource is
// the request path the error refers to.
func writeError(w http.ResponseWriter, requestID, resource string, ae APIError) {
	// Report the S3 error code to the metrics recorder, if one is wrapping the
	// response, so the front door counts errors by code without parsing the body.
	if cr, ok := w.(codeRecorder); ok {
		cr.recordError(ae.Code)
	}
	resp := errorResponse{
		Code:      ae.Code,
		Message:   ae.Description,
		Resource:  resource,
		RequestID: requestID,
		HostID:    requestID,
	}
	body, err := xml.Marshal(resp)
	if err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	status := ae.HTTPStatus
	if status == 0 {
		status = http.StatusInternalServerError
	}
	w.WriteHeader(status)
	_, _ = w.Write([]byte(xml.Header))
	_, _ = w.Write(body)
}
