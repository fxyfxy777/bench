package logger

import (
	"context"

	"go.uber.org/zap"
)

type ctxKey struct{}

// WithLogger stores a logger in the context. The logger should carry
// request-scoped fields (trace_id, request_id, etc.) so all downstream
// log calls automatically include them.
func WithLogger(ctx context.Context, l *zap.Logger) context.Context {
	return context.WithValue(ctx, ctxKey{}, l)
}

// FromContext retrieves the logger from context.
// Returns zap.NewNop() if no logger was stored — this keeps callers safe
// without allocating a global logger.
func FromContext(ctx context.Context) *zap.Logger {
	if l, ok := ctx.Value(ctxKey{}).(*zap.Logger); ok {
		return l
	}
	return zap.NewNop()
}
