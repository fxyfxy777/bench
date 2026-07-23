package servers

import (
	"fmt"
	"net/http"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func sseHandler(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	for i := 0; i < 50; i++ {
		fmt.Fprintf(w, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
		flusher.Flush()
		time.Sleep(5 * time.Millisecond)
	}
	fmt.Fprintf(w, "data: [DONE]\n\n")
	flusher.Flush()
}

// StartNetHTTP starts a net/http SSE server (HTTP/1.1) on the given addr.
func StartNetHTTP(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", sseHandler)
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// StartH2C starts a net/http SSE server with h2c (HTTP/2 cleartext) on the given addr.
func StartH2C(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", sseHandler)

	h2s := &http2.Server{}
	srv := &http.Server{
		Addr:    addr,
		Handler: h2c.NewHandler(mux, h2s),
	}
	go srv.ListenAndServe()
	return srv
}
