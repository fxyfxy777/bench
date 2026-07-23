# SGLang Router (sgl-model-gateway) 更新分析报告

> 分析时间：2026-05-13
> 项目路径：/Users/xingki/project/sglang/sgl-model-gateway/
> 分析范围：2025-12-05 ~ 2026-05-07（全部 379 个 commits）

---

## 1. 项目概况

### 1.1 基本信息

| 项 | 值 |
|---|---|
| 原名 | sgl-router |
| 现名 | sgl-model-gateway (SMG) |
| 语言 | Rust（核心），Python bindings，Go bindings |
| 首个 commit | 2025-12-05 (重命名 sgl-router → sgl-model-gateway) |
| 最新 commit | 2026-05-07 |
| 总 commit 数 | 379 |
| 活跃时长 | ~5 个月 |
| 最新版本 | 0.3.2 (2026-01-15) / 0.3.1 (2026-01-08) |

### 1.2 核心贡献者

| 贡献者 | Commits | 主要方向 |
|--------|---------|---------|
| Simo Lin | 196 (52%) | 核心架构、性能优化、路由策略 |
| fzyzcjy | 68 (18%) | CI/测试基础设施、代码重构 |
| Chang Su | 27 (7%) | gRPC router、Responses API |
| Praneth Paruchuri | 16 (4%) | Workflow engine、MCP |
| Kangyan Zhou | 9 (2%) | 工具解析、MCP |
| 其他 | 63 (17%) | 各方向贡献 |

### 1.3 更新频率统计

| 月份 | Commits | 日均 |
|------|---------|------|
| 2025-12 | 187 | 6.9 |
| 2026-01 | 149 | 4.8 |
| 2026-02 | 12 | 0.4 |
| 2026-03 | 15 | 0.5 |
| 2026-04 | 10 | 0.3 |
| 2026-05 (至7号) | 6 | 0.9 |

**趋势**：12月-1月为密集开发期（日均5-7次提交），2月后进入维护/稳定阶段。

---

## 2. 更新内容分类汇总

### 2.1 路由策略与负载均衡（27 commits）

这是 SMG router 最核心的功能方向，涵盖多种路由策略的实现和优化：

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-08 | 85d0ccfa | 修复缺失的策略决策记录 |
| 2025-12-09 | 9ad02b79 | 优化 radix tree 用于 cache-aware 负载均衡 |
| 2025-12-11 | e99ee0c6 | 修复 PowerOfTwo 策略中不兼容的 metric 比较 |
| 2025-12-15 | 62b3fdae | 修复 cache aware 路由中因错误负载追踪导致的错误路由 |
| 2025-12-17 | 70607e55 | 将 PolicyRegistry RwLock 替换为 DashMap 实现无锁策略查找 |
| 2025-12-24 | 2f7c6292 | 修复 IGW 路由并优化 RouterManager |
| 2025-12-25 | 45adad37 | 新增 ManualPolicy 手动路由策略 |
| 2025-12-25 | caa95c7e | 为 ManualPolicy 增加基于 header 的路由 |
| 2025-12-26 | 6ef543f9 | 使用 X-SMG-Routing-Key header 代替 JSON body |
| 2025-12-27 | 171912a9 | 为 ManualPolicy 添加一致性哈希 |
| 2025-12-27 | 3645ed0f | 新增 PrefixHash 负载均衡策略用于 KV cache 感知路由 |
| 2025-12-31 | 6e792332 | 新增 Radix Tree trait 和通用类型 |
| 2025-12-31 | 7c40cb8b | 新增 StringTree 实现（HTTP router） |
| 2025-12-31 | d2b49a44 | 新增 TokenTree 实现（gRPC router） |
| 2025-12-31 | cf07f3c5 | dp minimum tokens scheduler 支持 bucket |
| 2026-01-02 | 00562ee1 | ManualPolicy 支持 cache eviction |
| 2026-01-04 | 216ea910 | 支持转发 SMG routing key 到引擎端 |
| 2026-01-07 | 3be1e734 | 提取策略中的 header 提取逻辑 |
| 2026-01-07 | b5a94f8a | 修复外部 OpenAI workers 的 IGW 路由 |
| 2026-01-13 | 1f0e3d7f | 支持在 gateway 追踪 worker routing key 负载 |
| 2026-01-13 | 9d3018f4 | ManualPolicy 支持 min load 策略（除随机外） |
| 2026-01-13 | ff3ddb9d | 支持 key-based 负载均衡策略的最小 routing key 数 |
| 2026-01-17 | c824ddd5 | 修复 manual policy min group 模式下无 routing id 请求的不均衡 |
| 2026-01-17 | 9c253064 | **将路由策略 API 改为异步**以支持更多策略 |
| 2026-01-28 | 6f009961 | 优化 HashRing 构建减少堆分配 |
| 2026-01-28 | 897c35b4 | 优化一致性哈希热路径消除分配 |
| 2026-02-15 | f759960a | 在 Python CLI 参数中暴露 consistent_hashing 策略 |

