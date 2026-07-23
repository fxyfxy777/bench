package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/jsonutil"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// v2ChatMeta captures the minimal fields PaddleRL sends via OpenAI SDK extra_body.
// All fields are top-level in the request JSON (SDK merges extra_body into body).
type v2ChatMeta struct {
	Model        string          `json:"model"`
	Stream       bool            `json:"stream"`
	NeedACK      bool            `json:"need_ack"`
	SessionID    string          `json:"session_id"`
	InferenceID  string          `json:"inference_id,omitempty"`
	InstanceInfo *v2InstanceInfo `json:"instance_info,omitempty"`
}

// v2InstanceInfo specifies a direct backend instance (Seer mode).
type v2InstanceInfo struct {
	Host      string `json:"host"`
	InferPort int    `json:"infer_port"`
}

// ackChunkModel is cached at init time from v2ChatMeta.Model.
// The ACK SSE chunk format matches rollout-controller exactly.

// v2StreamBufPool pools 32KB buffers for V2 stream forwarding.
var v2StreamBufPool = sync.Pool{
	New: func() any { return make([]byte, 32*1024) },
}

// HandleV2ChatCompletion handles PaddleRL-compatible /api/v2/chat/completions.
//
// Differences from V1 (HandleChatCompletion):
//   - Parses body to extract need_ack, instance_info, session_id, model
//   - ACK mode: writes HTTP 200 + ACK chunk before backend responds
//   - instance_info: direct routing, bypasses allocator
//   - session_id: passed to allocator for SessionAwarePolicy affinity
//   - JSON error responses (OpenAI-compatible format)
func (s *Server) HandleV2ChatCompletion(w http.ResponseWriter, r *http.Request) {
	// PD mode: delegate to the splitwise handler.
	if s.routeMode(r) == RouteModePD {
		s.HandleV2SplitwiseChatCompletion(w, r)
		return
	}

	pc, r := s.runPreChecksWithOptions(w, r, preCheckOptions{skipPauseCheck: true})
	if !pc.passed {
		return
	}
	if pc.limiterOK {
		defer s.limiter.Release()
	}
	ctx := r.Context()
	reqLog := pc.reqLog
	bodyBytes := pc.bodyBytes

	// [2] Parse V2-specific meta fields.
	var meta v2ChatMeta
	if err := jsonutil.Unmarshal(bodyBytes, &meta); err != nil {
		reqLog.Warn("v2: invalid JSON body", zap.Error(err))
		writeErrorJSON(w, http.StatusBadRequest,
			"invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	if s.schedulerPaused(pc.resourceGroup) {
		reqLog.Info("v2: scheduler paused, returning abort response",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("resource_group", pc.resourceGroup))
		s.writeAbortResponse(w, meta.Model, meta.Stream || meta.NeedACK, false)
		return
	}

	// [3] Non-streaming non-ACK: delegate entirely to handleNonStreamChat.
	if !meta.Stream && !meta.NeedACK {
		s.v2DelegateNonStream(w, r, bodyBytes, reqLog, pc.start, meta)
		return
	}

	// [4] Streaming or ACK path — use lifecycle for automatic release + audit.
	lc := s.newRequestLifecycle(ctx, r, bodyBytes, pc.start, reqLog, v2StreamMode(meta))
	defer lc.finalize()

	// [5] Instance resolution: direct routing or allocator.
	if !s.v2ResolveInstance(w, r, reqLog, meta, bodyBytes, lc) {
		return
	}

	reqLog.Debug("v2: forwarding",
		logger.Event(logger.EventProxyForward),
		zap.String("instance", lc.instanceID),
		zap.String("endpoint", lc.endpoint),
		zap.Bool("ack", meta.NeedACK),
		zap.Bool("direct", lc.directMode),
		zap.String("session_id", meta.SessionID))

	// [6] Forward based on ACK flag.
	lc.audit.ForwardedAt = time.Now()
	lc.audit.Attempt = 1

	// Inject data_parallel_rank for sglang multi-DP instances.
	if lc.inst != nil {
		if dpRank := parseDPRankFromID(lc.inst.ID); dpRank >= 0 {
			bodyBytes = injectDPRank(bodyBytes, dpRank)
		}
	}

	var capture streamCapture
	var proxyErr error
	if meta.NeedACK {
		var fwdResult streamForwardResult
		fwdResult, proxyErr = s.handleV2ACKStream(w, r, reqLog, bodyBytes, meta, lc.instanceID, lc.endpoint, &capture)
		lc.audit.BackendStatus = fwdResult.BackendStatus
		lc.audit.StreamResult = fwdResult.StreamResult
	} else {
		proxyErr = s.handleV2StreamNonACK(w, r, reqLog, bodyBytes, lc.instanceID, lc.endpoint, pc.start, &capture)
	}
	lc.proxyErr = proxyErr

	// [7] Populate audit from stream capture + classify disconnect.
	lc.audit.RespondedAt = time.Now()
	lc.populateStreamAudit(&capture)
	s.classifyV2Disconnect(lc, proxyErr)

	// [8] Set cost override for client disconnect distinction.
	if errors.Is(proxyErr, errClientDisconnect) {
		lc.clientDisconnect = true
		lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = "client_disconnect" }
	}
}

