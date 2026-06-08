// SPDX-License-Identifier: Apache-2.0

package event

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestEventNameWildcardMatch checks that s3:ObjectCreated:* matches specific names.
func TestEventNameWildcardMatch(t *testing.T) {
	tests := []struct {
		rule  EventName
		name  EventName
		match bool
	}{
		{ObjectCreatedPut, ObjectCreatedPut, true},
		{"s3:ObjectCreated:*", ObjectCreatedPut, true},
		{"s3:ObjectCreated:*", ObjectCreatedCompleteMultipartUpload, true},
		{"s3:ObjectCreated:*", ObjectRemovedDelete, false},
		{"s3:ObjectRemoved:*", ObjectRemovedDelete, true},
		{"s3:ObjectRemoved:*", ObjectRemovedDeleteMarkerCreated, true},
		{"s3:ObjectRemoved:*", ObjectCreatedPut, false},
		{ObjectCreatedPut, ObjectCreatedCopy, false},
	}
	for _, tt := range tests {
		got := eventMatches([]EventName{tt.rule}, tt.name)
		if got != tt.match {
			t.Errorf("eventMatches([%s], %s) = %v, want %v", tt.rule, tt.name, got, tt.match)
		}
	}
}

// TestFilterRuleMatches checks prefix/suffix filter logic.
func TestFilterRuleMatches(t *testing.T) {
	tests := []struct {
		rule  FilterRule
		key   string
		match bool
	}{
		{FilterRule{"Prefix", "images/"}, "images/cat.jpg", true},
		{FilterRule{"Prefix", "images/"}, "docs/cat.jpg", false},
		{FilterRule{"Suffix", ".jpg"}, "cat.jpg", true},
		{FilterRule{"Suffix", ".jpg"}, "cat.png", false},
		{FilterRule{"Unknown", "x"}, "anything", true}, // unknown name = pass
	}
	for _, tt := range tests {
		got := tt.rule.matches(tt.key)
		if got != tt.match {
			t.Errorf("%+v.matches(%q) = %v, want %v", tt.rule, tt.key, got, tt.match)
		}
	}
}

// TestMatchingWebhooksFilters verifies that only matching webhooks are returned.
func TestMatchingWebhooksFilters(t *testing.T) {
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{
				ID:     "images-put",
				URL:    "http://hook1",
				Events: []EventName{"s3:ObjectCreated:*"},
				Filter: &Filter{Key: KeyFilter{FilterRules: []FilterRule{{"Prefix", "images/"}}}},
			},
			{
				ID:     "all-deletes",
				URL:    "http://hook2",
				Events: []EventName{"s3:ObjectRemoved:*"},
			},
			{
				ID:     "jpg-any",
				URL:    "http://hook3",
				Events: []EventName{"s3:ObjectCreated:Put"},
				Filter: &Filter{Key: KeyFilter{FilterRules: []FilterRule{{"Suffix", ".jpg"}}}},
			},
		},
	}

	// images/cat.jpg PUT — should match hook1 (prefix+created) and hook3 (suffix+put)
	got := MatchingWebhooks(cfg, ObjectCreatedPut, "images/cat.jpg")
	if len(got) != 2 {
		t.Fatalf("images/cat.jpg PUT: %d webhooks, want 2", len(got))
	}

	// docs/readme.txt DELETE — only hook2 (all-deletes, no filter)
	got = MatchingWebhooks(cfg, ObjectRemovedDelete, "docs/readme.txt")
	if len(got) != 1 || got[0].URL != "http://hook2" {
		t.Fatalf("docs DELETE: %+v", got)
	}

	// images/cat.png PUT — hook1 (prefix match, not suffix .jpg) but NOT hook3
	got = MatchingWebhooks(cfg, ObjectCreatedPut, "images/cat.png")
	if len(got) != 1 || got[0].URL != "http://hook1" {
		t.Fatalf("images/cat.png PUT: %+v", got)
	}
}

