// SPDX-License-Identifier: Apache-2.0

package admin

import (
	"bytes"
	"context"
	"crypto/rand"
	"net/http"
	"strconv"
	"time"

	"github.com/tamnd/liteio/object"
)

// PerfLayer is the subset of the object layer the perf-test endpoint exercises.
type PerfLayer interface {
	MakeBucket(ctx context.Context, bucket string, opts object.MakeBucketOptions) error
	PutObject(ctx context.Context, bucket, key string, r *object.PutReader, opts object.ObjectOptions) (object.ObjectInfo, error)
	GetObject(ctx context.Context, bucket, key string, opts object.ObjectOptions) (*object.GetObjectReader, error)
	DeleteObject(ctx context.Context, bucket, key string, opts object.ObjectOptions) (object.ObjectInfo, error)
	DeleteBucket(ctx context.Context, bucket string, opts object.DeleteBucketOptions) error
}

// WithPerfLayer wires the object layer for the built-in perf test endpoint.
func WithPerfLayer(pl PerfLayer) Option {
	return func(s *Server) { s.perf = pl }
}

// PerfResult is the JSON response body for POST /perf.
type PerfResult struct {
	Workers    int     `json:"workers"`
	ObjectSize int64   `json:"object_size"`
	DurationS  float64 `json:"duration_s"`
	PutMBs     float64 `json:"put_mbs"`
	GetMBs     float64 `json:"get_mbs"`
	PutOps     int64   `json:"put_ops"`
	GetOps     int64   `json:"get_ops"`
}

// startPerf handles POST /admin/v1/perf. It runs a synchronous PUT-then-GET
// load cycle and returns aggregate throughput numbers.
//
// Query parameters:
//
//	?duration=10s    How long to run each phase (default 5s).
//	?size=4096       Object size in bytes (default 4096).
//	?workers=4       Concurrent goroutines per phase (default 4).
func (s *Server) startPerf(w http.ResponseWriter, r *http.Request) {
	if s.perf == nil {
		writeJSON(w, http.StatusNotImplemented, apiError{Code: "NotImplemented", Message: "perf layer not configured"})
		return
	}
	q := r.URL.Query()
	dur := parseDuration(q.Get("duration"), 5*time.Second)
	size := parseInt64(q.Get("size"), 4096)
	workers := parseInt(q.Get("workers"), 4)
	if size <= 0 {
		size = 4096
	}
	if workers <= 0 {
		workers = 4
	}

	ctx := r.Context()
	const bucket = ".liteio-perf-internal"
	// Best-effort cleanup; the bucket may not exist yet.
	_ = s.perf.MakeBucket(ctx, bucket, object.MakeBucketOptions{})
	defer func() { _ = s.perf.DeleteBucket(ctx, bucket, object.DeleteBucketOptions{Force: true}) }()

	// Build an object body of the requested size.
	body := make([]byte, size)
	_, _ = rand.Read(body)

	// PUT phase.
	putOps, putBytes := runPhase(ctx, workers, dur, func(i int) int64 {
		key := "perf-" + strconv.Itoa(i)
		r := bytes.NewReader(body)
		if _, err := s.perf.PutObject(ctx, bucket, key, object.NewPutReader(r, size), object.ObjectOptions{}); err != nil {
			return 0
		}
		return size
	})

	// GET phase: only run if we managed at least one PUT.
	if putOps == 0 {
		writeJSON(w, http.StatusOK, PerfResult{
			Workers: workers, ObjectSize: size, DurationS: dur.Seconds(),
		})
		return
	}
	getOps, getBytes := runPhase(ctx, workers, dur, func(i int) int64 {
		key := "perf-" + strconv.Itoa(i%int(putOps))
		rc, err := s.perf.GetObject(ctx, bucket, key, object.ObjectOptions{})
		if err != nil {
			return 0
		}
		_ = rc.Close()
		return size
	})

	durS := dur.Seconds()
	writeJSON(w, http.StatusOK, PerfResult{
		Workers:    workers,
		ObjectSize: size,
		DurationS:  durS,
		PutMBs:     float64(putBytes) / 1e6 / durS,
		GetMBs:     float64(getBytes) / 1e6 / durS,
		PutOps:     putOps,
		GetOps:     getOps,
	})
}

// runPhase runs fn concurrently across workers goroutines for dur and returns
// total op count and total byte count.
func runPhase(ctx context.Context, workers int, dur time.Duration, fn func(i int) int64) (int64, int64) {
	type result struct {
		ops   int64
		bytes int64
	}
	ch := make(chan result, workers)
	deadline := time.Now().Add(dur)
	for w := 0; w < workers; w++ {
		go func() {
			var res result
			for i := 0; time.Now().Before(deadline) && ctx.Err() == nil; i++ {
				n := fn(i)
				if n > 0 {
					res.ops++
					res.bytes += n
				}
			}
			ch <- res
		}()
	}
	var totalOps, totalBytes int64
	for w := 0; w < workers; w++ {
		r := <-ch
		totalOps += r.ops
		totalBytes += r.bytes
	}
	return totalOps, totalBytes
}

// parseDuration parses s as a Go duration; returns def on parse error or empty s.
func parseDuration(s string, def time.Duration) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return def
	}
	return d
}

func parseInt64(s string, def int64) int64 {
	if s == "" {
		return def
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return def
	}
	return n
}

func parseInt(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}
