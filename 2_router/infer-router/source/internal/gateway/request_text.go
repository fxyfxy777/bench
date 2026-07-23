package gateway

import (
	"strings"
	"unicode/utf8"

	"github.com/yzx/rl-router/pkg/jsonutil"
)

// maxRequestTextLen caps the extracted request text to limit memory and tree overhead.
const maxRequestTextLen = 4096

// maxExtractTokenIDs caps prompt_token_ids to bound scheduler payload size.
const maxExtractTokenIDs = 32768

// truncateUTF8 truncates s to at most maxBytes while ensuring the result
// is valid UTF-8 — it never splits a multi-byte character.
func truncateUTF8(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	t := s[:maxBytes]
	// Walk backward past any incomplete trailing rune.
	for len(t) > 0 {
		r, _ := utf8.DecodeLastRuneInString(t)
		if r != utf8.RuneError {
			break
		}
		t = t[:len(t)-1]
	}
	return t
}

// extractRequestText extracts concatenated message content from an OpenAI-compatible
// chat completion request body for cache-aware prefix matching.
// Uses a minimal struct to avoid full deserialization — only "messages[].content" is parsed.
// Returns empty string on any parse failure (graceful degradation, not an error).
func extractRequestText(bodyBytes []byte) string {
	var req struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := jsonutil.Unmarshal(bodyBytes, &req); err != nil || len(req.Messages) == 0 {
		return ""
	}

	// Fast path: single message (common for inference).
	if len(req.Messages) == 1 {
		s := req.Messages[0].Content
		if len(s) > maxRequestTextLen {
			return truncateUTF8(s, maxRequestTextLen)
		}
		return s
	}

	// Multiple messages: concatenate with newline separator.
	var b strings.Builder
	b.Grow(maxRequestTextLen)
	for i, m := range req.Messages {
		if i > 0 {
			b.WriteByte('\n')
		}
		remaining := maxRequestTextLen - b.Len()
		if remaining <= 0 {
			break
		}
		if len(m.Content) > remaining {
			b.WriteString(truncateUTF8(m.Content, remaining))
			break
		}
		b.WriteString(m.Content)
	}
	return b.String()
}

// extractRequestTokenIDs extracts prompt_token_ids from an OpenAI-compatible
// request body. Returns nil when the field is absent, empty, or malformed.
func extractRequestTokenIDs(bodyBytes []byte) []int {
	var req struct {
		PromptTokenIDs []int `json:"prompt_token_ids"`
	}
	if err := jsonutil.Unmarshal(bodyBytes, &req); err != nil || len(req.PromptTokenIDs) == 0 {
		return nil
	}
	if len(req.PromptTokenIDs) > maxExtractTokenIDs {
		return req.PromptTokenIDs[:maxExtractTokenIDs]
	}
	return req.PromptTokenIDs
}
