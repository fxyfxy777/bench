package logger

import (
	"context"
	"fmt"
	"net/http"
	"runtime/debug"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yzx/rl-router/pkg/metrics"
)

// RecoveryMiddleware wraps an http.Handler to recover from panics.
// On panic, it logs the stack trace and returns 500 Internal Server Error.
func RecoveryMiddleware(log *zap.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rv := recover(); rv != nil {
					metrics.PanicRecoveries.WithLabelValues("http").Inc()
					log.Error("panic recovered in HTTP handler",
						Event(EventPanicRecovered),
						zap.String("method", r.Method),
						zap.String("path", r.URL.Path),
						zap.String("remote_addr", r.RemoteAddr),
						zap.Any("panic", rv),
						zap.String("stack", string(debug.Stack())))
					http.Error(w, "internal server error", http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// RecoveryUnaryServerInterceptor returns a gRPC server interceptor that
// recovers from panics within unary handlers.
func RecoveryUnaryServerInterceptor(log *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (resp any, err error) {
		defer func() {
			if rv := recover(); rv != nil {
				metrics.PanicRecoveries.WithLabelValues("grpc").Inc()
				log.Error("panic recovered in gRPC handler",
					Event(EventPanicRecovered),
					zap.String("method", info.FullMethod),
					zap.Any("panic", rv),
					zap.String("stack", string(debug.Stack())))
				err = status.Errorf(codes.Internal, "internal error: %v", rv)
			}
		}()
		return handler(ctx, req)
	}
}

// ChainUnaryServer chains multiple gRPC unary server interceptors.
// The first interceptor in the list is the outermost (executed first).
func ChainUnaryServer(interceptors ...grpc.UnaryServerInterceptor) grpc.UnaryServerInterceptor {
	switch len(interceptors) {
	case 0:
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}
	case 1:
		return interceptors[0]
	default:
		return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			buildChain := func(current grpc.UnaryServerInterceptor, next grpc.UnaryHandler) grpc.UnaryHandler {
				return func(currentCtx context.Context, currentReq any) (any, error) {
					return current(currentCtx, currentReq, info, next)
				}
			}
			chain := handler
			for i := len(interceptors) - 1; i >= 0; i-- {
				chain = buildChain(interceptors[i], chain)
			}
			return chain(ctx, req)
		}
	}
}

// statusClassFromCode returns the HTTP status class string for metrics.
func StatusClassFromCode(code int) string {
	switch {
	case code >= 200 && code < 300:
		return "2xx"
	case code >= 300 && code < 400:
		return "3xx"
	case code >= 400 && code < 500:
		return "4xx"
	case code >= 500:
		return "5xx"
	default:
		return fmt.Sprintf("%dxx", code/100)
	}
}
