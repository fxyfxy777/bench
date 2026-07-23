package gateway

import (
	"strconv"
	"time"
)

// SSE chunk framing constants used by ACK / abort builders.
// These match the rollout-controller wire format and must not be reordered
// without updating downstream parsers.
const (
	ackSSEPrefix = "data: "
	ackSSESuffix = "\n\n"
)

// buildACKChunk builds the SSE ACK chunk matching rollout-controller format.
// Shared by v2_chat (single-instance) and v2_splitwise_chat (PD) handlers.
func buildACKChunk(model string) []byte {
	ts := time.Now().Unix()
	// Pre-compute to avoid map/struct allocation for a fixed-format string.
	var buf []byte
	buf = append(buf, ackSSEPrefix...)
	buf = append(buf, `{"id":"ack","object":"chat.completion.chunk","created":`...)
	buf = strconv.AppendInt(buf, ts, 10)
	buf = append(buf, `,"choices":[],"model":"`...)
	buf = append(buf, model...)
	buf = append(buf, `","is_ack_response":true}`...)
	buf = append(buf, ackSSESuffix...)
	return buf
}

// buildAbortChunk builds an SSE chunk with finish_reason=abort for stream responses.
// Includes router_generated=true so upstream can distinguish router-originated aborts
// from backend-originated ones.
func buildAbortChunk(model string) []byte {
	ts := time.Now().Unix()
	var buf []byte
	buf = append(buf, ackSSEPrefix...)
	buf = append(buf, `{"id":"abort","object":"chat.completion.chunk","created":`...)
	buf = strconv.AppendInt(buf, ts, 10)
	buf = append(buf, `,"choices":[{"index":0,"delta":{},"finish_reason":"abort"}],"model":"`...)
	buf = append(buf, model...)
	buf = append(buf, `","router_generated":true}`...)
	buf = append(buf, ackSSESuffix...)
	return buf
}

// buildAbortNonStreamResponse builds a standard OpenAI chat.completion JSON
// response with finish_reason=abort for non-stream requests.
// Includes router_generated=true so upstream can distinguish router-originated aborts
// from backend-originated ones.
func buildAbortNonStreamResponse(model string) []byte {
	ts := time.Now().Unix()
	var buf []byte
	buf = append(buf, `{"id":"abort","object":"chat.completion","created":`...)
	buf = strconv.AppendInt(buf, ts, 10)
	buf = append(buf, `,"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"abort"}],"model":"`...)
	buf = append(buf, model...)
	buf = append(buf, `","router_generated":true,"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`...)
	return buf
}
