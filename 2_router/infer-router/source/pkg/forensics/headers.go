package forensics

import (
	"net/http"
	"strings"
)

// CaptureHeaders records all request headers as a flat map.
// Multi-value headers are joined with ", " (RFC 7230 §3.2.6 compatible).
//
// This is an internal GPU cluster system — no sanitization or filtering is applied.
// Full header recording maximizes debugging value: what the client sent, which
// Authorization was used, any custom headers — all visible in the audit log.
func CaptureHeaders(h http.Header) map[string]string {
	if len(h) == 0 {
		return nil
	}
	result := make(map[string]string, len(h))
	for key, values := range h {
		if len(values) == 1 {
			result[key] = values[0]
		} else {
			result[key] = strings.Join(values, ", ")
		}
	}
	return result
}
