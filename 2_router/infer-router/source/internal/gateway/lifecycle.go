package gateway

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/logger"
	"github.com/yzx/rl-router/pkg/metrics"
	"github.com/yzx/rl-router/pkg/tracing"
)

// allocSlot represents a single allocated instance slot. PD requests need two
// slots (decode = primary, prefill = secondary); normal requests use only the
// primary fields embedded in requestLifecycle.
//
// PD slots are owned by one pdAttempt. The released guard is slot-local so
// a delayed goroutine from an old attempt cannot release a newer attempt's slot.
type allocSlot struct {
	inst         *domain.Instance
	instanceID   string
	endpoint     string
	allocationID string
	role         string // "" for normal, "prefill"/"decode" for PD
	im           *metrics.InstanceMetrics
	released     atomic.Bool
}

// requestLifecycle carries state through the allocation → forwarding → release → audit
// phases of a single proxied request. Created once per request via newRequestLifecycle;
// caller MUST defer lc.finalize() to guarantee audit emission and allocation release.
//
// Slot model:
//   - The embedded fields (inst / allocationID / instanceID / endpoint / im)
//     form the "primary" slot. For PD requests primary == decode (the slot
//     whose lifetime spans the entire request); for normal requests primary
//     is the only allocation.
//   - decodeSlot/secondary are PD attempt slots. release uses slot-local CAS,
//     while secondary remains atomic only because prefill reader goroutines and
//     the main attempt loop may observe/clear the current pointer concurrently.
type requestLifecycle struct {
	server       *Server
	reqLog       *zap.Logger
	start        time.Time
	attemptStart time.Time // per-attempt start for retry latency tracking
	ctx          context.Context
	mode         string // "stream", "non_stream", "v2_ack", "v2_stream"

	// Audit record — always non-nil after newRequestLifecycle.
	audit *AuditRecord

	// Primary allocation result (set by allocate / allocateDirect / allocatePD).
	// For PD this is the decode slot.
	inst         *domain.Instance
	allocationID string
	instanceID   string
	endpoint     string
	decodeSlot   *allocSlot
	directMode   bool // V2 direct routing — skip release

	// Instance metrics handle (set after successful allocation).
	im *metrics.InstanceMetrics

	// Secondary slot — PD prefill only. nil for normal-mode requests.
	// Stored via atomic.Pointer because readPrefillRecv runs in a goroutine
	// that may finish concurrently with the main goroutine clearing the current
	// attempt in finalizePDAttempt/resetForPDRetry.
	secondary atomic.Pointer[allocSlot]
	pdMode    bool // true once allocatePD has populated primary as decode slot

	// Proxy outcome (set by handler after forwarding).
	proxyErr         error
	clientDisconnect bool // V2: client disconnect is neither OK nor error in instance metrics

	// costOverride allows handlers to inject extra fields into CostMetrics
	// before release (e.g., token counts, custom error codes for non-stream).
	costOverride func(m *domain.CostMetrics)

	// secondaryErrorCode is the errorCode passed to ReleasePD for the secondary
	// (prefill) slot when finalize releases it as a fallback. Set by
	// resetForPDRetry / proxy error paths; defaults to "" (success).
	secondaryErrorCode string

	// Guards.
	released  bool // prevents double-release (primary)
	finalized bool // prevents double-finalize
}

