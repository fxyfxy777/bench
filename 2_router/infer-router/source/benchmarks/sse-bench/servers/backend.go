package servers

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// BackendServer wraps an http.Server with clean shutdown.
type BackendServer struct {
	srv *http.Server
}

// Stop gracefully shuts down the backend.
func (b *BackendServer) Stop() {
	if b.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		b.srv.Shutdown(ctx)
	}
}

// StartBackend starts a simulated GPU inference backend.
// tokensPerReq: number of SSE tokens per request.
// tokenDelay: delay between each token.
// useH2C: if true, serves HTTP/2 cleartext.
func StartBackend(addr string, useH2C bool, tokensPerReq int, tokenDelay time.Duration) *BackendServer {
	handler := makeSSEHandler(tokensPerReq, tokenDelay)
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", handler)

	var h http.Handler = mux
	if useH2C {
		h = h2c.NewHandler(mux, &http2.Server{})
	}

	srv := &http.Server{Addr: addr, Handler: h}
	go srv.ListenAndServe()
	return &BackendServer{srv: srv}
}

// makeSSEHandler creates an SSE handler with configurable token count and delay.
func makeSSEHandler(tokensPerReq int, tokenDelay time.Duration) http.HandlerFunc {
	// Pre-generate token strings to avoid fmt.Fprintf alloc per request.
	tokens := make([]string, tokensPerReq)
	for i := 0; i < tokensPerReq; i++ {
		tokens[i] = fmt.Sprintf("data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
	}
	done := "data: [DONE]\n\n"

	return func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		for _, t := range tokens {
			w.Write([]byte(t))
			flusher.Flush()
			time.Sleep(tokenDelay)
		}
		w.Write([]byte(done))
		flusher.Flush()
	}
}
