package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// nonStreamForwardResult holds the outcome of a non-streaming backend call.
type nonStreamForwardResult struct {
	StatusCode int
	Header     http.Header
	Body       []byte
	Usage      tokenUsage
	Err        error
}

// nonStreamBufPool pools response body buffers for non-streaming requests.
// Initial capacity 64KB; buffers exceeding 256KB are discarded on return.
var nonStreamBufPool = sync.Pool{
	New: func() any { return bytes.NewBuffer(make([]byte, 0, 64*1024)) },
}

const nonStreamBufMaxCap = 256 * 1024

// nonStreamMaxRetries is the maximum number of backend retries for non-streaming requests.
const nonStreamMaxRetries = 2

// handleNonStreamChat handles a non-streaming request with backend retry.
// backendPath specifies the path to forward to on the backend (e.g. "/v1/chat/completions",
// "/v1/completions", "/generate").
func (s *Server) handleNonStreamChat(
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	reqLog *zap.Logger,
	start time.Time,
	backendPath string,
) {
	s.handleNonStreamChatInner(w, r, bodyBytes, reqLog, start, backendPath, extractSessionID(bodyBytes), false, "")
}

// handleNonStreamChatInner is the unified non-stream forwarding loop.
// abortOnPause enables chat-completions abort responses on paused errors
// (returning 200 + finish_reason=abort + router_generated=true).
func (s *Server) handleNonStreamChatInner(
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	reqLog *zap.Logger,
	start time.Time,
	backendPath string,
	sessionID string,
	abortOnPause bool,
	abortModel string,
) {
	s.handleBufferedForwardInner(w, r, bodyBytes, reqLog, start, bufferedForwardOptions{
		BackendPath:  backendPath,
		SessionID:    sessionID,
		AbortOnPause: abortOnPause,
		AbortModel:   abortModel,
		Mode:         "non_stream",
	})
}

type bufferedForwardOptions struct {
	BackendPath  string
	SessionID    string
	AbortOnPause bool
	AbortModel   string
	Mode         string
}

func (s *Server) handleBufferedForwardInner(
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	reqLog *zap.Logger,
	start time.Time,
	opts bufferedForwardOptions,
) {
	ctx := r.Context()
	mode := opts.Mode
	if mode == "" {
		mode = "non_stream"
	}
	lc := s.newRequestLifecycle(ctx, r, bodyBytes, start, reqLog, mode)
	// finalize handles: final releaseAllocation + EmitAudit + ReleaseAuditRecord.
	defer lc.finalize()
	baseRequestID := lc.audit.RequestID

	for attempt := 0; attempt <= nonStreamMaxRetries; attempt++ {
		if ctx.Err() != nil {
			s.logNonStreamClientDisconnect(reqLog, lc, attempt)
			return
		}

		requestID := s.buildAttemptRequestID(baseRequestID, attempt, reqLog)

		routeCtx := s.acquireRouteContext(r, requestID, opts.SessionID, extractRequestText(bodyBytes), bodyBytes)
		routeCtx.RequestTokenIDs = extractRequestTokenIDs(bodyBytes)

		if opts.AbortOnPause {
			if err := lc.tryAllocate(routeCtx); err != nil {
				lc.audit.Attempt = attempt + 1
				if isPausedError(err) {
					reqLog.Info("non-stream: allocation paused, returning abort response",
						logger.Event(logger.EventAllocate),
						logger.Status(logger.StatusFail),
						zap.String("reason", "paused"))
					lc.audit.DisconnectSource = "paused"
					abortModel := opts.AbortModel
					if abortModel == "" {
						abortModel = extractModel(bodyBytes)
					}
					s.writeAbortResponse(w, abortModel, false, false)
					return
				}
				src := forensics.ClassifyAllocError(err, ctx)
				code, msg, errType := allocErrorResponse(src, err)
				writeErrorJSON(w, code, msg, errType)
				return
			}
		} else {
			if !lc.allocate(w, routeCtx) {
				lc.audit.Attempt = attempt + 1
				return
			}
		}
		lc.audit.Attempt = attempt + 1
		lc.audit.ForwardedAt = time.Now()

		// Inject data_parallel_rank for sglang multi-DP instances.
		fwdBody := bodyBytes
		if lc.inst != nil {
			if dpRank := parseDPRankFromID(lc.inst.ID); dpRank >= 0 {
				fwdBody = injectDPRank(bodyBytes, dpRank)
			}
		}

		result := s.doBufferedForward(ctx, r.Method, fwdBody, r.Header, lc.endpoint, opts.BackendPath)

		if result.Err != nil && !isRetryableError(result.Err) {
			lc.proxyErr = result.Err
			lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = classifyNonStreamError(result) }
			s.logNonStreamNonRetryable(reqLog, lc, result)
			writeErrorJSON(w, http.StatusBadGateway, "backend request failed", "proxy_error")
			return
		}

		if isRetryableError(result.Err) || isRetryableStatus(result.StatusCode) {
			s.handleNonStreamRetryAttempt(reqLog, lc, result, attempt)
			if attempt < nonStreamMaxRetries {
				continue
			}
			s.logNonStreamAllRetriesFailed(reqLog, lc, result, attempt)
			writeErrorJSON(w, http.StatusBadGateway, "all backends failed", "proxy_error")
			return
		}

		// Success or non-retryable backend error (4xx, 500, etc.): forward as-is.
		s.writeNonStreamSuccess(w, reqLog, lc, result, opts.BackendPath)
		return
	}
}

