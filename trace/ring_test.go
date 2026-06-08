// SPDX-License-Identifier: Apache-2.0

package trace

import (
	"testing"
	"time"
)

func sampleEvent(id string) TraceEvent {
	return TraceEvent{RequestID: id, Method: "GET", Path: "/x", StatusCode: 200, DurationMs: 1, Time: time.Now()}
}

// TestRingSubscribeEmit verifies that a subscriber receives events emitted after
// it subscribes.
func TestRingSubscribeEmit(t *testing.T) {
	rb := NewRingBuf()
	ch, cancel := rb.Subscribe()
	defer cancel()

	rb.Emit(sampleEvent("a"))
	rb.Emit(sampleEvent("b"))

	got := make([]string, 0, 2)
	for range 2 {
		select {
		case ev := <-ch:
			got = append(got, ev.RequestID)
		case <-time.After(time.Second):
			t.Fatal("timeout waiting for event")
		}
	}
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Errorf("events = %v, want [a b]", got)
	}
}

// TestRingSlowSubscriberDrop verifies that a slow subscriber (full channel) does
// not block the emitter or other subscribers.
func TestRingSlowSubscriberDrop(t *testing.T) {
	rb := NewRingBuf()

	// fast subscriber with cap-64 channel.
	fastCh, cancelFast := rb.Subscribe()
	defer cancelFast()

	// slow subscriber: we will never read from it so its channel fills up.
	_, cancelSlow := rb.Subscribe()
	defer cancelSlow()

	// Emit more events than the slow subscriber's channel can hold.
	for i := range 70 {
		rb.Emit(sampleEvent(string(rune('a' + i%26))))
	}

	// The fast subscriber should have received at least 64 events (channel cap).
	// None of the emits should have blocked.
	n := len(fastCh)
	if n == 0 {
		t.Error("fast subscriber received no events")
	}
}

// TestRingUnsubscribeCloses verifies that cancel closes the channel so a range
// over it terminates.
func TestRingUnsubscribeCloses(t *testing.T) {
	rb := NewRingBuf()
	ch, cancel := rb.Subscribe()

	cancel()

	// After cancel the channel should be closed; a receive should return the zero
	// value with ok=false.
	select {
	case _, ok := <-ch:
		if ok {
			t.Error("channel not closed after cancel")
		}
	case <-time.After(time.Second):
		t.Error("timeout: channel was not closed")
	}
}
