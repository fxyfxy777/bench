# 基础提示词
你是一个分布式技术专家，你熟悉trino等大规模分布式系统的设计，你精通golang，你也精通网关设计，你也熟悉sglang、vllm等推理后端框架，也熟悉verl/slime等强化学习训练框架，熟悉rollout中的router的设计，你有自己的代码洁癖和主见。

# 基础架构

我需要设计一个流量调度系统在架构设计上需要满足以下条件：
* 既可以当中心Scheduler负责请求中心调度，也可以当gateway负责请求转发
* 支持多种调度算法，具备对算法的扩展性
* 这个系统整体叫做router，整个项目的名称代号叫做OneRouter
* gateway还需要快速返回请求信息给Scheduler做后续调度。
* 类gateway和scheduler会使用router的配置
* gateway 每次请求中需要向scheduler实时注册负载状态，结束请求需要释放负载状态
* 整体系统部署时只有会有一个scheduler和多个gateway，也可以scheduler和gateway混合单个节点
* gateway需要使用scheduler中的instance信息进行调度
* 需要scheduler有全局负载信息，做全局绝对的负载均衡
* 面对gpu调度的场景，请求的qps可能不会特别高，但是需要极致的负载均衡，以最高效地利用gpu
* gateway转发推理请求
* gateway后端需要支持多种后端sglang、vllm、fastdeploy等推理框架的chat completions接口，还需要支持openai的chat_completion接口
* Scheduler侧使用event-loop接收gateway的allocator请求
* gateway和Scheduler中增加注册机制，gateway节点起来之后会向Scheduler注册，上报心跳
* 每一轮推理为了互不影响，Scheduler需要有状态，每一轮互相隔离
* gateway到Scheduler的状态同步
  * scheduler收到 开始/停止 指令时，发送rpc请求给gateway，重试、超时控制
  * gateway 定时心跳同步状态
* 项目需要丰富的测试，以保证代码可以快速迭代
* 当gateway与Scheduler互相发送请求的时候,tcp连接出现问题了,请求状态不知道发没发送过去，也不知道能不能重试，这种“请求状态不确定”问题，所有内部接口都要考虑
* 每个方法写的时候需要防御性编程，在单测中需要考虑一些边界case
* 单测如果不过,你需要先思考原始代码设计是不是合理,再看看如何写单测
* 新增加代码需要考虑并发安全，如果有并发隐患，需要增加并发安全测试
* event-loop 专属方法（如 BatchSelect）的并发测试原则：方法本身是单线程的，内部 buffer 复用不需要并发安全；但方法访问的共享状态（sessionAssign、instanceMeta 等）可能被其他方法（Reset、RemoveSession、UpdateInstanceMeta）并发修改，必须测试这种真并发场景 — 1 个 goroutine 模拟 event-loop 串行调用，N 个 goroutine 并发调用会修改共享状态的方法。纯串行的正确性测试不要加 `Concurrency_` 前缀。

# 性能要求
* 要支持万卡后端，Scheduler需要能够维护1w实例的状态，能够根据1w实例的负载，快速调度
* Scheduler需要抗住10w的瞬时请求，不能崩溃，可以削峰填谷（16c 32g）
* gateway需要能够维持1w的sse模式下的chat_completions请求数 （8c 64g）
* 单次chat completions请求可能最大每秒2000tokens

# 稳定性要求
* 训练任务可能会跑持续半年，中间可能会出现任何问题，需要及时上报、快速定位、自动或手动恢复后不影响流程
* 一个Scheduler加多个gateway会服务一个实验任务，每个实验任务隔离，互不影响

# 可观测设计
* 日志、指标需要提前打出来，不能在hang住最需要日志和指标的时候，打不出日志，看不到指标
* 需要详细的日志、指标，达到只看日志和指标就能快读定位问题的能力
* 任何能力的增加都需要考虑自证能力，需要自证自己没问题

# 技术选型
* 日志框架：zap
* json序列化使用统一门面
    * go-json

* http框架兼容：
    * Gin
    * Hertz

* rpc框架：gRPC & Protobuf
* 测试：
    * Testify + GoMock
    * 集成测试：Docker Compose

