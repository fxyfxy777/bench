package gateway

import (
	"errors"
	"net/http"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/yzx/rl-router/pkg/forensics"
	"github.com/yzx/rl-router/pkg/jsonutil"
)

// openAIError is the OpenAI-compatible error response envelope.
// Used by V2 handler to match PaddleRL (OpenAI SDK) expectations.
type openAIError struct {
	Error openAIErrorDetail `json:"error"`
}

type openAIErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code"`
}

// writeErrorJSON writes an OpenAI-compatible JSON error response.
// Only used by V2 handler; V1 handler continues using http.Error for performance.
func writeErrorJSON(w http.ResponseWriter, status int, msg, errType string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = jsonutil.NewEncoder(w).Encode(openAIError{
		Error: openAIErrorDetail{
			Message: msg,
			Type:    errType,
			Code:    strconv.Itoa(status),
		},
	})
}

// allocErrorResponse maps a classified disconnect source to the appropriate
// HTTP status code, user-facing message, and OpenAI error type.
// When the error carries a gRPC status message with rich context from the
// scheduler, that message is surfaced instead of the static fallback.
func allocErrorResponse(src forensics.DisconnectSource, err error) (int, string, string) {
	msg := extractAllocErrorMessage(err)

	switch src {
	case forensics.DisconnectQueueFull:
		if msg == "" {
			msg = "rate limit exceeded"
		}
		return http.StatusTooManyRequests, msg, "rate_limit_exceeded"
	case forensics.DisconnectTimeout:
		if msg == "" {
			msg = "allocation timeout"
		}
		return http.StatusGatewayTimeout, msg, "timeout"
	case forensics.DisconnectClient:
		if msg == "" {
			msg = "client disconnected"
		}
		return 499, msg, "client_error"
	case forensics.DisconnectBackend:
		if msg == "" {
			msg = "internal error"
		}
		return http.StatusInternalServerError, msg, "internal_error"
	default: // DisconnectAllocFail and others
		if msg == "" {
			msg = "no available backend"
		}
		return http.StatusServiceUnavailable, msg, "service_unavailable"
	}
}

// extractAllocErrorMessage extracts the human-readable message from a gRPC
// status error, unwrapping through any "remote allocate:" prefix added by
// RemoteAllocator. Returns "" if no gRPC status is found.
func extractAllocErrorMessage(err error) string {
	if err == nil {
		return ""
	}
	// Walk the error chain deepest-first to find the innermost gRPC status.
	// This avoids returning a message that includes fmt.Errorf wrapper text
	// (e.g., "remote allocate: rpc error: ...") instead of the clean scheduler message.
	var best string
	for current := err; current != nil; current = errors.Unwrap(current) {
		if s, ok := status.FromError(current); ok && s.Code() != codes.OK {
			best = s.Message()
		}
	}
	return best
}
