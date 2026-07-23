package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/jsonutil"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
)

// pausedErrMsg is the substring present in ErrPaused ("scheduler allocation paused").
// Used for string-based detection when the gateway cannot import the scheduler package.
const pausedErrMsg = "allocation paused"

func grpcStatusFromErr(err error) (*status.Status, bool) {
	if err == nil {
		return nil, false
	}
	if st, ok := status.FromError(err); ok {
		return st, true
	}
	if unwrapped := errors.Unwrap(err); unwrapped != nil {
		return status.FromError(unwrapped)
	}
	return nil, false
}

// isPausedError detects whether an AllocatePD error indicates the scheduler is
// paused. Works for both hybrid (raw error) and gateway-only (gRPC wrapped) modes.
func isPausedError(err error) bool {
	if err == nil {
		return false
	}
	if st, ok := grpcStatusFromErr(err); ok && st.Code() == codes.Aborted {
		return strings.Contains(st.Message(), pausedErrMsg)
	}
	return strings.Contains(err.Error(), pausedErrMsg)
}

// isRetryableAllocPDError returns true if the AllocatePD error is transient and
// worth retrying (e.g., gRPC Unavailable, network timeout, queue timeout).
// Returns false for definitive rejections (paused, queue full, no workers, not serving).
func isRetryableAllocPDError(err error) bool {
	if err == nil {
		return false
	}
	if isPausedError(err) {
		return false
	}
	if st, ok := grpcStatusFromErr(err); ok {
		switch st.Code() {
		case codes.Unavailable, codes.DeadlineExceeded, codes.Canceled:
			return true
		default:
			return false
		}
	}
	msg := err.Error()
	for _, transient := range []string{
		"connection refused",
		"connection reset",
		"i/o timeout",
		"transport is closing",
		"server misbehaving",
		"waiting queue timeout",
	} {
		if strings.Contains(msg, transient) {
			return true
		}
	}
	return false
}

// splitwiseDecodeResponse is the successful decode-side response returned by
// the PD dispatch loop. Release ownership stays on pdAttempt slots; this
// value only carries the data needed to write the client response.
type splitwiseDecodeResponse struct {
	backendResp     *http.Response
	nonStreamBody   []byte
	rescheduleCount int
	prefillID       string
	decodeID        string
}

func (res *splitwiseDecodeResponse) Close() {
	if res == nil || res.backendResp == nil || res.backendResp.Body == nil {
		return
	}
	_ = res.backendResp.Body.Close()
}

// splitwiseDispatchOutcome makes the dispatch loop's terminal states explicit:
// error, already-written final response (pd_paused), or decode response ready.
type splitwiseDispatchOutcome struct {
	decode               *splitwiseDecodeResponse
	ackCommitted         bool
	finalResponseWritten bool
}

// pdRoundResult is the outcome of one executePDRound call. The dispatch loop
// checks Reschedule to decide whether to continue or return.
type pdRoundResult struct {
	Decode     *splitwiseDecodeResponse
	AckSent    bool // true if this round committed HTTP 200 + ACK chunk
	Reschedule bool // true if backend returned pd_reschedule and the pair was released
}

// pdAttempt owns the state for one PD outer attempt. A pd_reschedule
// creates a new pdAttempt with a new allocationRequestID; transient
// AllocatePD retries inside the same attempt keep this ID for idempotency.
type pdAttempt struct {
	attemptNo           int
	allocationRequestID string
	routeCtx            *domain.RouteContext
	decodeSlot          *allocSlot
	prefillSlot         *allocSlot
}