**关键策略类型**：
- **Cache-Aware (PrefixHash)**：基于 KV cache 前缀哈希的路由，利用 Radix Tree 实现前缀匹配
- **ManualPolicy**：支持 header-based routing key、一致性哈希、min load、cache eviction
- **PowerOfTwo (P2C)**：随机选两个 worker 取负载最小的
- **DP-Aware**：数据并行感知路由

### 2.2 性能优化（41 commits）

SMG 团队在性能优化上投入了大量精力，覆盖从内存分配到 CPU 开销的全面优化：

#### 内存/分配优化
| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-08 | 8550822d | 优化 HTTP router 内存使用 |
| 2025-12-08 | d69ecc19 | 减少多处 CPU 开销 |
| 2025-12-08 | 39f9a9c2 | 减少 gRPC router CPU 开销 |
| 2025-12-09 | 73df7a4e | 优化 tokenizer 减少 CPU 和内存开销 |
| 2025-12-09 | 8b98bb76 | 优化核心模块 |
| 2025-12-12 | 1834401e | 优化 worker 选择 |
| 2025-12-13 | 74ea45cc | 优化 metric labels 避免不必要的分配 |
| 2025-12-13 | 20ce9938 | 移除未使用的 TokenizerMetrics 减少 CPU 开销 |
| 2025-12-17 | d747147a | 减少 CPU 开销 |
| 2025-12-17 | 53e15194 | 优化 worker registry 减少 gRPC client fetch 的锁争用 |
| 2025-12-27 | 0e25aa43 | 优化 radix tree 内存并减少分配 |
| 2025-12-28 | ec8c831d | 优化 metrics 最小化 CPU 和内存开销 |
| 2025-12-28 | f13949e5 | 优化可观测日志最小化 CPU/内存开销 |
| 2025-12-29 | 8c6f865a | 优化 prefix_match 零拷贝 tenant + 延迟字符计数 |
| 2025-12-29 | 684e148e | 优化 INSERT 仅叶子节点时间戳更新 |
| 2025-12-29 | ac78f96e | 优化 radix tree 时间戳更新支持多租户扩展 |
| 2025-12-31 | b4ce7a6d | cache_aware 消除热路径中的 String 分配 |
| 2026-01-04 | 4436dc0f | 改善 middleware 锁争用和分配 |
| 2026-01-04 | cf6800f6 | 优化 responses api 中 Vec 和 HashMap 分配 |
| 2026-01-10 | 7c25687c | 重写 gauge_histogram.rs 零分配热路径 |
| 2026-01-13 | 250477d2 | 优化 L1 cache 插入使用增量哈希和分词 |
| 2026-01-26 | 511961870 | 使用 SHA-256 优化 WASM cache 查找 |
| 2026-01-26 | fc7096f8 | 使用 Aho-Corasick 优化特殊 token 搜索 |
| 2026-01-28 | 6f009961 | 优化 HashRing 构建减少堆分配 |
| 2026-01-28 | 897c35b4 | 优化一致性哈希热路径消除分配 |

