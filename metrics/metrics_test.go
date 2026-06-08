// SPDX-License-Identifier: Apache-2.0

package metrics

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// render writes the registry to text and returns it.
func render(t *testing.T, r *Registry) string {
	t.Helper()
	var b bytes.Buffer
	if err := r.WriteText(&b); err != nil {
		t.Fatalf("WriteText: %v", err)
	}
	return b.String()
}

func TestCounter(t *testing.T) {
	r := NewRegistry()
	c := r.NewCounter("liteio_requests_total", "Total requests.")
	c.Inc()
	c.Add(4)
	if got := c.Get(); got != 5 {
		t.Fatalf("Get = %v, want 5", got)
	}
	out := render(t, r)
	wantLines := []string{
		"# HELP liteio_requests_total Total requests.",
		"# TYPE liteio_requests_total counter",
		"liteio_requests_total 5",
	}
	for _, w := range wantLines {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n%s", w, out)
		}
	}
}

func TestCounterNegativePanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("Add of a negative value did not panic")
		}
	}()
	c := &Counter{}
	c.Add(-1)
}

func TestGauge(t *testing.T) {
	r := NewRegistry()
	g := r.NewGauge("liteio_inflight", "In-flight requests.")
	g.Set(10)
	g.Inc()
	g.Dec()
	g.Dec()
	g.Add(0.5)
	if got := g.Get(); got != 9.5 {
		t.Fatalf("Get = %v, want 9.5", got)
	}
	if out := render(t, r); !strings.Contains(out, "liteio_inflight 9.5") {
		t.Errorf("missing gauge sample\n%s", out)
	}
}

func TestGaugeFunc(t *testing.T) {
	r := NewRegistry()
	calls := 0
	r.NewGaugeFunc("liteio_online_drives", "Online drives.", func() float64 {
		calls++
		return 4
	})
	_ = render(t, r)
	_ = render(t, r)
	if calls != 2 {
		t.Fatalf("gauge func called %d times, want once per scrape (2)", calls)
	}
	if out := render(t, r); !strings.Contains(out, "liteio_online_drives 4") {
		t.Errorf("missing gauge-func sample\n%s", out)
	}
}

func TestCounterVec(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("liteio_api_requests_total", "Requests by API and method.", []string{"api", "method"})
	cv.With("GetObject", "GET").Add(3)
	cv.With("PutObject", "PUT").Inc()
	cv.With("GetObject", "GET").Inc()

	out := render(t, r)
	// Series are sorted by label values: GetObject before PutObject.
	getIdx := strings.Index(out, `liteio_api_requests_total{api="GetObject",method="GET"} 4`)
	putIdx := strings.Index(out, `liteio_api_requests_total{api="PutObject",method="PUT"} 1`)
	if getIdx < 0 || putIdx < 0 {
		t.Fatalf("missing labeled samples\n%s", out)
	}
	if getIdx > putIdx {
		t.Errorf("series not sorted by label value\n%s", out)
	}
	// The HELP/TYPE header appears once for the whole family, not per series.
	if n := strings.Count(out, "# TYPE liteio_api_requests_total counter"); n != 1 {
		t.Errorf("TYPE line appears %d times, want 1\n%s", n, out)
	}
}

func TestHistogram(t *testing.T) {
	r := NewRegistry()
	h := r.NewHistogram("liteio_latency_seconds", "Latency.", []float64{0.1, 0.5, 1})
	for _, v := range []float64{0.05, 0.2, 0.2, 0.7, 5} {
		h.Observe(v)
	}
	out := render(t, r)
	// Cumulative buckets: le=0.1 -> 1 (0.05); le=0.5 -> 3 (+0.2,0.2); le=1 -> 4 (+0.7);
	// +Inf -> 5 (+5). sum = 6.15, count = 5.
	want := []string{
		`liteio_latency_seconds_bucket{le="0.1"} 1`,
		`liteio_latency_seconds_bucket{le="0.5"} 3`,
		`liteio_latency_seconds_bucket{le="1"} 4`,
		`liteio_latency_seconds_bucket{le="+Inf"} 5`,
		`liteio_latency_seconds_sum 6.15`,
		`liteio_latency_seconds_count 5`,
		"# TYPE liteio_latency_seconds histogram",
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n%s", w, out)
		}
	}
}

func TestHistogramVec(t *testing.T) {
	r := NewRegistry()
	hv := r.NewHistogramVec("liteio_op_seconds", "Op latency.", []string{"op"}, []float64{0.1, 1})
	hv.With("get").Observe(0.05)
	hv.With("get").Observe(2)
	hv.With("put").Observe(0.5)

	out := render(t, r)
	want := []string{
		`liteio_op_seconds_bucket{op="get",le="0.1"} 1`,
		`liteio_op_seconds_bucket{op="get",le="1"} 1`,
		`liteio_op_seconds_bucket{op="get",le="+Inf"} 2`,
		`liteio_op_seconds_count{op="get"} 2`,
		`liteio_op_seconds_bucket{op="put",le="0.1"} 0`,
		`liteio_op_seconds_bucket{op="put",le="1"} 1`,
		`liteio_op_seconds_count{op="put"} 1`,
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output missing %q\n%s", w, out)
		}
	}
}