// HandleV2SplitwiseChatCompletion handles /api/v2/chat/completions in PD-split
// mode. The handler delegates lifecycle / audit / release / inflight tracking
// to requestLifecycle and the dispatchSplitwiseRequest inner loop; this function
// itself is responsible only for top-level setup, dispatch, and response
// writing.
func (s *Server) HandleV2SplitwiseChatCompletion(w http.ResponseWriter, r *http.Request) {
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

	var meta v2ChatMeta
	if err := jsonutil.Unmarshal(bodyBytes, &meta); err != nil {
		reqLog.Warn("v2-splitwise-central: invalid JSON body", zap.Error(err))
		writeErrorJSON(w, http.StatusBadRequest,
			"invalid JSON body: "+err.Error(), "invalid_request_error")
		return
	}
	reqLog = reqLog.With(zap.String("inference_id", meta.InferenceID))
	if s.schedulerPaused(pc.resourceGroup) {
		reqLog.Info("v2-splitwise-central: scheduler paused, returning abort response",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("resource_group", pc.resourceGroup))
		s.writePDAbortResponse(w, reqLog, meta, false)
		return
	}
	reqLog.Info("v2-splitwise-central: request accepted",
		zap.Int("body_bytes", len(bodyBytes)),
		zap.Bool("stream", meta.Stream),
		zap.Bool("need_ack", meta.NeedACK))

	var rawReq map[string]any
	if err := jsonutil.Unmarshal(bodyBytes, &rawReq); err != nil {
		reqLog.Error("v2-splitwise-central: failed to parse request body", zap.Error(err))
		writeErrorJSON(w, http.StatusBadRequest,
			"failed to parse request body", "invalid_request_error")
		return
	}

	mode := v2SplitwiseMode(meta)
	lc := s.newRequestLifecycle(ctx, r, bodyBytes, pc.start, reqLog, "v2_splitwise_central_"+mode)
	defer lc.finalize()

	requestText := policy.ExtractPromptFromChatRequest(rawReq)
	var requestTokenIDs []int
	if requestText == "" {
		requestTokenIDs = policy.ExtractTokenIDsFromRequest(rawReq)
	}
	out, callErr := s.dispatchSplitwiseRequest(lc, w, r, meta, rawReq, requestText, requestTokenIDs, bodyBytes)
	if callErr != nil {
		lc.proxyErr = callErr
		writeSplitwiseUpstreamError(w, callErr, out.ackCommitted)
		return
	}
	if out.finalResponseWritten {
		return
	}
	res := out.decode
	if res == nil {
		callErr := errors.New("internal error: splitwise dispatch returned no response")
		lc.proxyErr = callErr
		writeSplitwiseUpstreamError(w, callErr, out.ackCommitted)
		return
	}

	defer res.Close()

	reqLog.Info("v2-splitwise-central: request receiving",
		logger.Event(logger.EventProxyReceiving),
		logger.Status(logger.StatusOK),
		zap.Bool("stream", meta.Stream),
		zap.Bool("needAck", meta.NeedACK))

	proxyErr := s.writeSplitwiseDecodeResponse(reqLog, w, meta, res, out.ackCommitted)
	lc.proxyErr = proxyErr
	if proxyErr != nil {
		src := forensics.ClassifyTransportError(proxyErr, ctx)
		lc.audit.DisconnectSource = string(src)
		lc.audit.DisconnectPhase = string(forensics.PhaseStream)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = proxyErr.Error()
		metrics.DisconnectTotal.WithLabelValues(string(src), "stream", "stream").Inc()
	} else {
		lc.audit.DisconnectSource = string(forensics.DisconnectNone)
	}
	lc.audit.RespondedAt = time.Now()
	lc.audit.BackendStatus = res.backendResp.StatusCode

	reqLog.Info("v2-splitwise-central: request completed",
		logger.Event(logger.EventProxyComplete),
		logger.Status(logger.StatusOK),
		zap.String("mode", mode),
		zap.String("decode_id", res.decodeID),
		zap.String("prefill_id", lc.audit.PrefillInstanceID),
		zap.Int("backend_status", res.backendResp.StatusCode),
		zap.Int("reschedule_count", res.rescheduleCount),
		zap.Duration("latency", time.Since(pc.start)))
}

