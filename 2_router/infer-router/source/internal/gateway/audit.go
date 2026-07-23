package gateway

import (
	"sync"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"

	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/logger"
)

// RetryDetail records one failed attempt in the non-stream retry loop.
type RetryDetail struct {
	InstanceID string `json:"instance"`
	StatusCode int    `json:"status"`
	Error      string `json:"error,omitzero"`
	LatencyMs  int64  `json:"latency_ms"`
}

// AuditRecord captures the complete lifecycle of a single proxied request.
// A sync.Pool is used to avoid per-request heap allocations.
//
// After all fields are populated, call EmitAudit to output a single
// event=AUDIT JSON log line to the non-sampled audit logger.
//
// Fields are grouped by namespace in EmitAudit output:
//   - request:    client input (model, max_tokens, headers, body digest)
//   - response:   backend output (id, finish_reason, tokens, body digest)
//   - stream:     SSE stream state (result, bytes, chunks)
//   - routing:    where it went (instance, endpoint, attempt, retry_details)
//   - disconnect: who disconnected and when (source, phase, at)
//   - timeline:   timing (received_at, *_ms offsets, total_ms)
type AuditRecord struct {
	// Identity.
	TraceID     string
	RequestID   string
	OTelTraceID string
	OTelSpanID  string
	GatewayID   string

	// Timeline — all absolute timestamps; EmitAudit converts to ms-offsets.
	ReceivedAt  time.Time
	QueuedAt    time.Time // zero if no rate limiter
	DequeuedAt  time.Time // zero if no rate limiter
	AllocatedAt time.Time
	ForwardedAt time.Time
	RespondedAt time.Time
	ReleasedAt  time.Time

	// Routing.
	InstanceID        string
	Endpoint          string
	AllocationID      string
	Attempt           int    // 1-based; >1 means retried
	PrefillInstanceID string // PD only — prefill instance for splitwise requests; "" for normal

	// Result.
	Mode             string // "stream" / "non_stream" / "v2_ack" / "v2_stream"
	BackendStatus    int
	StreamResult     string // "done" / "interrupted" / "error" / "" for non-stream
	StreamChunks     int64
	StreamBytes      int64
	DisconnectSource string // forensics.DisconnectSource value
	ErrorMessage     string
	PromptTokens     int64
	CompletionTokens int64

	// === Phase 3.5 new fields ===

	// request namespace: what the client sent.
	RequestFields     forensics.RequestKeyFields
	RequestBodySize   int
	RequestBodyDigest string
	IngressHeaders    map[string]string

	// response namespace: what the backend returned.
	ResponseFields     forensics.ResponseKeyFields
	ResponseBodySize   int64
	ResponseBodyDigest string

	// disconnect namespace: disconnect attribution.
	DisconnectPhase string
	DisconnectAt    time.Time // absolute time; zero = normal completion

	// routing namespace: retry details.
	RetryDetails []RetryDetail
}

var auditRecordPool = sync.Pool{New: func() any { return new(AuditRecord) }}

// AcquireAuditRecord returns a zeroed AuditRecord from the pool.
func AcquireAuditRecord() *AuditRecord {
	r := auditRecordPool.Get().(*AuditRecord)
	*r = AuditRecord{} // zero all fields
	return r
}

// ReleaseAuditRecord returns an AuditRecord to the pool.
func ReleaseAuditRecord(r *AuditRecord) {
	if r == nil {
		return
	}
	r.IngressHeaders = nil // release map for GC
	r.RetryDetails = nil   // release slice for GC
	auditRecordPool.Put(r)
}

