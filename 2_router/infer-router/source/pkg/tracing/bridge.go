package tracing

import (
	"context"

	"go.opentelemetry.io/otel/trace"
	"go.uber.org/zap"
)

// TraceFields extracts OTel trace_id and span_id from ctx and returns them
// as zap fields. When no active span exists (noop provider or unsampled),
// returns nil — zero allocation on the common "tracing disabled" path.
//
// Usage:
//
//	if fields := tracing.TraceFields(ctx); fields != nil {
//	    reqLog = reqLog.With(fields...)
//	}
func TraceFields(ctx context.Context) []zap.Field {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []zap.Field{
		zap.String("otel_trace_id", sc.TraceID().String()),
		zap.String("otel_span_id", sc.SpanID().String()),
	}
}

// TraceID extracts the OTel trace ID from ctx as a hex string.
// Returns "" if no valid span context exists.
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.TraceID().String()
}

// SpanID extracts the OTel span ID from ctx as a hex string.
// Returns "" if no valid span context exists.
func SpanID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return ""
	}
	return sc.SpanID().String()
}