// dispatchSplitwiseRequest is the outer pd_reschedule retry loop.
// Each iteration: allocate a fresh PD pair → execute the round → return or retry.
func (s *Server) dispatchSplitwiseRequest(
	lc *requestLifecycle,
	w http.ResponseWriter,
	r *http.Request,
	meta v2ChatMeta,
	rawReq map[string]any,
	requestText string,
	requestTokenIDs []int,
	bodyBytes ...[]byte,
) (splitwiseDispatchOutcome, error) {
	maxAttempts := max(1, s.maxPDRescheduleRetries+1)
	ackSent := false

	for attempt := 1; attempt <= maxAttempts; attempt++ {
		pa := s.newPDAttempt(r, lc, meta.SessionID, requestText, requestTokenIDs, attempt, firstBodyBytes(bodyBytes))

		if err := s.allocatePDWithRetry(lc, pa, r, meta.SessionID, requestText, requestTokenIDs, firstBodyBytes(bodyBytes)); err != nil {
			return s.handleAllocFailure(lc, w, meta, ackSent, attempt, err)
		}

		round, err := s.executePDRound(lc, pa, w, r.Header, meta, rawReq, ackSent, maxAttempts-1)
		ackSent = ackSent || round.AckSent
		if err != nil {
			return splitwiseDispatchOutcome{ackCommitted: ackSent}, err
		}
		if round.Reschedule {
			continue
		}
		return splitwiseDispatchOutcome{decode: round.Decode, ackCommitted: ackSent}, nil
	}

	return splitwiseDispatchOutcome{ackCommitted: ackSent},
		fmt.Errorf("pd_reschedule: exceeded max retries (%d)", maxAttempts-1)
}

func (s *Server) handleAllocFailure(
	lc *requestLifecycle,
	w http.ResponseWriter,
	meta v2ChatMeta,
	ackCommitted bool,
	attempt int,
	err error,
) (splitwiseDispatchOutcome, error) {
	out := splitwiseDispatchOutcome{ackCommitted: ackCommitted}
	if isPausedError(err) {
		lc.reqLog.Info("v2-splitwise-central: PD paused, returning abort response",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			zap.String("reason", "pd_paused"),
			zap.Int("attempt", attempt))
		lc.audit.DisconnectSource = "pd_paused"
		s.writePDAbortResponse(w, lc.reqLog, meta, ackCommitted)
		out.finalResponseWritten = true
		return out, nil
	}

	src := forensics.ClassifyAllocError(err, lc.ctx)
	metrics.DisconnectTotal.WithLabelValues(string(src), "stream", "allocate").Inc()
	lc.reqLog.Error("v2-splitwise-central: PD allocation failed",
		logger.Event(logger.EventAllocate),
		logger.Status(logger.StatusFail),
		logger.Reason(logger.ReasonNoHealthyBackend),
		logger.DisconnectSourceField(string(src)),
		zap.Int("attempt", attempt),
		zap.Error(err))
	return out, err
}

func (s *Server) newPDAttempt(
	r *http.Request,
	lc *requestLifecycle,
	sessionID string,
	requestText string,
	requestTokenIDs []int,
	attemptNo int,
	bodyBytes ...[]byte,
) *pdAttempt {
	reqID := lc.audit.RequestID
	if attemptNo > 1 {
		reqID = fmt.Sprintf("%s/pdretry/%d", lc.audit.RequestID, attemptNo-1)
	}
	routeCtx := s.acquireRouteContext(r, reqID, sessionID, requestText, firstBodyBytes(bodyBytes))
	routeCtx.RequestTokenIDs = requestTokenIDs
	return &pdAttempt{
		attemptNo:           attemptNo,
		allocationRequestID: reqID,
		routeCtx:            routeCtx,
	}
}

func (a *pdAttempt) setRouteContext(routeCtx *domain.RouteContext) {
	a.routeCtx = routeCtx
}

func (s *Server) releasePDAttemptRouteContext(attempt *pdAttempt) {
	if attempt == nil || attempt.routeCtx == nil {
		return
	}
	if !s.ownsPDPool {
		domain.ReleaseRouteContext(attempt.routeCtx)
	}
	attempt.routeCtx = nil
}