// doNonStreamForward sends a non-streaming request to the backend and buffers
// the complete response. Reuses s.httpClient connection pool and the shared
// buffer pool for response bodies.
// backendPath is the path to forward to on the backend (e.g. "/v1/chat/completions", "/generate").
func (s *Server) doNonStreamForward(
	ctx context.Context,
	bodyBytes []byte,
	headers http.Header,
	endpoint, backendPath string,
) nonStreamForwardResult {
	return s.doBufferedForward(ctx, http.MethodPost, bodyBytes, headers, endpoint, backendPath)
}

func (s *Server) doBufferedForward(
	ctx context.Context,
	method string,
	bodyBytes []byte,
	headers http.Header,
	endpoint, backendPath string,
) nonStreamForwardResult {
	if method == "" {
		method = http.MethodPost
	}
	targetURL := buildNonStreamURL(endpoint, backendPath)

	req, err := http.NewRequestWithContext(ctx, method, targetURL, nil)
	if err != nil {
		return nonStreamForwardResult{Err: fmt.Errorf("build backend request: %w", err)}
	}
	replayableBody(req, bodyBytes)

	copyRequestHeaders(req.Header, headers)
	req.Header.Set("Content-Type", "application/json")
	tracing.InjectHTTP(ctx, req)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nonStreamForwardResult{Err: fmt.Errorf("backend request failed: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	// Read response body into pooled buffer.
	buf := nonStreamBufPool.Get().(*bytes.Buffer)
	buf.Reset()
	_, err = io.Copy(buf, resp.Body)
	if err != nil {
		returnNonStreamBuf(buf)
		return nonStreamForwardResult{
			StatusCode: resp.StatusCode,
			Header:     resp.Header,
			Err:        fmt.Errorf("read backend response: %w", err),
		}
	}

	respBody := make([]byte, buf.Len())
	copy(respBody, buf.Bytes())
	returnNonStreamBuf(buf)

	usage := extractUsage(respBody, backendPath)

	return nonStreamForwardResult{
		StatusCode: resp.StatusCode,
		Header:     resp.Header,
		Body:       respBody,
		Usage:      usage,
	}
}

// writeNonStreamResponse writes a buffered non-streaming response to the client.
func writeNonStreamResponse(w http.ResponseWriter, result nonStreamForwardResult) {
	for k, vv := range result.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(result.StatusCode)
	_, _ = w.Write(result.Body)
}

// buildNonStreamURL constructs the target URL for non-streaming forwarding.
// backendPath is the path to forward to (e.g. "/v1/chat/completions", "/generate").
func buildNonStreamURL(endpoint, backendPath string) string {
	if hasScheme(endpoint) {
		return endpoint + backendPath
	}
	return "http://" + endpoint + backendPath
}

// returnNonStreamBuf returns a buffer to the pool, discarding oversized buffers
// to prevent memory bloat from occasional large responses.
func returnNonStreamBuf(buf *bytes.Buffer) {
	if buf.Cap() <= nonStreamBufMaxCap {
		nonStreamBufPool.Put(buf)
	}
}

// isRetryableStatus returns true for HTTP status codes that warrant a retry
// on a different backend instance.
func isRetryableStatus(code int) bool {
	return code == http.StatusBadGateway || code == http.StatusServiceUnavailable
}

// isRetryableError returns true for network errors that warrant a retry.
func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if isStaleConnError(err) {
		return true
	}
	return false
}

// classifyNonStreamError returns an error code string for CostMetrics.
func classifyNonStreamError(result nonStreamForwardResult) string {
	if result.Err != nil {
		return "proxy_error"
	}
	return "backend_" + strconv.Itoa(result.StatusCode)
}

// buildAttemptRequestID returns the request ID for a given retry attempt.
// Attempt 0 returns baseRequestID as-is; subsequent attempts append "-retryN"
// and increment the NonStreamRetries counter.
func (s *Server) buildAttemptRequestID(baseRequestID string, attempt int, reqLog *zap.Logger) string {
	if attempt == 0 {
		return baseRequestID
	}
	requestID := baseRequestID + "-retry" + strconv.Itoa(attempt)
	metrics.NonStreamRetries.Inc()
	reqLog.Warn("non-stream: retrying on different instance",
		logger.Event(logger.EventNonStreamRetry),
		zap.Int("attempt", attempt+1),
		zap.String("request_id", requestID))
	return requestID
}

// logNonStreamClientDisconnect handles client disconnect detected in the retry loop.
// Sets audit disconnect fields and increments DisconnectTotal.
func (s *Server) logNonStreamClientDisconnect(reqLog *zap.Logger, lc *requestLifecycle, attempt int) {
	reqLog.Warn("non-stream: client disconnected during retry loop",
		logger.Event(logger.EventNonStreamForward),
		logger.DisconnectSourceField("client"),
		zap.Int("attempt", attempt+1))
	metrics.DisconnectTotal.WithLabelValues("client", "non_stream", "response").Inc()

	lc.audit.DisconnectSource = "client"
	lc.audit.DisconnectPhase = string(forensics.PhaseResponse)
	lc.audit.DisconnectAt = time.Now()
	lc.audit.Attempt = attempt + 1
}

// logNonStreamNonRetryable handles a non-retryable transport error (e.g. invalid URL).
// Sets audit disconnect fields and increments DisconnectTotal. The lifecycle's proxyErr
// and costOverride must be set by the caller before calling this.
func (s *Server) logNonStreamNonRetryable(reqLog *zap.Logger, lc *requestLifecycle, result nonStreamForwardResult) {
	src := forensics.ClassifyTransportError(result.Err, lc.ctx)
	reqLog.Error("non-stream: non-retryable error",
		logger.Event(logger.EventNonStreamForward),
		logger.Status(logger.StatusFail),
		logger.DisconnectSourceField(string(src)),
		zap.String("instance", lc.instanceID),
		zap.Error(result.Err))
	metrics.DisconnectTotal.WithLabelValues(string(src), "non_stream", "forward").Inc()

	lc.audit.DisconnectSource = string(src)
	lc.audit.DisconnectPhase = string(forensics.PhaseForward)
	lc.audit.DisconnectAt = time.Now()
	lc.audit.ErrorMessage = result.Err.Error()
	lc.audit.RespondedAt = time.Now()
}

// handleNonStreamRetryAttempt processes a failed retryable attempt: logs the failure,
// records retry detail in audit, releases the current allocation, and resets the
// lifecycle for the next attempt.
func (s *Server) handleNonStreamRetryAttempt(reqLog *zap.Logger, lc *requestLifecycle, result nonStreamForwardResult, attempt int) {
	reqLog.Warn("non-stream: backend failed, will retry",
		logger.Event(logger.EventNonStreamForward),
		logger.Status(logger.StatusFail),
		zap.String("failed_instance", lc.instanceID),
		zap.Int("failed_status", result.StatusCode),
		zap.Error(result.Err),
		zap.Int("attempt", attempt+1))

	// Record retry detail for audit trail.
	rd := RetryDetail{
		InstanceID: lc.instanceID,
		StatusCode: result.StatusCode,
		LatencyMs:  time.Since(lc.attemptStart).Milliseconds(),
	}
	if result.Err != nil {
		rd.Error = result.Err.Error()
	}
	lc.audit.RetryDetails = append(lc.audit.RetryDetails, rd)

	// Release current allocation with error cost, then reset for next attempt.
	// Always mark as error: retryable status (502/503) with nil Err is still a failure.
	if result.Err != nil {
		lc.proxyErr = result.Err
	} else {
		lc.proxyErr = fmt.Errorf("retryable backend status %d", result.StatusCode)
	}
	lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = classifyNonStreamError(result) }
	lc.releaseAllocation()
	lc.resetForRetry()
}

