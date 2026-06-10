// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"net/http"
	"strconv"
	"sync"
)

// listBufPool reuses the large byte buffers needed to serialize LIST responses.
// A LIST-1000 response is ~200–250 KB; pooling eliminates one big allocation and
// the GC pressure it causes at high request rates.
var listBufPool = sync.Pool{
	New: func() any {
		b := &bytes.Buffer{}
		b.Grow(256 * 1024)
		return b
	},
}

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

// writeListBucketV2 is a hand-written XML encoder for the ListObjectsV2
// (ListBucketResult) response. It is ~10x faster than encoding/xml for lists
// with hundreds of objects because it avoids reflection and intermediate
// allocations.
func writeListBucketV2(w http.ResponseWriter, requestID string, out listBucketV2Result) {
	b := listBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer listBufPool.Put(b)
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	xmlElem(b, "Name", out.Name)
	xmlElem(b, "Prefix", out.Prefix)
	if out.StartAfter != "" {
		xmlElem(b, "StartAfter", out.StartAfter)
	}
	if out.ContinuationToken != "" {
		xmlElem(b, "ContinuationToken", out.ContinuationToken)
	}
	if out.NextContinuationToken != "" {
		xmlElem(b, "NextContinuationToken", out.NextContinuationToken)
	}
	b.WriteString("<KeyCount>")
	b.WriteString(strconv.Itoa(out.KeyCount))
	b.WriteString("</KeyCount>")
	b.WriteString("<MaxKeys>")
	b.WriteString(strconv.Itoa(out.MaxKeys))
	b.WriteString("</MaxKeys>")
	if out.Delimiter != "" {
		xmlElem(b, "Delimiter", out.Delimiter)
	}
	if out.IsTruncated {
		b.WriteString("<IsTruncated>true</IsTruncated>")
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	var sizeBuf [20]byte
	for i := range out.Contents {
		o := &out.Contents[i]
		b.WriteString("<Contents>")
		xmlElem(b, "Key", o.Key)
		xmlElem(b, "LastModified", o.LastModified)
		xmlElem(b, "ETag", xmlEscape(o.ETag))
		b.WriteString("<Size>")
		b.Write(strconv.AppendInt(sizeBuf[:0], o.Size, 10))
		b.WriteString("</Size>")
		b.WriteString("<StorageClass>STANDARD</StorageClass>")
		b.WriteString("</Contents>")
	}
	for _, p := range out.CommonPrefixes {
		b.WriteString("<CommonPrefixes><Prefix>")
		b.WriteString(xmlEscape(p.Prefix))
		b.WriteString("</Prefix></CommonPrefixes>")
	}
	b.WriteString("</ListBucketResult>")

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

// writeListBucketV1 is a hand-written XML encoder for the ListObjects (v1)
// response. Avoids encoding/xml reflection overhead for large listings.
func writeListBucketV1(w http.ResponseWriter, requestID string, out listBucketV1Result) {
	b := listBufPool.Get().(*bytes.Buffer)
	b.Reset()
	defer listBufPool.Put(b)
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>`)
	b.WriteString(`<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/">`)
	xmlElem(b, "Name", out.Name)
	xmlElem(b, "Prefix", out.Prefix)
	xmlElem(b, "Marker", out.Marker)
	if out.NextMarker != "" {
		xmlElem(b, "NextMarker", out.NextMarker)
	}
	b.WriteString("<MaxKeys>")
	b.WriteString(strconv.Itoa(out.MaxKeys))
	b.WriteString("</MaxKeys>")
	if out.Delimiter != "" {
		xmlElem(b, "Delimiter", out.Delimiter)
	}
	if out.IsTruncated {
		b.WriteString("<IsTruncated>true</IsTruncated>")
	} else {
		b.WriteString("<IsTruncated>false</IsTruncated>")
	}
	var sizeBuf [20]byte
	for i := range out.Contents {
		o := &out.Contents[i]
		b.WriteString("<Contents>")
		xmlElem(b, "Key", o.Key)
		xmlElem(b, "LastModified", o.LastModified)
		xmlElem(b, "ETag", xmlEscape(o.ETag))
		b.WriteString("<Size>")
		b.Write(strconv.AppendInt(sizeBuf[:0], o.Size, 10))
		b.WriteString("</Size>")
		b.WriteString("<StorageClass>STANDARD</StorageClass>")
		b.WriteString("</Contents>")
	}
	for _, p := range out.CommonPrefixes {
		b.WriteString("<CommonPrefixes><Prefix>")
		b.WriteString(xmlEscape(p.Prefix))
		b.WriteString("</Prefix></CommonPrefixes>")
	}
	b.WriteString("</ListBucketResult>")

	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(b.Bytes())
}

// xmlElem writes <tag>val</tag> escaping val.
func xmlElem(b *bytes.Buffer, tag, val string) {
	b.WriteByte('<')
	b.WriteString(tag)
	b.WriteByte('>')
	b.WriteString(xmlEscape(val))
	b.WriteString("</")
	b.WriteString(tag)
	b.WriteByte('>')
}

// xmlEscape escapes the three XML special characters that must be escaped in
// element content (&, <, >). The quote characters (" and ') only need escaping
// in attribute values; skipping them here avoids a slow-path allocation for
// every S3 ETag (which is always a quoted string like "abc123...").
func xmlEscape(s string) string {
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '&', '<', '>':
			var b bytes.Buffer
			b.Grow(len(s) + 8)
			for j := 0; j < len(s); j++ {
				switch s[j] {
				case '&':
					b.WriteString("&amp;")
				case '<':
					b.WriteString("&lt;")
				case '>':
					b.WriteString("&gt;")
				default:
					b.WriteByte(s[j])
				}
			}
			return b.String()
		}
	}
	return s
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
