// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"encoding/xml"
	"io"
	"net/http"
	"strings"

	"github.com/tamnd/liteio/object"
)

// selectRequest mirrors the SelectObjectContentRequest XML body.
type selectRequest struct {
	XMLName              xml.Name             `xml:"SelectObjectContentRequest"`
	Expression           string               `xml:"Expression"`
	ExpressionType       string               `xml:"ExpressionType"`
	InputSerialization   selectInputSerial    `xml:"InputSerialization"`
	OutputSerialization  selectOutputSerial   `xml:"OutputSerialization"`
}

type selectInputSerial struct {
	CSV  *selectCSVInput  `xml:"CSV"`
	JSON *selectJSONInput `xml:"JSON"`
}

type selectCSVInput struct {
	FileHeaderInfo  string `xml:"FileHeaderInfo"`  // USE | IGNORE | NONE
	RecordDelimiter string `xml:"RecordDelimiter"`
	FieldDelimiter  string `xml:"FieldDelimiter"`
	QuoteCharacter  string `xml:"QuoteCharacter"`
	EscapeCharacter string `xml:"EscapeCharacter"`
}

type selectJSONInput struct {
	Type string `xml:"Type"` // DOCUMENT | LINES
}

type selectOutputSerial struct {
	CSV  *selectCSVOutput  `xml:"CSV"`
	JSON *selectJSONOutput `xml:"JSON"`
}

type selectCSVOutput struct {
	RecordDelimiter string `xml:"RecordDelimiter"`
	FieldDelimiter  string `xml:"FieldDelimiter"`
	QuoteCharacter  string `xml:"QuoteCharacter"`
}

type selectJSONOutput struct {
	RecordDelimiter string `xml:"RecordDelimiter"`
}

// selectObjectContent handles POST /bucket/key?select&select-type=2.
// It streams the query results as an AWS binary event stream.
func (s *Server) selectObjectContent(w http.ResponseWriter, r *http.Request, requestID, bucket, key string) {
	var req selectRequest
	if err := xml.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil {
		writeError(w, requestID, r.URL.Path, errMalformedXML)
		return
	}
	if !strings.EqualFold(req.ExpressionType, "SQL") {
		writeError(w, requestID, r.URL.Path, errInvalidArgument)
		return
	}
	if req.Expression == "" {
		writeError(w, requestID, r.URL.Path, errInvalidArgument)
		return
	}

	gr, err := s.layer.GetObject(r.Context(), bucket, key, object.ObjectOptions{})
	if err != nil {
		s.fail(w, requestID, r.URL.Path, err)
		return
	}
	defer func() { _ = gr.Close() }()

	// Set up streaming response headers before writing any bytes.
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)

	var (
		bytesScanned  int64
		bytesReturned int64
	)

	emit := func(line []byte) error {
		if err2 := writeRecordsEvent(w, line); err2 != nil {
			return err2
		}
		if canFlush {
			flusher.Flush()
		}
		bytesReturned += int64(len(line))
		return nil
	}

	switch {
	case req.InputSerialization.CSV != nil:
		in := req.InputSerialization.CSV
		inCfg := csvInputCfg{
			FileHeaderInfo:  in.FileHeaderInfo,
			RecordDelimiter: in.RecordDelimiter,
			FieldDelimiter:  in.FieldDelimiter,
			QuoteCharacter:  in.QuoteCharacter,
			EscapeCharacter: in.EscapeCharacter,
		}
		outCfg := csvOutputCfg{}
		outJSON := false
		if req.OutputSerialization.CSV != nil {
			oc := req.OutputSerialization.CSV
			outCfg.RecordDelimiter = oc.RecordDelimiter
			outCfg.FieldDelimiter = oc.FieldDelimiter
			outCfg.QuoteCharacter = oc.QuoteCharacter
		} else if req.OutputSerialization.JSON != nil {
			outJSON = true
			if req.OutputSerialization.JSON.RecordDelimiter != "" {
				outCfg.RecordDelimiter = req.OutputSerialization.JSON.RecordDelimiter
			}
		}
		parsed := parseSelectExpr(req.Expression)
		parsed.outJSON = outJSON
		scanned, ret, qerr := queryCSV(gr, inCfg, outCfg, req.Expression, emit)
		if qerr != nil {
			_ = qerr // response already started; best-effort
		}
		bytesScanned = scanned
		_ = ret
		_ = parsed

	case req.InputSerialization.JSON != nil:
		in := req.InputSerialization.JSON
		inCfg := jsonInputCfg{Type: in.Type}
		outCfg := jsonOutputCfg{}
		if req.OutputSerialization.JSON != nil {
			outCfg.RecordDelimiter = req.OutputSerialization.JSON.RecordDelimiter
		}
		scanned, _, qerr := queryJSON(gr, inCfg, outCfg, req.Expression, emit)
		if qerr != nil {
			_ = qerr
		}
		bytesScanned = scanned

	default:
		// No recognized input serialization.
		writeError(w, requestID, r.URL.Path, errInvalidArgument)
		return
	}

	_ = writeStatsEvent(w, bytesScanned, bytesScanned, bytesReturned)
	_ = writeEndEvent(w)
	if canFlush {
		flusher.Flush()
	}
}
