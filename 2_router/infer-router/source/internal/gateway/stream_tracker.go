package gateway

import (
	"bytes"
	"net/http"
	"time"
)

// doneMarker is the SSE sentinel indicating normal stream completion.
var doneMarker = []byte("data: [DONE]")

// streamResult constants for metrics reporting.
const (
	streamResultDone        = "done"
	streamResultInterrupted = "interrupted"
	streamResultError       = "error"
)

// streamTracker wraps http.ResponseWriter to track SSE stream state during
// transparent proxy forwarding. It records TTFT (time to first byte),
// detects the [DONE] sentinel, and counts chunks/bytes — all without
// buffering any data.
//
// streamTracker replaces the previous statusCapturer for streaming requests,
// while maintaining the same interface (http.ResponseWriter + http.Flusher + Unwrap).
type streamTracker struct {
	http.ResponseWriter
	status        int
	wroteHeader   bool
	firstByteTime time.Time // set on the first Write call
	hasFirstByte  bool
	streamDone    bool  // true if "data: [DONE]" was detected
	bytesWritten  int64
	chunksWritten int64 // incremented on each Flush call
	capture       streamCapture // captures last 2 SSE data payloads for usage/finish_reason extraction
}

func newStreamTracker(w http.ResponseWriter) *streamTracker {
	return &streamTracker{ResponseWriter: w}
}

func (st *streamTracker) WriteHeader(code int) {
	if !st.wroteHeader {
		st.status = code
		st.wroteHeader = true
	}
	st.ResponseWriter.WriteHeader(code)
}

// Write passes data through to the underlying writer while scanning for [DONE]
// and recording first-byte time. SSE chunks are typically small (tens to hundreds
// of bytes), so the bytes.Contains scan adds negligible overhead (< 1μs).
func (st *streamTracker) Write(p []byte) (int, error) {
	if !st.hasFirstByte && len(p) > 0 {
		st.firstByteTime = time.Now()
		st.hasFirstByte = true
	}

	// Detect [DONE] sentinel. In httputil.ReverseProxy with FlushInterval: -1,
	// each backend read becomes a single Write+Flush, so [DONE] will not be
	// split across Write calls.
	if !st.streamDone && bytes.Contains(p, doneMarker) {
		st.streamDone = true
	}

	// Capture SSE data payloads for usage/finish_reason extraction.
	st.capture.Feed(p)

	n, err := st.ResponseWriter.Write(p)
	st.bytesWritten += int64(n)
	return n, err
}

func (st *streamTracker) Flush() {
	st.chunksWritten++
	if f, ok := st.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (st *streamTracker) Unwrap() http.ResponseWriter {
	return st.ResponseWriter
}

// TTFT returns time-to-first-byte as duration from the given start time.
// Returns 0 if no byte was ever written.
func (st *streamTracker) TTFT(start time.Time) time.Duration {
	if !st.hasFirstByte {
		return 0
	}
	return st.firstByteTime.Sub(start)
}

// Result returns a stream completion result string for metrics:
//   - "done"        — [DONE] sentinel detected, normal completion
//   - "interrupted" — stream ended without [DONE] (client disconnect or backend crash)
//   - "error"       — proxy returned an error before any data was sent
func (st *streamTracker) Result(proxyErr error) string {
	if proxyErr != nil && !st.hasFirstByte {
		return streamResultError
	}
	if st.streamDone {
		return streamResultDone
	}
	return streamResultInterrupted
}
