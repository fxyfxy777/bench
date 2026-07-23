package logger

import (
	"context"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/status"
)

// UnaryServerInterceptor logs gRPC method name, latency, and status code.
// Successful calls are logged at Debug; failures at Warn.
func UnaryServerInterceptor(base *zap.Logger) grpc.UnaryServerInterceptor {
	return func(
		ctx context.Context,
		req any,
		info *grpc.UnaryServerInfo,
		handler grpc.UnaryHandler,
	) (any, error) {
		start := time.Now()
		resp, err := handler(ctx, req)
		elapsed := time.Since(start)

		code := status.Code(err)
		fields := []zap.Field{
			Event(EventGRPCCall),
			zap.String("method", info.FullMethod),
			zap.Duration("latency", elapsed),
			zap.String("code", code.String()),
		}

		if err != nil {
			base.Warn("gRPC call failed",
				append(fields, Status(StatusFail), zap.Error(err))...)
		} else {
			base.Debug("gRPC call completed",
				append(fields, Status(StatusOK))...)
		}

		return resp, err
	}
}

// UnaryClientInterceptor logs outbound gRPC calls with latency and status.
func UnaryClientInterceptor(base *zap.Logger) grpc.UnaryClientInterceptor {
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
		elapsed := time.Since(start)

		code := status.Code(err)
		fields := []zap.Field{
			Event(EventGRPCCall),
			zap.String("method", method),
			zap.Duration("latency", elapsed),
			zap.String("code", code.String()),
		}

		if err != nil {
			base.Warn("gRPC client call failed",
				append(fields, Status(StatusFail), zap.Error(err))...)
		} else {
			base.Debug("gRPC client call completed",
				append(fields, Status(StatusOK))...)
		}

		return err
	}
}
