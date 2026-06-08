// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bufio"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/auth"
	"github.com/tamnd/liteio/s3/sign"
	"github.com/tamnd/liteio/trace"
)

// TestTraceSSEReceivesEvents subscribes to the SSE stream, emits two events via
// the ring buf, and verifies both appear as "data:" lines.
func TestTraceSSEReceivesEvents(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	// *trace.RingBuf implements both TraceEmitter and TraceSource.
	ring := trace.NewRingBuf()
	s := NewServer(store, creds,
		WithClock(func() time.Time { return time.Now().UTC() }),
		WithTraceSource(ring),
	)
	ts := httptest.NewServer(s)
	defer ts.Close()

	// Use a cancellable context so we can stop the SSE connection once we have
	// collected enough events.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+APIPrefix+"/trace", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}

	// Collect data lines from the SSE body in a goroutine so the test does not
	// block the goroutine that emits events.
	dataLines := make(chan string, 10)
	go func() {
		scanner := bufio.NewScanner(resp.Body)
		for scanner.Scan() {
			line := scanner.Text()
			if strings.HasPrefix(line, "data: ") {
				dataLines <- line
			}
		}
		close(dataLines)
	}()

	// Emit two events. Subscribe was called before WriteHeader, so the handler
	// goroutine is already in its select loop and will receive these immediately.
	ev1 := trace.TraceEvent{RequestID: "req-1", Method: "GET", Path: "/b/k", StatusCode: 200, DurationMs: 5, Time: time.Now()}
	ev2 := trace.TraceEvent{RequestID: "req-2", Method: "PUT", Path: "/b/k", StatusCode: 200, DurationMs: 10, Time: time.Now()}
	ring.Emit(ev1)
	ring.Emit(ev2)

	timeout := time.NewTimer(3 * time.Second)
	defer timeout.Stop()
	var collected []string
	for len(collected) < 2 {
		select {
		case line, ok := <-dataLines:
			if !ok {
				t.Fatal("SSE stream closed before receiving 2 events")
			}
			collected = append(collected, line)
		case <-timeout.C:
			t.Fatalf("timed out after collecting %d/2 events", len(collected))
		}
	}

	// Cancel the context to stop the streaming handler.
	cancel()

	if !strings.Contains(collected[0], "req-1") {
		t.Errorf("first event = %q, want req-1", collected[0])
	}
	if !strings.Contains(collected[1], "req-2") {
		t.Errorf("second event = %q, want req-2", collected[1])
	}
}

// TestTraceNotConfigured501 verifies the handler returns 501 with no source wired.
func TestTraceNotConfigured501(t *testing.T) {
	store := auth.NewStore(adminCreds.AccessKey, adminCreds.SecretKey)
	creds := auth.NewStaticStore(adminCreds)
	s := NewServer(store, creds, WithClock(func() time.Time { return time.Now().UTC() }))
	ts := httptest.NewServer(s)
	defer ts.Close()

	req, _ := http.NewRequest(http.MethodGet, ts.URL+APIPrefix+"/trace", nil)
	sign.SignHeader(req, adminCreds, "us-east-1", sign.EmptyPayloadHash, time.Now().UTC())
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}