// allocatePDWithRetry wraps lc.allocatePDAttempt with the per-call retry-on-transient-error
// loop (network blip, queue timeout). Caller invokes this once per outer
// pd_reschedule attempt.
//
// Each inner retry preserves the same allocation-side RequestID so scheduler
// idempotency handles uncertain RPC outcomes like the centralized Allocate
// path. Only the outer pd_reschedule attempt changes allocationRequestID
// (`<base>/pdretry/<K>`) because the previous allocation has been released.
func (s *Server) allocatePDWithRetry(
	lc *requestLifecycle,
	attempt *pdAttempt,
	r *http.Request,
	sessionID string,
	requestText string,
	requestTokenIDs []int,
	bodyBytes ...[]byte,
) error {
	var err error
	maxRetries := max(1, s.maxAllocPDRetries)
	for allocAttemptIndex := range maxRetries {
		allocAttempt := allocAttemptIndex + 1
		if allocAttempt > 1 {
			routeCtx := s.acquireRouteContext(r, attempt.allocationRequestID, sessionID, requestText, firstBodyBytes(bodyBytes))
			routeCtx.RequestTokenIDs = requestTokenIDs
			attempt.setRouteContext(routeCtx)
		}
		err = lc.allocatePDAttempt(attempt)
		s.releasePDAttemptRouteContext(attempt)
		if err == nil {
			return nil
		}
		if !isRetryableAllocPDError(err) {
			return err
		}
		if allocAttempt >= maxRetries {
			break
		}
		jitter := s.nextPDAllocRetryBackoff()
		lc.reqLog.Warn("v2-splitwise-central: PD allocation failed, will retry",
			zap.Int("alloc_attempt", allocAttempt),
			zap.Duration("jitter", jitter),
			zap.Error(err))
		timer := time.NewTimer(jitter)
		select {
		case <-lc.ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return lc.ctx.Err()
		case <-timer.C:
		}
	}
	return err
}

func firstBodyBytes(bodyBytes [][]byte) []byte {
	if len(bodyBytes) == 0 {
		return nil
	}
	return bodyBytes[0]
}

func (lc *requestLifecycle) finalizePDAttempt(attempt *pdAttempt, errorCode string) {
	if attempt == nil {
		lc.resetForPDRetry(errorCode)
		return
	}
	elapsed := time.Since(lc.start).Milliseconds()
	// releasePDSlot Decs slot.im.ActiveRequests on CAS success — do not Dec
	// again here.
	lc.releasePDSlot(attempt.decodeSlot, elapsed, errorCode)
	lc.secondaryErrorCode = errorCode
	lc.releaseSecondarySlot(attempt.prefillSlot, errorCode)

	lc.inst = nil
	lc.allocationID = ""
	lc.instanceID = ""
	lc.endpoint = ""
	lc.decodeSlot = nil
	lc.im = nil
	lc.proxyErr = nil
	lc.clientDisconnect = false
	lc.costOverride = nil
	lc.released = false
	lc.secondary.Store(nil)
	// Refresh per-attempt start so the next attempt's audit latency reflects
	// only its own time on-instance, mirroring resetForRetry.
	lc.attemptStart = time.Now()
}

