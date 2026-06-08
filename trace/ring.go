// SPDX-License-Identifier: Apache-2.0

package trace

import "sync"

// RingBuf is an in-process fan-out for TraceEvents. It holds a list of active
// subscriber channels and delivers each event to all of them non-blocking, so a
// slow subscriber is silently skipped rather than stalling the S3 hot path.
//
// It implements TraceEmitter, so it can be installed as the global emitter and
// composed with the admin trace streaming endpoint.
type RingBuf struct {
	mu   sync.Mutex
	subs []chan TraceEvent
}

// NewRingBuf returns an empty ring buffer ready to accept subscribers.
func NewRingBuf() *RingBuf {
	return &RingBuf{}
}

// Emit delivers e to every subscribed channel. If a subscriber's channel is
// full the event is dropped for that subscriber only; the other subscribers
// still receive it.
func (rb *RingBuf) Emit(e TraceEvent) {
	rb.mu.Lock()
	defer rb.mu.Unlock()
	for _, ch := range rb.subs {
		select {
		case ch <- e:
		default:
			// subscriber too slow: drop this event for them
		}
	}
}

// Subscribe registers a new subscriber and returns its receive channel plus a
// cancel function. Calling cancel removes the subscriber and closes the channel.
// The channel has capacity 64 to absorb short bursts without dropping.
func (rb *RingBuf) Subscribe() (<-chan TraceEvent, func()) {
	ch := make(chan TraceEvent, 64)
	rb.mu.Lock()
	rb.subs = append(rb.subs, ch)
	rb.mu.Unlock()

	cancel := func() {
		rb.mu.Lock()
		defer rb.mu.Unlock()
		for i, s := range rb.subs {
			if s == ch {
				rb.subs = append(rb.subs[:i], rb.subs[i+1:]...)
				close(ch)
				return
			}
		}
	}
	return ch, cancel
}
