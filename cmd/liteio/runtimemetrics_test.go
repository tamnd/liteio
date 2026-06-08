// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/tamnd/liteio/metrics"
)

// TestRuntimeCollectorGet checks that a fresh collector returns a snapshot with
// non-zero goroutine count and that the TTL gates recomputation.
func TestRuntimeCollectorGet(t *testing.T) {
	now := time.Unix(2000, 0)
	rc := newRuntimeCollector(func() time.Time { return now })

	s := rc.get()
	if s.goroutines < 1 {
		t.Fatalf("goroutines = %v, want >= 1", s.goroutines)
	}
	if s.heapAlloc == 0 {
		t.Fatal("heapAlloc = 0")
	}

	// Mutate the snap directly to a sentinel so we can check the TTL short-circuits.
	rc.mu.Lock()
	rc.snap.goroutines = 9999
	rc.mu.Unlock()

	s2 := rc.get()
	if s2.goroutines != 9999 {
		t.Fatalf("expected memoized goroutines=9999, got %v", s2.goroutines)
	}

	// Advance past the TTL; the sentinel should disappear.
	now = now.Add(6 * time.Second)
	s3 := rc.get()
	if s3.goroutines == 9999 {
		t.Fatalf("snapshot not refreshed after TTL expiry")
	}
}

// TestRegisterRuntimeMetrics registers on a fresh registry, scrapes to text, and
// verifies every family is present with the right type.
func TestRegisterRuntimeMetrics(t *testing.T) {
	reg := metrics.NewRegistry()
	registerRuntimeMetrics(reg, time.Now)

	var b strings.Builder
	if err := reg.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	out := b.String()

	wantFamilies := []string{
		"liteio_go_goroutines",
		"liteio_go_heap_alloc_bytes",
		"liteio_go_heap_sys_bytes",
		"liteio_go_gc_pause_seconds_total",
		"liteio_go_gc_cycles_total",
		"liteio_buf_pool_gets_total",
		"liteio_buf_pool_puts_total",
	}
	for _, f := range wantFamilies {
		if !strings.Contains(out, f) {
			t.Errorf("exposition missing family %q\n%s", f, out)
		}
	}

	wantTypes := []string{
		"# TYPE liteio_go_goroutines gauge",
		"# TYPE liteio_go_heap_alloc_bytes gauge",
		"# TYPE liteio_go_heap_sys_bytes gauge",
		"# TYPE liteio_go_gc_pause_seconds_total counter",
		"# TYPE liteio_go_gc_cycles_total counter",
		"# TYPE liteio_buf_pool_gets_total counter",
		"# TYPE liteio_buf_pool_puts_total counter",
	}
	for _, w := range wantTypes {
		if !strings.Contains(out, w) {
			t.Errorf("exposition missing %q\n%s", w, out)
		}
	}
}
