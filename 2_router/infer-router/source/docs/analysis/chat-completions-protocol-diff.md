# Chat Completions 接口协议对比分析

> 目标：OneRouter Gateway 需要兼容 OpenAI、SGLang、vLLM、FastDeploy 四种后端的 `/v1/chat/completions` 接口。
> 本文档详尽列出各协议的入参、返回结构、流式协议、错误处理的差异，供 Gateway 转发层设计参考。

---

## 1. 端点与通用协议

| 维度 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| 路径 | `POST /v1/chat/completions` | `POST /v1/chat/completions` | `POST /v1/chat/completions` | `POST /v1/chat/completions` |
| Content-Type | `application/json` | `application/json`（validate_json_request） | `application/json`（validate_json_request） | `application/json` |
| 认证 | `Authorization: Bearer <key>` | 无内置认证 | 无内置认证 | `Authorization: Bearer <token>`（AuthenticationMiddleware，401） |
| 额外请求头 | `OpenAI-Organization`, `OpenAI-Project` | `X-Request-Id` | `X-Request-Id`, `X-data-parallel-rank`, `endpoint-load-metrics-format` | trace context headers |
| 额外字段策略 | 忽略未知字段 | Pydantic BaseModel（严格，未声明字段报错） | `extra="allow"`（接受但日志警告） | Pydantic BaseModel（严格） |

### 1.1 传输协议支持

> 以下对比基于各框架本地源码分析（sglang、vllm、FastDeploy），以及 OpenAI 官方文档。

| 维度 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| **HTTP/1.1** | ✅ 主要协议 | ✅ 默认模式（FastAPI + Uvicorn + uvloop） | ✅ 默认模式（FastAPI + Uvicorn + h11） | ✅ 唯一协议（FastAPI + Gunicorn/Uvicorn；Go Router 用 Gin） |
| **HTTP/2 (h2)** | ✅（通过 CDN/ALB 自动协商） | ❌ Uvicorn 不启用 h2 | ❌ Uvicorn 明确使用 h11 | ❌ 不支持 |
| **gRPC (HTTP/2 + Protobuf)** | ❌ 不提供 | ✅ `--grpc-mode` 启动独立 gRPC 服务（grpc.aio），**与 HTTP 互斥，同端口二选一** | ✅ 独立 gRPC 服务（grpc.aio），默认端口 50051，提供 `VllmEngine.Generate` 等 RPC | ❌ 不提供 |
| **HTTPS (TLS)** | ✅ 强制 TLS | ✅ Uvicorn 支持 SSL 参数 | ✅ `--ssl-keyfile/--ssl-certfile` | ✅ Uvicorn SSL 参数 |
| **WebSocket** | ✅ Realtime API | ❌ | ✅ `/v1/realtime`（音频转录） | ❌ |
| **SSE 流式** | ✅ `text/event-stream` | ✅ `text/event-stream` | ✅ `text/event-stream` | ✅ `text/event-stream` |
| **Unix Domain Socket** | ❌ | ❌ | ✅ `--uds` 参数 | ❌ |
| **服务框架** | SaaS（不可见） | Python: FastAPI + Uvicorn; Rust Gateway: Axum + Tonic | Python: FastAPI + Uvicorn; 内部 Rust Router 通过 gRPC 对接 | Python: FastAPI + Gunicorn(Uvicorn Worker); Go Router: Gin |
| **默认端口** | 443 (HTTPS) | 30000 | 8000 (HTTP) / 50051 (gRPC) | 8000 |

#### gRPC 模式详情

**SGLang gRPC**：
- 启动方式：`python -m sglang.launch_server --grpc-mode --port 30000`
- 服务定义：由外部包 `smg-grpc-proto` 提供（`sglang_scheduler_pb2`）
- 主要 RPC：`Generate`（server-streaming）、`Embed`（unary）、`HealthCheck`、`Abort`、`GetModelInfo`、`GetServerInfo`、`GetLoads`
- **注意**：gRPC 模式下不进行 tokenization，客户端需发送 pre-tokenized 输入
- **与 HTTP 互斥**：同一进程只能启动 HTTP 或 gRPC 中的一种

**vLLM gRPC**：
- 启动方式：独立 entrypoint `vllm/entrypoints/grpc_server.py`，默认端口 50051
- Proto 定义：`vllm/grpc/vllm_engine.proto`
- 主要 RPC：`Generate`（server-streaming）、`Embed`、`HealthCheck`、`Abort`、`GetModelInfo`、`GetServerInfo`
- **设计定位**：面向内部 Rust Router ↔ Python Engine 的高效二进制通信，**非用户直接调用**
- **与 HTTP 可共存**：HTTP 和 gRPC 是不同端口上的独立服务

#### 对 OneRouter Gateway 的影响

| 场景 | 推荐协议 | 说明 |
|------|---------|------|
| 转发到 OpenAI | HTTP/1.1 (HTTPS) | 标准 REST，`net/http` 直连 |
| 转发到 SGLang (默认) | HTTP/1.1 | FastAPI 默认模式，与当前 `ReverseProxy` 兼容 |
| 转发到 SGLang (gRPC) | gRPC (HTTP/2) | 需引入 `google.golang.org/grpc` 客户端，适用于高吞吐低延迟场景 |
| 转发到 vLLM (默认) | HTTP/1.1 | FastAPI 默认模式，与当前 `ReverseProxy` 兼容 |
| 转发到 vLLM (gRPC) | gRPC (HTTP/2) | 非官方推荐的用户接口，仅 Rust Router 使用 |
| 转发到 FastDeploy | HTTP/1.1 | 唯一可用协议 |

