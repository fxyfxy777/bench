package gateway

import "github.com/yzx/rl-router/pkg/jsonutil"

// tokenUsage holds parsed token counts from an inference response.
type tokenUsage struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
}

// usageEnvelope is a minimal struct for extracting only the "usage" field
// from an OpenAI-format response without deserializing choices/content.
type usageEnvelope struct {
	Usage *usagePayload `json:"usage,omitempty"`
}

type usagePayload struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

// generateMetaEnvelope extracts token counts from SGLang /generate responses.
// Response format: {"text":"...","meta_info":{"prompt_tokens":N,"completion_tokens":M,...}}
type generateMetaEnvelope struct {
	MetaInfo *generateMetaInfo `json:"meta_info,omitempty"`
}

type generateMetaInfo struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
}

// extractUsage extracts token counts from a backend response body.
// backendPath selects the parsing strategy:
//   - "/generate": parses SGLang meta_info format
//   - all others: parses OpenAI usage format (/v1/chat/completions, /v1/completions)
//
// Returns zero-value tokenUsage if the body is empty, malformed, or missing the expected field.
func extractUsage(body []byte, backendPath string) tokenUsage {
	if len(body) == 0 {
		return tokenUsage{}
	}

	if backendPath == "/generate" {
		return extractGenerateUsage(body)
	}
	return extractOpenAIUsage(body)
}

// extractOpenAIUsage parses the OpenAI "usage" envelope.
func extractOpenAIUsage(body []byte) tokenUsage {
	var env usageEnvelope
	if err := jsonutil.Unmarshal(body, &env); err != nil || env.Usage == nil {
		return tokenUsage{}
	}
	return tokenUsage{
		PromptTokens:     env.Usage.PromptTokens,
		CompletionTokens: env.Usage.CompletionTokens,
		TotalTokens:      env.Usage.TotalTokens,
	}
}

// extractGenerateUsage parses the SGLang /generate meta_info envelope.
func extractGenerateUsage(body []byte) tokenUsage {
	var env generateMetaEnvelope
	if err := jsonutil.Unmarshal(body, &env); err != nil || env.MetaInfo == nil {
		return tokenUsage{}
	}
	return tokenUsage{
		PromptTokens:     env.MetaInfo.PromptTokens,
		CompletionTokens: env.MetaInfo.CompletionTokens,
		TotalTokens:      env.MetaInfo.PromptTokens + env.MetaInfo.CompletionTokens,
	}
}