// newRequestLifecycle creates a lifecycle, populates audit identity fields from
// the HTTP request, and starts inflight tracking. Caller MUST defer lc.finalize().
func (s *Server) newRequestLifecycle(
	ctx context.Context,
	r *http.Request,
	bodyBytes []byte,
	start time.Time,
	reqLog *zap.Logger,
	mode string,
) *requestLifecycle {
	audit := AcquireAuditRecord()
	audit.ReceivedAt = start
	audit.TraceID = r.Header.Get("X-Trace-ID")
	audit.RequestID = generateRequestID(r)
	audit.GatewayID = s.gatewayAddr
	audit.OTelTraceID = tracing.TraceID(ctx)
	audit.OTelSpanID = tracing.SpanID(ctx)
	audit.RequestBodySize = len(bodyBytes)
	audit.RequestBodyDigest = forensics.BodyDigest(bodyBytes)
	audit.RequestFields = forensics.ExtractRequestKeyFields(bodyBytes)
	// Merge from extra_body (second priority)
	forensics.MergeExtraBodyFields(&audit.RequestFields, forensics.ExtractExtraBodyFields(bodyBytes))
	// Merge from headers (highest priority)
	forensics.MergeHeaderFields(&audit.RequestFields, forensics.ExtractTrackingHeaders(r.Header))
	audit.IngressHeaders = forensics.CaptureHeaders(r.Header)
	audit.Mode = mode

	if s.inflight != nil {
		s.inflight.Track(audit.RequestID)
	}

	return &requestLifecycle{
		server:       s,
		reqLog:       reqLog,
		start:        start,
		attemptStart: start,
		ctx:          ctx,
		mode:         mode,
		audit:        audit,
	}
}

// acquireRouteContext creates the scheduler allocation request for one attempt.
// requestID is supplied by the lifecycle so audit and allocator idempotency use
// the same identity even when the client did not provide X-Trace-ID.
func (s *Server) acquireRouteContext(
	r *http.Request,
	requestID string,
	sessionID string,
	requestText string,
	bodyBytes []byte,
) *domain.RouteContext {
	routeCtx := domain.AcquireRouteContext()
	routeCtx.TraceID = r.Header.Get("X-Trace-ID")
	routeCtx.RequestID = requestID
	routeCtx.GatewayID = s.gatewayAddr
	routeCtx.SessionID = sessionID
	routeCtx.RequestText = requestText
	routeCtx.ResourceGroup = extractResourceGroup(r, bodyBytes)
	return routeCtx
}

// finalize guarantees audit emission and allocation release. Called via defer.
// Idempotent — safe to call multiple times.
func (lc *requestLifecycle) finalize() {
	if lc.finalized {
		return
	}
	lc.finalized = true

	// Release allocation if not already done.
	lc.releaseAllocation()

	// Untrack inflight.
	if lc.server.inflight != nil && lc.audit != nil {
		lc.server.inflight.Untrack(lc.audit.RequestID)
	}

	// Emit and recycle audit record.
	if lc.audit != nil {
		EmitAudit(lc.server.auditLogger, lc.audit)
		ReleaseAuditRecord(lc.audit)
		lc.audit = nil
	}
}

// allocate performs instance allocation via the server's allocator.
// On success, populates lc.inst, lc.allocationID, lc.instanceID, lc.endpoint, lc.im,
// and increments ActiveRequests.
// On failure, writes an error response to w and returns false.
func (lc *requestLifecycle) allocate(w http.ResponseWriter, routeCtx *domain.RouteContext) bool {
	lc.reqLog.Debug("phase: allocate", logger.Event(logger.EventAllocate))

	inst, allocationID, err := lc.server.allocator.Allocate(lc.ctx, routeCtx)
	if !lc.server.ownsPool {
		domain.ReleaseRouteContext(routeCtx)
	}
	if err != nil {
		src := forensics.ClassifyAllocError(err, lc.ctx)
		lc.reqLog.Error("allocate failed",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonNoHealthyBackend),
			logger.DisconnectSourceField(string(src)),
			zap.Error(err))
		metrics.DisconnectTotal.WithLabelValues(string(src), lc.mode, "allocate").Inc()

		lc.audit.DisconnectSource = string(src)
		lc.audit.DisconnectPhase = string(forensics.PhaseAllocate)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = err.Error()

		code, msg, errType := allocErrorResponse(src, err)
		writeErrorJSON(w, code, msg, errType)
		return false
	}

	lc.inst = inst
	lc.allocationID = allocationID
	lc.instanceID = inst.ID
	lc.endpoint = inst.Endpoint
	lc.im = metrics.GetInstanceMetrics(inst.ID)
	lc.im.ActiveRequests.Inc()

	lc.audit.AllocatedAt = time.Now()
	lc.audit.InstanceID = inst.ID
	lc.audit.Endpoint = inst.Endpoint
	lc.audit.AllocationID = allocationID

	return true
}