---

## 2. 请求参数对比

### 2.1 OpenAI 标准字段（四者均支持）

| 字段 | 类型 | OpenAI 默认 | SGLang | vLLM | FastDeploy | 差异说明 |
|------|------|------------|--------|------|------------|---------|
| `messages` | `array` | **必填** | **必填** `List[ChatCompletionMessageParam]` | **必填** `list[ChatCompletionMessageParam]` | **必填** `Union[List[Any], List[int]]` | FD 额外支持直接传 token id 数组 |
| `model` | `string` | **必填** | `"default"` | `None` | `"default"` | OpenAI 必填；其余可选，有默认值。SGLang 支持 `"base:adapter"` LoRA 语法 |
| `frequency_penalty` | `float` | `0` | `0.0` | `0.0` | `None` | FD 默认 None（由后端决定） |
| `logit_bias` | `dict` | `null` | `None` | `None` | **不支持** | FD 无此字段 |
| `logprobs` | `bool` | `false` | `False` | `False` | `False` | 一致 |
| `top_logprobs` | `int` | `null` | `None` | `0` | `None` | OpenAI 范围 0-20；vLLM 默认 0 |
| `max_tokens` | `int` | `null` | `None` | `None` | `None` | 四者均标记为 deprecated，推荐 `max_completion_tokens` |
| `max_completion_tokens` | `int` | `null` | `None` | `None` | `None` | 一致 |
| `n` | `int` | `1` | `1` | `1` | `1` | 一致 |
| `presence_penalty` | `float` | `0` | `0.0` | `0.0` | `None` | FD 默认 None |
| `response_format` | `object` | `null` | 支持 `text/json_object/json_schema/structural_tag` | 支持 `text/json_object/json_schema/structural_tag` | 支持 `text/json_object/json_schema/structural_tag` | 三者都额外支持 `structural_tag` 类型 |
| `seed` | `int` | `null` | `None` | `None` | `None`（ge=0） | FD 约束 >= 0 |
| `stop` | `str\|array` | `null`（最多4个） | `None` | `[]` | `[]` | vLLM/FD 默认空数组 |
| `stream` | `bool` | `false` | `False` | `False` | `False` | 一致 |
| `stream_options` | `object` | `null` | 支持 | 支持 | 支持 | 见 StreamOptions 对比 |
| `temperature` | `float` | `1` | `None`→fallback `1.0` | `None`→server default `1.0` | `None` | 三者均 None 时回退到 1.0 |
| `top_p` | `float` | `1` | `None`→fallback `1.0` | `None`→server default `1.0` | `None` | 同上 |
| `tools` | `array` | `null` | `None` | `None` | `None` | 一致 |
| `tool_choice` | `str\|object` | `"auto"` | `"auto"`（有 tools 时）/ `"none"` | `"none"`→`"auto"`（有 tools 时） | **不支持** 此字段 | FD 缺少 tool_choice |
| `parallel_tool_calls` | `bool` | `true` | **不支持** | `True` | **不支持** | 仅 OpenAI 和 vLLM |
| `user` | `string` | `null` | `None` | `None`（**被忽略**） | `None` | vLLM 明确忽略此字段 |

### 2.2 StreamOptions 子结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `include_usage` | 支持 | `False` | `True` | `True` |
| `continuous_usage_stats` | **不支持** | `False` | `False` | `False` |

> **注意**：vLLM 和 FD 的 `include_usage` 默认为 `True`（OpenAI 需显式开启）。`continuous_usage_stats` 是三个推理框架的扩展，OpenAI 不支持。

### 2.3 采样扩展参数（非 OpenAI 标准）

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `top_k` | `None`→`-1` | `None`→`0` | `None` | 默认值不同！SGLang -1=禁用，vLLM 0=禁用 |
| `min_p` | `None`→`0.0` | `None`→`0.0` | `None` | 一致 |
| `repetition_penalty` | `None`→`1.0` | `None`→`1.0` | `None` | 一致 |
| `min_tokens` | `0` | `0` | `None` | FD 默认 None |
| `stop_token_ids` | `None` | `[]` | `[]` | 默认值不同 |
| `include_stop_str_in_output` | **不支持** | `False` | `False` | SGLang 有 `no_stop_trim`（反义） |
| `ignore_eos` | `True/False` | `False` | **不支持** | FD 无此字段 |
| `skip_special_tokens` | `True` | `True` | **不支持** | FD 无此字段 |
| `spaces_between_special_tokens` | **不支持** | `True` | **不支持** | 仅 vLLM |
| `bad_words` | **不支持** | `[]` | `None` | |
| `bad_words_token_ids` | **不支持** | **不支持** | `None` | 仅 FD |
| `length_penalty` | **不支持** | `1.0` | **不支持** | 仅 vLLM（beam search） |
| `use_beam_search` | **不支持** | `False` | **不支持** | 仅 vLLM |
| `truncate_prompt_tokens` | **不支持** | `None` | **不支持** | 仅 vLLM |
| `allowed_token_ids` | **不支持** | `None` | **不支持** | 仅 vLLM |
| `logits_processors_args` | **不支持** | **不支持** | `None` | 仅 FD |

