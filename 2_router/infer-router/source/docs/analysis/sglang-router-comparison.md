# OneRouter vs SGLang Router 对比分析

## Context

对比 SGLang 的 sgl-model-gateway（Rust 实现）与 OneRouter（Go 实现），识别缺失功能和可借鉴的设计模式。
两者定位相似——面向推理后端的 HTTP 级别跨实例流量路由器。SGLang 内部还有 Python 层的 GPU 级调度（Scheduler + RadixCache），
属于推理引擎内部，不在直接对比范围，但其设计思想值得参考。

本文档将保存到 `docs/analysis/sglang-router-comparison.md`。

---

## 一、架构层对比

### 1.1 整体架构差异

```
SGLang sgl-model-gateway (Rust):
┌──────────────────────────────────────────────────┐
│  HTTP Gateway (Axum)                             │
│  ┌─────────┐  ┌──────────┐  ┌───────────────┐   │
│  │ Middleware│→│  Router   │→│ Worker Registry│   │
│  │ RateLimit│  │ (Policy)  │  │ Health + CB   │   │
│  └─────────┘  └──────────┘  └───────────────┘   │
│       每个请求独立路由，无中心状态                   │
└──────────────────────────────────────────────────┘

OneRouter (Go):
┌─────────────────────┐     gRPC     ┌──────────────────────┐
│  Gateway             │◄───────────►│  Scheduler            │
│  ┌───────┐ ┌──────┐ │             │  ┌──────────┐        │
│  │ Proxy │ │Alloc │ │             │  │Event Loop│        │
│  │ (fwd) │ │Client│ │             │  │ (串行化)  │        │
│  └───────┘ └──────┘ │             │  ├──────────┤        │
│                      │             │  │ Policy   │        │
│  N 个 Gateway 实例    │             │  │ Store    │        │
└─────────────────────┘             │  │ Collector│        │
                                     │  1 个 Scheduler      │
                                     └──────────────────────┘
```

| 维度 | SGLang | OneRouter |
|------|--------|-----------|
| 架构模型 | 去中心化 Gateway | 中心化 Scheduler + 分布式 Gateway |
| 状态持有 | 每个 Gateway 独立维护 worker 列表 | Scheduler 持有全局状态，Gateway 无状态 |
| 调度精度 | 本地视角（近似） | 全局视角（精确） |
| 单点风险 | 无 | Scheduler 是单点 |
| 一致性 | 弱（mesh gossip 同步） | 强（event-loop 串行化） |
| 实现语言 | Rust（零成本抽象 + lock-free） | Go（goroutine + channel） |

### 1.2 对齐能力清单

| 能力 | OneRouter 实现 | SGLang 实现 | 差距 |
| --- | --- | --- | --- |
| 调度策略 | 4 种 (min_load, round_robin, min_request, session_aware) | 8 种 | OneRouter 缺 ConsistentHashing, PrefixHash, Bucket, Manual |
| Cache-Aware | session_id → instance 映射，O(1) | Radix Tree + Shortest Queue 动态切换 | SGLang 分析请求内容，更精准 |
| P2C | >16 节点时随机取 2 选低分 | PowerOfTwo + token 级负载 | SGLang 支持 token 级 |
| 后端健康检查 | **双通道**: 独立 HealthChecker (`/health` 探针，SuccessThreshold 恢复) + MetricsCollector (指标采集) | 连续失败/成功双阈值 + 周期探测 | **完全对齐**: 双通道互补 |
| Circuit Breaker | **已实现**: per-instance 状态机 Closed→Open→HalfOpen→Closed，event-loop 单线程 plain fields | 全 atomic 实现 (Closed/Open/HalfOpen)，CAS 状态转换 | **功能对齐**: 设计差异（单线程 vs atomic） |
| 非流式请求重试 | **已实现**: 最多 2 次 backend retry + 换实例 (`forward_nonstream.go`) | RetryExecutor，最多 5 次，指数退避+抖动 | OneRouter 保守 2 次，SGLang 通用 5 次 |
| SSE 流式代理 | `FlushInterval: -1` + streamTracker (TTFT + [DONE] 检测) | Axum streaming | 对齐，OneRouter 额外有流式完成追踪 |
| Prometheus 指标 | 28 项 | 6 层 ~40+ 项 | 差距收窄，OneRouter 有独有的幂等性/event-loop/流式完成指标 |
| TTFT 追踪 | **已实现**: `time_to_first_byte_ms{instance_id, mode}` | `ttft_seconds` | **对齐** |
| Token 计数 | **部分**: `tokens_prompt_total` + `tokens_completion_total` (非流式) | `tokens_total` (input/output/total) | SGLang 更精细（三维度 + 流式） |
| 流式完成追踪 | **已实现**: `stream_completion_total{result}` (done/interrupted/error) | — | **OneRouter 独有** |
| Rate Limiting | 无 | Token Bucket + FIFO 排队 (queue_size=100, timeout=60s) | **OneRouter 缺失** (P0) |
| OpenTelemetry | 仅 X-Trace-ID 透传 | OTLP/gRPC export + W3C Trace Context | **OneRouter 缺失** (P1) |
| 幂等性 | allocate/release dedup (131072 预分配) | — (无集中调度) | OneRouter 优于 SGLang |
| Panic Recovery | HTTP + gRPC middleware | — (Rust safe by default) | OneRouter 有需求，已实现 |
| 批量调度 | BatchSelect (inline min-heap, O(N+K*logN)) | — | OneRouter 独有优势 |
| Step 隔离 | 状态机 IDLE→SERVING→DRAINING→IDLE | — | OneRouter 独有（RL 训练场景） |
| 漂移补偿 | DriftAware (acquire+1/release-1) | — | OneRouter 独有 |

### 1.3 Prometheus 指标对比（28 vs 40+）

#### OneRouter 指标（28 项，namespace: `rl_router`）

| 类别 | 指标名 | 类型 | 标签 |
| --- | --- | --- | --- |
| **请求核心** | `requests_total` | Counter | instance_id, status |
| | `active_requests` | Gauge | instance_id |
| | `request_duration_ms` | Histogram | instance_id |
| **调度** | `registered_gateways` | Gauge | — |
| | `step_phase` | Gauge | — |
| | `step_id` | Gauge | — |
| | `heartbeat_total` | Counter | gateway_id |
| **幂等性** | `alloc_dedup_hits_total` | Counter | — |
| | `alloc_caller_gone_skips_total` | Counter | — |
| | `release_dedup_hits_total` | Counter | — |
| | `release_retries_total` | Counter | — |
| **Event Loop** | `event_channel_depth` | Gauge | — |
| | `event_loop_batch_size` | Histogram | — |
| **代理** | `proxy_backend_status_total` | Counter | instance_id, status_class |
| | `non_stream_retries_total` | Counter | — |
| **稳定性** | `panic_recoveries_total` | Counter | layer |
| **Collector** | `metrics_sweep_duration_seconds` | Histogram | — |
| | `metrics_sweep_instances` | Gauge | — |
| | `metrics_sweep_failures` | Gauge | — |
| | `metrics_unhealthy_instances` | Gauge | — |
| **Health Checker** | `health_probe_results_total` | Counter | result |
| | `health_probe_unhealthy_instances` | Gauge | — |
| | `health_sweep_duration_seconds` | Histogram | — |
| **Circuit Breaker** | `circuit_breaker_trips_total` | Counter | instance_id, transition |
| **Token** | `tokens_prompt_total` | Counter | instance_id |
| | `tokens_completion_total` | Counter | instance_id |
| **推理 SLO** | `time_to_first_byte_ms` | Histogram | instance_id, mode |
| | `stream_completion_total` | Counter | instance_id, result |

#### SGLang 独有的指标类别（OneRouter 缺失）

| 指标类别 | 代表指标 | 用途 | OneRouter 替代方案 |
| --- | --- | --- | --- |
| **推理 SLO** | `smg_router_tpot_seconds`, `generation_duration_seconds` | 每 token 生成耗时、端到端生成耗时 | 缺失（需推理引擎配合） |
| **In-flight** | `smg_http_inflight_request_age_count` | 请求年龄分布（30s~24h 桶），发现卡住请求 | 缺失 |
| **Rate Limit** | `smg_http_rate_limit_total` | 限流接受/拒绝/排队计数 | 缺失（Rate Limiting 未实现） |
| **Discovery** | `smg_discovery_*` | 服务发现相关（注册/同步/发现数量） | 不适用（OneRouter 用 HTTP API 注册） |
| **MCP** | `smg_mcp_*` | Tool calling 调用次数/耗时 | 不适用（RL 场景不需要） |
| **DB** | `smg_db_*` | 会话存储操作计数/耗时 | 不适用（OneRouter 全内存） |
| **Worker 细分** | `smg_worker_selection_total`, `connections_active`, `routing_keys_active` | Per-worker 选中次数、活跃连接、路由 key | 部分替代：`active_requests` per-instance |

#### 指标设计差异

| 维度 | SGLang | OneRouter |
| --- | --- | --- |
| 标签值优化 | String Interning (DashMap + `Arc<str>`)，static lookup table | `sync.Map` 缓存 per-instance metric handles (`GetInstanceMetrics`) |
| 热路径开销 | 1 次 DashMap read + Arc::clone per label | 1 次 atomic load per instance（4 mutex → 1 atomic） |
| 指标清理 | — | `PurgeInstanceMetrics()` 清理下线实例缓存 |

---

## 二、OneRouter 缺失的功能详解

### 缺失功能进度总览

