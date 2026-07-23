# OneRouter 设计文档

## 1. 概述

OneRouter 是面向强化学习（RL）训练场景的 GPU 推理请求流量调度系统。它位于训练框架与 GPU 推理后端集群之间，负责将推理请求（`/v1/chat/completions`、`/v1/completions`、`/generate`、`/v1/reward`）智能路由到最优的后端实例（vLLM、SGLang、FastDeploy 等），同时与训练框架的 Step 生命周期深度联动。

### 1.1 设计目标

| 目标 | 量化指标 |
|------|----------|
| 万卡级实例管理 | 单 Scheduler 维护 10,000 实例的实时负载状态 |
| 高吞吐调度 | 抗住 10w 瞬时请求（16c32g），event-loop 吞吐 ~24w QPS，端到端 ~408ms |
| 高并发代理 | 单 Gateway 维持 10,000 SSE 长连接（8c64g） |
| 极致负载均衡 | 全局绝对负载均衡，最大化 GPU 利用率 |

---

## 2. 旧架构核心问题（What We Fixed）

基于对旧系统 `Rollout-Controller` 的深度分析，我们识别出以下 **5 大核心问题** 和 **6 个衍生问题**，并在新架构中逐一解决。

### 2.1 问题总览

| # | 旧问题 | 根因 | 影响 | 新架构解法 | 对应章节 |
|---|--------|------|------|-----------|---------|
| P0-1 | **全局单例泛滥，模块强耦合** | 10+ 全局变量（RouterInstance、RedisClient 等），缺乏依赖注入 | 无法单测、改一处动全身 | 接口驱动 + 构造函数注入 + App 容器组装 | §3.3 |
| P0-2 | **Redis 滥用导致每请求 3-15ms 额外延迟** | 每次请求 3-5 次 Redis 调用（ZRangeWithScores、HGet、ZIncrBy），无本地缓存 | 调度延迟不可控 | 纯内存状态 + Event-Loop 串行化，**零外部 I/O** | §3.1, §4.2 |
| P1-1 | **核心调度逻辑无单元测试** | Scheduler、负载均衡策略 0% 覆盖率 | 修改无信心 | 全接口化设计，可 mock 可测 | §3.3 |
| P1-2 | **可观测性薄弱** | logit 无压缩/轮转/采样，指标维度不足，追踪覆盖不全 | 线上排障困难 | zap 结构化日志 + Prometheus 全指标 + readyz 探针 | §5 |
| P2-1 | **Bucket 定制化功能散落 4 个文件，配置硬编码** | 缺少统一抽象，环境变量硬编码 | 可读性极差 | 统一 Policy 接口 + 工厂模式，配置集中管理 | §3.2 |
| P2-2 | **HTTP 无连接池，每请求新建 http.Client** | `client := &http.Client{}` | 1w 并发下连接建立/销毁开销大 | 共享 `httputil.ReverseProxy` + SSE 流式转发 | §4.3 |
| P3-1 | **GDP 框架无人维护** | 依赖 logit/pbrpc/gorm_adapter 等废弃组件 | 安全漏洞无法修复，Go 版本锁定 | 完全去 GDP，技术栈替换为 zap + gRPC + 标准库 | §5.1 |
| P3-2 | **配置管理混乱** | string 类型存 bool/int/Duration，多来源（TOML+ENV+YAML+硬编码） | 类型不安全，运维易出错 | 强类型 Config 结构体 + CLI flag 统一入口 | §5.2 |
| 新-1 | **错误处理不一致** | 混用 panic/error/静默吞错 | 线上异常难定位 | 统一 error 返回 + 结构化日志上下文 | §5.3 |
| 新-2 | **并发安全隐患** | 全局变量无锁保护（`EnableBucketBalance`） | 可能 panic | 原子化 + Event-Loop 单线程写 | §4.1 |
| 新-3 | **无优雅降级** | Redis/MySQL 故障直接报错，无熔断 | 服务不可用 | 零外部依赖（纯内存），自愈注册机制 | §3.1, §4.4 |

---

## 3. 核心架构设计

### 3.1 总体架构

```
训练框架 (HTTP)
   │
   │  POST /v1/steps/start  ← 开始推理
   │  POST /v1/steps/end    ← 结束推理
   │  POST /v1/instances    ← 注册后端实例
   │  PUT  /v1/instances    ← 同步后端实例（原子全量替换）
   │  GET  /v1/status       ← 全量运行时状态（QA/调试用）
   v
┌──────────────────────────────────────────────────────────────┐
│                  SCHEDULER（控制面）                           │
│                                                              │
│  ┌───────────┐    channel     ┌─────────────┐               │
│  │ GRPCHandler├──────────────>│  Event Loop  │               │
│  └───────────┘                │  (单线程)     │               │
│  ┌───────────┐                │              │               │
│  │ HTTPHandler├──────────────>│  Select +    │               │
│  └───────────┘                │  Acquire     │               │
│                               │  原子操作     │               │
│  ┌────────────────┐           └──────┬───────┘               │
│  │ GatewayRegistry │                 │                       │
│  │ (心跳+过期检测)  │                 v                       │
│  └────────────────┘           ┌──────────────┐               │
│                               │NodeStateStore│               │
│  ┌──────────────┐             │ (纯内存状态)  │               │
│  │ StepNotifier │             └──────────────┘               │
│  │ (推送状态变更) │                    │                       │
│  └──────────────┘             ┌───────┴────────┐             │
│                               │ Policy (可插拔)      │             │
│                               │ ├ min_load          │             │
│                               │ ├ min_request       │             │
│                               │ ├ round_robin       │             │
│                               │ ├ session_aware       │             │
│                               │ └ session_aware_v3  │             │
│                               └─────────────────────┘             │
└──────────────────────────────────────────────────────────────┘
   ▲  gRPC (Allocate/Release)              │ HTTP Push
   │  gRPC (Register/Heartbeat)            │ (/v1/internal/step-state)
   │                                       v
┌──────────────────────────────────────────────────────────────┐
│                  GATEWAY（数据面）                             │
│                                                              │
│  ┌─────────────────┐    ┌───────────────┐                   │
│  │ SchedulerClient  │    │ GatewayServer │                   │
│  │ (注册+心跳+自愈)  │    │               │                   │
│  └─────────────────┘    │ Allocate      │                   │
│                         │   ↓            │                   │
│  ┌─────────────────┐    │ ReverseProxy  │                   │
│  │InstanceAllocator │    │ (SSE流式转发)  │                   │
│  │ ├ Local (hybrid) │    │   ↓            │                   │
│  │ └ Remote (gRPC)  │    │ Release       │                   │
│  └─────────────────┘    └───────────────┘                   │
└──────────────────────────────────────────────────────────────┘
   ▲                                       │
   │  /v1/chat/completions | /v1/reward    │  HTTP Proxy
   │  (OpenAI/FastDeploy 兼容)             v
 客户端                              后端推理实例
                                    (vLLM / SGLang / FastDeploy)
```

### 3.2 三种部署模式

系统通过 `Mode` 配置支持三种部署形态，同一份代码适应不同规模：

| 模式 | 运行组件 | 适用场景 | Gateway→Scheduler 通信 |
|------|---------|---------|----------------------|
| `hybrid` | Scheduler + Gateway 同进程 | 单节点部署、开发调试 | 函数直调（零网络开销） |
| `scheduler` | 仅 Scheduler | 大规模集群，独立控制面 | — |
| `gateway` | 仅 Gateway | 大规模集群，水平扩展数据面 | gRPC |

> **解决旧问题**：旧架构只有单一部署模式，无法按需扩展。新架构通过 `InstanceAllocator` 接口的 `LocalAllocator`/`RemoteAllocator` 两种实现，在编译期零成本切换。

### 3.3 接口驱动的依赖注入

旧架构最严重的问题是 **10+ 全局单例** 导致的模块耦合和不可测。新架构通过以下手段彻底解决：

#### 核心接口清单

```go
// 调度策略 — 可插拔算法
type Policy interface {
    Select(ctx, req, nodes) (*Instance, error)
    Feedback(instanceID, metrics)
}
type BatchSelector interface {   // 可选：批量优化
    BatchSelect(reqs, nodes) []BatchSelectResult
}
type Resettable interface {      // 可选：Step 间状态重置
    Reset()
}

// 实例分配 — 屏蔽本地/远程差异
type InstanceAllocator interface {
    Allocate(ctx, req) (*Instance, error)
    Release(ctx, instanceID, gatewayID, metrics) error
}

// 反向代理 — 可替换转发实现
type Proxy interface {
    Forward(w, r, target) error
}
```

#### 依赖注入方式

所有组件通过 **构造函数注入**，由 `App` 容器根据 Mode 一次性组装：

```
App.New(config, logger)
  ├── initScheduler()
  │     ├── policy.Build(name)          ← 工厂创建策略
  │     ├── NewNodeStateStore()
  │     ├── NewServer(store, policy)    ← 核心 Event-Loop
  │     ├── NewGatewayRegistry()        ← 心跳管理
  │     ├── NewStepNotifier(registry)   ← 推送通知
  │     ├── NewGRPCHandler(server, registry)
  │     └── NewHTTPHandler(server, registry, notifier)
  │
  └── initGateway()
        ├── [hybrid] NewLocalAllocator(server.Allocate, server.Release)
        │            WithStepChecker(localStepChecker)
        │
        └── [gateway] NewSchedulerClient(addr, interval)
                      NewRemoteAllocator(client.Conn())
                      WithStepChecker(schedulerClient)
                      WithStepUpdater(schedulerClient)
```

> **解决旧问题**：零全局变量、零 `init()` 副作用。所有依赖显式传入，任意组件可 mock 替换。

---

## 4. 关键设计决策

### 4.1 Event-Loop 单线程调度（解决：并发安全 + Redis 依赖）

这是新架构最核心的设计决策，**同时解决了旧架构的两个 P0 问题**。

**旧架构问题**：
- 每次调度需 3-5 次 Redis 调用（ZRangeWithScores 获取负载 → HGet 获取实例 → ZIncrBy 更新负载），累计 3-15ms 额外延迟
- 多个 goroutine 并发读写全局变量，存在 race condition

**新架构方案**：

```
                     ┌──────────────────────┐
goroutine A ─event─→ │                      │
goroutine B ─event─→ │   buffered channel   │──→ 单线程 Event-Loop
goroutine C ─event─→ │   (cap: 131,072)     │    ├ Select（策略选择）
                     └──────────────────────┘    ├ Acquire（计数+1）
                                                  └ 原子完成，无锁
```

核心文件：[server.go](internal/scheduler/server.go)

**设计要点**：

1. **所有状态变更串行化**：Select + Acquire、Release + Feedback、Step 转换 均通过 event channel 进入单线程处理，**无需任何锁**
2. **安全投递**：所有 public API 通过 `submitEvent(ctx, ev)` 投递，channel 满、调用方取消、event-loop 停止时都能有界返回，不会卡在 `eventCh <- ev`
3. **批量排空优化**：`drainAll()` 非阻塞收集最多 8192 个事件，`processBatch()` 保持 FIFO 有序处理
4. **纯内存状态**：`NodeStateStore` 完全在内存中维护实例负载，**零外部 I/O**
5. **原子读取器**：`NodeState` 的 `ActiveRequests`、`LockedMemory`、`ActualLoad` 使用 `atomic` 操作，允许监控协程无锁读取；`PolicyName`、normal/PD active count 也通过 atomic mirror 暴露给 HTTP 状态接口

**量化对比**：

| 指标 | 旧架构 | 新架构 | 提升 |
|------|--------|--------|------|
| 单次调度 I/O | 3-5 次 Redis RTT (3-15ms) | 0 次（纯内存） | **延迟消除** |
| 并发安全 | 全局变量无锁（可能 panic） | Event-Loop 串行化 | **正确性保证** |
| 外部依赖 | Redis 强依赖（故障=不可用） | 零外部依赖 | **100% 自治** |

#### 多资源组隔离

OneRouter 支持在同一个 Scheduler 中管理多个相互隔离的资源组，用于多租户、多实验或多 rollout 任务共享同一控制面。资源组是 `Instance.ResourceGroup` 和 `RouteContext.ResourceGroup` 的显式字段；未设置时统一归入 `default`。

核心原则：

- **单 event-loop，不按资源组拆 event-loop**：所有状态变更仍由一个 event-loop 串行化，避免跨 loop 协调、释放乱序和排队公平性问题。
- **每个资源组一个 `groupRuntime`**：独立维护 step phase、step id、pause、policy、policy config、waiting queue、normal/PD active count、allocate/release dedup、PD policy。
- **实例按显式字段建二级索引**：`NodeStateStore` 仍保留全局实例表，同时维护 `resource_group -> nodes` 的 COW 索引；调度时 policy 只看到当前资源组的候选节点。
- **资源组 selector 写入实例归属**：控制面显式传入 `resource_group` 时，该 selector 是实例组唯一归属；同一个 instance ID 已属于其他资源组时返回 `409 Conflict`，调用方必须先从旧组注销，避免跨组“偷实例”。`labels.resource_group` 仅作为普通 metadata 保留，不参与调度归属。
- **Gateway 按资源组缓存 step 状态**：Scheduler 的 register/heartbeat 返回全量 `resource_group_states` 快照，step start/end/pause/continue 再通过 `/v1/internal/step-state` 推送单组增量；Gateway precheck 读出请求资源组后只按该组状态快速拒绝。Gateway 用互斥保护本地状态缓存的 read-copy-update，避免 heartbeat 全量替换覆盖 push 增量。
- **allocation_id 含资源组维度**：每个资源组独立维护 allocation counter，allocation id 编入 step id 和 resource group hash；同一 step id 下不同资源组不会产生 release 归错组的碰撞。
- **限流分层**：`global_max_inflight` 保护整个 Scheduler；`max_inflight` 保护单个资源组，normal 请求按 1 个 slot 计，PD 请求按 prefill+decode 两个 slot 计。

资源组选择顺序（Gateway 数据面与 Scheduler 控制面一致）：

1. `X-InferRouter-Resource-Group`
2. query `resource_group`
3. body 顶层 `resource_group`
4. body `metadata.resource_group`
5. `default`

FastDeploy reward 模型使用同一套资源组机制。上游通过实例注册时的显式 `resource_group` 决定 reward 模型属于哪个资源池，请求侧通过上述 selector 决定转发到哪个资源池；`resource_type` 仅作为实例元数据保留，不参与资源组选择。

资源组生命周期保持隐式，避免新增一组 register/delete API：

- **注册**：`POST/PUT /v1/instances` 带 selector 写入实例时，实例索引中出现该资源组；`POST /v1/steps/start` 带 selector 时创建该资源组的运行时状态。
- **注销**：删除该组全部实例，或 `PUT /v1/instances?resource_group=rg-a` 同步空实例列表；当非 `default` 资源组无实例、phase 为 `IDLE`、无 active 和等待队列时，Scheduler 自动清理其 runtime。
- **查询无副作用**：`GET /v1/steps/current?resource_group=unknown` 返回 `IDLE` 快照，但不会创建资源组。
- **未知组防污染**：只有 `steps/start` 允许隐式创建 runtime；allocate 遇到无实例且无 runtime 的资源组会拒绝，`pause`、`continue`、`end` 遇到不存在的资源组返回 `404`，不会撑大 `s.groups`。
- **default 不清理**：`default` 是旧客户端兼容组，始终保留。
- **未知组不过早拒绝**：gateway 模式下，非 `default` 资源组若本地缓存尚未同步，Gateway 放行到 Scheduler，由 Scheduler event-loop 基于真实资源组状态返回成功或拒绝；避免 default stop 误伤其他租户。

控制面 API 复用既有接口，通过 selector 控制作用域，避免为每个资源组扩展一组嵌套路由：

| API | 语义 |
|-----|------|
| `GET /v1/resource-groups` | 列出已知资源组（runtime + instance index） |
| `GET /v1/instances?resource_group=rg-a` | 查询指定资源组实例；无 selector 时查询全局实例 |
| `POST /v1/instances?resource_group=rg-a` | 向指定资源组追加/更新实例；无 selector 时按实例 `resource_group` 字段注册，未填进入 `default` |
| `PUT /v1/instances?resource_group=rg-a` | 原子同步单个资源组实例，不影响其他组；无 selector 时同步全局实例表 |
| `DELETE /v1/instances?resource_group=rg-a` | 只删除当前资源组内的指定实例；无 selector 时按实例 ID 全局删除 |
| `POST /v1/steps/start?resource_group=rg-a` | 启动资源组推理轮次，可配置独立 policy / queue / max_inflight；无 selector 时操作 `default` |
| `POST /v1/steps/end?resource_group=rg-a` | 停止资源组推理轮次，只 drain 当前组；无 selector 时操作 `default` |
| `POST /v1/steps/pause?resource_group=rg-a` | 暂停当前资源组分配并清空该组等待队列；无 selector 时操作 `default` |
| `POST /v1/steps/continue?resource_group=rg-a` | 恢复当前资源组分配；无 selector 时操作 `default` |
| `GET /v1/steps/current?resource_group=rg-a` | 查询当前资源组 step/pause 状态；无 selector 时查询旧单组视图 |

V2 兼容接口不新增资源组专用 API，而是在现有 `/api/v2/start_infer`、`/api/v2/stop_infer`、`PUT /api/v2/instances`、`/api/v2/session_finish` 和 `/api/v2/chat/completions` 上复用同一套 selector。实现上 v1 HTTP handler 和 v2 adapter 只负责各自的 wire 协议解析与响应格式，底层统一调用 `internal/controlplane` 的 start/end/sync 命令，避免维护两套资源组语义。

V1 `POST /v1/steps/end` 保持非阻塞语义：有在途请求时立即返回 `DRAINING` 和 `pending_requests`。`pending_requests` 只统计 selector 命中的当前资源组；step end 日志用 `normal_pending_requests` / `pd_pending_requests` 拆分当前组 pending，并额外输出 `total_pending_requests` 作为全局参考，避免多租排障时把其他资源组的在途请求误判成当前组无法 IDLE。step end、drain complete 和 event-loop alive 日志还输出 `gateway_allocs_total`、`group_gateway_allocs`、`untracked_active_requests`、`active_by_resource_group` 等字段，用于判断 active allocation 是否缺少 gateway tracking，从而定位 Release 未回传、GatewayID 缺失或 ghost cleanup 无法兜底的问题。V2 `/api/v2/stop_infer` 面向 PaddleRL/rollout-controller 兼容客户端，会在当前资源组进入 `DRAINING` 后继续等待 `DRAINING → IDLE`，再返回成功，避免上游紧接着发下一轮 `/api/v2/start_infer` 时被旧轮次的 drain 状态拒绝。等待只作用于 selector 命中的资源组，不影响其他资源组。

完整调用流程、时序图和接口协议见 [multi-tenant-resource-groups.md](multi-tenant-resource-groups.md)。

### 4.2 批量调度 + 堆排序（解决：万卡级性能）

**旧架构问题**：逐请求 O(N) 扫描所有实例，10w 请求 × 1w 节点 = 10 亿次比较

**新架构方案**：

核心文件：[min_load.go](internal/scheduler/policy/min_load.go)

```
步骤 1：构建最小堆                   O(N_nodes)
步骤 2：逐请求弹出堆顶 + 模拟 Acquire  O(K × log(N_nodes))
总复杂度：O(N + K×logN)
```

- 通过 `BatchSelector` 可选接口实现，**不强制所有策略**
- 堆内模拟 `Acquire`（本地 `active++`），弹出超限节点，保证批内分配一致性
- 未实现 `BatchSelector` 的策略自动 fallback 为逐请求 `Select`

**量化对比**（1w 节点 × 8192 请求）：

| 方式 | 操作次数 | 倍数 |
|------|---------|------|
| 旧：逐请求线性扫描 | 8192 × 10,000 = **8192w** | 1x |
| 新：堆排序批量调度 | 10,000 + 8192 × 14 ≈ **12w** | **680x** |

### 4.3 Step 状态机（解决：训练步骤间隔离）

RL 训练的每一轮 rollout 需要互相隔离。新架构引入了严格的三态状态机：

```
         StartStep(step_id)           EndStep(step_id)
              │                            │
              v                            v
  ┌──────┐       ┌─────────┐       ┌──────────┐
  │ IDLE │──────>│ SERVING │──────>│ DRAINING │
  └──┬───┘       └─────────┘       └────┬─────┘
     ^                                   │
     │       activeCount == 0            │
     └───────────────────────────────────┘
```