### 2.4 推理 / 思维链扩展参数

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `reasoning_effort` | `"medium"` | `None` | **不支持** | SGLang 默认 medium；vLLM 可选 low/medium/high |
| `include_reasoning` | **不支持** | `True` | **不支持** | 仅 vLLM |
| `separate_reasoning` | `True` | **不支持** | **不支持** | 仅 SGLang |
| `stream_reasoning` | `True` | **不支持** | **不支持** | 仅 SGLang |
| `reasoning_max_tokens` | **不支持** | **不支持** | `None` | 仅 FD |
| `response_max_tokens` | **不支持** | **不支持** | `None` | 仅 FD |

### 2.5 模板 / 提示控制扩展

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `chat_template` | **不支持** | `None` | `None` | 自定义 Jinja 模板 |
| `chat_template_kwargs` | `None` | `None` | `None` | 三者一致 |
| `add_generation_prompt` | **不支持** | `True` | **不支持** | 仅 vLLM |
| `continue_final_message` | `False` | `False` | **不支持** | SGLang 和 vLLM |
| `add_special_tokens` | **不支持** | `False` | **不支持** | 仅 vLLM |
| `echo` | **不支持** | `False` | **不支持** | 仅 vLLM |
| `documents` | **不支持** | `None`（RAG） | **不支持** | 仅 vLLM |
| `disable_chat_template` | **不支持** | **不支持** | `False` | 仅 FD |

### 2.6 结构化输出 / 受限解码

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `response_format` (json_schema) | ✅ | ✅ | ✅ | 三者一致 |
| `response_format` (structural_tag) | ✅ | ✅ | ✅ | 三者一致 |
| `regex` | ✅ | **不支持**（通过 structured_outputs） | **不支持**（通过 guided_regex） | 字段名不同！ |
| `ebnf` | ✅ | **不支持** | **不支持** | 仅 SGLang |
| `structured_outputs` | **不支持** | ✅（json/regex/choice） | **不支持** | 仅 vLLM |
| `guided_json` | **不支持** | **不支持** | ✅ | 仅 FD |
| `guided_regex` | **不支持** | **不支持** | ✅ | 仅 FD |
| `guided_choice` | **不支持** | **不支持** | ✅ | 仅 FD |
| `guided_grammar` | **不支持** | **不支持** | ✅ | 仅 FD |
| `structural_tag`（独立字段） | **不支持** | **不支持** | ✅ | 仅 FD |

### 2.7 调度 / 路由 / 缓存扩展

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `priority` | `None` | `0` | **不支持** | |
| `request_id` / `rid` | `rid` | `request_id` | `request_id` | 字段名不同！ |
| `cache_salt` | ✅ | ✅ | **不支持** | |
| `routed_dp_rank` | ✅ | **不支持** | **不支持** | 仅 SGLang |
| `bootstrap_host/port/room` | ✅（PD disagg） | **不支持** | **不支持** | 仅 SGLang |
| `kv_transfer_params` | **不支持** | ✅ | **不支持** | 仅 vLLM（disagg serving） |
| `disaggregate_info` | **不支持** | **不支持** | ✅ | 仅 FD |
| `vllm_xargs` | **不支持** | ✅ | **不支持** | 仅 vLLM 通用扩展参数 |

### 2.8 多模态扩展

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| content part: `image_url` | ✅ | ✅ | ✅ | 一致 |
| content part: `video_url` | ✅ | ✅ | ✅（视具体模型） | |
| content part: `audio_url` | ✅ | ✅ | ✅ | |
| `mm_processor_kwargs` | **不支持** | ✅ | **不支持** | 仅 vLLM |
| `mm_hashes` | **不支持** | **不支持** | ✅ | 仅 FD |
| `max/min_dynamic_patch` | ✅（请求+content part 级） | **不支持** | **不支持** | 仅 SGLang |

### 2.9 调试 / 返回扩展

| 字段 | SGLang | vLLM | FastDeploy | 说明 |
|------|--------|------|------------|------|
| `return_hidden_states` | ✅ | **不支持** | **不支持** | 仅 SGLang |
| `return_routed_experts` | ✅ | **不支持** | **不支持** | 仅 SGLang |
| `return_cached_tokens_details` | ✅ | **不支持** | **不支持** | 仅 SGLang |
| `return_token_ids` | **不支持** | ✅ | ✅ | vLLM 和 FD |
| `return_tokens_as_token_ids` | **不支持** | ✅ | **不支持** | 仅 vLLM |
| `prompt_logprobs` | **不支持** | ✅ | ✅ | vLLM 和 FD |
| `prompt_token_ids`（输入） | **不支持** | **不支持** | ✅ | 仅 FD |
| `collect_metrics` | **不支持** | **不支持** | ✅ | 仅 FD |

---

## 3. 返回结构对比

### 3.1 非流式响应 (ChatCompletionResponse)

#### 顶层结构

