package gateway

import "net/http"

// hopHeaders are hop-by-hop headers that must not be forwarded to the backend
// nor copied to the client. RFC 7230 §6.1.
var hopHeaders = map[string]struct{}{
	"Connection":          {},
	"Keep-Alive":          {},
	"Proxy-Authenticate":  {},
	"Proxy-Authorization": {},
	"Te":                  {},
	"Trailers":            {},
	"Transfer-Encoding":   {},
	"Upgrade":             {},
}

// copyRequestHeaders copies request headers, skipping hop-by-hop and Content-Length.
// Used by V2 chat ACK forward and non-stream forward paths.
func copyRequestHeaders(dst, src http.Header) {
	for k, vv := range src {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		if k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}

// copyResponseHeaders copies response headers, skipping hop-by-hop and Content-Length.
func copyResponseHeaders(dst, src http.Header) {
	for k, vv := range src {
		if _, hop := hopHeaders[k]; hop {
			continue
		}
		if k == "Content-Length" {
			continue
		}
		for _, v := range vv {
			dst.Add(k, v)
		}
	}
}