| ID | 功能 | 优先级 | 状态 | 实现位置 |
| --- | --- | --- | --- | --- |
| P0-1 | Circuit Breaker | P0 | ✅ 已完成 | `circuitbreaker/breaker.go`, `manager.go` |
| P0-2 | 请求级重试 | P0 | ⚠️ 非流式完成 | `forward_nonstream.go` |
| P0-3 | Rate Limiting / 并发控制 | P0 | ❌ 未开始 | — |
| P1-1 | Radix Tree Cache-Aware | P1 | ❌ 未开始 | — |
| P1-2 | Consistent Hashing | P1 | ❌ 未开始 | — |
| P1-3 | Token 级负载感知 | P1 | ❌ 未开始 | — |
| P1-4 | OpenTelemetry Tracing | P1 | ❌ 未开始 | — |
| P1-5 | TTFT / TPOT | P1 | ⚠️ TTFT 完成 | `stream_tracker.go` |
| P2-1 | PrefixHash | P2 | ❌ 未开始 | — |
| P2-2 | Manual/Sticky 路由 | P2 | ❌ 未开始 | — |
| P2-3 | 请求排队 | P2 | ❌ 未开始 | — |
| P2-4 | K8s Service Discovery | P2 | ❌ 未开始 | — |
| P2-5 | In-flight Age Tracking | P2 | ❌ 未开始 | — |
| 新增 | Backend Health Checker | P0 | ✅ 已完成 | `healthcheck/checker.go` |
| 新增 | Panic Recovery | P0 | ✅ 已完成 | `pkg/logger/recovery.go` |
| 新增 | `/v1/completions` 端点 | P2 | ✅ 已完成 | `server.go` → `handleInferenceRequest` |
| 新增 | `/generate` 端点 | P2 | ✅ 已完成 | `server.go` → `handleInferenceRequest` |
| 新增 | Token Usage 提取 | P1 | ⚠️ 非流式完成 | `usage.go` |
| 新增 | Stream Completion Tracking | P1 | ✅ 已完成 | `stream_tracker.go` |

### P0 — 直接影响万卡稳定性

---

#### P0-1. Circuit Breaker（熔断器）

**SGLang 实现**: [circuit_breaker.rs](sgl-model-gateway/src/core/circuit_breaker.rs)

```
状态机: Closed ──(failures>=5)──→ Open ──(30s timeout)──→ HalfOpen
                                   ↑                         │
                                   └──(1 failure)────────────┘
         Closed ←──(successes>=2)── HalfOpen
```

核心设计：
- **全 atomic 实现**，热路径零锁开销
  - `state: AtomicU8` (0=Closed, 1=Open, 2=HalfOpen)
  - `consecutive_failures: AtomicU32`
  - 状态转换使用 `compare_exchange(AcqRel)` CAS
  - 信息性计数器使用 `Relaxed` ordering
- **Monotonic epoch**: 使用 `OnceLock<Instant>` + elapsed，避免 SystemTime 系统调用
- 配置: `failure_threshold=5, success_threshold=2, timeout=30s, window=60s`
- 每次状态转换上报 Prometheus metric

**OneRouter 实现**: `internal/scheduler/circuitbreaker/breaker.go` + `manager.go` — ✅ 已完成

已完整实现，核心设计：

- **状态机**: Closed ──(failures >= FailThreshold)──→ Open ──(OpenDuration elapsed)──→ HalfOpen ──(successes >= SuccessThreshold)──→ Closed; HalfOpen ──(any failure)──→ Open
- **单线程设计**: 运行于 scheduler event-loop 内，所有字段为 plain（非 atomic），零锁开销
- **懒分配 (Lazy allocation)**: Manager 仅在首次失败时创建 Breaker，健康实例不占内存
- **GC 清理**: Breaker 回到 Closed 后从 map 中删除，防止内存膨胀
- **配置**: `FailThreshold=3, SuccessThreshold=2, OpenDuration=30s`（可通过 YAML / CircuitBreakerConfig 配置）
- **集成点**:
  - Release RPC 的 `CostMetrics.ErrorCode` 非空时触发 `RecordFailure()`
  - `NodeState.CircuitOpen` (atomic int32) 由 event-loop 写入
  - 所有 policy 通过 `NodeState.LoadAvailable()` 检查：`Healthy == 1 && CircuitOpen == 0`
  - `LogHeartbeat()` 定期调用 `SyncToNodeState()` 处理 Open→HalfOpen 的时间推进
  - `StartStep` 时调用 `Reset()` 清空所有 breaker 状态
- **指标**: `circuit_breaker_trips_total{instance_id, transition}` — transition: "open", "half_open", "closed"

**与 SGLang 的设计差异**: SGLang 使用全 atomic 实现（多线程访问 + CAS 状态转换），OneRouter 使用 plain fields（event-loop 串行化，零开销）。功能完全对齐，性能特性不同。

---

#### P0-2. 请求级重试 + 故障转移

**SGLang 实现**: [retry.rs](sgl-model-gateway/src/core/retry.rs)

```go
// 伪代码：SGLang 的 RetryExecutor 模式
func ExecuteWithRetry(config, operation, shouldRetry, onBackoff, onExhausted) Response {
    for attempt := 0; attempt < config.MaxRetries; attempt++ {
        resp := operation(attempt)
        if !shouldRetry(resp, attempt) { return resp }
        if attempt+1 >= config.MaxRetries {
            onExhausted()
            return resp  // 返回最后一次失败的响应，不是 error
        }
        delay := Backoff(config, attempt)  // 指数退避 + 抖动
        onBackoff(delay, attempt+1)
        time.Sleep(delay)
    }
}
```

核心设计：
- **回调分离**: 4 个 hook（operation/shouldRetry/onBackoff/onExhausted）把重试编排与业务逻辑完全解耦
- **可重试状态码**: 408, 429, 500, 502, 503, 504
- **退避公式**: `delay = min(initial * multiplier^attempt, maxBackoff) * (1 + Uniform[-jitter, +jitter])`
- 默认配置: `max_retries=5, initial_backoff=50ms, max_backoff=30s, multiplier=1.5, jitter=0.2`
- **返回最后响应而非 error**: 调用者始终拿到有效 HTTP 响应

**OneRouter 现状**: **非流式请求已实现 backend retry**。⚠️ 非流式已完成，流式不适用。

`internal/gateway/forward_nonstream.go`:

- 最多 2 次 retry（`nonStreamMaxRetries = 2`）
- 触发条件：`isRetryableError` (net.OpError, context.DeadlineExceeded) 或 `isRetryableStatus` (502, 503)
- 每次 retry: Release 当前分配 → 重新 Allocate（获取新实例）→ 重新 Forward
- 使用 pooled response buffers（64KB 初始，超 256KB 丢弃）
- 指标: `non_stream_retries_total`

**流式请求**: SSE 流一旦开始（HTTP 200 + headers 已发送），数据已到达客户端，不可重试。这是正确的设计决策。

**与 SGLang 差距**:

- SGLang: 通用 RetryExecutor 模式，5 次重试 + 指数退避 + 抖动，回调分离（4 个 hook）
- OneRouter: 固定 2 次，无退避延迟（推理请求耗时长，重试次数应保守），内联在 handleNonStreamChat 中
- SGLang 每次 retry 重新选择 worker（OneRouter 同样：Release + 重新 Allocate）

**剩余建议**: 可选实现退避 + 抖动，但 2 次无延迟在 RL 场景已够用（每次 retry 换实例，大概率成功）。

---

#### P0-3. 请求级 Rate Limiting / 并发控制

**SGLang 实现**: [token_bucket.rs](sgl-model-gateway/src/core/token_bucket.rs) + [middleware.rs](sgl-model-gateway/src/middleware.rs)

核心设计：
- **Token Bucket 双模式**:
  - `refill_rate > 0`: 传统令牌桶（时间回填）
  - `refill_rate == 0`: 纯并发限制器（信号量语义，token 只能通过 return 归还）
- **TokenGuardBody**（SSE token 泄漏防护，最巧妙的设计之一）:
  ```rust
  struct TokenGuardBody {
      inner: Body,
      token_bucket: Option<Arc<TokenBucket>>,
      tokens: f64,
  }
  impl Drop for TokenGuardBody {
      fn drop(&mut self) {
          // SSE 流结束时自动归还 token，无论正常/异常
          bucket.return_tokens_sync(self.tokens);
      }
  }
  ```
  关键: 使用 `parking_lot::Mutex`（同步锁）而非 `tokio::Mutex`，因为 `Drop` 是同步的
- **有界排队**: `queue_size=100, timeout=60s`，满时返回 429
- 默认: `max_concurrent_requests=256`

**OneRouter 现状**: 仅靠 event channel (131072 buffer) 做隐式背压，无显式限流。

**为什么必须做**:
- 10w 瞬时请求场景，无并发控制 → goroutine 爆炸 / OOM
- SSE 长连接场景，一个连接可能持续数分钟，必须限制并发连接数
- 需要区分"正在 proxy 的请求"和"等待分配的请求"

**建议实现**:
- 在 Gateway 层实现 `semaphore` 或 `token bucket`（Go 用 `golang.org/x/sync/semaphore`）
- SSE 响应结束时释放 token：用 `defer` 在 `HandleChatCompletion` 结尾释放，或实现类似 TokenGuardBody 的 ResponseWriter wrapper
- 超限时排队而非立即拒绝（推理请求不可简单重试）

---

### P1 — 影响路由质量和运维效率

---

#### P1-1. 近似 Radix Tree Cache-Aware 路由

**SGLang 实现**: [session_aware.rs](sgl-model-gateway/src/policies/session_aware.rs) + [tree.rs](sgl-model-gateway/src/policies/tree.rs)

```
算法核心:
1. 计算所有 worker 的 (min_load, max_load)
2. 判断是否失衡:
   imbalanced = (max - min > abs_threshold=32) AND (max > min * rel_threshold=1.1)
3. 失衡 → Shortest Queue（选最低负载的 worker）
4. 均衡 → Radix Tree 前缀匹配:
   match_rate = matched_chars / input_chars
   if match_rate > cache_threshold(0.5) → 路由到匹配的 worker（cache hit）
   else → 路由到最低负载 worker（最多 cache 空间）
5. 所有路由结果都插入 tree，维护近似状态
```

Radix Tree 设计亮点:
- **字符级**（非 token 级），避免 gateway 层 tokenization 开销
- **多租户**: 每个节点跟踪哪些 worker URL 拥有它 (`tenant_last_access_time`)
- **概率性时间戳更新**: `epoch & 0x7 == 0`（1/8 概率写入），减少 DashMap 写竞争
- **分级 shard**: root 节点 32 shards（高竞争），内部节点 8 shards，节省 ~90% 内存
- **Custom CharHasher**: 单字符 identity hash + golden ratio mixing，避免 SipHash 开销
- **ASCII 快速路径**: `advance_by_chars()` 先尝试字节级操作，回退到 `char_indices()`
- **LRU 驱逐**: 基于 atomic epoch（非 wall clock），无系统调用

