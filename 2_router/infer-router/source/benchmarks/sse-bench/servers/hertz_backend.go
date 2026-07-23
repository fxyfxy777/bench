package servers

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
)

// HertzBackendServer wraps a hertz server with clean shutdown, matching BackendServer.
type HertzBackendServer struct {
	h      *server.Hertz
	cancel context.CancelFunc
}

// Stop gracefully shuts down the hertz backend.
func (b *HertzBackendServer) Stop() {
	if b.cancel != nil {
		b.cancel()
	}
	if b.h != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		b.h.Shutdown(ctx) //nolint:errcheck
	}
}

// StartHertzBackend starts a hertz-based simulated GPU inference backend.
// hertz does NOT support HTTP/2, so this always serves HTTP/1.1.
func StartHertzBackend(addr string, tokensPerReq int, tokenDelay time.Duration) *HertzBackendServer {
	// Pre-generate token strings.
	tokens := make([]string, tokensPerReq)
	for i := 0; i < tokensPerReq; i++ {
		tokens[i] = fmt.Sprintf("data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
	}
	done := "data: [DONE]\n\n"

	ctx, cancel := context.WithCancel(context.Background())

	h := server.Default(
		server.WithHostPorts(addr),
		server.WithExitWaitTime(500*time.Millisecond),
	)

	h.GET("/sse", func(_ context.Context, c *app.RequestContext) {
		c.SetContentType("text/event-stream")
		c.Response.Header.Set("Cache-Control", "no-cache")
		c.Response.Header.Set("Connection", "keep-alive")

		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			for _, t := range tokens {
				pw.Write([]byte(t))
				time.Sleep(tokenDelay)
			}
			pw.Write([]byte(done))
		}()

		c.SetBodyStream(pr, -1)
		c.SetStatusCode(consts.StatusOK)
	})

	go func() {
		go h.Spin()
		<-ctx.Done()
	}()

	return &HertzBackendServer{h: h, cancel: cancel}
}
