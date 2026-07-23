package metrics

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Namespace: "rl_router",
		Name:      "http_request_duration_ms",
		Help:      "HTTP request duration in milliseconds.",
		Buckets:   prometheus.ExponentialBuckets(1, 2, 16), // 1ms .. 32.7s
	}, []string{"method", "path", "status"})

	HTTPRequestsInFlight = promauto.NewGauge(prometheus.GaugeOpts{
		Namespace: "rl_router",
		Name:      "http_requests_in_flight",
		Help:      "Current number of in-flight HTTP requests.",
	})
)

// statusRecorder wraps http.ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (sr *statusRecorder) WriteHeader(code int) {
	sr.status = code
	sr.ResponseWriter.WriteHeader(code)
}

// Unwrap supports http.ResponseController and middleware that calls
// http.ResponseWriter interface checks (e.g. http.Flusher).
func (sr *statusRecorder) Unwrap() http.ResponseWriter {
	return sr.ResponseWriter
}

// normalizePath returns a low-cardinality path label.
// Strips query strings and normalizes known API patterns.
func normalizePath(path string) string {
	// Truncate query string.
	if i := indexOf(path, '?'); i >= 0 {
		path = path[:i]
	}
	// Keep well-known short paths as-is; for very long or
	// dynamic paths, return a generic bucket to limit cardinality.
	if len(path) > 64 {
		return "/other"
	}
	return path
}

func indexOf(s string, c byte) int {
	for i := range len(s) {
		if s[i] == c {
			return i
		}
	}
	return -1
}

// HTTPMetricsMiddleware returns middleware that records per-request duration
// and tracks in-flight request count.
func HTTPMetricsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		HTTPRequestsInFlight.Inc()
		start := time.Now()

		sr := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sr, r)

		HTTPRequestsInFlight.Dec()
		elapsedMs := float64(time.Since(start).Microseconds()) / 1000.0
		path := normalizePath(r.URL.Path)
		HTTPRequestDuration.WithLabelValues(
			r.Method, path, strconv.Itoa(sr.status),
		).Observe(elapsedMs)
	})
}