* 配置：
* 可观测：
    * OpenTelemetry
    * Prometheus


# 目录参考

```
RL-Router/
├── .gitignore
├── Makefile
├── go.mod
├── go.sum
├── ci.yml
│
├── api/
│   └── proto/
│       └── router.proto            # gRPC 服务定义
│
├── cmd/
│   └── router/
│       └── main.go                 # 入口，解析 flag，组装 App
│
├── internal/
│   ├── app/
│   │   └── app.go                  # 顶层容器，按 Mode 组装 scheduler/gateway
│   │
│   ├── compat/
│   │   └── v2_adapter.go           # /api/v2/* 向后兼容路由
│   │
│   ├── config/
│   │   └── config.go               # 配置结构体，Mode 定义 (gateway/scheduler/hybrid)
│   │
│   ├── domain/
│   │   └── models.go               # 领域模型：Instance, NodeState, RouteContext, CostMetrics
│   │
│   ├── gateway/
│   │   ├── allocator.go            # InstanceAllocator 接口，LocalAllocator, RemoteAllocator
│   │   ├── proxy.go                # 反向代理
│   │   ├── scheduler_client.go     # SchedulerClient：注册、心跳、状态同步
│   │   └── server.go               # Gateway 数据面 HTTP 服务
│   │
│   └── scheduler/
│       ├── server.go               # Scheduler 控制面，event-loop 串行化
│       ├── event.go                # event 类型定义、对象池、常量
│       ├── http_handler.go         # HTTP 控制面 handler
│       ├── grpc_handler.go         # gRPC handler
│       ├── store/                  # NodeStateStore 节点负载状态存储
│       ├── registry/               # GatewayRegistry 网关注册表
│       ├── notifier/               # StepNotifier 步骤状态推送
│       ├── collector/              # 后端实例指标采集器
│       └── policy/                 # 调度策略（min_load, round_robin 等）
│
├── docs/
│   ├── design.md                   # 总设计文档
│   ├── ops-runbook.md              # 运维手册
│   ├── testing-strategy.md         # 测试策略
│   ├── perf/                       # 性能分析类文档
│   └── analysis/                   # 对比分析类文档
│
└── pkg/
    ├── logger/
    │   └── logger.go               # zap 日志封装
    └── metrics/
        └── metrics.go              # Prometheus 指标
```

# 开发注意事项
* 日志 和 错误信息一定要需要说清楚上下文，需要包含where——happen / what——happen / why——happen / with which data
* 所有请求一定假设会出错，一定要有重试机制，一定要有幂等性
* 当网络访问不通时，使用以下代理：
```
    export http_proxy=agent.baidu.com:8188
    export https_proxy=agent.baidu.com:8188
    export no_proxy=127.0.0.1,0.0.0.0,localhost,bcebos.com,baidu.com,baidu-int.com 
```
* 函数不能超过150行
* 不能用rand，需要用v2
* 每次修改都需要考虑性能
* 每次修改完了，把修改部分自动生成对应commit message，并提交到本地，千万不要提交到远端！！！
* 需要考虑优雅退出
* commit message 不要加 co-authored-by...
* commit message 需要精简、清晰、工程化，优先参考历史提交格式：
  * 标题使用 `type(scope): summary`，summary 说明本次改动的核心动作，不要口语化。
  * 正文按需使用：复杂变更先一句概述，再按主题列 bullet；简单变更可以只保留标题或一小段正文。
  * bullet 要描述“做了什么 / 为什么”，不要写流水账；避免把多个无关点塞进一段。
  * `Tested` 不是必填项。只有验证信息对 review、回归或上线判断有价值时才写；纯文档、注释、指令类提交通常省略。
  * 如果写 `Tested`，保持简短，优先压缩成一行；只有复杂验证矩阵才展开多行。
  * 不要手写 `Change-Id`，由本地 hook 自动处理。
  * 推荐格式：