| 字段 | 类型 | OpenAI | SGLang | vLLM | FastDeploy |
|------|------|--------|--------|------|------------|
| `id` | `string` | `chatcmpl-xxx` | `chatcmpl-<hex>` | `chatcmpl-<uuid>` | `chatcmpl-<uuid>` |
| `object` | `string` | `"chat.completion"` | `"chat.completion"` | `"chat.completion"` | `"chat.completion"` |
| `created` | `int` | Unix timestamp | Unix timestamp | Unix timestamp | Unix timestamp |
| `model` | `string` | ✅ | ✅ | ✅ | ✅ |
| `choices` | `array` | ✅ | ✅ | ✅ | ✅ |
| `usage` | `object` | ✅ | ✅ | ✅ | ✅ |
| `system_fingerprint` | `string` | ✅ | **无** | `None` | **无** |
| `service_tier` | `string` | ✅ | **无** | `None` | **无** |
| `metadata` | `dict` | **无** | ✅（如 weight_version） | **无** | **无** |
| `sglext` | `object` | **无** | ✅（routed_experts, cached_tokens_details） | **无** | **无** |
| `prompt_logprobs` | `array` | **无** | **无** | ✅ | **无** |
| `prompt_token_ids` | `array` | **无** | **无** | ✅ | **无** |
| `kv_transfer_params` | `dict` | **无** | **无** | ✅ | **无** |

#### Choice 结构

| 字段 | 类型 | OpenAI | SGLang | vLLM | FastDeploy |
|------|------|--------|--------|------|------------|
| `index` | `int` | ✅ | ✅ | ✅ | ✅ |
| `message` | `object` | ✅ | ✅ | ✅ | ✅ |
| `logprobs` | `object\|null` | ✅ | ✅ | ✅ | ✅ |
| `finish_reason` | `string` | `stop/length/content_filter/tool_calls` | `stop/length/tool_calls/content_filter/function_call/abort` | `stop/length/tool_calls/error` | `stop/length/tool_calls/recover_stop` |
| `matched_stop` | `int\|str\|null` | **无** | ✅ | **无** | **无** |
| `stop_reason` | `int\|str\|null` | **无** | **无** | ✅ | **无** |
| `hidden_states` | `object` | **无** | ✅ | **无** | **无** |
| `token_ids` | `array` | **无** | **无** | ✅ | **无** |
| `draft_logprobs` | `object` | **无** | **无** | **无** | ✅ |
| `prompt_logprobs` | `object` | **无** | **无** | **无** | ✅ |
| `speculate_metrics` | `object` | **无** | **无** | **无** | ✅ |

> **finish_reason 差异汇总**：
> - OpenAI: `stop`, `length`, `content_filter`, `tool_calls`
> - SGLang: 额外增加 `function_call`, `abort`
> - vLLM: 额外增加 `error`（替代 content_filter）
> - FastDeploy: 额外增加 `recover_stop`（错误恢复）

#### Message 结构 (ChatMessage)

| 字段 | 类型 | OpenAI | SGLang | vLLM | FastDeploy |
|------|------|--------|--------|------|------------|
| `role` | `string` | `"assistant"` | ✅ | ✅ | ✅ |
| `content` | `string\|null` | ✅ | ✅ | ✅ | ✅ |
| `refusal` | `string\|null` | ✅ | **无** | ✅ | **无** |
| `tool_calls` | `array\|null` | ✅ | ✅ | ✅ | ✅ |
| `function_call` | `object` | **deprecated** | **无** | ✅ | **无** |
| `reasoning_content` | `string\|null` | **无** | ✅ | **无** | ✅ |
| `reasoning` | `string\|null` | **无** | **无** | ✅ | **无** |
| `annotations` | `object` | **无** | **无** | ✅ | **无** |
| `audio` | `object` | **无** | **无** | ✅ | **无** |
| `multimodal_content` | `array` | **无** | **无** | **无** | ✅ |
| `audio_content` | `string` | **无** | **无** | **无** | ✅ |
| `prompt_token_ids` | `array` | **无** | **无** | **无** | ✅ |
| `completion_token_ids` | `array` | **无** | **无** | **无** | ✅ |
| `prompt_tokens` | `string` | **无** | **无** | **无** | ✅ |
| `completion_tokens` | `string` | **无** | **无** | **无** | ✅ |

> **关键差异**：reasoning 字段命名不同！
> - SGLang & FastDeploy: `reasoning_content`
> - vLLM: `reasoning`

#### ToolCall 结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `id` | `string` | `Optional[str]` | `string` | `string` |
| `type` | `"function"` | `"function"` | `"function"` | `"function"` |
| `index` | **无** | `Optional[int]` | **无** | **无** |
| `function.name` | `string` | `Optional[str]` | `string` | `string` |
| `function.arguments` | `string`（JSON） | `str\|Dict` | `string` | `string` |

> SGLang 的 `function.arguments` 可能返回 Dict 而非 JSON 字符串，这是一个重要差异。

#### Usage 结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `prompt_tokens` | ✅ | ✅ | ✅ | ✅ |
| `completion_tokens` | ✅ | ✅ | ✅ | ✅ |
| `total_tokens` | ✅ | ✅ | ✅ | ✅ |
| `prompt_tokens_details.cached_tokens` | ✅ | ✅ | ✅ | ✅ |
| `prompt_tokens_details.image_tokens` | **无** | **无** | **无** | ✅ |
| `prompt_tokens_details.video_tokens` | **无** | **无** | **无** | ✅ |
| `completion_tokens_details.reasoning_tokens` | ✅ | **无**（顶层 `reasoning_tokens`） | **无** | ✅ |
| `completion_tokens_details.image_tokens` | **无** | **无** | **无** | ✅ |
| `reasoning_tokens`（顶层） | **无** | ✅ | **无** | **无** |