// executePDRound performs one complete PD forward-and-inspect cycle:
// validate slots → inject disaggregate_info → send ACK → PostToPD →
// check finish_reason. If the backend returns pd_reschedule (and retries
// remain), it releases the pair and sets Reschedule=true.
func (s *Server) executePDRound(
	lc *requestLifecycle,
	pa *pdAttempt,
	w http.ResponseWriter,
	reqHeaders http.Header,
	meta v2ChatMeta,
	rawReq map[string]any,
	ackAlreadySent bool,
	maxRetries int,
) (pdRoundResult, error) {
	// Validate slots.
	if pa.prefillSlot == nil || pa.prefillSlot.inst == nil {
		return pdRoundResult{}, fmt.Errorf("internal error: prefill slot not set after allocatePD")
	}
	if pa.decodeSlot == nil || pa.decodeSlot.inst == nil {
		return pdRoundResult{}, fmt.Errorf("internal error: decode slot not set after allocatePD")
	}
	prefill := pa.prefillSlot.inst
	decode := pa.decodeSlot.inst

	lc.reqLog.Info("v2-splitwise-central: forwarding",
		logger.Event(logger.EventProxyForward),
		zap.String("prefill_id", prefill.ID),
		zap.String("decode_id", decode.ID),
		zap.String("prefill_host", prefill.Host),
		zap.String("decode_host", decode.Host),
		zap.Bool("stream", meta.Stream),
		zap.Bool("need_ack", meta.NeedACK),
		zap.Int("attempt", pa.attemptNo))

	// Build disaggregate_info + marshal request body.
	mutated, err := buildDisaggregateMutator(prefill, decode)(rawReq)
	if err != nil {
		lc.reqLog.Error("v2-splitwise-central: failed to build disaggregate_info",
			logger.Event(logger.EventProxyForward),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonInternalError),
			zap.Error(err))
		lc.audit.ErrorMessage = err.Error()
		lc.secondaryErrorCode = "internal_error"
		lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = "internal_error" }
		return pdRoundResult{}, fmt.Errorf("failed to build disaggregate_info: %w", err)
	}
	reqBody, err := jsonutil.Marshal(mutated)
	if err != nil {
		lc.reqLog.Error("v2-splitwise-central: failed to encode modified request",
			logger.Event(logger.EventProxyForward),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonInternalError),
			zap.Error(err))
		lc.audit.ErrorMessage = err.Error()
		lc.secondaryErrorCode = "internal_error"
		lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = "internal_error" }
		return pdRoundResult{}, fmt.Errorf("failed to encode modified request: %w", err)
	}

	if pa.attemptNo == 1 {
		w.Header().Set("X-Router-Prefill-Instance", prefill.ID)
		w.Header().Set("X-Router-Decode-Instance", decode.ID)
		lc.audit.ForwardedAt = time.Now()
	}

	ackSent := writeSplitwiseACKIfNeeded(w, meta, ackAlreadySent)

	lc.reqLog.Info("v2-splitwise-central: request to fd",
		logger.Event(logger.EventProxyRequest),
		zap.String("prefill_id", prefill.ID),
		zap.String("decode_id", decode.ID),
		zap.String("request_str", string(reqBody)),
		zap.Bool("stream", meta.Stream),
		zap.Bool("need_ack", meta.NeedACK))

	// Forward to PD pair.
	prefillRelease := func() { lc.releaseSecondarySlot(pa.prefillSlot, "") }
	pdRes, err := PostToPD(pdForwardRequest{
		Context:         lc.ctx,
		Client:          s.pdHTTPClient,
		Log:             lc.reqLog,
		Headers:         reqHeaders,
		DecodeEndpoint:  decode.Endpoint,
		PrefillEndpoint: prefill.Endpoint,
		Body:            reqBody,
		Stream:          meta.Stream,
		OnPrefillDone:   prefillRelease,
	})
	if err != nil {
		lc.markPDForwardFailure(err, pa.attemptNo)
		return pdRoundResult{AckSent: ackSent}, err
	}

	// Build decode response.
	res := &splitwiseDecodeResponse{
		backendResp:     pdRes.DecodeResp,
		prefillID:       pa.prefillSlot.instanceID,
		decodeID:        pa.decodeSlot.instanceID,
		rescheduleCount: pa.attemptNo - 1,
	}

	// Stream mode: cannot inspect finish_reason without consuming the SSE body.
	if meta.Stream {
		return pdRoundResult{Decode: res, AckSent: ackSent}, nil
	}

	// Non-stream: read body and check for pd_reschedule.
	body, readErr := io.ReadAll(res.backendResp.Body)
	_ = res.backendResp.Body.Close()
	if readErr != nil {
		lc.markPDReadFailure(readErr, pa.attemptNo)
		return pdRoundResult{AckSent: ackSent}, readErr
	}
	logNonStreamBody(lc.reqLog, body, res.backendResp.StatusCode)

	respFields := forensics.ExtractResponseKeyFields(body, "/v1/chat/completions")
	if respFields.FinishReason == "pd_reschedule" && pa.attemptNo <= maxRetries {
		lc.reqLog.Warn("v2-splitwise-central: pd_reschedule, retrying",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusRetry),
			logger.Reason("pd_reschedule"),
			zap.String("decode_id", lc.instanceID),
			zap.Int("attempt", pa.attemptNo))
		lc.finalizePDAttempt(pa, "pd_reschedule")
		return pdRoundResult{AckSent: ackSent, Reschedule: true}, nil
	}

	if respFields.FinishReason == "pd_reschedule" {
		lc.reqLog.Warn("v2-splitwise-central: pd_reschedule persists after max retries, forwarding as-is",
			zap.Int("max_retries", maxRetries))
	}
	lc.reqLog.Info("v2-splitwise-central: normal finish reason",
		zap.String("finish_reason", respFields.FinishReason))
	if respFields.FinishReason == "abort" {
		lc.reqLog.Info("v2-splitwise-central: abort request",
			zap.String("response_body", string(body)))
	}

	res.backendResp.Body = io.NopCloser(bytes.NewReader(nil))
	res.nonStreamBody = body
	return pdRoundResult{Decode: res, AckSent: ackSent}, nil
}

