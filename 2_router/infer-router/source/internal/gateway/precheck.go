package gateway

import (
	"errors"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// preCheckResult holds the outcome of the pre-check chain.
type preCheckResult struct {
	bodyBytes     []byte
	resourceGroup string
	reqLog        *zap.Logger
	start         time.Time
	passed        bool // false means error response was already written
	limiterOK     bool // true when limiter.Acquire succeeded (caller must defer Release)
}

type preCheckOptions struct {
	skipPauseCheck bool
}

// runPreChecks executes the common pre-check chain shared by V1 and V2 handlers:
//
//  1. OTel trace span + logger enrichment
//  2. Rate limiter acquire (blocks until token available or rejected)
//  3. Body read (io.ReadAll)
//  4. Resource-group scoped step serving check, and optional pause check
//
// On failure, writes the error response to w and returns passed=false.
// When passed=true and limiterOK=true, caller MUST defer s.limiter.Release().
func (s *Server) runPreChecks(w http.ResponseWriter, r *http.Request) (preCheckResult, *http.Request) {
	return s.runPreChecksWithOptions(w, r, preCheckOptions{})
}

func (s *Server) runPreChecksWithOptions(
	w http.ResponseWriter,
	r *http.Request,
	opts preCheckOptions,
) (preCheckResult, *http.Request) {
	ctx := r.Context()
	ctx, span := tracing.StartSpan(ctx, "gateway.inference")
	defer span.End()
	r = r.WithContext(ctx)

	start := time.Now()
	reqLog := logger.FromContext(ctx)
	if fields := tracing.TraceFields(ctx); fields != nil {
		reqLog = reqLog.With(fields...)
	}
	// Enrich logger with request identity from headers early — before body read —
	// so that all precheck rejection logs are searchable by trace_id.
	if traceID := r.Header.Get("X-Trace-ID"); traceID != "" {
		reqLog = reqLog.With(zap.String("trace_id", traceID))
	}

	fail := preCheckResult{start: start, reqLog: reqLog}

	// Local rate limit — acquire before body read to save memory.
	var limiterOK bool
	if s.limiter != nil {
		if err := s.limiter.Acquire(ctx); err != nil {
			src := forensics.ClassifyAllocError(err, ctx)
			reqLog.Warn("rate limiter rejected",
				logger.Event(logger.EventAllocate),
				logger.Status(logger.StatusFail),
				logger.DisconnectSourceField(string(src)),
				zap.Error(err))
			metrics.DisconnectTotal.WithLabelValues(string(src), "queue", "queue").Inc()
			switch {
			case errors.Is(err, ErrQueueFull), errors.Is(err, ErrQueueTimeout):
				writeErrorJSON(w, http.StatusTooManyRequests,
					"too many concurrent requests", "rate_limit_error")
			default:
				writeErrorJSON(w, http.StatusServiceUnavailable,
					"request cancelled while queuing", "service_unavailable")
			}
			return fail, r
		}
		limiterOK = true
	}

	// Read body.
	if s.maxRequestBodyBytes > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, s.maxRequestBodyBytes)
	}
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			reqLog.Warn("request body too large",
				zap.Int64("max_request_body_bytes", s.maxRequestBodyBytes),
				zap.Error(err))
			writeErrorJSON(w, http.StatusRequestEntityTooLarge,
				"request body too large", "request_too_large")
			if limiterOK {
				s.limiter.Release()
			}
			return preCheckResult{start: start, reqLog: reqLog}, r
		}
		reqLog.Error("read body failed", zap.Error(err))
		writeErrorJSON(w, http.StatusBadGateway,
			"failed to read request body", "proxy_error")
		// Must release the limiter token we acquired.
		if limiterOK {
			s.limiter.Release()
		}
		return preCheckResult{start: start, reqLog: reqLog}, r
	}

	resourceGroup := extractResourceGroup(r, bodyBytes)

	if !s.schedulerServing(resourceGroup) {
		reqLog.Warn("rejected: scheduler not serving",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonSchedulerNotServing),
			zap.String("resource_group", resourceGroup))
		writeErrorJSON(w, http.StatusServiceUnavailable,
			"scheduler not serving", "service_unavailable")
		if limiterOK {
			s.limiter.Release()
		}
		return fail, r
	}

	if !opts.skipPauseCheck && s.schedulerPaused(resourceGroup) {
		reqLog.Warn("rejected: scheduler paused",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("resource_group", resourceGroup))
		writeErrorJSON(w, http.StatusServiceUnavailable,
			"scheduler paused", "service_unavailable")
		if limiterOK {
			s.limiter.Release()
		}
		return fail, r
	}

	return preCheckResult{
		bodyBytes:     bodyBytes,
		resourceGroup: resourceGroup,
		reqLog:        reqLog,
		start:         start,
		passed:        true,
		limiterOK:     limiterOK,
	}, r
}
