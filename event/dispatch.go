// SPDX-License-Identifier: Apache-2.0

package event

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// defaultQueueDepth is the per-target event buffer. A slow webhook cannot
	// block the write path — events are dropped (counted) when the buffer is full.
	defaultQueueDepth = 4096
	// defaultWorkers is the number of concurrent delivery goroutines per target URL.
	defaultWorkers = 2
	// defaultTimeout is the HTTP POST deadline for one delivery attempt.
	defaultTimeout = 5 * time.Second
	// maxRetries is how many times delivery is retried before the event is dropped.
	maxRetries = 3
	// retryDelay is the base backoff between retries (doubles each attempt).
	retryDelay = 500 * time.Millisecond
)

// DispatchStats carries delivery counters for observability.
type DispatchStats struct {
	Queued  int64
	Sent    int64
	Dropped int64
	Failed  int64
}

// Dispatcher routes event records to webhook targets. Events are enqueued on the
// write path (non-blocking) and delivered asynchronously from a worker pool. A
// slow or unreachable target drops events (at-least-once with bounded memory).
type Dispatcher struct {
	client *http.Client
	now    func() time.Time

	mu      sync.RWMutex
	targets map[string]*target // keyed by URL

	queued  atomicInt64
	sent    atomicInt64
	dropped atomicInt64
	failed  atomicInt64
}

type target struct {
	url    string
	queue  chan delivery
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

type delivery struct {
	records []Record
}

// NewDispatcher creates a Dispatcher using client for HTTP POSTs. Pass nil to
// use a default client with the delivery timeout.
func NewDispatcher(client *http.Client) *Dispatcher {
	if client == nil {
		client = &http.Client{Timeout: defaultTimeout}
	}
	return &Dispatcher{
		client:  client,
		now:     time.Now,
		targets: make(map[string]*target),
	}
}

// Dispatch enqueues records for delivery to all matching webhook targets in cfg.
// It returns immediately; delivery is asynchronous. The call is safe to make from
// a hot write path.
func (d *Dispatcher) Dispatch(cfg NotificationConfig, name EventName, key string, records []Record) {
	webhooks := MatchingWebhooks(cfg, name, key)
	if len(webhooks) == 0 {
		return
	}
	for _, wh := range webhooks {
		d.mu.RLock()
		t, ok := d.targets[wh.URL]
		d.mu.RUnlock()
		if !ok {
			t = d.ensureTarget(wh.URL)
		}
		d.queued.add(1)
		select {
		case t.queue <- delivery{records: records}:
		default:
			d.dropped.add(1)
		}
	}
}

// ensureTarget creates and starts a delivery target for url if one does not
// exist yet, or returns the existing one.
func (d *Dispatcher) ensureTarget(url string) *target {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.targets[url]; ok {
		return t // another goroutine won the race
	}
	ctx, cancel := context.WithCancel(context.Background())
	t := &target{
		url:    url,
		queue:  make(chan delivery, defaultQueueDepth),
		cancel: cancel,
	}
	for range defaultWorkers {
		t.wg.Go(func() { d.worker(ctx, t) })
	}
	d.targets[url] = t
	return t
}

// worker drains the target's queue and POSTs each batch with retries.
func (d *Dispatcher) worker(ctx context.Context, t *target) {
	for {
		select {
		case <-ctx.Done():
			return
		case del, ok := <-t.queue:
			if !ok {
				return
			}
			if err := d.post(t.url, del.records); err != nil {
				d.failed.add(1)
			} else {
				d.sent.add(1)
			}
		}
	}
}

// post delivers records to url with retry/backoff.
func (d *Dispatcher) post(url string, records []Record) error {
	body, err := json.Marshal(Envelope{Records: records})
	if err != nil {
		return fmt.Errorf("event: marshal: %w", err)
	}
	delay := retryDelay
	for attempt := range maxRetries {
		req, err := http.NewRequestWithContext(
			context.Background(), http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := d.client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < 300 {
				return nil
			}
			err = fmt.Errorf("event: webhook %s returned %d", url, resp.StatusCode)
		}
		if attempt < maxRetries-1 {
			time.Sleep(delay)
			delay *= 2
		}
		_ = err // last attempt, fall through
	}
	return fmt.Errorf("event: webhook %s: all %d attempts failed", url, maxRetries)
}

// Close stops all delivery workers and waits for in-flight deliveries to finish.
func (d *Dispatcher) Close() {
	d.mu.Lock()
	targets := make([]*target, 0, len(d.targets))
	for _, t := range d.targets {
		targets = append(targets, t)
	}
	d.mu.Unlock()
	for _, t := range targets {
		t.cancel()
		t.wg.Wait()
	}
}

// Stats returns a snapshot of the delivery counters.
func (d *Dispatcher) Stats() DispatchStats {
	return DispatchStats{
		Queued:  d.queued.load(),
		Sent:    d.sent.load(),
		Dropped: d.dropped.load(),
		Failed:  d.failed.load(),
	}
}

// atomicInt64 is a simple lock-free counter.
type atomicInt64 struct{ n atomic.Int64 }

func (a *atomicInt64) add(delta int64) { a.n.Add(delta) }
func (a *atomicInt64) load() int64     { return a.n.Load() }
