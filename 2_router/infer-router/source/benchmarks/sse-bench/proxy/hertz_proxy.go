package proxy

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/hertz-contrib/http2/factory"
	"golang.org/x/net/http2"
)

// HertzProxy is an SSE reverse proxy using hertz as the serving framework.
type HertzProxy struct {
	cfg        Config
	h          *server.Hertz
	cancel     context.CancelFunc
	h1Client   *http.Client
	h2Client   *http.Client
	rrCounter  uint64
	activeConn int64
}

// NewHertz creates a new hertz-based SSE proxy.
func NewHertz(cfg Config) *HertzProxy {
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

	return &HertzProxy{
		cfg:      cfg,
		h1Client: &http.Client{Transport: h1Transport, Timeout: 60 * time.Second},
		h2Client: &http.Client{Transport: h2Transport, Timeout: 60 * time.Second},
	}
}

func (p *HertzProxy) pickBackend() string {
	idx := atomic.AddUint64(&p.rrCounter, 1)
	return p.cfg.Backends[int(idx)%len(p.cfg.Backends)]
}

func (p *HertzProxy) backendClient() *http.Client {
	if p.cfg.BackendH2C {
		return p.h2Client
	}
	return p.h1Client
}

// handleSSE proxies SSE requests through hertz.
func (p *HertzProxy) handleSSE(_ context.Context, c *app.RequestContext) {
	backend := p.pickBackend()
	url := fmt.Sprintf("http://%s/sse", backend)

	atomic.AddInt64(&p.activeConn, 1)
	defer atomic.AddInt64(&p.activeConn, -1)

	resp, err := p.backendClient().Get(url)
	if err != nil {
		c.String(consts.StatusBadGateway, err.Error())
		return
	}
	// NOTE: resp.Body is closed inside the goroutine, NOT deferred here,
	// because SetBodyStream reads asynchronously after the handler returns.

	c.SetContentType("text/event-stream")
	c.Response.Header.Set("Cache-Control", "no-cache")
	c.Response.Header.Set("Connection", "keep-alive")

	pr, pw := io.Pipe()
	go func() {
		defer resp.Body.Close()
		defer pw.Close()
		buf := make([]byte, 4096)
		for {
			n, err := resp.Body.Read(buf)
			if n > 0 {
				if _, werr := pw.Write(buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	c.SetBodyStream(pr, -1)
	c.SetStatusCode(consts.StatusOK)
}

// ActiveConns returns the current number of active backend connections.
func (p *HertzProxy) ActiveConns() int64 {
	return atomic.LoadInt64(&p.activeConn)
}

// Start starts the hertz proxy server.
func (p *HertzProxy) Start() {
	opts := []config.Option{
		server.WithHostPorts(p.cfg.ListenAddr),
		server.WithExitWaitTime(500 * time.Millisecond),
	}
	if p.cfg.EnableH2C {
		opts = append(opts, server.WithH2C(true))
	}

	p.h = server.Default(opts...)

	if p.cfg.EnableH2C {
		p.h.AddProtocol("h2", factory.NewServerFactory())
	}

	p.h.GET("/sse", p.handleSSE)

	ctx, cancel := context.WithCancel(context.Background())
	p.cancel = cancel

	go func() {
		go p.h.Spin()
		<-ctx.Done()
	}()
}

// Stop gracefully shuts down the hertz proxy.
func (p *HertzProxy) Stop() {
	if p.cancel != nil {
		p.cancel()
	}
	if p.h != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		p.h.Shutdown(ctx) //nolint:errcheck
	}
}