// EmitAudit writes a single event=AUDIT Info log line containing the complete
// request lifecycle. Uses zap.Object to create sibling namespace groups so that
// humans and AI can instantly identify which phase each field belongs to.
//
// Namespace structure (all siblings at top level):
//
//	request.*    — client input
//	response.*   — backend output
//	stream.*     — SSE stream state
//	routing.*    — instance selection + retry details
//	disconnect.* — disconnect attribution
//	timeline.*   — timing (received_at absolute + ms offsets)
func EmitAudit(auditLog *zap.Logger, r *AuditRecord) {
	if auditLog == nil || r == nil {
		return
	}

	base := r.ReceivedAt

	auditLog.Info("request lifecycle audit",
		// Identity (top-level).
		logger.Event(logger.EventAudit),
		zap.String("trace_id", r.TraceID),
		zap.String("request_id", r.RequestID),
		zap.String("otel_trace_id", r.OTelTraceID),
		zap.String("otel_span_id", r.OTelSpanID),
		zap.String("gateway_id", r.GatewayID),
		zap.String("mode", r.Mode),

		// Sibling namespace groups via zap.Object.
		zap.Object("request", requestMarshaler{r}),
		zap.Object("response", responseMarshaler{r}),
		zap.Object("stream", streamMarshaler{r}),
		zap.Object("routing", routingMarshaler{r}),
		zap.Object("disconnect", disconnectMarshaler{r}),
		zap.Object("timeline", timelineMarshaler{r, base}),
	)
}

// ---------- ObjectMarshaler implementations ----------

type requestMarshaler struct{ r *AuditRecord }

func (m requestMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	rf := &m.r.RequestFields
	enc.AddString("model", rf.Model)
	if rf.MaxTokens != nil {
		enc.AddInt64("max_tokens", *rf.MaxTokens)
	}
	if rf.MaxCompletionTokens != nil {
		enc.AddInt64("max_completion_tokens", *rf.MaxCompletionTokens)
	}
	if rf.Temperature != nil {
		enc.AddFloat64("temperature", *rf.Temperature)
	}
	if rf.TopP != nil {
		enc.AddFloat64("top_p", *rf.TopP)
	}
	if rf.TopK != nil {
		enc.AddInt64("top_k", *rf.TopK)
	}
	if rf.RepetitionPenalty != nil {
		enc.AddFloat64("repetition_penalty", *rf.RepetitionPenalty)
	}
	if rf.FrequencyPenalty != nil {
		enc.AddFloat64("frequency_penalty", *rf.FrequencyPenalty)
	}
	if rf.PresencePenalty != nil {
		enc.AddFloat64("presence_penalty", *rf.PresencePenalty)
	}
	if rf.N != nil {
		enc.AddInt64("n", *rf.N)
	}

	// === Tracing and correlation fields ===
	if rf.InferenceID != "" {
		enc.AddString("inference_id", rf.InferenceID)
	}
	if rf.RolloutID != "" {
		enc.AddString("rollout_id", rf.RolloutID)
	}
	if rf.SessionID != "" {
		enc.AddString("session_id", rf.SessionID)
	}
	if rf.RequestID != "" {
		enc.AddString("request_id", rf.RequestID)
	}
	if rf.DataID != "" {
		enc.AddString("data_id", rf.DataID)
		// _data_id: truncated version (first 16 characters)
		if len(rf.DataID) > 16 {
			enc.AddString("_data_id", rf.DataID[:16])
		} else {
			enc.AddString("_data_id", rf.DataID)
		}
	}
	if rf.GenID != "" {
		enc.AddString("gen_id", string(rf.GenID))
	}
	if rf.TurnID != "" {
		enc.AddString("turn_id", string(rf.TurnID))
	}
	if rf.StepID != "" {
		enc.AddString("step_id", rf.StepID)
	}
	if rf.QueryID != "" {
		enc.AddString("query_id", rf.QueryID)
	}

	// === Extended model parameters ===
	if rf.EnableLogprob != nil {
		enc.AddBool("enable_logprob", *rf.EnableLogprob)
	}
	if rf.NumResponses != nil {
		enc.AddInt64("num_responses", *rf.NumResponses)
	}
	if rf.ReasoningMaxTokens != nil {
		enc.AddInt64("reasoning_max_tokens", *rf.ReasoningMaxTokens)
	}
	if rf.EnableThinking != nil {
		enc.AddBool("enable_thinking", *rf.EnableThinking)
	}

	// === Framework identification ===
	if rf.RLCVersion != "" {
		enc.AddString("rlc_version", rf.RLCVersion)
	}

	// === Timeout control ===
	if rf.Timeout != nil {
		enc.AddInt64("timeout", *rf.Timeout)
	}

	enc.AddInt("body_size", m.r.RequestBodySize)
	enc.AddString("body_digest", m.r.RequestBodyDigest)
	if m.r.IngressHeaders != nil {
		_ = enc.AddReflected("headers", m.r.IngressHeaders)
	}
	return nil
}

