#!/bin/bash
# demo: 模拟 sglang 服务，配合真实 infer-router 使用
# 只监听 30100，提供 /health、/server_info（供 register_router.sh 读 dp_size）、
# /v1/chat/completions（openai-chat SSE，供 router 转发过来的压测流量）
python3 - <<'PY'
import http.server
import json
import random
import socketserver
import time

FAKE_OUTPUT_WORDS = ["hello", "world", "sglang", "benchmark", "test", "token", "response", "ok"]


class ThreadingHTTPServer(socketserver.ThreadingMixIn, http.server.HTTPServer):
    allow_reuse_address = True
    daemon_threads = True


class Handler(http.server.BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def do_GET(self):
        if self.path == "/health":
            self.send_response(200)
            self.end_headers()
        elif self.path == "/server_info":
            body = json.dumps({"dp_size": 1}).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
        else:
            self.send_response(404)
            self.end_headers()

    def do_POST(self):
        if not self.path.endswith("/v1/chat/completions"):
            self.send_response(404)
            self.end_headers()
            return

        length = int(self.headers.get("Content-Length", 0))
        body = self.rfile.read(length) if length else b"{}"
        try:
            payload = json.loads(body)
        except json.JSONDecodeError:
            payload = {}

        messages = payload.get("messages", [])
        input_len = sum(len(str(m.get("content", ""))) // 4 for m in messages) or 100
        cached_len = int(input_len * random.uniform(0.5, 0.9))
        num_output_tokens = random.randint(20, 80)

        # 默认非流式（跟真实 OpenAI/sglang 语义一致）；benchmark_serving.py
        # 会显式传 stream=True，register_router.sh 的验证请求没传，走非流式分支
        stream = payload.get("stream", False)

        if not stream:
            content = " ".join(random.choice(FAKE_OUTPUT_WORDS) for _ in range(num_output_tokens))
            resp = {
                "id": "fake-1",
                "choices": [{"message": {"content": content}, "finish_reason": "stop"}],
                "usage": {
                    "prompt_tokens": input_len,
                    "completion_tokens": num_output_tokens,
                    "prompt_tokens_details": {"cached_tokens": cached_len},
                },
            }
            body = json.dumps(resp).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return

        # 流式响应：不知道总长度，也不用 chunked 编码，靠 Connection: close
        # 让客户端（router）通过连接关闭来判断响应体结束，否则在 HTTP/1.1
        # keep-alive 下客户端会一直等下一部分数据，导致请求挂起超时
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Connection", "close")
        self.end_headers()
        self.close_connection = True

        time.sleep(random.uniform(0.05, 0.2))  # 模拟 TTFT
        for _ in range(num_output_tokens):
            chunk = {
                "id": "fake-1",
                "choices": [{"delta": {"content": random.choice(FAKE_OUTPUT_WORDS) + " "}}],
            }
            self.wfile.write(f"data: {json.dumps(chunk)}\n\n".encode())
            self.wfile.flush()
            time.sleep(random.uniform(0.01, 0.03))  # 模拟 ITL

        usage_chunk = {
            "id": "fake-1",
            "choices": [],
            "usage": {
                "prompt_tokens": input_len,
                "completion_tokens": num_output_tokens,
                "prompt_tokens_details": {"cached_tokens": cached_len},
            },
        }
        self.wfile.write(f"data: {json.dumps(usage_chunk)}\n\n".encode())
        self.wfile.write(b"data: [DONE]\n\n")
        self.wfile.flush()

    def log_message(self, fmt, *args):
        pass


print("fake server starting...", flush=True)
print("Uvicorn running on http://0.0.0.0:30100", flush=True)
print("The server is fired up and ready to roll", flush=True)
with ThreadingHTTPServer(("0.0.0.0", 30100), Handler) as httpd:
    httpd.serve_forever()
PY
