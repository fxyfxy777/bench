package forensics

import (
	"net/http"

	"github.com/yzx/rl-router/pkg/jsonutil"
)

// ExtraBodyFields holds fields from the extra_body object in request body.
type ExtraBodyFields struct {
	RLCVersion         string  `json:"rlc_version,omitzero"`
	DataID             string  `json:"data_id,omitzero"`
	GenID              FlexString `json:"gen_id,omitzero"`
	TurnID             FlexString `json:"turn_id,omitzero"`
	InferenceID        string  `json:"inference_id,omitzero"`
	SessionID          string  `json:"session_id,omitzero"`
	RequestID          string  `json:"request_id,omitzero"`
	QueryID            string  `json:"query_id,omitzero"`
	EnableLogprob      *bool   `json:"enable_logprob,omitzero"`
	EnableThinking     *bool   `json:"enable_thinking,omitzero"`
	ReasoningMaxTokens *int64  `json:"reasoning_max_tokens,omitzero"`
}

// ExtractExtraBodyFields extracts fields from the extra_body object in request body.
// Returns zero value on malformed JSON or missing extra_body.
func ExtractExtraBodyFields(body []byte) ExtraBodyFields {
	if len(body) == 0 {
		return ExtraBodyFields{}
	}

	// Parse as map first to check for extra_body key
	var raw map[string]any
	if err := jsonutil.Unmarshal(body, &raw); err != nil {
		return ExtraBodyFields{}
	}

	extraBodyRaw, ok := raw["extra_body"]
	if !ok {
		return ExtraBodyFields{}
	}

	// Convert extra_body to JSON bytes for typed unmarshal
	extraBodyBytes, err := jsonutil.Marshal(extraBodyRaw)
	if err != nil {
		return ExtraBodyFields{}
	}

	var fields ExtraBodyFields
	if err := jsonutil.Unmarshal(extraBodyBytes, &fields); err != nil {
		return ExtraBodyFields{}
	}
	return fields
}

// HeaderFields holds tracking fields from HTTP headers.
type HeaderFields struct {
	DataID string
	GenID  FlexString
	TurnID FlexString
}

// ExtractTrackingHeaders extracts tracking fields from HTTP headers.
func ExtractTrackingHeaders(h http.Header) HeaderFields {
	return HeaderFields{
		DataID: h.Get("RL-DATA-ID"),
		GenID:  FlexString(h.Get("RL-GEN-ID")),
		TurnID: FlexString(h.Get("RL-TURN-ID")),
	}
}