// tryAllocate performs instance allocation without writing an HTTP response on
// failure. Returns the allocation error (if any) so the caller can apply
// mode-specific handling (e.g., returning abort response for paused errors).
// On success, populates lc fields identically to allocate.
func (lc *requestLifecycle) tryAllocate(routeCtx *domain.RouteContext) error {
	lc.reqLog.Debug("phase: allocate", logger.Event(logger.EventAllocate))

	inst, allocationID, err := lc.server.allocator.Allocate(lc.ctx, routeCtx)
	if !lc.server.ownsPool {
		domain.ReleaseRouteContext(routeCtx)
	}
	if err != nil {
		src := forensics.ClassifyAllocError(err, lc.ctx)
		lc.reqLog.Error("allocate failed",
			logger.Event(logger.EventAllocate),
			logger.Status(logger.StatusFail),
			logger.Reason(logger.ReasonNoHealthyBackend),
			logger.DisconnectSourceField(string(src)),
			zap.Error(err))
		metrics.DisconnectTotal.WithLabelValues(string(src), lc.mode, "allocate").Inc()

		lc.audit.DisconnectSource = string(src)
		lc.audit.DisconnectPhase = string(forensics.PhaseAllocate)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = err.Error()
		return err
	}

	lc.inst = inst
	lc.allocationID = allocationID
	lc.instanceID = inst.ID
	lc.endpoint = inst.Endpoint
	lc.im = metrics.GetInstanceMetrics(inst.ID)
	lc.im.ActiveRequests.Inc()

	lc.audit.AllocatedAt = time.Now()
	lc.audit.InstanceID = inst.ID
	lc.audit.Endpoint = inst.Endpoint
	lc.audit.AllocationID = allocationID

	return nil
}

// allocateDirect sets lifecycle fields for V2 direct-routing mode (no allocator call).
func (lc *requestLifecycle) allocateDirect(instanceID, endpoint string) {
	lc.directMode = true
	lc.instanceID = instanceID
	lc.endpoint = endpoint
	lc.im = metrics.GetInstanceMetrics(instanceID)
	lc.im.ActiveRequests.Inc()

	lc.audit.AllocatedAt = time.Now()
	lc.audit.InstanceID = instanceID
	lc.audit.Endpoint = endpoint
}

// allocatePD performs a PD instance-pair allocation via lc.server.pdAllocator
// and populates both the primary (decode) and secondary (prefill) slots.
//
// Unlike allocate(), this method does not write an HTTP response on failure —
// the splitwise handler needs to distinguish pd_paused from generic failures
// and emit different responses. The caller inspects the returned error and
// invokes writePDAbortResponse / writeErrorJSON itself.
//
// On success: increments decode ActiveRequests; populates audit
// AllocatedAt/InstanceID/Endpoint/AllocationID for the decode side and
// PrefillInstanceID for the prefill side. Returns nil.
//
// On failure: routeCtx is NOT released here (caller may retry). Audit
// disconnect fields are populated.
func (lc *requestLifecycle) allocatePD(routeCtx *domain.RouteContext) error {
	return lc.allocatePDAttempt(&pdAttempt{routeCtx: routeCtx})
}

