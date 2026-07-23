package logger

import (
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"go.uber.org/zap"
)

// AccessLogMiddleware wraps an http.Handler to:
//   - inject a request-scoped logger (with trace_id, request_id) into ctx
//   - log request_start (Debug) on entry
//   - log request_complete (Info) on exit with status, latency, bytes
//   - fire slow_request (Warn) if the request exceeds slowThreshold
//
// Probe paths (/healthz, /readyz, /metrics) are skipped to avoid log noise.
func AccessLogMiddleware(base *zap.Logger, gatewayAddr string, slowThreshold time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Skip probe/metrics endpoints.
			if isProbe(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()

			traceID := r.Header.Get("X-Trace-ID")
			requestID := r.Header.Get("X-Request-ID")

			reqLogger := base.With(
				zap.String("trace_id", traceID),
				zap.String("request_id", requestID),
				zap.String("gw", gatewayAddr),
			)

			// Debug: request entry — visible when investigating hang issues.
			reqLogger.Debug("request received",
				Event(EventRequestStart),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.String("remote_addr", r.RemoteAddr),
			)

			// Slow request watchdog: fires even at Info level.
			slowTimer := time.AfterFunc(slowThreshold, func() {
				reqLogger.Warn("request exceeds slow threshold",
					Event(EventSlowRequest),
					zap.Duration("elapsed", time.Since(start)),
				)
			})

			// Inject into context for downstream use.
			ctx := WithLogger(r.Context(), reqLogger)
			r = r.WithContext(ctx)

			sw := &statusWriter{ResponseWriter: w, code: http.StatusOK}
			next.ServeHTTP(sw, r)

			slowTimer.Stop()

			elapsed := time.Since(start)
			reqLogger.Info("request completed",
				Event(EventRequestComplete),
				Status(statusCategory(sw.code)),
				zap.Int("status", sw.code),
				zap.Duration("latency", elapsed),
				zap.Int64("bytes", sw.bytesWritten.Load()),
			)
		})
	}
}

// statusWriter wraps http.ResponseWriter to capture the response status code
// and bytes written, without breaking http.Flusher (required for SSE).
type statusWriter struct {
	http.ResponseWriter
	code         int
	bytesWritten atomic.Int64
	wroteHeader  bool
}

func (sw *statusWriter) WriteHeader(code int) {
	if !sw.wroteHeader {
		sw.code = code
		sw.wroteHeader = true
	}
	sw.ResponseWriter.WriteHeader(code)
}

func (sw *statusWriter) Write(b []byte) (int, error) {
	n, err := sw.ResponseWriter.Write(b)
	sw.bytesWritten.Add(int64(n))
	return n, err
}

// Flush implements http.Flusher, required for SSE streaming.
func (sw *statusWriter) Flush() {
	if f, ok := sw.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap supports http.ResponseController (Go 1.20+).
func (sw *statusWriter) Unwrap() http.ResponseWriter {
	return sw.ResponseWriter
}

func isProbe(path string) bool {
	return path == "/healthz" || path == "/readyz" || strings.HasPrefix(path, "/metrics")
}

func statusCategory(code int) string {
	if code >= 200 && code < 400 {
		return StatusOK
	}
	if code >= 400 && code < 500 {
		return StatusFail
	}
	return StatusFail
}
