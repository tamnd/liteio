// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/tamnd/liteio/object"
)

const maxCORSBody = 64 << 10 // 64 KiB

// --- XML wire types ----------------------------------------------------------

type corsConfigurationXML struct {
	XMLName xml.Name      `xml:"CORSConfiguration"`
	XMLNS   string        `xml:"xmlns,attr,omitempty"`
	Rules   []corsRuleXML `xml:"CORSRule"`
}

type corsRuleXML struct {
	ID            string   `xml:"ID,omitempty"`
	AllowedOrigin []string `xml:"AllowedOrigin"`
	AllowedMethod []string `xml:"AllowedMethod"`
	AllowedHeader []string `xml:"AllowedHeader,omitempty"`
	ExposeHeader  []string `xml:"ExposeHeader,omitempty"`
	MaxAgeSeconds int      `xml:"MaxAgeSeconds,omitempty"`
}

// validHTTPMethods is the set of upper-case HTTP verbs S3 considers legal for
// CORS AllowedMethod fields.
var validHTTPMethods = map[string]bool{
	"GET": true, "PUT": true, "POST": true, "DELETE": true,
	"HEAD": true, "OPTIONS": true, "PATCH": true,
}

// --- handlers ----------------------------------------------------------------

// putBucketCors handles PUT /bucket?cors.
func (s *Server) putBucketCors(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	body, err := io.ReadAll(io.LimitReader(r.Body, maxCORSBody+1))
	if err != nil || len(body) > maxCORSBody {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	if len(body) == 0 {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	var xcfg corsConfigurationXML
	if err := xml.Unmarshal(body, &xcfg); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	// Validate AllowedMethods.
	for _, rule := range xcfg.Rules {
		for _, m := range rule.AllowedMethod {
			if !validHTTPMethods[strings.ToUpper(m)] {
				writeError(w, requestID, r.URL.Path, errInvalidRequest)
				return
			}
		}
	}
	cfg := corsConfigFromXML(xcfg)
	if err := s.layer.SetBucketCORS(r.Context(), bucket, cfg); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// getBucketCors handles GET /bucket?cors.
func (s *Server) getBucketCors(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	cfg, err := s.layer.GetBucketCORS(r.Context(), bucket)
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	writeXML(w, requestID, http.StatusOK, corsConfigToXML(cfg))
}

// deleteBucketCors handles DELETE /bucket?cors.
func (s *Server) deleteBucketCors(w http.ResponseWriter, r *http.Request, requestID, bucket string) {
	if err := s.layer.DeleteBucketCORS(r.Context(), bucket); err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- CORS preflight and response headers -------------------------------------

// handleCORSPreflight handles OPTIONS requests before SigV4 verification. It
// parses the bucket from the path, fetches the CORS config, and checks whether
// the request's origin and requested method are permitted. On a match it writes
// 200 with the relevant Access-Control-* headers; on a miss it writes 403.
func (s *Server) handleCORSPreflight(w http.ResponseWriter, r *http.Request) {
	res := s.parseResource(r)
	if res.bucket == "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	cfg, err := s.layer.GetBucketCORS(r.Context(), res.bucket)
	if err != nil {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	origin := r.Header.Get("Origin")
	method := r.Header.Get("Access-Control-Request-Method")
	rule, ok := corsAllowed(cfg, origin, method)
	if !ok {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	writeCORSHeaders(w, origin, rule)
	w.WriteHeader(http.StatusOK)
}

// addCORSHeaders looks up the CORS config for bucket and, if a rule matches the
// request's Origin, appends the appropriate Access-Control-* response headers.
// It is a best-effort decoration: if the config cannot be read (no config, bucket
// missing, etc.) it does nothing and leaves the main response unaffected.
func (s *Server) addCORSHeaders(w http.ResponseWriter, r *http.Request, bucket string) {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return
	}
	cfg, err := s.layer.GetBucketCORS(r.Context(), bucket)
	if err != nil {
		return
	}
	rule, ok := corsAllowed(cfg, origin, r.Method)
	if !ok {
		return
	}
	writeCORSHeaders(w, origin, rule)
}

// writeCORSHeaders stamps the Access-Control-* response headers for a matched
// CORS rule.
func writeCORSHeaders(w http.ResponseWriter, origin string, rule object.CORSRule) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", origin)
	h.Add("Vary", "Origin")
	if len(rule.AllowedMethods) > 0 {
		h.Set("Access-Control-Allow-Methods", strings.Join(rule.AllowedMethods, ", "))
	}
	if len(rule.AllowedHeaders) > 0 {
		h.Set("Access-Control-Allow-Headers", strings.Join(rule.AllowedHeaders, ", "))
	}
	if len(rule.ExposeHeaders) > 0 {
		h.Set("Access-Control-Expose-Headers", strings.Join(rule.ExposeHeaders, ", "))
	}
	if rule.MaxAgeSeconds > 0 {
		h.Set("Access-Control-Max-Age", strconv.Itoa(rule.MaxAgeSeconds))
	}
}

// corsAllowed returns the first rule whose AllowedOrigins covers origin and
// whose AllowedMethods covers method. A "*" in AllowedOrigins matches any
// origin. The method comparison is case-insensitive.
func corsAllowed(cfg object.CORSConfig, origin, method string) (object.CORSRule, bool) {
	upperMethod := strings.ToUpper(method)
	for _, rule := range cfg.Rules {
		if !originMatches(rule.AllowedOrigins, origin) {
			continue
		}
		for _, m := range rule.AllowedMethods {
			if strings.ToUpper(m) == upperMethod {
				return rule, true
			}
		}
	}
	return object.CORSRule{}, false
}

// originMatches reports whether any entry in allowed matches origin. A single
// "*" entry matches any non-empty origin.
func originMatches(allowed []string, origin string) bool {
	if origin == "" {
		return false
	}
	for _, a := range allowed {
		if a == "*" || a == origin {
			return true
		}
	}
	return false
}

// --- XML conversion helpers --------------------------------------------------

func corsConfigFromXML(x corsConfigurationXML) object.CORSConfig {
	cfg := object.CORSConfig{Rules: make([]object.CORSRule, 0, len(x.Rules))}
	for _, r := range x.Rules {
		cfg.Rules = append(cfg.Rules, object.CORSRule{
			ID:             r.ID,
			AllowedOrigins: r.AllowedOrigin,
			AllowedMethods: r.AllowedMethod,
			AllowedHeaders: r.AllowedHeader,
			ExposeHeaders:  r.ExposeHeader,
			MaxAgeSeconds:  r.MaxAgeSeconds,
		})
	}
	return cfg
}

func corsConfigToXML(cfg object.CORSConfig) corsConfigurationXML {
	x := corsConfigurationXML{XMLNS: s3XMLNS, Rules: make([]corsRuleXML, 0, len(cfg.Rules))}
	for _, r := range cfg.Rules {
		x.Rules = append(x.Rules, corsRuleXML{
			ID:            r.ID,
			AllowedOrigin: r.AllowedOrigins,
			AllowedMethod: r.AllowedMethods,
			AllowedHeader: r.AllowedHeaders,
			ExposeHeader:  r.ExposeHeaders,
			MaxAgeSeconds: r.MaxAgeSeconds,
		})
	}
	return x
}
