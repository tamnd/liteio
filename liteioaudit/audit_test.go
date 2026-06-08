// SPDX-License-Identifier: Apache-2.0

package liteioaudit

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNopDoesNotPanic(t *testing.T) {
	var n Nop
	n.Log(Event{Time: time.Now(), Method: "GET"})
}

func TestWriterAuditorEncodesJSON(t *testing.T) {
	var buf bytes.Buffer
	a := NewWriterAuditor(&buf)
	e := Event{
		Time:       time.Now().UTC().Truncate(time.Second),
		RequestID:  "rid-001",
		AccessKey:  "testkey",
		Method:     "PutObject",
		Bucket:     "mybucket",
		Key:        "path/to/obj",
		Status:     200,
		BytesIn:    1024,
		DurationMs: 5,
	}
	a.Log(e)
	line := buf.String()
	if !strings.Contains(line, "PutObject") {
		t.Fatalf("missing method in output: %s", line)
	}
	if !strings.Contains(line, "mybucket") {
		t.Fatalf("missing bucket in output: %s", line)
	}
	var decoded Event
	if err := json.Unmarshal([]byte(strings.TrimSpace(line)), &decoded); err != nil {
		t.Fatalf("invalid JSON: %v — %s", err, line)
	}
	if decoded.RequestID != "rid-001" {
		t.Errorf("RequestID = %q, want rid-001", decoded.RequestID)
	}
	if decoded.BytesIn != 1024 {
		t.Errorf("BytesIn = %d, want 1024", decoded.BytesIn)
	}
}

func TestWriterAuditorMultipleEvents(t *testing.T) {
	var buf bytes.Buffer
	a := NewWriterAuditor(&buf)
	for i := 0; i < 5; i++ {
		a.Log(Event{Method: "GetObject", Status: 200})
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected 5 lines, got %d", len(lines))
	}
}

func TestFileAuditorWritesAndReopens(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.log")
	a, err := Open(path)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	a.Log(Event{Method: "PutObject", Bucket: "b", Key: "k", Status: 200})
	if err := a.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if !strings.Contains(string(data), "PutObject") {
		t.Fatalf("event not found in file: %s", data)
	}
	// Reopen and append a second event.
	a2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	a2.Log(Event{Method: "DeleteObject", Status: 204})
	_ = a2.Close()
	data2, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data2)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines after append, got %d: %s", len(lines), data2)
	}
}
