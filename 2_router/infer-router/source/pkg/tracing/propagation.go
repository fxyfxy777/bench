package tracing

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// InjectHTTP injects the trace context from ctx into the outgoing HTTP request headers.
// Call this before sending requests to backend services so they can join the same trace.
func InjectHTTP(ctx context.Context, req *http.Request) {
	otel.GetTextMapPropagator().Inject(ctx, propagation.HeaderCarrier(req.Header))
}

// ExtractHTTP extracts the trace context from incoming HTTP request headers into ctx.
// Use this in middleware or handler entry points to continue an incoming trace.
func ExtractHTTP(ctx context.Context, r *http.Request) context.Context {
	return otel.GetTextMapPropagator().Extract(ctx, propagation.HeaderCarrier(r.Header))
}