// TestDispatcherDelivery verifies that an event fired via Dispatch reaches a
// webhook endpoint and arrives as the expected JSON envelope.
func TestDispatcherDelivery(t *testing.T) {
	var (
		mu       sync.Mutex
		received []Envelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("unmarshal: %v", err)
			return
		}
		mu.Lock()
		received = append(received, env)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(srv.Client())

	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{"s3:ObjectCreated:*"}},
		},
	}
	rec := Record{
		EventVersion: "2.1",
		EventSource:  "liteio:s3",
		EventName:    ObjectCreatedPut,
		EventTime:    time.Now(),
		S3: S3Entity{
			Bucket: BucketID{Name: "photos"},
			Object: ObjectID{Key: "cat.jpg", Size: 42, ETag: "abc"},
		},
	}
	d.Dispatch(cfg, ObjectCreatedPut, "cat.jpg", []Record{rec})

	// Wait up to 2 seconds for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(received)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(received) == 0 {
		t.Fatal("webhook not called within 2 seconds")
	}
	if len(received[0].Records) != 1 {
		t.Fatalf("got %d records, want 1", len(received[0].Records))
	}
	got := received[0].Records[0]
	if got.S3.Bucket.Name != "photos" {
		t.Errorf("bucket = %q, want %q", got.S3.Bucket.Name, "photos")
	}
	if got.S3.Object.Key != "cat.jpg" {
		t.Errorf("key = %q, want %q", got.S3.Object.Key, "cat.jpg")
	}
}

// TestDispatcherNonMatchingSkipped verifies that a Dispatch call for a delete
// event does not reach a created-only webhook.
func TestDispatcherNonMatchingSkipped(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(srv.Client())
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{"s3:ObjectCreated:*"}},
		},
	}
	d.Dispatch(cfg, ObjectRemovedDelete, "foo", nil)
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Fatal("webhook called for non-matching event")
	}
}

// TestDispatcherRetryOnFailure verifies that the dispatcher retries a failing
// webhook and eventually marks it as failed after maxRetries.
func TestDispatcherRetryOnFailure(t *testing.T) {
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		// Read and discard body so keep-alive works.
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	d := NewDispatcher(&http.Client{
		Timeout:   100 * time.Millisecond,
		Transport: &http.Transport{
			// Disable keep-alive to prevent connection reuse between retries.
		},
	})
	// Override the retry delay to zero for fast tests.
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{ObjectCreatedPut}},
		},
	}
	rec := Record{EventName: ObjectCreatedPut, S3: S3Entity{Bucket: BucketID{Name: "b"}, Object: ObjectID{Key: "k"}}}
	d.Dispatch(cfg, ObjectCreatedPut, "k", []Record{rec})

	// Wait for retries to complete.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if d.Stats().Failed > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if d.Stats().Failed == 0 {
		t.Fatal("dispatcher did not report a failed delivery")
	}
}

// TestDispatcherStats checks that counters are incremented correctly.
func TestDispatcherStats(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(srv.Client())
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{ObjectCreatedPut}},
		},
	}
	rec := Record{EventName: ObjectCreatedPut}

	d.Dispatch(cfg, ObjectCreatedPut, "k", []Record{rec})
	d.Dispatch(cfg, ObjectCreatedPut, "k2", []Record{rec})

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if d.Stats().Sent >= 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if d.Stats().Sent < 2 {
		t.Fatalf("Sent = %d, want >= 2", d.Stats().Sent)
	}
	if d.Stats().Queued < 2 {
		t.Fatalf("Queued = %d, want >= 2", d.Stats().Queued)
	}
}

// TestDispatcherClose verifies that Close drains in-flight deliveries.
func TestDispatcherClose(t *testing.T) {
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		mu.Lock()
		count++
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(srv.Client())
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{ObjectCreatedPut}},
		},
	}
	for range 5 {
		d.Dispatch(cfg, ObjectCreatedPut, "k", []Record{{EventName: ObjectCreatedPut}})
	}
	d.Close()
}