type responseMarshaler struct{ r *AuditRecord }

func (m responseMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	resp := &m.r.ResponseFields
	enc.AddString("id", resp.ID)
	enc.AddString("model", resp.Model)
	enc.AddString("finish_reason", resp.FinishReason)
	enc.AddInt64("prompt_tokens", m.r.PromptTokens)
	enc.AddInt64("completion_tokens", m.r.CompletionTokens)
	enc.AddInt64("total_tokens", resp.TotalTokens)
	enc.AddInt("backend_status", m.r.BackendStatus)
	enc.AddInt64("body_size", m.r.ResponseBodySize)
	enc.AddString("body_digest", m.r.ResponseBodyDigest)
	return nil
}

type streamMarshaler struct{ r *AuditRecord }

func (m streamMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("result", m.r.StreamResult)
	enc.AddInt64("bytes", m.r.StreamBytes)
	enc.AddInt64("chunks", m.r.StreamChunks)
	return nil
}

type routingMarshaler struct{ r *AuditRecord }

func (m routingMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("instance", m.r.InstanceID)
	enc.AddString("endpoint", m.r.Endpoint)
	enc.AddString("allocation_id", m.r.AllocationID)
	enc.AddInt("attempt", m.r.Attempt)
	if m.r.PrefillInstanceID != "" {
		enc.AddString("prefill_instance", m.r.PrefillInstanceID)
	}
	if len(m.r.RetryDetails) > 0 {
		_ = enc.AddReflected("retry_details", m.r.RetryDetails)
	}
	return nil
}

type disconnectMarshaler struct{ r *AuditRecord }

func (m disconnectMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddString("source", m.r.DisconnectSource)
	enc.AddString("phase", m.r.DisconnectPhase)
	if !m.r.DisconnectAt.IsZero() {
		enc.AddTime("at", m.r.DisconnectAt)
	}
	if m.r.ErrorMessage != "" {
		enc.AddString("error", m.r.ErrorMessage)
	}
	return nil
}

type timelineMarshaler struct {
	r    *AuditRecord
	base time.Time
}

func (m timelineMarshaler) MarshalLogObject(enc zapcore.ObjectEncoder) error {
	enc.AddTime("received_at", m.r.ReceivedAt)
	enc.AddInt64("queued_ms", msOffset(m.base, m.r.QueuedAt))
	enc.AddInt64("dequeued_ms", msOffset(m.base, m.r.DequeuedAt))
	enc.AddInt64("allocated_ms", msOffset(m.base, m.r.AllocatedAt))
	enc.AddInt64("forwarded_ms", msOffset(m.base, m.r.ForwardedAt))
	enc.AddInt64("responded_ms", msOffset(m.base, m.r.RespondedAt))
	enc.AddInt64("released_ms", msOffset(m.base, m.r.ReleasedAt))
	enc.AddInt64("total_ms", msOffset(m.base, m.r.ReleasedAt))
	return nil
}

// msOffset returns the millisecond offset of t relative to base.
// Returns 0 if t is zero (timeline event didn't occur).
func msOffset(base, t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.Sub(base).Milliseconds()
}
