// SPDX-License-Identifier: Apache-2.0

package s3

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"hash/crc32"
	"io"
	"net/http"
	"strings"
	"testing"
)

// decodeEventStream parses the binary event stream from r and returns all
// Records payloads concatenated.
func decodeEventStream(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	var records []byte
	pos := 0
	for pos < len(data) {
		if pos+12 > len(data) {
			break
		}
		total := int(binary.BigEndian.Uint32(data[pos:]))
		if total < 16 || pos+total > len(data) {
			break
		}
		hlen := int(binary.BigEndian.Uint32(data[pos+4:]))
		// preludeCRC at pos+8 (4 bytes) — skip
		msgStart := pos
		headersEnd := pos + 12 + hlen
		payloadEnd := pos + total - 4
		// message CRC at payloadEnd (4 bytes) — skip

		// Verify message CRC.
		wantCRC := binary.BigEndian.Uint32(data[payloadEnd:])
		gotCRC := crc32.ChecksumIEEE(data[msgStart:payloadEnd])
		if gotCRC != wantCRC {
			break
		}

		headers := data[pos+12 : headersEnd]
		payload := data[headersEnd:payloadEnd]

		// Extract :event-type header value.
		eventType := extractEventType(headers)
		if eventType == "Records" {
			records = append(records, payload...)
		}
		pos += total
	}
	return records, nil
}

// extractEventType scans the binary headers block for the :event-type value.
func extractEventType(headers []byte) string {
	pos := 0
	for pos < len(headers) {
		if pos >= len(headers) {
			break
		}
		nlen := int(headers[pos])
		pos++
		if pos+nlen > len(headers) {
			break
		}
		name := string(headers[pos : pos+nlen])
		pos += nlen
		if pos >= len(headers) {
			break
		}
		typ := headers[pos]
		pos++
		if pos+2 > len(headers) {
			break
		}
		vlen := int(binary.BigEndian.Uint16(headers[pos:]))
		pos += 2
		if pos+vlen > len(headers) {
			break
		}
		val := string(headers[pos : pos+vlen])
		pos += vlen
		_ = typ
		if name == ":event-type" {
			return val
		}
	}
	return ""
}

