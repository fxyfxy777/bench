package servers

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"time"
)

// ============================================================
// 优化策略 1: Batch Flush（批量刷写）
// 核心思路：不是每个 token flush 一次，而是攒 N 个 token 再 flush
// 减少 syscall 次数：50次 → 5次
// ============================================================
func StartNetHTTPBatchFlush(addr string, batchSize int) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
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
			// 每 batchSize 个 token 才 flush 一次，或者是最后一个 token
			if (i+1)%batchSize == 0 || i == 49 {
				flusher.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprintf(w, "data: [DONE]\n\n")
		flusher.Flush()
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// ============================================================
// 优化策略 2: Hijack + TCP_NODELAY + bufio 直写
// 核心思路：绕过 net/http 的 ResponseWriter，直接操作 TCP 连接
//   - 设置 TCP_NODELAY 禁用 Nagle 算法，小包立即发送
//   - 用 bufio.Writer 手动控制刷写时机
//   - 减少 ResponseWriter 的内部 header 检查开销
// ============================================================
func StartNetHTTPHijack(addr string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, bufrw, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		// 设置 TCP_NODELAY：禁用 Nagle，每次写立即发送
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}

		bw := bufrw.Writer
		// 手写 HTTP 响应头（绕过 ResponseWriter 的 header 处理逻辑）
		bw.WriteString("HTTP/1.1 200 OK\r\n")
		bw.WriteString("Content-Type: text/event-stream\r\n")
		bw.WriteString("Cache-Control: no-cache\r\n")
		bw.WriteString("Connection: keep-alive\r\n")
		bw.WriteString("\r\n")
		bw.Flush()

		for i := 0; i < 50; i++ {
			fmt.Fprintf(bw, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
			bw.Flush()
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprintf(bw, "data: [DONE]\n\n")
		bw.Flush()
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// ============================================================
// 优化策略 3: Hijack + TCP_NODELAY + Batch Flush 组合
// 综合以上所有优化
// ============================================================
func StartNetHTTPHijackBatch(addr string, batchSize int) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
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

		for i := 0; i < 50; i++ {
			fmt.Fprintf(bw, "data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
			if (i+1)%batchSize == 0 || i == 49 {
				bw.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprintf(bw, "data: [DONE]\n\n")
		bw.Flush()
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// ============================================================
// 优化策略 4: 预分配 buffer + WriteString 避免 fmt.Fprintf
// 核心思路：fmt.Fprintf 每次调用都有反射和内存分配开销
//   - 预拼接 token 字符串，用 WriteString 直写
//   - 配合 sync.Pool 复用 buffer
// ============================================================
func StartNetHTTPPrealloc(addr string) *http.Server {
	// 预生成所有 token 字符串
	tokens := make([]string, 50)
	for i := 0; i < 50; i++ {
		tokens[i] = fmt.Sprintf("data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
	}
	done := "data: [DONE]\n\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")

		bw := bufio.NewWriterSize(w, 4096)
		for i := 0; i < 50; i++ {
			bw.WriteString(tokens[i])
			bw.Flush()
			flusher.Flush()
			time.Sleep(5 * time.Millisecond)
		}
		bw.WriteString(done)
		bw.Flush()
		flusher.Flush()
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}

// ============================================================
// 优化策略 5: 终极组合 — Hijack + TCP_NODELAY + Prealloc + BatchFlush
// ============================================================
func StartNetHTTPUltimate(addr string, batchSize int) *http.Server {
	tokens := make([]string, 50)
	for i := 0; i < 50; i++ {
		tokens[i] = fmt.Sprintf("data: {\"token\":\"hello_%d\",\"seq\":%d}\n\n", i, i)
	}
	done := "data: [DONE]\n\n"

	mux := http.NewServeMux()
	mux.HandleFunc("/sse", func(w http.ResponseWriter, r *http.Request) {
		hj, ok := w.(http.Hijacker)
		if !ok {
			http.Error(w, "hijack unsupported", http.StatusInternalServerError)
			return
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer conn.Close()

		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetNoDelay(true)
		}

		bw := bufio.NewWriterSize(conn, 8192)
		bw.WriteString("HTTP/1.1 200 OK\r\n")
		bw.WriteString("Content-Type: text/event-stream\r\n")
		bw.WriteString("Cache-Control: no-cache\r\n")
		bw.WriteString("Connection: keep-alive\r\n")
		bw.WriteString("\r\n")
		bw.Flush()

		for i := 0; i < 50; i++ {
			bw.WriteString(tokens[i])
			if (i+1)%batchSize == 0 || i == 49 {
				bw.Flush()
			}
			time.Sleep(5 * time.Millisecond)
		}
		bw.WriteString(done)
		bw.Flush()
	})
	srv := &http.Server{Addr: addr, Handler: mux}
	go srv.ListenAndServe()
	return srv
}
