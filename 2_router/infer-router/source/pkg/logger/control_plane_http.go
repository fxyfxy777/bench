package logger

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
)

// ControlPlaneHTTPLogHandler logs full scheduler control-plane HTTP requests
// and responses. It restores r.Body before invoking next, so existing handlers
// can decode the request normally after the middleware inspects it.
func ControlPlaneHTTPLogHandler(base *zap.Logger, route string, next http.HandlerFunc) http.HandlerFunc {
	return ControlPlaneHTTPLogMiddleware(base, route)(next).ServeHTTP
}

func ControlPlaneHTTPLogMiddleware(base *zap.Logger, route string) func(http.Handler) http.Handler {
	if base == nil {
		base = zap.NewNop()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if shouldSkipControlPlaneHTTPLog(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}

			start := time.Now()
			requestBody, readErr := readAndRestoreHTTPBody(r)
			rec := &controlPlaneResponseRecorder{
				ResponseWriter: w,
				status:         http.StatusOK,
			}
			next.ServeHTTP(rec, r)

			fields := []zap.Field{
				Event(EventControlPlaneHTTP),
				Status(statusCategory(rec.status)),
				zap.String("route", route),
				zap.String("method", r.Method),
				zap.String("path", r.URL.Path),
				zap.String("raw_query", r.URL.RawQuery),
				zap.String("request_uri", r.RequestURI),
				zap.String("host", r.Host),
				zap.String("proto", r.Proto),
				zap.String("remote_addr", r.RemoteAddr),
				zap.Int64("request_content_length", r.ContentLength),
				zap.Any("request_headers", r.Header.Clone()),
				zap.Int("request_body_bytes", len(requestBody)),
				zap.String("request_body", string(requestBody)),
				zap.Int("status", rec.status),
				zap.Duration("latency", time.Since(start)),
				zap.Any("response_headers", rec.Header().Clone()),
				zap.Int("response_body_bytes", rec.body.Len()),
				zap.String("response_body", rec.body.String()),
			}
			if readErr != nil {
				fields = append(fields, zap.Error(readErr))
			}
			base.Info("scheduler control-plane http request completed", fields...)
		})
	}
}

type controlPlaneResponseRecorder struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
	body        bytes.Buffer
}

func (r *controlPlaneResponseRecorder) WriteHeader(status int) {
	if r.wroteHeader {
		return
	}
	r.status = status
	r.wroteHeader = true
	r.ResponseWriter.WriteHeader(status)
}

func (r *controlPlaneResponseRecorder) Write(p []byte) (int, error) {
	if !r.wroteHeader {
		r.WriteHeader(http.StatusOK)
	}
	n, err := r.ResponseWriter.Write(p)
	if n > 0 {
		_, _ = r.body.Write(p[:n])
	}
	return n, err
}

func (r *controlPlaneResponseRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (r *controlPlaneResponseRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}

func readAndRestoreHTTPBody(r *http.Request) ([]byte, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	body, err := io.ReadAll(r.Body)
	r.Body = io.NopCloser(bytes.NewReader(body))
	return body, err
}

func shouldSkipControlPlaneHTTPLog(path string) bool {
	return path == "/health" ||
		path == "/healthz" ||
		path == "/readyz" ||
		path == "/version" ||
		strings.HasPrefix(path, "/metrics") ||
		strings.HasPrefix(path, "/debug/pprof/")
}
