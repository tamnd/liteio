// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"math"
	"sort"
	"sync"
	"sync/atomic"
)

// Registry holds a set of named metrics and writes them in the Prometheus text
// exposition format. It is safe for concurrent use: registration takes a lock, and
// the metrics themselves update atomically, so scraping never blocks instrumentation.
type Registry struct {
	mu      sync.Mutex
	order   []string // registration order, for stable output grouping
	entries map[string]collector
}

// NewRegistry returns an empty registry.
func NewRegistry() *Registry {
	return &Registry{entries: map[string]collector{}}
}

// collector is one registered metric family. It knows how to write its own HELP,
// TYPE, and sample lines.
type collector interface {
	writeTo(w *textWriter)
}

func (r *Registry) register(name string, c collector) {
	mustValidName(name)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, dup := r.entries[name]; dup {
		panic("metrics: duplicate registration of " + name)
	}
	r.entries[name] = c
	r.order = append(r.order, name)
}

// NewCounter registers and returns a plain counter.
func (r *Registry) NewCounter(name, help string) *Counter {
	c := &Counter{}
	r.register(name, &counterCollector{name: name, help: help, c: c})
	return c
}

// NewGauge registers and returns a plain gauge.
func (r *Registry) NewGauge(name, help string) *Gauge {
	g := &Gauge{}
	r.register(name, &gaugeCollector{name: name, help: help, get: g.Get})
	return g
}

// NewGaugeFunc registers a gauge whose value is read from fn at scrape time. Use it
// for a value that is cheaper to sample on demand than to keep updated (online drive
// count, goroutine count, capacity figures pulled from the object layer).
func (r *Registry) NewGaugeFunc(name, help string, fn func() float64) {
	r.register(name, &gaugeCollector{name: name, help: help, get: fn})
}

// NewHistogram registers and returns a plain histogram over the given upper bounds.
func (r *Registry) NewHistogram(name, help string, buckets []float64) *Histogram {
	h := newHistogram(buckets)
	r.register(name, &histogramCollector{name: name, help: help, h: h})
	return h
}

// NewCounterVec registers and returns a labeled counter family.
func (r *Registry) NewCounterVec(name, help string, labelNames []string) *CounterVec {
	mustValidLabels(labelNames)
	cv := &CounterVec{v: newVec(labelNames, func() *Counter { return &Counter{} })}
	r.register(name, &counterVecCollector{name: name, help: help, cv: cv})
	return cv
}

// NewGaugeVec registers and returns a labeled gauge family.
func (r *Registry) NewGaugeVec(name, help string, labelNames []string) *GaugeVec {
	mustValidLabels(labelNames)
	gv := &GaugeVec{v: newVec(labelNames, func() *Gauge { return &Gauge{} })}
	r.register(name, &gaugeVecCollector{name: name, help: help, gv: gv})
	return gv
}

// NewHistogramVec registers and returns a labeled histogram family sharing buckets.
func (r *Registry) NewHistogramVec(name, help string, labelNames []string, buckets []float64) *HistogramVec {
	mustValidLabels(labelNames)
	bounds := normalizeBounds(buckets)
	hv := &HistogramVec{v: newVec(labelNames, func() *Histogram { return newHistogramBounds(bounds) })}
	r.register(name, &histogramVecCollector{name: name, help: help, labelNames: labelNames, hv: hv})
	return hv
}

// WriteText writes the whole registry in the Prometheus text exposition format, one
// family at a time in registration order.
func (r *Registry) WriteText(w io.Writer) error {
	r.mu.Lock()
	names := append([]string(nil), r.order...)
	entries := make([]collector, len(names))
	for i, n := range names {
		entries[i] = r.entries[n]
	}
	r.mu.Unlock()

	tw := &textWriter{}
	for _, c := range entries {
		c.writeTo(tw)
	}
	_, err := w.Write(tw.b.Bytes())
	return err
}

func newHistogram(buckets []float64) *Histogram {
	return newHistogramBounds(normalizeBounds(buckets))
}

func newHistogramBounds(bounds []float64) *Histogram {
	return &Histogram{bounds: bounds, counts: make([]atomic.Uint64, len(bounds)+1)}
}

// normalizeBounds sorts the bounds ascending and drops a trailing +Inf (it is the
// implicit final bucket), returning a defensive copy so the caller's slice is not
// retained or mutated.
func normalizeBounds(buckets []float64) []float64 {
	out := make([]float64, 0, len(buckets))
	for _, b := range buckets {
		if math.IsInf(b, 1) {
			continue
		}
		out = append(out, b)
	}
	sort.Float64s(out)
	return out
}
