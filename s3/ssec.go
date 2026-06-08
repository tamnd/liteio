// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"net/http"

	"github.com/tamnd/liteio/object"
	"github.com/tamnd/liteio/ssec"
)

// parseSSECKey parses the SSE-C customer key from the request headers and, if
// present, injects it into opts.SSECKey. It writes an S3 error and returns false
// when the headers are malformed.
func parseSSECKey(w http.ResponseWriter, r *http.Request, requestID string, opts *object.ObjectOptions) (ok bool) {
	key, _, present, err := ssec.ParseKey(r)
	if err != nil {
		writeError(w, requestID, r.URL.Path, errSSECBadRequest)
		return false
	}
	if present {
		opts.SSECKey = &key
	}
	return true
}

// setSSECResponseHeaders adds the SSE-C algorithm and key-MD5 headers to the
// response when the object was stored with SSE-C. They are read from the
// object's metadata; the response never echoes the key itself.
func setSSECResponseHeaders(w http.ResponseWriter, meta map[string]string) {
	if md5, ok := meta[ssec.MetaKeyMD5]; ok {
		w.Header().Set("x-amz-server-side-encryption-customer-algorithm", ssec.Algorithm)
		w.Header().Set("x-amz-server-side-encryption-customer-key-md5", md5)
	}
}