// v2StreamMode returns the audit mode string based on the ACK flag.
func v2StreamMode(meta v2ChatMeta) string {
	if meta.NeedACK {
		return "v2_ack"
	}
	return "v2_stream"
}

// v2DelegateNonStream delegates a V2 non-stream non-ACK request to the non-stream
// forwarding loop with V2-style abort handling (paused → 200 + finish_reason=abort).
func (s *Server) v2DelegateNonStream(
	w http.ResponseWriter,
	r *http.Request,
	bodyBytes []byte,
	reqLog *zap.Logger,
	start time.Time,
	meta v2ChatMeta,
) {
	s.handleNonStreamChatInner(w, r, bodyBytes, reqLog, start, "/v1/chat/completions", meta.SessionID, true, meta.Model)
}

// v2ResolveInstance resolves the target instance via direct routing or allocator.
// On success populates lc fields and returns true.
// On failure writes an error response to w and returns false.
// For paused errors, writes the abort response (consistent with PD splitwise path).
func (s *Server) v2ResolveInstance(
	w http.ResponseWriter,
	r *http.Request,
	reqLog *zap.Logger,
	meta v2ChatMeta,
	bodyBytes []byte,
	lc *requestLifecycle,
) bool {
	if meta.InstanceInfo != nil && meta.InstanceInfo.Host != "" && meta.InstanceInfo.InferPort > 0 {
		// Direct routing: bypass allocator entirely.
		endpoint := net.JoinHostPort(meta.InstanceInfo.Host, strconv.Itoa(meta.InstanceInfo.InferPort))
		instanceID := "direct-" + endpoint
		lc.allocateDirect(instanceID, endpoint)
		reqLog.Debug("v2: direct routing via instance_info",
			zap.String("endpoint", endpoint))
		return true
	}

	// Allocate via scheduler (session_id enables SessionAware affinity).
	routeCtx := s.acquireRouteContext(r, lc.audit.RequestID, meta.SessionID, extractRequestText(bodyBytes), bodyBytes)
	routeCtx.RequestTokenIDs = extractRequestTokenIDs(bodyBytes)

	if err := lc.tryAllocate(routeCtx); err != nil {
		if isPausedError(err) {
			reqLog.Info("v2: allocation paused, returning abort response",
				logger.Event(logger.EventAllocate),
				logger.Status(logger.StatusFail),
				zap.String("reason", "paused"))
			lc.audit.DisconnectSource = "paused"
			s.writeAbortResponse(w, meta.Model, meta.Stream || meta.NeedACK, false)
			return false
		}
		src := forensics.ClassifyAllocError(err, lc.ctx)
		code, msg, errType := allocErrorResponse(src, err)
		writeErrorJSON(w, code, msg, errType)
		return false
	}
	return true
}