#### 并发/架构优化
| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-15 | bab20a84 | 并行化 metrics 请求 |
| 2025-12-19 | 0adfc42b | 使用预计算依赖图优化 workflow 引擎 |
| 2025-12-21 | 537ef18d | 使用实例池化和组件缓存优化 WASM Runtime |
| 2025-12-23 | 80ae2229 | 使用无锁快照优化 router 选择 |
| 2026-01-05 | b12258bf | **HTTP Router Fan-out：串行执行替换为并发流** |

### 2.3 可观测性（48 commits）

#### Metrics 体系
| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-14 | 47633c19 | **新增 SMG 6 层 metrics 架构** |
| 2025-12-14 | f9bceea0 | 实现 Layer 1 HTTP metrics 打点 |
| 2025-12-14 | b11af135 | 实现 Layer 2 router metrics (smg_router_*) |
| 2025-12-14 | fb96669f | 新增 Layer 3 worker metrics (smg_worker_*) |
| 2025-12-14 | bd9c3a47 | 新增 harmony gRPC router 流式 metrics |
| 2025-12-14 | 0612175c | 新增 gRPC router 流式 metrics (TTFT, TPOT, tokens, duration) |
| 2025-12-11 | 617e9b3b | 支持自定义 Prometheus duration buckets |
| 2025-12-13 | 9a5d6a84 | Prometheus metrics 增加 error code + X-SMG-Error-Code header |
| 2026-01-04 | c88aaf22 | 支持 in-flight request age metrics |
| 2026-01-10 | 1f9d4795 | 新增 gauge histogram 抽象 |
| 2026-01-14 | f0918583 | 新增 GetLoads RPC 提供全面负载指标 |

#### OpenTelemetry Tracing
| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-06 | e12c6b32 | **实现 OpenTelemetry 请求追踪** (HTTP) |
| 2025-12-07 | aff1238e | 重组 metrics、logging 和 otel 到独立模块 |
| 2025-12-07 | 8fbf7dd5 | 重构 otel 提升效率 |
| 2025-12-07 | c136b42d | 新增 otel 集成测试 |
| 2025-12-08 | edde5e5d | 新增 gRPC router OTEL 集成 |
| 2026-03-07 | 8a411a9a | 移除 mini_lb 中废弃的 tracing 代码 |

#### 结构化日志
| 日期 | Commit | 内容 |
|------|--------|------|
| 2026-01-02 | b23e7ed1 | 缩短日志 target 从 sgl_model_gateway 到 smg |
| 2026-03-05 | e58391dd | 新增 --json-log 标志支持结构化 JSON 日志 |
| 2026-04-14 | c456cba7 | 支持 SGLANG_LOG_MS 毫秒精度日志 |

### 2.4 高可用与可靠性（22 commits）

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-07 | 5e2cda61 | 修复 WASM 安全漏洞 - 执行超时 |
| 2025-12-07 | a4ffd665 | 修复 WASM 每模块内存限制 |
| 2025-12-07 | 2970f229 | 修复 WASM 无界请求/响应体读取漏洞 |
| 2025-12-08 | 7bf16c63 | 修复 WASM 任意文件读取安全漏洞 |
| 2025-12-15 | 5ca962ce | 提取 circuit breaker 状态结构体 |
| 2025-12-19 | 50cad014 | 修复 TLS/Non-TLS server 优雅退出 |
| 2025-12-19 | d72e908b | 提取通用优雅退出代码 |
| 2025-12-21 | 122c2503 | **gRPC routers 新增重试和熔断器支持** |
| 2025-12-21 | 1167867e | **OpenAI router chat endpoint 新增重试支持** |
| 2025-12-22 | 2142881b | 所有 workers 熔断时返回 503 |
| 2026-01-02 | a2d4f58a | 上游超时使用 504 Gateway Timeout |
| 2026-01-12 | d0092dec | 修复 workflow 引擎竞态条件并添加优雅退出 |
| 2026-01-14 | 5938c3b0 | **HA - 轻量级状态层 + gRPC Mesh** |
| 2026-01-12 | 6620548f | StateStore trait 改为 async 支持外部持久化 |
| 2026-01-13 | cf25852a | 新增 --disable-health-check 跳过 worker 健康探测 |
| 2026-02-26 | 3b5c8e65 | 上游取消时取消流式请求 |
| 2026-02-26 | 6f504a2b | 新增 router 上游请求取消 |
| 2026-05-06 | d363315d | HTTP 连接池空闲超时可配置 |
| 2026-05-07 | be088f80 | 配置 HTTP client 连接设置 |