```text
refactor(pd): canonicalize instance endpoint

Normalize instance address ownership across the PD path:

- Remove InferPort from domain.Instance and PDInstanceInfo; reserve protobuf field 4.
- Keep Host as topology-only metadata for IPC/RDMA placement checks.
- Convert /api/v2/instances infer_port to Endpoint at the compatibility boundary.
- Forward PD prefill/decode requests using Instance.Endpoint directly.

Updated coverage and docs:

- Cover instanceToPDInfo endpoint mapping.
- Update splitwise/compat tests and design docs for endpoint-only forwarding.
```
* codex会review你的代码
* 旧架构代码目录在：/Users/xingki/project/rollout-controller
* 测试
  * mock 后端接口
  * 单测覆盖率98%
  * 集成测试
  * 场景性能测试
* 启动RC的时候每次帮我删除RC的日志 
* 当新增加http新api时，需要帮我修改对应单测，保证不同启动模式下注册的http api是符合预期的
* 每次实现新的功能之后，帮我同步更新design.md文件
* 当有增加新特性的时候，帮我看看这些新特性存不存在内存泄漏和并发冲突，如果有可能存在，帮我增加安全并发测试
* 当有增加新特性可能会影响性能的时候，帮我增加并运行benchmark_test，并分析结果进行优化，对比performance-optimization.md里面的数据，如果有性能提升，帮我更新performance-optimization.md文档
* 每次提交前需要用modern-go-guidelines skill自查一下代码
* 每次提交前用go-perf-concurrency skill自查一下是否有性能优化的点
* 写出的代码需要便于ai阅读
* 每次提交代码前需要用go-test-coverage 确保单测覆盖率和边界case


# 健康检查

管理端点（探针、metrics、pprof、log-level）默认部署在独立的 Admin 端口（`AdminAddr`，默认 `:8081`），与业务流量隔离。设置 `admin_addr: ""` 或 `--admin-listen ""` 可回退到共享主端口。

## Admin 端口端点

| 端点 | 语义 | 成功 | 失败 |
|------|------|------|------|
| `GET /healthz` | Liveness：进程是否存活 | 200（空 body） | 进程已不可达 |
| `GET /readyz` | Readiness：能否接收流量 | 200 + JSON | 503 + JSON |
| `GET /metrics` | Prometheus 指标 | 200 | — |
| `GET/PUT /v1/admin/log-level` | 运行时日志级别调整 | 200 | 400/404 |
| `GET /debug/pprof/*` | Go pprof 性能分析 | 200 | — |

### /readyz 检查项

按当前组件自动组装，后续增加功能时在 `handleReadyz` 中追加 check 即可：

| check key | 触发条件 | ready 判定 |
|-----------|---------|-----------|
| `step` | scheduler 存在（scheduler/hybrid 模式） | `phase == serving` |
| `scheduler_conn` | schedulerClient 存在（gateway 模式） | `IsServing() == true` |

响应格式：
```json
// 200 OK
{"status":"ready","checks":{"step":"serving (step_id=42)"}}

// 503 Service Unavailable
{"status":"not_ready","checks":{"scheduler_conn":"not_serving"}}
```

### 扩展方式

在 `app.handleReadyz` 中增加检查项，模式：
```go
if a.xxx != nil {
    checks["xxx"] = "..."
    if !xxxReady { ready = false }
}
```

# 稳定性现状评估

目标：训练任务持续半年运行，中间任何故障可及时上报、快速定位、自动或手动恢复后不影响流程。

### 已具备的能力

| 能力 | 实现位置 | 说明 |
|------|---------|------|
| Gateway 自愈 | `scheduler_client.go` | 心跳连续 3 次失败自动重注册，指数退避（500ms~10s） |
| 幽灵分配清理 | `gateway_registry.go` → `server.go` | Gateway 心跳超时后 Scheduler 自动释放其全部占用 |
| Step 隔离 | `server.go` | 严格状态机 IDLE→SERVING→DRAINING→IDLE，每轮开始重置负载+策略状态 |
| 优雅退出 | `main.go` + `app.go` | 双阶段信号处理，HTTP/gRPC 各自 graceful stop + 超时兜底 |
| 健康探针 | `app.go` | `/healthz` + `/readyz` 多维检查，可对接 K8s |
| 结构化日志 | `logger.go` | zap + 上下文字段（traceID、gatewayID 等） |
| Prometheus 指标 | `metrics.go` + `server.go` | 7 项指标 + `/metrics` 端点 |
| errgroup 联动 | `app.go` | 任一 server goroutine 异常，全组件联动退出 |