func TestSelectCSV(t *testing.T) {
	h := newHarness(t)

	// Create bucket and upload a CSV object.
	mustStatus(t, h.do(http.MethodPut, "/data", nil, nil), http.StatusOK)
	csv := []byte("name,age,city\nAlice,30,NYC\nBob,25,LA\nCarol,35,NYC\n")
	mustStatus(t, h.do(http.MethodPut, "/data/people.csv", csv, map[string]string{
		"Content-Type": "text/csv",
	}), http.StatusOK)

	t.Run("select_star", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <CSV>
      <FileHeaderInfo>USE</FileHeaderInfo>
    </CSV>
  </InputSerialization>
  <OutputSerialization>
    <CSV/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/data/people.csv?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		if res.status != http.StatusOK {
			t.Fatalf("status = %d, want 200; body=%s", res.status, res.body)
		}
		records, err := decodeEventStream(bytes.NewReader(res.body))
		if err != nil {
			t.Fatalf("decode event stream: %v", err)
		}
		if !strings.Contains(string(records), "Alice") {
			t.Errorf("records missing Alice: %q", records)
		}
		if !strings.Contains(string(records), "Bob") {
			t.Errorf("records missing Bob: %q", records)
		}
	})

	t.Run("select_where", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object WHERE city = 'NYC'</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <CSV>
      <FileHeaderInfo>USE</FileHeaderInfo>
    </CSV>
  </InputSerialization>
  <OutputSerialization>
    <CSV/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/data/people.csv?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		if strings.Contains(string(records), "Bob") {
			t.Errorf("WHERE city=NYC should exclude Bob; got: %q", records)
		}
		if !strings.Contains(string(records), "Alice") {
			t.Errorf("WHERE city=NYC should include Alice; got: %q", records)
		}
		if !strings.Contains(string(records), "Carol") {
			t.Errorf("WHERE city=NYC should include Carol; got: %q", records)
		}
	})

	t.Run("select_limit", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object LIMIT 1</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <CSV>
      <FileHeaderInfo>USE</FileHeaderInfo>
    </CSV>
  </InputSerialization>
  <OutputSerialization>
    <CSV/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/data/people.csv?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		lines := strings.Split(strings.TrimRight(string(records), "\n"), "\n")
		if len(lines) > 1 {
			t.Errorf("LIMIT 1 returned %d rows: %q", len(lines), records)
		}
	})

	t.Run("select_no_header", func(t *testing.T) {
		// Upload a headerless CSV.
		csvNoHdr := []byte("Alice,30,NYC\nBob,25,LA\n")
		mustStatus(t, h.do(http.MethodPut, "/data/nohdr.csv", csvNoHdr, nil), http.StatusOK)

		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object WHERE _2 = '30'</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <CSV>
      <FileHeaderInfo>NONE</FileHeaderInfo>
    </CSV>
  </InputSerialization>
  <OutputSerialization>
    <CSV/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/data/nohdr.csv?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		if !strings.Contains(string(records), "Alice") {
			t.Errorf("expected Alice in result, got: %q", records)
		}
		if strings.Contains(string(records), "Bob") {
			t.Errorf("Bob should be filtered out, got: %q", records)
		}
	})

	t.Run("missing_object_404", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization><CSV/></InputSerialization>
  <OutputSerialization><CSV/></OutputSerialization>
</SelectObjectContentRequest>`)
		res := h.do(http.MethodPost, "/data/missing.csv?select&select-type=2", body, nil)
		if res.status != http.StatusNotFound {
			t.Fatalf("expected 404 for missing object, got %d", res.status)
		}
	})
}

func TestSelectJSON(t *testing.T) {
	h := newHarness(t)

	mustStatus(t, h.do(http.MethodPut, "/jsondata", nil, nil), http.StatusOK)

	// JSON Lines object.
	jsonl := []byte(`{"name":"Alice","age":30,"city":"NYC"}
{"name":"Bob","age":25,"city":"LA"}
{"name":"Carol","age":35,"city":"NYC"}
`)
	mustStatus(t, h.do(http.MethodPut, "/jsondata/people.jsonl", jsonl, map[string]string{
		"Content-Type": "application/x-ndjson",
	}), http.StatusOK)

	t.Run("select_star_lines", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <JSON>
      <Type>LINES</Type>
    </JSON>
  </InputSerialization>
  <OutputSerialization>
    <JSON/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/jsondata/people.jsonl?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, err := decodeEventStream(bytes.NewReader(res.body))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if !strings.Contains(string(records), "Alice") {
			t.Errorf("expected Alice; got: %q", records)
		}
	})

	t.Run("select_where_json", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object WHERE city = 'NYC'</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <JSON>
      <Type>LINES</Type>
    </JSON>
  </InputSerialization>
  <OutputSerialization>
    <JSON/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/jsondata/people.jsonl?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		if strings.Contains(string(records), "Bob") {
			t.Errorf("Bob should be filtered; got: %q", records)
		}
		if !strings.Contains(string(records), "Carol") {
			t.Errorf("Carol expected; got: %q", records)
		}
	})

	t.Run("select_column_json", func(t *testing.T) {
		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT name FROM s3object WHERE age > 28</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <JSON>
      <Type>LINES</Type>
    </JSON>
  </InputSerialization>
  <OutputSerialization>
    <JSON/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/jsondata/people.jsonl?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		// Verify Alice and Carol appear but not Bob; output should be {"name":...}
		lines := strings.Split(strings.TrimRight(string(records), "\n"), "\n")
		for _, line := range lines {
			if strings.TrimSpace(line) == "" {
				continue
			}
			var obj map[string]interface{}
			if err := json.Unmarshal([]byte(line), &obj); err != nil {
				t.Errorf("line is not valid JSON: %q", line)
				continue
			}
			if _, ok := obj["age"]; ok {
				t.Errorf("projected column 'name' only, but 'age' present in: %q", line)
			}
		}
		if strings.Contains(string(records), "Bob") {
			t.Errorf("Bob (age 25) should be filtered by age > 28; got: %q", records)
		}
	})

	t.Run("select_document_json", func(t *testing.T) {
		// A JSON document (array of objects).
		doc := []byte(`[{"id":1,"val":"a"},{"id":2,"val":"b"},{"id":3,"val":"c"}]`)
		mustStatus(t, h.do(http.MethodPut, "/jsondata/doc.json", doc, nil), http.StatusOK)

		body := []byte(`<?xml version="1.0" encoding="UTF-8"?>
<SelectObjectContentRequest>
  <Expression>SELECT * FROM s3object WHERE id > 1</Expression>
  <ExpressionType>SQL</ExpressionType>
  <InputSerialization>
    <JSON>
      <Type>DOCUMENT</Type>
    </JSON>
  </InputSerialization>
  <OutputSerialization>
    <JSON/>
  </OutputSerialization>
</SelectObjectContentRequest>`)

		res := h.do(http.MethodPost, "/jsondata/doc.json?select&select-type=2", body, map[string]string{
			"Content-Type": "application/xml",
		})
		mustStatus(t, res, http.StatusOK)
		records, _ := decodeEventStream(bytes.NewReader(res.body))
		if strings.Contains(string(records), `"id":1`) {
			t.Errorf("id=1 should be filtered; got: %q", records)
		}
		if !strings.Contains(string(records), `"id":2`) {
			t.Errorf("id=2 expected; got: %q", records)
		}
	})
}

// TestSelectEventStream verifies the binary event stream framing itself:
// CRC32 correctness and the header encoding round-trip.
func TestSelectEventStream(t *testing.T) {
	var buf bytes.Buffer
	payload := []byte("hello,world\n")
	if err := writeRecordsEvent(&buf, payload); err != nil {
		t.Fatalf("writeRecordsEvent: %v", err)
	}
	if err := writeEndEvent(&buf); err != nil {
		t.Fatalf("writeEndEvent: %v", err)
	}

	data := buf.Bytes()
	// Parse and verify first message (Records).
	if len(data) < 12 {
		t.Fatal("event stream too short")
	}
	total := int(binary.BigEndian.Uint32(data[0:]))
	wantMsgCRC := binary.BigEndian.Uint32(data[total-4:])
	gotMsgCRC := crc32.ChecksumIEEE(data[:total-4])
	if gotMsgCRC != wantMsgCRC {
		t.Errorf("message CRC mismatch: got %x want %x", gotMsgCRC, wantMsgCRC)
	}
	// Prelude CRC check.
	wantPreludeCRC := binary.BigEndian.Uint32(data[8:])
	gotPreludeCRC := crc32.ChecksumIEEE(data[:8])
	if gotPreludeCRC != wantPreludeCRC {
		t.Errorf("prelude CRC mismatch: got %x want %x", gotPreludeCRC, wantPreludeCRC)
	}

	// Records should round-trip to the payload we fed in.
	records, err := decodeEventStream(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decodeEventStream: %v", err)
	}
	if !bytes.Equal(records, payload) {
		t.Errorf("payload mismatch: got %q want %q", records, payload)
	}
}
