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

// HertzServer wraps hertz with a clean shutdown.
type HertzServer struct {
	h      *server.Hertz
	cancel context.CancelFunc
}

// Shutdown stops the hertz server.
func (hs *HertzServer) Shutdown() {
	if hs.cancel != nil {
		hs.cancel()
	}
	if hs.h != nil {
		ctx, c := context.WithTimeout(context.Background(), 2*time.Second)
		defer c()
		hs.h.Shutdown(ctx) //nolint:errcheck
	}
}

// StartHertz starts a Hertz SSE server on the given addr.
func StartHertz(addr string) *HertzServer {
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
			for i := 0; i < 50; i++ {
				fmt.Fprintf(pw, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
				time.Sleep(5 * time.Millisecond)
			}
			fmt.Fprintf(pw, "data: [DONE]\n\n")
		}()

		c.SetBodyStream(pr, -1)
		c.SetStatusCode(consts.StatusOK)
	})

	go func() {
		go h.Spin()
		<-ctx.Done()
	}()

	return &HertzServer{h: h, cancel: cancel}
}