**OneRouter 现状**: SessionAwarePolicy 仅做 `session_id → instance_id` 静态映射，不分析请求内容。

**为什么值得做**:
- rollout 场景中同一 prompt template 被大量使用，前缀高度重复
- SGLang 论文数据：cache-aware 路由可提升 2-5x 吞吐
- OneRouter 的 Scheduler 有全局视角，天然适合维护 Radix Tree

**建议实现**:
- **短期**: PrefixHash 策略（下面 P2-1，O(1) per-request，无树开销）
- **长期**: 在 Scheduler 侧维护轻量 Radix Trie，AllocateRequest 中携带 prompt 摘要/hash
- 关键：OneRouter 的 event-loop 是单线程的，tree 不需要并发安全，比 SGLang 的 DashMap 实现更简单

---

#### P1-2. Consistent Hashing 策略

**SGLang 实现**: [consistent_hashing.rs](sgl-model-gateway/src/policies/consistent_hashing.rs)

```
Hash Ring:
- 每个 worker 150 个虚拟节点
- blake3 hash: hash(worker_url + "#" + vnode_index_le_bytes)
- 预计算排序数组，二分查找 O(log(n*150))
- 顺时针 walk 跳过不健康 worker

路由优先级:
1. X-SMG-Target-Worker → 直接索引路由
2. X-SMG-Routing-Key → 一致性哈希
3. 隐式 key: Authorization / X-Forwarded-For / Cookie → 一致性哈希
4. 回退: 随机选择
```

**OneRouter 现状**: 无一致性哈希。SessionAware 在实例变动时丢失所有亲和关系。

**为什么值得做**:
- 扩缩容时最小化 key 重分配（~1/N 迁移），对 KV cache 友好
- 推理实例弹性伸缩时，减少 cache 冷启动

---

#### P1-3. Token 级负载感知

**SGLang 实现**: [power_of_two.rs](sgl-model-gateway/src/policies/power_of_two.rs)

```
核心逻辑:
- 从外部监控获取 per-worker 的 token 级负载 (token count, not request count)
- 选 2 个随机 worker，比较 token 负载选低的
- 不兼容处理: 部分 worker 有 token 数据、部分没有 → 统一回退到 request count
```

**OneRouter 现状**: MinLoadPolicy 的 composite score = `ActiveRequests + WaitingCount * Coefficient`，仅 request 级别。Collector 已采集 WaitingCount、RunningCount、AvailableBlocks，但 P2C 选择未用 token 级指标。

**为什么值得做**:
- 推理请求 token 数差异巨大（10~4000 tokens），请求数≠GPU 负载
- OneRouter 已有基础设施（Collector + MetaUpdater），补 token 维度成本低

---

#### P1-4. 分布式 Tracing（OpenTelemetry）

**SGLang 实现**: [otel_trace.rs](sgl-model-gateway/src/observability/otel_trace.rs)
- Trace context 注入，跨 gateway→worker 分布式追踪

**OneRouter 现状**: 仅 `X-Trace-ID` header 透传，无 span 上报。

---

#### P1-5. 推理 SLO 指标（TTFT / TPOT）

**SGLang 实现**: 6 层指标体系

| 关键推理指标 | SGLang | OneRouter | 状态 |
| --- | --- | --- | --- |
| `ttft_seconds` / `time_to_first_byte_ms` | 有 | **已实现**: `streamTracker` 记录首次 Write 时间 | ✅ |
| `tpot_seconds` (Time Per Output Token) | 有 | 无（需推理引擎配合上报 per-token 时间戳） | ❌ |
| `tokens_total` (input/output/total) | 有 | **部分**: `tokens_prompt_total` + `tokens_completion_total` (从非流式响应 `usage` 字段提取) | ⚠️ |
| `generation_tokens` | 有 | 无 | ❌ |
| `upstream_responses_total` (per status) | 有 | **已实现**: `proxy_backend_status_total{instance_id, status_class}` | ✅ |
| `inflight_request_age` (age buckets) | 有 (30s~24h 桶) | 无 | ❌ |
| circuit breaker state/transitions | 有 | **已实现**: `circuit_breaker_trips_total{instance_id, transition}` | ✅ |
| `stream_completion_total` (done/interrupted/error) | 无 | **已实现**: streamTracker [DONE] 检测 + 三态分类 | ✅ OneRouter 独有 |
| `non_stream_retries_total` | 有 (worker_retries) | **已实现** | ✅ |

**String Interning 优化** (SGLang metrics.rs):
```rust
// Global interner: 首次调用分配，后续调用仅 Arc::clone
static STRING_INTERNER: Lazy<DashMap<String, Arc<str>>> = ...;
fn intern_string(s: &str) -> Arc<str> {
    if let Some(entry) = STRING_INTERNER.get(s) { return Arc::clone(entry.value()); }
    STRING_INTERNER.entry(s.to_string()).or_insert_with(|| Arc::from(s)).clone()
}
```

**OneRouter 已有的优化**: `sync.Map` 缓存 per-instance metric handles (GetInstanceMetrics)，减少 4 次 mutex → 1 次 atomic。

**OneRouter TTFT 实现详情**: `internal/gateway/stream_tracker.go`

- `streamTracker` wraps `http.ResponseWriter`，记录第一次 `Write()` 调用的时间作为 TTFT
- 同时检测 `data: [DONE]` sentinel 判断流是否正常结束
- 结果分类：`done`（检测到 [DONE]）/ `interrupted`（客户端断开）/ `error`（proxy 错误）
- 指标:
  - `time_to_first_byte_ms{instance_id, mode}` — mode 为 "stream"/"non_stream"（histogram，10ms~81.9s）
  - `stream_completion_total{instance_id, result}` — result: "done" / "interrupted" / "error"
- 零缓冲设计：SSE chunk 通常很小（数十~数百字节），`bytes.Contains` 扫描 < 1μs

**Token 计数实现**: `internal/gateway/usage.go`

- 非流式: 从 JSON 响应 `usage` 字段提取 `prompt_tokens` / `completion_tokens`
- 支持 OpenAI 格式和 SGLang `/generate` 的 `meta_info` 格式
- 指标: `tokens_prompt_total{instance_id}`, `tokens_completion_total{instance_id}`
- 流式: 目前不提取 token 数（需解析每个 SSE chunk 的 delta，复杂度高）

**仍缺失**: TPOT（Time Per Output Token）需要 per-token 时间戳，只有推理引擎内部才能精确提供。

---

### P2 — 增强功能

---

#### P2-1. PrefixHash 轻量级 Cache 亲和

**SGLang 实现**: [prefix_hash.rs](sgl-model-gateway/src/policies/prefix_hash.rs)

```
算法:
1. 取前 N 个 token (default: 256)
2. hash = xxh3_64(tokens[..prefix_len])
3. Consistent ring lookup → 找到目标 worker
4. Load-bounded check: load <= avg_load * load_factor(1.25)
   - 超载 → walk ring 找下一个未超载的
   - 全超载 → 回退到初始 worker
```

| 对比 | PrefixHash | Radix Tree |
|------|-----------|------------|
| 时间复杂度 | O(log n) ring lookup | O(prefix_len) tree walk |
| 空间复杂度 | O(workers × 虚拟节点) | O(total_tokens) |
| 精度 | 前缀分组（粗粒度） | 精确匹配 |
| 实现复杂度 | 低 | 高 |

**建议**: 作为 Radix Tree 的前置方案先实现，ROI 最高。

---

#### P2-2. Manual/Sticky 路由策略

**SGLang 实现**: [manual.rs](sgl-model-gateway/src/policies/manual.rs)
- `X-Routing-Key` header → 强亲和
- 双候选 worker failover
- TTL 过期清理 (default: 4h)
- 不参与扩缩容重分配（比 ConsistentHashing 更"粘"）

---

#### P2-3. 请求排队 + 排队等待

**SGLang 实现**: middleware.rs
```
请求到达 → 尝试获取 token
  → 成功 → 处理请求
  → 失败 → 进入有界队列 (size=100)
     → 队列已满 → 429 Too Many Requests
     → 排队等待 → 获取 token 或超时 (60s)
        → 超时 → 408 Request Timeout
```

**为什么排队比直接拒绝好**: 推理请求不可简单重试，训练框架可能没有重试逻辑。排队 60s 内等到资源比拒绝后客户端盲重试更优。

---

#### P2-4. K8s Service Discovery

**SGLang 实现**: [service_discovery.rs](sgl-model-gateway/src/service_discovery.rs)
- kube-rs watch Pod 就绪状态
- Label selector 区分 Regular/Prefill/Decode
- 自动注册/注销

**OneRouter 现状**: 仅 HTTP API 手动注册。万卡场景实例频繁启停，手动不现实。

---

#### P2-5. In-flight Request Age Tracking

**SGLang 实现**: [inflight_tracker.rs](sgl-model-gateway/src/observability/inflight_tracker.rs)
```
Age buckets: [30s, 1m, 3m, 5m, 10m, 20m, 1h, 2h, 4h, 8h, 24h]

RAII 模式:
  guard = tracker.track()   // DashMap.insert(id, Instant::now())
  defer guard.Drop()        // DashMap.remove(id)

周期采样 → gauge histogram: smg_http_inflight_request_age_count
```

**用途**: 快速发现"卡住"的请求（年龄异常大），半年运行中非常有价值。

---

## 三、SGLang Python 层可借鉴思想

以下功能不在 sgl-model-gateway 中，而是在 SGLang 推理引擎内部的 Python Scheduler 层。
它们与 OneRouter 不直接对标，但设计理念可以参考。

### 3.1 Token Budget 批次控制

**SGLang**: `PrefillAdder` 按 token 预算（而非请求数）构建 batch:
- `rem_total_tokens`: 可用 KV cache 总量
- `rem_input_tokens`: 每批最大输入 token
- `rem_chunk_tokens`: 每 chunk 最大 token
- 超额请求被 chunk 化（截断后下一轮继续）

