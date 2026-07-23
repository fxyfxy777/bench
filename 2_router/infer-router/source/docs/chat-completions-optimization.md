# Chat Completions 接口优化总结

## 1. 优化背景

OneRouter 作为 RL 训练场景的 GPU 推理流量调度系统，其核心数据面接口是 OpenAI 兼容的 `/v1/chat/completions`。原有实现对所有请求统一使用 `httputil.ReverseProxy` 透明代理，功能上可用但在可观测性、容错性和资源利用效率上存在显著不足。

本次优化参考 sglang router 的成熟设计（`AttachedBody`、`AbortOnDropStream`、TTFT tracking、`[DONE]` sentinel detection 等），对流式和非流式两条路径进行差异化设计和实现。

---

## 2. 优化项与必要性

### 2.1 流式/非流式路径分离

**问题**：所有请求走同一条 `httputil.ReverseProxy` 路径，无法针对不同模式做差异化处理。

**优化**：通过零反序列化的 `parseStreamField` 扫描请求体中的 `"stream"` 字段，在请求入口处分叉到 `handleStreamChat` 或 `handleNonStreamChat` 两条独立路径。

**必要性**：
- 非流式请求可以缓冲完整响应，支持重试和 token 提取
- 流式请求可以追踪 TTFT、检测 `[DONE]` 等 SSE 特有信号
- 两条路径的错误处理和指标需求完全不同

### 2.2 非流式后端重试

**问题**：后端返回 502/503 或连接拒绝时，直接返回错误给 client，无法利用其他健康实例。

**优化**：非流式请求支持最多 2 次重试（共 3 次尝试），每次重新 Allocate 不同实例。仅对可重试错误（502/503/连接拒绝/超时）重试，4xx 等不重试直接透传。

**必要性**：
- 万卡集群中个别实例故障是常态，重试可将用户可见错误率降低一个数量级
- sglang router 同样对非流式实现了重试（流式不可重试，SSE headers 已发送）
- RL 训练场景中一次推理失败可能导致整个 step 重做，重试的 ROI 极高

### 2.3 TTFT（Time to First Token）指标

**问题**：无法衡量推理后端的首 token 延迟，这是 LLM Serving 最关键的性能指标。

**优化**：`streamTracker` 在首次 `Write` 调用时记录时间戳，与请求开始时间相减得到 TTFT，暴露为 Prometheus Histogram。

**必要性**：
- TTFT 直接反映模型推理的预填充（prefill）性能
- 监控 TTFT 可及时发现后端 KV Cache 压力、batch size 过大等问题
- 是 sglang、vLLM 等推理框架的标准可观测指标

### 2.4 `[DONE]` 检测与流完成状态

**问题**：无法区分流正常结束、client 断开、后端异常中断三种情况。

**优化**：`streamTracker` 在每次 `Write` 中扫描 `data: [DONE]` 标记，结合 proxy 返回的 error 判定流的最终状态（`done`/`interrupted`/`error`），暴露为 `stream_completion_total` Counter。

**必要性**：
- 流中断意味着推理结果不完整，RL 训练框架需据此决定是否重试
- 区分 client 主动断开 vs 后端故障，对运维排障至关重要
- sglang 通过 `memmem` 检测 `[DONE]` sentinel，是行业标准做法

### 2.5 Token 用量提取与反馈

**问题**：Scheduler 没有 token 级别的调度信息，只知道请求数和持续时间。

**优化**：非流式请求从 OpenAI 格式的响应体中提取 `usage.prompt_tokens`/`completion_tokens`/`total_tokens`，通过 `CostMetrics` 反馈给 Scheduler。

**必要性**：
- Token 数量是 GPU 显存和计算开销的直接度量
- 未来调度策略可基于 token 负载（而非请求数）做更精准的均衡
- 为实现 token-aware 调度策略（如 sglang 的 cache-aware）奠定数据基础

### 2.6 V2 ACK 路径 client disconnect 修复

**问题**：client 断开后 `streamForward` 中的 `DrainBody` 会继续读取后端全部数据，阻塞数分钟。

**优化**：使用 `context.WithCancel` 包装后端请求，在 write error（client 断开）时主动 `cancelBackend()`，后端请求立即中止。

