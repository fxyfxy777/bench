package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// streamWithStaleRetry wraps proxy.Forward with a single retry on stale connection errors.
// Returns the tracker from the final attempt, the proxy start time, and the error.
// Used by both V1 streaming (handleStreamChat) and V2 non-ACK streaming (handleV2StreamNonACK).
func (s *Server) streamWithStaleRetry(
	w http.ResponseWriter,
	r *http.Request,
	instanceID, endpoint string,
	reqLog *zap.Logger,
) (*streamTracker, time.Time, error) {
	const maxRetries = 1
	var tracker *streamTracker
	var proxyErr error
	var proxyStart time.Time

	for attempt := 0; attempt <= maxRetries; attempt++ {
		tracker = newStreamTracker(w)
		proxyStart = time.Now()
		proxyErr = s.proxy.Forward(tracker, r, endpoint)

		if proxyErr != nil && isStaleConnError(proxyErr) && attempt < maxRetries {
			reqLog.Warn("streaming stale connection, retrying",
				logger.Event(logger.EventProxyForward),
				logger.Status(logger.StatusRetry),
				zap.String("instance", instanceID),
				zap.Int("attempt", attempt+1),
				zap.Error(proxyErr))
			metrics.StaleConnRetries.WithLabelValues("stream").Inc()
			continue
		}
		break
	}

	return tracker, proxyStart, proxyErr
}

// doHTTPWithStaleRetry sends an HTTP request with one retry on stale connection errors.
// Used by streamForward (ACK mode) where httputil.ReverseProxy cannot be used.
// buildReq is called to create the request for each attempt.
func (s *Server) doHTTPWithStaleRetry(
	ctx context.Context,
	reqLog *zap.Logger,
	instanceID string,
	buildReq func() (*http.Request, error),
) (*http.Response, time.Time, error) {
	req, err := buildReq()
	if err != nil {
		return nil, time.Time{}, fmt.Errorf("build backend request: %w", err)
	}

	proxyStart := time.Now()
	resp, err := s.httpClient.Do(req)
	if err != nil && isStaleConnError(err) {
		reqLog.Warn("streamForward: stale connection, retrying",
			logger.Event(logger.EventProxyForward),
			zap.String("instance", instanceID),
			zap.Error(err))
		metrics.StaleConnRetries.WithLabelValues("stream").Inc()

		req, err = buildReq()
		if err != nil {
			return nil, time.Time{}, fmt.Errorf("build backend retry request: %w", err)
		}
		tracing.InjectHTTP(ctx, req)
		proxyStart = time.Now()
		resp, err = s.httpClient.Do(req)
	}
	if err != nil {
		return nil, proxyStart, fmt.Errorf("backend request failed: %w", err)
	}
	return resp, proxyStart, nil
}
