package policy

import "strings"

// maxExtractTokenIDs caps the number of token IDs extracted to limit memory overhead.
const maxExtractTokenIDs = 32768

// ExtractPromptFromChatRequest extracts text prompt from an OpenAI ChatCompletions-style
// request body (already unmarshalled to map[string]any). The returned string is used
// for token estimation in the PD selection process.
func ExtractPromptFromChatRequest(rawReq map[string]any) string {
	messagesVal, ok := rawReq["messages"]
	if !ok {
		return ""
	}

	messages, ok := messagesVal.([]any)
	if !ok {
		return ""
	}

	var builder strings.Builder

	appendText := func(s string) {
		s = strings.TrimSpace(s)
		if s == "" {
			return
		}
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(s)
	}

	for _, msg := range messages {
		msgMap, ok := msg.(map[string]any)
		if !ok {
			continue
		}
		content, ok := msgMap["content"]
		if !ok {
			continue
		}

		switch v := content.(type) {
		case string:
			appendText(v)
		case []any:
			for _, item := range v {
				itemMap, ok := item.(map[string]any)
				if !ok {
					continue
				}
				itemType, _ := itemMap["type"].(string)
				if itemType != "text" {
					continue
				}
				if textVal, ok := itemMap["text"].(string); ok {
					appendText(textVal)
				}
			}
		}
	}

	return builder.String()
}

// ExtractTokenIDsFromRequest extracts prompt_token_ids from an OpenAI-compatible
// request body (already unmarshalled to map[string]any). Returns nil if the field
// is absent, empty, or contains non-numeric elements.
func ExtractTokenIDsFromRequest(rawReq map[string]any) []int {
	val, ok := rawReq["prompt_token_ids"]
	if !ok {
		return nil
	}

	arr, ok := val.([]any)
	if !ok || len(arr) == 0 {
		return nil
	}

	n := min(len(arr), maxExtractTokenIDs)
	result := make([]int, 0, n)
	for i, item := range arr {
		if i >= maxExtractTokenIDs {
			break
		}
		switch v := item.(type) {
		case float64:
			result = append(result, int(v))
		default:
			return nil
		}
	}
	return result
}