func TestBucketsSortedAndInfDropped(t *testing.T) {
	r := NewRegistry()
	// Unsorted input with an explicit +Inf, which must be dropped (it is implicit).
	h := r.NewHistogram("liteio_x_seconds", "X.", []float64{1, 0.1, 0.5})
	h.Observe(0.3)
	out := render(t, r)
	// Bounds emit ascending; +Inf appears once, as the implicit final bucket.
	order := []string{`le="0.1"`, `le="0.5"`, `le="1"`, `le="+Inf"`}
	last := -1
	for _, tok := range order {
		idx := strings.Index(out, tok)
		if idx < 0 {
			t.Fatalf("missing %q\n%s", tok, out)
		}
		if idx < last {
			t.Errorf("bucket %q out of order\n%s", tok, out)
		}
		last = idx
	}
	if n := strings.Count(out, `le="+Inf"`); n != 1 {
		t.Errorf("+Inf bucket appears %d times, want 1", n)
	}
}

func TestLabelAndHelpEscaping(t *testing.T) {
	r := NewRegistry()
	cv := r.NewCounterVec("liteio_escapes_total", "Help with a \\ and a\nnewline.", []string{"path"})
	cv.With(`a"b\c` + "\n").Inc()
	out := render(t, r)
	if !strings.Contains(out, `# HELP liteio_escapes_total Help with a \\ and a\nnewline.`) {
		t.Errorf("help not escaped\n%s", out)
	}
	if !strings.Contains(out, `liteio_escapes_total{path="a\"b\\c\n"} 1`) {
		t.Errorf("label value not escaped\n%s", out)
	}
}

func TestRegistrationOrderStable(t *testing.T) {
	r := NewRegistry()
	r.NewCounter("liteio_b_total", "B.")
	r.NewCounter("liteio_a_total", "A.")
	out := render(t, r)
	if strings.Index(out, "liteio_b_total") > strings.Index(out, "liteio_a_total") {
		t.Errorf("families not in registration order\n%s", out)
	}
}

func TestDuplicateRegistrationPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("duplicate registration did not panic")
		}
	}()
	r := NewRegistry()
	r.NewCounter("liteio_dup_total", "First.")
	r.NewGauge("liteio_dup_total", "Second.")
}

func TestInvalidNamePanics(t *testing.T) {
	for _, name := range []string{"", "1leading_digit", "has space", "has-dash"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("name %q did not panic", name)
				}
			}()
			NewRegistry().NewCounter(name, "x")
		}()
	}
}

func TestInvalidLabelPanics(t *testing.T) {
	for _, label := range []string{"", "__reserved", "has:colon", "1digit"} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("label %q did not panic", label)
				}
			}()
			NewRegistry().NewCounterVec("liteio_ok_total", "x", []string{label})
		}()
	}
}

func TestWrongLabelCountPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("wrong label count did not panic")
		}
	}()
	cv := NewRegistry().NewCounterVec("liteio_two_total", "x", []string{"a", "b"})
	cv.With("only-one")
}

// TestConcurrentObserveAndScrape runs observers against a histogram vec while another
// goroutine scrapes, then asserts the totals are exact: the atomic counters must lose
// no observation under the race detector.
func TestConcurrentObserveAndScrape(t *testing.T) {
	r := NewRegistry()
	hv := r.NewHistogramVec("liteio_race_seconds", "Race.", []string{"op"}, DefBuckets)
	cv := r.NewCounterVec("liteio_race_total", "Race.", []string{"op"})

	const goroutines, each = 8, 1000
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				_ = render(t, r)
			}
		}
	}()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				hv.With("get").Observe(0.01)
				cv.With("get").Inc()
			}
		}()
	}
	wg.Wait()
	close(stop)

	const want = goroutines * each
	if got := cv.With("get").Get(); got != want {
		t.Errorf("counter = %v, want %d", got, want)
	}
	_, _, count := hv.With("get").snapshot()
	if count != want {
		t.Errorf("histogram count = %d, want %d", count, want)
	}
}

func TestValidNames(t *testing.T) {
	valid := []string{"a", "_x", "liteio:http_requests_total", "A9_b"}
	invalid := []string{"", "9a", "a b", "a-b", "a.b"}
	for _, s := range valid {
		if !validName(s) {
			t.Errorf("validName(%q) = false, want true", s)
		}
	}
	for _, s := range invalid {
		if validName(s) {
			t.Errorf("validName(%q) = true, want false", s)
		}
	}
}