func (lc *requestLifecycle) allocatePDAttempt(attempt *pdAttempt) error {
	lc.reqLog.Debug("phase: allocate (pd)", logger.Event(logger.EventAllocate))

	if lc.server.pdAllocator == nil {
		err := errors.New("pd allocator is not configured")
		lc.audit.DisconnectSource = string(forensics.DisconnectBackend)
		lc.audit.DisconnectPhase = string(forensics.PhaseAllocate)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = err.Error()
		return err
	}

	prefill, decode, prefillAllocID, decodeAllocID, err := lc.server.pdAllocator.AllocatePD(lc.ctx, attempt.routeCtx)
	if err != nil {
		src := forensics.ClassifyAllocError(err, lc.ctx)
		lc.audit.DisconnectSource = string(src)
		lc.audit.DisconnectPhase = string(forensics.PhaseAllocate)
		lc.audit.DisconnectAt = time.Now()
		lc.audit.ErrorMessage = err.Error()
		return err
	}

	// Primary = decode (lifetime spans the entire request).
	primary := &allocSlot{
		inst:         decode,
		instanceID:   decode.ID,
		endpoint:     decode.Endpoint,
		allocationID: decodeAllocID,
		role:         "decode",
		im:           metrics.GetInstanceMetrics(decode.ID),
	}
	secondary := &allocSlot{
		inst:         prefill,
		instanceID:   prefill.ID,
		endpoint:     prefill.Endpoint,
		allocationID: prefillAllocID,
		role:         "prefill",
		im:           metrics.GetInstanceMetrics(prefill.ID),
	}
	attempt.decodeSlot = primary
	attempt.prefillSlot = secondary

	lc.inst = decode
	lc.allocationID = decodeAllocID
	lc.instanceID = decode.ID
	lc.endpoint = primary.endpoint
	lc.decodeSlot = primary
	lc.im = primary.im
	lc.im.ActiveRequests.Inc()
	// Track prefill ActiveRequests symmetrically; Dec happens in releasePDSlot
	// when the slot-local CAS flips, regardless of which path releases it.
	secondary.im.ActiveRequests.Inc()
	lc.pdMode = true

	// Secondary = prefill (released early once prefill backend is done).
	lc.secondary.Store(secondary)
	lc.secondaryErrorCode = ""

	if lc.audit.AllocatedAt.IsZero() {
		lc.audit.AllocatedAt = time.Now()
		lc.audit.Attempt = 1
	}
	lc.audit.InstanceID = decode.ID
	lc.audit.Endpoint = lc.endpoint
	lc.audit.AllocationID = decodeAllocID
	lc.audit.PrefillInstanceID = prefill.ID

	return nil
}

func (lc *requestLifecycle) releasePDSlot(slot *allocSlot, durationMs int64, errorCode string) bool {
	if slot == nil {
		return false
	}
	if !slot.released.CompareAndSwap(false, true) {
		return false
	}
	lc.server.releasePDWithRetry(lc.ctx, lc.reqLog,
		slot.instanceID, lc.server.gatewayAddr, slot.allocationID,
		slot.role, durationMs, errorCode)
	// Dec ActiveRequests authoritatively here so prefill/decode are symmetric
	// regardless of which call path won the CAS (early callback, fallback,
	// retry-reset, or final release).
	if slot.im != nil {
		slot.im.ActiveRequests.Dec()
	}
	return true
}

// releaseSecondaryEarly releases the current secondary (prefill) slot
// immediately. New code should prefer releaseSecondarySlot with an
// attempt-captured slot; this wrapper remains for tests and legacy callers.
// Idempotent: subsequent calls (and the finalize fallback) are no-ops thanks to
// the slot-local CAS guard.
//
// errorCode is passed straight through to ReleasePD so the scheduler attributes
// the release to the correct cost bucket. durationMs is currently always 0
// because prefill duration is not tracked separately from the parent request.
func (lc *requestLifecycle) releaseSecondaryEarly(errorCode string) {
	lc.releaseSecondarySlot(lc.secondary.Load(), errorCode)
}

func (lc *requestLifecycle) releaseSecondarySlot(sec *allocSlot, errorCode string) {
	lc.releasePDSlot(sec, 0, errorCode)
}