**必要性**：
- RL 训练中 client 超时断开是常见场景（step timeout）
- 不取消后端 = GPU 空转继续推理 + 内存泄漏（goroutine 阻塞在 DrainBody）
- sglang 的 `AbortOnDropStream` 实现了相同模式

### 2.7 OpenAI 兼容 JSON 错误响应

**问题**：错误响应使用 `http.Error` 返回纯文本，不兼容 OpenAI SDK 的错误解析。

**优化**：统一使用 `writeErrorJSON` 返回 `{"error":{"message":"...","type":"..."}}` 格式。

**必要性**：
- PaddleRL 等训练框架使用 OpenAI SDK 发起请求，SDK 期望 JSON 格式的错误响应
- 纯文本错误会导致 SDK 解析失败，抛出误导性的异常信息

---

## 3. 收益总结

| 维度 | 收益 | 量化 |
|------|------|------|
| **可用性** | 非流式重试降低用户可见错误率 | 502/503 重试成功率预估 >80% |
| **可观测性** | TTFT + 流完成状态 + token 用量 | 5 个新 Prometheus 指标 |
| **资源效率** | client disconnect 立即释放 GPU | 从数分钟 drain → 立即取消 |
| **兼容性** | OpenAI JSON 错误格式 | SDK 可正确解析错误 |
| **调度精度** | Token 级别负载反馈 | 为 token-aware 策略奠基 |
| **排障效率** | 区分流正常/中断/错误 | 秒级定位流式推理异常 |

---

## 4. 适用场景

### 4.1 强化学习训练（核心场景）

- 训练框架批量发起推理请求，部分实例偶发故障 → **非流式重试**避免整个 batch 失败
- 训练 step 超时控制 → **client disconnect 取消后端**释放 GPU
- 长时间训练需精细化运维 → **TTFT + 流状态指标**提前发现性能退化

### 4.2 在线推理服务

- 非流式 API 调用（如 embedding、分类） → **重试**提升可用性
- 流式 chat 场景（如对话机器人） → **TTFT 指标**衡量用户体验
- 多实例负载均衡 → **token 用量反馈**辅助更精准的调度

### 4.3 大规模集群管理

- 万卡集群个别节点故障是常态 → **重试 + 错误分类**自动容错
- GPU 资源昂贵 → **disconnect 取消**避免空转浪费
- 运维需要一目了然的指标 → **per-instance 指标**快速定位问题节点

---

## 5. 不适用场景与已知限制

| 限制 | 原因 | 后续计划 |
|------|------|---------|
| 流式请求不支持重试 | SSE headers 已发送，无法回滚 HTTP 状态码 | 行业共性限制，sglang 同样不支持 |
| 流式 token 不提取 | 需逐 chunk 解析 SSE JSON，万卡场景性能影响需评估 | 作为 P2 优化项单独评估 |
| 无背压控制 | SSE proxy 无 bounded buffer | 行业共性（sglang 同样 unbounded） |
| 非流式大响应 (>256KB) | buffer 不回池，依赖 GC | 极少见场景，监控 pool miss 率即可 |

---

## 6. 新增文件清单

| 文件 | 说明 |
|------|------|
| `internal/gateway/stream_detect.go` | `parseStreamField`：零反序列化 stream 字段检测 |
| `internal/gateway/stream_detect_test.go` | 15 个单测 + 2 个 benchmark |
| `internal/gateway/usage.go` | `tokenUsage` + `extractUsage`：response body token 提取 |
| `internal/gateway/usage_test.go` | 9 个单测 + 1 个 benchmark |
| `internal/gateway/stream_tracker.go` | SSE 流状态追踪器（TTFT / [DONE] / chunks） |
| `internal/gateway/stream_tracker_test.go` | 13 个单测 + 2 个 benchmark |
| `internal/gateway/forward_nonstream.go` | 非流式转发 + 重试 + buffer pool |
| `internal/gateway/forward_nonstream_test.go` | 13 个单测 |

修改的文件：`server.go`、`v2_chat.go`、`domain/models.go`、`router.proto`、`allocator.go`、`grpc_handler.go`、`metrics.go`、`logger/fields.go`
