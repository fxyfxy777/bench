package metrics

import (
	"context"
	"path"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// UnaryServerMetricsInterceptor returns a gRPC server interceptor that records
// request count (by method and status code) and latency (by method).
func UnaryServerMetricsInterceptor() grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0

		method := path.Base(info.FullMethod)
		code := status.Code(err).String()

		GRPCServerRequestsTotal.WithLabelValues(method, code).Inc()
		GRPCServerRequestDuration.WithLabelValues(method).Observe(elapsedMs)

		return resp, err
	}
}

// UnaryClientMetricsInterceptor returns a gRPC client interceptor that records
// request count (by method and status code) and latency (by method).
func UnaryClientMetricsInterceptor() grpc.UnaryClientInterceptor {
	return func(
		ctx context.Context,
		method string,
		req, reply any,
		cc *grpc.ClientConn,
		invoker grpc.UnaryInvoker,
		opts ...grpc.CallOption,
	) error {
		start := time.Now()
		err := invoker(ctx, method, req, reply, cc, opts...)
		elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0

		shortMethod := path.Base(method)
		code := status.Code(err).String()

		GRPCClientRequestsTotal.WithLabelValues(shortMethod, code).Inc()
		GRPCClientRequestDuration.WithLabelValues(shortMethod).Observe(elapsedMs)

		return err
	}
}