// releaseSecondaryFallback is invoked from finalize/releaseAllocation. It only
// releases the current secondary if no earlier call has already done so via the
// slot-local CAS.
// Uses lc.secondaryErrorCode (set by error paths) to attribute the release.
func (lc *requestLifecycle) releaseSecondaryFallback() {
	lc.releaseSecondarySlot(lc.secondary.Load(), lc.secondaryErrorCode)
}

// resetForPDRetry releases the current PD pair (with errorCode applied to both
// slots) and clears slot state so the lifecycle can be reused for the next
// pd_reschedule attempt. Audit fields (RetryDetails, AllocatedAt) are preserved
// to maintain a single audit record across retries.
//
// Difference from resetForRetry (normal path):
//   - Both primary (decode) and secondary (prefill) are released — primary via
//     pdAllocator.ReleasePD, secondary via releaseSecondaryEarly.
//   - lc.released is reset so the next allocatePD can succeed.
func (lc *requestLifecycle) resetForPDRetry(errorCode string) {
	// Release primary (decode) using pdAllocator with the supplied errorCode.
	// releasePDSlot internally Decs slot.im.ActiveRequests on CAS success.
	if lc.instanceID != "" && !lc.directMode && lc.pdMode {
		elapsed := time.Since(lc.start).Milliseconds()
		lc.releasePDSlot(lc.decodeSlot, elapsed, errorCode)
	}

	// Release secondary (prefill) — the early-release callback may have already
	// fired for this slot, in which case the slot CAS short-circuits.
	lc.secondaryErrorCode = errorCode
	lc.releaseSecondaryFallback()

	// Clear primary slot state. Audit fields and pdMode are preserved.
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

	// Drop secondary slot. allocatePD will install a fresh slot with its own CAS.
	lc.secondary.Store(nil)
	// Refresh per-attempt start so retry audit latency reflects only the new
	// attempt, not cumulative time across all PD attempts.
	lc.attemptStart = time.Now()
}

// releaseAllocation performs release-with-retry, pool cleanup, and instance
// metrics update (ActiveRequests, DurationMs, RequestsOK/Error).
// Idempotent — guarded by lc.released.
//
// For PD requests, also releases the secondary (prefill) slot as a fallback if
// it was not already released early via releaseSecondaryEarly.
func (lc *requestLifecycle) releaseAllocation() {
	// Secondary fallback runs even if primary is empty/already-released:
	// the two slots are released independently, and a CAS in
	// allocSlot ensures exactly-once.
	lc.releaseSecondaryFallback()

	if lc.released || lc.instanceID == "" {
		return
	}
	lc.released = true

	elapsed := time.Since(lc.start).Milliseconds()

	// Release the scheduler allocation (unless direct-routed).
	if !lc.directMode {
		costMetrics := domain.AcquireCostMetrics()
		costMetrics.DurationMs = elapsed
		if lc.proxyErr != nil {
			costMetrics.ErrorCode = "proxy_error"
		}
		// Apply any extra cost fields set by the handler (e.g., token counts, custom error codes).
		if lc.costOverride != nil {
			lc.costOverride(costMetrics)
		}
		if ce := lc.reqLog.Check(zap.DebugLevel, "phase: release"); ce != nil {
			ce.Write(
				logger.Event(logger.EventRelease),
				zap.String("instance", lc.instanceID),
				zap.String("allocation_id", lc.allocationID),
				zap.String("gateway_addr", lc.server.gatewayAddr),
				zap.Bool("pd_primary", lc.isPDPrimary()),
				zap.Int64("duration_ms", elapsed),
				zap.String("error_code", costMetrics.ErrorCode),
			)
		}
		// PD primary is the decode slot — route through pdAllocator.ReleasePD
		// so the scheduler accounts for role correctly. Normal slots route
		// through the regular allocator.Release path.
		if lc.isPDPrimary() {
			lc.releasePDSlot(lc.decodeSlot, elapsed, costMetrics.ErrorCode)
		} else {
			lc.server.releaseWithRetry(lc.ctx, lc.reqLog, lc.instanceID, lc.server.gatewayAddr, lc.allocationID, costMetrics)
		}
		if lc.isPDPrimary() || !lc.server.ownsPool {
			domain.ReleaseCostMetrics(costMetrics)
		}
	}
	lc.audit.ReleasedAt = time.Now()

	// Instance metrics. For PD primary, releasePDSlot has already Dec'd
	// ActiveRequests via the slot.im path; Dec only on the normal/direct path
	// to avoid double-decrement.
	if lc.im != nil {
		if !lc.isPDPrimary() {
			lc.im.ActiveRequests.Dec()
		}
		lc.im.DurationMs.Observe(float64(elapsed))
		if lc.clientDisconnect {
			// Client disconnect: neither OK nor error — tracked via DisconnectTotal.
		} else if lc.proxyErr != nil {
			lc.im.RequestsError.Inc()
		} else {
			lc.im.RequestsOK.Inc()
		}
	}
}

