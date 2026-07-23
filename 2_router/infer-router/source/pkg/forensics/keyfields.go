package forensics

import (
	"bytes"

	"github.com/yzx/rl-router/pkg/jsonutil"
)

// FlexString deserializes from JSON string, number, or bool into a Go string.
// Solves upstream type mismatch: some callers (e.g. PaddleRL) send gen_id/turn_id
// as JSON numbers, which causes strict Unmarshal to fail the entire struct.
type FlexString string

// UnmarshalJSON accepts JSON string ("1"), number (1), or bool (true) values.
func (s *FlexString) UnmarshalJSON(b []byte) error {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	if b[0] == '"' {
		// Standard JSON string — delegate to jsonutil for proper unescape.
		var raw string
		if err := jsonutil.Unmarshal(b, &raw); err != nil {
			return err
		}
		*s = FlexString(raw)
		return nil
	}
	// Number or bool — keep raw text representation (e.g. 42 → "42").
	*s = FlexString(b)
	return nil
}

// String implements fmt.Stringer.
func (s FlexString) String() string { return string(s) }

// RequestKeyFields holds inference-related parameters extracted from the request body.
// Pointer fields distinguish "not sent" from "sent as zero" — critical for self-proof
// scenarios like "user sent max_tokens=100 but backend returned 500 tokens".
//
// Uses go-json partial unmarshal — messages/prompt large fields are skipped (~5-10μs for 50KB body).
type RequestKeyFields struct {
	// === Standard OpenAI fields ===
	Model               string   `json:"model,omitzero"`
	MaxTokens           *int64   `json:"max_tokens,omitzero"`
	MaxCompletionTokens *int64   `json:"max_completion_tokens,omitzero"`
	Temperature         *float64 `json:"temperature,omitzero"`
	TopP                *float64 `json:"top_p,omitzero"`
	TopK                *int64   `json:"top_k,omitzero"`
	RepetitionPenalty   *float64 `json:"repetition_penalty,omitzero"`
	FrequencyPenalty    *float64 `json:"frequency_penalty,omitzero"`
	PresencePenalty     *float64 `json:"presence_penalty,omitzero"`
	N                   *int64   `json:"n,omitzero"`
	Stream              bool     `json:"stream,omitzero"`
	Stop                any      `json:"stop,omitzero"`

	// === Tracing and correlation fields (for audit and debugging) ===
	InferenceID string `json:"inference_id,omitzero"` // Unique inference ID, format: data_id:gen_id:turn
	RolloutID   string `json:"rollout_id,omitzero"`   // Rollout task ID, format: data_id:gen_id
	SessionID   string `json:"session_id,omitzero"`   // Session ID, format: step:data_id:gen_id
	RequestID   string `json:"request_id,omitzero"`   // Request unique ID (UUID or chatcmpl-xxx)
	DataID      string `json:"data_id,omitzero"`      // Data ID for query-response correlation
	GenID       FlexString `json:"gen_id,omitzero"`       // Generation ID (which generation of the same data)
	TurnID      FlexString `json:"turn_id,omitzero"`      // Turn ID in multi-turn conversations
	StepID      string `json:"step_id,omitzero"`      // Training step ID
	QueryID     string `json:"query_id,omitzero"`     // Query ID for correlation

	// === Extended model parameters ===
	EnableLogprob      *bool  `json:"enable_logprob,omitzero"`      // Enable log probabilities
	NumResponses       *int64 `json:"num_responses,omitzero"`       // Number of responses (same as n)
	ReasoningMaxTokens *int64 `json:"reasoning_max_tokens,omitzero"` // Max tokens for reasoning mode
	EnableThinking     *bool  `json:"enable_thinking,omitzero"`     // Enable thinking mode

	// === Framework identification ===
	RLCVersion string `json:"rlc_version,omitzero"` // RLC framework version for compatibility audit

	// === Timeout control ===
	Timeout *int64 `json:"timeout,omitzero"` // Request timeout in seconds
}