func writeSplitwiseACKIfNeeded(w http.ResponseWriter, meta v2ChatMeta, ackSent bool) bool {
	if !meta.NeedACK || ackSent {
		return false
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buildACKChunk(meta.Model))
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return true
}

func (lc *requestLifecycle) markPDForwardFailure(err error, attempt int) {
	src := forensics.ClassifyTransportError(err, lc.ctx)
	metrics.DisconnectTotal.WithLabelValues(string(src), "stream", "forward").Inc()
	lc.reqLog.Error("v2-splitwise-central: PostToPD failed",
		logger.Event(logger.EventProxyComplete),
		logger.Status(logger.StatusFail),
		logger.Reason(logger.ReasonProxyError),
		logger.DisconnectSourceField(string(src)),
		zap.Int("attempt", attempt),
		zap.Error(err))
	lc.audit.DisconnectSource = string(src)
	lc.audit.DisconnectPhase = string(forensics.PhaseForward)
	lc.audit.DisconnectAt = time.Now()
	lc.audit.ErrorMessage = err.Error()
	lc.secondaryErrorCode = "transport_error"
	lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = "transport_error" }
}

func (lc *requestLifecycle) markPDReadFailure(err error, attempt int) {
	lc.reqLog.Error("v2-splitwise-central: failed to read non-stream response body",
		zap.Int("attempt", attempt),
		zap.Error(err))
	lc.secondaryErrorCode = "read_error"
	lc.costOverride = func(m *domain.CostMetrics) { m.ErrorCode = "read_error" }
}

// writeSplitwiseDecodeResponse writes the decode-side response back to the
// client. Stream mode runs through pumpSSE; non-stream writes the buffered
// body. ackSent indicates whether HTTP 200 + ACK was already committed (in
// which case headers/StatusCode must NOT be re-written).
func (s *Server) writeSplitwiseDecodeResponse(
	reqLog *zap.Logger,
	w http.ResponseWriter,
	meta v2ChatMeta,
	res *splitwiseDecodeResponse,
	ackSent bool,
) error {
	// ACK already committed — only the body remains to be transferred.
	if ackSent {
		if meta.Stream {
			if err := pumpSSE(w, res.backendResp.Body, nil); err != nil && !errors.Is(err, errClientDisconnect) {
				reqLog.Error("v2-splitwise-central: SSE pump error", zap.Error(err))
				return err
			}
			return nil
		}
		if _, err := w.Write(res.nonStreamBody); err != nil {
			reqLog.Error("v2-splitwise-central: failed to write response body (ack non-stream)", zap.Error(err))
			return err
		}
		return nil
	}

	// Non-ACK path: copy backend headers, set status, then write body.
	for k, v := range res.backendResp.Header {
		if k != "Content-Length" {
			w.Header()[k] = v
		}
	}
	w.Header().Set("Instance-ID", res.decodeID)

	if meta.Stream && res.backendResp.StatusCode != http.StatusOK {
		reqLog.Warn("v2-splitwise-central: backend returned error",
			logger.Event(logger.EventProxyComplete),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonUpstream5xx),
			zap.Int("backend_status", res.backendResp.StatusCode))
		w.WriteHeader(res.backendResp.StatusCode)
		_, _ = io.Copy(w, res.backendResp.Body)
		recordBackendStatus(res.decodeID, res.backendResp.StatusCode)
		return nil
	}

	w.WriteHeader(res.backendResp.StatusCode)
	var writeErr error
	if meta.Stream {
		if err := pumpSSE(w, res.backendResp.Body, nil); err != nil && !errors.Is(err, errClientDisconnect) {
			reqLog.Error("v2-splitwise-central: SSE pump error", zap.Error(err))
			writeErr = err
		}
	} else {
		if _, err := w.Write(res.nonStreamBody); err != nil {
			reqLog.Error("v2-splitwise-central: failed to write response body", zap.Error(err))
			writeErr = err
		}
	}
	recordBackendStatus(res.decodeID, res.backendResp.StatusCode)
	return writeErr
}