**OneRouter 借鉴**: BatchSelect 目前按请求数批量分配。可扩展为"每批最多 N tokens"的约束，在 Collector 采集 token 级指标后，BatchSelect 按 token budget 分配。

### 3.2 优先级调度 + 抢占

**SGLang**: 请求带 `priority` 字段，高优先级可抢占低优先级:
- `preempt_to_schedule()`: 收回低优先级运行中的请求腾出资源
- 阈值: `priority_scheduling_preemption_threshold`

**OneRouter 借鉴**: 在 `AllocateRequest` 中增加 `priority` 字段，`BatchSelect` 先排序再分配。Scheduler 全局视角天然适合做优先级调度。

### 3.3 Retract / 降级

**SGLang**: KV cache 不足时自动回收运行中请求:
- 按 (output_length DESC, input_length ASC) 排序，回收"输出最短、输入最长"的
- 回收后请求重新排队，不丢失

**OneRouter 借鉴**: 当后端过载时，Scheduler 可主动取消低优先级分配，释放 GPU 资源给高优先级请求。

### 3.4 动态保守度

**SGLang**: `new_token_ratio` 根据实际 decode 进展动态调整:
- 无 retraction → 逐渐降低（更激进）
- 发生 retraction → 提高（更保守）

**OneRouter 借鉴**: MinLoadPolicy 的 `WaitingCoefficient` 可根据实际分配成功率动态调整。

---

## 四、OneRouter 的独有优势

不仅看差距，也要认识到 OneRouter 在某些方面优于 SGLang:

| 优势 | 说明 |
|------|------|
| **全局精确负载均衡** | Scheduler 拥有所有实例的实时负载，不存在 SGLang 多 gateway 间的视角不一致 |
| **BatchSelect** | 一次 heap 操作分配 K 个请求，O(N+K*logN)，SGLang 每请求独立路由 |
| **Step 隔离** | IDLE→SERVING→DRAINING→IDLE 状态机，每轮训练互不影响 |
| **幂等性保证** | allocate/release 双向去重，网络抖动不会导致重复分配/释放 |
| **漂移补偿** | DriftAware 在 metric 采集间隔内补偿分配计数偏差 |
| **Ghost load 清理** | Gateway 崩溃时 Scheduler 自动释放其全部占用 |
| **COW 无锁读** | NodeStateStore atomic.Pointer + generation counter |
| **三层健康架构** | HealthChecker（主动 `/health` 探针）+ MetricsCollector（指标采集）+ CircuitBreaker（被动反馈），三通道协同，`LoadAvailable()` 统一判定 |
| **非流式 Backend Retry** | 自动 2 次重试 + 换实例，retryable error/status (502,503) 自动触发，pooled buffer |
| **流式完成追踪** | streamTracker 检测 `[DONE]` sentinel，区分 done/interrupted/error 三种结束状态，SGLang 无此能力 |
| **多推理端点** | `/v1/chat/completions` + `/v1/completions` + `/generate` 三端点统一处理，复用同一 `handleInferenceRequest` |
| **Token 使用量提取** | 从非流式响应自动提取 prompt/completion token 数，支持 OpenAI `usage` 和 SGLang `meta_info` 两种格式 |
| **日志级别热调** | `PUT /v1/admin/log-level` 动态调整，per-module 级别控制，排查问题不需重启 |
| **全量状态快照** | `/v1/status` 一次调用获取所有实例负载 + gateway 列表 + step 状态 + 分配统计 |

---

## 五、实施建议优先级

```
Phase 1 — 稳定性基础:
  ├─ ✅ P0-1. Circuit Breaker         → 已完成: circuitbreaker/breaker.go + manager.go
  ├─ ⚠️ P0-2. 请求级重试+故障转移      → 非流式已完成（2 次 retry + 换实例），流式不适用
  └─ ❌ P0-3. Rate Limiting / 并发控制 → 仍缺失

Phase 2 — 路由质量（核心竞争力）:
  ├─ ⚠️ P1-5. TTFT / TPOT 指标        → TTFT 已完成 (stream_tracker.go)，TPOT 缺失
  ├─ ❌ P1-3. Token 级负载感知         → 缺失（Collector 已采集 WaitingCount/RunningCount，但 P2C 未用 token 级指标）
  ├─ ❌ P2-1. PrefixHash 策略         → 缺失
  └─ ❌ P1-2. Consistent Hashing      → 缺失

Phase 3 — 可观测（长期运行保障）:
  ├─ ❌ P1-4. OpenTelemetry            → 缺失
  ├─ ❌ P2-5. In-flight age tracking   → 缺失
  └─ ⚠️ 指标分层 + String Interning    → 已有 sync.Map 缓存 per-instance metric handles (GetInstanceMetrics)

Phase 4 — 高级特性:
  ├─ ❌ P1-1. Radix Tree Cache-Aware   → 缺失
  ├─ ❌ P2-2. Manual/Sticky 路由       → 缺失
  ├─ ❌ P2-3. 请求排队                 → 缺失
  └─ ❌ P2-4. K8s Service Discovery    → 缺失
```

---

## 六、API / 协议 / 接口完整对比

### 6.1 HTTP API 对比

#### SGLang sgl-model-gateway（共 60 个 HTTP 端点）

**推理数据面（22 个，需 API Key 认证）**:

| # | 端点 | OneRouter 支持? | 说明 |
| -- | ---- | -------------- | ---- |
| 1 | `POST /generate` | **已支持** | SGLang 原生文本生成接口，OneRouter 通过 `handleInferenceRequest` 统一处理 |
| 2 | `POST /v1/chat/completions` | **已支持** | OpenAI 兼容 chat completions |
| 3 | `POST /v1/completions` | **已支持** | OpenAI 兼容 text completions，OneRouter 通过 `handleInferenceRequest` 统一处理 |
| 4 | `POST /rerank` | 不支持 | Rerank 重排序接口 |
| 5 | `POST /v1/rerank` | 不支持 | Rerank v1 路径 |
| 6 | `POST /v1/responses` | 不支持 | OpenAI Responses API（新一代对话接口） |
| 7 | `GET /v1/responses/{id}` | 不支持 | 查询存储的 response |
| 8 | `POST /v1/responses/{id}/cancel` | 不支持 | 取消进行中的 response |
| 9 | `DELETE /v1/responses/{id}` | 不支持 | 删除 response |
| 10 | `GET /v1/responses/{id}/input_items` | 不支持 | 查询 response 输入项 |
| 11-18 | `POST/GET/DELETE /v1/conversations/*` | 不支持 | Conversation CRUD + items 管理（8 个端点） |
| 19 | `POST /v1/tokenize` | 不支持 | 文本 → token IDs |
| 20 | `POST /v1/detokenize` | 不支持 | token IDs → 文本 |
| 21 | `POST /v1/embeddings` | 不支持 | OpenAI 兼容 embeddings |
| 22 | `POST /v1/classify` | 不支持 | 分类接口 |

**公开端点（8 个，无需认证）**:

| # | 端点 | OneRouter 支持? | 说明 |
| -- | ---- | -------------- | ---- |
| 23 | `GET /liveness` | 部分（`/healthz`） | 存活探针 |
| 24 | `GET /readiness` | 部分（`/readyz`） | 就绪探针，SGLang 返回 worker 数量统计 |
| 25 | `GET /health` | **已支持**（V2 compat） | 简单健康检查 |
| 26 | `GET /health_generate` | 不支持 | 深度健康检查（检查 worker 能否实际生成） |
| 27 | `GET /engine_metrics` | 不支持 | 聚合所有 worker 的引擎指标 |
| 28 | `GET /v1/models` | 不支持 | OpenAI 兼容 list models |
| 29 | `GET /get_model_info` | 不支持 | 获取模型详细信息 |
| 30 | `GET /get_server_info` | 不支持 | 获取服务器信息 |

**管理端点（12 个，需控制面认证）**:

| # | 端点 | OneRouter 支持? | 说明 |
| -- | ---- | -------------- | ---- |
| 31 | `POST /flush_cache` | 不支持 | 清空所有 worker 的 KV cache |
| 32 | `GET /get_loads` | 部分（`/v1/status`） | 获取所有 worker 负载 |
| 33 | `POST /parse/function_call` | 不支持 | 解析模型输出中的 tool call |
| 34 | `POST /parse/reasoning` | 不支持 | 分离推理文本和正常文本 |
| 35-37 | `POST/DELETE/GET /wasm` | 不支持 | WASM 中间件管理 |
| 38-42 | `POST/GET/DELETE /v1/tokenizers/*` | 不支持 | Tokenizer 管理（注册/列表/删除/状态） |

**Worker 管理（5 个）**:

| # | 端点 | OneRouter 支持? | 说明 |
| -- | ---- | -------------- | ---- |
| 43 | `POST /workers` | 部分（`POST /v1/instances`） | 注册 worker |
| 44 | `GET /workers` | 部分（`GET /v1/instances`） | 列出 worker |
| 45 | `GET /workers/{id}` | 不支持 | 查询单个 worker |
| 46 | `PUT /workers/{id}` | 不支持 | 更新单个 worker |
| 47 | `DELETE /workers/{id}` | 部分（`DELETE /v1/instances`） | 删除 worker（OneRouter 用批量删除） |

**Mesh / HA 管理（12 个）**:

| # | 端点 | OneRouter 支持? | 说明 |
| -- | ---- | -------------- | ---- |
| 48-59 | `/ha/*` | 全部不支持 | 集群状态/健康/worker 状态/策略状态/配置/限流/优雅关闭 |

#### OneRouter 独有端点（SGLang 不具备）