> SGLang 把 `reasoning_tokens` 放在 UsageInfo 顶层，而非嵌套在 `completion_tokens_details` 中。

---

### 3.2 流式响应 (SSE Streaming)

#### 通用格式

四者的 SSE 格式完全一致：
```
data: {"id":"...","object":"chat.completion.chunk",...}\n\n
...
data: [DONE]\n\n
```

#### 流式顶层结构 (ChatCompletionStreamResponse)

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `id` | ✅ | ✅ | ✅ | ✅ |
| `object` | `"chat.completion.chunk"` | `"chat.completion.chunk"` | `"chat.completion.chunk"` | `"chat.completion.chunk"` |
| `created` | ✅ | ✅ | ✅ | ✅ |
| `model` | ✅ | ✅ | ✅ | ✅ |
| `choices` | ✅ | ✅ | ✅ | ✅ |
| `usage` | 最后一个 chunk | 可选 | 可选 | 可选 |
| `system_fingerprint` | ✅ | **无** | **无** | **无** |
| `service_tier` | ✅ | **无** | **无** | **无** |
| `sglext` | **无** | ✅ | **无** | **无** |
| `prompt_token_ids` | **无** | **无** | ✅（首个 chunk） | **无** |
| `metrics` | **无** | **无** | **无** | ✅（collect_metrics=True） |

#### 流式 Choice 结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `index` | ✅ | ✅ | ✅ | ✅ |
| `delta` | ✅ | ✅ | ✅ | ✅ |
| `logprobs` | ✅ | ✅ | ✅ | ✅ |
| `finish_reason` | ✅ | ✅ | ✅ | ✅ |
| `matched_stop` | **无** | ✅ | **无** | **无** |
| `stop_reason` | **无** | **无** | ✅ | **无** |
| `token_ids` | **无** | **无** | ✅ | **无** |
| `draft_logprobs` | **无** | **无** | **无** | ✅ |
| `prompt_logprobs` | **无** | **无** | **无** | ✅ |
| `arrival_time` | **无** | **无** | **无** | ✅ |
| `speculate_metrics` | **无** | **无** | **无** | ✅ |

#### DeltaMessage 结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `role` | ✅（首个 chunk） | ✅ | ✅ | ✅ |
| `content` | ✅ | ✅ | ✅ | ✅ |
| `refusal` | ✅ | **无** | **无** | **无** |
| `tool_calls` | ✅ | ✅ | ✅ | ✅ |
| `reasoning_content` | **无** | ✅ | **无** | ✅ |
| `reasoning` | **无** | **无** | ✅ | **无** |
| `hidden_states` | **无** | ✅ | **无** | **无** |
| `multimodal_content` | **无** | **无** | **无** | ✅ |
| `audio_content` | **无** | **无** | **无** | ✅ |
| `prompt_token_ids` | **无** | **无** | **无** | ✅ |
| `completion_token_ids` | **无** | **无** | **无** | ✅ |

#### DeltaToolCall 结构

| 字段 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| `index` | ✅ | ✅ (Optional) | ✅ (Required) | ✅ |
| `id` | ✅ | `Optional` | `Optional` | `Optional` |
| `type` | ✅ | `"function"` | `Optional` | `Optional` |
| `function.name` | ✅ | `Optional` | `Optional` | `Optional` |
| `function.arguments` | ✅ | `Optional` | `Optional` | `Optional` |

#### 流式序列对比

```
┌─────────────┬──────────────────────────────────────────────┐
│ 阶段         │ 各框架行为                                    │
├─────────────┼──────────────────────────────────────────────┤
│ 首个 chunk   │ 四者一致: delta={role:"assistant", content:""} │
│             │ FD额外: reasoning_content=""                   │
│             │ FD额外: prompt_token_ids, prompt_logprobs      │
│             │ vLLM额外: prompt_token_ids(if return_token_ids)│
├─────────────┼──────────────────────────────────────────────┤
│ Echo chunk  │ 仅 vLLM 支持 (echo=True)                      │
├─────────────┼──────────────────────────────────────────────┤
│ 内容 chunk  │ 四者一致: delta={content:"..."}                 │
│             │ 推理: SGLang/FD用 reasoning_content,            │
│             │       vLLM用 reasoning                         │
├─────────────┼──────────────────────────────────────────────┤
│ 结束 chunk   │ 四者一致: finish_reason != null                │
│             │ 具体值见 finish_reason 差异表                    │
├─────────────┼──────────────────────────────────────────────┤
│ Usage chunk │ 四者一致: choices=[], usage={...}              │
│             │ SGLang/vLLM/FD 支持 continuous_usage_stats     │
├─────────────┼──────────────────────────────────────────────┤
│ 终止        │ 四者一致: data: [DONE]\n\n                      │
└─────────────┴──────────────────────────────────────────────┘
```

---

## 4. 错误处理对比

### 4.1 错误响应结构

| 维度 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| 外层 key | `{"error": {...}}` | `{"object":"error", "message":..., "type":..., "code":...}` | `{"error": {...}}` | `{"error": {...}}` |
| `error.message` | ✅ string | ✅ 顶层 `message` | ✅ string | ✅ string |
| `error.type` | ✅ string | ✅ 顶层 `type` | ✅ string | ✅ string（可 null） |
| `error.param` | ✅ string\|null | ✅ 顶层 `param`（可 null） | ✅ string\|null | ✅ string\|null |
| `error.code` | ✅ string\|null | ✅ 顶层 `code`（**int**） | ✅ **int** | ✅ string\|null |
| 额外字段 | 无 | `object: "error"` | 无 | 无 |