### 2.5 PD 分离架构（Prefill-Decode Disaggregation）（12 commits）

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-08 | cb4cdb43 | 修复 dp-aware 与 service-discovery 不兼容 |
| 2025-12-20 | 3c116d5e | 修复 cache_aware 在 grpc 中的不均衡转发 |
| 2025-12-24 | f65fa047 | gRPC router 启用 IGW 模式，service discovery 开启时自动启用 IGW |
| 2026-01-02 | f66b0916 | 改进 pd_types.rs 文档和风格 |
| 2026-01-04 | 66dfb8c1 | 修复非 PD router HTTP header 白名单缺失 |
| 2026-01-05 | f02d8221 | PD 配置冲突检查移入 model gateway |
| 2026-01-27 | a723d1c5 | PD router 忽略 embeddings/classify 错误 |
| 2026-02-24 | 539f772f | **完全支持 PD 分离模式下的外部 DP 调度** |
| 2026-02-27 | 98e433e3 | 移除 Rust Router 构造参数中的 test_external_dp_routing |
| 2026-04-15 | 9e84f537 | 为 mini_lb 添加绕过 Rust 依赖的 fallback |
| 2026-04-16 | a5bfbcb8 | **R3 支持 PD 分离 (mini_lb)** |

### 2.6 WASM 中间件扩展（15 commits）

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-05 | b72f0268 | 修复 wasm 中遗留的 sgl-router 名称 |
| 2025-12-07 | 5e2cda61 | 修复 WASM 安全漏洞 - 执行超时 |
| 2025-12-07 | a4ffd665 | 修复 WASM 每模块内存限制 |
| 2025-12-07 | 2970f229 | 修复 WASM 无界请求/响应体读取漏洞 |
| 2025-12-08 | 7bf16c63 | 修复 WASM 任意文件读取安全漏洞 |
| 2025-12-21 | 537ef18d | 使用实例池化和组件缓存优化 WASM Runtime |
| 2025-12-28 | 8fab4895 | 修复多核机器上 WASM 测试错误 |
| 2026-01-04 | 4436dc0f | 改善 middleware 锁争用和分配 |
| 2026-01-09 | bd1afeb5 | 通过优化 WASM middleware 缓冲恢复响应流式传输 |
| 2026-01-13 | af1232b2 | 修复 wasm 示例 |
| 2026-01-26 | 8c2d8b51 | 修复 wasm 示例 2 |
| 2026-01-26 | 02c1dabf | 修复 wasm 示例 3 |
| 2026-01-26 | 511961870 | 使用 SHA-256 优化 WASM cache 查找 |
| 2026-01-26 | ed75136e | 替换自管理 WASM 为官方 crate |
| 2026-01-26 | d4adff31 | 更新 wasm endpoint |