| 端点 | 说明 | 使用场景 |
| ---- | ---- | ------- |
| `POST /v1/steps/start` | 开始训练 step | RL 训练场景：每轮 rollout 开始时调用 |
| `POST /v1/steps/end` | 结束训练 step | RL 训练场景：每轮 rollout 结束时调用 |
| `GET /v1/steps/current` | 查询当前 step | 运维排查 |
| `GET /v1/status` | 全量运行时状态 | 运维排查：包含所有实例负载、gateway 列表 |
| `PUT /v1/admin/log-level` | 动态调整日志级别 | 在线排查：不重启改日志级别 |
| `POST /api/v2/start_infer` | V2 兼容开始推理 | 兼容 rollout-controller |
| `POST /api/v2/stop_infer` | V2 兼容停止推理 | 兼容 rollout-controller |
| `POST /api/v2/session_finish` | V2 会话结束通知 | SessionAware 策略清理会话 |
| `POST /api/v2/chat/completions` | V2 chat（ACK 模式 + 直连路由） | 兼容 PaddleRL/rollout-controller |
| `POST /v1/internal/step-state` | 内部 step 状态推送 | Scheduler→Gateway 状态同步 |

### 6.2 gRPC 服务对比

| 维度 | SGLang | OneRouter |
| ---- | ------ | --------- |
| **gRPC 角色** | 仅作为 **客户端**连接后端 worker | 作为 **服务端**提供调度 API |
| **暴露 gRPC 服务** | 无（纯 HTTP 对外） | `SchedulerService`（4 个 RPC） |
| **gRPC Client** | 连接 SGLang/vLLM 后端（generate, embed, health_check, get_model_info） | 无（Gateway 用 HTTP 连后端） |
| **Proto 定义** | 外部 crate `smg-grpc-client` | `api/proto/router.proto` |
| **连接模式** | HTTP 或 gRPC 可选（per-worker） | Gateway→Scheduler 强制 gRPC |

**OneRouter gRPC 服务（4 个 RPC）**:

| RPC | 说明 | 使用场景 |
| --- | ---- | ------- |
| `Allocate` | 请求分配后端实例 | Gateway 每个推理请求前调用 |
| `Release` | 释放后端实例占用 | Gateway 推理请求完成后调用 |
| `Register` | Gateway 注册到 Scheduler | Gateway 启动时调用 |
| `Heartbeat` | Gateway 心跳 + 状态同步 | 周期性调用，附带 step 状态同步 |

**SGLang gRPC Client（4 个 RPC，连接后端）**:

| RPC | 说明 | OneRouter 对应 |
| --- | ---- | ------------- |
| `generate()` | 双向流式生成 | 不支持。OneRouter 用 HTTP 反向代理 |
| `embed()` | 嵌入向量 | 不支持 |
| `health_check()` | 后端健康检查 | Collector HTTP 采集（不用 gRPC） |
| `get_model_info()` | 获取模型信息 | 不支持 |

### 6.3 协议支持对比

| 协议 | SGLang | OneRouter | 说明 |
| ---- | ------ | --------- | ---- |
| **HTTP/1.1** | 支持 | **支持** | 主要数据面协议 |
| **HTTP/2 (h2c)** | 不支持（Axum 默认 HTTP/1.1） | **支持** | `H2CReverseProxy` 选项，多路复用 |
| **gRPC** | 客户端（连后端） | 服务端（调度 API） | 角色不同 |
| **SSE** | 支持 | **支持** | 流式推理响应 |
| **WebSocket** | 不支持 | 不支持 | 两者均不支持 |
| **MCP** | 支持（tool calling） | 不支持 | Model Context Protocol |
| **WASM 中间件** | 支持 | 不支持 | 请求/响应转换插件 |

### 6.4 认证对比

| 认证方式 | SGLang | OneRouter |
| ------- | ------ | --------- |
| API Key（Bearer Token） | 数据面 + 控制面 | 不支持 |
| JWT + JWKS | 控制面可选 | 不支持 |
| 常量时间比较（防时序攻击） | 支持（`subtle::ConstantTimeEq`） | 不支持 |
| 无认证 | 支持 | **当前仅此模式** |

### 6.5 缺失 API 的使用场景和优先级

| 缺失 API | 使用场景 | 触发条件 | 建议优先级 |
| -------- | ------- | ------- | --------- |
| ~~`POST /v1/completions`~~ | ~~text completion~~ | ~~已支持~~ | ✅ 已实现 |
| `GET /v1/models` | 客户端发现可用模型，兼容 OpenAI SDK | 接入 OpenAI 兼容客户端时 | P1（兼容性关键） |
| `POST /v1/embeddings` | 向量检索、RAG 场景 | 部署 embedding 模型时 | P2（非 RL 训练核心场景） |
| `POST /v1/tokenize` / `detokenize` | token 计数、prompt 截断 | 客户端需要精确 token 控制时 | P3 |
| `POST /flush_cache` | 清空后端 KV cache | 模型切换/OOM 恢复时 | P1（运维必备） |
| `GET /health_generate` | 深度健康检查 | K8s readiness 需要验证实际生成能力 | P2 |
| `POST /v1/responses` | OpenAI Responses API | 接入新版 OpenAI SDK 时 | P3（新 API，生态尚未成熟） |
| API Key 认证 | 生产环境安全 | 多租户/公网暴露时 | P1 |

---

## 七、Prefill-Decode (PD) 分离功能对比

### 7.1 什么是 PD 分离

推理过程分为两个阶段：
1. **Prefill**：处理输入 prompt，计算 KV cache（计算密集，GPU 利用率高）
2. **Decode**：基于 KV cache 逐 token 生成（内存密集，GPU 利用率低）

PD 分离将两个阶段部署到不同的 GPU 节点上，通过 RDMA/NVLink 传输 KV cache，实现：
- Prefill 节点可以使用更少但更强的 GPU
- Decode 节点可以使用更多但内存更大的 GPU
- 两个阶段独立扩缩容，提升整体吞吐

### 7.2 SGLang PD 实现

**Gateway 层（Rust sgl-model-gateway）**:

```text
请求到达
  │
  ├─ 选择 Prefill Worker（独立策略，如 session_aware）
  ├─ 选择 Decode Worker（独立策略，如 power_of_two）
  │
  ├─ 注入 bootstrap 信息到请求 JSON:
  │   {
  │     ...原始请求...,
  │     "bootstrap_host": "prefill-worker-1.example.com",
  │     "bootstrap_port": 9001,
  │     "bootstrap_room": 7382947291038475  // 随机 room ID
  │   }
  │
  ├─ tokio::join! 并发发送到两个 worker:
  │   ├─ Prefill Worker: 处理 prompt → 计算 KV cache → 通过 bootstrap 传输给 Decode
  │   └─ Decode Worker: 接收 KV cache → 开始 token 生成 → 流式返回
  │
  └─ 返回 Decode Worker 的响应（可选合并 Prefill 的 logprobs）
```

核心组件:
- **Worker 类型**: `WorkerType::Prefill { bootstrap_port }` 和 `WorkerType::Decode`
- **独立策略**: Prefill 和 Decode 各自独立的 `LoadBalancingPolicy`
- **PDRouter**: `routers/http/pd_router.rs` — 双路分发 + bootstrap 注入 + logprob 合并
- **K8s 发现**: 通过 label selector 区分 prefill/decode Pod，annotation 携带 bootstrap port
- **KV 传输后端**: Mooncake（RDMA）、Mori、NIXL、Ascend、Fake（测试）

配置示例:
```bash
smg launch --pd-disaggregation \
  --prefill http://prefill-1:30001 9001 \
  --prefill http://prefill-2:30002 9002 \
  --decode http://decode-1:30003 \
  --decode http://decode-2:30004 \
  --prefill-policy session_aware \
  --decode-policy power_of_two
```

### 7.3 OneRouter 现状

**OneRouter 完全不支持 PD 分离**。原因和差距：

| 维度 | SGLang | OneRouter |
| ---- | ------ | --------- |
| Worker 类型区分 | Regular/Prefill/Decode 三种 | 仅一种（无类型区分） |
| 独立策略池 | Prefill 和 Decode 各自策略 | 所有实例共享一个策略 |
| Bootstrap 注入 | 自动注入 host/port/room 到请求 | 无 |
| 双路并发分发 | tokio::join! 同时发两个 worker | 仅单路转发 |
| KV cache 传输 | RDMA/NVLink 多后端 | 无 |
| K8s Pod 分类 | label selector 区分角色 | 无 K8s 集成 |

### 7.4 是否需要支持 PD 分离

**短期（P3）**: 不需要。理由：
- OneRouter 的核心场景是 RL 训练的 rollout，后端通常是同构的 SGLang/vLLM 实例
- PD 分离是推理服务优化，RL 训练场景中后端已经自行处理 PD（SGLang 内部的 disagg scheduler）
- PD 分离需要 RDMA 基础设施，部署复杂度高

**长期（如果 OneRouter 扩展为通用推理网关）**: 值得考虑。需要：
1. `Instance` 增加 `WorkerType` 字段（Prefill/Decode/Regular）
2. `NodeStateStore` 支持按 WorkerType 过滤
3. Policy 层支持双策略池
4. Gateway proxy 支持双路并发转发 + bootstrap 注入
5. Collector 支持按 WorkerType 分别采集

---

## 八、架构选型分析：去中心化 vs 中心化

### 8.1 核心问题

SGLang 采用去中心化 Gateway（每个实例独立路由），OneRouter 采用中心化 Scheduler + 分布式 Gateway。
这不是一个"谁更好"的问题，而是**场景决定架构**。

### 8.2 去中心化（SGLang）的优劣

**优势**:

| 优势 | 说明 |
| ---- | ---- |
| 无单点故障 | 任何一个 gateway 挂了，其他 gateway 不受影响 |
| 线性水平扩展 | 加 gateway 即加吞吐，无中心瓶颈 |
| 延迟更低 | 请求本地决策，省去一次 gateway→scheduler 的 RPC 往返（~1-5ms） |
| 部署简单 | 无需维护额外的 scheduler 进程，运维成本低 |
| 适合超大规模在线推理 | 10w+ QPS 场景下，中心化 scheduler 本身成为瓶颈 |

**代价**:

| 代价 | 说明 |
| ---- | ---- |
| 负载均衡不精确 | 每个 gateway 只有本地视角，多个 gateway 可能同时把请求路由到同一个"看起来最空闲"的实例 |
| 状态一致性弱 | 各 gateway 间通过 gossip/轮询同步，存在窗口期内的视角不一致 |
| 无法做全局批量调度 | 每个请求独立路由，无法像 BatchSelect 那样一次性最优分配 K 个请求 |
| Step 隔离困难 | 没有中心节点协调"这一轮训练开始/结束"，各 gateway 难以同步状态 |