// classifyV2Disconnect classifies the disconnect source for V2 streaming paths
// and sets audit disconnect fields + increments DisconnectTotal.
func (s *Server) classifyV2Disconnect(lc *requestLifecycle, proxyErr error) {
	if errors.Is(proxyErr, errClientDisconnect) {
		lc.audit.DisconnectSource = string(forensics.DisconnectClient)
		lc.audit.DisconnectPhase = string(forensics.PhaseStream)
		lc.audit.DisconnectAt = time.Now()
		metrics.DisconnectTotal.WithLabelValues("client", "stream", "stream").Inc()
	} else if proxyErr != nil {
		src := forensics.ClassifyTransportError(proxyErr, lc.ctx)
		lc.audit.DisconnectSource = string(src)
		lc.audit.DisconnectPhase = string(forensics.PhaseStream)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = proxyErr.Error()
		metrics.DisconnectTotal.WithLabelValues(string(src), "stream", "stream").Inc()
	} else {
		lc.audit.DisconnectSource = string(forensics.DisconnectNone)
	}
}

// handleV2StreamNonACK forwards a streaming non-ACK request using the transparent
// reverse proxy with streamTracker for metrics (TTFT, stream completion, backend status).
func (s *Server) handleV2StreamNonACK(
	w http.ResponseWriter,
	r *http.Request,
	reqLog *zap.Logger,
	bodyBytes []byte,
	instanceID, endpoint string,
	start time.Time,
	capture *streamCapture,
) error {
	originalPath, originalRawPath := r.URL.Path, r.URL.RawPath
	r.URL.Path = "/v1/chat/completions"
	r.URL.RawPath = ""
	defer func() {
		r.URL.Path = originalPath
		r.URL.RawPath = originalRawPath
	}()
	replayableBody(r, bodyBytes)

	// Use the shared stale-connection retry helper (also used by V1 stream).
	tracker, proxyStart, proxyErr := s.streamWithStaleRetry(w, r, instanceID, endpoint, reqLog)

	// Collect stream stats.
	streamResult := tracker.Result(proxyErr)
	ttft := tracker.TTFT(proxyStart)
	backendStatus := tracker.status

	if proxyErr != nil {
		reqLog.Error("v2: streaming forward failed",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonProxyError),
			zap.String("instance", instanceID),
			zap.String("endpoint", endpoint),
			zap.Int("backend_status", backendStatus),
			zap.Error(proxyErr))
	} else {
		reqLog.Debug("v2: proxy completed",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusOK),
			zap.String("instance", instanceID),
			zap.Int("backend_status", backendStatus),
			zap.String("stream_result", streamResult),
			zap.Int64("bytes_written", tracker.bytesWritten),
			zap.Int64("chunks_written", tracker.chunksWritten))
		if backendStatus > 0 {
			metrics.ProxyBackendStatus.WithLabelValues(
				instanceID, logger.StatusClassFromCode(backendStatus),
			).Inc()
		}
	}

	// Stream-specific metrics.
	if ttft > 0 {
		metrics.TimeToFirstByte.WithLabelValues(instanceID, "stream").Observe(float64(ttft.Milliseconds()))
	}
	metrics.StreamCompletionTotal.WithLabelValues(instanceID, streamResult).Inc()

	// Copy tracker's captured SSE data to caller's capture for usage extraction.
	if capture != nil {
		*capture = tracker.capture
	}

	return proxyErr
}

// handleV2ACKStream implements ACK mode: write 200+ACK chunk first, then stream backend response.
// Cannot use httputil.ReverseProxy because it manages WriteHeader itself.
// Returns the streamForwardResult and non-nil error on proxy failure; the caller handles metrics.
func (s *Server) handleV2ACKStream(
	w http.ResponseWriter,
	r *http.Request,
	reqLog *zap.Logger,
	bodyBytes []byte,
	meta v2ChatMeta,
	instanceID, endpoint string,
	capture *streamCapture,
) (streamForwardResult, error) {
	// Write HTTP 200 + SSE headers + ACK chunk immediately.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	ackChunk := buildACKChunk(meta.Model)
	_, _ = w.Write(ackChunk)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	// Forward to backend.
	fwdResult, proxyErr := s.streamForward(w, r, reqLog, bodyBytes, instanceID, endpoint, capture)
	if proxyErr != nil {
		// Already committed 200, so we can only write an error SSE event.
		errEvent := fmt.Sprintf("data: {\"error\":{\"message\":\"%s\",\"type\":\"proxy_error\"}}\n\n",
			strings.ReplaceAll(proxyErr.Error(), "\"", "'"))
		_, _ = w.Write([]byte(errEvent))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		reqLog.Error("v2: stream forward failed",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonProxyError),
			zap.String("instance", instanceID),
			zap.Int("backend_status", fwdResult.BackendStatus),
			zap.Error(proxyErr))
	}
	return fwdResult, proxyErr
}

