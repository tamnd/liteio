// SPDX-License-Identifier: Apache-2.0

// Package metrics is a small, dependency-free Prometheus metrics registry. It
// provides counters, gauges, and histograms (each plain or labeled) and writes the
// Prometheus text exposition format. liteio exposes these on a token-protected
// metrics endpoint (doc 10.4); keeping the implementation in-tree avoids pulling the
// official client's large dependency tree into a project that values a lean build.
//
// The text format this package emits is the documented Prometheus exposition format:
// a "# HELP" line, a "# TYPE" line, then one sample line per series. A counter and a
// gauge emit a single value; a histogram emits cumulative "_bucket" lines, a "_sum",
// and a "_count".
package metrics

import (
	"math"
	"sync/atomic"
)

// addFloat atomically adds v to the float64 stored as the bit pattern in bits.
func addFloat(bits *atomic.Uint64, v float64) {
	for {
		old := bits.Load()
		next := math.Float64bits(math.Float64frombits(old) + v)
		if bits.CompareAndSwap(old, next) {
			return
		}
	}
}

// Counter is a monotonically increasing float64. Adding a negative value panics, the
// same programmer-error contract the Prometheus client enforces: a counter that can
// go down is a gauge.
type Counter struct {
	bits atomic.Uint64
}

// Inc adds one to the counter.
func (c *Counter) Inc() { addFloat(&c.bits, 1) }

// Add increases the counter by v, which must not be negative.
func (c *Counter) Add(v float64) {
	if v < 0 {
		panic("metrics: Counter.Add of a negative value")
	}
	addFloat(&c.bits, v)
}

// Get returns the current value.
func (c *Counter) Get() float64 { return math.Float64frombits(c.bits.Load()) }

// Gauge is a float64 that can be set to any value or moved up and down. Use it for a
// value that rises and falls (queue depth, online drives), not for a running total.
type Gauge struct {
	bits atomic.Uint64
}

// Set replaces the gauge's value.
func (g *Gauge) Set(v float64) { g.bits.Store(math.Float64bits(v)) }

// Add moves the gauge by v (negative to decrease).
func (g *Gauge) Add(v float64) { addFloat(&g.bits, v) }

// Inc adds one to the gauge.
func (g *Gauge) Inc() { addFloat(&g.bits, 1) }

// Dec subtracts one from the gauge.
func (g *Gauge) Dec() { addFloat(&g.bits, -1) }

// Get returns the current value.
func (g *Gauge) Get() float64 { return math.Float64frombits(g.bits.Load()) }

// Histogram counts observations into a fixed set of buckets and tracks their sum and
// count. The buckets hold upper bounds in increasing order; an implicit +Inf bucket
// catches everything above the last bound, so every observation lands somewhere.
type Histogram struct {
	bounds []float64       // upper bounds, ascending, no +Inf (it is implicit)
	counts []atomic.Uint64 // one per bound plus a final slot for the +Inf bucket
	sum    atomic.Uint64   // float64 bits
	count  atomic.Uint64
}

// Observe records one sample.
func (h *Histogram) Observe(v float64) {
	i := len(h.bounds)
	for j, b := range h.bounds {
		if v <= b {
			i = j
			break
		}
	}
	h.counts[i].Add(1)
	addFloat(&h.sum, v)
	h.count.Add(1)
}

// snapshot returns the cumulative bucket counts (aligned with h.bounds plus a final
// +Inf entry), the sum, and the total count, read for exposition.
func (h *Histogram) snapshot() (cumulative []uint64, sum float64, count uint64) {
	cumulative = make([]uint64, len(h.counts))
	var running uint64
	for i := range h.counts {
		running += h.counts[i].Load()
		cumulative[i] = running
	}
	return cumulative, math.Float64frombits(h.sum.Load()), h.count.Load()
}

// DefBuckets is a general-purpose latency bucket set in seconds, matching the
// Prometheus client's defaults: a request path's p50/p99/p999 fall within it.
var DefBuckets = []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10}