// isPDPrimary reports whether the primary slot is a PD decode slot.
func (lc *requestLifecycle) isPDPrimary() bool {
	return lc.pdMode
}

// resetForRetry clears allocation state so the lifecycle can be reused for the
// next non-stream retry attempt. The audit record is preserved (RetryDetails accumulate).
func (lc *requestLifecycle) resetForRetry() {
	lc.inst = nil
	lc.allocationID = ""
	lc.instanceID = ""
	lc.endpoint = ""
	lc.im = nil
	lc.proxyErr = nil
	lc.clientDisconnect = false
	lc.costOverride = nil
	lc.released = false
	lc.attemptStart = time.Now()
}

// recordStreamMetrics records TTFT, StreamCompletionTotal, and ProxyBackendStatus.
func (lc *requestLifecycle) recordStreamMetrics(
	ttft time.Duration,
	streamResult string,
	backendStatus int,
) {
	if ttft > 0 && lc.instanceID != "" {
		metrics.TimeToFirstByte.WithLabelValues(lc.instanceID, "stream").Observe(float64(ttft.Milliseconds()))
	}
	if lc.instanceID != "" {
		metrics.StreamCompletionTotal.WithLabelValues(lc.instanceID, streamResult).Inc()
	}
	if backendStatus > 0 && lc.proxyErr == nil && lc.instanceID != "" {
		metrics.ProxyBackendStatus.WithLabelValues(
			lc.instanceID, logger.StatusClassFromCode(backendStatus),
		).Inc()
	}
}

// classifyAndRecordDisconnect sets audit disconnect fields and increments
// the DisconnectTotal metric counter.
func (lc *requestLifecycle) classifyAndRecordDisconnect(
	err error,
	streamDone bool,
) {
	if err == nil {
		lc.audit.DisconnectSource = string(forensics.DisconnectNone)
		return
	}
	src := forensics.ClassifyProxyDisconnect(err, lc.ctx, streamDone)
	lc.audit.DisconnectSource = string(src)
	lc.audit.DisconnectPhase = string(forensics.PhaseStream)
	lc.audit.DisconnectAt = time.Now()
	lc.audit.ErrorMessage = err.Error()
	metrics.DisconnectTotal.WithLabelValues(string(src), lc.mode, "stream").Inc()
}

// populateStreamAudit fills audit fields from a streamCapture (usage, finish_reason, bytes/chunks).
func (lc *requestLifecycle) populateStreamAudit(capture *streamCapture) {
	if capture == nil {
		return
	}
	streamUsage := capture.Usage()
	lc.audit.ResponseFields = streamUsage
	lc.audit.PromptTokens = streamUsage.PromptTokens
	lc.audit.CompletionTokens = streamUsage.CompletionTokens
	if fr := capture.FinishReason(); fr != "" {
		lc.audit.ResponseFields.FinishReason = fr
	}
	lc.audit.StreamBytes = capture.bytesWritten
	lc.audit.StreamChunks = capture.chunksWritten
}
