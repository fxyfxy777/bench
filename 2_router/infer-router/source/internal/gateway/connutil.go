package gateway

import (
	"bytes"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// parseDPRankFromID extracts the DP rank from a multi-DP instance ID.
// Convention: multi-DP instance IDs end with "_dp{rank}" (e.g., "sglang-0_dp2").
// Returns -1 if the ID does not match the multi-DP naming pattern.
func parseDPRankFromID(instanceID string) int {
	idx := strings.LastIndex(instanceID, "_dp")
	if idx < 0 || idx == len(instanceID)-3 {
		return -1
	}
	rank, err := strconv.Atoi(instanceID[idx+3:])
	if err != nil || rank < 0 {
		return -1
	}
	return rank
}

// replayableBody sets r.Body, r.ContentLength, and r.GetBody from bodyBytes
// so that Go's http.Transport can transparently retry on stale connections.
//
// Background: Go's Transport retries requests on stale/closed pool connections,
// but only if Request.GetBody is set (so the body can be replayed). Without
// GetBody, POST requests are not considered "replayable" and the Transport
// returns the stale-connection error directly.
//
// The common pattern io.NopCloser(bytes.NewReader(bodyBytes)) does NOT cause
// Go to set GetBody automatically, because NopCloser hides the underlying
// *bytes.Reader type from http.NewRequest's type-switch.
func replayableBody(r *http.Request, bodyBytes []byte) {
	r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
	r.ContentLength = int64(len(bodyBytes))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(bodyBytes)), nil
	}
}

// injectDPRank inserts "data_parallel_rank":dpRank into a JSON object body.
// Uses byte-level manipulation to avoid full JSON parse/serialize overhead.
// Returns the original bodyBytes unmodified if dpRank < 0 or body is not a JSON object.
func injectDPRank(bodyBytes []byte, dpRank int) []byte {
	if dpRank < 0 {
		return bodyBytes
	}
	idx := bytes.IndexByte(bodyBytes, '{')
	if idx < 0 {
		return bodyBytes
	}
	// Build the injection fragment: "data_parallel_rank":N
	// We always append a comma since a valid inference body has at least one field.
	var buf []byte
	buf = append(buf, `{"data_parallel_rank":`...)
	buf = strconv.AppendInt(buf, int64(dpRank), 10)
	buf = append(buf, ',')
	// Append everything after the opening '{'.
	buf = append(buf, bodyBytes[idx+1:]...)
	// Prepend anything before '{' (unlikely but defensive).
	if idx > 0 {
		result := make([]byte, 0, idx+len(buf))
		result = append(result, bodyBytes[:idx]...)
		result = append(result, buf...)
		return result
	}
	return buf
}

// isStaleConnError returns true if the error indicates a stale/closed
// connection from the pool, which is safe to retry since no request data
// was delivered to the backend.
//
// These errors occur when the router's connection pool holds a TCP connection
// that the backend has already closed (e.g., due to keepalive timeout).
//
// Error catalog (Go 1.26 net/http source audit):
//
//   - HTTP/1.1: EOF (non-parsing), use of closed network connection,
//     connection reset by peer, broken pipe,
//     server closed idle connection (errServerClosedIdle),
//     HTTP/1.x transport connection broken (mapRoundTripError)
//   - HTTP/2: stream error, server sent GOAWAY (GoAwayError),
//     Transport received Server's graceful shutdown GOAWAY (errClientConnGotGoAway),
//     client conn not usable (errClientConnUnusable),
//     client conn is closed (errClientConnClosed),
//     no cached connection was available (ErrNoCachedConn)
//   - TLS: bad record MAC, connection reset
func isStaleConnError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()

	// HTTP/1.1 common stale connection errors.
	// Use specific patterns to avoid matching parsing errors that happen to contain "EOF".
	// Connection-level EOF patterns:
	//   - "Post \"url\": EOF" (from http.Client.Do unwrapping transport error)
	//   - "read tcp ...: EOF" (from net package on read)
	//   - ": EOF" (when Go's Transport wraps errors)
	// Note: We exclude parsing-context patterns like "while parsing" or "unmarshal".
	if (strings.Contains(msg, "EOF") && !strings.Contains(msg, "parsing") && !strings.Contains(msg, "unmarshal")) ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "server closed idle connection") ||
		strings.Contains(msg, "HTTP/1.x transport connection broken") {
		return true
	}

	// HTTP/2 specific errors (stream reset, connection gone, pool miss).
	if strings.Contains(msg, "stream error") ||
		strings.Contains(msg, "http2: server sent GOAWAY") ||
		strings.Contains(msg, "http2: Transport received Server's graceful shutdown GOAWAY") ||
		strings.Contains(msg, "http2: client conn not usable") ||
		strings.Contains(msg, "http2: client conn is closed") ||
		strings.Contains(msg, "http2: no cached connection was available") {
		return true
	}

	// TLS-specific errors (session expired, bad record).
	if strings.Contains(msg, "tls: bad record MAC") ||
		strings.Contains(msg, "tls: connection reset") {
		return true
	}

	return false
}
