package gateway

import "github.com/yzx/rl-router/pkg/forensics"

// streamCapture accumulates SSE stream metadata in a position-independent way.
// Unlike the previous rolling-window design (prevDataLine/lastDataLine), this
// version scans every SSE data payload for finish_reason and usage fields,
// keeping the last non-empty value seen. This handles all backend formats:
// finish_reason before usage, after usage, in the same chunk, or no usage chunk at all.
//
// Also tracks raw bytes and SSE payload counts for audit logging.
//
// Shared between streamTracker (reverse proxy path) and streamForward (V2 ACK path).
type streamCapture struct {
	lastID           string
	lastModel        string
	lastFinishReason string
	promptTokens     int64
	completionTokens int64
	totalTokens      int64
	bytesWritten     int64 // total raw bytes fed (before SSE parsing)
	chunksWritten    int64 // total SSE data payloads seen (excluding [DONE])
}

// Feed scans a chunk for SSE "data: " payloads and accumulates metadata.
// For each non-[DONE] payload, extracts id, model, finish_reason, and usage fields.
// The last non-empty value seen for each field wins.
func (sc *streamCapture) Feed(chunk []byte) {
	sc.bytesWritten += int64(len(chunk))
	payloads := forensics.ExtractDataPayloads(chunk)
	sc.chunksWritten += int64(len(payloads))
	for _, p := range payloads {
		// Extract id, model, and usage.
		u := forensics.ExtractStreamFinalUsage(p)
		if u.ID != "" {
			sc.lastID = u.ID
		}
		if u.Model != "" {
			sc.lastModel = u.Model
		}
		if u.PromptTokens != 0 || u.CompletionTokens != 0 || u.TotalTokens != 0 {
			sc.promptTokens = u.PromptTokens
			sc.completionTokens = u.CompletionTokens
			sc.totalTokens = u.TotalTokens
		}
		// Extract finish_reason.
		if fr := forensics.ExtractStreamFinishReason(p); fr != "" {
			sc.lastFinishReason = fr
		}
	}
}

// Usage returns accumulated token usage and stream identity (id, model).
func (sc *streamCapture) Usage() forensics.ResponseKeyFields {
	return forensics.ResponseKeyFields{
		ID:               sc.lastID,
		Model:            sc.lastModel,
		PromptTokens:     sc.promptTokens,
		CompletionTokens: sc.completionTokens,
		TotalTokens:      sc.totalTokens,
	}
}

// FinishReason returns the last non-empty finish_reason seen across all payloads.
func (sc *streamCapture) FinishReason() string {
	return sc.lastFinishReason
}
