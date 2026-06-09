// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"net/http"

	"github.com/tamnd/liteio/object"
)

// putBucketEncryption handles PUT /bucket?encryption.
func (s *Server) putBucketEncryption(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	var req serverSideEncryptionConfigurationXML
	if !decodeXMLBody(w, r, requestID, &req) {
		return
	}
	if len(req.Rules) == 0 {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	rule := req.Rules[0].ApplyServerSideEncryptionByDefault
	cfg := object.BucketEncryptionConfig{
		Algorithm: rule.SSEAlgorithm,
		KMSKeyID:  rule.KMSMasterKeyID,
	}
	if err := s.layer.SetBucketEncryption(r.Context(), bucket, cfg); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketEncryption handles GET /bucket?encryption.
func (s *Server) getBucketEncryption(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	cfg, err := s.layer.GetBucketEncryption(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	resp := serverSideEncryptionConfigurationXML{
		XMLNS: s3XMLNS,
		Rules: []encryptionRuleXML{{
			ApplyServerSideEncryptionByDefault: applySSEXML{
				SSEAlgorithm:   cfg.Algorithm,
				KMSMasterKeyID: cfg.KMSKeyID,
			},
		}},
	}
	writeXML(w, requestID, http.StatusOK, resp)
}

// deleteBucketEncryption handles DELETE /bucket?encryption.
func (s *Server) deleteBucketEncryption(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketEncryption(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// parseSSES3Header inspects the x-amz-server-side-encryption header.
//   - "AES256" sets opts.SSES3.
//   - "aws:kms" sets opts.SSES3 and opts.SSEKMSKeyID (from the optional
//     x-amz-server-side-encryption-aws:kms-key-id header, or empty for the
//     bucket/server default key).
//
// Other values or absence leave opts unchanged.
func parseSSES3Header(r *http.Request, opts *object.ObjectOptions) {
	switch r.Header.Get("x-amz-server-side-encryption") {
	case "AES256":
		opts.SSES3 = true
	case "aws:kms":
		opts.SSES3 = true
		opts.SSEKMSKeyID = r.Header.Get("x-amz-server-side-encryption-aws:kms-key-id")
	}
}

// setSSES3ResponseHeaders echoes the SSE algorithm header that was stored in
// object metadata. It handles both SSE-S3 ("AES256") and SSE-KMS ("aws:kms").
func setSSES3ResponseHeaders(w http.ResponseWriter, meta map[string]string) {
	alg, ok := meta["x-amz-server-side-encryption"]
	if !ok {
		return
	}
	switch alg {
	case "AES256":
		w.Header().Set("x-amz-server-side-encryption", "AES256")
	case "aws:kms":
		w.Header().Set("x-amz-server-side-encryption", "aws:kms")
		if keyID, has := meta["x-amz-server-side-encryption-aws:kms-key-id"]; has && keyID != "" {
			w.Header().Set("x-amz-server-side-encryption-aws:kms-key-id", keyID)
		}
	}
}

// serverSideEncryptionConfigurationXML is the S3 wire type for
// GET/PUT /bucket?encryption.
type serverSideEncryptionConfigurationXML struct {
	XMLName xml.Name            `xml:"ServerSideEncryptionConfiguration"`
	XMLNS   string              `xml:"xmlns,attr,omitempty"`
	Rules   []encryptionRuleXML `xml:"Rule"`
}

type encryptionRuleXML struct {
	ApplyServerSideEncryptionByDefault applySSEXML `xml:"ApplyServerSideEncryptionByDefault"`
}

type applySSEXML struct {
	SSEAlgorithm   string `xml:"SSEAlgorithm"`
	KMSMasterKeyID string `xml:"KMSMasterKeyID,omitempty"`
}
