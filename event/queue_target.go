// SPDX-License-Identifier: Apache-2.0

package event

import (
	"context"
	"encoding/json"
	"os"
	"sync"
)

// QueueTarget is a named destination that receives event envelopes synchronously.
// Unlike webhook targets (HTTP POST, async with retries), a QueueTarget delivers
// to a local or in-process sink — a file, an in-memory buffer, or a message queue
// adapter. Delivery is best-effort from the dispatcher goroutine.
type QueueTarget interface {
	// Deliver sends env to the target. The ctx may carry a deadline. Errors are
	// counted in the dispatcher's QueueFailed counter.
	Deliver(ctx context.Context, env Envelope) error
	// Close flushes pending state and releases any held resources.
	Close() error
}

// FileQueueTarget appends one JSON line per envelope to a file. It is safe for
// concurrent use; a mutex serialises writes so lines never interleave.
type FileQueueTarget struct {
	path string
	mu   sync.Mutex
	f    *os.File
	enc  *json.Encoder
}

// NewFileQueueTarget opens (or creates) the file at path for append and wraps it
// with a JSON encoder. The file is created with mode 0600 when new.
func NewFileQueueTarget(path string) (*FileQueueTarget, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &FileQueueTarget{
		path: path,
		f:    f,
		enc:  json.NewEncoder(f),
	}, nil
}

// Deliver encodes env as a single JSON line. The context is not used (writes are
// fast local I/O), but is accepted to satisfy the QueueTarget interface.
func (t *FileQueueTarget) Deliver(_ context.Context, env Envelope) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.enc.Encode(env)
}

// Close flushes and closes the underlying file.
func (t *FileQueueTarget) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Close()
}