// ExtractRequestKeyFields extracts inference parameters from the request body.
// Returns zero value on malformed JSON — never blocks the request path.
func ExtractRequestKeyFields(body []byte) RequestKeyFields {
	if len(body) == 0 {
		return RequestKeyFields{}
	}
	var fields RequestKeyFields
	if err := jsonutil.Unmarshal(body, &fields); err != nil {
		return RequestKeyFields{}
	}
	return fields
}

// MergeExtraBodyFields merges extra_body fields into RequestKeyFields.
// extra_body has second priority over top-level body fields.
func MergeExtraBodyFields(fields *RequestKeyFields, extra ExtraBodyFields) {
	if extra.RLCVersion != "" {
		fields.RLCVersion = extra.RLCVersion
	}
	if extra.EnableLogprob != nil {
		fields.EnableLogprob = extra.EnableLogprob
	}
	if extra.EnableThinking != nil {
		fields.EnableThinking = extra.EnableThinking
	}
	if extra.ReasoningMaxTokens != nil {
		fields.ReasoningMaxTokens = extra.ReasoningMaxTokens
	}
	// extra_body has priority for ID fields over top-level
	if extra.DataID != "" {
		fields.DataID = extra.DataID
	}
	if extra.GenID != "" {
		fields.GenID = extra.GenID
	}
	if extra.TurnID != "" {
		fields.TurnID = extra.TurnID
	}
	if extra.InferenceID != "" {
		fields.InferenceID = extra.InferenceID
	}
	if extra.SessionID != "" {
		fields.SessionID = extra.SessionID
	}
	if extra.RequestID != "" {
		fields.RequestID = extra.RequestID
	}
	if extra.QueryID != "" {
		fields.QueryID = extra.QueryID
	}
}

// MergeHeaderFields merges header fields into RequestKeyFields (highest priority).
// Headers have highest priority for data_id, gen_id, turn_id.
func MergeHeaderFields(fields *RequestKeyFields, hf HeaderFields) {
	if hf.DataID != "" {
		fields.DataID = hf.DataID
	}
	if hf.GenID != "" {
		fields.GenID = hf.GenID
	}
	if hf.TurnID != "" {
		fields.TurnID = hf.TurnID
	}
}

// ResponseKeyFields holds key result fields extracted from backend responses.
// Used for both non-stream (full response) and stream (last SSE data chunk).
type ResponseKeyFields struct {
	ID               string `json:"id,omitzero"`
	Model            string `json:"model,omitzero"`
	FinishReason     string `json:"finish_reason,omitzero"`
	PromptTokens     int64  `json:"prompt_tokens,omitzero"`
	CompletionTokens int64  `json:"completion_tokens,omitzero"`
	TotalTokens      int64  `json:"total_tokens,omitzero"`
}

// openAIResponseEnvelope extracts id, model, choices[0].finish_reason, and usage
// from OpenAI-format responses (/v1/chat/completions, /v1/completions).
type openAIResponseEnvelope struct {
	ID      string              `json:"id"`
	Model   string              `json:"model"`
	Choices []openAIChoiceShort `json:"choices"`
	Usage   *openAIUsageShort   `json:"usage,omitempty"`
}

type openAIChoiceShort struct {
	FinishReason string `json:"finish_reason"`
}

type openAIUsageShort struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// generateResponseEnvelope extracts meta_info from SGLang /generate responses.
type generateResponseEnvelope struct {
	MetaInfo *generateMeta `json:"meta_info,omitempty"`
}

type generateMeta struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// ExtractResponseKeyFields extracts key result fields from a non-stream backend response.
// backendPath selects the parsing strategy:
//   - "/generate": SGLang meta_info format
//   - all others: OpenAI format (id, model, choices[0].finish_reason, usage)
//
// Returns zero value on malformed JSON.
func ExtractResponseKeyFields(body []byte, backendPath string) ResponseKeyFields {
	if len(body) == 0 {
		return ResponseKeyFields{}
	}
	if backendPath == "/generate" {
		return extractGenerateResponseFields(body)
	}
	return extractOpenAIResponseFields(body)
}