// recordBackendStatus records the status-class metric for a backend response.
func recordBackendStatus(instanceID string, code int) {
	if code <= 0 {
		return
	}
	metrics.ProxyBackendStatus.WithLabelValues(instanceID, logger.StatusClassFromCode(code)).Inc()
}

// writeSplitwiseUpstreamError writes either a streaming-format error chunk
// (if ackSent is true and the connection is committed to SSE) or a JSON
// error response. Mirrors the pre-refactor behavior.
func writeSplitwiseUpstreamError(w http.ResponseWriter, callErr error, ackSent bool) {
	if ackSent {
		errEvent := fmt.Sprintf(
			"data: {\"error\":{\"message\":\"%s\",\"type\":\"proxy_error\"}}\n\n",
			strings.ReplaceAll(callErr.Error(), "\"", "'"))
		_, _ = w.Write([]byte(errEvent))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		return
	}
	writeErrorJSON(w, http.StatusBadGateway,
		"failed to connect to backend service", "proxy_error")
}

// releasePDWithRetry is the PD-mode wrapper around Server.retryRelease.
// ReleasePD is idempotent via allocationID, so retries are safe.
// The role label distinguishes prefill vs decode releases in retry logs.
func (s *Server) releasePDWithRetry(ctx context.Context, reqLog *zap.Logger, instanceID, gatewayAddr, allocationID, role string, durationMs int64, errorCode string) {
	target := releaseTarget{
		InstanceID:   instanceID,
		GatewayAddr:  gatewayAddr,
		AllocationID: allocationID,
		Role:         role,
	}
	s.retryRelease(ctx, reqLog, target, func(c context.Context) error {
		return s.pdAllocator.ReleasePD(c, instanceID, gatewayAddr, allocationID, role, durationMs, errorCode)
	})
}

// v2SplitwiseMode returns the mode string for logging and audit records.
func v2SplitwiseMode(meta v2ChatMeta) string {
	if meta.NeedACK {
		if meta.Stream {
			return "ack_stream"
		}
		return "ack"
	}
	if meta.Stream {
		return "stream"
	}
	return "non_stream"
}

// logNonStreamBody logs up to maxNonStreamLogBytes of a non-stream response body.
// It does not write to the client — the caller is responsible for that.
func logNonStreamBody(log *zap.Logger, body []byte, statusCode int) {
	const maxNonStreamLogBytes = 2048
	logStr := string(body)
	if len(body) > maxNonStreamLogBytes {
		logStr = string(body[:maxNonStreamLogBytes]) + "...(truncated)"
	}
	log.Info("v2-splitwise-central: non-stream response body",
		zap.Int("status_code", statusCode),
		zap.Int("total_bytes", len(body)),
		zap.String("response_body", logStr))
}

// writePDAbortResponse writes the appropriate abort response based on request type.
// For stream/ACK requests: SSE chunk with finish_reason=abort.
// For non-stream requests: standard OpenAI JSON with finish_reason=abort.
// writePDAbortResponse writes the appropriate abort response based on request type.
// Delegates to the unified writeAbortResponse.
func (s *Server) writePDAbortResponse(w http.ResponseWriter, reqLog *zap.Logger, meta v2ChatMeta, ackSent bool) {
	s.writeAbortResponse(w, meta.Model, meta.Stream || meta.NeedACK, ackSent)
	reqLog.Info("v2-splitwise-central: write abort response finished",
		zap.String("reason", "pd_paused"),
		zap.Bool("ack_sent", ackSent))
}
