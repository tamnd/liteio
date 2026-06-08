// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/tamnd/liteio/auth"
)

// errorResponse is the JSON envelope every admin error travels in: a stable Code
// callers branch on and a human Message.
type errorResponse struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// writeJSON encodes v as the response body with the given status.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes a JSON error with the given status, code, and message.
func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorResponse{Code: code, Message: message})
}

// writeIAMError maps an auth store error to its HTTP status and code. A nil error
// is a programming mistake (callers check first) and maps to 500.
func writeIAMError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, auth.ErrExists):
		writeError(w, http.StatusConflict, "AlreadyExists", err.Error())
	case errors.Is(err, auth.ErrNotFound):
		writeError(w, http.StatusNotFound, "NotFound", err.Error())
	case errors.Is(err, auth.ErrUnknownPolicy):
		writeError(w, http.StatusBadRequest, "UnknownPolicy", err.Error())
	case errors.Is(err, auth.ErrInvalid):
		writeError(w, http.StatusBadRequest, "InvalidOperation", err.Error())
	default:
		writeError(w, http.StatusInternalServerError, "InternalError", "internal error")
	}
}

// decodeJSON reads the request body into v, capping it so a runaway body cannot
// exhaust memory. It returns false and writes a 400 on a malformed body.
func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	const maxBody = 1 << 20 // 1 MiB: IAM bodies are tiny; this is a generous ceiling
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeError(w, http.StatusBadRequest, "MalformedRequest", "request body is not valid JSON: "+err.Error())
		return false
	}
	return true
}
