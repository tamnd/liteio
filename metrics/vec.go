// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"sort"
	"strings"
	"sync"
)

// labelSep joins label values into a child key. It is a byte that cannot appear in a
// label value the caller passes (US, the ASCII unit separator), so distinct label
// tuples never collide on the same key.
const labelSep = "\x1f"

// child pairs a stored metric with the label values that key it, kept so exposition
// can pair each value with its labels in a stable order.
type child[T any] struct {
	values []string
	metric *T
}

// vec is the shared machinery behind the typed label vectors: a set of child metrics
// keyed by their label values, created on first use. The zero metric is the starting
// point (a counter at 0, a gauge at 0), matching the Prometheus client's lazy series.
type vec[T any] struct {
	labelNames []string
	mkChild    func() *T // builds a fresh child (a Histogram needs its bounds)

	mu       sync.Mutex
	children map[string]*child[T]
}

func newVec[T any](labelNames []string, mkChild func() *T) *vec[T] {
	return &vec[T]{labelNames: labelNames, mkChild: mkChild, children: map[string]*child[T]{}}
}

// with returns the child for the given label values, creating it on first use. The
// number of values must match the declared label names.
func (v *vec[T]) with(values ...string) *T {
	if len(values) != len(v.labelNames) {
		panic("metrics: label value count does not match the declared labels")
	}
	key := strings.Join(values, labelSep)
	v.mu.Lock()
	defer v.mu.Unlock()
	if c, ok := v.children[key]; ok {
		return c.metric
	}
	m := v.mkChild()
	v.children[key] = &child[T]{values: append([]string(nil), values...), metric: m}
	return m
}

// sorted returns the children ordered by their label values, so exposition output is
// deterministic across scrapes regardless of creation order.
func (v *vec[T]) sorted() []*child[T] {
	v.mu.Lock()
	out := make([]*child[T], 0, len(v.children))
	for _, c := range v.children {
		out = append(out, c)
	}
	v.mu.Unlock()
	sort.Slice(out, func(i, j int) bool {
		return strings.Join(out[i].values, labelSep) < strings.Join(out[j].values, labelSep)
	})
	return out
}

// CounterVec is a set of counters sharing a name, partitioned by label values.
type CounterVec struct{ v *vec[Counter] }

// With returns the counter for the given label values, creating it on first use.
func (cv *CounterVec) With(labelValues ...string) *Counter { return cv.v.with(labelValues...) }

// GaugeVec is a set of gauges sharing a name, partitioned by label values.
type GaugeVec struct{ v *vec[Gauge] }

// With returns the gauge for the given label values, creating it on first use.
func (gv *GaugeVec) With(labelValues ...string) *Gauge { return gv.v.with(labelValues...) }

// HistogramVec is a set of histograms sharing a name and buckets, partitioned by
// label values.
type HistogramVec struct{ v *vec[Histogram] }

// With returns the histogram for the given label values, creating it on first use.
func (hv *HistogramVec) With(labelValues ...string) *Histogram { return hv.v.with(labelValues...) }
