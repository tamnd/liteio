// SPDX-License-Identifier: Apache-2.0

// Package liteioaudit delivers S3 request audit events as structured JSON lines.
// An Auditor receives one event per completed request carrying the identity,
// operation, resource, outcome, and timing. The default file-backed auditor
// rotates the current file atomically on Open; a no-op auditor satisfies the
// interface with no overhead for deployments that do not need auditing.
package liteioaudit

import (
	"encoding/json"
	"io"
	"os"
	"sync"
	"time"
)

// Event is one audit record: a single completed S3 request.
type Event struct {
	Time       time.Time `json:"time"`
	RequestID  string    `json:"request_id"`
	AccessKey  string    `json:"access_key"`
	Method     string    `json:"method"`
	Bucket     string    `json:"bucket,omitempty"`
	Key        string    `json:"key,omitempty"`
	Status     int       `json:"status"`
	BytesIn    int64     `json:"bytes_in,omitempty"`
	BytesOut   int64     `json:"bytes_out,omitempty"`
	DurationMs int64     `json:"duration_ms"`
	RemoteIP   string    `json:"remote_ip,omitempty"`
	Error      string    `json:"error,omitempty"`
}

// Auditor receives completed-request events.
type Auditor interface {
	Log(e Event)
}

// Nop is an Auditor that discards every event. It satisfies Auditor with no
// overhead for deployments that do not need auditing.
type Nop struct{}

// Log discards e.
func (Nop) Log(Event) {}

// FileAuditor writes JSON-line audit events to a file. It is goroutine-safe.
type FileAuditor struct {
	mu  sync.Mutex
	enc *json.Encoder
	w   io.Closer
}

// Open creates (or truncates) the file at path and returns a FileAuditor that
// writes to it. The caller must call Close when done.
func Open(path string) (*FileAuditor, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileAuditor{enc: json.NewEncoder(f), w: f}, nil
}

// Log encodes e as a JSON line. It never returns an error to callers; any
// write failure is silently swallowed because audit failures must not abort
// the request.
func (a *FileAuditor) Log(e Event) {
	a.mu.Lock()
	_ = a.enc.Encode(e)
	a.mu.Unlock()
}

// Close flushes and closes the underlying file.
func (a *FileAuditor) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.w.Close()
}

// WriterAuditor wraps any io.Writer so tests and webhook bridges can plug in.
type WriterAuditor struct {
	mu  sync.Mutex
	enc *json.Encoder
}

// NewWriterAuditor returns an Auditor that encodes events as JSON lines to w.
func NewWriterAuditor(w io.Writer) *WriterAuditor {
	return &WriterAuditor{enc: json.NewEncoder(w)}
}

// Log encodes e as a JSON line.
func (a *WriterAuditor) Log(e Event) {
	a.mu.Lock()
	_ = a.enc.Encode(e)
	a.mu.Unlock()
}
