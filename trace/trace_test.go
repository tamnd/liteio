// SPDX-License-Identifier: Apache-2.0

package trace

import (
	"context"
	"testing"
)

func TestNopTracerNoAlloc(t *testing.T) {
	ctx := context.Background()
	allocs := testing.AllocsPerRun(100, func() {
		ctx2, span := Start(ctx, "test.op")
		span.SetAttr("k", "v")
		span.SetStatus(true, "")
		span.End()
		_ = ctx2
	})
	// The no-op path must be zero-allocation.
	if allocs != 0 {
		t.Fatalf("nop tracer allocated %.0f times per Start, want 0", allocs)
	}
}

func TestWithRequestID(t *testing.T) {
	ctx := context.Background()
	if id := RequestID(ctx); id != "" {
		t.Fatalf("empty context: got %q, want empty", id)
	}
	ctx2 := WithRequestID(ctx, "req-abc")
	if id := RequestID(ctx2); id != "req-abc" {
		t.Fatalf("got %q, want req-abc", id)
	}
	// Original context is not polluted.
	if id := RequestID(ctx); id != "" {
		t.Fatalf("original context polluted: %q", id)
	}
}

func TestCustomTracer(t *testing.T) {
	var started []string
	tracer := &recordingTracer{starts: &started}
	SetGlobal(tracer)
	defer SetGlobal(nopTracer{}) // restore

	ctx, span := Start(context.Background(), "my.op")
	span.End()
	_ = ctx
	if len(started) != 1 || started[0] != "my.op" {
		t.Fatalf("starts = %v, want [my.op]", started)
	}
}

type recordingTracer struct{ starts *[]string }

func (r *recordingTracer) Start(ctx context.Context, name string) (context.Context, Span) {
	*r.starts = append(*r.starts, name)
	return ctx, nopSpan{}
}