### 2.7 gRPC Router（28 commits）

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-08 | edde5e5d | gRPC router 新增 OTEL 集成 |
| 2025-12-08 | 39f9a9c2 | 减少 gRPC router CPU 开销 |
| 2025-12-14 | 0612175c | gRPC router 新增流式 metrics (TTFT, TPOT) |
| 2025-12-14 | bd9c3a47 | harmony gRPC router 新增流式 metrics |
| 2025-12-17 | 53e15194 | 优化 worker registry 减少 gRPC client fetch 锁争用 |
| 2025-12-21 | 122c2503 | gRPC routers 新增重试和熔断器支持 |
| 2025-12-23 | 5f3a47d8 | gRPC router embeddings endpoint 实现 |
| 2025-12-23 | dd620987 | gRPC router 替换 tokenizer 为动态加载的 tokenizer registry |
| 2025-12-24 | f65fa047 | gRPC router 启用 IGW 模式 |
| 2025-12-29 | b2a3f055 | classify pipeline 接入 gRPC router |
| 2025-12-31 | d2b49a44 | gRPC router 新增 TokenTree 实现 |
| 2026-01-05 | 454dc9e2 | 重构 harmony/responses.rs |
| 2026-01-05 | 5a2b1ed4 | 重构 grpc/regular/responses |
| 2026-01-05 | 51541404 | 重构 openai 模块 |
| 2026-01-06 | 21da2dc1 | 统一 ResponsesContext 和 HarmonyResponsesContext |
| 2026-01-06 | 05b54b6d | 用 ExtractedToolCall 替换 Vec<(String, String, String)> |
| 2026-01-08 | 1bc7aa58 | 更新 gRPC proto 匹配上游变更 |
| 2026-01-14 | f0918583 | 新增 GetLoads RPC 全面负载指标 |
| 2026-01-14 | 5938c3b0 | HA - 轻量级状态层 + gRPC Mesh |
| 2026-01-15 | c020d300 | gRPC 创建带 chat template 的 tokenizer |
| 2026-02-12 | 2f283d81 | 整合 gRPC client 到共享 crate 依赖 |

### 2.8 Workflow Engine（12 commits）

SMG 内部引入了 Workflow Engine 概念，用于编排 worker 管理逻辑：

| 日期 | Commit | 内容 |
|------|--------|------|
| 2025-12-12 | 10c68f62 | 提取 workflow engine 到 src/workflow 模块 |
| 2025-12-12 | 306e5b8d | DAG 并行执行支持和 workflow 优化 |
| 2025-12-12 | 56d0ad47 | 处理 workflow 死锁并优化环检测 |
| 2025-12-12 | 526fd008 | workflow engine 清理和小优化 |
| 2025-12-13 | fd37cc5d | 重构 worker steps 并新增 update workflow |
| 2025-12-19 | 0adfc42b | 使用预计算依赖图优化 workflow engine |
| 2026-01-12 | d0092dec | 修复 workflow 引擎竞态条件并添加优雅退出 |
| 2026-01-12 | fa51b854 | 转换 workflow 系统为类型安全的 workflow data |
| 2026-01-12 | ed729d22 | 从类型擦除重构为类型化引擎 |
| 2026-01-12 | 6e158e55 | 改善 workflow engine 代码质量 |
| 2026-01-12 | 6620548f | StateStore trait 改为 async 支持外部持久化 |
| 2026-01-12 | e0ac559a | 新增调度/延迟 steps 和条件分支 |

### 2.9 API 能力扩展（20+ commits）

#### Responses API (OpenAI 兼容)
- 多次重构清理 responses API 架构债务
- 统一 ResponsesContext 和 HarmonyResponsesContext
- 新增 list_tools_for_servers 和 server_keys 线程化
- 优化 Vec 和 HashMap 分配

#### Embeddings & Classification
- 2025-12-23: gRPC router embeddings endpoint
- 2025-12-29: classify pipeline stages 和 protocol types
- 2025-12-29: classify pipeline 接入 gRPC router
- 2026-01-02: embedding correctness test（对比 HuggingFace）

#### Tool Calling & MCP
- 2025-12-10: 动态填充 Tool Call Parser 选择
- 2025-12-21: GLM47 tool parser
- 2026-01-05: responses API 增加 list_tools_for_servers
- 2026-01-12: Qwen Coder tool parser 支持 XML 格式
- 2026-01-25: 替换自管理 MCP 为官方 rmcp crate
- 2026-01-25: 替换自管理 tool call 代码为新 crate

### 2.10 CI/测试基础设施（50+ commits）

这是提交量最大的类别之一，主要由 fzyzcjy 推动：