// logNonStreamAllRetriesFailed handles the case where all retry attempts are exhausted.
// Sets proxyErr, audit disconnect fields, and increments DisconnectTotal.
func (s *Server) logNonStreamAllRetriesFailed(reqLog *zap.Logger, lc *requestLifecycle, result nonStreamForwardResult, attempt int) {
	lc.proxyErr = result.Err

	src := forensics.ClassifyTransportError(result.Err, lc.ctx)
	reqLog.Error("non-stream: all retries failed",
		logger.Event(logger.EventNonStreamForward),
		logger.Status(logger.StatusFail),
		logger.DisconnectSourceField(string(src)),
		zap.Int("total_attempts", attempt+1))
	metrics.DisconnectTotal.WithLabelValues(string(src), "non_stream", "response").Inc()

	lc.audit.DisconnectSource = string(src)
	lc.audit.DisconnectPhase = string(forensics.PhaseResponse)
	lc.audit.DisconnectAt = time.Now()
	if result.Err != nil {
		lc.audit.ErrorMessage = result.Err.Error()
	}
	lc.audit.BackendStatus = result.StatusCode
}

// writeNonStreamSuccess handles the success path: writes the response to the client,
// sets cost metrics, records instance-level token metrics, and populates audit fields.
func (s *Server) writeNonStreamSuccess(
	w http.ResponseWriter,
	reqLog *zap.Logger,
	lc *requestLifecycle,
	result nonStreamForwardResult,
	backendPath string,
) {
	// Set cost override for release — includes error code for 4xx/5xx and token counts.
	lc.costOverride = func(m *domain.CostMetrics) {
		if result.StatusCode >= 400 {
			m.ErrorCode = "backend_" + strconv.Itoa(result.StatusCode)
		}
		m.PromptTokens = result.Usage.PromptTokens
		m.CompletionTokens = result.Usage.CompletionTokens
		m.TotalTokens = result.Usage.TotalTokens
	}

	// Mark proxyErr based on backend status for correct instance metrics.
	if result.StatusCode >= 400 {
		lc.proxyErr = fmt.Errorf("backend returned %d", result.StatusCode)
	}

	// Response body integrity digest.
	respDigest := forensics.BodyDigest(result.Body)

	writeNonStreamResponse(w, result)

	// Record non-stream TTFT and backend status.
	elapsed := time.Since(lc.start).Milliseconds()
	metrics.TimeToFirstByte.WithLabelValues(lc.instanceID, "non_stream").Observe(float64(elapsed))
	metrics.ProxyBackendStatus.WithLabelValues(lc.instanceID, logger.StatusClassFromCode(result.StatusCode)).Inc()

	// Instance-level token metrics (only for successful responses).
	if lc.im != nil && result.StatusCode < 400 {
		if result.Usage.PromptTokens > 0 {
			lc.im.TokensPrompt.Add(float64(result.Usage.PromptTokens))
		}
		if result.Usage.CompletionTokens > 0 {
			lc.im.TokensCompletion.Add(float64(result.Usage.CompletionTokens))
		}
	}

	reqLog.Debug("non-stream: completed",
		logger.Event(logger.EventNonStreamForward),
		logger.Status(logger.StatusOK),
		zap.String("instance", lc.instanceID),
		zap.Int("backend_status", result.StatusCode),
		zap.Int64("prompt_tokens", result.Usage.PromptTokens),
		zap.Int64("completion_tokens", result.Usage.CompletionTokens))

	// Populate audit fields.
	lc.audit.RespondedAt = time.Now()
	lc.audit.BackendStatus = result.StatusCode
	lc.audit.ResponseBodySize = int64(len(result.Body))
	lc.audit.ResponseBodyDigest = respDigest
	lc.audit.PromptTokens = result.Usage.PromptTokens
	lc.audit.CompletionTokens = result.Usage.CompletionTokens
	lc.audit.DisconnectSource = string(forensics.DisconnectNone)
	lc.audit.ResponseFields = forensics.ExtractResponseKeyFields(result.Body, backendPath)
}
