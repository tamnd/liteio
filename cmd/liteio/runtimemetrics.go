// SPDX-License-Identifier: Apache-2.0

package main

import (
	"runtime"
	"sync"
	"time"

	"github.com/tamnd/liteio/buf"
	"github.com/tamnd/liteio/metrics"
)

// runtimeSnapshot is the derived view one scrape reports from the Go runtime and
// the buffer pool, every field already a float64 so a gauge or counter func returns
// it directly.
type runtimeSnapshot struct {
	goroutines   float64
	heapAlloc    float64
	heapSys      float64
	gcPauseTotal float64
	gcCycles     float64
	bufGets      float64
	bufPuts      float64
}

// runtimeCollector exposes Go runtime health and buffer-pool activity as metrics
// (spec 2020, doc 10.4). runtime.ReadMemStats is a stop-the-world call, so the
// collector memoizes one snapshot for a 5-second window; a scrape reads all seven
// series from the same consistent view without re-stopping the world.
type runtimeCollector struct {
	now func() time.Time
	ttl time.Duration

	mu     sync.Mutex
	snap   runtimeSnapshot
	expiry time.Time
	valid  bool
}

func newRuntimeCollector(now func() time.Time) *runtimeCollector {
	return &runtimeCollector{now: now, ttl: 5 * time.Second}
}

// get returns the current snapshot, recomputing it when the memoized one has
// expired. The lock serializes the recompute so a burst of concurrent scrapes
// stops the world once, not once per scrape.
func (rc *runtimeCollector) get() runtimeSnapshot {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	if rc.valid && rc.now().Before(rc.expiry) {
		return rc.snap
	}
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	gets, puts := buf.PoolStats()
	rc.snap = runtimeSnapshot{
		goroutines:   float64(runtime.NumGoroutine()),
		heapAlloc:    float64(ms.HeapAlloc),
		heapSys:      float64(ms.HeapSys),
		gcPauseTotal: float64(ms.PauseTotalNs) / 1e9,
		gcCycles:     float64(ms.NumGC),
		bufGets:      float64(gets),
		bufPuts:      float64(puts),
	}
	rc.expiry = rc.now().Add(rc.ttl)
	rc.valid = true
	return rc.snap
}

// registerRuntimeMetrics wires the Go runtime and buffer-pool families onto reg.
// goroutines, heap sizes are point-in-time gauges; GC pause total, GC cycle count,
// and pool totals are monotonic counters.
func registerRuntimeMetrics(reg *metrics.Registry, now func() time.Time) {
	rc := newRuntimeCollector(now)
	reg.NewGaugeFunc("liteio_go_goroutines",
		"Number of goroutines that currently exist.",
		func() float64 { return rc.get().goroutines })
	reg.NewGaugeFunc("liteio_go_heap_alloc_bytes",
		"Bytes of allocated heap objects.",
		func() float64 { return rc.get().heapAlloc })
	reg.NewGaugeFunc("liteio_go_heap_sys_bytes",
		"Bytes of heap memory obtained from the OS.",
		func() float64 { return rc.get().heapSys })
	reg.NewCounterFunc("liteio_go_gc_pause_seconds_total",
		"Total time spent in GC stop-the-world pauses, in seconds.",
		func() float64 { return rc.get().gcPauseTotal })
	reg.NewCounterFunc("liteio_go_gc_cycles_total",
		"Number of completed GC cycles.",
		func() float64 { return rc.get().gcCycles })
	reg.NewCounterFunc("liteio_buf_pool_gets_total",
		"Total buf.Get calls since process start.",
		func() float64 { return rc.get().bufGets })
	reg.NewCounterFunc("liteio_buf_pool_puts_total",
		"Total buf.Put calls since process start.",
		func() float64 { return rc.get().bufPuts })
}
