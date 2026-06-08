// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"io"
	"testing"
)

// BenchmarkCounterAdd measures the hot instrumentation path: a contended counter add.
func BenchmarkCounterAdd(b *testing.B) {
	c := &Counter{}
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			c.Inc()
		}
	})
}

// BenchmarkHistogramObserve measures an observation, the cost added to a request path.
func BenchmarkHistogramObserve(b *testing.B) {
	h := newHistogram(DefBuckets)
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.Observe(0.042)
		}
	})
}

// BenchmarkCounterVecWith measures resolving a child from a labeled family, the cost
// per instrumented call when the series already exists.
func BenchmarkCounterVecWith(b *testing.B) {
	cv := NewRegistry().NewCounterVec("liteio_bench_total", "Bench.", []string{"api", "method"})
	cv.With("GetObject", "GET") // create the series up front
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			cv.With("GetObject", "GET").Inc()
		}
	})
}

// BenchmarkWriteText measures a scrape over a registry shaped like the real one: a
// handful of vec families each with several series.
func BenchmarkWriteText(b *testing.B) {
	r := NewRegistry()
	reqs := r.NewCounterVec("liteio_api_requests_total", "Requests.", []string{"api", "method"})
	errs := r.NewCounterVec("liteio_api_errors_total", "Errors.", []string{"api", "code"})
	lat := r.NewHistogramVec("liteio_api_seconds", "Latency.", []string{"api"}, DefBuckets)
	apis := []string{"GetObject", "PutObject", "ListObjectsV2", "DeleteObject", "HeadObject"}
	for _, api := range apis {
		reqs.With(api, "GET").Add(1000)
		errs.With(api, "NoSuchKey").Add(3)
		for i := 0; i < 100; i++ {
			lat.With(api).Observe(0.01)
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := r.WriteText(io.Discard); err != nil {
			b.Fatal(err)
		}
	}
}