### 8.3 中心化（OneRouter）的优劣

**优势**:

| 优势 | 说明 |
| ---- | ---- |
| 全局精确均衡 | Scheduler 拥有所有实例的实时负载，分配决策基于全局真实状态 |
| BatchSelect | 一次 heap 操作分配 K 个请求，避免多个 gateway 竞争同一实例 |
| Step 隔离天然支持 | 中心节点控制 IDLE→SERVING→DRAINING 状态机，每轮训练互不干扰 |
| 幂等性容易保证 | allocate/release 在单点串行化，去重逻辑简单可靠 |
| Ghost load 可清理 | Gateway 崩溃时 Scheduler 知道该 gateway 持有哪些分配，可全量释放 |
| 策略切换全局生效 | 换调度策略只改 Scheduler 一处，所有 gateway 立即生效 |

**代价**:

| 代价 | 说明 |
| ---- | ---- |
| Scheduler 是单点 | 挂了就全挂。必须做 HA（主备/选举）才能用于生产 |
| 吞吐天花板 | event-loop 串行化，单 Scheduler 的 QPS 上限受限于单机性能 |
| 多一跳延迟 | 每个请求 gateway→scheduler (Allocate) → gateway→backend，多 1 次 RPC |
| 部署复杂度 | 多一个有状态进程要运维 |

### 8.4 场景适配矩阵

```text
                        ─────────────────          ──────────────────
在线推理服务 (高 QPS)     ⚠️ 可部署多实例但路由退化   ❌ Scheduler 成瓶颈
RL 训练 rollout          ❌ Step 隔离困难           ✅ 最优
GPU 利用率极致优化        ⚠️ 近似均衡               ✅ 全局精确
万卡规模 (10w QPS)       ⚠️ 多实例负载感知退化      ⚠️ 需要分片/HA
中小规模 (<1000 实例)     ✅ 简单够用               ✅ 精确但略重
长期运行稳定性           ✅ 天然高可用              ⚠️ 依赖 HA 方案
```

### 8.5 OneRouter 选择中心化的合理性

OneRouter 选择中心化是**正确的**，核心理由：

1. **RL 训练场景是核心**: rollout 需要 Step 隔离、全局状态控制、每轮开始/结束的协调——这些在去中心化架构下极难实现
2. **QPS 不高但要求极致均衡**: GPU 推理请求 QPS 通常 100~10000 级别，远未到中心化的瓶颈，但每个请求的 GPU 成本极高，负载不均 = 浪费 GPU
3. **BatchSelect 是杀手锏**: RL 训练中同一 step 的大量请求几乎同时到达，批量分配比逐个路由更优

### 8.6 中心化架构需补齐的短板

| 短板 | 当前状态 | 建议 |
| ---- | ------- | ---- |
| Scheduler 单点 | 无 HA | **P0**: 实现 checkpoint + 快速恢复；长期做主备 |
| 吞吐上限 | event-loop 131072 buffer | 当前够用（万卡 ~10w QPS），超大规模时考虑分片 Scheduler |
| 多一跳延迟 | Allocate RPC ~1-5ms | 对推理请求（秒级~分钟级）可忽略 |
| Circuit Breaker | ✅ 已实现 | `circuitbreaker.Manager` + `NodeState.CircuitOpen` + `LoadAvailable()` |

### 8.7 SGLang 真的能水平扩展吗？—— 源码级证伪

之前的对比文档中说 SGLang「线性水平扩展」是过于乐观的结论。
通过源码分析，SGLang **可以部署多实例**，但绝大多数策略在多实例下路由质量严重退化。

#### 证据 1：负载计数是进程本地 AtomicUsize

`sgl-model-gateway/src/core/worker.rs:620-622`:

```rust
pub struct BasicWorker {
    pub load_counter: Arc<AtomicUsize>,  // 进程内 atomic，不跨实例
}
fn increment_load(&self) {
    self.load_counter.fetch_add(1, Ordering::Relaxed);  // L759
}
```

每个 gateway 只能看到**自己转发的请求数**。2 个 gateway 各看到 worker-A 负载为 5，实际 worker-A 负载是 10。
所有基于 `worker.load()` 的策略（P2C、ShortestQueue、SessionAware 的 imbalance 检测）全部失准。

#### 证据 2：P2C 的 token 级负载未跨 gateway 聚合

`sgl-model-gateway/src/policies/power_of_two.rs:21-22`:

```rust
cached_loads: RwLock<HashMap<String, isize>>,  // 每个进程独立缓存
```

#### 证据 3：Radix Tree 是进程内 DashMap

`sgl-model-gateway/src/policies/session_aware.rs:84-86`:

```rust
pub struct SessionAwarePolicy {
    trees: Arc<DashMap<String, Arc<Tree>>>,  // 进程内
    mesh_sync: OptionalMeshSyncManager,      // 可选 mesh 同步
}
```

gateway-A 看到 "Hello world" 路由到 worker-1 并插入 tree，gateway-B 完全不知道。
两个 gateway 的 tree 状态逐渐分裂，cache-aware 命中率从理论 ~80% 降到 ~50%。

#### 证据 4：Circuit Breaker 是进程本地 Atomic

`sgl-model-gateway/src/core/circuit_breaker.rs:103-106`:

```rust
pub struct CircuitBreaker {
    state: AtomicU8,                  // 进程内
    consecutive_failures: AtomicU32,  // 进程内
}
```

worker-X 挂了，gateway-A 探到 5 次失败触发 Open，gateway-B 还是 Closed——继续往坏节点发请求。

#### 证据 5：Token Bucket 是进程本地

`sgl-model-gateway/src/core/token_bucket.rs:20-22`:

```rust
pub struct TokenBucket {
    inner: Arc<Mutex<TokenBucketInner>>,  // 进程内 Mutex
}
```

配置 `max_concurrent=256`，两个 gateway 各允许 256，后端实际承受 512 并发。

#### 证据 6：smg-mesh 框架设计精良但 model_gateway 桥接未完成

SGLang 的 mesh 层（`smg-mesh` crate）实际上是一个**设计精良的分布式一致性框架**，
包含 CRDT OR-Map、Gossip 协议、分片 Rate Limit、冷启动状态机、网络分区检测。
源码位于 `/smg/crates/mesh/src/`。

**smg-mesh 核心架构**:

```text
Gateway-1                    Gateway-2                    Gateway-3
┌──────────────┐             ┌──────────────┐             ┌──────────────┐
│ WorkerStore  │←── gossip ──│ WorkerStore  │←── gossip ──│ WorkerStore  │
│ PolicyStore  │  sync_stream│ PolicyStore  │  sync_stream│ PolicyStore  │
│ AppStore     │  (gRPC双向流)│ AppStore     │             │ AppStore     │
│ RateLimitStore│            │ RateLimitStore│            │ RateLimitStore│
│ MembershipStore│           │ MembershipStore│           │ MembershipStore│
└──────┬───────┘             └──────┬───────┘             └──────┬───────┘
       │                            │                            │
       └────── CRDT OR-Map ─────────┴────────────────────────────┘
               (LWW + Tombstone + Lamport Clock)
```

**CRDT OR-Map** (`crdt_kv/crdt.rs`):

- Observed-Remove Map，每个 key 维护版本列表 `Vec<ValueMetadata>`
- LWW 语义：`(timestamp, replica_id)` 排序，大的赢
- Lamport Clock 保证因果一致，merge 时只 apply 未见过的 operation
- Per-key lock（`DashMap<String, Arc<Mutex<()>>>`），不阻塞全局

**5 类 State Store** (`stores.rs`):

| Store | 用途 | 同步方式 |
| ----- | ---- | ------- |
| MembershipStore | 节点成员关系 | Gossip Ping + StateSync |
| WorkerStore | Worker 状态 (health, **load**, url) | CRDT OR-Map + 版本号 LWW |
| PolicyStore | 路由策略配置 | CRDT OR-Map + 版本号 LWW |
| AppStore | 应用配置 | CRDT OR-Map + 版本号 LWW |
| RateLimitStore | 限流计数器 | CRDT + Consistent Hash 分片 |

**Gossip + Sync Stream** (`controller.rs`):

- 每秒随机选一个 peer，Ping + StateSync
- SWIM 故障检测：直接 Ping 失败 → 委托 3 个随机节点 PingReq → 仍失败 → Suspected → Down
- gRPC 双向流（`sync_stream`），建立后每秒发送 incremental update
- 连接去重：字典序 `self_name < peer_name` 时才主动发起

**全局 Rate Limit** (`stores.rs:426-667`):

- 每个 node 维护自己的 shard (`"global::actor:node1"`)
- 查询时聚合所有 shard (`aggregate_counter`)
- Consistent Hash Ring 决定 key 的 owner，故障时 ownership transfer

**冷启动状态机** (`node_state_machine.rs`):

```text
NotReady → Joining → SnapshotPull(60s timeout) → Converging(5次稳定+10s) → Ready
```

**网络分区检测** (`partition.rs`):

- `Normal / PartitionedWithQuorum / PartitionedWithoutQuorum`
- Quorum 判断：`reachable_count >= quorum_threshold`

**关键发现——load 桥接未完成**:

`WorkerStore` 设计了 `load: f64` 字段，理论上可通过 CRDT 同步到所有节点。
但 model_gateway 在两处调用中硬编码为 `0.0`:

`model_gateway/src/core/worker_registry.rs:379-385`:

```rust
mesh_sync.sync_worker_state(
    worker_id..., model_id..., url...,
    worker.is_healthy(),
    0.0, // TODO: Get actual load    ← 注册时 load=0.0
);
```

`model_gateway/src/core/worker_registry.rs:497-503`:

```rust
mesh_sync.sync_worker_state(
    worker_id..., model_id..., url...,
    is_healthy,
    0.0, // TODO: Get actual load    ← 健康状态更新时 load=0.0
);
```

**smg-mesh 各能力完成度**:

| 能力 | 框架层 (smg-mesh) | 应用层 (model_gateway) | 端到端生效? |
| ---- | ---------------- | --------------------- | ---------- |
| Worker 健康同步 | CRDT store 完善 | `is_healthy` 已桥接 | **是** |
| Worker 负载同步 | `load: f64` 字段已有 | `0.0 // TODO` 未桥接 | **否** |
| Tree 操作同步 | TreeState subscriber | insert/remove 已桥接 | **是**（有延迟） |
| 全局 Rate Limit | 分片 CRDT + ConsistentHash | middleware 已集成 | **是** |
| 冷启动快照 | SnapshotPull + Convergence | 已集成 | **是** |
| 分区检测 | PartitionDetector | 已集成 | **是** |
| 负载均衡决策 | — | `BasicWorker.load_counter` 仍为进程本地 | **否** |

#### 证据 7：唯一能多实例正确工作的策略是 Consistent Hashing

`sgl-model-gateway/src/core/worker_registry.rs:43-48`:

```rust
pub struct HashRing {
    entries: Arc<[(u64, Arc<str>)]>,  // 确定性：相同 worker 列表 → 相同 ring
}
```

Consistent Hashing 不依赖运行时状态，只依赖 worker 列表。多 gateway 只要 worker 列表一致，路由结果就一致。

#### 多实例部署退化矩阵

| 维度 | 单实例 | 多实例（无 mesh） | 多实例（有 mesh） | 说明 |
| ---- | ------ | --------------- | --------------- | ---- |
| 负载计数 | 精确 | 每 gateway 只见自己的 | **框架支持但未桥接** (`0.0 // TODO`) | WorkerStore 有 `load: f64`，但 model_gateway 写死 0.0 |
| Worker 健康 | 精确 | 各自独立判断 | **已通过 mesh 同步** | `is_healthy` 已正确桥接到 CRDT store |
| Radix Tree | 完整 | 分裂，cache 命中率下降 | **gossip 同步**（~1s 延迟） | TreeStateSubscriber 已桥接 insert/remove 事件 |
| Circuit Breaker | 正确 | 各自独立判断 | 未同步（进程本地 Atomic） | 框架层无 CB store，需应用层自行同步 |
| Rate Limit | 精确 | 总量 = N × 配置 | **mesh 全局限流** | CRDT 分片 + ConsistentHash 聚合，已完整集成 |
| Consistent Hash | 确定性 | 确定性 | 确定性 | 不依赖运行时状态，天然多实例安全 |
| P2C | 精确 | 各自近似 | **不精确**（依赖 load，未桥接） | `cached_loads` 仍为进程本地 RwLock |

#### 对 OneRouter 的启示

**smg-mesh 的设计质量值得尊重**——它是一套完整的 CRDT + Gossip 分布式一致性框架，
具备冷启动收敛、分区检测、全局限流等生产级能力。问题不在框架本身，而在应用层桥接：

1. **负载同步的 `0.0 // TODO` 说明这是一个工程优先级问题，而非架构缺陷**。
   smg-mesh 的 `WorkerStore.load` 字段已经预留了 CRDT 同步通道，
   一旦 model_gateway 补上桥接代码，多实例间的负载视图就能收敛（虽然有 gossip 延迟）。

2. **但即使桥接完成，CRDT 最终一致性 ≠ 精确一致性**。
   Gossip 同步有 ~1s 延迟，高并发下多 gateway 看到的负载快照存在时间差。
   对在线推理服务（QPS 高、单请求轻量）这完全可接受；
   对 RL 训练 rollout（QPS 低、单请求重、需要严格步骤隔离），精确负载视图更重要。

3. **OneRouter 的中心化 Scheduler 在 RL 场景是更优解**——
   不是因为去中心化做不到，而是因为全局 event-loop 天然提供了精确一致性，
   省去了 CRDT 收敛延迟和最终一致性的复杂性。代价是单点风险，需通过 HA 解决。

4. **值得借鉴的设计**:
   - smg-mesh 的冷启动状态机（SnapshotPull → Converging → Ready）可用于 OneRouter Gateway 的降级模式
   - 全局 Rate Limit 的分片 + 聚合模式可用于 OneRouter 的多 Scheduler HA 方案
   - CRDT OR-Map 的 operation log 设计可用于 Scheduler 状态恢复（checkpoint + replay）

### 8.8 结论

> **两种架构各有最优适用场景，不存在绝对优劣。**
>
> **SGLang 去中心化 + smg-mesh**：框架层设计精良（CRDT OR-Map、Gossip、分区检测、全局限流），
> 适合在线推理服务的水平扩展。当前短板是 model_gateway 的负载桥接未完成（`0.0 // TODO`），
> 这是工程优先级问题而非架构缺陷——一旦补上，多 gateway 间可实现最终一致的负载视图。
> 但 CRDT 最终一致性天然有 gossip 延迟（~1s），在高精度负载均衡场景存在收敛窗口。
>
> **OneRouter 中心化 Scheduler**：通过全局 event-loop 提供精确一致的负载视图，
> 天然适合 RL 训练 rollout 场景（QPS 低、单请求重、需严格步骤隔离）。
> 代价是 Scheduler 单点风险，需通过 HA（主备 + checkpoint）解决。
>
> **推荐策略**：OneRouter 维持中心化核心架构，同时借鉴 smg-mesh 的设计实现降级模式——
> Scheduler 正常时走精确分配，Scheduler 不可用时 Gateway 本地降级为 Consistent Hashing 等无状态策略。
> 这兼顾了精确性和可用性。

---

## 九、源码参考路径

### SGLang sgl-model-gateway (Rust)

| 组件 | 路径 |
| ---- | ---- |
| Rust 主入口 | `SGLang/sgl-model-gateway/src/main.rs` |
| HTTP Server | `SGLang/sgl-model-gateway/src/server.rs` |
| 策略 trait | `SGLang/sgl-model-gateway/src/policies/mod.rs` |
| Cache-Aware | `SGLang/sgl-model-gateway/src/policies/session_aware.rs` |
| Radix Tree | `SGLang/sgl-model-gateway/src/policies/tree.rs` |
| PrefixHash | `SGLang/sgl-model-gateway/src/policies/prefix_hash.rs` |
| ConsistentHashing | `SGLang/sgl-model-gateway/src/policies/consistent_hashing.rs` |
| PowerOfTwo | `SGLang/sgl-model-gateway/src/policies/power_of_two.rs` |
| Manual | `SGLang/sgl-model-gateway/src/policies/manual.rs` |
| Bucket | `SGLang/sgl-model-gateway/src/policies/bucket.rs` |
| Circuit Breaker | `SGLang/sgl-model-gateway/src/core/circuit_breaker.rs` |
| Token Bucket | `SGLang/sgl-model-gateway/src/core/token_bucket.rs` |
| Retry | `SGLang/sgl-model-gateway/src/core/retry.rs` |
| Worker 抽象 | `SGLang/sgl-model-gateway/src/core/worker.rs` |
| Worker Registry | `SGLang/sgl-model-gateway/src/core/worker_registry.rs` |
| Middleware | `SGLang/sgl-model-gateway/src/middleware.rs` |
| Metrics | `SGLang/sgl-model-gateway/src/observability/metrics.rs` |
| InFlight Tracker | `SGLang/sgl-model-gateway/src/observability/inflight_tracker.rs` |
| OTel Trace | `SGLang/sgl-model-gateway/src/observability/otel_trace.rs` |
| Service Discovery | `SGLang/sgl-model-gateway/src/service_discovery.rs` |
| PD Router (HTTP) | `SGLang/sgl-model-gateway/src/routers/http/pd_router.rs` |
| PD Types | `SGLang/sgl-model-gateway/src/routers/http/pd_types.rs` |
| PD Router (gRPC) | `SGLang/sgl-model-gateway/src/routers/grpc/pd_router.rs` |
| Router Manager | `SGLang/sgl-model-gateway/src/routers/router_manager.rs` |
| OpenAI Router | `SGLang/sgl-model-gateway/src/routers/openai/router.rs` |
| Conversations | `SGLang/sgl-model-gateway/src/routers/conversations/handlers.rs` |
| Tokenize | `SGLang/sgl-model-gateway/src/routers/tokenize/handlers.rs` |
| WASM Route | `SGLang/sgl-model-gateway/src/wasm/route.rs` |
| Mesh HA | `SGLang/sgl-model-gateway/src/routers/mesh/handlers.rs` |
| Python Router Args | `SGLang/sgl-model-gateway/bindings/python/src/sglang_router/router_args.py` |
| Python DP Controller | `SGLang/python/sglang/srt/managers/data_parallel_controller.py` |
| Python Schedule Policy | `SGLang/python/sglang/srt/managers/schedule_policy.py` |
| Python RadixCache | `SGLang/python/sglang/srt/mem_cache/radix_cache.py` |
| Python PD Prefill | `SGLang/python/sglang/srt/disaggregation/prefill.py` |
| Python PD Decode | `SGLang/python/sglang/srt/disaggregation/decode.py` |
| Python PD Utils | `SGLang/python/sglang/srt/disaggregation/utils.py` |

### OneRouter 新增组件路径

| 组件 | 路径 |
| ---- | ---- |
| Circuit Breaker | `internal/scheduler/circuitbreaker/breaker.go` |
| Circuit Breaker Manager | `internal/scheduler/circuitbreaker/manager.go` |
| Health Checker | `internal/scheduler/healthcheck/checker.go` |
| Non-stream Forward + Retry | `internal/gateway/forward_nonstream.go` |
| Stream Tracker (TTFT + [DONE]) | `internal/gateway/stream_tracker.go` |
| Stream Detection (零分配) | `internal/gateway/stream_detect.go` |
| Token Usage Extraction | `internal/gateway/usage.go` |
| Panic Recovery Middleware | `pkg/logger/recovery.go` |
| gRPC Interceptors | `pkg/logger/grpc_interceptor.go` |
| Access Log Middleware | `pkg/logger/middleware.go` |
| Context Logger | `pkg/logger/ctxlogger.go` |
| Prometheus Metrics (28 项) | `pkg/metrics/metrics.go` |
| Config (CB + HC + Collector) | `internal/config/config.go` |
| V2 Chat (ACK 模式) | `internal/gateway/v2_chat.go` |

