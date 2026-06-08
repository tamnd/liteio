// SPDX-License-Identifier: Apache-2.0

package object

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

	"github.com/tamnd/liteio/event"
)

// TestSetGetDeleteBucketNotification exercises the storage lifecycle for
// notification configs: set → get → verify → delete → get returns empty.
func TestSetGetDeleteBucketNotification(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "events")

	cfg := event.NotificationConfig{
		WebhookConfigurations: []event.WebhookConfig{
			{
				ID:     "created-hook",
				URL:    "http://example.com/hook",
				Events: []event.EventName{event.ObjectCreatedPut},
			},
		},
	}
	if err := sp.SetBucketNotification(ctx, "events", cfg); err != nil {
		t.Fatalf("SetBucketNotification: %v", err)
	}
	got, err := sp.GetBucketNotification(ctx, "events")
	if err != nil {
		t.Fatalf("GetBucketNotification: %v", err)
	}
	if len(got.WebhookConfigurations) != 1 || got.WebhookConfigurations[0].URL != "http://example.com/hook" {
		t.Fatalf("unexpected config: %+v", got)
	}

	if err := sp.DeleteBucketNotification(ctx, "events"); err != nil {
		t.Fatalf("DeleteBucketNotification: %v", err)
	}
	// After deletion, GetBucketNotification returns an empty config (not an error).
	got, err = sp.GetBucketNotification(ctx, "events")
	if err != nil {
		t.Fatalf("GetBucketNotification after delete: %v", err)
	}
	if len(got.WebhookConfigurations)+len(got.QueueConfigurations) != 0 {
		t.Fatalf("expected empty config after delete, got %+v", got)
	}
}

// TestDeleteBucketNotificationIdempotent verifies that deleting a notification
// config when none is set does not return an error.
func TestDeleteBucketNotificationIdempotent(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	mustMakeBucket(t, sp, "b")
	if err := sp.DeleteBucketNotification(ctx, "b"); err != nil {
		t.Fatalf("idempotent delete: %v", err)
	}
}

// TestGetBucketNotificationMissingBucket checks that GetBucketNotification
// returns ErrBucketNotFound for a non-existent bucket.
func TestGetBucketNotificationMissingBucket(t *testing.T) {
	ctx := context.Background()
	sp := newLayer(t, 4, 2)
	_, err := sp.GetBucketNotification(ctx, "ghost")
	if err == nil {
		t.Fatal("expected error for missing bucket, got nil")
	}
}

// TestPutObjectFiresWebhookEvent is an integration test: it sets a webhook
// notification config on a bucket, then PUTs an object and verifies the event
// reaches the test webhook server.
func TestPutObjectFiresWebhookEvent(t *testing.T) {
	var (
		mu       sync.Mutex
		received []event.Envelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env event.Envelope
		if err := json.Unmarshal(body, &env); err != nil {
			t.Errorf("unmarshal: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		mu.Lock()
		received = append(received, env)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	disp := event.NewDispatcher(srv.Client())
	sp := newLayer(t, 4, 2)
	sp.dispatcher = disp

	mustMakeBucket(t, sp, "watched")
	cfg := event.NotificationConfig{
		WebhookConfigurations: []event.WebhookConfig{
			{URL: srv.URL, Events: []event.EventName{"s3:ObjectCreated:*"}},
		},
	}
	if err := sp.SetBucketNotification(ctx, "watched", cfg); err != nil {
		t.Fatalf("SetBucketNotification: %v", err)
	}

	// Write an object — should fire an event.
	body := []byte("hello event world")
	if _, err := sp.PutObject(ctx, "watched", "hello.txt",
		NewPutReader(bytes.NewReader(body), int64(len(body))),
		ObjectOptions{SourceIP: "1.2.3.4"}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}

	// Wait up to 2 seconds for the webhook to be called.
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
	recs := received[0].Records
	if len(recs) != 1 {
		t.Fatalf("got %d records, want 1", len(recs))
	}
	r := recs[0]
	if r.EventName != event.ObjectCreatedPut {
		t.Errorf("EventName = %q, want %q", r.EventName, event.ObjectCreatedPut)
	}
	if r.S3.Bucket.Name != "watched" {
		t.Errorf("bucket = %q, want %q", r.S3.Bucket.Name, "watched")
	}
	if r.S3.Object.Key != "hello.txt" {
		t.Errorf("key = %q, want %q", r.S3.Object.Key, "hello.txt")
	}
	if r.RequestParameters.SourceIPAddress != "1.2.3.4" {
		t.Errorf("sourceIP = %q, want 1.2.3.4", r.RequestParameters.SourceIPAddress)
	}
}

// TestDeleteObjectFiresWebhookEvent checks that a DELETE fires an
// ObjectRemovedDelete event to the webhook.
func TestDeleteObjectFiresWebhookEvent(t *testing.T) {
	var (
		mu       sync.Mutex
		received []event.Envelope
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var env event.Envelope
		if json.Unmarshal(body, &env) == nil {
			mu.Lock()
			received = append(received, env)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	disp := event.NewDispatcher(srv.Client())
	sp := newLayer(t, 4, 2)
	sp.dispatcher = disp

	mustMakeBucket(t, sp, "deleteme")
	cfg := event.NotificationConfig{
		WebhookConfigurations: []event.WebhookConfig{
			{URL: srv.URL, Events: []event.EventName{"s3:ObjectRemoved:*"}},
		},
	}
	if err := sp.SetBucketNotification(ctx, "deleteme", cfg); err != nil {
		t.Fatalf("SetBucketNotification: %v", err)
	}

	// Write then delete.
	data := []byte("bye")
	if _, err := sp.PutObject(ctx, "deleteme", "file.txt",
		NewPutReader(bytes.NewReader(data), int64(len(data))),
		ObjectOptions{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	if _, err := sp.DeleteObject(ctx, "deleteme", "file.txt", ObjectOptions{}); err != nil {
		t.Fatalf("DeleteObject: %v", err)
	}

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
		t.Fatal("delete webhook not called")
	}
	recs := received[0].Records
	if len(recs) == 0 || recs[0].EventName != event.ObjectRemovedDelete {
		t.Errorf("unexpected records: %+v", recs)
	}
}

// TestNonMatchingEventNotFired verifies that a PUT does not trigger a
// delete-only webhook.
func TestNonMatchingEventNotFired(t *testing.T) {
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body) //nolint:errcheck
		called = true
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx := context.Background()
	disp := event.NewDispatcher(srv.Client())
	sp := newLayer(t, 4, 2)
	sp.dispatcher = disp

	mustMakeBucket(t, sp, "quiet")
	cfg := event.NotificationConfig{
		WebhookConfigurations: []event.WebhookConfig{
			{URL: srv.URL, Events: []event.EventName{"s3:ObjectRemoved:*"}},
		},
	}
	if err := sp.SetBucketNotification(ctx, "quiet", cfg); err != nil {
		t.Fatalf("SetBucketNotification: %v", err)
	}

	data := []byte("data")
	if _, err := sp.PutObject(ctx, "quiet", "f",
		NewPutReader(bytes.NewReader(data), int64(len(data))),
		ObjectOptions{}); err != nil {
		t.Fatalf("PutObject: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if called {
		t.Fatal("delete webhook called for a PUT event")
	}
}