**核心改进**：
- 从 Python 集成测试迁移到 Rust 集成测试
- 统一 E2E 测试基础设施（GPU 分配器、Model Pool）
- 迁移所有测试到新基础设施：chat completions、responses API、function calling、validation、reasoning、embeddings
- 新增 K8s 集成测试
- 构建优化：sccache 配置、wheel 共享
- 解决孤儿进程问题

### 2.11 代码模块化与依赖治理（2026-01-25 集中）

一天内完成大规模 crate 外部化：

| Commit | 内容 |
|--------|------|
| 6af22f8d | 使用已发布的 reasoning parser crate |
| 8db2802b | 使用官方 openai protocol crate |
| 7a1d7ab4 | 移除自管理 protocols，使用官方 OAI spec |
| 86c7bc64 | 更新 tools crate |
| 2c1e2674 | 替换自管理 tool call 代码为新 crate |
| f5ac1ca1 | 使用官方 tokenizer crate |
| 97a36a72 | 移除死代码 tokenizer |
| bf139af3 | 使用已发布 auth crate 加速编译 |
| 52d0ca94 | 使用官方 wfaas crate |
| 46bc53a8 | 导入 db crate |
| 7890a96f | 替换自管理 MCP 为官方 rmcp crate |
| ed75136e | 替换自管理 WASM |

---

## 3. 架构演进时间线

```
2025-12-05  sgl-router 重命名为 sgl-model-gateway
     │
2025-12-05~12-15  基础能力建设
     │  - OpenTelemetry tracing
     │  - 6层 metrics 架构
     │  - WASM 安全漏洞修复（4个）
     │  - Circuit breaker + retry
     │  - 优雅退出 TLS 支持
     │  - 错误响应格式统一
     │
2025-12-15~12-31  核心功能密集开发
     │  - ManualPolicy（header-based routing + consistent hashing）
     │  - PrefixHash 策略（KV cache aware）
     │  - Radix Tree（StringTree/TokenTree）
     │  - WASM Runtime 优化（实例池化）
     │  - 无锁 PolicyRegistry (DashMap)
     │  - 无锁 router selection (lock-free snapshots)
     │  - Workflow Engine（DAG 并行执行）
     │  - Classification pipeline
     │
2026-01-01~01-17  稳定与性能
     │  - 大量性能优化（零分配热路径、增量哈希）
     │  - HA - gRPC Mesh 轻量级状态层
     │  - 异步路由策略 API
     │  - Workflow Engine 类型安全重构
     │  - Redis 持久化后端
     │  - GetLoads RPC
     │  - 版本发布 0.3.1 → 0.3.2
     │
2026-01-25  依赖外部化（一天内完成）
     │  - 12+ crates 外部化
     │
2026-01-26~02-28  优化与 PD 分离
     │  - 一致性哈希热路径优化
     │  - PD Disaggregation + External DP Dispatch
     │  - 上游请求取消
     │
2026-03~05  维护期
     │  - 结构化 JSON 日志
     │  - mini_lb R3 支持
     │  - HTTP 连接池配置
     │  - K8s 集成测试
     │
```

---

## 4. 关键设计决策总结

### 4.1 路由架构
- **双协议路由**：HTTP router (axum) + gRPC router，共享路由策略
- **策略插件化**：PolicyRegistry 支持动态注册，策略 API 异步化
- **Cache-Aware**：Radix Tree + PrefixHash 利用 KV cache 局部性
- **无锁设计**：DashMap 替代 RwLock，lock-free snapshot 用于 router 选择

### 4.2 可扩展性
- **WASM 中间件**：通过 WebAssembly 支持用户自定义请求处理逻辑
- **Workflow Engine**：DAG 编排 worker 管理流程，支持条件分支和延迟步骤
- **gRPC Mesh**：多实例 HA 状态同步

### 4.3 性能哲学
- 热路径零分配（zero-allocation hot path）
- 增量哈希/分词代替全量计算
- Aho-Corasick 替代线性搜索
- 实例池化（WASM/gRPC client）
- 并发流替代串行执行

### 4.4 可靠性
- Circuit Breaker + 自动重试
- 优雅退出（TLS/Non-TLS）
- 上游取消传播
- Worker 健康检测可选
- 503 全熔断保护