func extractOpenAIResponseFields(body []byte) ResponseKeyFields {
	var env openAIResponseEnvelope
	if err := jsonutil.Unmarshal(body, &env); err != nil {
		return ResponseKeyFields{}
	}
	r := ResponseKeyFields{
		ID:    env.ID,
		Model: env.Model,
	}
	if len(env.Choices) > 0 {
		r.FinishReason = env.Choices[0].FinishReason
	}
	if env.Usage != nil {
		r.PromptTokens = env.Usage.PromptTokens
		r.CompletionTokens = env.Usage.CompletionTokens
		r.TotalTokens = env.Usage.TotalTokens
	}
	return r
}

func extractGenerateResponseFields(body []byte) ResponseKeyFields {
	var env generateResponseEnvelope
	if err := jsonutil.Unmarshal(body, &env); err != nil {
		return ResponseKeyFields{}
	}
	if env.MetaInfo == nil {
		return ResponseKeyFields{}
	}
	return ResponseKeyFields{
		PromptTokens:     env.MetaInfo.PromptTokens,
		CompletionTokens: env.MetaInfo.CompletionTokens,
		TotalTokens:      env.MetaInfo.PromptTokens + env.MetaInfo.CompletionTokens,
	}
}

// maxDataLineLen is the maximum SSE data payload size we capture.
// Payloads exceeding this are skipped — usage extraction degrades gracefully.
const maxDataLineLen = 4096

// dataPrefix is the SSE data line prefix.
var dataPrefix = []byte("data: ")

// donePayload is the SSE stream termination sentinel.
var donePayload = []byte("[DONE]")

// ExtractStreamFinalUsage extracts usage from the last SSE data payload
// (the usage chunk before [DONE]).
// Returns zero value if payload is nil, empty, or malformed.
func ExtractStreamFinalUsage(lastDataPayload []byte) ResponseKeyFields {
	if len(lastDataPayload) == 0 {
		return ResponseKeyFields{}
	}
	// The payload is the JSON portion after "data: " prefix (already stripped by Feed).
	var env openAIResponseEnvelope
	if err := jsonutil.Unmarshal(lastDataPayload, &env); err != nil {
		return ResponseKeyFields{}
	}
	r := ResponseKeyFields{
		ID:    env.ID,
		Model: env.Model,
	}
	if env.Usage != nil {
		r.PromptTokens = env.Usage.PromptTokens
		r.CompletionTokens = env.Usage.CompletionTokens
		r.TotalTokens = env.Usage.TotalTokens
	}
	return r
}

// ExtractStreamFinishReason extracts finish_reason from the second-to-last SSE data payload
// (the chunk before the usage chunk).
// Returns empty string if payload is nil, empty, or malformed.
func ExtractStreamFinishReason(prevDataPayload []byte) string {
	if len(prevDataPayload) == 0 {
		return ""
	}
	var env openAIResponseEnvelope
	if err := jsonutil.Unmarshal(prevDataPayload, &env); err != nil {
		return ""
	}
	if len(env.Choices) > 0 {
		return env.Choices[0].FinishReason
	}
	return ""
}

// ExtractDataPayloads scans a chunk for SSE "data: " lines and returns
// the JSON payload portions (without the "data: " prefix).
// Skips [DONE] payloads and payloads exceeding maxDataLineLen.
func ExtractDataPayloads(chunk []byte) [][]byte {
	var payloads [][]byte
	remaining := chunk
	for len(remaining) > 0 {
		idx := bytes.Index(remaining, dataPrefix)
		if idx < 0 {
			break
		}
		start := idx + len(dataPrefix)
		remaining = remaining[start:]

		// Find end of this data line (terminated by \n or \r\n).
		end := bytes.IndexByte(remaining, '\n')
		var line []byte
		if end >= 0 {
			line = remaining[:end]
			remaining = remaining[end+1:]
		} else {
			line = remaining
			remaining = nil
		}

		// Trim trailing \r.
		line = bytes.TrimRight(line, "\r")

		// Skip [DONE] and oversized payloads.
		if bytes.Equal(line, donePayload) {
			continue
		}
		if len(line) == 0 || len(line) > maxDataLineLen {
			continue
		}

		// Clone to avoid holding references to the original chunk buffer.
		payload := bytes.Clone(line)
		payloads = append(payloads, payload)
	}
	return payloads
}