### 欠缺项（按优先级排列）

#### P0 — 不解决会直接导致长期任务中断

| # | 问题 | 影响 | 建议方案 |
|---|------|------|---------|
| 1 | **Scheduler 无状态恢复** | 100% 内存，进程重启 = 全量丢失（实例注册、分配状态、step 阶段）。半年中 Scheduler 崩溃一次即全量中断 | 实现 checkpoint/snapshot 到本地文件或 etcd；长期考虑主备选举 |
| 2 | **无 panic recovery** | HTTP handler / gRPC handler 任何 panic 直接崩溃进程，无兜底 | 添加 HTTP middleware + gRPC interceptor 做 `recover()` + 报警 |
| 3 | **无后端实例健康检测** | 后端推理实例宕机后 Scheduler 仍向其分配流量，gateway 代理失败只记日志不摘除。半年运行中 GPU 实例故障是必然事件 | 被动检测：连续 N 次代理失败标记 unhealthy 并停止分配；可选主动探测 |

#### P1 — 显著影响稳定性

| # | 问题 | 影响 | 建议方案 |
|---|------|------|---------|
| 4 | **无分布式 Tracing** | 仅有 X-Trace-ID 日志透传，万卡规模靠 grep 日志难以定位问题 | 接入 OpenTelemetry，覆盖 gateway→scheduler→backend 关键路径 |
| 5 | **无告警机制** | Prometheus 指标已有但无 Alertmanager 规则/webhook，"及时上报"无法实现 | 定义关键告警：心跳丢失、分配失败率飙升、active request 积压、gateway 下线 |
| 6 | **Gateway 关闭时不主动注销** | 只断 gRPC 连接，Scheduler 等心跳超时（最长 15s）才感知，期间分配到已关闭 gateway 的请求失败 | shutdown 时主动发送 Deregister RPC |
| 7 | **代理转发无重试** | 推理请求代理失败直接返回 502，不尝试其他实例 | 可重试错误（连接拒绝、超时）自动选择新实例重试 1~2 次 |

#### P2 — 影响可运维性和定位效率

| # | 问题 | 建议方案 |
|---|------|---------|
| 8 | `step_phase` / `step_id` gauge 定义了但从未写入 | 在 `handleStartStep` / `handleEndStep` 中补充 `.Set()` |
| 9 | gRPC 无 metrics interceptor | 添加 gRPC server/client interceptor，采集 Allocate/Release/Heartbeat 延迟和错误率 |
| 10 | event-loop 无性能指标 | 暴露 channel 深度、批次大小、处理延迟 |
| 11 | 无 circuit breaker | 后端持续超时时不熔断，拖垮整条链路。建议对 proxy 层添加熔断 |
| 12 | RemoteAllocator 无显式超时 | 继承 HTTP context，scheduler 慢时请求无限挂起。建议加独立超时（如 3s） |
| 13 | Allocate 请求不带 step ID | Gateway 缓存的 step 状态过时可能分配到错误轮次。建议请求中携带 step_id 并校验 |
| 14 | 零测试覆盖 | 整个项目无 `_test.go`，长期运行信心不足 |
| 15 | `os.Exit(1)` 跳过 defer | 超时强退时 zap 日志丢失最后几条关键信息。改用 `runtime.Goexit` 或先手动 `Sync()` |

### 结论

当前系统在**单次短周期运行**中基本可用。但要达到"**持续半年运行**"的稳定性目标，核心差距三项：

1. **不能容忍 Scheduler 崩溃**（无状态恢复 / 无 HA）
2. **不能容忍后端实例故障**（无健康检测 / 无自动摘除）
3. **不能快速发现和定位问题**（无告警 / 无 Tracing）

建议推进顺序：**P0（3 项）→ P1 告警 + Tracing → P1 其余 → P2**