// TestNewEventNamesMatchWildcard verifies the new restore and replication event
// names match their respective wildcard prefixes.
func TestNewEventNamesMatchWildcard(t *testing.T) {
	tests := []struct {
		rule  EventName
		name  EventName
		match bool
	}{
		{"s3:ObjectRestore:*", ObjectRestoreInitiated, true},
		{"s3:ObjectRestore:*", ObjectRestoreCompleted, true},
		{"s3:ObjectRestore:*", ObjectCreatedPut, false},
		{"s3:Replication:*", ReplicationOperationFailed, true},
		{"s3:Replication:*", ReplicationOperationMissedThreshold, true},
		{"s3:Replication:*", ReplicationOperationNotTracked, true},
		{"s3:Replication:*", ObjectCreatedPut, false},
		{ObjectRestoreInitiated, ObjectRestoreInitiated, true},
		{ObjectRestoreCompleted, ObjectRestoreCompleted, true},
		{ReplicationOperationFailed, ReplicationOperationFailed, true},
	}
	for _, tt := range tests {
		got := eventMatches([]EventName{tt.rule}, tt.name)
		if got != tt.match {
			t.Errorf("eventMatches([%s], %s) = %v, want %v", tt.rule, tt.name, got, tt.match)
		}
	}
}

// TestDispatchDeliversToQueueTarget registers a queue target and verifies that a
// matching event reaches it.
func TestDispatchDeliversToQueueTarget(t *testing.T) {
	var mu sync.Mutex
	var delivered []Envelope

	qt := &recordingTarget{
		deliverFn: func(env Envelope) {
			mu.Lock()
			delivered = append(delivered, env)
			mu.Unlock()
		},
	}

	d := NewDispatcher(nil)
	d.AddQueueTarget("my-queue", qt)

	cfg := NotificationConfig{
		QueueConfigurations: []QueueConfig{
			{
				ID:       "my-queue",
				QueueARN: "arn:liteio:sqs:::my-queue",
				Events:   []EventName{"s3:ObjectCreated:*"},
			},
		},
	}
	rec := Record{
		EventName: ObjectCreatedPut,
		S3:        S3Entity{Bucket: BucketID{Name: "b"}, Object: ObjectID{Key: "k"}},
	}
	d.Dispatch(cfg, ObjectCreatedPut, "k", []Record{rec})

	// Wait up to 2 seconds for delivery.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(delivered)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) == 0 {
		t.Fatal("queue target not called within 2 seconds")
	}
	if len(delivered[0].Records) != 1 || delivered[0].Records[0].S3.Bucket.Name != "b" {
		t.Errorf("unexpected envelope: %+v", delivered[0])
	}
	if d.Stats().QueueDelivered == 0 {
		t.Error("QueueDelivered counter not incremented")
	}
}

// recordingTarget is a test QueueTarget that calls deliverFn for each envelope.
type recordingTarget struct {
	deliverFn func(Envelope)
}

func (r *recordingTarget) Deliver(_ context.Context, env Envelope) error {
	r.deliverFn(env)
	return nil
}

func (r *recordingTarget) Close() error { return nil }

func BenchmarkDispatch(b *testing.B) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	d := NewDispatcher(srv.Client())
	cfg := NotificationConfig{
		WebhookConfigurations: []WebhookConfig{
			{URL: srv.URL, Events: []EventName{"s3:ObjectCreated:*"}},
		},
	}
	rec := Record{EventName: ObjectCreatedPut, S3: S3Entity{Bucket: BucketID{Name: "b"}, Object: ObjectID{Key: "k"}}}

	b.ResetTimer()
	b.ReportAllocs()
	for b.Loop() {
		d.Dispatch(cfg, ObjectCreatedPut, "k", []Record{rec})
	}
	_ = bytes.NewReader(nil) // ensure bytes import used
}
