package servers

import (
	"bufio"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
)

// StartFiber starts a Fiber SSE server on the given addr.
func StartFiber(addr string) *fiber.App {
	app := fiber.New(fiber.Config{
		DisableStartupMessage: true,
	})

	app.Get("/sse", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/event-stream")
		c.Set("Cache-Control", "no-cache")
		c.Set("Connection", "keep-alive")

		c.Context().SetBodyStreamWriter(func(w *bufio.Writer) {
			for i := 0; i < 50; i++ {
				fmt.Fprintf(w, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
				w.Flush()
				time.Sleep(5 * time.Millisecond)
			}
			fmt.Fprintf(w, "data: [DONE]\n\n")
			w.Flush()
		})
		return nil
	})

	go app.Listen(addr)
	return app
}
