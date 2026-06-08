// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"math"
	"strconv"
	"strings"
)

// textWriter accumulates the exposition output. Keeping the buffer here lets each
// collector share the header and sample formatters without re-deriving them.
type textWriter struct {
	b bytes.Buffer
}

// header writes the "# HELP" and "# TYPE" lines for a family.
func (w *textWriter) header(name, help, typ string) {
	w.b.WriteString("# HELP ")
	w.b.WriteString(name)
	w.b.WriteByte(' ')
	w.b.WriteString(escapeHelp(help))
	w.b.WriteByte('\n')
	w.b.WriteString("# TYPE ")
	w.b.WriteString(name)
	w.b.WriteByte(' ')
	w.b.WriteString(typ)
	w.b.WriteByte('\n')
}

// sample writes one sample line: name, optional labels, value. The labelNames and
// labelValues slices are paired by index; an empty pair writes a bare name.
func (w *textWriter) sample(name string, labelNames, labelValues []string, value float64) {
	w.b.WriteString(name)
	writeLabels(&w.b, labelNames, labelValues)
	w.b.WriteByte(' ')
	w.b.WriteString(formatFloat(value))
	w.b.WriteByte('\n')
}

// writeLabels writes {n1="v1",n2="v2"} (nothing when there are no labels).
func writeLabels(b *bytes.Buffer, names, values []string) {
	if len(names) == 0 {
		return
	}
	b.WriteByte('{')
	for i, n := range names {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n)
		b.WriteString(`="`)
		b.WriteString(escapeLabelValue(values[i]))
		b.WriteByte('"')
	}
	b.WriteByte('}')
}

// formatFloat renders a value the way Prometheus expects: the +Inf/-Inf/NaN
// keywords, and otherwise the shortest round-tripping decimal.
func formatFloat(v float64) string {
	switch {
	case math.IsInf(v, 1):
		return "+Inf"
	case math.IsInf(v, -1):
		return "-Inf"
	case math.IsNaN(v):
		return "NaN"
	default:
		return strconv.FormatFloat(v, 'g', -1, 64)
	}
}

// escapeHelp escapes a backslash and newline in help text (the two characters the
// format reserves on a HELP line).
func escapeHelp(s string) string {
	return strings.NewReplacer(`\`, `\\`, "\n", `\n`).Replace(s)
}

// escapeLabelValue escapes a backslash, double quote, and newline in a label value.
func escapeLabelValue(s string) string {
	return strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`).Replace(s)
}

// counterCollector writes a single counter.
type counterCollector struct {
	name, help string
	c          *Counter
}

func (e *counterCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "counter")
	w.sample(e.name, nil, nil, e.c.Get())
}

// gaugeCollector writes a single gauge (its value read from get, so it also serves a
// GaugeFunc).
type gaugeCollector struct {
	name, help string
	get        func() float64
}

func (e *gaugeCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "gauge")
	w.sample(e.name, nil, nil, e.get())
}

// counterVecCollector writes every series of a labeled counter.
type counterVecCollector struct {
	name, help string
	cv         *CounterVec
}

func (e *counterVecCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "counter")
	for _, c := range e.cv.v.sorted() {
		w.sample(e.name, e.cv.v.labelNames, c.values, c.metric.Get())
	}
}

// gaugeVecCollector writes every series of a labeled gauge.
type gaugeVecCollector struct {
	name, help string
	gv         *GaugeVec
}

func (e *gaugeVecCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "gauge")
	for _, c := range e.gv.v.sorted() {
		w.sample(e.name, e.gv.v.labelNames, c.values, c.metric.Get())
	}
}

// histogramCollector writes a single histogram.
type histogramCollector struct {
	name, help string
	h          *Histogram
}

func (e *histogramCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "histogram")
	writeHistogram(w, e.name, nil, nil, e.h)
}

// histogramVecCollector writes every series of a labeled histogram.
type histogramVecCollector struct {
	name, help string
	labelNames []string
	hv         *HistogramVec
}

func (e *histogramVecCollector) writeTo(w *textWriter) {
	w.header(e.name, e.help, "histogram")
	for _, c := range e.hv.v.sorted() {
		writeHistogram(w, e.name, e.labelNames, c.values, c.metric)
	}
}

// writeHistogram emits the cumulative _bucket lines (one per bound plus +Inf), then
// _sum and _count, for one histogram series. The bucket label "le" carries the upper
// bound; the +Inf bucket equals _count.
func writeHistogram(w *textWriter, name string, labelNames, labelValues []string, h *Histogram) {
	cumulative, sum, count := h.snapshot()

	bucketNames := append(append([]string(nil), labelNames...), "le")
	for i, bound := range h.bounds {
		vals := append(append([]string(nil), labelValues...), formatFloat(bound))
		w.sample(name+"_bucket", bucketNames, vals, float64(cumulative[i]))
	}
	infVals := append(append([]string(nil), labelValues...), "+Inf")
	w.sample(name+"_bucket", bucketNames, infVals, float64(cumulative[len(cumulative)-1]))

	w.sample(name+"_sum", labelNames, labelValues, sum)
	w.sample(name+"_count", labelNames, labelValues, float64(count))
}
