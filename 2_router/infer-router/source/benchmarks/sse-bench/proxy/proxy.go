package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

// SSEProxy is the common interface for SSE reverse proxies.
type SSEProxy interface {
	Start()
	Stop()
	ActiveConns() int64
}

// Config defines the proxy configuration.
type Config struct {
	// Frontend
	ListenAddr   string
	FrontendMode string // "standard" or "hijack"
	EnableH2C    bool   // serve frontend as h2c

	// Backend
	Backends    []string // backend addrs
	BackendH2C  bool     // connect to backends via h2c
	BackendPool int      // connections per backend
}

// Proxy is an SSE reverse proxy.
type Proxy struct {
	cfg        Config
	srv        *http.Server
	h1Client   *http.Client
	h2Client   *http.Client
	rrCounter  uint64
	activeConn int64 // track active backend connections for monitoring
}

// New creates a new SSE proxy.
func New(cfg Config) *Proxy {
	poolSize := cfg.BackendPool
	if poolSize <= 0 {
		poolSize = 1000
	}

	h1Transport := &http.Transport{
		MaxIdleConns:        poolSize,
		MaxIdleConnsPerHost: poolSize,
		MaxConnsPerHost:     poolSize,
		IdleConnTimeout:     90 * time.Second,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}

	h2Transport := &http2.Transport{
		AllowHTTP: true,
		DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
			return net.DialTimeout(network, addr, 5*time.Second)
		},
	}

	return &Proxy{
		cfg:      cfg,
		h1Client: &http.Client{Transport: h1Transport, Timeout: 60 * time.Second},
		h2Client: &http.Client{Transport: h2Transport, Timeout: 60 * time.Second},
	}
}

func (p *Proxy) pickBackend() string {
	idx := atomic.AddUint64(&p.rrCounter, 1)
	return p.cfg.Backends[int(idx)%len(p.cfg.Backends)]
}

func (p *Proxy) backendClient() *http.Client {
	if p.cfg.BackendH2C {
		return p.h2Client
	}
	return p.h1Client
}

// handleStandard proxies using standard ResponseWriter.
func (p *Proxy) handleStandard(w http.ResponseWriter, r *http.Request) {
	backend := p.pickBackend()
	url := fmt.Sprintf("http://%s/sse", backend)

	atomic.AddInt64(&p.activeConn, 1)
	defer atomic.AddInt64(&p.activeConn, -1)

	resp, err := p.backendClient().Get(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			w.Write(buf[:n])
			flusher.Flush()
		}
		if err != nil {
			break
		}
	}
}

// handleHijack proxies using Hijacked connection for lower overhead.
func (p *Proxy) handleHijack(w http.ResponseWriter, r *http.Request) {
	backend := p.pickBackend()
	url := fmt.Sprintf("http://%s/sse", backend)

	atomic.AddInt64(&p.activeConn, 1)
	defer atomic.AddInt64(&p.activeConn, -1)

	resp, err := p.backendClient().Get(url)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	hj, ok := w.(http.Hijacker)
	if !ok {
		// fallback to standard mode
		p.handleStandard(w, r)
		return
	}

	conn, bufrw, err := hj.Hijack()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer conn.Close()

	if tc, ok := conn.(*net.TCPConn); ok {
		tc.SetNoDelay(true)
	}

	bw := bufrw.Writer
	bw.WriteString("HTTP/1.1 200 OK\r\n")
	bw.WriteString("Content-Type: text/event-stream\r\n")
	bw.WriteString("Cache-Control: no-cache\r\n")
	bw.WriteString("Connection: keep-alive\r\n")
	bw.WriteString("\r\n")
	bw.Flush()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			bw.Write(buf[:n])
			bw.Flush()
		}
		if err != nil {
			break
		}
	}
}

// ActiveConns returns the current number of active backend connections.
func (p *Proxy) ActiveConns() int64 {
	return atomic.LoadInt64(&p.activeConn)
}

// Start starts the proxy server.
func (p *Proxy) Start() {
	mux := http.NewServeMux()
	handler := p.handleStandard
	if p.cfg.FrontendMode == "hijack" {
		handler = p.handleHijack
	}
	mux.HandleFunc("/sse", handler)

	var finalHandler http.Handler = mux
	if p.cfg.EnableH2C {
		h2s := &http2.Server{}
		finalHandler = h2c.NewHandler(mux, h2s)
	}

	p.srv = &http.Server{
		Addr:    p.cfg.ListenAddr,
		Handler: finalHandler,
	}
	go p.srv.ListenAndServe()
}

// Stop gracefully shuts down the proxy.
func (p *Proxy) Stop() {
	if p.srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		p.srv.Shutdown(ctx)
	}
}