> **关键差异**：
> 1. SGLang 的错误响应**不嵌套**在 `error` 对象中，而是直接在顶层，且多一个 `object: "error"` 字段
> 2. SGLang 和 vLLM 的 `code` 是 **int**（HTTP 状态码），OpenAI 和 FD 的 `code` 是 **string**

### 4.2 错误类型映射

| 场景 | OpenAI | SGLang | vLLM | FastDeploy |
|------|--------|--------|------|------------|
| 请求参数错误 | 400 `invalid_request_error` | 400 `BadRequestError` | 400 `BadRequestError` | 400 `invalid_request_error` |
| 认证失败 | 401 `authentication_error` | N/A | N/A | 401 `{"error":"Unauthorized"}` |
| 权限不足 | 403 `permission_error` | N/A | N/A | N/A |
| 模型不存在 | 404 `not_found_error` | 400 `BadRequestError` | 404 `NotFoundError` | 500 `internal_error` |
| 速率限制 | 429 `rate_limit_error` | N/A | N/A | 429 HTTPException |
| 内部错误 | 500 `server_error` | 500 `InternalServerError` | 500 `InternalServerError` | 500 `internal_error` |
| 超时 | N/A | N/A | N/A | 500 `timeout_error` |
| 上下文长度超限 | 400 `context_length_exceeded` | 400 `BadRequestError` | 400 `BadRequestError` | 500 `context_length_exceeded` |
| 功能未实现 | N/A | N/A | 501 `NotImplementedError` | N/A |
| 客户端断开 | N/A | N/A | `"Client disconnected"` | 500 `client_aborted` |
| Worker 不健康 | N/A | N/A | N/A | 304 `"Worker Service Not Healthy"` |

### 4.3 流式错误处理

| 框架 | 格式 |
|------|------|
| OpenAI | `data: {"error":{...}}\n\n` 然后 `data: [DONE]\n\n` |
| SGLang | `data: {"error":{...}}\n\n`（使用 `create_streaming_error_response`） 然后 `data: [DONE]\n\n` |
| vLLM | `data: {"error":{...}}\n\n` 然后 `data: [DONE]\n\n` |
| FastDeploy | `data: {"error":{...}}\n\n` 然后 `data: [DONE]\n\n` |

> 流式错误格式基本一致，但 SGLang 的错误结构内嵌在 `error` key 下（与非流式不同）。

### 4.4 HTTP 状态码差异

| 框架 | 错误时 HTTP Status | 流式错误 HTTP Status |
|------|-------------------|---------------------|
| OpenAI | 对应错误码（400/401/403/404/429/500/502/503） | 开始前：对应错误码；开始后：200（错误在 SSE 中） |
| SGLang | 对应错误码（400/500 为主） | 200（错误在 SSE 中） |
| vLLM | `error.code` 值作为 HTTP status | 200（错误在 SSE 中） |
| FastDeploy | 500 为主（少量 400/401/429） | 200（错误在 SSE 中） |

---

## 5. Gateway 兼容性设计建议

### 5.1 透传策略（推荐）

由于 OneRouter Gateway 的核心职责是**路由转发**而非协议转换，建议采用**透传模式**：

1. **请求透传**：Gateway 不解析请求 body，直接转发到后端实例
2. **响应透传**：Gateway 不解析响应 body，直接将后端响应流式/非流式返回给客户端
3. **必要的元数据提取**：仅解析 Gateway 自身需要的少量字段（如 `model`、`stream`）

### 5.2 需要 Gateway 感知的字段

| 字段 | 用途 | 解析级别 |
|------|------|---------|
| `model` | 路由到正确的后端实例组 | 必须解析 |
| `stream` | 决定连接管理方式（长连接 vs 短连接） | 必须解析 |
| `request_id` / `rid` | 请求追踪和日志关联 | 建议解析 |
| `priority` | 调度优先级 | 可选解析 |

### 5.3 需要注意的兼容性陷阱

| 编号 | 陷阱 | 影响 | 建议 |
|------|------|------|------|
| 1 | SGLang 错误响应结构不同（非嵌套） | 客户端解析错误响应失败 | Gateway 可选择统一错误格式 |
| 2 | `error.code` 类型不一致（int vs string） | 客户端类型解析异常 | 统一转为 string 或 int |
| 3 | reasoning 字段名不同（`reasoning_content` vs `reasoning`） | 客户端取不到推理内容 | 透传模式下由客户端适配 |
| 4 | `top_k` 禁用值不同（-1 vs 0） | 不影响 Gateway | 透传即可 |
| 5 | SGLang `function.arguments` 可能是 Dict | 不符合 OpenAI 规范（应为 JSON string） | 透传模式下由客户端适配 |
| 6 | FD 的 `model` 默认 `"default"` | 路由时需要处理默认值 | Gateway 维护 model → 实例组映射 |
| 7 | FD 无 `tool_choice` 字段 | 如果客户端传了 tool_choice 到 FD 后端，可能被忽略 | 透传即可（FD 的 Pydantic 模型会忽略或报错） |
| 8 | 认证机制不一致 | FD 有认证，其他无 | Gateway 统一在 Gateway 层做认证，转发时移除/替换 token |
| 9 | FD 错误 HTTP status 多为 500 | 不便于客户端区分错误类型 | 可选在 Gateway 层统一错误码 |

