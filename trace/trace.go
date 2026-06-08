// SPDX-License-Identifier: Apache-2.0

// Package trace is liteio's thin request-tracing facade. It wraps OpenTelemetry
// concepts behind a small interface so the data path does not import the full OTel
// SDK at build time. The default implementation is a no-op (zero allocation, zero
// overhead); building with the `otel` tag wires in a real OTLP exporter.
//
// Usage:
//
//	ctx, span := trace.Start(ctx, "S3.PutObject")
//	defer span.End()
//	span.SetAttr("bucket", bucket)
package trace

import "context"

// Span is a single unit of work in a distributed trace.
type Span interface {
	// SetAttr records a string key-value attribute on the span.
	SetAttr(key, value string)
	// SetStatus marks the span as failed with msg.
	SetStatus(ok bool, msg string)
	// End records the span's end time and flushes it.
	End()
}

// Tracer creates spans.
type Tracer interface {
	Start(ctx context.Context, name string) (context.Context, Span)
}

// nopSpan is a zero-cost span.
type nopSpan struct{}

func (nopSpan) SetAttr(_, _ string)        {}
func (nopSpan) SetStatus(_ bool, _ string) {}
func (nopSpan) End()                       {}

// nopTracer is the default Tracer: Start returns ctx unchanged and a nopSpan.
type nopTracer struct{}

func (nopTracer) Start(ctx context.Context, _ string) (context.Context, Span) {
	return ctx, nopSpan{}
}

// global is the active Tracer; replaced by SetGlobal.
var global Tracer = nopTracer{}

// SetGlobal replaces the global tracer. Call it once at startup with an OTel
// tracer (using the `otel` build tag) or any Tracer implementation.
func SetGlobal(t Tracer) { global = t }

// Start creates a new span using the global tracer.
func Start(ctx context.Context, name string) (context.Context, Span) {
	return global.Start(ctx, name)
}

// contextKey is the key type for storing a request ID in context.
type contextKey struct{}

// WithRequestID returns a context carrying the given request ID so log lines
// and trace spans can correlate to the x-amz-request-id header.
func WithRequestID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

// RequestID extracts the request ID from ctx. Returns "" if none is stored.
func RequestID(ctx context.Context) string {
	s, _ := ctx.Value(contextKey{}).(string)
	return s
}
