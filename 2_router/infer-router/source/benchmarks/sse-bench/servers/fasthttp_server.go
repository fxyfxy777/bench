package servers

import (
	"bufio"
	"fmt"
	"time"

	"github.com/valyala/fasthttp"
)

// StartFastHTTP starts a fasthttp SSE server on the given addr.
func StartFastHTTP(addr string) *fasthttp.Server {
	handler := func(ctx *fasthttp.RequestCtx) {
		if string(ctx.Path()) != "/sse" {
			ctx.SetStatusCode(404)
			return
		}
		ctx.SetContentType("text/event-stream")
		ctx.Response.Header.Set("Cache-Control", "no-cache")
		ctx.Response.Header.Set("Connection", "keep-alive")

		ctx.SetBodyStreamWriter(func(w *bufio.Writer) {
			for i := 0; i < 50; i++ {
				fmt.Fprintf(w, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
				w.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
		})
	}

	srv := &fasthttp.Server{Handler: handler}
	go srv.ListenAndServe(addr)
	return srv
}