核心文件：[server.go:272-329](internal/scheduler/server.go#L272-L329)

**关键行为**：

| 转换 | 触发条件 | 操作 |
|------|---------|------|
| IDLE → SERVING | `StartStep(step_id, policy?)` | 若指定 policy 则重建策略；否则 Reset 当前策略。重置所有 NodeState + 清空 gatewayAllocs + 清空幂等去重缓存 |
| SERVING → DRAINING | `EndStep(step_id)` 且有在途请求 | 停止接受新请求，等待在途完成 |
| SERVING → IDLE | `EndStep(step_id)` 且无在途请求 | 直接回到 IDLE |
| DRAINING → IDLE | 最后一个在途请求 Release | 自动转换，含 Gateway 崩溃后的幽灵负载清理 |

> **解决旧问题**：旧架构无 Step 隔离机制，训练步骤间的负载状态互相污染。

### 4.4 Allocate/Release 幂等性（解决：请求状态不确定）

**问题**：Gateway 与 Scheduler 之间 TCP 连接出现异常时，请求状态不确定——不知道发没发成功，也不知道能不能安全重试：

| 场景 | 后果 |
|------|------|
| Allocate 重试 | 幽灵分配（activeCount 虚高），DRAINING 永远完不成 |
| Release 丢失 | 负载计数泄漏，实例永远显示高负载 |
| Release 重复 | activeCount 下溢，DRAINING 提前结束 |

**设计方案**：引入 `request_id`（Allocate 幂等键）和 `allocation_id`（Release 幂等键），在 Event-Loop 内通过 O(1) map 查找去重，不加锁、不影响性能。

核心文件：[server.go](../internal/scheduler/server.go)

#### 4.4.1 数据流

```
Gateway                                Scheduler Event-Loop
   │                                        │
   │  Allocate(request_id="req-abc")        │
   │───────────────────────────────────────>│
   │                                        │  allocDedup["req-abc"] 未命中
   │                                        │  → Policy.Select → Acquire
   │                                        │  → 生成 allocation_id="s1-a1"
   │                                        │  → 写入 allocDedup["req-abc"]
   │  <── (inst, allocation_id="s1-a1") ────│
   │                                        │
   │  ⚡ TCP 超时，Gateway 不确定是否成功       │
   │                                        │
   │  Allocate(request_id="req-abc") [重试]  │
   │───────────────────────────────────────>│
   │                                        │  allocDedup["req-abc"] 命中！
   │                                        │  → 直接返回缓存（不重复 Acquire）
   │  <── (inst, allocation_id="s1-a1") ────│
   │                                        │
   │  ... 推理完成 ...                        │
   │                                        │
   │  Release(allocation_id="s1-a1")        │
   │───────────────────────────────────────>│
   │                                        │  releaseDedup["s1-a1"] 未命中
   │                                        │  → Release + Feedback
   │                                        │  → 写入 releaseDedup["s1-a1"]
   │                                        │
   │  Release(allocation_id="s1-a1") [重试]  │
   │───────────────────────────────────────>│
   │                                        │  releaseDedup["s1-a1"] 命中！
   │                                        │  → 跳过（不重复 Release）
```

#### 4.4.2 核心机制

**Allocate 去重（request_id）**

```go
// Server 新增字段（Event-Loop only，无锁）
allocCounter uint64                      // 步内递增计数器
allocDedup   map[string]*allocCacheEntry // request_id → 缓存结果
```

- Gateway 在 `requestLifecycle` 中生成 `request_id`：优先使用 `X-Trace-ID` 请求头，否则使用进程级随机前缀 + 原子递增序号。Audit 与 Allocate 共用同一个 `request_id`，非流式重试只追加 `-retryN`
- Event-Loop 中 `batchAllocate` 先查 `allocDedup`：命中则直接返回缓存（不调 Policy、不增 activeCount）
- 未命中则正常走 Policy.Select → Acquire，分配成功后生成 `allocation_id = "s{stepID}-a{counter++}"` 并写入缓存
- **空 request_id 不去重**：无有效键时每次独立分配，防止空字符串共享 `""` 键导致误匹配

**Release 去重（allocation_id）**

```go
// Server 新增字段（Event-Loop only，无锁）
releaseDedup map[string]bool // allocation_id → 已释放
```

- Gateway 将 Allocate 返回的 `allocation_id` 原样传给 Release
- Event-Loop 中 `handleRelease` 先查 `releaseDedup`：命中则跳过（不减 activeCount）
- 未命中则正常处理 Release + Feedback，写入已释放标记
- **空 allocation_id 不去重**：同理防止空键误匹配

**Step 间清理**

`handleStartStep` 每轮开始时重置 `allocCounter`、`allocDedup`、`releaseDedup`，与 `gatewayAllocs` 一起清空。Step 间去重状态互不干扰。

#### 4.4.3 Gateway 侧 Release 重试

Release 是 fire-and-forget 场景，TCP 失败后 Gateway 会在本地重试：

```
Release 失败
   │
   ├── 重试 1（100ms 后）
   ├── 重试 2（200ms 后）
   └── 重试 3（400ms 后）
        │
        └── 全部失败 → 记日志，依赖 Scheduler 的幽灵负载清理兜底
```

最多 3 次指数退避重试（100/200/400ms），上下文取消时提前终止。即使全部失败，Scheduler 侧的 Gateway 心跳超时后会触发 `CleanupGateway` 自动释放全部占用。

#### 4.4.4 Proto 变更

```protobuf
message AllocateRequest {
    ...
    string request_id = 4;     // 幂等键：Gateway 生成
}
message AllocateResponse {
    ...
    string allocation_id = 3;  // 分配标识：Scheduler 生成
}
message ReleaseRequest {
    ...
    string allocation_id = 6;  // 原样回传
}
```

Protobuf 新增字段向后兼容：旧 Gateway 发送空字符串 → 跳过去重，完全走旧路径。

#### 4.4.5 性能影响

| 路径 | 开销 | 10w 请求影响 |
|------|------|-------------|
| allocDedup map 查找 | O(1)，~50ns/次 | 5ms，可忽略 |
| allocation_id 生成（strconv 拼接） | ~100ns/次 | 10ms |
| releaseDedup map 查找 | O(1)，~50ns/次 | 5ms |
| Gateway generateRequestID | ~30ns/次（atomic + rand/v2 进程前缀） | 在 Gateway goroutine，不在 Event-Loop |
| 内存（10w 条 dedup 缓存） | ~15MB | 32GB 预算内，每 Step 清空 |

所有去重操作在 Event-Loop 单线程内完成，无锁、无额外同步开销。

### 4.5 Gateway 自愈注册机制（解决：网络不稳定 + 优雅降级）

**旧架构问题**：Redis/MySQL 故障时服务直接报错，无降级策略，无熔断机制。

**新架构方案**：

核心文件：[scheduler_client.go](internal/gateway/scheduler_client.go)

```
Gateway 启动
    │
    v
 注册（指数退避重试: 500ms → 1s → 2s → ... → 10s cap）
    │
    v
 心跳循环（带 ±20% 抖动，初始随机延迟防惊群）
    │
    ├──成功──→ 更新本地缓存的 StepPhase/StepID
    │
    └──失败──→ 连续失败 ≥ 3 次 ──→ 自动重新注册
```

**补充机制——推拉结合的状态同步**：

| 机制 | 延迟 | 可靠性 | 用途 |
|------|------|--------|------|
| **Pull**：心跳响应携带 StepPhase | 秒级（心跳间隔） | 高（持续运行） | 常态同步 |
| **Push**：StepNotifier HTTP POST 广播 | 毫秒级 | 中（可能丢失） | Step 转换即时通知 |

两者互补：Push 保证关键转换的即时性，Pull 保证最终一致性。

**Scheduler 侧——幽灵负载清理**：

核心文件：[server.go:331-368](internal/scheduler/server.go#L331-L368)

当 Gateway 心跳超时被 `GatewayRegistry` 驱逐时，触发 `CleanupGateway` 事件：

1. 遍历该 Gateway 的所有分配记录（`gatewayAllocs[addr]`）
2. 逐一 Release 对应实例的 `ActiveRequests`
3. 检查 DRAINING 阶段是否可以提前完成

> **解决旧问题**：彻底消除 Gateway 崩溃后的负载计数器漂移问题，无需人工干预。

### 4.6 调度策略可插拔（解决：Bucket 功能散落 + 硬编码）

**旧架构问题**：Bucket 分桶逻辑散落在 4 个文件，开关通过 `os.Getenv` 硬编码在 `init()` 中。

**新架构方案**：

核心文件：[interface.go](internal/scheduler/policy/interface.go), [factory.go](internal/scheduler/policy/factory.go)

```go
// 注册表 + 工厂模式
var builders = map[Name]func(PolicyConfig) Policy{
    "round_robin":       func(_ PolicyConfig) Policy { return NewRoundRobinPolicy() },
    "min_load":          func(_ PolicyConfig) Policy { return NewMinLoadPolicy() },
    "min_request":       func(_ PolicyConfig) Policy { return NewMinRequestPolicy() },
    "session_aware":       func(cfg PolicyConfig) Policy { return NewSessionAwarePolicy(cfg.MaxSessionLoad) },
    "session_aware_v3":  func(cfg PolicyConfig) Policy { return NewSessionAwareV3Policy(cfg.MaxSessionLoad, cfg.LoadDiffThreshold) },
}

// 通过命令行 flag 选择策略
policy.Build(policy.Name(cfg.Policy), policy.PolicyConfig{...})
```

**七种内置策略**：

| 策略 | 适用场景 | 核心逻辑 | 特殊能力 |
|------|---------|---------|---------|
| `round_robin` | 后端性能均匀 | 确定性轮询，跳过已满实例 | — |
| `min_load` | **默认策略**，通用场景 | 综合负载评分 = Active + Waiting×系数 | `BatchSelector`（堆排序） |
| `min_request` | 简单场景，追求最小活跃连接 | 选择 ActiveRequests 最少的实例 | — |
| `session_aware` | 多轮对话，KV 缓存命中率敏感 | 同一 session_id 路由到同一实例（强绑定） | `Resettable`（Step 间清除映射） |
| `session_aware_v3` | Mooncake 全局 KV-Cache 场景 | session 弱绑定 + 请求级负载均衡，动态 stay/migrate 决策 | `BatchSelector` + `Resettable` + `SessionRemover` + `Feedback` |
| `cache_aware` | 前缀缓存命中率敏感（sglang 等） | Radix Tree 追踪实例前缀缓存，前缀匹配率 vs 负载均衡双模式路由 | `BatchSelector` + `Resettable` + `SessionRemover` + `Feedback` |
| `session_aware_v4` | Mooncake 全局 KV-Cache + 负载均衡优先 | V3 session 亲和性 + cache_aware 双模式路由 + 堆位置追踪 + session LRU 驱逐 | `BatchSelector` + `Resettable` + `SessionRemover` + `Feedback` |

**扩展方式**：只需实现 `Policy` 接口并在 `builders` map 中注册，零侵入。

#### 4.6.2 session_aware_v3 策略详解

核心文件：[session_aware_v3.go](internal/scheduler/policy/session_aware_v3.go)

**背景**：`session_aware`（v2）使用强 session-instance 绑定，session 一旦分配到某实例不可迁移。在接入 Mooncake（HiCache L3 分布式缓存层）后，所有推理实例共享全局 KV-Cache 池（RDMA 零拷贝），session 迁移代价从「完全重新 prefill」降为「从 L3 拉取 KV cache」，因此 session 亲和性重要性降低，负载均衡重要性提升。

**与 session_aware 的核心差异**：

| 维度 | session_aware (v2) | session_aware_v3 |
|------|-----------------|-----------------|
| 负载追踪粒度 | sessionLoad（每实例绑定 session 数） | requestLoad（每实例在飞请求数，Select+1/Feedback-1） |
| session 绑定 | 强绑定，分配后不可迁移 | 弱绑定，每个请求动态 CompareAndSchedule |
| 负载均衡效果 | session 级均衡 | 请求级实时均衡 |
| Mooncake 适配 | session 绑死，L3 全局缓存优势浪费 | 自由迁移，充分利用 L3 跨实例共享 |

**CompareAndSchedule 决策逻辑**（每个请求执行）：

```
已有 session 映射：
  1. lastLoad >= MaxSessionLoad      → 迁移到最低负载实例（实例过载）
  2. (lastLoad - minLoad) > LoadDiffThreshold → 迁移（负载差值超阈值）
  3. 否则                             → 留在原实例（缓存亲和）

新 session：
  准入检查 → 分配到最低 requestLoad 实例
```

**配置参数**（通过 `POST /v1/steps/start` 传入）：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `max_session_load` | 100 | 单实例最大承载 session 数，也用于全局准入控制 |
| `load_diff_threshold` | 1 | 负载差值阈值，越小迁移越激进 |

```json
POST /v1/steps/start
{
  "step_id": 42,
  "policy": "session_aware_v3",
  "max_session_load": 32,
  "load_diff_threshold": 2
}
```

**BatchSelect 算法**：O(N + K×logN)，使用内联 min-heap（按 requestLoad 排序）。堆内 "stay" 决策不更新堆序，产生安全偏差（更激进迁移），在 Mooncake 场景下反而有利于利用全局 KV cache。

**Feedback 机制**：`server.go` 的 `handleRelease` 调用 `Feedback(instanceID, costMetrics)` 递减 requestLoad。每个请求结束时自动调用，无需额外配置。

#### 4.6.3 cache_aware 策略详解

核心文件：[cache_aware.go](internal/scheduler/policy/cache_aware.go)、[radixtree.go](internal/scheduler/policy/radixtree.go)

**背景**：`session_aware` 系列策略基于 `session_id → instance_id` 映射做亲和性路由，本质是 session 粒度的绑定。但在 sglang 等支持 **automatic prefix caching** 的推理后端中，真正决定缓存命中率的不是 session 绑定，而是**请求文本的前缀是否已被某个实例缓存在 KV Cache 中**。`cache_aware` 策略参考 sglang model gateway 的 radix tree 算法，使用**近似 radix tree** 追踪每个实例可能缓存了哪些文本前缀，通过前缀匹配率 vs 负载均衡做路由决策。

**与 session_aware 系列的核心差异**：

| 维度 | session_aware / v3 | cache_aware |
|------|-------------------|-------------|
| 路由依据 | session_id 绑定 | 请求文本前缀匹配 |
| 缓存模型 | 假设同 session 有相同前缀 | 精确追踪每个实例的文本前缀 |
| 负载均衡 | session 级 / 请求级 | 请求级，双模式（均衡/不均衡） |
| 适用后端 | 通用 | sglang、vLLM 等支持前缀缓存的后端 |
| 数据结构 | session map | Multi-tenant Radix Tree |

**Multi-Tenant Radix Tree**：

- 核心数据结构，每个节点存储压缩的文本段 + `tenants map[string]uint64`（instance_id → epoch）
- 同一前缀可被多个实例共享——例如 "hello world" 前缀节点可同时记录 instance-A 和 instance-B
- `Insert(text, tenant, epoch)` — 沿路径插入，自动 split 共享前缀节点
- `PrefixMatch(text)` — 返回对输入文本有最深前缀匹配的实例（~100ns/0alloc）
- `EvictBySize(maxBytesPerTenant)` — 全局 LRU 驱逐，按 epoch 从旧到新淘汰
- 子节点索引使用 `[256]*radixNode` 数组，O(1) 查找；节点通过 `sync.Pool` 回收减少 GC

**双模式路由算法**（参考 sglang）：

```
1. 计算所有可用实例的 minLoad, maxLoad

2. 不均衡模式（maxLoad - minLoad > BalanceAbsThreshold 且 maxLoad > BalanceRelThreshold × minLoad）：
   → 选择负载最低的实例（最短队列）
   → 仍更新 radix tree 以维护缓存状态

3. 均衡模式（默认）：
   a. PrefixMatch(requestText) → (tenant, matchRate)
   b. matchRate > CacheThreshold → 路由到匹配的 tenant 实例（缓存命中）
   c. matchRate ≤ CacheThreshold → 路由到 radix tree 中占用字节最少的实例（最多空闲缓存容量）
```

**配置参数**（通过 `POST /v1/steps/start` 传入）：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `cache_threshold` | 0.5 | 前缀匹配率阈值。高于此值认为缓存命中，路由到匹配实例；低于此值路由到最空闲实例 |
| `balance_abs_threshold` | 32 | 负载绝对差值阈值。`maxLoad - minLoad` 超过此值进入不均衡模式 |
| `balance_rel_threshold` | 1.1 | 负载相对比例阈值。`maxLoad > balance_rel_threshold × minLoad` 进入不均衡模式 |
| `max_tree_size` | 10000 | 每个 tenant 在 radix tree 中最大占用字节数，超过后 LRU 驱逐 |
| `eviction_interval_sec` | 60 | 驱逐检查间隔（秒），避免每次 Select 都触发驱逐 |
| `max_session_load` | 100 | 单实例最大在飞请求数（复用 session_aware 参数名） |

```json
POST /v1/steps/start
{
  "step_id": 42,
  "policy": "cache_aware",
  "cache_threshold": 0.5,
  "balance_abs_threshold": 32,
  "balance_rel_threshold": 1.1,
  "max_tree_size": 10000,
  "eviction_interval_sec": 60,
  "max_session_load": 100
}
```

**参数调优建议**：

| 场景 | 推荐参数 |
|------|---------|
| 高前缀重复率（RL rollout） | `cache_threshold: 0.3`（更激进匹配），`max_tree_size: 50000` |
| 后端实例数少（<8） | `balance_abs_threshold: 8`，更早进入不均衡模式避免热点 |
| 后端实例数多（>64） | `balance_abs_threshold: 64`，`eviction_interval_sec: 30`（更频繁驱逐） |
| 短文本请求为主 | `cache_threshold: 0.7`（短文本匹配率波动大，提高阈值减少误判） |

**Stale Tenant 容错**：当 PrefixMatch 返回高匹配率但对应 tenant 实例不可用（已下线、熔断或过载）时，策略会：
1. 调用 `tree.RemoveTenant(staleTenant)` 清除该实例在 radix tree 中的所有记录
2. 回退到 `selectMinTreeBytes` 路由到最空闲实例
3. 这保证实例故障不会导致缓存亲和性"卡死"在已失效的实例上

**BatchSelect 算法**：

- 不均衡模式：O(N + K×logN)，使用内联 min-heap（按 requestLoad 排序），与 session_aware_v3 相同模式。每次分配后仍将文本 Insert 到 tree 以维护缓存状态
- 均衡模式：O(N + K×textLen)，逐请求调用 `selectByPrefixLocked` 做前缀匹配
- 两种模式共享同一个 epoch（`nextEpoch()` 原子递增），避免批次内每个请求都调用 atomic
- `resultsBuf` 和 `heapBuf` 在 event-loop 中复用，零分配

**驱逐机制**：驱逐不基于真实时间，而基于 epoch 增量（`EvictionIntervalSec × 100` 个 epoch 触发一次）。每个 `Select` / `BatchSelect` 调用末尾检查 `currentEpoch - lastEvictionEpoch` 是否超过阈值，超过则调用 `tree.EvictBySize(MaxTreeSize)` 按 LRU 淘汰每个 tenant 超出上限的旧叶子节点。这避免了引入后台 goroutine 和时间依赖，保持 event-loop 串行化模型的简洁性。

**并发模型**：

- `Select` / `BatchSelect` / `Feedback` / `Reset` 均持 `sync.Mutex`
- `Select` 和 `BatchSelect` 通常由 event-loop 串行调用；`Reset` 可能来自 `handleStartStep` 的另一个 goroutine
- `resultsBuf` / `heapBuf` 仅在 event-loop 单线程中复用，不需要额外同步
- radix tree 自身不持锁，由外层 `CacheAwarePolicy.mu` 保护

**RequestText 提取**：gateway 层从 chat completion 请求体中自动提取 `messages[].content` 拼接文本，截断到 4096 字节，设置到 `RouteContext.RequestText`。gRPC `AllocateRequest` 中对应字段为 `request_text`。如果文本为空，回退到最小 tree 字节实例路由。

**Feedback 机制**：与 session_aware_v3 一致，`Feedback(instanceID, costMetrics)` 递减 requestLoad。当 `requestLoad` 降为 0 时直接从 map 中删除 key（避免 map 膨胀）。每个请求结束时自动调用，无需额外配置。

**性能基准**（benchmark 参考数据）：

| 操作 | 耗时 | 分配 |
|------|------|------|
| `RadixTree.PrefixMatch` | ~100 ns/op | 0 alloc |
| `RadixTree.Insert`（短文本） | ~200 ns/op | 0-1 alloc |
| `CacheAwarePolicy.Select`（均衡高匹配） | ~2 μs/op | 0 alloc |
| `CacheAwarePolicy.Select`（不均衡） | ~1 μs/op | 0 alloc |
| `CacheAwarePolicy.BatchSelect`（100 请求，均衡） | ~55 μs/op | 0 alloc |

#### 4.6.4 session_aware_v4 策略详解

核心文件：[session_aware_v4.go](internal/scheduler/policy/session_aware_v4.go)

**背景**：`session_aware_v3` 在 Mooncake 全局 KV-Cache 场景下表现良好，但存在以下导致负载不均和长尾延迟的痛点：

1. **无全局不均衡检测**：每个请求独立做局部比较（`lastLoad - minLoad > LoadDiffThreshold`），无法感知整体负载形态。当集群出现严重倾斜时（如 step 切换导致部分实例积压），仍然逐请求缓慢迁移
2. **BatchSelect 堆失真**：session "stay" 决策不更新堆条目，大批次中累积偏差导致 min-load 估计失准，部分请求在应该迁移时选择留下
3. **session 映射无过期**：`sessionAssign` 在 step 内无限增长，长时间不活跃的 session 占据映射但 KV-Cache 早已冷却，亲和性毫无价值
4. **逐个清理失效映射**：实例下线后，每个受影响 session 独立发现 stale mapping 并逐一扫描 `findMinLoadLocked`，最坏 O(K×N)

**V4 在 V3 基础上的改进**：

| 维度 | session_aware_v3 | session_aware_v4 |
|------|-----------------|-----------------|
| 不均衡感知 | 无（逐请求局部比较） | `isImbalanced()` 全局双阈值检测 |
| 不均衡路由 | 无（始终走 stay/migrate） | 模式 A：全部走最短队列，忽略亲和性 |
| 迁移门槛 | 固定 `LoadDiffThreshold` | warmth-based 动态门槛：`LoadDiffThreshold + min(requestCount, SessionAffinityBoost)` |
| BatchSelect 堆准确性 | stay 不更新堆（安全偏差） | `heapIndexBuf` 堆位置追踪，stay 同步更新堆序 |
| session 过期 | 无（仅 Reset 清除） | `maybeEvictSessionsLocked` epoch-based LRU 驱逐 |
| 死实例清理 | 逐个发现、逐个扫描 | `invalidateInstanceSessionsLocked` 批量清理 + `deadInstCache` 去重 |

**双模式路由算法**：

```
1. 计算所有可用实例的 minLoad, maxLoad（O(N)）

2. 不均衡模式（isImbalanced = maxLoad - minLoad > BalanceAbsThreshold 且 maxLoad > BalanceRelThreshold × minLoad）：
   → 所有请求路由到负载最低的实例（最短队列）
   → 完全忽略 session 亲和性和 warmth
   → 仍更新 sessionAssign（为下一批次恢复亲和性做准备），requestCount 重置为 0

3. 均衡模式（默认）：
   a. 已有 session 映射且实例存活：
      - 计算 effectiveThreshold = LoadDiffThreshold + min(requestCount, SessionAffinityBoost)
      - lastLoad >= MaxSessionLoad → 强制迁移（requestCount 重置为 0）
      - lastLoad - minLoad > effectiveThreshold → 迁移（requestCount 重置为 0）
      - 否则 → stay（requestCount++）
   b. stay 决策时更新 heapIndex 中对应堆条目的 load，维护堆性质
   c. 新 session：准入控制 → 分配到 min-load 实例（requestCount = 0）
```

**堆位置追踪**（BatchSelect 均衡模式核心改进）：

V3 的 BatchSelect 中，"stay" 决策仅递增 `requestLoad[instanceID]` 但不更新堆条目的 load 值。随着批次处理推进，堆顶 min-load 可能已过时——某个实例实际负载很高但堆中仍显示低负载。V4 通过 `heapIndexBuf map[string]int` 维护 instance_id → 堆位置索引：

- 每次 stay：`requestLoad[id]++` → `heap[idx].load++` → `v4HeapDownIdx` 维护堆性质
- 每次 migrate/new：`heap[0].load++` → `v4HeapDownIdx(0)` 向下调整
- 每次 heap swap：同步更新 `heapIndexBuf` 中两个元素的位置
- 结果：批次中每个请求看到的 heap[0] 始终是真正的最小负载实例

**Session LRU 驱逐**：

每次 `Select`/`BatchSelect` 调用末尾检查 `currentEpoch - lastEvictionEpoch >= SessionEvictEpochs`，超过则遍历 `sessionAssign`，删除 `lastEpoch < currentEpoch - SessionEvictEpochs` 的冷 session。默认 `SessionEvictEpochs=6000`（约 60s@100req/s），避免 session 映射在长 step 中无限增长。设置为 0 禁用驱逐。

**批量失效**：

当 BatchSelect 发现某 session 映射到已下线实例时，调用 `invalidateInstanceSessionsLocked(instanceID)` 一次性清理该实例的所有 session 映射，并在 `deadInstCache` 中缓存已清理的死实例 ID。同一批次中后续遇到相同死实例时直接跳过扫描，避免 O(K×N) 退化。

**Session Warmth 亲和性增强**：

借鉴 sglang cache-aware 路由的思想（prefix cache 越热越不愿迁移），V4 通过 `requestCount` 追踪每个 session 在当前实例上的连续请求数，动态提高迁移门槛：

```
effectiveThreshold = LoadDiffThreshold + min(requestCount, SessionAffinityBoost)
```

- **新 session / 刚迁移**：`requestCount=0`，`effectiveThreshold = LoadDiffThreshold`（最容易迁移）
- **warm session**：随着请求累积，`requestCount` 增长，迁移门槛逐渐提高
- **cap**：`requestCount` 的贡献上限为 `SessionAffinityBoost`（默认 8），避免极端粘性

实际效果（假设 `LoadDiffThreshold=1`, `SessionAffinityBoost=8`）：

| requestCount | effectiveThreshold | 行为 |
|---|---|---|
| 0（刚迁移） | 1 | load diff > 1 即迁移（最灵活） |
| 3（中等） | 4 | 能容忍一定的负载差异 |
| 8+（很热） | 9 | 只有明显不均衡才迁移（最粘） |

这确保了：
1. **冷 session 快速均衡**：新创建或刚迁移的 session 几乎不附加额外门槛，能快速响应负载变化
2. **热 session 保持亲和**：已在实例上累积大量请求的 session（对应 sglang 中高 prefix match rate 的场景），拥有更高的迁移阻力，减少不必要的 cache 失效
3. **迁移即重置**：session 迁移到新实例后 `requestCount` 清零，重新从"冷"状态开始
4. **不均衡模式不受影响**：当集群整体负载严重不均衡时（`isImbalanced()=true`），所有 session 无条件路由到最低负载实例，warmth 不生效

**配置参数**（通过 `POST /v1/steps/start` 传入）：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `max_session_load` | 100 | 单实例最大在飞请求数 |
| `load_diff_threshold` | 1 | CompareAndSchedule 迁移灵敏度（`lastLoad - minLoad > 此值` 触发迁移） |
| `balance_abs_threshold` | 32 | 负载绝对差值阈值。`maxLoad - minLoad` 超过此值进入不均衡模式 |
| `balance_rel_threshold` | 1.1 | 负载相对比例阈值。`maxLoad > balance_rel_threshold × minLoad` 进入不均衡模式 |
| `session_evict_epochs` | 6000 | session LRU 驱逐间隔（epoch 数）。约 60s@100req/s，设为 0 禁用 |
| `session_affinity_boost` | 8 | Session warmth 迁移门槛增量上限。warm session 的 `effectiveThreshold = load_diff_threshold + min(requestCount, 此值)`。设为 0 禁用 warmth 机制 |

```json
POST /v1/steps/start
{
  "step_id": 42,
  "policy": "session_aware_v4",
  "max_session_load": 100,
  "load_diff_threshold": 1,
  "balance_abs_threshold": 32,
  "balance_rel_threshold": 1.1,
  "session_evict_epochs": 6000,
  "session_affinity_boost": 8
}
```

**参数调优建议**：

| 场景 | 推荐参数 |
|------|---------|
| Mooncake 通用场景 | 默认值即可，V4 自动在均衡/不均衡模式间切换 |
| 后端实例数少（<8） | `balance_abs_threshold: 8`，更早进入不均衡模式避免热点 |
| 后端实例数多（>64） | `balance_abs_threshold: 64`，避免频繁切换模式 |
| 长 step（>10min） | `session_evict_epochs: 3000`（更积极驱逐冷 session） |
| 短 step（<1min） | `session_evict_epochs: 0`（禁用驱逐，step 间 Reset 已清理） |
| 高 QPS（>1000req/s） | `balance_rel_threshold: 1.05`，对不均衡更敏感 |
| 高 cache 命中率场景 | `session_affinity_boost: 16`，更强亲和性减少 cache 失效 |
| 无 cache 纯负载均衡 | `session_affinity_boost: 0`，禁用 warmth，退化为 V3 行为 |

**Feedback 机制**：与 V3 一致，`Feedback(instanceID, costMetrics)` 递减 requestLoad。当 `requestLoad` 降为 0 时直接从 map 中删除 key（避免 map 膨胀）。每个请求结束时自动调用，无需额外配置。

**并发模型**：

- `Select` / `BatchSelect` / `Feedback` / `Reset` / `RemoveSession` 均持 `sync.Mutex`
- `Select` 和 `BatchSelect` 通常由 event-loop 串行调用；`Reset` / `RemoveSession` 可能来自其他 goroutine
- `resultsBuf` / `heapBuf` / `heapIndexBuf` 仅在 event-loop 单线程中复用，不需要额外同步

**性能基准**（benchmark 参考数据，100 nodes / 128 batch）：

| 操作 | 耗时 | 分配 |
|------|------|------|
| `BatchSelect AllStay`（100 nodes） | ~25 μs/op | 0 alloc |
| `BatchSelect AllMigrate`（100 nodes） | ~18 μs/op | 0 alloc |
| `BatchSelect Imbalanced`（100 nodes） | ~12 μs/op | 0 alloc |
| `Select Hit Stay`（100 nodes） | ~600 ns/op | 0 alloc |
| `Select Miss`（100 nodes） | ~600 ns/op | 0 alloc |

#### 4.6.1 每轮动态切换策略

每轮 Step 开始时，训练框架可通过 `POST /v1/steps/start` 的 `policy` 字段指定本轮使用的调度策略：

```json
// 指定本轮使用 round_robin 策略
POST /v1/steps/start
{"step_id": 42, "policy": "round_robin"}

// 不指定 policy，保持当前策略（仅 Reset）
POST /v1/steps/start
{"step_id": 43}
```

**行为规则**：

| `policy` 字段 | 行为 |
|---------------|------|
| 非空（如 `"min_load"`） | 通过 `policy.Build()` 重建全新策略实例，替换当前策略 |
| 空或不传 | 保持当前策略不变，若策略实现了 `Resettable` 接口则调用 `Reset()` 清除内部状态 |
| 无效名称 | 返回错误（HTTP 409），Step 不启动 |

**响应**中回显实际使用的 `policy`（仅在显式指定时），便于调用方确认：

```json
{"success": true, "step_id": 42, "phase": "SERVING", "policy": "round_robin"}
```

**设计动机**：RL 训练不同阶段可能需要不同的调度策略。例如 warmup 阶段使用 `round_robin` 均匀预热，正式训练阶段切换为 `min_load` 追求极致均衡。该设计保持向后兼容——不传 `policy` 时行为与原来完全一致。

### 4.6.5 session_aware_v5 策略详解

核心文件：[session_aware_v5.go](internal/scheduler/policy/session_aware_v5.go)

**背景**：`session_aware_v4` 结合了 V3 的 session 亲和性与 cache_aware 的双模式负载均衡，在 Mooncake 场景下表现良好。然而在**多轮 SWE 场景**下存在以下痛点：

1. **load 维度单一**：`lastLoad` 和 `heap[0].load` 仅考虑 in-flight request 数量，忽略了 GPU Block 压力和等待队列深度
2. **缺乏紧急迁移**：当实例 GPU blocks 严重不足（影响推理延迟）时，仍需等待 `load_diff_threshold` 超过才触发迁移
3. **累积陈旧**：STAY 决策只更新 requestLoad 但不更新 heap entry，导致堆中累积陈旧分数
4. **无 metadata 接口**：无法接收 `available_gpu_block_num` 和 `num_requests_waiting` 指标

**核心改进**：

| 维度 | session_aware_v3 | session_aware_v4 | session_aware_v5 |
|------|-------------------|-------------------|-------------------|
| 负载评分 | requestLoad 单一维度 | requestLoad 单一维度 | **4 维度加权评分**（load + waiting + block_pressure + drift） |
| GPU 感知 | 无 | 无 | **MetaUpdater + DriftAware** |
| Block 门控 | 无 | 无 | **BlockMinThreshold 阻止新 session 分配到低 block 节点** |
| 紧急迁移 | 仅超载触发 | 仅超载触发 | **BlockEmergencyThresh 强制迁移** |
| Heap 陈旧 | 无累积问题 | STAY 仅更新 load，不更新 heap | **STAY 原地更新 heap 分数** |
| 并发模型 | mutex 保护所有操作 | mutex 保护所有操作 | **event-loop 单线程写 + atomic 读取** |

#### 4.6.5.1 核心数据结构

```go
type SessionAwareV5Policy struct {
    mu sync.Mutex

    // Session affinity (复用 V4 结构)
    sessionAssign  map[string]sessionEntry  // session_id -> {instance_id, lastEpoch, requestCount}
    requestLoad   map[string]int64          // instance_id -> current in-flight requests
    sessionCount  int64                   // global active session count
    deadInstCache map[string]struct{}      // batch invalidation dedup

    // Atomic instance metadata (复用 MinLoad 模式)
    metaPtr atomic.Pointer[v5InstanceMetaMap]  // lock-free reads for event-loop

    // 配置项
    MaxSessionLoad       int64   // default 32 (SWE 场景)
    LoadDiffThreshold    int64   // default 2
    BalanceAbsThreshold  int64   // default 32
    BalanceRelThreshold  float64 // default 1.1
    SessionEvictEpochs   uint64  // default 6000
    SessionAffinityBoost int64   // default 8

    // Block-aware 调度配置
    BlockMinThreshold    int64   // default 256: 新 session 门控阈值
    BlockEmergencyThresh int64   // default 128: 紧急迁移阈值

    // 复合评分权重（和为 1.0）
    WeightRequestLoad    float64 // default 0.3
    WeightWaitingCount   float64 // default 0.3
    WeightBlockPressure  float64 // default 0.3
    WeightDrift          float64 // default 0.1
}

type v5InstanceMeta struct {
    waitingCount    atomic.Int64  // from MetricsCollector
    availableBlocks atomic.Int64  // from MetricsCollector
    totalBlocks    atomic.Int64  // calculated from available + cache_usage
    localDrift      atomic.Int64  // compensated by TrackAcquire/TrackRelease
}
```

#### 4.6.5.2 复合评分公式

V5 使用 4 维度加权评分来选择最优节点：

```
score = W_load * norm_load + W_waiting * norm_waiting + W_block * norm_block + W_drift * norm_drift

其中：
- norm_load = active_requests / MaxSessionLoad                        [0, 1]
- norm_waiting = (waiting_count + drift) / MaxSessionLoad              [0, 1]
- norm_block = 1 - available_blocks / total_blocks                     [0, 1]  # 1 = 无可用 block
- norm_drift = drift / MaxSessionLoad                                [0, 1]
```

**归一化策略**：
- 新 session 首次请求：使用全局 min/max bounds 计算相对得分
- 后续请求：复用上一次 bounds 避免重复计算（O(1)）

**默认权重配置**（针对 SWE 场景）：
- `weight_request_load = 0.3`：优先考虑实例当前负载
- `weight_waiting_count = 0.3`：优先考虑排队深度
- `weight_block_pressure = 0.3`：优先考虑 GPU block 压力
- `weight_drift = 0.1`：补偿指标轮询延迟

#### 4.6.5.3 Block-aware 门控与紧急迁移

**新 session 门控**（BlockMinThreshold = 256）：

```go
// 阻止新 session 分配到低 block 节点
if meta.availableBlocks.Load() < p.BlockMinThreshold {
    // 尝试下一个最优节点
    // 如果所有节点都门控，返回 ErrAllExceedSession
}
```

**紧急迁移**（BlockEmergencyThresh = 128）：

```go
// 检查 session 所在节点是否处于紧急状态
if p.isBlockEmergency(entry.instanceID) {
    // 不考虑 session 亲和性，强制迁移到非当前、非 gated 的最优节点
    // 适用于：GPU blocks 即将耗尽导致推理延迟飙升
}
func (p *SessionAwareV5Policy) isBlockEmergency(instanceID string) bool {
    metaSnap := p.metaPtr.Load()
    if meta, ok := (*metaSnap)[instanceID]; ok {
        return meta.availableBlocks.Load() <= p.BlockEmergencyThresh
    }
    return false
}
```

紧急迁移不会把 session 重新分配回当前告急实例；目标节点必须可用、未超过 `MaxSessionLoad`，且 metadata 存在时 `available_blocks >= BlockMinThreshold`。当没有安全目标时返回 `ErrAllOverloaded`，避免继续向已耗尽 KV blocks 的实例压流量。

#### 4.6.5.4 Session Warmth 机制

复用 V4 的 requestCount 机制，Warm session 可以容忍更多负载不均：

```go
// 基础阈值 + Warmth Boost
effectiveThreshold := p.LoadDiffThreshold + min(requestCount, p.SessionAffinityBoost)

// 冷 session: requestCount = 0 -> threshold = 2
// 温 session: requestCount = 8 -> threshold = 10
// 热 session: requestCount >= 8 -> threshold = 10
```

#### 4.6.5.5 原子元数据 COW 模式

复用 MinLoad 的 COW（Copy-On-Write）模式，实现零锁读取和高频写入：

```go
type instanceMetaMap = map[string]*v5InstanceMeta

func (p *SessionAwareV5Policy) BatchUpdateInstanceMeta(updates []InstanceMetaUpdate) {
    curPtr := p.metaPtr.Load()
    cur := *curPtr

    // 快速路径：所有 key 都已存在 -> 原地 atomic 写入
    needRebuild := false
    for i := range updates {
        if _, ok := cur[updates[i].InstanceID]; !ok {
            needRebuild = true
            break
        }
    }

    if needRebuild {
        // 低频路径：map 结构变化 -> COW 交换
        newMeta := make(v5InstanceMetaMap, len(cur) + len(updates))
        for k, v := range cur {
            newMeta[k] = v  // 共享指针（atomic 字段并发安全）
        }
        for i := range updates {
            u := &updates[i]
            if meta, ok := newMeta[u.InstanceID]; ok {
                meta.waitingCount.Store(u.WaitingCount)
                meta.availableBlocks.Store(u.AvailableBlocks)
                if u.TotalBlocks > 0 {
                    meta.totalBlocks.Store(u.TotalBlocks)
                }
                meta.localDrift.Store(0)  // 指标刷新 -> 校准重置
            } else {
                m := &v5InstanceMeta{}
                m.waitingCount.Store(u.WaitingCount)
                m.availableBlocks.Store(u.AvailableBlocks)
                m.totalBlocks.Store(u.TotalBlocks)
                newMeta[u.InstanceID] = m
            }
        }
        p.metaPtr.Store(&newMeta)  // 原子交换
    } else {
        // 高频路径：原地更新 -> 零分配
        for i := range updates {
            u := &updates[i]
            if meta, ok := cur[u.InstanceID]; ok {
                meta.waitingCount.Store(u.WaitingCount)
                meta.availableBlocks.Store(u.AvailableBlocks)
                if u.TotalBlocks > 0 {
                    meta.totalBlocks.Store(u.TotalBlocks)
                }
                meta.localDrift.Store(0)
            }
        }
    }
}
```

#### 4.6.5.6 Drift 补偿机制

MetricsCollector 每 500ms 轮询一次指标，Event-Loop 可能在此期间持续分配/释放请求。Drift 补偿机制让 V5 能够感知实时负载变化：

```go
// 分配后增加 drift
func (p *SessionAwareV5Policy) TrackAcquire(instanceID string) {
    if metaMap := p.metaPtr.Load(); metaMap != nil {
        if meta, ok := (*metaMap)[instanceID]; ok {
            meta.localDrift.Add(1)
        }
    }
}

// 释放后减少 drift
func (p *SessionAwareV5Policy) TrackRelease(instanceID string) {
    if metaMap := p.metaPtr.Load(); metaMap != nil {
        if meta, ok := (*metaMap)[instanceID]; ok {
            meta.localDrift.Add(-1)
        }
    }
}

// 使用 drift 修正的等待计数
waiting := max(int64(0), meta.waitingCount.Load() + meta.localDrift.Load())
```

#### 4.6.5.7 Heap 原地更新（STAY 分支）

复用 V4 的 heapIndexBuf 机制，STAY 决策时原地更新堆中节点的分数：

```go
// STAY 分支：原地更新 heap[heapIndex[instanceID]]
heap[heapIndex[entry.instanceID]].compositeScore = calculateCompositeScore(...)
v5HeapDownIdx(heap, idx, len(heap), heapIndex)
v5HeapUpIdx(heap, idx, heapIndex)
```

避免了 V3 中每次 STAY 都创建新 heap 的问题，复杂度从 O(K×N) 降到 O(N + K×logN)。

#### 4.6.5.8 分支计数

| 分支 | 触发条件 | 说明 |
|------|-----------|------|
| `stay` | session 亲和且负载均衡 | session cache 命中 |
| `migrate_overload` | instance 负载 >= MaxSessionLoad | 超载触发迁移 |
| `migrate_score_diff` | (lastScore - bestScore) > effectiveThreshold | 评分差异触发迁移 |
| `migrate_emergency` | available_blocks <= BlockEmergencyThresh | 紧急状态强制迁移 |
| `imbalanced` | 集群负载不均 | 进入 imbalanced 模式，所有请求路由到最优节点 |
| `new_session` | session 不存在 | 新 session 分配 |
| `stale_reassign` | session 绑定实例不存在 | 实例下线触发重新分配 |
| `no_session_fallback` | 无 session ID | 非会话请求路由到最优节点 |
| `block_gated` | 新 session 且最优节点 block < BlockMinThreshold | 门控拒绝，尝试次优节点 |

#### 4.6.5.9 配置参数

| 参数 | 默认值 | 说明 |
|------|---------|------|
| `max_session_load` | 32 | 单实例最大在飞请求数（SWE 场景默认值） |
| `load_diff_threshold` | 2 | 迁移敏感度，warm session 可额外增加 SessionAffinityBoost |
| `balance_abs_threshold` | 32 | 绝对负载差阈值，超过进入不均衡模式 |
| `balance_rel_threshold` | 1.1 | 相对负载比阈值，超过进入不均衡模式 |
| `session_evict_epochs` | 6000 | Session LRU 驱逐间隔（约 60s @ 100 req/s） |
| `session_affinity_boost` | 8 | Warm session 最大额外迁移阈值 |
| `block_min_threshold` | 256 | 新 session 门控，低于此值的节点不接受新 session |
| `block_emergency_thresh` | 128 | 紧急迁移阈值，低于此值强制迁移 |
| `weight_request_load` | 0.3 | requestLoad 权重 |
| `weight_waiting_count` | 0.3 | waitingCount 权重 |
| `weight_block_pressure` | 0.3 | blockPressure 权重 |
| `weight_drift` | 0.1 | drift 权重 |

**参数调优建议**：

| 场景 | 推荐参数 |
|------|-----------|
| SWE 通用场景 | 默认值即可 |
| 后端实例少（<8） | `balance_abs_threshold: 8`，更早进入不均衡模式 |
| 后端实例多（>64） | `balance_abs_threshold: 64`，避免频繁切换模式 |
| 高 cache 命中率 | `session_affinity_boost: 16`，更强 session 亲和性 |
| 低 cache 命中率 | `session_affinity_boost: 0`，退化为 V3 行为 |

#### 4.6.5.10 并发模型

- **Select/BatchSelect/Feedback**：由 event-loop 单线程调用，无需额外同步
- **Reset/RemoveSession**：可能来自其他 goroutine，使用 `mu` 保护
- **BatchUpdateInstanceMeta**：由 MetricsCollector goroutine 调用，通过 COW + atomic 实现无锁读取
- **TrackAcquire/TrackRelease**：由 event-loop 调用，通过 atomic.Add 实现 lock-free 更新

**关键不变量**：
- `resultsBuf`、`heapBuf`、`heapIndexBuf` 仅在 event-loop 中使用，无需并发保护
- `metaPtr` 指向的 map 及其元素（v5InstanceMeta）使用 atomic.Int64，允许并发读写
- `sessionAssign`、`requestLoad`、`deadInstCache` 由 `mu` 保护

#### 4.6.5.11 MetricsCollector 集成

V5 实现 `MetaUpdater` 接口，接收 MetricsCollector 的批量更新：

```go
// FastDeploy /metrics 端点返回
num_requests_waiting 10
available_gpu_block_num 500
gpu_cache_usage_perc 70.5  // 新增指标

// Collector 计算总 blocks
total_blocks = int64(available / (1.0 - cache_usage_perc / 100.0))
           = int64(500 / 0.295) = 1694
```

```go
// InstanceMetaUpdate 结构体扩展
type InstanceMetaUpdate struct {
    InstanceID      string
    WaitingCount    int64
    AvailableBlocks int64
    AvgIOLength     int64
    TotalBlocks     int64  // 新增字段
}
```

#### 4.6.5.12 性能基准

参考 V4 的性能数据，V5 的额外开销主要来自：

| 操作 | V4 基准 | V5 开销 | 说明 |
|------|-----------|---------|------|
| 复合评分计算 | O(1) | +~20ns | 4 维度加权计算 |
| 归一化 bounds 计算 | - | +~50μs (每批次) | 单次扫描，批次内复用 |
| COW 交换 | - | 极少 (仅新实例) | 大多数更新走原地写入 |

**预期性能**（100 nodes / 128 batch）：
- `BatchSelect`: ~14 μs/op（V4 基准）
- `Select`: ~600 ns/op（无 session 时）

### 4.7 多推理端点支持

OneRouter 作为透明代理同时支持多种推理端点，参考 SGLang Rust Router 的设计——不做请求/响应格式转换，仅做路径保持的透明转发 + 负载均衡。

| 端点 | 后端路径 | 协议 | 用途 |
|------|---------|------|------|
| `POST /v1/chat/completions` | `/v1/chat/completions` | OpenAI Chat Completions | 对话补全，训练框架默认路径 |
| `POST /v1/completions` | `/v1/completions` | OpenAI Completions | 文本补全（非对话模式） |
| `POST /generate` | `/generate` | SGLang Native | RL 训练场景（Slime/veRL），传 `input_ids` + `sampling_params` |
| `POST /v1/reward` | `/v1/reward` | FastDeploy Reward | reward 模型打分；资源池由上游注册与请求 selector 决定 |

这些端点共享同一套 **allocate → forward → release** 生命周期，通过 `handleInferenceRequest(w, r, backendPath)` 或专用 handler 统一处理：

- **Streaming**：`httputil.ReverseProxy` 天然保持 `r.URL.Path`，无需额外处理
- **Non-streaming**：`buildNonStreamURL(endpoint, backendPath)` 构建正确的后端 URL
- **Token 用量提取**：`/generate` 使用 `meta_info.prompt_tokens`/`meta_info.completion_tokens`；其余使用 OpenAI `usage` 字段
- **Reward 转发**：`/v1/reward` 走 buffered non-stream 路径，保留请求方法、鉴权 header 和 JSON body，后端响应原样返回

### 4.8 Chat Completions 流式/非流式双路径设计

参考 sglang router 的成熟设计，OneRouter 对所有推理端点（`/v1/chat/completions`、`/v1/completions`、`/generate`）和 `/api/v2/chat/completions` 接口实现了流式（SSE）和非流式两条差异化处理路径。
`/v1/reward` 当前按 FastDeploy reward API 的非流式响应模型接入，复用 buffered non-stream 的重试、审计和释放逻辑。

#### 4.8.1 问题背景

| # | 问题 | 影响 |
|---|------|------|
| 1 | 不区分 stream/non-stream，所有请求走 `httputil.ReverseProxy` | 无法针对非流式做缓冲+重试 |
| 2 | 无 TTFT（Time to First Token）指标 | 最关键的 LLM Serving 指标缺失 |
| 3 | 无 `[DONE]` 检测 | 无法区分流正常结束 vs 中断 |
| 4 | 无 token usage 提取 | Scheduler 缺少 token 级调度信息 |
| 5 | 非流式无后端重试 | 可重试场景直接返回 502 |
| 6 | V2 client disconnect 不取消后端 | `DrainBody` 阻塞数分钟，GPU 空转 |
| 7 | 错误响应为纯文本 | 不兼容 OpenAI JSON 格式 |

#### 4.8.2 架构总览

```
HandleChatCompletion (V1)                    HandleV2ChatCompletion (V2)
│                                            │
├── io.ReadAll → bodyBytes                   ├── io.ReadAll (已有)
├── parseStreamField(bodyBytes)              ├── v2ChatMeta.Stream
│                                            │
├── stream=false ──────────┐                 ├── !stream && !needACK ─┐
│   handleNonStreamChat()  │                 │   handleNonStreamChat() │
│   ├── allocate           │                 │   (共享 V1 路径)        │
│   ├── doNonStreamForward │                 │                         │
│   │   └── http.Client.Do │                 │                         │
│   │   └── extractUsage   │                 │                         │
│   ├── retry (max 2)      │                 │                         │
│   └── release (tokens)   │                 │                         │
│                          │                 │                         │
├── stream=true ───────────┤                 ├── stream || needACK ────┤
│   handleStreamChat()     │                 │   (needACK: ACK 路径)   │
│   ├── allocate           │                 │   (!needACK: stream)    │
│   ├── streamTracker(w)   │                 │                         │
│   │   ├── TTFT           │                 │                         │
│   │   ├── [DONE] 检测    │                 │                         │
│   │   └── chunks/bytes   │                 │                         │
│   ├── proxy.Forward      │                 │                         │
│   └── release + metrics  │                 │                         │
└──────────────────────────┘                 └─────────────────────────┘
```

#### 4.8.3 关键组件

**`parseStreamField`**：零反序列化 JSON 扫描器，跟踪引号状态和大括号深度，在 O(n) 内找到顶层 `"stream":true/false`，避免误匹配 message content 中的嵌套字符串。

**`streamTracker`**：包装 `http.ResponseWriter` 的零缓冲 SSE 状态追踪器：
- TTFT 记录：首次 `Write` 时记录时间戳
- `data: [DONE]` 检测：每次 `Write` 用 `bytes.Contains` 扫描（SSE chunk 通常几百字节，开销 <1μs）
- chunks/bytes 计数：`Flush` 调用计数 ≈ SSE chunk 数
- 实现 `http.Flusher` + `Unwrap()`，不破坏透明代理

**`doNonStreamForward`**：使用 `http.Client.Do()` 独立发送请求并缓冲完整响应，支持 `extractUsage` 提取 token 用量。使用 `sync.Pool<bytes.Buffer>` 池化响应缓冲（64KB 初始，>256KB 不回池）。

**`extractUsage`**：使用只含 `Usage` 字段的小结构体反序列化，避免解析完整 OpenAI response。

#### 4.8.4 非流式重试策略

- 最多 2 次重试（共 3 次尝试），每次尝试重新 Allocate 不同实例
- **可重试**：连接拒绝、连接超时、HTTP 502/503
- **不重试**：400/401/403/404/429/500（直接透传 backend 响应给 client）
- 每次重试前 release 当前 allocation（带 error_code）
- requestID 加 `-retryN` 后缀避免 Allocate dedup 冲突

#### 4.8.5 V2 ACK 路径 client disconnect 修复

原有 `streamForward` 中 client 断连后 `defer DrainBody(resp.Body)` 会阻塞读完后端全部数据。修复后使用 `context.WithCancel`，在 write error 时主动 `cancelBackend()` 取消后端请求，立即释放 GPU 资源。

#### 4.8.6 V2 ACK 路径 backend 状态码 & 流结果透传

`streamForward` 返回 `streamForwardResult{BackendStatus, StreamResult}` 结构，使调用方可将后端 HTTP 状态码和流完成状态写入 audit 记录。修复前 V2 ACK 路径的 audit 中 `backend_status=0` / `stream_result=""` 为零值，无法区分成功、后端错误和空响应。

新增的可观测行为：
- 后端返回非 2xx：`event=BACKEND_NON_2XX` Warn 日志 + `ProxyBackendStatus` metric
- 后端返回空 body：`event=BACKEND_NON_2XX` Warn 日志 + `StreamCompletionTotal{error}` metric，`StreamResult="empty"`
- EOF 分支细化：`seenDone=true` → "done"；`firstWrite=true`（空 body）→ "empty"；`StatusCode>=300` → "error"；其余 → "interrupted"

#### 4.8.7 Precheck 拒绝日志 trace_id 注入

`runPreChecks` 在日志初始化后从 `X-Trace-ID` header 注入 trace_id 到 reqLog，使所有 precheck 拒绝日志（not-serving、rate-limit）可通过 trace_id 检索。

#### 4.8.8 Token 信息反馈

非流式请求的 token 用量（prompt/completion/total）通过 `CostMetrics` 反馈给 Scheduler，作为调度策略的参考信息。`ReleaseRequest` proto 新增 3 个 token 字段（field 7-9）。

#### 4.8.9 新增 Prometheus 指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `router_tokens_prompt_total` | Counter | instance_id | 非流式 prompt token 累计 |
| `router_tokens_completion_total` | Counter | instance_id | 非流式 completion token 累计 |
| `router_time_to_first_byte_ms` | Histogram | instance_id, mode | TTFT（stream/non_stream） |
| `router_stream_completion_total` | Counter | instance_id, result | 流完成状态（done/interrupted/error） |
| `router_nonstream_retries_total` | Counter | - | 非流式重试次数 |

#### 4.8.10 Chat Completions pause 语义统一

`/v1/chat/completions` 与 `/api/v2/chat/completions` 在 scheduler paused 时保持一致：跳过 gateway pause 快速 503，读取请求 body 后把本地 pause 状态或 allocation 返回的 `ErrPaused` 转成 OpenAI chat completion abort 响应。

- 非流式：HTTP 200，`finish_reason="abort"`，`router_generated=true`，`X-Router-Generated: true`
- 流式：HTTP 200 SSE，单个 abort chunk，`finish_reason="abort"`，`router_generated=true`
- `not serving` 仍在 `runPreChecks` 快速返回 503；`/v1/completions` 和 `/generate` 仍保留 pause 快速 503

---

### 4.9 PD/Normal 链路激进收敛（PD Disaggregation Unified on Lifecycle）

**背景**：PD（Prefill-Decode 分离）链路与 Normal 链路最初是两条平行实现，各自维护 audit / inflight / 释放重试 / SSE 转发，约 3164 LOC 重复，且 PD 链路未复用 `requestLifecycle`。激进收敛的目标是让两条链路共用同一套基础设施，仅在「allocation 形态」与「per-attempt 重试编排」上保留差异。

**收敛后骨架**：

```
HandleV2ChatCompletion (v2_chat.go)
    │
    ├── routeMode(r) == RouteModePD ──► HandleV2SplitwiseChatCompletion (v2_splitwise_chat.go)
    │                                       │
    │                                       ├── runPreChecks (tracing / serving / limiter / body)
    │                                       ├── newRequestLifecycle (audit / inflight / request_id)
    │                                       └── dispatchSplitwiseRequest (pd_reschedule retry loop)
    │                                              ├── newPDAttempt (attempt-local request_id / RouteContext)
    │                                              ├── allocatePDWithRetry ──► lc.allocatePDAttempt
    │                                              ├── executePDRound ──► PostToPD(pdForwardRequest)
    │                                              │       │
    │                                              │       └── readPrefillRecv goroutine
    │                                              │              └── lc.releaseSecondarySlot(pa.prefillSlot)
    │                                              │       └── check finish_reason (reschedule / done)
    │                                              ├── pumpSSE / writeSplitwiseDecodeResponse
    │                                              └── on retry: lc.finalizePDAttempt(errorCode)
    │
    └── (default)  ──► runPreChecks + newRequestLifecycle
                            └── handleV2ACKStream / handleV2StreamNonACK / handleNonStreamChat
                                    └── pumpSSE
```

#### 4.9.1 关键统一点

| 维度 | 收敛前 | 收敛后 |
|------|-------|--------|
| **Forward 抽象** | 死代码 `ForwardStrategy` 接口 + 两个实现 | 删除整个抽象，直接用闭包注入 `buildDisaggregateMutator` |
| **释放重试** | `releaseWithRetry` / `releasePDWithRetry` 两份独立实现 | `Server.retryRelease(ctx, log, releaseTarget, op)` 通用编排，两边只剩薄 wrapper |
| **Lifecycle** | PD 链路完全旁路 lifecycle，手动管理 audit / inflight / metrics | PD 与 Normal 共用 `requestLifecycle`；PD 外层重调度使用 `pdAttempt` 隔离每次 allocation |
| **Precheck** | Normal 与 PD 各自做 serving check / 读 body / 限流释放 | 统一走 `runPreChecks`，PD 与 Normal 共享 tracing、step check、FlowController 语义 |
| **Body 上限** | `io.ReadAll` 无上限，异常请求可放大内存 | `max_request_body_bytes` 默认 64MiB，`0` 表示禁用上限 |
| **RouteContext** | 多处重复生成 `TraceID/GatewayID/RequestID/SessionID/RequestText` | `Server.acquireRouteContext` 单点创建；PD 每个 outer attempt 使用独立 `request_id`，同一 attempt 内的 AllocatePD transient retry 复用该 attempt 的 `request_id`，与集中式 Allocate 幂等语义一致 |
| **SSE 转发** | `streamForward`（Normal）+ `splitwiseStreamForward`（PD）独立循环 | `pumpSSE(w, body, observer)` 单点实现；Normal 用 observer 注入 TTFT/[DONE]/capture，PD 直透 |
| **Dispatch** | `if s.splitwiseEnabled` 写死在 handler 顶部 | `s.routeMode(r) == RouteModePD`，可通过 `WithRouteModeFunc` 多租户覆写 |
| **实例地址** | PD 路径用 `Host + InferPort` 重新拼接推理地址 | `Endpoint` 作为唯一转发地址；`Host` 仅表示节点拓扑，用于 IPC/RDMA 判断 |

#### 4.9.2 requestLifecycle PD 扩展

```go
type allocSlot struct {
    inst         *domain.Instance
    instanceID   string
    endpoint     string // from Instance.Endpoint
    allocationID string
    role         string
    im           *metrics.InstanceMetrics
    released     atomic.Bool
}

type pdAttempt struct {
    attemptNo           int
    allocationRequestID string
    routeCtx            *domain.RouteContext
    decodeSlot          *allocSlot
    prefillSlot         *allocSlot
}

type requestLifecycle struct {
    // ... 请求级 audit / inflight / proxy outcome ...
    decodeSlot         *allocSlot                // PD decode；normal 仍使用原主字段
    secondary          atomic.Pointer[allocSlot] // 当前 PD prefill；nil 表示 normal
    pdMode             bool
    secondaryErrorCode string
}
```

**双 slot 释放语义**：
- **Primary**（normal slot 或 PD decode）走 `releaseAllocation` → `pdMode` 决定走 `pdAllocator.ReleasePD` 还是 `allocator.Release`。
- **Secondary**（PD prefill）有两条释放路径：
  1. `releaseSecondarySlot(pa.prefillSlot, errorCode)`：从 `readPrefillRecv` goroutine 在 prefill backend 发出 `[DONE]` 时调用，提早归还容量。
  2. `releaseSecondaryFallback()`：从 `releaseAllocation` / `resetForPDRetry` 兜底调用。

  每个 `allocSlot` 自带 `released.CompareAndSwap(false, true)`。晚到的旧 prefill goroutine 只能释放它捕获的旧 slot，不能通过 `requestLifecycle.secondary` 误释放新 attempt 的 prefill。

**并发安全**：`secondary` 字段使用 `atomic.Pointer[allocSlot]`，因为 `readPrefillRecv` goroutine 与主 goroutine 的 `finalizePDAttempt`（清空 slot 准备下一次 attempt）会真正并发访问该指针。释放幂等性不依赖这个全局指针，而依赖 slot-local CAS。

**池对象所有权**：普通 `LocalAllocator` 通过 `PoolResourceOwner` 表达本地 scheduler 会接管 `RouteContext/CostMetrics`。PD 对应使用 `PDPoolResourceOwner`：`LocalPDAllocator` 由 scheduler event-loop 释放 `RouteContext`，`RemotePDAllocator` 由 gateway 释放。PD `ReleasePD` 只传 duration/errorCode 标量，因此 gateway 侧创建的 `CostMetrics` 始终由 gateway 释放。

#### 4.9.2.1 Scheduler 侧 PD allocation 跟踪

Scheduler 的 ghost cleanup 不再按 `gatewayAddr -> instanceID -> count` 粗粒度统计，而是按 `gatewayAddr -> allocationID -> {instanceID, kind}` 记录：

- normal allocation 记录为 `kind=normal`，cleanup 时回调主 policy `Feedback` 并递减 `activeCount`。
- PD allocation 拆成两条记录：prefill/decode 各自携带独立 allocation_id 与 role，cleanup 时分别回调 `pdPrefillPolicy` / `pdDecodePolicy` 并递减 `pdActiveCount`。
- `/v1/status` 和 waiting-queue 诊断接口同时暴露 `active_count`、`pd_active_count`、`total_active_count`、`pd_waiting_queue_depth`，global inflight headroom 统一使用 normal+PD 总量。

PD allocation 不做 batch 聚合：每个请求按顺序选择 prefill 再选择 decode。P/D 仍复用统一的 `policy.Policy` 接口和策略实现，只是各自持有独立 policy 实例，并分别作用于对应 role 的节点集合，避免 counter/cache/session 状态互相污染。

#### 4.9.2.2 PD cache-aware 调度

PD prefill 的 `pd_cache_aware` 默认恢复为更偏 cache 命中的 splitwise 语义：

- Gateway 在 PD splitwise 路径优先从 OpenAI-compatible `messages` 提取 `RequestText`，保持与原始 cache-aware prefix 行为一致；只有没有可用文本时才提取 `prompt_token_ids` 写入 `RouteContext.RequestTokenIDs`，覆盖 token-only rollout 请求。
- Normal `Allocate` 仍会透传 `prompt_token_ids`；Scheduler 侧 `process_tokens` 与 `pd_cache_aware` 在收到 `RequestTokenIDs` 时优先按真实 token 序列计负载，否则回退到 `RequestText` 的估算 token。
- `pd_cache_aware` 使用 64-token block hash 记录 prefill worker 的 prefix；命中率 `<= 60` 或 token 为空时回退到 `process_tokens`，避免冷启动/弱命中时被 cache score 误导。
- 默认权重参考原 splitwise 方案：`hit_ratio_weight=1.0`、`load_balance_weight=0.5`、`balance_abs_threshold=30.0`、`balance_rel_threshold=0.01`。这让一个已有 warm prefix 的 worker 可以承受少量在途请求，而不是被单个空闲冷 worker 抢走。
- `splitwise.cache_block_size`、`hit_ratio_weight`、`load_balance_weight`、`balance_abs_threshold`、`balance_rel_threshold` 会作为 Scheduler 的 PD policy 启动默认值；`/v1/steps/start` 与 `/api/v2/start_infer` 的同名字段可在单个 step 内覆盖。
- `RouteContext` 归还对象池时会清空 token slice，防止长 prompt token 序列被池对象长期持有。

这条链路同时覆盖文本与 token-only 请求：常规 chat 请求按文本 prefix 建立稳定亲和；`messages` 为空或没有可抽取文本时，只要请求携带 `prompt_token_ids`，prefill 调度仍能建立 token-level prefix 亲和。

#### 4.9.3 retryRelease 通用编排

```go
type releaseTarget struct {
    InstanceID, AllocationID, Role string
}

func (s *Server) retryRelease(parent context.Context, log *zap.Logger,
    target releaseTarget, op func(ctx context.Context) error) {
    // 1. context.WithoutCancel + 10s timeout：脱离请求生命周期，防止客户端断开导致释放失败
    // 2. 1+3 次重试，指数退避（200ms → 400ms → 800ms）
    // 3. metrics.ReleaseRetries / 失败日志 / Role-aware 日志字段
}
```

Gateway 到 Scheduler 的短 RPC 使用 `scheduler_rpc_timeout`（默认 3s）建立独立 per-RPC deadline，覆盖 Release/ReleasePD/Register/Heartbeat。Allocate/AllocatePD 使用独立的 `scheduler_allocate_timeout`（默认 0，即跟随外层 HTTP request context），避免 Scheduler waiting queue 默认 600s 被 3s 控制面 timeout 截断；只有明确希望分配快速失败时才应把该值设置为正数。若 AllocatePD 在 caller cancel 后才返回 fresh allocation，Scheduler 会 drain 结果并提交 event-loop 内的补偿事件；如果同一 `request_id` 的 dedup retry 已经恢复该 allocation，则跳过补偿，否则一次性释放 prefill/decode 两侧并删除匹配的 PD alloc dedup entry，避免调用方没有 allocation_id 时留下幽灵占用。gRPC handler 将 `context.Canceled` / `context.DeadlineExceeded` 映射为对应状态码，避免将正常取消误标成 Internal。

PD 的 `pd_reschedule` 与 AllocatePD transient retry 上限分别由 `splitwise.max_reschedule_retries`（默认 1000）和 `splitwise.max_alloc_retries`（默认 100）控制，并始终受请求 context/deadline 约束。`pd_reschedule` 是业务级重新调度，会生成新的 scheduler `request_id`；同一 outer attempt 内的 AllocatePD transient retry 复用该 attempt 的 `request_id`，让 Scheduler 幂等缓存处理“不确定是否已提交”的 RPC 重试，避免重复分配和幽灵占用。`codes.Canceled` / `codes.DeadlineExceeded` 视为 transient allocation error；如果外层 HTTP request 已取消，retry loop 会立即停止。

PD 后端 HTTP 转发使用 `splitwise.backend_response_header_timeout`（默认 5m）限制等待 Prefill/Decode 响应头的时间，覆盖 FD preallocate 卡住但 TCP 已建连的场景。该 timeout 不限制 SSE 响应头返回后的流式生命周期；设置为 `0` 可关闭。

`releaseWithRetry` / `releasePDWithRetry` 各自只构造闭包：

```go
return s.retryRelease(ctx, log, target, func(ctx context.Context) error {
    return s.allocator.Release(ctx, ...)  // 或 pdAllocator.ReleasePD(...)
})
```

#### 4.9.4 pumpSSE 共享 SSE 循环

```go
type sseObserver func(chunk []byte)

func pumpSSE(w http.ResponseWriter, body io.Reader, observer sseObserver) error {
    buf := v2StreamBufPool.Get().([]byte)
    defer v2StreamBufPool.Put(buf)
    for {
        n, readErr := body.Read(buf)
        if n > 0 {
            if observer != nil { observer(buf[:n]) }
            if _, writeErr := w.Write(buf[:n]); writeErr != nil {
                return errClientDisconnect
            }
            if flusher, ok := w.(http.Flusher); ok { flusher.Flush() }
        }
        if readErr == io.EOF { return nil }
        if readErr != nil { return readErr }
    }
}
```

- Normal `streamForward` 注入 observer 用于 TTFT 记录、`[DONE]` 检测、`streamCapture.Feed`。流结果分类（done / interrupted / error）移到 pumpSSE 返回后，根据 `(firstWrite, seenDone, statusCode)` 推断。
- PD `splitwiseStreamForward` observer = `nil`，纯字节透传。

#### 4.9.5 Per-request Route Dispatch Hook

```go
type RouteMode int
const (
    RouteModeNormal RouteMode = iota
    RouteModePD
)
type RouteModeFunc func(r *http.Request) RouteMode

func WithRouteModeFunc(fn RouteModeFunc) ServerOption { ... }
func (s *Server) routeMode(r *http.Request) RouteMode {
    if s.routeModeFunc != nil { return s.routeModeFunc(r) }
    if s.splitwiseEnabled    { return RouteModePD }
    return RouteModeNormal
}
```

默认行为完全等价于 `splitwiseEnabled`。多租户网关可通过 header / host / URL 覆写：

```go
NewServer(..., WithRouteModeFunc(func(r *http.Request) RouteMode {
    if r.Header.Get("X-Tenant-Mode") == "pd" {
        return RouteModePD
    }
    return RouteModeNormal
}))
```

注意：`RouteModeFunc` 在 body 解析之前调用，**不要**读取 `r.Body`。默认模型仍是 gateway 级别 dispatch：`splitwiseEnabled=true` 时默认 PD-only，否则 normal-only。需要基于 model / extra_body 混合路由时，应先通过 header/path 显式传递路由信号，或后续引入共享 body parse 后的 `RequestMeta` 决策阶段。

#### 4.9.6 收敛收益

- **代码量**：Normal/V2/PD 入口不再手写 audit / inflight / RouteContext；`server.go`、`v2_chat.go`、`v2_splitwise_chat.go` 的入口样板进一步收敛。
- **行为一致性**：PD 与 Normal 共享 precheck、audit 字段集（新增 `prefill_instance`）、释放重试参数（10s 超时 / 3 次重试 / WithoutCancel）、SSE buffer pool。
- **幂等一致性**：无 `X-Trace-ID` 时，`requestLifecycle` 生成一次 `request_id` 并同时写入 audit 与 Allocate；非流式重试基于同一 id 派生 `-retryN`，避免排障日志与 Scheduler 去重键分叉。
- **可扩展性**：新增 forward 模式（如 future MoE 分散调度）只需扩 lifecycle 的 slot 数量与新建 attempt loop，retry / SSE / audit 全免费复用。

---

## 5. 工程质量改进

### 5.1 技术栈替换（解决：GDP 框架无人维护）

| 能力 | 旧（GDP 体系） | 新（社区标准） |
|------|---------------|-------------|
| 日志 | gdp/logit（无压缩/轮转） | **zap**（结构化、高性能、可配置级别和格式） |
| RPC | pbrpc（无人维护） | **gRPC + Protobuf**（标准生态） |
| HTTP | gdp/ghttp | **标准库 net/http**（零依赖） |
| Redis | gdp/redis（每请求强依赖） | **无 Redis 依赖**（纯内存） |
| ORM | gdp/gorm_adapter | **无数据库依赖** |
| 可观测 | gdp/metrics（维度不足） | **Prometheus client_golang**（promauto 自注册） + **OpenTelemetry**（分布式 Tracing） |

### 5.2 配置管理（解决：类型混乱 + 多来源）

核心文件：[config.go](internal/config/config.go), [duration.go](internal/config/duration.go), [flagoverride.go](internal/config/flagoverride.go)

#### 分层加载机制

配置按优先级从高到低分三层加载，高层覆盖低层：

```
CLI flag  >  YAML 配置文件  >  代码默认值
```

加载流程（`cmd/router/main.go`）：

```
1. 解析 CLI flag
2. 若传入 --version，打印构建信息后直接退出，不加载配置、不启动监听器
3. cfg := config.Defaults()           ← 全部填充默认值
4. cfg.LoadFromFile("config.yaml")    ← YAML 覆盖（仅出现的字段）
5. config.ApplyCLIOverrides(cfg, fs)  ← CLI flag 覆盖（仅显式传入的 flag）
6. cfg.Validate()                     ← 校验逻辑一致性
```

**CLI 覆盖检测**：通过 `flag.FlagSet.Visit()` 只收集用户显式传入的 flag，未传入的 flag 不会覆盖 YAML 中的值。

#### 配置结构

```go
type Config struct {
    Mode              Mode     // gateway | scheduler | hybrid
    ListenAddr        string   // 业务 HTTP 端口（默认 :8080）
    AdminAddr         string   // Admin HTTP 端口（默认 :8081），空值回退到 ListenAddr
    GRPCAddr          string
    SchedulerAddr     string
    AdvertiseAddr     string
    Policy            string
    HeartbeatInterval Duration // 自定义类型，支持 YAML "5s" / 裸数字
    HeartbeatTimeout  Duration
    ShutdownGrace     Duration // 优雅关停超时
    Metrics           MetricsConfig
    Log               LogConfig
    MetricsCollector  MetricsCollectorConfig
    HealthChecker     HealthCheckerConfig
    CircuitBreaker    CircuitBreakerConfig
    RateLimit         RateLimitConfig
    Tracing           TracingConfig          // OpenTelemetry 分布式 Tracing
}
```

#### Duration 类型

`yaml.v3` 不能直接解析 `time.Duration`，通过自定义 `Duration` 类型解决：

```go
type Duration struct { Duration time.Duration }
```

- YAML 字符串 `"5s"` / `"100ms"` → `time.ParseDuration`
- YAML 裸数字 `10` / `0.5` → 解释为秒
- 引号数字 `"10"` → 先尝试 `ParseFloat`（当作秒），再尝试 `ParseDuration`

#### 校验规则（`Validate()`）

| 规则 | 说明 |
|------|------|
| Mode 合法性 | 必须为 gateway / scheduler / hybrid |
| gateway 必须有 scheduler_addr | 网关模式需要知道调度器地址 |
| Duration > 0 | heartbeat_interval、heartbeat_timeout、shutdown_grace |
| heartbeat_timeout > heartbeat_interval | 超时必须大于心跳间隔 |
| admin_addr != listen_addr | 若显式设置 admin_addr，不能与 listen_addr 相同 |

#### 向后兼容

- 不提供 `--config` 时行为与纯 CLI flag 完全一致
- 零配置启动仍可用（全部使用 `Defaults()` 默认值）
- 所有现有 CLI flag 保持不变

#### 配置示例

参见 [config.example.yaml](../config.example.yaml)，包含所有默认值（注释状态），复制后按需取消注释即可。

### 5.3 错误处理（解决：panic/error 混用）

设计原则：
- **统一 error 返回**，禁止业务逻辑中使用 `panic`
- **结构化日志**：每条日志必须包含 where + what + why + with which data
- **Event-Loop 内部不 panic**：所有异常通过 `resultCh` 返回给调用方

### 5.4 健康检查（解决：可观测性薄弱）

健康探针和管理端点默认在独立的 Admin 端口（`AdminAddr`，默认 `:8081`）上提供服务，与业务流量隔离。当 `AdminAddr` 为空时，所有端点回退到主端口（`ListenAddr`），保持向后兼容。

#### Admin 端口端点

| 端点 | 语义 | 检查项 |
|------|------|--------|
| `GET /healthz` | Liveness（进程存活） | 200 空 body |
| `GET /readyz` | Readiness（可接收流量） | scheduler: 任一资源组 `SERVING`；gateway: remote scheduler 已完成注册连接 |
| `GET /metrics` | Prometheus 指标暴露 | requests_total, active_requests, request_duration_ms, step_phase, registered_gateways 等 |
| `GET /version` | 构建版本信息 | version, commit, build_date（UTC+8 北京时间） |
| `GET /v1/admin/log-level` | 查询当前日志级别 | — |
| `PUT /v1/admin/log-level` | 运行时动态调整日志级别 | — |
| `GET /debug/pprof/*` | Go pprof 性能剖析 | — |

#### 主端口端点（ListenAddr）

主端口只承载业务路由，按部署模式注册：

| 端点 | 模式 | 说明 |
|------|------|------|
| `POST /v1/chat/completions` | gateway / hybrid | 数据面：Chat Completions 推理请求 |
| `POST /v1/completions` | gateway / hybrid | 数据面：Completions 推理请求 |
| `POST /generate` | gateway / hybrid | 数据面：Generate 推理请求 |
| `POST /v1/reward` | gateway / hybrid | 数据面：FastDeploy reward 模型打分请求 |
| `POST /v1/steps/start` | scheduler / hybrid | 控制面：开始推理步骤 |
| `POST /v1/steps/end` | scheduler / hybrid | 控制面：结束推理步骤 |
| `POST /v1/instances` | scheduler / hybrid | 控制面：注册后端实例 |
| `PUT /v1/instances` | scheduler / hybrid | 控制面：同步后端实例 |
| `GET /v1/status` | scheduler / hybrid | 控制面：全量运行时状态 |
| `/api/v2/*` | gateway / hybrid | V2 兼容路由 |

#### CLI 配置

| Flag | 配置键 | 默认值 | 说明 |
|------|--------|--------|------|
| `--listen` | `listen_addr` | `:8080` | 业务 HTTP 端口 |
| `--admin-listen` | `admin_addr` | `:8081` | Admin HTTP 端口（空值回退到主端口） |
| `--version` | — | — | 打印二进制构建信息并退出，不读取配置文件 |

### 5.4.1 Admin 端口分离

#### 设计动机

在生产环境中，管理流量（K8s 探针、Prometheus 抓取、pprof 剖析）与业务流量（推理请求转发）混合在同一端口上会带来以下问题：

1. **资源竞争**：K8s 每 5~10s 一次的 liveness/readiness 探测和 Prometheus 15~30s 一次的 scrape 请求，在高负载时与推理请求竞争连接和 goroutine 资源。推理请求（尤其是 SSE 长连接）可能因管理请求的存在而增加尾延迟。
2. **安全暴露面**：pprof 端点会暴露运行时内存、goroutine 堆栈等敏感信息。若与业务端口共享，外部客户端可直接访问。
3. **网络策略粒度不足**：运维无法独立控制管理端口的访问权限——要么全放行，要么全拒绝。

#### 架构方案

```
外部客户端 / 训练框架
        │
        │  推理请求 / 控制面 API
        v
  ┌──────────────────┐
  │  主端口 :8080     │  ← ListenAddr
  │  业务路由          │
  │  ├ /v1/chat/...   │
  │  ├ /v1/steps/...  │
  │  └ /api/v2/...    │
  └──────────────────┘

K8s kubelet / Prometheus / 运维工具
        │
        │  探针 / 指标抓取 / pprof
        v
  ┌──────────────────┐
  │  Admin 端口 :8081 │  ← AdminAddr
  │  管理路由          │
  │  ├ /healthz       │
  │  ├ /readyz        │
  │  ├ /metrics       │
  │  ├ /v1/admin/*    │
  │  └ /debug/pprof/* │
  └──────────────────┘
```

#### 关键收益

| 收益 | 说明 |
|------|------|
| K8s 探针不影响业务 | 探针和 Prometheus 抓取使用独立监听器和 goroutine 池，不与推理请求竞争 |
| pprof 安全隔离 | pprof 仅暴露在 Admin 端口，可通过 K8s NetworkPolicy 限制仅集群内部访问 |
| 网络策略独立控制 | 运维可对 Admin 端口和业务端口分别配置防火墙/NetworkPolicy |
| 向后兼容 | `AdminAddr` 为空时所有端点回退到主端口，不影响已有部署和配置 |

#### 中间件差异

| 层 | 主端口 | Admin 端口 |
|----|--------|-----------|
| Recovery | 有 | 有 |
| HTTPMetrics | 有 | 有 |
| AccessLog | 有（含慢请求检测） | 无（避免探针日志淹没） |
| Tracing | 有 | 无 |

### 5.5 三层健康架构（Three-Layer Health Architecture）

后端实例的健康管理拆分为三个完全独立的层，遵循 SGLang 的设计模式：每层各自采集数据、写入各自的状态字段、互不调用、互不写对方的状态。

#### 架构总览

| 组件 | 数据来源 | 写入目标 | 运行方式 |
|------|---------|---------|---------|
| **HealthChecker** | `GET /health`（后端健康端点） | `NodeState.Healthy` | 独立 goroutine + ticker |
| **MetricsCollector** | `GET /metrics`（后端指标端点） | `policy.MetaUpdater` | 独立 goroutine + ticker |
| **CircuitBreaker** | Release ErrorCode（请求释放时的错误码） | `NodeState.CircuitOpen` | Event-loop 内联（无锁） |

三层的数据流完全隔离：

```
                           ┌──────────────────────────────────┐
                           │         后端推理实例               │
                           │  (vLLM / SGLang / FastDeploy)    │
                           └──┬──────────────┬────────────────┘
                              │              │
                     GET /health      GET /metrics
                              │              │
                              v              v
                    ┌─────────────┐  ┌──────────────────┐
                    │HealthChecker│  │ MetricsCollector  │
                    │ (goroutine) │  │   (goroutine)     │
                    └──────┬──────┘  └────────┬──────────┘
                           │                  │
                NodeState.Healthy    policy.MetaUpdater
                           │                  │
                           v                  v
                    ┌────────────────────────────────────┐
         ┌─────────┤       NodeStateStore                │
         │         └────────────────────────────────────┘
         │
         │  Release(errorCode)
         │         │
         │         v
         │  ┌──────────────┐
         │  │CircuitBreaker│
         │  │ (event-loop) │
         │  └──────┬───────┘
         │         │
         │  NodeState.CircuitOpen
         │         │
         v         v
    ┌────────────────────────────────────┐
    │  LoadAvailable() = Healthy &&      │
    │                    !CircuitOpen     │
    │  ─── 所有 5 个 Policy 的调度判据 ───  │
    └────────────────────────────────────┘
```

#### 关键设计决策

1. **三层完全独立**：HealthChecker、MetricsCollector、CircuitBreaker 永远不调用彼此、不写彼此的状态字段。这使得每层可以独立开关、独立测试、独立演进。
2. **统一调度判据**：所有 5 个 Policy（MinLoad、MinRequest、RoundRobin、SessionAware、SessionAwareV3）使用 `LoadAvailable()` 替代原来的 `LoadHealthy()`。`LoadAvailable() = Healthy && !CircuitOpen`，共 7 处调用点统一替换。
3. **默认全部关闭**：HealthChecker 和 CircuitBreaker 默认 disabled（opt-in），不影响已有部署。MetricsCollector 保持原有独立配置。
4. **CircuitBreaker 运行在 event-loop 内**：使用 plain fields（非 atomic），因为只有 event-loop 单线程访问，零锁开销。采用懒创建模式，首次遇到实例错误时才为该实例分配 breaker。

#### 5.5.1 HealthChecker

独立 goroutine 定时 `GET /health` 检测后端实例是否存活。

**判定规则**：

- Transport 错误（连接拒绝、超时、DNS 失败）**和** HTTP 非 200 响应均计为失败
- 连续 `fail_threshold` 次失败标记 `NodeState.Healthy = false`
- 恢复需连续 `success_threshold` 次成功才重新标记 `Healthy = true`（避免抖动）
- 与 MetricsCollector 的错误分类不同：Collector 只在 transport 错误时影响健康，HTTP 非 200 仅记日志；HealthChecker 对两者一视同仁，因为 `/health` 返回非 200 明确表示服务不健康

**并发控制**：

- `max_concurrency` 限制同时探测的实例数，避免万卡场景下瞬间 1w 并发 HTTP 请求
- 使用带缓冲 channel 作为信号量

**文件**：`internal/scheduler/healthcheck/checker.go`

#### 5.5.2 MetricsCollector（精简后）

MetricsCollector 不再负责健康检测，专注于负载指标采集：

- **职责收窄**：仅将 waiting_count/available_blocks 等指标喂给 `policy.MetaUpdater`
- **健康逻辑移除**：原有的 `consecutiveFails` 计数和 `Healthy` 标记逻辑已移至 HealthChecker
- **错误分类保持不变**：Transport 错误和 HTTP 状态错误仍区分记录，但均不再影响 `NodeState.Healthy`

##### 混合原子方案（零锁读写）

- **Map 结构变更**（低频：实例注册/注销）→ COW `atomic.Pointer` swap
- **字段值更新**（高频：每 2s）→ `atomic.Int64.Store` 原地写
- **读路径**（event-loop）→ `atomic.Pointer.Load` + `atomic.Int64.Load`，零锁零分配

| 路径 | 操作 | 开销 |
|------|------|------|
| 读（event-loop） | Pointer.Load + map[id] + 4x Int64.Load (含 localDrift) | ~200ns/instance，0 allocs |
| 写-高频（Collector） | 4000x 4x Int64.Store (含 drift reset) | ~16us/sweep，0 allocs |
| 写-低频（新实例加入） | COW map clone + swap | ~50us，仅在实例变更时 |

##### 多后端支持

通过 `MetricsFetcher` 接口 + 注册表工厂，同一 Scheduler 管理 FastDeploy + sglang + vllm：

```go
type MetricsFetcher interface {
    Fetch(ctx context.Context, endpoint string) (InstanceMetrics, error)
    Name() string
}
```

`Instance.BackendType` 字段确定使用哪个 Fetcher，空值使用 `default_backend` 配置。

##### 本地漂移补偿（Local Drift Compensation）

指标每 2s 拉取一次，但调度是实时的。在两次 metric sweep 之间，Scheduler 已经分配了多个请求，
polled `waitingCount` 无法反映这些增量。本地漂移补偿解决此 staleness gap：

```
effectiveWaiting = max(0, polledWaiting + localDrift)
```

- **Acquire**: `localDrift.Add(+1)` — 分配一个请求，预期后端 waiting 增加
- **Release**: `localDrift.Add(-1)` — 释放一个请求，预期后端 waiting 减少
- **Metrics 刷新**: `localDrift.Store(0)` — 真实指标到达，校准复位

`DriftAware` 接口由 MinLoadPolicy 实现，Server 在构造时缓存类型断言结果以避免热路径开销。

#### 5.5.3 CircuitBreaker

基于请求释放时的错误码（Release ErrorCode）驱动，运行在 event-loop 内部，使用 plain fields 而非 atomic（单线程保证）。

**状态机**：

```
         失败次数 >= fail_threshold
  Closed ──────────────────────────> Open
    ^                                  │
    │                                  │ open_duration 超时
    │                                  v
    │    成功次数 >= success_threshold
    └─────────────────────────────── HalfOpen
              (失败则回 Open)
```

- **Closed**：正常状态，记录连续失败次数
- **Open**：熔断状态，`NodeState.CircuitOpen = true`，该实例不参与调度。持续 `open_duration` 后自动转为 HalfOpen
- **HalfOpen**：试探状态，允许少量请求通过。连续 `success_threshold` 次成功则恢复 Closed；任一失败立即回到 Open

**懒创建**：CircuitBreakerManager 不为所有实例预分配 breaker，而是在首次收到该实例的错误时才创建。正常运行的实例零内存开销。

**文件**：

- `internal/scheduler/circuitbreaker/breaker.go` — 单实例状态机
- `internal/scheduler/circuitbreaker/manager.go` — 实例 ID → breaker 的映射管理

#### 5.5.4 `LoadAvailable()` 替代 `LoadHealthy()`

```go
// internal/domain/models.go
func (n *NodeState) LoadAvailable() bool {
    return n.LoadHealthy() && !n.LoadCircuitOpen()
}
```

所有 Policy 在筛选可用实例时统一调用 `LoadAvailable()`，同时考虑健康探测结果和熔断状态。修改涉及 7 处调用点：

- `MinLoadPolicy.Select` / `MinLoadPolicy.BatchSelect`
- `MinRequestPolicy.Select` / `MinRequestPolicy.BatchSelect`
- `RoundRobinPolicy.Select` / `RoundRobinPolicy.rebuildLocked`
- `SessionAwarePolicy.Select`
- `SessionAwareV3Policy.Select` / `SessionAwareV3Policy.BatchSelect`

#### 5.5.5 配置

```yaml
# 后端指标采集（已有）
metrics_collector:
  enabled: true
  default_backend: "fastdeploy"
  scrape_interval: 2s
  scrape_timeout: 1s
  max_concurrency: 128

# 后端健康探测（新增，默认关闭）
health_checker:
  enabled: false            # opt-in，不影响已有部署
  health_path: "/health"    # 后端健康端点路径
  interval: 3s              # 探测间隔
  timeout: 2s               # 单次探测超时
  fail_threshold: 3         # 连续 3 次失败标记 unhealthy
  success_threshold: 2      # 连续 2 次成功才恢复 healthy
  max_concurrency: 128      # 最大并发探测数

# 熔断器（新增，默认关闭）
circuit_breaker:
  enabled: false            # opt-in，不影响已有部署
  fail_threshold: 3         # 连续 3 次请求失败触发熔断
  success_threshold: 2      # HalfOpen 下连续 2 次成功恢复
  open_duration: 30s        # Open 状态持续时间，之后进入 HalfOpen
```

#### 5.5.6 涉及文件清单

**新增文件**：

| 文件 | 职责 |
|------|------|
| `internal/scheduler/healthcheck/checker.go` | HealthChecker 实现 |
| `internal/scheduler/healthcheck/checker_test.go` | HealthChecker 单测 |
| `internal/scheduler/circuitbreaker/breaker.go` | 单实例 CircuitBreaker 状态机 |
| `internal/scheduler/circuitbreaker/breaker_test.go` | breaker 单测 |
| `internal/scheduler/circuitbreaker/manager.go` | CircuitBreakerManager（实例 ID → breaker 映射） |
| `internal/scheduler/circuitbreaker/manager_test.go` | manager 单测 |

**修改文件**：

| 文件 | 变更内容 |
|------|---------|
| `internal/domain/models.go` | 新增 `CircuitOpen` 字段、`LoadAvailable()` 方法 |
| `internal/scheduler/store/node_state_store.go` | `ResetAll` 清除 `CircuitOpen` 状态 |
| `internal/scheduler/collector/collector.go` | 移除健康检测逻辑，精简为纯指标采集 |
| `internal/scheduler/policy/min_load.go` | `LoadHealthy()` → `LoadAvailable()` |
| `internal/scheduler/policy/min_request.go` | `LoadHealthy()` → `LoadAvailable()` |
| `internal/scheduler/policy/round_robin.go` | `LoadHealthy()` → `LoadAvailable()` |
| `internal/scheduler/policy/session_aware.go` | `LoadHealthy()` → `LoadAvailable()` |
| `internal/scheduler/server.go` | CircuitBreaker 集成（通过 ServerOption 注入） |
| `internal/config/config.go` | 新增 `HealthCheckerConfig`、`CircuitBreakerConfig` 结构体 |
| `internal/app/app.go` | 组装 HealthChecker 和 CircuitBreaker，注入 Scheduler |
| `internal/scheduler/http_handler.go` | `/v1/status` API 增加 `circuit_open` 字段 |
| `pkg/metrics/metrics.go` | 新增 CircuitBreaker 和 HealthChecker 相关 Prometheus 指标 |
| `pkg/logger/fields.go` | 新增 circuit breaker 事件日志常量 |

---

## 6. 性能优化总结

以下优化已落地，吞吐从 ~2w QPS 提升至 ~24w QPS（event-loop 实测）：

| 优化项 | 技术手段 | 效果 |
|--------|---------|------|
| **零外部 I/O 调度** | 纯内存 NodeStateStore 替代 Redis | 单次调度从 3-15ms → **<0.01ms** |
| **批量事件排空** | channel buffer 131072 + drainAll 批量收集 | 10w 瞬时请求不阻塞 |
| **堆排序批量选择** | MinLoadPolicy 最小堆 O(N+K×logN) | 1w节点×8192请求 **680x** 提升 |
| **原子化 NodeState** | `atomic.LoadInt64` 替代 `sync.RWMutex` | 消除 1w 节点遍历的 2w 次锁操作 |
| **sync.Pool 复用** | `allocResult` channel 对象池 | 10w 请求场景 GC 压力 **~10x** 下降 |
| **COW NodeStateStore** | `atomic.Pointer` + Copy-on-Write 替代 `sync.RWMutex` + 每次 map 拷贝 | GetNodes 从 311μs/437KB → **0.33ns/0B**（10k 节点） |
| **Value-type 堆** | `[]nodeEntry` + 内联 heapInit/heapDown 替代 `container/heap` + `[]*nodeEntry` | BatchSelect 每批从 10003 allocs → **2 allocs** |
| **RoundRobin 排序缓存** | 缓存 sorted slice，仅节点数变化时重建 | Select 从 1.5ms/82KB → **13ns/0B**（10k 节点，113000x） |
| **allocDedup 值类型** | `allocCacheEntry` 存 `*Instance` 指针 + map 值类型 | 消除每请求 1 alloc + dedup hit 时 Instance 分配 |
| **StepNotifier 连接池** | `http.Transport` 配置 `MaxIdleConns=200` | 100-gateway 广播 3.8ms → **1.2ms** |
| **MinLoad P2C 单请求选择** | Power-of-2-Choices 随机采样 O(1) | Select 10K 节点 115μs → **~200ns**（~500x） |
| **MinLoad P2C 缓存预热** | Scheduler 在 `SetGeneration` 后调用 `EnsureCachedIDs` | 生产链路单请求 Select 真正进入 O(1) P2C，而不是停留在线性扫描 |
| **SessionAware BatchSelect** | 两阶段：缓存命中 O(1) + miss 最小堆 O(N+K×logN) | 8192 miss × 10K 节点从 O(K×N) → O(N+K×logN) |
| **Prometheus 指标预缓存** | `sync.Map` 缓存 `InstanceMetrics` 四元组 | 每请求 4×mutex → 1×atomic load |
| **Proxy URL 规范化缓存** | 双 key 缓存（原始 + 规范化），首次后零分配 | schemeless endpoint 零字符串拼接 |
| **Burst 分配预热** | map 预分配 4096 + pool 预热 8192 对象 | 首次 burst 10w 延迟 901ms → **408ms**（2.2x） |

### 6.1 性能基准测试覆盖

项目已实现完整的性能基准测试套件，覆盖以下维度：

| 测试类别 | 文件位置 | 覆盖场景 |
|----------|----------|----------|
| Scheduler 调度吞吐 | `internal/scheduler/benchmark_test.go` | Allocate/Release、Event-Loop、幂等去重、Step 状态机、负载均衡公平性 |
| 策略选择性能 | `internal/scheduler/policy/benchmark_test.go` | 四策略对比、BatchSelect 规模扩展、缓存命中/未命中 |
| Gateway 代理吞吐 | `internal/gateway/benchmark_test.go` | SSE 流式代理、并发连接、延迟分布、完整 E2E 路径 |
| Domain 原子操作 | `internal/domain/benchmark_test.go` | NodeState 原子读写、Clone、并发混合访问 |

**关键指标验证结果**（Apple M2）：

| 场景 | 延迟 | 说明 |
|------|------|------|
| NodeState 原子读 | ~0.3ns/op | `LoadActiveRequests` / `LoadActualLoad` |
| NodeState 原子写 | ~0.4ns/op | `StoreActiveRequests` / `StoreActualLoad` |
| NodeState Clone | ~0.7ns/op | 快照拷贝 |
| GetNodes（10k 节点） | **0.33ns/op, 0B** | COW atomic.Pointer，零拷贝 |
| RoundRobin Select（10k 节点） | **13ns/op, 0B** | 预排序缓存，零分配 |
| MinLoad Select（1000 节点） | ~10μs/op | 线性扫描（≤16 节点）/ P2C（>16 节点） |
| MinLoad Select P2C（10k 节点） | **~200ns/op** | P2C 随机采样 O(1)，cachedIDs 预热后 |
| MinLoad BatchSelect（1w 节点×8192 请求） | ~670μs/op, 2 allocs | 值类型堆 + 内联堆操作 |
| SessionAware BatchSelect AllMiss（10k×4096） | ~1ms/op | 两阶段堆分配 O(N+K×logN) |
| Allocate+Release（10k 节点） | ~194μs/op, 12 allocs | 完整调度周期（含 event-loop） |
| Burst 10w 瞬时请求 | ~408ms（10w goroutine） | 含 StartStep + 10w Allocate + 10w Release + EndStep |
| Gateway E2E（非流式） | ~50μs/op | 完整网关路径 |
| StepNotifier 广播（100 Gateway） | ~1.2ms/op | HTTP 连接池复用 |

**运行基准测试**：

```bash
# 运行所有基准测试
go test -bench=. -benchmem ./internal/domain/... ./internal/scheduler/... ./internal/scheduler/policy/... ./internal/gateway/...

# 性能回归检测
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -count=5 ./internal/scheduler/... > old.txt
# ... 修改代码 ...
go test -bench=. -count=5 ./internal/scheduler/... > new.txt
benchstat old.txt new.txt
```

---

## 7. 数据流详解

### 7.1 请求全链路（Gateway 模式）

```
Client
  │ POST /v1/chat/completions | /v1/completions | /generate | /v1/reward
  v
Gateway.handleInferenceRequest(backendPath) / HandleReward
  │
  ├── 1. runPreChecks()
  │     ├── tracing span + trace_id 日志字段
  │     ├── StepChecker.IsServing()     ← 快速拒绝非 SERVING 请求
  │     ├── FlowController.Acquire()    ← gateway 本地限流
  │     └── io.ReadAll(r.Body)          ← 读取请求体
  ├── 2. parseStreamField(bodyBytes)    ← 检测 stream 字段
  ├── 3. newRequestLifecycle()          ← audit / inflight / request_id
  │
  ├─── stream=true ─────────────────────────────────────────────────
  │   handleStreamChat()
  │   ├── Allocator.Allocate(req)       ← gRPC → Scheduler
  │   ├── streamTracker(w)              ← 包装 ResponseWriter
  │   │   ├── 记录 TTFT（首个 token 时间）
  │   │   ├── 检测 data: [DONE]
  │   │   └── 计数 chunks / bytes
  │   ├── Proxy.Forward(tracker, r)     ← httputil.ReverseProxy（SSE）
  │   ├── Allocator.Release(id, cost)   ← 反馈 duration + error_code
  │   └── 上报 TTFT / stream_completion 指标
  │
  └─── stream=false ────────────────────────────────────────────────
      handleNonStreamChat()
      for attempt := 0..2 {
      ├── Allocator.Allocate(req)       ← 每次重试分配新实例
      ├── doNonStreamForward()
      │   ├── http.Client.Do()          ← 独立 HTTP 请求
      │   ├── 缓冲完整 response body
      │   └── extractUsage()            ← 提取 token 用量
      ├── retryable? → release + continue
      └── success / non-retryable → writeResponse + release
      }
```

### 7.2 Step 生命周期

```
训练框架                    Scheduler                      Gateway(s)
    │                          │                              │
    │  POST /v1/steps/start    │                              │
    │  {step_id, policy?}      │                              │
    │────────────────────────> │                              │
    │                          │  Event: evStartStep          │
    │                          │  ├ policy指定? Build新策略    │
    │                          │  │ 否则 Policy.Reset()       │
    │                          │  ├ ResetAll()                │
    │                          │  └ IDLE → SERVING            │
    │                          │                              │
    │                          │  StepNotifier.Broadcast ────>│ (Push)
    │                          │                              │ UpdateStepState(SERVING)
    │                          │                              │
    │                          │       ... 推理请求进行中 ...    │
    │                          │                              │
    │  POST /v1/steps/end      │                              │
    │────────────────────────> │                              │
    │                          │  Event: evEndStep            │
    │                          │  └ SERVING → DRAINING        │
    │                          │                              │
    │                          │  StepNotifier.Broadcast ────>│ (Push)
    │                          │                              │ UpdateStepState(DRAINING)
    │                          │                              │
    │                          │  最后一个 Release 完成         │
    │                          │  └ DRAINING → IDLE           │
```

---

## 8. 目录结构

```
RL-Router/
├── cmd/router/main.go                  # 入口：解析 flag，三层配置加载，组装 App
│
├── config.example.yaml                 # YAML 配置示例（所有默认值，注释状态）
│
├── api/proto/
│   ├── router.proto                    # gRPC 服务定义
│   └── routerpb/                       # 生成的 Go 代码
│
├── internal/
│   ├── app/app.go                      # 顶层容器，Mode-based 组装
│   │
│   ├── config/
│   │   ├── config.go                  # 强类型配置 + Defaults/LoadFromFile/Validate
│   │   ├── duration.go                # 自定义 Duration 类型（yaml.Unmarshaler）
│   │   └── flagoverride.go            # CLI flag 覆盖逻辑（flag.Visit 检测）
│   │
│   ├── domain/models.go                # 领域模型（Instance, NodeState, StepPhase, ...）
│   │
│   ├── gateway/
│   │   ├── allocator.go                # InstanceAllocator 接口 + Local/Remote 实现
│   │   ├── forward_nonstream.go        # 非流式转发 + 重试 + buffer pool
│   │   ├── lifecycle.go                # requestLifecycle：allocation → forward → release → audit 统一管理
│   │   ├── precheck.go                 # runPreChecks：serving check → rate limit → body read → OTel 注入
│   │   ├── proxy.go                    # 反向代理（SSE 流式）
│   │   ├── retry_stream.go             # streamWithStaleRetry / doHTTPWithStaleRetry：统一 stale-conn 重试
│   │   ├── scheduler_client.go         # 注册 + 心跳 + 自愈
│   │   ├── server.go                   # 数据面 HTTP 处理（V1 stream/non-stream fork）
│   │   ├── stream_detect.go            # parseStreamField：轻量 JSON stream 字段检测
│   │   ├── stream_tracker.go           # SSE 流状态追踪（TTFT / [DONE] / chunks）
│   │   ├── usage.go                    # tokenUsage + extractUsage
│   │   └── v2_chat.go                  # V2 chat completions（ACK / direct routing）
│   │
│   └── scheduler/
│       ├── server.go                   # Event-Loop 核心（struct, NewServer, loop, public API）
│       ├── server_allocate.go          # batchAllocate 流水线 + recordAllocation + handleRelease
│       ├── server_step.go              # handleStartStep + handleEndStep + handleCleanupGateway
│       ├── server_queue.go             # Normal/PD 等待队列：enqueue / drain / reject / pause / getters
│       ├── node_state_store.go         # 内存实例状态存储
│       ├── gateway_registry.go         # Gateway 注册 + 心跳过期
│       ├── grpc_handler.go             # gRPC 适配层
│       ├── http_handler.go             # HTTP 控制面 API
│       ├── step_notifier.go            # Step 状态推送广播
│       └── policy/
│           ├── interface.go            # Policy / BatchSelector / Resettable
│           ├── factory.go              # 策略工厂
│           ├── round_robin.go          # 轮询
│           ├── min_load.go             # 最小负载（堆排序批量）
│           ├── min_request.go          # 最少请求
│           ├── session_aware.go        # KV 缓存亲和（session 绑定）
│           ├── session_aware_v3.go    # 弱绑定 + 请求级负载均衡（Mooncake 场景）
│           ├── session_aware_v4.go    # V3 + 双模式路由 + 堆追踪 + session LRU 驱逐
│           ├── cache_aware.go         # 前缀缓存感知路由（radix tree，参考 sglang）
│           └── radixtree.go           # Multi-tenant radix tree 数据结构
│
└── pkg/
    ├── jsonutil/jsonutil.go            # JSON 统一门面
    ├── logger/logger.go                # zap 日志工厂
    ├── metrics/
    │   ├── metrics.go                  # Prometheus 指标定义 + InstanceMetrics 预缓存
    │   ├── grpc_interceptor.go         # gRPC server/client metrics 拦截器
    │   └── http_middleware.go          # HTTP 请求延迟 + in-flight 中间件
    └── tracing/
        ├── tracing.go                  # OTel TracerProvider 初始化（noop / OTLP）
        ├── propagation.go             # W3C traceparent 注入/提取 helper
        └── http_middleware.go          # HTTP trace context 提取 + server span 中间件
```

---

## 9. 设计模式总结

| 模式 | 应用位置 | 解决的问题 |
|------|---------|-----------|
| **Event-Loop / Actor** | `scheduler.Server` | 无锁串行化所有状态变更 |
| **State Machine** | Step 生命周期 (IDLE→SERVING→DRAINING→IDLE) | 训练步骤间隔离 |
| **Strategy** | `policy.Policy` 接口 + 5 种实现 | 调度算法可插拔 |
| **Factory** | `policy.Build()` 注册表 | 策略按名创建 |
| **Functional Options** | `gateway.ServerOption` | 可选依赖注入 |
| **Interface-based DI** | `InstanceAllocator`, `Proxy`, `StepChecker` | 消除全局单例，可测试 |
| **DTO / Anti-Corruption** | `instanceEntry` → `domain.Instance` | API 契约与领域模型分离 |
| **Object Pool** | `sync.Pool` 复用 allocResult channel / nonStreamBuf | 降低 GC 压力 |
| **Request Lifecycle** | `gateway.requestLifecycle` | 统一 alloc→forward→release→audit，defer finalize() 保证资源不泄漏 |
| **Push-Pull 混合同步** | StepNotifier（Push）+ Heartbeat（Pull） | 兼顾即时性与可靠性 |
| **Self-Healing** | `SchedulerClient` 指数退避 + 自动重注册 | 网络抖动自愈 |
| **Ghost Load Cleanup** | `gatewayAllocs` per-gateway 跟踪 | Gateway 崩溃不留残余负载 |
| **Idempotency Key** | `allocDedup` / `releaseDedup` O(1) map 去重 | TCP 不确定时安全重试 |
| **ResponseWriter Wrapper** | `streamTracker` 包装透明代理 | 零缓冲追踪 TTFT / [DONE] / 流状态 |
| **Buffered Retry** | `handleNonStreamChat` 缓冲完整响应 | 非流式可重试不同后端 |

---

## 10. 新旧架构关键对比

| 维度 | Rollout-Controller（旧） | OneRouter（新） |
|------|------------------------|----------------|
| **调度状态存储** | Redis（3-5次/请求） | 纯内存（零 I/O） |
| **并发模型** | 全局变量 + 无锁保护 | Event-Loop 单线程串行 |
| **外部依赖** | Redis + MySQL + GDP | 零外部依赖（自包含） |
| **调度算法** | 硬编码在 Scheduler 中 | `Policy` 接口可插拔 |
| **可测试性** | 0% 核心逻辑覆盖率 | 全接口化，可 mock |
| **部署灵活性** | 单一模式 | hybrid / scheduler / gateway 三模式 |
| **框架依赖** | GDP（无人维护） | zap + gRPC + 标准库 |
| **配置管理** | string 类型 + 多来源混合 | 强类型 + YAML 配置文件 + CLI flag 分层覆盖 |
| **错误处理** | panic / error / 静默吞错混用 | 统一 error 返回 |
| **Gateway 故障恢复** | 无（负载计数器漂移） | 自愈注册 + 幽灵负载清理 |
| **Step 隔离** | 无 | 三态状态机严格隔离 |
| **调度吞吐** | ~2w QPS | ~24w QPS（event-loop 实测） |

---

## 11. 日志系统设计

### 11.1 设计目标

| 目标 | 要求 |
|------|------|
| **性能** | 100K QPS 下日志不阻塞请求热路径，不同日志类型互不淹没 |
| **定位能力** | 只看日志即可快速定位问题，每条日志包含 WHERE / WHAT / WHY / WITH WHICH DATA |
| **自证清白** | 网关能通过日志证明"问题不在我"——区分网关自身开销 vs 后端延迟 |
| **Hang 检测** | 请求 hang 住时日志能反映出 hang 在哪个阶段 |

### 11.2 双独立 Logger 架构

核心文件：[logger.go](pkg/logger/logger.go)

日志系统采用 **双独立 Logger** 架构，`Bundle.Root`（控制面）和 `Bundle.Access`（访问面）各自独立构建，互不 Tee：

```
  ┌──────────────────────────┐         ┌──────────────────────────────┐
  │      Bundle.Root         │         │       Bundle.Access          │
  │   (控制面 Logger)         │         │    (访问面 Logger)            │
  └─────────┬────────────────┘         └─────────┬────────────────────┘
            │                                     │
    ┌───────┴────────┐                   ┌────────▼──────────────────┐
    │   Tee Core     │                   │  Sampler                  │
    │  ┌───────────┐ │                   │  (100/s first, 1000th)    │
    │  │sync stderr│ │                   │  ┌────────────────────┐   │
    │  └───────────┘ │                   │  │BufferedWriteSyncer │   │
    │  ┌───────────┐ │                   │  │ (256KB, 1s flush)  │   │
    │  │sync file  │ │                   │  │  → access.log      │   │
    │  │router.log │ │                   │  └────────────────────┘   │
    │  └───────────┘ │                   └───────────────────────────┘
    └────────────────┘
    (LogDir 为空时仅 stderr)              (LogDir 为空时写缓冲 stderr)
```

| Logger | 写方式 | Sampling | 用途 | 性能 |
|--------|--------|----------|------|------|
| Root (控制面) | 同步 Tee(stderr + router.log) | 无 | 错误、生命周期事件、Step 状态机、调度决策 | 保证输出，~3.9μs/op |
| Access (访问面) | 异步 BufferedWriteSyncer(access.log) | 100/s 首发，之后每 1000 条取 1 条 | 请求热路径、代理日志 | 不阻塞调用者，~105ns/op |

**关键设计**：两个 Logger 完全独立，Access 的 BufferedWriteSyncer 真正异步 — 调用者只写内存缓冲区，后台 goroutine 批量 flush。

**运行模式**：
- `LogDir` 非空（生产模式）：Root → Tee(stderr, router.log via lumberjack)，Access → Buffered(access.log via lumberjack)
- `LogDir` 为空（开发模式）：Root → sync stderr，Access → Buffered(stderr)

**文件轮转**（lumberjack）：`max_size_mb`(500) × `max_backups`(5) = 2.5GB 上限/文件类型，gzip 压缩后更小。

### 11.3 Named Sub-loggers

通过 `bundle.Sub(name)` / `bundle.SubAccess(name)` 创建各组件的命名子日志器。每个 sub-logger 通过 `moduleCore` 包装器拥有独立的 `AtomicLevel`，`SetModuleLevel()` 真正控制日志过滤：

- `Sub(name)` — 派生自 `Bundle.Root`（控制面），默认使用控制面级别
- `SubAccess(name)` — 派生自 `Bundle.Access`（访问面），默认使用访问面级别

| 名称 | 方法 | 使用方 | 注册位置 |
|------|------|--------|---------|
| `access` | `SubAccess` | HTTP Access Log 中间件 | `app.go` → `AccessLogMiddleware` |
| `scheduler` | `SubAccess` | Scheduler 请求级日志（allocate/release/batch） | `app.go` → `initScheduler` |
| `grpc` | `Sub` | gRPC Server Interceptor | `app.go` → `serveGRPC` |
| `grpc-client` | `Sub` | gRPC Client Interceptor | `app.go` → `grpcClientDialOpts` |
| `allocator` | `Named` | `RemoteAllocator` RPC 日志 | `app.go` → `buildRemoteGatewayDeps` |

日志输出示例：

```
2026-03-08T10:01:02.123+0800  INFO  access  request completed  {"event":"REQUEST_COMPLETE","status":"OK","trace_id":"trace-abc","status":200,"latency":"45ms","bytes":1234}
2026-03-08T10:01:02.124+0800  ERROR allocator  remote allocate failed  {"event":"ALLOCATE","status":"FAIL","trace_id":"trace-def","rpc_latency":"3.2s","error":"context deadline exceeded"}
```

### 11.4 标准化日志 Schema

核心文件：[fields.go](pkg/logger/fields.go)

所有日志遵循统一的结构化字段规范，通过零分配的辅助函数生成 zap 字段：

#### 11.4.1 标准字段

| 字段 | 来源 | 示例 | 用途 |
|------|------|------|------|
| `time` | zap 自带 | `2026-03-08T00:01:02.123+0800` | 时间线 |
| `level` | zap 自带 | `INFO` / `WARN` / `ERROR` | 级别过滤 |
| `logger` | Named sub-logger | `access` / `allocator` / `grpc` | 模块归属 |
| `caller` | `zap.AddCaller()` | `server.go:145` | 代码位置 |
| `trace_id` | ctx logger 注入 | `trace-abc` | 请求链路追踪 |
| `request_id` | ctx logger 注入 | `a1b2-42` | 幂等键 |
| `gw` | ctx logger 注入 | `10.0.0.1:8080` | 网关实例标识 |

#### 11.4.2 事件字段 (event)

事件常量替代自由文本 msg，便于日志搜索和聚合：

| 事件 | 含义 | 触发位置 |
|------|------|---------|
| `REQUEST_START` | 请求入口 | Access Log 中间件 |
| `REQUEST_COMPLETE` | 请求完成 | Access Log 中间件 |
| `SLOW_REQUEST` | 慢请求告警（超过阈值主动触发） | Access Log 中间件 |
| `ALLOCATE` | 实例分配 | `HandleChatCompletion` / `RemoteAllocator` |
| `ALLOCATE_SLOW` | 分配阻塞检测 | Scheduler event-loop（预留） |
| `PROXY_FORWARD` | 代理转发开始 | `HandleChatCompletion` |
| `PROXY_COMPLETE` | 代理转发结束 | `HandleChatCompletion` |
| `RELEASE` | 分配释放 | `releaseWithRetry` |
| `BATCH_PROCESSED` | 批次处理（预留） | Scheduler event-loop |
| `STEP_START` / `STEP_END` | Step 状态变更（预留） | Scheduler |
| `GATEWAY_REGISTER` / `GATEWAY_EXPIRED` | Gateway 注册/过期（预留） | `GatewayRegistry` |
| `GRPC_CALL` | gRPC 调用 | gRPC Interceptor |

#### 11.4.3 状态字段 (status)

| 状态 | 含义 |
|------|------|
| `OK` | 成功 |
| `FAIL` | 失败 |
| `TIMEOUT` | 超时 |
| `RETRY` | 重试中 |

#### 11.4.4 错误归因 (reason)

`reason` 字段是自证能力的核心。当 level >= WARN 时，日志必须携带 reason，明确错误归属：

| reason 值 | 含义 | 归属方 |
|-----------|------|--------|
| `UPSTREAM_TIMEOUT` | 后端推理超时 | 后端 |
| `UPSTREAM_CONN_REFUSED` | 后端连接拒绝 | 后端 |
| `UPSTREAM_5XX` | 后端返回 5xx | 后端 |
| `NO_HEALTHY_BACKEND` | 无可用后端实例 | 配置/探活 |
| `ALL_OVERLOADED` | 所有实例过载 | 容量不足 |
| `SCHEDULER_SLOW` | event-loop 响应慢 | 调度器 |
| `SCHEDULER_NOT_SERVING` | Scheduler 未在 SERVING 状态 | 控制面 |
| `RELEASE_FAILED` | 释放分配失败 | 内部 |
| `PROXY_ERROR` | 代理转发失败 | 网关/后端 |
| `INTERNAL_ERROR` | 其他内部错误 | 网关 |

使用示例：

```go
reqLog.Error("proxy failed",
    logger.Event(logger.EventProxyComplete),
    logger.Status(logger.StatusFail),
    logger.Reason(logger.ReasonProxyError),      // ← 一眼看出归属
    zap.String("instance", inst.ID),
    zap.Duration("proxy_latency", proxyLatency),
    zap.Error(proxyErr))
```

#### 11.4.5 性能考虑

- `Event()` / `Status()` / `Reason()` 返回 `zap.String`，**零分配**
- 事件常量是编译期字符串常量，无运行时开销
- reason 值是有限枚举，可直接匹配 Prometheus 标签或告警规则

### 11.5 请求上下文 Logger

核心文件：[ctxlogger.go](pkg/logger/ctxlogger.go)

通过 `context.Context` 传递请求级 Logger，一次构建，全链路自动携带 `trace_id` / `request_id`：

```go
// 中间件中构建一次
reqLogger := base.With(
    zap.String("trace_id", traceID),
    zap.String("request_id", requestID),
    zap.String("gw", gatewayAddr),
)
ctx = logger.WithLogger(r.Context(), reqLogger)

// 任何下游通过 ctx 获取，自动携带所有字段
reqLog := logger.FromContext(ctx)
reqLog.Error("allocate failed", ...)  // 自动包含 trace_id, request_id, gw
```

**解决的核心问题**：改造前错误路径缺失 trace_id——`allocate failed`、`proxy failed`、`release failed` 等错误日志无法与请求关联。改造后所有路径统一通过 ctx logger 输出。

`FromContext` 在 ctx 中无 logger 时返回 `zap.NewNop()`，保证调用方安全。

### 11.6 HTTP Access Log 中间件

核心文件：[middleware.go](pkg/logger/middleware.go)

中间件包装每个 HTTP 请求，提供完整的请求生命周期日志：

```
请求入口                                           请求出口
   │                                                  │
   ▼                                                  ▼
REQUEST_START (Debug)                          REQUEST_COMPLETE (Info)
├ method, path, remote_addr                    ├ status, latency, bytes
├ trace_id, request_id, gw                     ├ trace_id, request_id, gw
└ 即使 hang 住也有入口痕迹                        └ 总延迟（含网关+后端）

                   ┌──── 超过阈值 ────┐
                   │                  │
                   ▼
             SLOW_REQUEST (Warn)
             ├ elapsed
             └ 即使在 Info 级别也会触发
```

**关键设计**：

| 特性 | 实现方式 |
|------|---------|
| 慢请求检测 | `time.AfterFunc(threshold)` 定时器，超 30s 主动触发 Warn |
| SSE 兼容 | `statusWriter` 实现 `http.Flusher` + `Unwrap()` |
| 探针跳过 | `/healthz`、`/readyz`、`/metrics` 不记日志；Admin 端口单独运行时不挂载 AccessLog 中间件，从根本上避免 K8s 探测淹没 |
| 状态码捕获 | 包装 `ResponseWriter`，记录首次 `WriteHeader` 的 code |
| 字节计量 | `atomic.Int64` 累加 `Write` 字节数（并发安全） |

### 11.7 Gateway 请求热路径日志设计

核心文件：[server.go](internal/gateway/server.go)

`HandleChatCompletion` 的三阶段日志设计，实现问题定位和 hang 检测：

```
Gateway.HandleChatCompletion
  │
  │  reqLog = logger.FromContext(ctx)
  │
  ├── [Debug] "phase: allocate"     ← 阶段标记 ①
  │     event=ALLOCATE
  │
  │  ← 如果 hang 在 Allocate，最后一条日志就是 ① ──┐
  │                                                │
  ├── [Debug] "forwarding to backend"              │  排查时开 Debug
  │     event=PROXY_FORWARD                        │  即可看到 hang 位置
  │     target, allocation_id                      │
  │                                                │
  ├── [Debug] "phase: proxy"        ← 阶段标记 ② ──┘
  │     instance
  │
  │  proxyStart = time.Now()
  │  s.proxy.Forward(w, r, endpoint)
  │  proxyLatency = time.Since(proxyStart)
  │
  ├── [Error] "proxy failed"        ← 仅失败时
  │     event=PROXY_COMPLETE, status=FAIL
  │     reason=PROXY_ERROR
  │     instance, proxy_latency, error
  │
  ├── [Debug] "proxy completed"     ← 成功时
  │     event=PROXY_COMPLETE, status=OK
  │     instance, proxy_latency          ← 关键：后端延迟
  │
  ├── [Debug] "phase: release"      ← 阶段标记 ③
  │     instance
  │
  └── releaseWithRetry(ctx, reqLog, ...)
        ├── [Warn] retry: event=RELEASE, status=RETRY, reason=RELEASE_FAILED
        └── [Error] final: event=RELEASE, status=FAIL, reason=RELEASE_FAILED
```

**关键变更（vs 改造前）**：

| 变更 | 改造前 | 改造后 | 目的 |
|------|--------|--------|------|
| forwarding 级别 | **Info**（每请求触发） | **Debug**（生产不输出） | 100K QPS 下消除 100K 次 write/s |
| 错误路径 trace_id | 缺失（用 `s.logger`） | 自动携带（用 `reqLog`） | 错误日志可关联请求 |
| proxy 耗时 | 不记录 | `proxy_latency` 字段 | 区分网关 vs 后端延迟 |
| reason 字段 | 无 | 标准 reason 常量 | 一眼看出问题归属 |
| 阶段标记 | 无 | `phase: allocate/proxy/release` | hang 住时定位阶段 |

### 11.8 gRPC Interceptor

核心文件：[grpc_interceptor.go](pkg/logger/grpc_interceptor.go)

Server 端和 Client 端统一拦截器，记录 gRPC 方法调用的延迟和状态：

```go
// Server 端 — scheduler gRPC 服务
grpc.NewServer(grpc.UnaryInterceptor(logger.UnaryServerInterceptor(bundle.Sub("grpc"))))

// Client 端 — gateway 连接 scheduler 时
grpc.NewClient(addr, grpc.WithUnaryInterceptor(logger.UnaryClientInterceptor(bundle.Sub("grpc-client"))))
```

| 结果 | 级别 | 字段 |
|------|------|------|
| 成功 | Debug | event=GRPC_CALL, method, latency, code=OK |
| 失败 | Warn | event=GRPC_CALL, method, latency, code, error |

### 11.9 RemoteAllocator 日志

核心文件：[allocator.go](internal/gateway/allocator.go)

`RemoteAllocator` 新增 `logger` 字段，记录每次 gRPC RPC 的耗时和结果：

| 方法 | 成功 | 失败 |
|------|------|------|
| `Allocate` | Debug: trace_id, instance_id, rpc_latency | Error: trace_id, rpc_latency, error |
| `Release` | 静默（成功是常态） | Error: allocation_id, rpc_latency, error |

### 11.10 Hang 检测机制

三层检测确保请求 hang 住时有日志可查：

| 层级 | 机制 | 级别 | 触发条件 |
|------|------|------|---------|
| 1. 阶段标记 | `phase: allocate/proxy/release` | Debug | 每个阶段入口 |
| 2. 慢请求告警 | `time.AfterFunc(30s)` | **Warn** | 请求超过 30s 未完成 |
| 3. 请求入口 | `REQUEST_START` | Debug | 请求一进来就打 |

**排查流程**：

```
1. 运维发现请求超时
2. 搜索 trace_id → 找到 REQUEST_START，确认请求到达网关
3. 搜索 SLOW_REQUEST → 确认请求确实慢（Info 级别可见）
4. 如需定位 hang 阶段 → 动态切 Debug：
   curl -X PUT /v1/admin/log-level -d '{"access_level":"debug"}'
5. 看最后一条 phase 日志 → 确定 hang 在 allocate / proxy / release
6. 排查完恢复：
   curl -X PUT /v1/admin/log-level -d '{"access_level":"info"}'
```

### 11.11 运行时级别调整

核心文件：[app.go](internal/app/app.go) — `handleLogLevel`

端点 `PUT /v1/admin/log-level` 支持**全局**和 **per-module** 两种粒度：

```bash
# 查询当前级别
GET /v1/admin/log-level
# Response:
{"level":"info","access_level":"info","modules":{"access":"info","grpc":"info","allocator":"info"}}

# 全局调整 — 管控日志降到 debug
PUT /v1/admin/log-level
{"level":"debug"}

# 全局调整 — 请求日志降到 debug
PUT /v1/admin/log-level
{"access_level":"debug"}

# Per-module 调整 — 仅调整 allocator 模块到 debug
PUT /v1/admin/log-level
{"module":"allocator","level":"debug"}
```

**设计优势**：生产排查时可只开启特定模块的 Debug（如仅开 `allocator` Debug 看 RPC 延迟），避免全局 Debug 带来的日志风暴。

### 11.12 自证能力矩阵

优化后，运维可通过以下日志链快速定位问题归属：

```
                    REQUEST_COMPLETE
                    total_latency = 5.2s
                          │
          ┌───────────────┴───────────────┐
          │                               │
    PROXY_COMPLETE                   差值 = 网关开销
    proxy_latency = 5.1s             = 5.2s - 5.1s = 0.1s
          │
          │  如果 proxy_latency ≈ total_latency → 问题在后端
          │  如果差值大 → 问题在网关调度链路
```

完整的日志追踪链：

| 阶段 | 日志 event | 级别 | 关键字段 |
|------|-----------|------|---------|
| 请求入口 | `REQUEST_START` | Debug | trace_id, method, path |
| 请求出口 | `REQUEST_COMPLETE` | Info | trace_id, status, **latency** (总延迟) |
| 慢请求 | `SLOW_REQUEST` | Warn | trace_id, elapsed |
| 分配 | `ALLOCATE` OK/FAIL | Debug/Error | trace_id, instance, allocation_id, **reason** |
| 代理转发 | `PROXY_FORWARD` | Debug | trace_id, target, allocation_id |
| 代理完成 | `PROXY_COMPLETE` OK/FAIL | Debug/Error | trace_id, instance, **proxy_latency**, **reason** |
| 释放 | `RELEASE` retry/fail | Warn/Error | trace_id, instance, attempt, **reason** |
| 批次处理 | `BATCH_PROCESSED` | Debug | batch_size, channel_depth |
| 调度失败 | `ALLOCATE` FAIL | Warn | failed, total, candidates, **reason** |
| Step 开始 | `STEP_START` | Info | step_id, policy, candidates |
| Step 结束 | `STEP_END` | Info | step_id, alloc_dedup_entries, release_dedup_entries |
| Gateway 注册 | `GATEWAY_REGISTER` | Info | gateway_addr, total_gateways |
| Gateway 过期 | `GATEWAY_EXPIRED` | Warn | gateway_addr, since_last_heartbeat, timeout |
| gRPC 调用 | `GRPC_CALL` | Debug/Warn | method, latency, code |

### 11.13 配置

[config.go](internal/config/config.go) 中 `LogConfig` 扩展：

```yaml
log:
  level: info            # 管控日志级别（scheduler 生命周期、错误等）
  access_level: info     # 请求日志级别（access log、proxy 日志等）
  format: console        # "json" (生产) 或 "console" (开发)
  slow_request: 30s      # 慢请求告警阈值
  log_dir: ""            # 日志目录；空 = 仅 stderr（开发模式）
  max_size_mb: 500       # 单文件上限 MB（lumberjack 轮转）
  max_backups: 5         # 保留历史文件数
  max_age_days: 30       # 历史文件最大保留天数
  compress: true         # gzip 压缩历史文件
```

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `level` | `info` | 管控核心级别，控制 scheduler、registry、notifier 等组件 |
| `access_level` | `info` | 请求核心级别，控制 access log、proxy、allocator 等热路径 |
| `format` | `console` | 日志格式，生产环境建议 `json` 便于日志平台解析 |
| `slow_request` | `30s` | 慢请求 Warn 告警阈值，支持 `time.Duration` 格式 |
| `log_dir` | `""` | 日志文件目录；空则仅输出到 stderr（开发模式） |
| `max_size_mb` | `500` | 单日志文件轮转阈值（MB），由 lumberjack 管理 |
| `max_backups` | `5` | 每类日志保留的历史文件数 |
| `max_age_days` | `30` | 历史日志文件最大保留天数 |
| `compress` | `true` | 是否 gzip 压缩已轮转的历史文件 |

### 11.14 文件结构

```
pkg/logger/
├── logger.go            # Bundle 双独立 Logger 架构 + moduleCore + Sub/SubAccess/Close
├── fields.go            # event/status/reason 常量 + Event()/Status()/Reason() 辅助函数
├── ctxlogger.go         # WithLogger/FromContext — 请求上下文 Logger
├── middleware.go         # AccessLogMiddleware + statusWriter (SSE 兼容)
└── grpc_interceptor.go  # UnaryServerInterceptor + UnaryClientInterceptor
```

### 11.15 Scheduler 日志增强

#### Scheduler 双日志分流

核心文件：[server.go](internal/scheduler/server.go)

Scheduler `Server` 拥有两个独立的 Logger 字段，按**"日志是否关联到具体请求"**分流：

```go
type Server struct {
    logger    *zap.Logger // 控制面：step 生命周期、gateway 事件、系统错误
    accessLog *zap.Logger // 请求面：allocate/release/dedup/batch 等请求级日志
}
```

`accessLog` 由 `bundle.SubAccess("scheduler")` 创建，写入 `access.log`（异步、带采样）；`logger` 由 `bundle.Sub(...)` 或直接使用 `bundle.Root` 创建，写入 `router.log`（同步、保证输出）。

**分流原则**：按"这条日志是否关联到某条具体请求"划分，而非按日志级别。同一类日志的所有级别（Debug/Warn/Error）写入同一个文件，确保 `grep trace_id access.log` 即可看到请求的完整生命周期。

| 日志点 | 写入目标 | 理由 |
|--------|---------|------|
| `allocated` (Debug) | `accessLog` | 某条请求的分配结果 |
| `released` (Debug) | `accessLog` | 某条请求的释放 |
| `allocate dedup hit` (Debug) | `accessLog` | 某条请求的去重命中 |
| `release dedup hit` (Debug) | `accessLog` | 某条请求的释放去重 |
| `batch processed` (Debug) | `accessLog` | 一批请求的处理摘要 |
| `batch allocate rejected` (Warn) | `accessLog` | 一批请求因 phase 被拒 |
| `allocation failures in batch` (Warn) | `accessLog` | 一批请求中的分配失败 |
| `step started/ended/draining` (Info) | `logger` | 系统生命周期事件 |
| `step draining complete` (Info) | `logger` | 系统状态转换 |
| `scheduler control-plane http request completed` (Info) | `logger` | Scheduler 控制面 HTTP 请求/响应全量 dump，包含 method/path/query/header/body/status/response_header/response_body/latency；跳过 health/readyz/metrics/pprof 等高频探针 |
| `gateway re-registered/cleaned` (Warn) | `logger` | 系统运维事件 |

**排查场景**：
- **查某条请求**：`grep trace_id access.log` — 完整请求链路（包含 Warn/Error）
- **查系统事件**：看 `router.log` — step 状态变更、gateway 注册/清理

#### StepPhaseGauge / StepIDGauge 补充（修复 P2 #8）

`metrics.StepPhaseGauge` 和 `metrics.StepIDGauge` 已定义但此前从未写入。现在在以下位置补充 `.Set()`：

- `handleStartStep` → `StepPhaseGauge.Set(SERVING)` + `StepIDGauge.Set(stepID)`
- `handleEndStep` → `StepPhaseGauge.Set(IDLE 或 DRAINING)`
- `handleRelease`（DRAINING→IDLE）→ `StepPhaseGauge.Set(IDLE)`
- `handleCleanupGateway`（DRAINING→IDLE）→ `StepPhaseGauge.Set(IDLE)`

#### 批次指标日志

`processBatch` 末尾，当 batch_size > 1 时记录 Debug 日志：

```
event=BATCH_PROCESSED batch_size=128 channel_depth=50432
```

`channel_depth` 接近 131072 表示 event-loop 处于背压，运维可据此扩容。

#### 调度决策日志

- **分配被拒**（phase 非 SERVING）：Warn + event=ALLOCATE + reason=SCHEDULER_NOT_SERVING
- **批次内分配失败**：Warn + event=ALLOCATE + reason=ALL_OVERLOADED + failed/total/candidates
- **Step 边界**：Info + event=STEP_START/STEP_END + alloc_dedup_entries + release_dedup_entries

#### 冗余日志清理

`grpc_handler.go` 的 `Heartbeat` 方法移除了 `"heartbeat from unregistered gateway"` Warn 日志，因为 `gateway_registry.go` 的 `Heartbeat` 方法已记录相同信息。

### 11.16 性能影响分析

| 变更 | 100K QPS 下影响 | 原因 |
|------|----------------|------|
| forwarding Info → Debug | **-100K write/s** | 单项最大优化 |
| BufferedWriteSyncer | write 系统调用 100K/s → ~80/s | 256KB 缓冲批量写入 |
| Access log Sampling | 持续高负载下 100K → ~100 条/s | sampler 降低稳态日志量 |
| ctx logger 构建 | +1 alloc/req (`zap.With`) | 相比 allocate/release 开销可忽略 |
| gRPC interceptor | +1 `time.Now()`/RPC | <100ns |

**净效果**：生产 Info 级别下，每请求的同步写入从 1 次降为 0 次（access log 通过 buffer 异步写），系统调用开销降低 **~1000x**。

### 11.17 运维可观测增强

#### 11.17.1 Panic Recovery（修复 P0 #2）

核心文件：[recovery.go](pkg/logger/recovery.go)

HTTP 和 gRPC 均加入 panic recovery，防止单个请求的 panic 导致进程崩溃：

| 层 | 实现 | 触发时行为 |
|----|------|-----------|
| HTTP | `RecoveryMiddleware` — 最外层 wrapper | 记 Error 日志 + stack trace → 返回 500 |
| gRPC | `RecoveryUnaryServerInterceptor` — 链式拦截器最外层 | 记 Error 日志 + stack trace → 返回 `codes.Internal` |

两者都会增加 `rl_router_panic_recoveries_total{layer="http"|"grpc"}` 计数器。

拦截器链顺序（gRPC）：Recovery → Logging → Handler

```go
// app.go serveGRPC()
chained := logger.ChainUnaryServer(
    logger.RecoveryUnaryServerInterceptor(log),
    logger.UnaryServerInterceptor(log),
)
```

#### 11.17.2 Event-loop 心跳与背压指标

核心文件：[server.go](internal/scheduler/server.go) — `logHeartbeat()`

| 指标/日志 | 类型 | 说明 |
|----------|------|------|
| `event-loop alive` | Info 日志（router.log） | 每 30s 输出一次，含 phase/step_id/active_requests/channel_depth/instances |
| `rl_router_event_channel_depth` | Gauge | event channel 当前深度，每批处理后 + 每 30s 心跳时更新 |
| `rl_router_event_loop_batch_size` | Histogram | 每批处理的事件数分布，桶：1, 2, 4, ..., 8192 |

**排查流程 — event-loop 是否存活**：

```
1. 检查 router.log 是否有 30s 内的 "event-loop alive" 日志
   - 有 → event-loop 正常运行
   - 无 → event-loop 可能死锁或进程假死
2. 检查 channel_depth 是否持续增长
   - 接近 131072 → event-loop 处理能力不足，需排查慢操作
3. Prometheus: rl_router_event_channel_depth > 100000 → 告警
```

#### 11.17.3 后端 Response Status 记录（举证能力增强）

核心文件：[server.go](internal/gateway/server.go) — `statusCapturer`

代理转发阶段新增 `backend_status` 字段，记录后端返回的 HTTP 状态码：

```
PROXY_COMPLETE status=OK instance=vllm-0 backend_status=200 proxy_latency=1.5s
PROXY_COMPLETE status=OK instance=vllm-1 backend_status=500 proxy_latency=0.1s
```

新增 Prometheus 指标：

| 指标 | 标签 | 说明 |
|------|------|------|
| `rl_router_proxy_backend_status_total` | `instance_id`, `status_class` (2xx/4xx/5xx) | 后端返回的 HTTP 状态码分布 |

**举证场景 — 外部请求结果不符合预期**：

```
1. 通过 trace_id 找到 PROXY_COMPLETE 日志
2. 检查 backend_status:
   - 200 → 后端返回正常，问题在客户端解析
   - 400/500 → 后端返回错误，router 原样转发
   - 0 → 连接级错误（connection refused / timeout），对应 PROXY_COMPLETE status=FAIL
3. 对比 REQUEST_COMPLETE.status 和 backend_status:
   - 两者一致 → router 未篡改响应
   - REQUEST_COMPLETE.status=200 but backend_status=500 → 不可能，说明观测方式有误
```

#### 11.17.4 采样参数可配置

核心文件：[logger.go](pkg/logger/logger.go) — `buildAccessCore()`

新增配置字段：

```yaml
log:
  sample_initial: 100    # 每秒前 N 条全量记录（默认 100）
  sample_thereafter: 1000 # 之后 1/N 采样（默认 1000）
  # 两个都设为 0 → 禁用采样，全量记录（适合低 QPS GPU 调度场景）
```

**GPU 调度场景建议**：

```yaml
log:
  sample_initial: 0
  sample_thereafter: 0
```

QPS < 1000 时全量记录的磁盘开销 < 10MB/分钟（JSON 格式），性能影响可忽略，但每条请求都有完整日志链路。

#### 11.17.5 运维排查手册

##### 场景 1：系统 hang 住

```
1. 检查 router.log 最后一条 "event-loop alive" 日志
   ├── 30s 内有 → event-loop 正常，问题在其他地方
   │   └── 检查 channel_depth 是否异常高 → event-loop 慢处理
   └── 超过 60s 无 → event-loop 死锁
       └── 检查进程是否存活：curl /healthz
           ├── 可达 → 进程活着但 event-loop 死锁
           │   └── goroutine dump: curl /debug/pprof/goroutine?debug=2
           └── 不可达 → 进程已崩溃
               └── 检查 router.log 最后几行是否有 PANIC_RECOVERED

2. 检查 access.log 是否有 SLOW_REQUEST
   ├── 有 → 请求在某阶段 hang
   │   └── 动态切 Debug: PUT /v1/admin/log-level {"access_level":"debug"}
   │   └── 看最后的 phase 日志 → hang 在 allocate / proxy / release
   └── 无 → 请求未到达 gateway（网络/DNS/TLS 问题）

3. 检查 Prometheus
   ├── rl_router_step_phase != 1 → scheduler 不在 SERVING
   ├── rl_router_active_requests 持续不降 → 请求堆积
   └── rl_router_event_channel_depth 接近 131072 → event-loop 背压
```

##### 场景 2：请求结果不符合预期

```
1. grep trace_id access.log → 找到完整请求链
2. 检查 PROXY_COMPLETE.backend_status
   ├── 200 → 后端正常返回，router 原样转发
   ├── 4xx/5xx → 后端返回错误
   │   └── 检查 backend 日志确认
   └── 0 → 连接级错误
       └── 检查 PROXY_COMPLETE.reason (PROXY_ERROR)

3. 对比延迟
   ├── REQUEST_COMPLETE.latency ≈ PROXY_COMPLETE.proxy_latency → 慢在后端
   └── REQUEST_COMPLETE.latency >> proxy_latency → 慢在调度或释放
```

##### 场景 3：大量请求下如何判断系统健康

```
1. 即使 access.log 被采样，Prometheus 指标是全量的：
   ├── rl_router_requests_total → 实际处理请求总数
   ├── rl_router_request_duration_ms → 延迟分布
   └── rl_router_active_requests → 当前在途请求

2. router.log 中的 "event-loop alive" 每 30s 一行：
   └── active_requests + channel_depth → 宏观健康状态

3. 如需恢复全量日志（排查特定问题）：
   └── PUT /v1/admin/log-level {"access_level":"debug"}
   └── 或设置 sample_initial=0, sample_thereafter=0 后重启

4. 告警建议（Alertmanager rules）：
   ├── rl_router_event_channel_depth > 100000 持续 1min → event-loop 背压
   ├── rate(rl_router_requests_total{status="error"}[5m]) / rate(rl_router_requests_total[5m]) > 0.05 → 错误率 > 5%
   ├── rl_router_active_requests > 5000 持续 5min → 请求堆积
   └── absent(rl_router_step_phase) 持续 1min → 指标上报中断
```

##### 场景 4：Gateway 断连恢复

```
1. router.log: "cleaned up ghost allocations" — Scheduler 清理了断连 Gateway 的幽灵分配
2. router.log: GATEWAY_REGISTER — Gateway 重新注册成功
3. Gateway 侧日志: "re-registered" — 确认 phase/step_id 已同步
4. Prometheus: rl_router_registered_gateways → 确认 Gateway 数量恢复
```

## 12. 限流与并发控制

### 12.1 设计背景

RL 训练场景中，step 切换瞬间可能涌入大量并发请求冲击后端 GPU 实例；未来在线推理场景也需要 RPS 速率限制保护后端不过载。参考 SGLang Router 双层设计（local token bucket + mesh global counter），结合 OneRouter 中心化 Scheduler 架构的优势，实现 **三层并发控制** 方案。

### 12.2 三层架构

```
                            ┌──────────────────────┐
                            │   Scheduler (全局)    │
                            │  globalMaxInflight    │
                            │  event-loop 单线程    │
                            │  精确 activeCount     │
                            └──────────┬───────────┘
                                       │
                                       │ 策略层 (per-instance)
                                       │ MaxRequestLoad
                                       │
                   ┌───────────────────┼───────────────────┐
                   │                   │                   │
            ┌──────┴──────┐     ┌──────┴──────┐     ┌──────┴──────┐
            │  Gateway-1  │     │  Gateway-2  │     │  Gateway-N  │
            │ FlowControl │     │ FlowControl │     │ FlowControl │
            │ (本地限流)   │     │ (本地限流)   │     │ (本地限流)   │
            └─────────────┘     └─────────────┘     └─────────────┘
```

三层各司其职，不应合并为单一参数：

| 层级 | 参数 | 作用域 | 执行位置 | 语义 |
| --- | --- | --- | --- | --- |
| Layer 1 — Scheduler 全局限流 | `GlobalMaxInflight` | 全局（所有实例 × 所有 gateway） | event-loop, policy 之前 | 系统级过载保护：总 in-flight 上限 |
| Layer 2 — 策略层单实例限流 | `MaxRequestLoad` | 单个后端实例 | policy Select/BatchSelect 内部 | 实例级过载保护：单实例活跃请求/负载分数上限 |
| Layer 3 — Gateway 本地限流 | `MaxConcurrentRequests` / `RatePerSecond` | 单个 gateway 进程 | gateway 入口，RPC 之前 | 本地快速拒绝，减少无效 RPC |

#### Layer 2 — MaxRequestLoad（单实例负载上限）

`MaxRequestLoad` 是策略层对**单个后端实例**的活跃请求（或复合负载分数）上限。超过此值的实例在调度时会被跳过。

使用此参数的策略：

| 策略 | 判定条件 | 默认值 |
| --- | --- | --- |
| `round_robin` | `ActiveRequests >= MaxRequestLoad` → 跳过 | 128 |
| `min_request` | `minActiveRequests > MaxRequestLoad` → 全部过载 | 128 |
| `min_load` | 复合分数 `score > MaxRequestLoad` → 跳过 | 128 |
| `cache_aware` | `requestLoad >= MaxRequestLoad` → 跳过 | 100 |

配置方式：
- **YAML 默认值**：`max_request_load: 128`（顶层 Config，启动时生效）
- **StartStep API 覆盖**：`POST /v1/steps/start` 中传 `"max_request_load": 64`（per-step 动态调整）
- **V2 兼容**：`POST /api/v2/start_infer` 中传 `"max_request_load": 64`
- **零值**：表示使用策略默认值（round_robin/min_load/min_request = 128, cache_aware = 100）

### 12.3 技术选型

| 模式 | 引擎 | 来源 | 用途 |
| --- | --- | --- | --- |
| 并发控制 (semaphore) | `golang.org/x/sync/semaphore.Weighted` | go.mod 已有 | RL 训练场景：限制同时在途请求数 |
| 速率限制 (token bucket) | `golang.org/x/time/rate.Limiter` | go.mod 已有 | 在线推理场景：限制 RPS + burst |

**为什么用 stdlib-x 而不自建 `chan struct{}`**:

- `semaphore.Weighted` 有 FIFO 保证（内部 wait list），`chan struct{}` 在高竞争下调度是伪随机的
- `rate.Limiter` 正确实现 token bucket refill、burst、Reservation 取消
- 两者都原生支持 `context.Context` 取消和超时

**为什么不需要 SGLang 的 TokenGuardBody 模式**:
Go 的 `httputil.ReverseProxy.ServeHTTP()` 是同步阻塞的——把后端响应逐块 flush 到客户端，直到 Body 读完才返回。`defer limiter.Release()` 天然在流完成后才执行。

### 12.4 Gateway FlowController 接口

```go
type FlowController interface {
    Acquire(ctx context.Context) error  // 阻塞获取令牌
    TryAcquire() bool                  // 非阻塞快路径
    Release()                          // 归还令牌
}
```

两种实现：

| 维度 | ConcurrencyLimiter | RateLimiter |
| --- | --- | --- |
| 引擎 | `semaphore.Weighted` | `rate.Limiter` |
| 控制维度 | 同时在途请求数 | 每秒请求数 + burst |
| Token 回收 | `Release()` 手动归还 | 自动 refill（Release 为 no-op） |
| 典型场景 | RL 训练 rollout | 在线推理服务 |

两种模式共享排队机制：bounded queue + queue timeout，超限返回 HTTP 429。

### 12.5 Scheduler 全局限流

`Server` 新增 `globalMaxInflight int64` 字段（event-loop 单线程，plain field，零锁）。在 `batchAllocate()` 的 phase check 之后、dedup/policy 之前检查：

- `headroom = globalMaxInflight - activeCount`
- `headroom <= 0`：全部拒绝
- `headroom < len(pending)`：前 headroom 个通过，其余拒绝（partial admission）
- 被拒绝的请求返回 `ErrGlobalRateLimitExceeded`，gRPC 映射为 `codes.ResourceExhausted`，HTTP 映射为 429

### 12.6 配置

```yaml
# 顶层策略参数 — 启动时生效，可被 StartStep API 覆盖
policy: min_load
max_request_load: 128              # 单实例最大请求负载（0 = 策略默认值）

# 场景 1: RL 训练 — 并发控制模式
rate_limit:
  global_max_inflight: 5000        # Scheduler 全局上限
  enabled: true                     # Gateway 本地限流开关
  mode: concurrency                 # concurrency | rate
  max_concurrent_requests: 1000
  queue_size: 500
  queue_timeout: 10s

# 场景 2: 在线推理 — 速率限制模式
rate_limit:
  global_max_inflight: 0           # 0 = 不限
  enabled: true
  mode: rate
  rate_per_second: 500.0
  burst: 100
  queue_size: 200
  queue_timeout: 5s
```

### 12.7 可观测指标

| 指标 | 类型 | 说明 |
| --- | --- | --- |
| `rl_router_global_rate_limit_rejects_total` | Counter | Scheduler 全局限流拒绝数 |
| `rl_router_global_rate_limit_headroom` | Gauge | 全局剩余容量 |
| `rl_router_rate_limit_total{result}` | CounterVec | Gateway 限流结果（acquired/queued_acquired/rejected_full/rejected_timeout） |
| `rl_router_rate_limit_concurrent_requests` | Gauge | Gateway 当前在途请求数 |
| `rl_router_rate_limit_queue_depth` | Gauge | Gateway 当前排队数 |
| `rl_router_rate_limit_wait_duration_ms` | Histogram | Gateway 排队等待时长 |

### 12.8 请求流经路径

```
Client Request
  → [Gateway] Step-check (503 if not serving)
  → [Gateway] FlowController.Acquire (429 if rejected)            ← Layer 3: 本地限流
  → [Gateway] Read body
  → [Gateway] requestLifecycle starts audit / inflight / request_id
  → [Gateway] Allocate RPC to Scheduler
  → [Scheduler] batchAllocate: global limit check (429 if rejected)  ← Layer 1: 全局限流
  → [Scheduler] dedup + policy select (MaxRequestLoad filter)        ← Layer 2: 单实例限流
  → [Gateway] Proxy forward to backend
  → [Gateway] FlowController.Release (defer)
  → [Gateway] requestLifecycle.finalize → Release RPC + audit emit
```

---

## 13. 可观测性体系

OneRouter 的可观测性覆盖三个维度：**Prometheus 指标**（全量、实时、低开销）、**OpenTelemetry 分布式 Tracing**（请求链路追踪）、**结构化日志**（zap，上下文丰富）。

### 13.1 Prometheus 指标全景

所有指标以 `rl_router_` 为命名空间，使用 `promauto` 自注册，无需手动 `Register()`。高频指标通过 `InstanceMetrics` 预缓存（`sync.Map`），每请求仅 1 次 atomic load。

#### 13.1.1 请求路径指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_requests_total` | CounterVec | instance_id, status | 路由请求总数（ok/error） |
| `rl_router_active_requests` | GaugeVec | instance_id | 当前在途请求数 |
| `rl_router_request_duration_ms` | HistogramVec | instance_id | 请求时长（10ms~20s） |
| `rl_router_time_to_first_byte_ms` | HistogramVec | instance_id, mode | TTFT（stream/non_stream） |
| `rl_router_stream_completion_total` | CounterVec | instance_id, result | SSE 流完成状态（done/interrupted/error） |
| `rl_router_tokens_prompt_total` | CounterVec | instance_id | 非流式 prompt token 累计 |
| `rl_router_tokens_completion_total` | CounterVec | instance_id | 非流式 completion token 累计 |
| `rl_router_instance_sessions` | GaugeVec | instance_id | 每个实例当前活跃的 session 数量 |
| `rl_router_non_stream_retries_total` | Counter | - | 非流式重试次数 |
| `rl_router_proxy_backend_status_total` | CounterVec | instance_id, status_class | 后端 HTTP 状态码分布（2xx/4xx/5xx） |

#### 13.1.2 Scheduler 内部指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_event_channel_depth` | Gauge | - | 事件队列深度 |
| `rl_router_event_loop_batch_size` | Histogram | - | 每次 batch 处理的事件数 |
| `rl_router_event_loop_batch_duration_ms` | Histogram | - | 每次 batch 处理耗时（0.01ms~327ms） |
| `rl_router_alloc_queue_wait_ms` | Histogram | - | Allocate 事件在 channel 中等待时间（0.1ms~409ms） |
| `rl_router_policy_select_duration_ms` | Histogram | - | 策略选择耗时（1us~65ms） |
| `rl_router_alloc_dedup_hits_total` | Counter | - | Allocate 去重命中数 |
| `rl_router_alloc_caller_gone_skips_total` | Counter | - | 调用方已取消的 Allocate 跳过数 |
| `rl_router_release_dedup_hits_total` | Counter | - | Release 去重命中数 |
| `rl_router_alloc_dedup_size` | Gauge | - | Allocate dedup map 当前大小 |
| `rl_router_release_dedup_size` | Gauge | - | Release dedup map 当前大小 |
| `rl_router_step_phase` | Gauge | - | 当前 step 阶段（0=IDLE, 1=SERVING, 2=DRAINING） |
| `rl_router_step_id` | Gauge | - | 当前 step ID |

#### 13.1.3 Gateway / 网络指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_registered_gateways` | Gauge | - | 已注册 gateway 数 |
| `rl_router_heartbeat_total` | CounterVec | gateway_id | 心跳接收次数 |
| `rl_router_release_retries_total` | Counter | - | Release 重试次数 |
| `rl_router_remote_alloc_latency_ms` | HistogramVec | method | RemoteAllocator RPC 延迟（allocate/release） |

#### 13.1.4 gRPC 四项指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_grpc_server_requests_total` | CounterVec | method, code | gRPC server 请求计数 |
| `rl_router_grpc_server_request_duration_ms` | HistogramVec | method | gRPC server 延迟（0.1ms~3.2s） |
| `rl_router_grpc_client_requests_total` | CounterVec | method, code | gRPC client 请求计数 |
| `rl_router_grpc_client_request_duration_ms` | HistogramVec | method | gRPC client 延迟（0.1ms~3.2s） |

#### 13.1.5 HTTP 聚合指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_http_request_duration_seconds` | HistogramVec | method, path, status | HTTP 请求延迟 |
| `rl_router_http_requests_in_flight` | Gauge | - | HTTP 当前在途请求数 |

path 标签通过 `normalizePath()` 规范化：截断 query string、超过 64 字符归为 `/other`。

#### 13.1.6 运维 / 稳定性指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_step_notify_total` | CounterVec | result | StepNotifier 推送结果（success/failure） |
| `rl_router_ghost_cleanup_total` | Counter | - | 幽灵分配清理事件数 |
| `rl_router_ghost_cleanup_released_total` | Counter | - | 幽灵清理释放的分配数 |
| `rl_router_pd_alloc_compensations_total` | Counter | - | AllocatePD caller 取消后补偿释放 fresh allocation 的次数 |
| `rl_router_control_plane_ops_total` | CounterVec | op, result | 控制面 handler 调用数 |
| `rl_router_panic_recoveries_total` | CounterVec | layer | panic 恢复次数（http/grpc） |
| `rl_router_health_probe_results_total` | CounterVec | result | 健康探测结果 |
| `rl_router_health_probe_unhealthy_instances` | Gauge | - | 不健康实例数 |
| `rl_router_circuit_breaker_trips_total` | CounterVec | instance_id, transition | 熔断状态转换 |
| `rl_router_metrics_sweep_duration_seconds` | Histogram | - | 指标采集 sweep 耗时 |

#### 13.1.8 取证与断连归因指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_disconnect_total` | CounterVec | source, path | 请求断连归因（source: none/client/backend/timeout/queue_full/queue_timeout/alloc_fail；path: stream/non_stream/queue） |
| `rl_router_inflight_request_age_seconds` | Histogram | - | 在途请求年龄采样（桶位 1/5/10/30/60/120/300/600s），每 5s sweep |

#### 13.1.7 限流指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_global_rate_limit_rejects_total` | Counter | - | Scheduler 全局限流拒绝 |
| `rl_router_global_rate_limit_headroom` | Gauge | - | 全局剩余容量 |
| `rl_router_rate_limit_total` | CounterVec | result | Gateway 限流结果 |
| `rl_router_rate_limit_concurrent_requests` | Gauge | - | Gateway 当前在途 |
| `rl_router_rate_limit_queue_depth` | Gauge | - | Gateway 排队深度 |
| `rl_router_rate_limit_wait_duration_ms` | Histogram | - | Gateway 排队等待时长 |

#### 13.1.9 策略路由分支指标

参考 sglang-router 的 `branch_total` Counter 模式，追踪 session_aware / cache_aware 策略每次路由决策走了哪个分支。

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_policy_branch_total` | CounterVec | policy, branch | 各策略路由分支命中次数 |

**session_aware (V2) 分支：**

| branch | 含义 |
|--------|------|
| `stay` | session 已映射且实例存活，保持亲和 |
| `new_session` | 首次分配 session 到 min-session-load 实例 |
| `stale_reassign` | 旧实例消失，清理映射后重新分配 |
| `no_session_fallback` | 空 SessionID，走 min-load 回退 |

**session_aware_v3 分支：**

| branch | 含义 |
|--------|------|
| `stay` | CompareAndSchedule 判断留在当前实例 |
| `migrate_overload` | `lastLoad >= MaxSessionLoad`，强制迁移 |
| `migrate_load_diff` | `(lastLoad - minLoad) > LoadDiffThreshold`，负载差迁移 |
| `new_session` | 首次分配 |
| `stale_reassign` | 旧实例消失 |
| `no_session_fallback` | 空 SessionID |

**session_aware_v4 分支：**

| branch | 含义 |
|--------|------|
| `stay` | balanced 模式下亲和性保持（warmth 增长） |
| `migrate_overload` | balanced 模式下过载迁移 |
| `migrate_load_diff` | balanced 模式下超过 effectiveThreshold 迁移 |
| `imbalanced_migrate` | imbalanced 模式，忽略亲和，走最短队列 |
| `new_session` | 首次分配 |
| `stale_reassign` | dead instance batch invalidation 后重分配 |
| `no_session_fallback` | 空 SessionID |

**cache_aware 分支：**

| branch | 含义 |
|--------|------|
| `cache_hit` | matchRate > CacheThreshold 且 tenant 可用 |
| `tenant_evict` | matchRate 高但 tenant 不可用/超载，evict 后 fallback |
| `cache_miss` | matchRate 低，走 min-tree-bytes |
| `imbalanced` | imbalanced 模式，走最短队列 |
| `empty_text` | 空 RequestText，走 min-tree-bytes |

**性能设计：** BatchSelect 中使用 stack-local int 变量累加各分支计数，循环结束后一次性 `Add()` 刷到 Prometheus，将 4096 次 atomic op 降到 ~7 次。Counter handle 在策略构造时预解析，热路径无 map 查找。

### 13.2 OpenTelemetry 分布式 Tracing

#### 13.2.1 架构设计

```
                    ┌──────────────────────────────┐
                    │     OTLP Collector           │
                    │ (Jaeger / Tempo / SigNoz)    │
                    └──────────┬───────────────────┘
                               ▲ gRPC OTLP
                               │
            ┌──────────────────┴──────────────────┐
            │         TracerProvider                │
            │  ┌─────────────────────────────┐     │
            │  │ BatchSpanProcessor          │     │
            │  │ └─ OTLPTraceGRPCExporter    │     │
            │  └─────────────────────────────┘     │
            │  ParentBased(TraceIDRatioBased)       │
            └──────────────────────────────────────┘

Disabled mode → noop.TracerProvider（~10ns/span, 零分配）
Enabled mode  → BatchSpanProcessor（异步导出, ~1us/span）
```

#### 13.2.2 Span 层级

完整请求链路的 Span 树：

```
http.request (tracing.HTTPMiddleware)
  └── gateway.inference (server.handleInferenceRequest)
      ├── gateway.rpc.allocate (RemoteAllocator.Allocate)  ← W3C traceparent via gRPC StatsHandler
      │   └── scheduler.allocate (Scheduler.Allocate)
      │       └── scheduler.batch_select (batchAllocate)
      ├── gateway.proxy.stream (streamTracker + ReverseProxy)
      │   └── [backend span]  ← W3C traceparent via InjectHTTP
      └── gateway.rpc.release (RemoteAllocator.Release)
          └── scheduler.release (Scheduler event-loop)

V2 路径:
http.request
  └── gateway.v2.chat (HandleV2ChatCompletion)
      ├── gateway.rpc.allocate → scheduler.allocate
      ├── streamForward → [backend] (traceparent injected)
      └── gateway.rpc.release → scheduler.release

控制面:
scheduler.start_step (handleStartStep)
scheduler.end_step (handleEndStep)
```

#### 13.2.3 Trace Context 传播

| 路径 | 传播机制 | 实现 |
|------|---------|------|
| Client → Gateway | W3C `traceparent` header | `tracing.HTTPMiddleware` 提取 |
| Gateway → Scheduler (gRPC) | W3C `traceparent` 自动传播 | `otelgrpc.NewClientHandler()` / `NewServerHandler()` |
| Gateway → Backend (HTTP) | W3C `traceparent` header | `tracing.InjectHTTP(ctx, req)` |

#### 13.2.4 配置

```yaml
tracing:
  enabled: false          # 默认关闭，零开销
  endpoint: "localhost:4317"  # OTLP gRPC 端点
  service_name: "rl-router"
  sample_rate: 1.0        # 采样率 0.0~1.0
  insecure: true          # 是否跳过 TLS
```

校验规则：`enabled=true` 时 `endpoint` 必填；`sample_rate` 必须在 `[0, 1]` 范围内。

#### 13.2.5 中间件链顺序

HTTP 中间件按以下顺序链接（外层 → 内层）：

```
AccessLog → HTTPMetrics → Tracing → Recovery → mux
```

gRPC 拦截器 + StatsHandler：

```
Server: StatsHandler(otelgrpc) + Chain(metrics → recovery → logging)
Client: StatsHandler(otelgrpc) + Chain(metrics)
```

### 13.3 性能影响

| 改动 | 开销 | 说明 |
|------|------|------|
| gRPC metrics 拦截器 | ~200ns/RPC | 1x `time.Now` + 1x counter + 1x histogram |
| Event-loop batch 延迟 | ~100次/sec | 每 batch 1 次 `time.Now`，非每 event |
| Allocate queue wait | 0 额外分配 | 复用 event 结构体的 `time.Time` 字段 |
| OTel tracing（disabled） | ~10ns/span | noop tracer 零分配 |
| OTel tracing（enabled） | ~1us/span | BatchSpanProcessor 异步导出 |
| HTTP metrics middleware | ~150ns/req | 1x histogram observe |
| Policy select 延迟打点 | ~50ns/batch | 1x `time.Now` + 1x histogram |

### 13.4 请求取证与自证能力（Request Forensics & Self-Proof）

GPU 集群实验（mpirun）不可重跑。用户怀疑 router 导致训练问题时（数据篡改、重试改内容、hang 住、断连归因不清），router 必须**始终在线**地证明自己没问题——不能要求"开 debug 重跑"。

#### 13.4.1 设计原则

1. **始终在线**：所有取证能力默认开启，零配置。审计日志**永不采样**。
2. **可密码学自证**：SHA256 摘要（非 xxHash），因为自证场景需要密码学级别的不可伪造性。
3. **低开销**：SHA256 1KB body ~500ns，`sync.Pool` 复用 hasher/AuditRecord 避免 GC。
4. **单行取证**：每个请求结束时输出一条 `event=AUDIT` 日志，包含完整生命周期时间线 + body 摘要 + 断连归因。

#### 13.4.2 Body 完整性校验（`pkg/forensics/digest.go`）

```go
// BodyDigest 返回 body 的 hex-encoded SHA256。nil/empty → ""。
// 使用 sync.Pool 复用 hash.Hash，避免每请求 2 allocs (~400B)。
func BodyDigest(body []byte) string
```

- 请求到达时计算 `reqDigest = BodyDigest(bodyBytes)`
- 转发前不再读 body（直接用同一份 `bodyBytes`），摘要必然相同 → 自证未篡改
- 重试时同样转发同一份 `bodyBytes`，审计行记录 `req_body_digest`，多次重试摘要一致 → 自证重试未改 body
- 非流式响应 body 已完整缓冲在 `doNonStreamForward` 的 `respBody`，同样计算 `respDigest = BodyDigest(respBody)` → 自证响应未篡改
- 流式响应无法整体 hash，改用 `stream_bytes` + `stream_chunks` + `stream_result` 三字段联合证明

性能基准：
| Body 大小 | 耗时 | 分配 |
|-----------|------|------|
| 1KB | ~500ns | 0 allocs（sync.Pool） |
| 1MB | ~405us | 0 allocs |

#### 13.4.3 断连归因（`pkg/forensics/disconnect.go`）

7 种断连来源，覆盖请求全生命周期中所有中断场景：

| DisconnectSource | 含义 | 触发条件 |
|------------------|------|---------|
| `none` | 正常完成 | 请求成功返回 |
| `client` | 客户端断连 | `ctx.Err() == context.Canceled`（排队/转发阶段） |
| `backend` | 后端断连/错误 | proxy 错误且 client ctx 未取消 |
| `timeout` | 超时 | `ctx.Err() == context.DeadlineExceeded` |
| `queue_full` | 排队满 | rate limiter 拒绝（容量满） |
| `queue_timeout` | 排队超时 | rate limiter 排队等待超时 |
| `alloc_fail` | 分配失败 | Scheduler 无可用实例 |

核心分类函数：

```go
// ClassifyProxyDisconnect 根据 proxyErr + clientCtx.Err() + streamDone 判断断连来源。
// 核心逻辑：clientCtx.Err() == Canceled → client 断了；否则 → backend 问题。
func ClassifyProxyDisconnect(proxyErr error, clientCtx context.Context, streamDone bool) DisconnectSource

// ClassifyTransportError 分类非流式转发错误。
func ClassifyTransportError(err error, clientCtx context.Context) DisconnectSource

// ClassifyAllocError 分类 Allocate 失败原因。
func ClassifyAllocError(err error, clientCtx context.Context) DisconnectSource
```

**client disconnect 语义修复**：`streamForward` 返回 sentinel error `errClientDisconnect` 替代 `nil`。Client 断连既不计入 `RequestsOK` 也不计入 `RequestsError`，独立统计到 `DisconnectTotal{source="client"}`，避免 error 率告警被 client 行为污染。

#### 13.4.4 Inflight 请求老化检测（`pkg/forensics/inflight.go`）

```go
type InflightTracker struct {
    entries sync.Map  // requestID → startNano (int64)
    stopCh  chan struct{}
    done    chan struct{}
    started atomic.Bool
}
```

- 每 5s sweep 一次，计算每个 inflight 请求的 age，写入 `rl_router_inflight_request_age_seconds` histogram
- 桶位：`[1, 5, 10, 30, 60, 120, 300, 600]`s
- 10K 并发 × 1us/entry = 10ms/sweep，可接受
- `Stop()` 在 `Start()` 未调用时为 no-op（`atomic.Bool` 守卫），防止 shutdown 死锁

#### 13.4.5 OTel ↔ 日志桥接（`pkg/tracing/bridge.go`）

```go
// TraceFields 从 ctx 提取 OTel trace_id/span_id 返回 zap.Field。
// 无 active span 时返回 nil（零分配，~12ns）。
func TraceFields(ctx context.Context) []zap.Field
```

- 在 handler 入口处调用，注入到 request-scoped logger
- 后续所有日志自动带 `otel_trace_id` + `otel_span_id`
- 审计行也显式记录这两个字段，实现日志 ↔ trace 交叉关联

#### 13.4.6 审计日志（`internal/gateway/audit.go`）

单条审计行包含完整请求生命周期，写入独立 `request_audit.log`（非采样 SubAudit logger）。
字段按 zap.Object 分组为 6 个 sibling namespace，AI 和人类可直接按分组定位阶段：

```json
{
  "event": "AUDIT",
  "trace_id": "abc123",
  "otel_trace_id": "4bf92f3577b34da6a3ce929d0e0e4736",
  "request_id": "req-001",
  "mode": "non_stream",
  "request": {
    "model": "gpt-4",
    "max_tokens": 100,
    "temperature": 0.7,
    "body_size": 1024,
    "body_digest": "a3f2...",
    "headers": {"Content-Type":"application/json","Authorization":"Bearer sk-xxx"}
  },
  "response": {
    "id": "chatcmpl-xyz",
    "model": "gpt-4",
    "finish_reason": "length",
    "prompt_tokens": 50,
    "completion_tokens": 500,
    "total_tokens": 550,
    "backend_status": 200,
    "body_size": 8192,
    "body_digest": "b4c3..."
  },
  "stream": {
    "result": "",
    "bytes": 0,
    "chunks": 0
  },
  "routing": {
    "instance": "gpu-node-7",
    "endpoint": "10.0.1.7:8080",
    "allocation_id": "alloc-xyz",
    "attempt": 1
  },
  "disconnect": {
    "source": "none",
    "phase": ""
  },
  "timeline": {
    "received_at": "2026-04-01T14:23:05.001Z",
    "queued_ms": 0,
    "dequeued_ms": 2,
    "allocated_ms": 5,
    "forwarded_ms": 8,
    "responded_ms": 1523,
    "released_ms": 1530,
    "total_ms": 1530
  }
}
```

**自证场景示例**：`request.max_tokens=100` 但 `response.completion_tokens=500`，后端 bug 一目了然。

**流式审计行额外包含**：

- `stream.result`：done / interrupted / error
- `stream.bytes` / `stream.chunks`：client 实际收到的数据量
- `response.prompt_tokens` / `response.completion_tokens`：从最后 SSE data chunk 提取

**重试审计行额外包含**：

```json
"routing": {
  "attempt": 3,
  "retry_details": [
    {"instance":"gpu-1","status":502,"error":"connection refused","latency_ms":15},
    {"instance":"gpu-2","status":503,"error":"overloaded","latency_ms":230}
  ]
}
```

AuditRecord 使用 `sync.Pool` 复用，Acquire/Release ~18ns/op，0 allocs。EmitAudit ~900ns/op，3 allocs。

#### 13.4.7 Gateway 请求路径改造

**`handleInferenceRequest`：**

1. body 读取后立即计算 `reqDigest := forensics.BodyDigest(bodyBytes)`
2. 创建 `AuditRecord`，填充 ReceivedAt、RequestBodySize、RequestBodyDigest
3. 提取 `RequestKeyFields`（model、max_tokens、temperature 等推理参数）+ `IngressHeaders`
4. OTel bridge：`tracing.TraceFields(ctx)` 注入到 reqLog
5. `inflight.Track(requestID)` / `defer inflight.Untrack(requestID)`
6. rate limiter 拒绝时：补充日志 + `DisconnectTotal.Inc()` + `DisconnectPhase`

**`handleStreamChat`：**

1. allocate 成功后记录 AllocatedAt
2. proxy 完成后用 `forensics.ClassifyProxyDisconnect` 替代原 `tracker.Result()` 逻辑
3. 从 `streamTracker.capture` 提取流式 usage（prompt_tokens、completion_tokens）和 finish_reason
4. 设置 `DisconnectPhase`（allocate / stream）+ `DisconnectAt`
5. release 完成后记录 ReleasedAt
6. `EmitAudit(s.auditLogger, record)`

**`handleNonStreamChat`：**

1. 每次重试前记录失败实例和失败原因 → `RetryDetail` 列表
2. client 断连提前跳出重试循环 + `DisconnectPhase=response`
3. 成功时计算 `respDigest` + 提取 `ResponseKeyFields`（id、finish_reason、usage）
4. 各断连路径设置 `DisconnectPhase`（allocate / forward / response）+ `DisconnectAt`
5. `EmitAudit` 包含最终结果 + 所有重试信息

**`HandleV2ChatCompletion`：**

1. 计算 body digest，提取 `RequestKeyFields` + `IngressHeaders`
2. 使用 `errClientDisconnect` sentinel error 修复 client 断连语义
3. ACK stream 和 non-ACK stream 统一归因
4. 通过 `streamCapture` 提取流式 usage 和 finish_reason（ACK 和 non-ACK 路径共享）

#### 13.4.8 用户自证场景对照

| 怀疑 | 自证方式 |
|------|---------|
| "router 篡改了请求 body" | 审计行 `request.body_digest` 在收到和转发时是同一份 bodyBytes 的 SHA256，不可能不同 |
| "重试发了不同的内容" | 多次 attempt 的 `request.body_digest` 相同（同一份 bodyBytes slice）；`routing.retry_details` 显示每次重试的实例和结果 |
| "router 改了响应" | 非流式：`response.body_digest` = 后端原始响应 SHA256。流式：`stream.bytes` + `stream.chunks` 与后端日志对比 |
| "FastDeploy max_tokens 不生效" | 审计行 `request.max_tokens=100` vs `response.completion_tokens=500`，一目了然 |
| "请求 hang 住了" | `rl_router_inflight_request_age_seconds` histogram 显示 >30s 的请求数；审计行 `timeline.*` 显示哪个阶段卡住 |
| "是 client 断了还是 backend 断了" | `disconnect.source` + `disconnect.phase` 明确归因（谁断的 + 断在哪）；`disconnect.at` 精确到毫秒 |
| "排队时 hang 住" | 审计行 `timeline.queued_ms → dequeued_ms` 差值 = 排队耗时；rate limiter 拒绝有显式日志 |
| "分配不到实例" | `disconnect.source=alloc_fail` + `disconnect.phase=allocate` vs `disconnect.source=client`，区分"真无实例"和"client 等不及断了" |
| "client 收到了多少数据就断了" | `stream.bytes` + `stream.chunks` 精确显示传输量；`disconnect.at` 显示断连绝对时间 |
| "client 传了什么 headers" | `request.headers` 全量记录所有入口 headers |

#### 13.4.9 涉及文件清单

| 文件 | 改动 | 说明 |
|------|------|------|
| `pkg/forensics/digest.go` | 新建 | SHA256 body 摘要 + sync.Pool |
| `pkg/forensics/disconnect.go` | 新建 | 断连来源分类器 + DisconnectPhase 常量 |
| `pkg/forensics/keyfields.go` | 新建 | RequestKeyFields + ResponseKeyFields 提取（OpenAI + SGLang 兼容） |
| `pkg/forensics/headers.go` | 新建 | CaptureHeaders 全量记录请求 headers |
| `pkg/forensics/inflight.go` | 新建 | inflight 请求老化追踪 |
| `pkg/tracing/bridge.go` | 新建 | OTel trace_id ↔ zap 桥接 |
| `pkg/logger/logger.go` | 修改 | 独立 `request_audit.log` + SubAudit 非采样审计 logger |
| `pkg/logger/fields.go` | 修改 | AUDIT event + disconnect_source 字段 |
| `pkg/metrics/metrics.go` | 修改 | DisconnectTotal 3 标签（source, path, phase） + InflightRequestAge |
| `internal/gateway/audit.go` | 新建 | AuditRecord + EmitAudit（zap.Object 分组）+ pool + RetryDetail |
| `internal/gateway/stream_capture.go` | 新建 | streamCapture（prevDataLine/lastDataLine），提取流式 usage/finish_reason |
| `internal/gateway/stream_tracker.go` | 修改 | 内嵌 streamCapture |
| `internal/gateway/server.go` | 修改 | RequestKeyFields + IngressHeaders + 流式 usage 提取 + DisconnectPhase |
| `internal/gateway/forward_nonstream.go` | 修改 | ResponseKeyFields + RetryDetails + DisconnectPhase |
| `internal/gateway/v2_chat.go` | 修改 | RequestKeyFields + streamCapture + DisconnectPhase |
| `internal/config/config.go` | 修改 | AuditLogConfig 嵌套结构体 |
| `internal/app/app.go` | 修改 | 审计 logger + inflight tracker 接线 |

#### 13.4.10 性能影响

| 改动 | 开销 | 说明 |
|------|------|------|
| SHA256 body digest | ~550ns/1KB body | sync.Pool 复用 hasher，3 allocs |
| AuditRecord pool | ~18ns/op, 0 allocs | sync.Pool 复用 |
| EmitAudit（zap.Object 分组） | ~900ns/行, 3 allocs | JSON 编码写入 request_audit.log |
| RequestKeyFields 提取 | ~180ns/小 body, ~35μs/50KB body | go-json 部分反序列化，messages 被跳过 |
| ResponseKeyFields 提取 | ~310ns (OpenAI), ~130ns (/generate) | 扩展现有 extractUsage |
| 流式 lastDataLine 捕获 | ~94ns/chunk | bytes.Index 扫描 SSE data 前缀 |
| 流式 usage 提取 | ~235ns（parse 最后 data payload） | 流结束时一次 |
| CaptureHeaders | ~250ns（6 个 headers） | 遍历 headers |
| Inflight tracker | ~10ms/5s sweep | 10K 并发 × 1us/entry |
| OTel bridge（无 span） | ~12ns, 0 allocs | 快速路径，OTel 关闭时零开销 |
| OTel bridge（有 span） | ~80ns, 3 allocs | 提取 trace_id + span_id |
| DisconnectSource 分类 | ~50ns/req | 纯条件判断，零分配 |
| **合计每请求新增** | **~2-3μs（小 body）** | **大部分在 post-proxy 路径** |

## 14. Stale Connection 防御

### 14.1 问题背景

FastDeploy (Gunicorn) `keepalive` 默认 **2s**，而 RL-Router 的 `http.Transport` `IdleConnTimeout = 90s`。后端 2s 关闭空闲连接后，Router 仍认为连接有效并复用，写入失败产生 `use of closed network connection`。

Go 的 `http.Transport` 对 stale connection 有内置重试，但**仅限 `Request.GetBody != nil` 的请求**。代码中 `io.NopCloser(bytes.NewReader())` 包装遮蔽了底层 `*bytes.Reader` 类型，导致 Go 无法自动设置 `GetBody`，因此不重试。

### 14.2 业界对比

| 后端 | keepalive 超时 | TCP KeepAlive | Idle Pool Timeout | Stale Retry |
|------|---------------|---------------|-------------------|-------------|
| **FastDeploy** (Gunicorn) | 2s | — | — | — |
| **sglang** (uvicorn + Rust router) | 5s | 30s (OS probe) | 50s | hyper 自动重试 + 5次应用层重试 |
| **vllm** (uvicorn) | 5s | — | 60s (benchmark client) | httpx retry |
| **RL-Router** (Go) | 依赖后端 | **30s** | 90s | **GetBody + 应用层 1 次重试** |

### 14.3 四层防御架构

| 层 | 机制 | 文件 | 说明 |
|----|------|------|------|
| **L1** | `replayableBody()` 设置 `GetBody` | `connutil.go` | 启用 Go Transport 原生 stale conn 自动重试 |
| **L2** | TCP KeepAlive 30s | `server.go`, `proxy.go` | OS 层探测半开连接，参考 sglang |
| **L3** | 自定义 `sharedHTTP1Transport` | `proxy.go` | `httputil.ReverseProxy` 共享 Transport，不再使用 `DefaultTransport` |
| **L4** | `streamForward` 应用层重试 | `v2_chat.go` | `isStaleConnError` 匹配后重建请求重试 1 次 |

### 14.4 关键实现

#### `replayableBody` (`connutil.go`)

为 `http.Request` 设置 `Body`、`ContentLength` 和 `GetBody`，使 Go Transport 在检测到 stale connection 时能透明重试：

```go
func replayableBody(r *http.Request, bodyBytes []byte) {
    r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
    r.ContentLength = int64(len(bodyBytes))
    r.GetBody = func() (io.ReadCloser, error) {
        return io.NopCloser(bytes.NewReader(bodyBytes)), nil
    }
}
```

替换了 5 处 `io.NopCloser(bytes.NewReader())` 模式：`server.go`、`v2_chat.go`（3处）、`forward_nonstream.go`。

#### `isStaleConnError` (`connutil.go`) - 2026-04-21 更新

基于 Go 1.26.1 源码审计（`transport.go` + `h2_bundle.go`），补全所有可重试的 stale connection 错误模式：

- HTTP/1.1: EOF（排除 parsing/unmarshal 上下文）、use of closed network connection、
  connection reset by peer、broken pipe、
  **server closed idle connection**（`errServerClosedIdle`）、
  **HTTP/1.x transport connection broken**（`mapRoundTripError`）
- HTTP/2: stream error、server sent GOAWAY（`GoAwayError`）、
  **Transport received Server's graceful shutdown GOAWAY**（`errClientConnGotGoAway`）、
  **client conn not usable**（`errClientConnUnusable`）、
  **client conn is closed**（`errClientConnClosed`，替代旧版 `client conn lost`）、
  **no cached connection was available**（`ErrNoCachedConn`）
- TLS: bad record MAC、connection reset

用于 `streamForward` (V2 ACK) 应用层重试和 `isRetryableError` 增强。

#### V1/V2 流式重试 - 2024-04-18 新增

解决所有流式请求路径的 stale connection 重试问题：

- V1 流式 (`handleStreamChat`)：添加应用层重试循环，maxRetries=1
- V2 Non-ACK 流式 (`handleV2StreamNonACK`)：新增 `streamProxyWithRetry()` 包装器
- V2 ACK 流式 (`streamForward`)：已有重试逻辑（修复 EOF 匹配后生效）

重试特性：

- 最大 1 次重试（足够刷新连接池）
- 使用新 `streamTracker` 每次重试
- 记录日志："v1/v2: streaming stale connection, retrying"
- 指标：`StaleConnRetries.WithLabelValues("stream").Inc()`

测试覆盖：

- 单元测试：EOF/HTTP-2/TLS 错误匹配、并发安全（100 goroutine × 1000）
- 集成测试：V1 流式重试成功/失败场景
- 基准测试：0 allocs/op，无性能回归

#### Transport 配置

`server.go` 的 `httpClient` 和 `proxy.go` 的 `ReverseProxy` Transport 统一配置：
- `DialContext.Timeout = 10s`（连接超时，对齐 sglang）
- `DialContext.KeepAlive = 30s`（TCP keepalive 探测，对齐 sglang）
- `MaxIdleConnsPerHost = 100`、`MaxIdleConns = 1000`
- `IdleConnTimeout` = 可配置（`backend_idle_timeout`，默认 3s）

`IdleConnTimeout` 应 < 后端 keepalive 的 50%-80%（业界标准，Nginx/Envoy 均遵循）：
- uvicorn (sglang/vllm) keepalive=5s → 建议 3s（默认值）
- FastDeploy (Gunicorn) keepalive=2s → 建议 1s
- 0 表示禁用空闲超时（不推荐）

```yaml
# config.yaml
backend_idle_timeout: 3s  # 默认 3s，适配 uvicorn keepalive=5s
```

流式路径（`httputil.ReverseProxy`）在 `forwardViaCache` 中自动缓存 POST body 并设置 `GetBody`，使 Go Transport 内建重试覆盖流式 POST 请求。

### 14.5 指标

| 指标 | 类型 | Labels | 说明 |
|------|------|--------|------|
| `rl_router_stale_conn_retries_total` | Counter | `path` (stream/non_stream) | 应用层 stale connection 重试次数 |

## 15. 等待队列（Waiting Queue）

### 15.1 背景

RL 训练 rollout 场景中，每个 step 的所有推理请求**必须完成**，不能因限流快速报错（429/503）导致训练中断。当前架构在后端全满时（`ErrAllOverloaded`）立即拒绝请求。等待队列机制让这些请求排队等候，直到后端释放容量后自动分配。

**改动范围**：仅 Scheduler 侧等待队列。自适应并发策略（后端容量发现、新 fetcher 等）后续单独做。

### 15.2 架构概览

```
配置文件 (config.yaml)
    │ waiting_queue:
    │   enabled: true          ← 默认开关
    │   max_size: 100000       ← 默认队列上限
    │   timeout: 300s          ← 默认超时
    ▼
┌─── Scheduler 启动 ─────────────────────────────────────────────┐
│  读取 config.WaitingQueue → 设为 step 级默认值                   │
└─────────────────────────────────────────────────────────────────┘
    │
    │ StartStep(waiting_queue_enabled=true)   ← 可选覆盖
    │ 如果 StartStep 不传 → 使用启动配置的默认值
    │ 如果 StartStep 传了 → 以 StartStep 为准
    ▼
┌───────────────────── Scheduler Event-Loop ─────────────────────┐
│                                                                 │
│  batchAllocate:                                                 │
│    queue 非空 → 新请求无条件入队尾部（FIFO 保障）                  │
│    queue 空   → 正常分配：                                       │
│      有容量  → policy.Select → recordAllocation → 返回实例       │
│      无容量 + 队列启用 → enqueue → waitingQueue                  │
│      无容量 + 队列关闭 → 返回 ErrAllOverloaded（现有行为）        │
│                                                                 │
│  handleRelease:                                                 │
│    释放容量 → (在 processBatch 结束后) drainWaitingQueue          │
│                                                                 │
│  waitingQueue: [ev1, ev2, ...] FIFO                             │
│    ├── 客户端取消 ctx → drain 时移除                              │
│    ├── 超时 → 返回 ErrWaitingQueueTimeout                        │
│    ├── policy.Select 成功 → recordAllocation → 移除               │
│    └── 仍然全满 → break（提前退出，不扫描全队列）                  │
│                                                                 │
│  handleStartStep: 清空队列 + 读取队列配置                        │
│  handleEndStep: 向所有排队请求返回错误 + 清空队列                 │
│                                                                 │
└─────────────────────────────────────────────────────────────────┘
```

### 15.3 配置

两层配置，运行时覆盖启动默认值：

```yaml
# config.yaml — 启动默认值
waiting_queue:
  enabled: true
  max_size: 100000     # 队列硬上限
  timeout: 300s        # 单请求最大等待时间
```

```json
// StartStep API — 每 step 可覆盖（nil/未传 = 用启动默认值）
{
  "waiting_queue_enabled": true,
  "waiting_queue_max_size": 50000,
  "waiting_queue_timeout_sec": 120
}
```

优先级：`StartStep API > config.yaml > 代码默认值`

**配置合并逻辑**（`handleStartStep` 中）：PolicyConfig 使用指针字段（`*bool`, `*int64`），nil 表示未设置，使用启动配置默认值；非 nil 则以 StartStep 传入值为准。

### 15.4 核心流程

```
Allocate 请求到达 event-loop
    │
    ├── 队列非空？ → 无条件入队尾部（FIFO 保障）
    │
    └── 队列空 → 正常分配：
          ├── 有容量 → policy.Select → 分配成功
          └── 无容量：
                ├── 队列启用 → 入队
                └── 队列关闭 → 返回 ErrAllOverloaded

Release 事件 → processBatch 末尾调用 drainWaitingQueue()
    │
    └── 遍历队列（writeIdx 压缩模式）：
          ├── ctx 取消 → 返回 ctx.Err()，移除
          ├── 超时 → 返回 ErrWaitingQueueTimeout，移除
          ├── policy.Select 成功 → recordAllocation，移除
          ├── ErrAllOverloaded → 提前退出（后面也分配不了）
          └── 其他错误 → 返回错误，移除

EndStep → rejectWaitingQueue() 批量拒绝所有排队请求
StartStep → rejectWaitingQueue() + 合并新配置
```

**FIFO 保障**：当 waitingQueue 非空时，新到达的 Allocate 无条件追加到队尾，不尝试分配。这防止 batch 中 Allocate/Release 交错时产生顺序反转：

```
batch = [A1, A2, A3, R1, A4, A5]

processBatch:
  flushAllocs([A1,A2,A3]):
    batchAllocate: queue 空 → 正常分配
      A1✓  A2✓  A3✗→入队  queue=[A3]
  handleRelease(R1): 容量+1
  flushAllocs([A4,A5]):
    batchAllocate: queue 非空 → A4,A5 无条件入队  queue=[A3,A4,A5]
  drainWaitingQueue:
    A3→有容量→分配✓   A4→看容量...

结果：A3 先到先分配 → 顺序正确 ✓
不做 FIFO 保障：A4 直接分配跳过 A3 → 顺序反转 ✗
```

**提前退出**：drainWaitingQueue 遇到第一个 `ErrAllOverloaded` 时 break，不扫描全队列。通常 O(1)。

**入队事件 ownership**：`ev.enqueued == true` 表示事件已被 normal/PD waiting queue 接管，当前处理方不再释放 RouteContext 或回收 event；最终由对应 queue 的 drain/reject 路径清理。

### 15.5 内存安全

#### 每个排队请求开销

| 组件 | 大小 | 说明 |
|------|------|------|
| `*event` 指针 | 8B | waitingQueue slice |
| `event` 结构体 | ~280B | 联合体字段 |
| `allocResult` chan | ~96B | buffered 1 |
| `*RouteContext` | ~200B | TraceID/RequestID/SessionID |
| `context.Context` | ~64B | 带 cancel |
| **合计** | **~650B** | |

#### 规模估算（Scheduler 16c 32g）

| 排队数 | 队列结构内存 | goroutine 栈 | 合计 | 占 32GB |
|--------|------------|-------------|------|---------|
| 1 万 | 6.5 MB | 160 MB | ~170 MB | 0.5% |
| 10 万 | 65 MB | 1.6 GB | ~1.7 GB | 5% |

**goroutine 栈是主开销**：每个 `Allocate()` 调用方 goroutine 挂起等待 resultCh，栈 8~16 KB。

#### RL 训练实际规模

```
万卡集群 = ~1250 实例 (8 GPU/node), 每实例 max_batch ≈ 64
每 step 总请求 = 1250 × 64 = 80,000
排队峰值 = 80,000 - 10,000(容量) = 70,000
70,000 × 16KB = 1.12 GB → 32GB 完全承受
```

#### gRPC 连接模型

每 Gateway → Scheduler **1 条 TCP 连接**（HTTP/2 多路复用）。10w 排队 = 10w HTTP/2 stream，不是 10w TCP 连接。

| 资源 | 10w 合计 |
|------|---------|
| Scheduler gRPC handler goroutine | ~1.6 GB |
| HTTP/2 stream 状态 | ~200 MB |
| Gateway 侧 caller goroutine | ~1.6 GB |

`grpc.MaxConcurrentStreams(50000)` 限制单连接最大并发 stream，防止 HTTP/2 stream 爆炸。

#### 防护机制

| 风险 | 防护 |
|------|------|
| 队列无限增长 | `queueMaxSize` 硬限制（默认 10w），超过返回 `ErrWaitingQueueFull` |
| 请求永不返回 | `queueTimeout`（默认 300s），超时返回 `ErrWaitingQueueTimeout` |
| goroutine 泄漏 | 客户端 ctx 取消 → drain 时清理 |
| 后端全卡住 | `NO_RELEASE_ALARM` 日志告警（连续 10 次 drain cycle 无 Release） |
| Step 结束未清理 | `handleEndStep` 批量拒绝 + 清空 |
| gRPC stream 爆炸 | `MaxConcurrentStreams(50000)` 限制 |
| TCP 断连 | gRPC 框架自动取消 ctx → drain 清理；重连后 request_id 幂等 |

### 15.6 gRPC 错误映射

| 错误 | gRPC Code | HTTP 语义 |
|------|-----------|----------|
| `ErrWaitingQueueFull` | `ResourceExhausted` | 429 |
| `ErrWaitingQueueTimeout` | `DeadlineExceeded` | 504 |

### 15.7 可观测性

#### 指标

| 指标 | 类型 | 说明 |
|------|------|------|
| `rl_router_waiting_queue_depth` | Gauge | 当前队列深度 |
| `rl_router_waiting_queue_enqueue_total` | Counter | 入队总次数 |
| `rl_router_waiting_queue_drain_total` | CounterVec | 出队总次数（reason: success/timeout/cancelled/error） |
| `rl_router_waiting_queue_drain_cycles` | Counter | drain 调用次数 |
| `rl_router_waiting_queue_drain_capped` | Counter | 因 AllOverloaded 提前退出次数 |
| `rl_router_waiting_queue_wait_duration_ms` | Histogram | 排队等待时间分布 |
| `rl_router_event_loop_last_active_ts` | Gauge | event-loop 最后活跃时间戳 |

**自证能力**：
- `last_active_ts` 超 5s 没更新 → event-loop 卡死。持续更新 → 问题在别处
- `drain_capped` 持续增长 + `depth` 不降 → 后端确实没容量
- `enqueue_total - Σ(drain_total) ≈ depth` → 数值自洽，无泄漏

#### 日志事件

| 事件 | 级别 | 携带字段 | 说明 |
|------|------|---------|------|
| `QUEUE_ENQUEUE` | Debug | request_id, queue_depth | 请求入队 |
| `QUEUE_DRAIN_CYCLE` | Debug | drained, timeout, cancelled, remaining | drain 周期完成 |
| `QUEUE_DRAIN_SUCCESS` | Debug | request_id, instance_id, wait_ms | 排队请求分配成功 |
| `QUEUE_TIMEOUT` | Warn | request_id, wait_ms | 请求超时 |
| `QUEUE_FULL_REJECT` | Warn | request_id, queue_depth, max_size | 队列满拒绝 |
| `QUEUE_CLIENT_CANCEL` | Info | request_id, wait_ms | 客户端取消 |
| `QUEUE_STEP_REJECT` | Warn | rejected_count, step_id | step 结束批量拒绝 |
| `NO_RELEASE_ALARM` | Warn | depth, cycles_without_release | 队列非空但连续 10 次 drain 无 Release |

#### 诊断端点

`GET /v1/admin/waiting-queue`（Admin 端口）返回队列完整快照：

```json
{
  "queue_enabled": true,
  "queue_depth": 42,
  "queue_max_size": 100000,
  "queue_timeout_sec": 300,
  "step_id": 7,
  "step_phase": "serving",
  "event_channel_len": 3
}
```

#### 告警规则

```yaml
# Prometheus Alertmanager 规则
- alert: SchedulerEventLoopStuck
  expr: time() - rl_router_event_loop_last_active_ts > 10
  for: 30s
  labels: {severity: critical}
  annotations:
    summary: "Scheduler event-loop 卡死超过 10s"

- alert: NoReleaseWhileQueued
  expr: rl_router_waiting_queue_depth > 0 and rate(rl_router_release_total[2m]) == 0
  for: 3m
  labels: {severity: critical}
  annotations:
    summary: "队列非空但无 Release，后端可能卡住"

- alert: QueueWaitTooLong
  expr: histogram_quantile(0.99, rl_router_waiting_queue_wait_duration_ms_bucket) > 60000
  for: 2m
  labels: {severity: warning}
  annotations:
    summary: "P99 排队等待时间超过 60s"

- alert: QueueDepthHigh
  expr: rl_router_waiting_queue_depth > 50000
  for: 1m
  labels: {severity: warning}
  annotations:
    summary: "队列深度超过 50000，接近上限"
```

### 15.8 排查 Playbook

```
Step 1: GET /v1/admin/waiting-queue
  ├─ queue_depth > 0 → 在排队（Step 2）
  └─ queue_depth == 0 → 不在排队（查 active_requests 或 gateway 日志）

Step 2: event_channel_len 正常？（<1000）
  ├─ 是 → event-loop 正常（Step 3）
  └─ 否 → event-loop 可能卡死（查 pprof goroutine dump，重启）

Step 3: rl_router_waiting_queue_drain_capped 持续增长？
  ├─ 是 → 后端确实全满（Step 4）
  └─ 否 → 查 drain_total 各 reason 分布：
        ├─ timeout 多 → queueTimeout 太短或后端太慢
        ├─ cancelled 多 → 客户端超时设置太短
        └─ error 多 → 查具体错误日志

Step 4: 为什么不释放？
  ├─ rate(rl_router_release_total[2m]) == 0 → 无请求完成
  │   ├─ gateway 存活？（检查 heartbeat_total）
  │   ├─ 后端 /metrics: running > 0 但不动？→ 后端推理卡住
  │   └─ 所有实例都不释放 vs 部分 → 全局 vs 单点问题
  ├─ untracked_active_requests > 0 且 gateway_allocs_total 很低
  │   ├─ Allocate 是否带 GatewayID / gateway_addr？
  │   ├─ gateway 是否绕过 router 直接调用 scheduler？
  │   └─ ghost cleanup 无法释放未追踪 allocation
  ├─ gateway 有 "pending release enqueued for heartbeat retry"
  │   └─ Release RPC 已失败到补偿队列，继续查 heartbeat piggyback 是否 delivered
  └─ release rate > 0 但 depth 不降 → 入队速率 > 出队速率（正常过载，等待即可）

Step 5: 队列积压但 depth 持续增长？
  ├─ 检查 queueMaxSize 是否足够
  ├─ 检查 rl_router_waiting_queue_enqueue_total 增长速率
  └─ 考虑增加后端实例或调整 max_request_load
```

### 15.9 涉及文件清单

| 文件 | 变更 |
|------|------|
| [config.go](../internal/config/config.go) | 新增 `WaitingQueueConfig` + Defaults + Validate |
| [event.go](../internal/scheduler/event.go) | 新增 `enqueued`, `queueEntryTime` 字段；`enqueued` 表示 normal/PD queue ownership 转移 |
| [factory.go](../internal/scheduler/policy/factory.go) | `PolicyConfig` 新增 3 个队列指针字段 |
| [server.go](../internal/scheduler/server.go) | 核心：waitingQueue 字段 + enqueue/drain/reject + batchAllocate/flushAllocs/processBatch 修改 |
| [step_lifecycle.go](../internal/scheduler/step_lifecycle.go) | handleStartStep 配置合并 + handleEndStep 清空队列 |
| [http_handler.go](../internal/scheduler/http_handler.go) | startStepRequest/statusResponse 新增字段 + `/v1/admin/waiting-queue` 诊断端点 |
| [grpc_handler.go](../internal/scheduler/grpc_handler.go) | 错误映射：QueueFull→ResourceExhausted, QueueTimeout→DeadlineExceeded |
| [metrics.go](../pkg/metrics/metrics.go) | 新增 7 个队列指标 + event-loop 心跳指标 |
| [fields.go](../pkg/logger/fields.go) | 新增 QUEUE_*/NO_RELEASE_ALARM 事件常量 |
| [app.go](../internal/app/app.go) | `MaxConcurrentStreams(50000)` + 传递 WaitingQueueConfig + 注册 admin 路由 |
| [server_queue_test.go](../internal/scheduler/server_queue_test.go) | normal waiting queue、共享控制语义、queue-policy 交互单测 + 并发测试 + 基准测试 |
| [server_queue_pd_test.go](../internal/scheduler/server_queue_pd_test.go) | PD waiting queue 单测 |