### smg-mesh (Rust CRDT + Gossip 框架)

| 组件 | 路径 |
| ---- | ---- |
| Mesh 库入口 | `smg/crates/mesh/src/lib.rs` |
| CRDT OR-Map | `smg/crates/mesh/src/crdt_kv/crdt.rs` |
| Operation Log | `smg/crates/mesh/src/crdt_kv/operation.rs` |
| KV Store | `smg/crates/mesh/src/crdt_kv/kv_store.rs` |
| Replica / Lamport Clock | `smg/crates/mesh/src/crdt_kv/replica.rs` |
| 5 State Stores | `smg/crates/mesh/src/stores.rs` |
| Gossip Controller | `smg/crates/mesh/src/controller.rs` |
| Sync Manager | `smg/crates/mesh/src/sync.rs` |
| 冷启动状态机 | `smg/crates/mesh/src/node_state_machine.rs` |
| 网络分区检测 | `smg/crates/mesh/src/partition.rs` |
| Worker Registry (桥接层) | `smg/model_gateway/src/core/worker_registry.rs` |

---

## 十、问题排查能力对比

### 10.1 排查工具矩阵

| 维度 | SGLang SMG | OneRouter | 评价 |
|------|-----------|-----------|------|
| **日志系统** | 可配 debug/info/warn/error + file sink，request-id 透传，隐私保护（默认不记请求内容） | 双 logger (Root 同步 + Access 异步采样)，named sub-loggers，lumberjack 轮转（size/age/压缩），慢请求 watchdog，结构化字段 (event/status/reason) | **OneRouter 更强**: 分层日志 + 采样控制 + 慢请求检测 |
| **日志级别热调** | 不支持 runtime 调整（需重启） | `PUT /v1/admin/log-level` 动态调整，per-module 级别控制 | **OneRouter 独有** |
| **分布式追踪** | OpenTelemetry OTLP/gRPC export，W3C Trace Context 传播，Batch span 处理 | 仅 `X-Trace-ID` header 透传，无 span 上报 | **SGLang 远强**: 端到端 trace 是万卡场景的必需品 |
| **请求卡住检测** | `inflight_request_age` histogram (30s~24h 桶)，RAII guard 自动清理 | 无 inflight age tracking | **SGLang 独有**: 半年运行中发现"卡住"请求的关键手段 |
| **健康诊断** | `/health` + `/health_generate` (深度验证：实际发送生成请求) + `/readiness` + `/liveness` | `/healthz` + `/readyz` (组件级：step phase + scheduler conn) + `/v1/status` (全量运行时状态) | **各有优势**: SGLang 有深度生成验证，OneRouter 有全量状态快照 |
| **运行时状态查看** | `/get_loads` (worker 负载) + `/workers` (worker 详情) + `/workers/{id}` (单个 worker) | `/v1/status` (所有实例负载 + gateway 列表 + step 状态 + 分配统计，一个端点全量) | **OneRouter 更全面**: 单次调用获取完整调度上下文 |
| **指标覆盖** | 40+ Prometheus 指标，6 层分类 (HTTP/Router/Inference/Worker/CB/Retry/Discovery/MCP/DB) | 28 Prometheus 指标，7 类 (请求/调度/幂等性/EventLoop/代理/稳定性/Collector/HC/CB/Token/SLO) | **SGLang 更多**: 但 OneRouter 有 SGLang 没有的幂等性/event-loop/流式完成指标 |
| **Crash 诊断** | `--crash-dump-folder` (崩溃前 5min 请求 dump + replay) | 无 crash dump（Scheduler 全内存，崩溃 = 全丢） | **SGLang 独有**: 推理引擎级别的 crash dump |
| **Panic Recovery** | Rust safe（编译时保证无 panic，除非显式 `unwrap`） | HTTP middleware + gRPC interceptor `recover()` + `panic_recoveries_total` 指标 | **对等**: Rust 天然安全，Go 需要 recovery middleware（已实现） |
| **Event Loop 可观测** | 无（去中心化，无 event loop） | `event_channel_depth` (gauge) + `event_loop_batch_size` (histogram) + heartbeat 日志 | **OneRouter 独有**: 调度核心可观测 |

### 10.2 典型故障排查场景对比

| 故障场景 | SGLang 排查手段 | OneRouter 排查手段 | 差距 |
|---------|----------------|-------------------|------|
| **请求延迟飙升** | `smg_router_ttft_seconds` + `smg_router_tpot_seconds` + OTel trace 定位慢 span | `time_to_first_byte_ms` + `request_duration_ms` + access log 慢请求标记 | OneRouter 缺 TPOT 和分布式 trace |
| **后端实例宕机** | health check 探测 + CB 自动熔断 + `smg_worker_cb_state` + OTel trace | HealthChecker 探测 + CB 熔断 + Collector unhealthy 标记 + `circuit_breaker_trips_total` | **对齐**: 双通道检测（主动+被动） |
| **请求卡住不返回** | `inflight_request_age_count` (>5min 桶告警) + request timeout (600s) | access log 慢请求 watchdog + `active_requests` gauge 异常 | OneRouter 缺 inflight age tracking |
| **负载不均** | `/get_loads` + per-worker `smg_worker_selection_total` | `/v1/status` 全量负载 + `active_requests` per-instance + `event_loop_batch_size` | **OneRouter 更佳**: 全局精确视图 |
| **Gateway 掉线** | mesh gossip 检测 + `smg_discovery_*` | heartbeat 超时 + ghost allocation cleanup + `registered_gateways` gauge | **对等**: 不同机制但都能检测 |
| **流式中断** | 无专门追踪 | `stream_completion_total{result="interrupted"}` + streamTracker | **OneRouter 独有** |

### 10.3 可观测差距总结

**OneRouter 需补齐**:
1. **OpenTelemetry Tracing** (P1): 万卡场景 grep 日志不可行，需端到端 trace
2. **In-flight Request Age Tracking** (P2): 长期运行发现卡住请求的关键手段
3. **TPOT 指标** (P2): 推理 SLO 核心指标，需推理引擎配合上报
4. **告警规则** (P1): 28 指标已有，但无 Alertmanager 规则模板

**OneRouter 独有优势**:
1. 日志级别动态调整（不重启排查问题）
2. Event Loop 可观测（调度核心诊断）
3. 流式完成追踪（done/interrupted/error 三态）
4. 全量状态快照 `/v1/status`（一次调用获取完整上下文）

---

## 十一、SGLang 独有高级特性（OneRouter RL 场景不需要的）

SGLang SMG 作为通用推理网关，包含大量 OneRouter 不需要的功能。
列出这些特性以避免被误认为"差距"——它们是**场景差异**而非**能力缺失**。

| 特性 | SGLang 实现 | OneRouter 不需要的原因 |
|------|------------|---------------------|
| **WASM 中间件** | 沙箱化 WebAssembly 模块（64MB 内存隔离，1s 超时），OnRequest/OnResponse 两个挂载点 | RL 训练场景请求格式固定（chat completions），无需自定义请求/响应转换 |
| **MCP Tool Calling** | Model Context Protocol 支持，tool call 解析/路由 | RL rollout 不涉及 tool calling，纯文本生成 |
| **Conversation/Response CRUD** | 8 个端点管理多轮对话历史，支持 PostgreSQL/Redis/Oracle 存储后端 | RL 训练每轮 rollout 独立，无需持久化对话历史 |
| **Tokenizer 管理** | 动态注册/管理 tokenizer（从 HuggingFace 异步加载），L0/L1 两级缓存 | OneRouter 不做 tokenization，请求透传到后端推理引擎 |
| **OpenAI Router** | 代理到外部 OpenAI/Claude 等 vendor，保持本地对话历史 | RL 训练使用自有 GPU 集群，不走外部 vendor |
| **TLS/mTLS + JWT 认证** | 4 层认证 (None → API Key → mTLS → JWT+JWKS)，rustls 实现 | RL 训练集群在内网，当前无多租户需求 |
| **gRPC Router** | 全 Rust tokenization + reasoning parser + tool parser in-process pipeline | OneRouter 的后端连接用 HTTP 反向代理（简单可靠），gRPC 仅用于 Gateway↔Scheduler |
| **IGW 多模型路由** | RouterManager 按 model_id 分发到不同后端，per-model 独立策略 | RL 训练通常单模型（或同构多副本） |
| **PD 分离路由** | 独立 Prefill/Decode 策略池 + bootstrap 注入 + KV cache 传输后端 (Mooncake/NIXL) | PD 分离由推理引擎内部处理（SGLang disagg scheduler），网关层无需介入 |
| **K8s Service Discovery** | kube-rs watch Pod，label selector 区分角色 | OneRouter 使用 HTTP API 注册，当前部署规模可控；K8s 集成为 P2 待做 |
| **Mesh HA (gossip)** | smg-mesh CRDT 框架，多 gateway 状态同步 | OneRouter 采用中心化 Scheduler（RL 场景更优），HA 通过 checkpoint + 主备实现 |
| **Storage Backends** | Memory/PostgreSQL/Redis/Oracle 多后端 | OneRouter 全内存（event-loop 串行化），RL 场景不需要持久化存储 |
| **Rerank/Embedding/Classify** | `/v1/rerank`, `/v1/embeddings`, `/v1/classify` | RL 训练仅需 text generation（chat completions / completions / generate） |

### 关键结论

> **SGLang SMG 和 OneRouter 的功能差异大部分来自场景定位不同**:
>
> - SGLang SMG = **通用推理网关**，需要支持多租户、多模型、多种推理任务（生成/embedding/rerank）、外部 vendor 代理、持久化对话、插件扩展
> - OneRouter = **RL 训练 rollout 专用路由器**，需要极致负载均衡、Step 隔离、全局精确调度、幂等性保证、批量分配
>
> **真正的能力差距**（不是场景差异）集中在三个领域:
> 1. **可观测**: OpenTelemetry Tracing、Inflight Age Tracking
> 2. **调度策略**: Radix Tree Cache-Aware、Consistent Hashing、PrefixHash
> 3. **流量控制**: Rate Limiting / 并发控制