// errClientDisconnect is a sentinel error returned by streamForward when the
// client disconnects mid-stream. This is NOT a backend error — callers should
// track it via DisconnectTotal rather than RequestsError to avoid polluting
// error rate alerting with client-side behavior.
var errClientDisconnect = errors.New("client disconnected during stream")

// streamForwardResult carries backend status and stream outcome from streamForward.
// Separated from error to distinguish transport failures from backend-level issues.
type streamForwardResult struct {
	BackendStatus int    // HTTP status code from backend (0 if transport error before response)
	StreamResult  string // "done" / "interrupted" / "error" / "empty"
}

// streamForward creates an HTTP request to the backend and streams the response.
// Used only in ACK mode where we've already committed HTTP 200 to the client.
// On client disconnect (write error), the backend context is cancelled immediately
// to stop GPU inference and avoid blocking on DrainBody.
// Returns errClientDisconnect on client write failure.
// Tracks TTFT and stream completion metrics via instanceID.
// If capture is non-nil, feeds SSE data payloads for usage/finish_reason extraction.
func (s *Server) streamForward(
	w http.ResponseWriter,
	r *http.Request,
	reqLog *zap.Logger,
	bodyBytes []byte,
	instanceID, endpoint string,
	capture *streamCapture,
) (streamForwardResult, error) {
	if s.httpClient == nil {
		return streamForwardResult{}, fmt.Errorf("httpClient not initialized")
	}

	// Build target URL: always forward to /v1/chat/completions on the backend.
	targetURL := "http://" + endpoint + "/v1/chat/completions"
	if hasScheme(endpoint) {
		targetURL = endpoint + "/v1/chat/completions"
	}

	// Create a cancellable context so we can abort the backend on client disconnect.
	backendCtx, cancelBackend := context.WithCancel(r.Context())
	defer cancelBackend()

	backendReq, err := http.NewRequestWithContext(backendCtx, http.MethodPost, targetURL, nil)
	if err != nil {
		return streamForwardResult{}, fmt.Errorf("build backend request: %w", err)
	}
	replayableBody(backendReq, bodyBytes)

	// Copy original headers (excluding hop-by-hop and Content-Length).
	copyRequestHeaders(backendReq.Header, r.Header)
	backendReq.Header.Set("Content-Type", "application/json")
	tracing.InjectHTTP(backendCtx, backendReq)

	proxyStart := time.Now()
	resp, err := s.httpClient.Do(backendReq)
	if err != nil && isStaleConnError(err) {
		// Stale connection from pool — retry once with a fresh connection.
		// Safe because ACK was sent but no backend data has reached the client yet.
		reqLog.Warn("streamForward: stale connection, retrying",
			logger.Event(logger.EventProxyForward),
			zap.String("instance", instanceID),
			zap.Error(err))
		metrics.StaleConnRetries.WithLabelValues("stream").Inc()

		backendReq, err = http.NewRequestWithContext(backendCtx, http.MethodPost, targetURL, nil)
		if err != nil {
			return streamForwardResult{}, fmt.Errorf("build backend retry request: %w", err)
		}
		replayableBody(backendReq, bodyBytes)
		copyRequestHeaders(backendReq.Header, r.Header)
		backendReq.Header.Set("Content-Type", "application/json")
		tracing.InjectHTTP(backendCtx, backendReq)

		proxyStart = time.Now()
		resp, err = s.httpClient.Do(backendReq)
	}
	if err != nil {
		return streamForwardResult{}, fmt.Errorf("backend request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	result := streamForwardResult{BackendStatus: resp.StatusCode}

	// Log non-2xx backend status — this is the #1 cause of "ACK then done with
	// no useful content" reports: backend returns 500 but the client already
	// received HTTP 200 from the ACK, so the error is invisible to the caller.
	if resp.StatusCode >= 300 {
		reqLog.Warn("streamForward: backend returned non-2xx after ACK committed",
			logger.Event(logger.EventBackendNon2xx),
			zap.String("instance", instanceID),
			zap.Int("backend_status", resp.StatusCode),
			zap.String("endpoint", endpoint))
		metrics.ProxyBackendStatus.WithLabelValues(
			instanceID, logger.StatusClassFromCode(resp.StatusCode),
		).Inc()
	}

	// Copy response headers to client (already committed 200, skip WriteHeader).
	copyResponseHeaders(w.Header(), resp.Header)

	// Stream body to client via shared pumpSSE. The observer captures TTFT,
	// detects the [DONE] marker, and feeds the streamCapture if present;
	// classification of the outcome happens after pumpSSE returns.
	firstWrite := true
	seenDone := false
	observer := func(chunk []byte) {
		if firstWrite {
			firstWrite = false
			ttft := time.Since(proxyStart)
			metrics.TimeToFirstByte.WithLabelValues(instanceID, "stream").Observe(float64(ttft.Milliseconds()))
		}
		if !seenDone && bytes.Contains(chunk, []byte("[DONE]")) {
			seenDone = true
		}
		if capture != nil {
			capture.Feed(chunk)
		}
	}

	pumpErr := pumpSSE(w, resp.Body, observer)
	if pumpErr == errClientDisconnect {
		cancelBackend() // abort backend to stop GPU inference
		reqLog.Debug("v2: client write error (likely disconnect)")
		result.StreamResult = "interrupted"
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "interrupted").Inc()
		return result, errClientDisconnect
	}
	if pumpErr != nil {
		result.StreamResult = "error"
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "error").Inc()
		return result, fmt.Errorf("backend body read: %w", pumpErr)
	}

	// Clean EOF — classify the stream outcome.
	switch {
	case seenDone:
		result.StreamResult = "done"
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "done").Inc()
	case firstWrite:
		// Backend returned headers but zero body bytes — likely an
		// error response with empty body or a premature close.
		result.StreamResult = "empty"
		reqLog.Warn("streamForward: backend returned empty response body",
			logger.Event(logger.EventBackendNon2xx),
			zap.String("instance", instanceID),
			zap.Int("backend_status", resp.StatusCode),
			zap.String("endpoint", endpoint))
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "error").Inc()
	case resp.StatusCode >= 300:
		// Backend returned non-2xx with some body — error content
		// was streamed to client but no [DONE] marker seen.
		result.StreamResult = "error"
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "error").Inc()
	default:
		result.StreamResult = "interrupted"
		metrics.StreamCompletionTotal.WithLabelValues(instanceID, "interrupted").Inc()
	}
	return result, nil
}

// writeAbortResponse writes an abort response for paused allocation.
// Unified handler for both PD and centralized paths so abort behavior stays
// consistent regardless of routing mode.
//
//   - stream=true: SSE chunk with finish_reason=abort
//   - stream=false: standard OpenAI JSON with finish_reason=abort
//   - ackSent=true: headers already committed (skip header writes)
//
// All responses include router_generated=true in the body and X-Router-Generated
// header (when headers are not yet committed) so upstream can distinguish
// router-originated aborts from backend-originated ones.
func (s *Server) writeAbortResponse(w http.ResponseWriter, model string, stream bool, ackSent bool) {
	if !ackSent {
		w.Header().Set("X-Router-Generated", "true")
	}
	if stream {
		if !ackSent {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Connection", "keep-alive")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(buildAbortChunk(model))
	} else {
		if !ackSent {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
		}
		_, _ = w.Write(buildAbortNonStreamResponse(model))
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

// hasScheme is defined in proxy.go.