---

## 5. 对 RL-Router 的参考价值

| SMG 特性 | RL-Router 对应/参考 |
|----------|-------------------|
| 异步策略 API | scheduler policy 可考虑异步接口 |
| Cache-Aware 路由 (Radix Tree) | 推理场景 KV cache 复用可参考 |
| Circuit Breaker + Retry | P0 稳定性需求：后端实例健康检测 |
| gRPC Mesh HA | P0 需求：Scheduler 无状态恢复 / HA |
| 6 层 Metrics 架构 | 可借鉴分层设计：HTTP → Router → Worker |
| WASM 中间件 | 可选的请求处理扩展性 |
| 零分配热路径优化 | 高性能调度参考 |
| 上游请求取消传播 | SSE 代理场景需支持 |
| HTTP Fan-out 并发流 | Gateway 多后端请求可参考 |
| ManualPolicy (header routing) | step-based 路由策略参考 |
| GetLoads RPC | Scheduler ← Gateway 负载上报参考 |
| PD Disaggregation | Prefill-Decode 分离架构路由支持 |
| Lock-free PolicyRegistry | 调度策略查询无锁化 |
| 结构化 JSON 日志 | 已有 zap，可对齐格式 |

---

## 6. 附录：全部 379 commits 时间分布热力图

```
日期        commits
12-05       ████ 4
12-06       ██ 2
12-07       ███████ 7
12-08       ██████████ 10
12-09       ████████ 8
12-10       ███████ 7
12-11       ████████ 8
12-12       ██████████████ 14
12-13       ████████████████ 16
12-14       ███████████ 11
12-15       ███████ 7
12-17       ████ 4
12-19       █████████ 9
12-20       ██ 2
12-21       █████████ 9
12-22       ███ 3
12-23       █████████ 9
12-24       █████████ 9
12-25       █████ 5
12-26       ███ 3
12-27       █████ 5
12-28       ██████████ 10
12-29       ██████████ 10
12-30       ██ 2
12-31       █████████████ 13
01-01       ███████ 7
01-02       █████████████████ 17
01-03       ███ 3
01-04       ██████████████ 14
01-05       ████████████ 12
01-06       ████████████ 12
01-07       ███████████ 11
01-08       █████████████ 13
01-09       ██ 2
01-10       ██ 2
01-11       █ 1
01-12       ███████████ 11
01-13       ██████ 6
01-14       ██ 2
01-15       ████ 4
01-16       ████ 4
01-17       ████ 4
01-22       █ 1
01-25       ███████████ 11
01-26       ████████ 8
01-27       █ 1
01-28       ██ 2
01-29       █ 1
02-12       ██ 2
02-14       █ 1
02-15       █ 1
02-23       ██ 2
02-24       ██ 2
02-26       ██ 2
02-27       ██ 2
03-03~05-07  ~20 (维护期)
```

---

## 7. 总结

SGLang Model Gateway (SMG) 在 5 个月内经历了从 `sgl-router` 重命名到功能完备的快速演进：

1. **更新频率极高**：前 2 个月日均 5-7 次提交，由 2-3 名核心贡献者驱动
2. **性能优化为核心**：零分配热路径、无锁数据结构、Aho-Corasick/RadixTree 等算法优化
3. **策略丰富**：从简单 round-robin 到 cache-aware (PrefixHash)、consistent hashing、manual routing
4. **生产就绪**：circuit breaker、重试、优雅退出、6 层 metrics、OpenTelemetry tracing
5. **架构解耦**：大量 crate 外部化，WASM 中间件扩展，Workflow Engine 编排
6. **PD 分离**：专门支持 Prefill-Decode 分离架构的路由策略

对于 RL-Router 项目，最值得借鉴的是：
- **HA 设计**（gRPC Mesh 状态同步）
- **熔断/重试**（后端实例健康检测）
- **无锁热路径**（DashMap、lock-free snapshot）
- **分层 Metrics**（HTTP → Router → Worker → Engine）
- **请求取消传播**（SSE 场景）
