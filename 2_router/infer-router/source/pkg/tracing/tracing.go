package tracing

import (
	"context"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// Config mirrors config.TracingConfig but avoids circular imports.
type Config struct {
	Enabled     bool
	Endpoint    string
	ServiceName string
	SampleRate  float64
	Insecure    bool
}

const tracerName = "rl-router"

var globalTracer trace.Tracer = noop.NewTracerProvider().Tracer(tracerName)

// Init initialises the global OTel TracerProvider.
//
// When cfg.Enabled is false, a noop provider is installed (~10ns/span, zero allocation).
// When enabled, a BatchSpanProcessor with OTLP gRPC exporter is configured.
//
// Returns a shutdown function that flushes remaining spans; call it during graceful shutdown.
func Init(ctx context.Context, cfg Config) (shutdown func(context.Context) error, err error) {
	if !cfg.Enabled {
		tp := noop.NewTracerProvider()
		otel.SetTracerProvider(tp)
		globalTracer = tp.Tracer(tracerName)
		return func(context.Context) error { return nil }, nil
	}

	// Build OTLP gRPC exporter options.
	exporterOpts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(cfg.Endpoint),
	}
	if cfg.Insecure {
		exporterOpts = append(exporterOpts, otlptracegrpc.WithDialOption(grpc.WithTransportCredentials(insecure.NewCredentials())))
	}

	exporter, err := otlptracegrpc.New(ctx, exporterOpts...)
	if err != nil {
		return nil, err
	}

	res, err := resource.New(ctx,
		resource.WithAttributes(
			semconv.ServiceNameKey.String(cfg.ServiceName),
			attribute.String("library.language", "go"),
		),
	)
	if err != nil {
		return nil, err
	}

	sampler := sdktrace.ParentBased(sdktrace.TraceIDRatioBased(cfg.SampleRate))

	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sampler),
	)

	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	globalTracer = tp.Tracer(tracerName)

	return tp.Shutdown, nil
}

// Tracer returns the global tracer instance.
func Tracer() trace.Tracer {
	return globalTracer
}

// StartSpan is a convenience wrapper around Tracer().Start().
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return globalTracer.Start(ctx, name, opts...)
}

// SpanFromContext extracts the current span from context.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}