### 5.4 错误响应统一方案（可选）

如果需要向客户端提供统一的错误体验，Gateway 可在转发后对错误响应做标准化：

```json
// 统一错误格式（OpenAI 兼容）
{
  "error": {
    "message": "具体错误信息",
    "type": "error_type",
    "param": null,
    "code": "error_code_string"
  }
}
```

转换规则：
- SGLang: 将顶层字段包装进 `{"error": {...}}`，`code` int → string
- vLLM: `code` int → string
- FastDeploy: 保持不变
- OpenAI: 保持不变

---

## 6. 附录：各框架协议文件索引

| 框架 | 协议定义文件 | Handler 文件 |
|------|------------|-------------|
| SGLang | `sglang/python/sglang/srt/entrypoints/openai/protocol.py` | `sglang/.../serving_chat.py`, `serving_base.py` |
| vLLM | `vllm/vllm/entrypoints/openai/chat_completion/protocol.py` | `vllm/.../chat_completion/serving.py` |
| FastDeploy | `FastDeploy/fastdeploy/entrypoints/openai/protocol.py` | `FastDeploy/.../serving_chat.py` |
| OpenAI | [API Reference](https://platform.openai.com/docs/api-reference/chat) | N/A（SaaS 服务） |

---

## 7. Golang 调用代码示例

> 以下示例展示如何用 Go 调用各后端的 `/v1/chat/completions` 接口（HTTP/1.1 + SSE 流式），
> 以及 SGLang/vLLM 的 gRPC 接口。所有代码可直接用于 OneRouter Gateway 的后端转发实现。

### 7.1 通用 HTTP/1.1 调用（适用于全部四种后端）

四种后端在 HTTP 模式下的 chat/completions 接口完全兼容，可复用同一套 Go 代码。

```go
package backend

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// ChatRequest 是发送给后端的请求体（透传模式下直接转发原始 JSON）。
type ChatRequest struct {
	Model    string        `json:"model"`
	Messages []interface{} `json:"messages"`
	Stream   bool          `json:"stream,omitempty"`
}

// HTTPClient 封装了对各后端 HTTP chat/completions 接口的调用。
type HTTPClient struct {
	client  *http.Client
	baseURL string // 如 "http://10.0.0.1:30000"（SGLang）、"http://10.0.0.1:8000"（vLLM/FD）
}

func NewHTTPClient(baseURL string) *HTTPClient {
	return &HTTPClient{
		client: &http.Client{
			Timeout: 5 * time.Minute, // SSE 长连接场景需要较长超时
			Transport: &http.Transport{
				MaxIdleConnsPerHost: 100,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		baseURL: strings.TrimRight(baseURL, "/"),
	}
}

// ChatCompletion 发起非流式请求，返回完整响应 body。
func (c *HTTPClient) ChatCompletion(ctx context.Context, reqBody []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
	}
	return body, nil
}

// ChatCompletionStream 发起 SSE 流式请求，通过 callback 逐 chunk 回调。
// callback 返回 false 时提前终止读取。
func (c *HTTPClient) ChatCompletionStream(
	ctx context.Context,
	reqBody []byte,
	onChunk func(data []byte) bool,
) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	resp, err := c.client.Do(req)
	if err != nil {
		return fmt.Errorf("do request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("backend returned %d: %s", resp.StatusCode, string(body))
	}

	// 解析 SSE 流：四种后端格式一致 —— "data: {json}\n\n" ... "data: [DONE]\n\n"
	scanner := bufio.NewScanner(resp.Body)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		if payload == "[DONE]" {
			break
		}
		if !onChunk([]byte(payload)) {
			break
		}
	}
	return scanner.Err()
}
```

#### 各后端调用区别仅在 baseURL 和默认端口

```go
// OpenAI
openai := NewHTTPClient("https://api.openai.com")
// 需额外设置: req.Header.Set("Authorization", "Bearer sk-xxx")

// SGLang（HTTP 默认模式，端口 30000）
sglang := NewHTTPClient("http://10.0.0.1:30000")

// vLLM（HTTP 默认模式，端口 8000）
vllm := NewHTTPClient("http://10.0.0.1:8000")

// FastDeploy（HTTP 唯一模式，端口 8000）
fastdeploy := NewHTTPClient("http://10.0.0.1:8000")
```

### 7.2 SGLang gRPC 调用

SGLang 通过 `--grpc-mode` 启动 gRPC 服务（与 HTTP 互斥）。由于其 proto 定义在外部包 `smg-grpc-proto` 中，
Go 侧需从该包生成 `.pb.go`，或根据 gRPC 反射动态调用。以下为基于 proto 生成代码的示例：

```go
package backend

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	// 假设已从 smg-grpc-proto 生成
	pb "github.com/yzx/rl-router/pkg/proto/sglang"
)

// SGLangGRPCClient 封装 SGLang gRPC Generate 调用。
type SGLangGRPCClient struct {
	conn   *grpc.ClientConn
	client pb.SGLangSchedulerClient
}

func NewSGLangGRPCClient(addr string) (*SGLangGRPCClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(256*1024*1024), // 256MB，与 SGLang 服务端一致
		),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	return &SGLangGRPCClient{
		conn:   conn,
		client: pb.NewSGLangSchedulerClient(conn),
	}, nil
}

// Generate 发起 server-streaming 生成请求。
// 注意：gRPC 模式下需发送 pre-tokenized input（token IDs），不支持原始文本。
func (c *SGLangGRPCClient) Generate(
	ctx context.Context,
	tokenIDs []int32,
	maxTokens int32,
	onToken func(resp *pb.GenerateResponse) bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	stream, err := c.client.Generate(ctx, &pb.GenerateRequest{
		InputIds:  tokenIDs,
		MaxTokens: maxTokens,
	})
	if err != nil {
		return fmt.Errorf("generate rpc: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		if !onToken(resp) {
			return nil
		}
	}
}

func (c *SGLangGRPCClient) Close() error {
	return c.conn.Close()
}
```

### 7.3 vLLM gRPC 调用

vLLM 的 gRPC 服务设计为 Rust Router ↔ Python Engine 的内部通信协议，
proto 定义位于 `vllm/grpc/vllm_engine.proto`。以下为 Go 调用示例：

```go
package backend

import (
	"context"
	"fmt"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	// 从 vllm/grpc/vllm_engine.proto 生成
	pb "github.com/yzx/rl-router/pkg/proto/vllm"
)

// VLLMGRPCClient 封装 vLLM gRPC VllmEngine 调用。
type VLLMGRPCClient struct {
	conn   *grpc.ClientConn
	client pb.VllmEngineClient
}

func NewVLLMGRPCClient(addr string) (*VLLMGRPCClient, error) {
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(
			grpc.MaxCallRecvMsgSize(-1), // unlimited，与 vLLM 服务端一致
		),
	)
	if err != nil {
		return nil, fmt.Errorf("grpc dial %s: %w", addr, err)
	}
	return &VLLMGRPCClient{
		conn:   conn,
		client: pb.NewVllmEngineClient(conn),
	}, nil
}

// Generate 发起 server-streaming 生成请求。
func (c *VLLMGRPCClient) Generate(
	ctx context.Context,
	req *pb.GenerateRequest,
	onToken func(resp *pb.GenerateResponse) bool,
) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	stream, err := c.client.Generate(ctx, req)
	if err != nil {
		return fmt.Errorf("generate rpc: %w", err)
	}

	for {
		resp, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return fmt.Errorf("recv: %w", err)
		}
		if !onToken(resp) {
			return nil
		}
	}
}

// HealthCheck 检测后端是否健康。
func (c *VLLMGRPCClient) HealthCheck(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	_, err := c.client.HealthCheck(ctx, &pb.HealthCheckRequest{})
	return err
}

func (c *VLLMGRPCClient) Close() error {
	return c.conn.Close()
}
```

### 7.4 Gateway 反向代理透传（当前 OneRouter 推荐方式）

OneRouter 当前采用透传模式，Gateway 不解析请求/响应 body，直接用 `httputil.ReverseProxy` 转发。
此方式天然兼容全部四种 HTTP 后端：

```go
package gateway

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

// SharedTransportProxy 复用 Transport 连接池的反向代理（优于每请求新建 Proxy）。
type SharedTransportProxy struct {
	transport *http.Transport
}

func NewSharedTransportProxy() *SharedTransportProxy {
	return &SharedTransportProxy{
		transport: &http.Transport{
			MaxIdleConnsPerHost:   256,
			MaxConnsPerHost:       512,
			IdleConnTimeout:       90 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
			// SSE 场景不设 TLSHandshakeTimeout 限制
		},
	}
}

func (p *SharedTransportProxy) Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error {
	target, err := url.Parse(targetEndpoint)
	if err != nil {
		return fmt.Errorf("parse target: %w", err)
	}

	proxy := &httputil.ReverseProxy{
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
		},
		Transport: p.transport, // 复用连接池
		FlushInterval: -1,      // SSE 立即 flush
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			http.Error(w, fmt.Sprintf("proxy error: %v", err), http.StatusBadGateway)
		},
	}

	proxy.ServeHTTP(w, r)
	return nil
}
```

### 7.5 协议选择决策树

```
客户端请求 /v1/chat/completions
        │
        ▼
   Gateway 解析 model → 确定后端类型
        │
        ├── OpenAI ──────────► HTTP/1.1 (HTTPS) ── net/http + TLS
        │
        ├── SGLang
        │     ├── HTTP 模式 ─► HTTP/1.1 ── httputil.ReverseProxy 透传
        │     └── gRPC 模式 ─► gRPC (HTTP/2) ── google.golang.org/grpc
        │                      ⚠️ 需 pre-tokenize，协议转换开销大
        │
        ├── vLLM
        │     ├── HTTP 模式 ─► HTTP/1.1 ── httputil.ReverseProxy 透传
        │     └── gRPC 模式 ─► gRPC (HTTP/2) ── google.golang.org/grpc
        │                      ⚠️ 内部接口，非官方用户 API
        │
        └── FastDeploy ──────► HTTP/1.1 ── httputil.ReverseProxy 透传
```

> **建议**：OneRouter Gateway 优先使用 HTTP/1.1 透传模式（当前实现），覆盖全部四种后端。
> gRPC 模式仅在需要绕过 tokenization 或追求极致内部通信效率时考虑，需额外引入 proto 依赖和协议转换层。
