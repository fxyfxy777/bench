# OneRouter 测试策略文档

## 1. 概述

本文档定义 OneRouter 项目的完整测试策略，覆盖从单元测试到生产级压测的全链路，确保系统在**可靠性、扩展性、快速迭代**三个维度达到设计目标。

### 1.1 设计目标回顾

| 目标 | 量化指标 |
|------|----------|
| 万卡级实例管理 | 单 Scheduler 维护 10,000 实例的实时负载状态 |
| 高吞吐调度 | 抗住 10w 瞬时请求（16c32g），40ms 内完成全部调度 |
| 高并发代理 | 单 Gateway 维持 10,000 SSE 长连接（8c64g） |
| 长期稳定运行 | 训练任务持续半年，故障可自愈 |
| 极致负载均衡 | 全局绝对负载均衡，最大化 GPU 利用率 |

### 1.2 测试现状

| 维度 | 现状 | 风险等级 |
|------|------|----------|
| 单元测试 | **零覆盖**（主模块无任何 `_test.go`） | 极高 |
| 集成测试 | 79 个 OneRouter + 11 个基线对比（全 PASS） | 中（仅覆盖 hybrid 单节点） |
| 并发安全 | 无 `-race` 测试 | 高 |
| 性能基准 | 仅有 SSE bench（hertz vs net/http） | 高（核心指标未验证） |
| 稳定性测试 | 无 | 极高（"半年运行"目标无保障） |
| 压力测试 | 无 | 高 |

---

## 2. 测试分层体系

```
                        ┌─────────────────────┐
                        │  生产环境验证 (多机)   │  ← 独立环境
                        ├─────────────────────┤
                    ┌───┤  压测 / 耐久 (单机)   │  ← 独立环境
                    │   ├─────────────────────┤
                    │   │  故障注入 / 混沌测试   │  ← 本地 + 独立环境
                ┌───┤   ├─────────────────────┤
                │   │   │  集成测试 (分布式模式)  │  ← 本地多进程
            ┌───┤   │   ├─────────────────────┤
            │   │   │   │  集成测试 (hybrid 模式) │  ← 本地（已有 79 个）
        ┌───┤   │   │   ├─────────────────────┤
        │   │   │   │   │  性能基准 (Benchmark)  │  ← 本地
    ┌───┤   │   │   │   ├─────────────────────┤
    │   │   │   │   │   │  并发安全 (-race)      │  ← 本地
    │   │   │   │   │   ├─────────────────────┤
    │   │   │   │   │   │  单元测试              │  ← 本地
    └───┴───┴───┴───┴───┴─────────────────────┘
```

### 环境分类

| 环境 | 适用测试层 | 资源要求 |
|------|-----------|---------|
| **本地开发机** | 单测、-race、Benchmark、集成测试（hybrid + 分布式）、进程级故障注入 | 普通开发机（8c16g+） |
| **独立测试环境** | 大规模压测、1w SSE 并发、24h+ 耐久测试、网络故障注入 | 40c 176g+（详见 §8） |

---

## 3. 单元测试

> 当前覆盖率 0%。这是最高优先级。

### 3.1 Scheduler 核心

#### `internal/scheduler/server_test.go`

| 用例 | 描述 | 验证点 |
|------|------|--------|
| `TestStateMachine_NormalFlow` | IDLE→SERVING→DRAINING→IDLE 全路径 | 每次转换后 phase 正确 |
| `TestStateMachine_IllegalTransition` | IDLE→DRAINING、DRAINING→SERVING 等 | 返回错误，phase 不变 |
| `TestStateMachine_DrainingAutoIdle` | DRAINING 阶段最后一个 Release 后自动回 IDLE | phase == IDLE |
| `TestAllocate_NotServing` | 非 SERVING 阶段 Allocate | 返回错误 |
| `TestAllocate_NoInstances` | SERVING 但无注册实例 | 返回错误 |
| `TestAllocate_Success` | 正常分配 | 返回 Instance，ActiveRequests++ |
| `TestRelease_Success` | 正常释放 | ActiveRequests-- |
| `TestStartStep_ResetsState` | StartStep 重置 NodeState + Policy | 所有 ActiveRequests == 0 |
| `TestConcurrentAllocateRelease` | 100 goroutine 并发 Allocate + Release | ActiveRequests 最终归零 |
| `TestCleanupGateway` | Gateway 心跳超时触发清理 | 该 Gateway 的分配全部释放 |
| `TestBatchDrain` | 大量并发 Allocate 进入 channel | 批量排空 + FIFO 有序处理 |

#### `internal/scheduler/node_state_store_test.go`

| 用例 | 描述 |
|------|------|
| `TestRegister` | 注册实例，GetNodes 包含该实例 |
| `TestUnregister` | 注销实例，GetNodes 不含该实例 |
| `TestAcquire` | Acquire 后 ActiveRequests +1 |
| `TestRelease` | Release 后 ActiveRequests -1 |
| `TestRelease_NeverNegative` | 对 ActiveRequests==0 的实例 Release，不能变负数 |
| `TestResetAll` | 所有实例 ActiveRequests 归零 |
| `TestGetNodes_Snapshot` | 修改返回的 map 不影响原始数据 |

#### `internal/scheduler/gateway_registry_test.go`

| 用例 | 描述 |
|------|------|
| `TestRegister` | 注册 gateway，列表包含 |
| `TestHeartbeatRenewal` | 心跳更新 LastHeartbeat |
| `TestExpiry` | 超过 HeartbeatTimeout 后触发 OnExpired 回调 |
| `TestRegisterIdempotent` | 重复注册同一地址不产生副本 |

#### `internal/scheduler/step_notifier_test.go`

| 用例 | 描述 |
|------|------|
| `TestBroadcast_AllGateways` | 推送到达所有已注册 Gateway |
| `TestBroadcast_UnreachableGateway` | 某个 Gateway 不可达不阻塞其他 Gateway |
| `TestBroadcast_Timeout` | 推送超过 3s 自动取消 |

### 3.2 调度策略

#### `internal/scheduler/policy/round_robin_test.go`

| 用例 | 描述 |
|------|------|
| `TestSelect_RoundRobin` | N 个实例轮询分配，顺序正确 |
| `TestSelect_SkipFull` | 跳过 ActiveRequests >= GPUNum*64 的实例 |
| `TestSelect_AllFull` | 所有实例满，返回错误 |
| `TestReset` | Reset 后计数器归零，从头轮询 |

#### `internal/scheduler/policy/min_load_test.go`

| 用例 | 描述 |
|------|------|
| `TestSelect_MinLoad` | 选负载最小的实例 |
| `TestSelect_CompositeScore` | 验证 Active + Waiting*0.3 的评分逻辑 |
| `TestBatchSelect_Even` | 1000 节点 × 5000 请求，分配均匀（stddev/mean < 10%） |
| `TestBatchSelect_EjectOverloaded` | 超限节点被堆弹出，不再分配 |
| `TestBatchSelect_Complexity` | 10000 节点基准延迟，验证 O(N+K*logN) |

#### `internal/scheduler/policy/min_request_test.go`

| 用例 | 描述 |
|------|------|
| `TestSelect_MinRequest` | 选 ActiveRequests 最少的实例 |
| `TestSelect_Tie` | 并列时行为确定（不随机） |

#### `internal/scheduler/policy/session_aware_test.go`

| 用例 | 描述 |
|------|------|
| `TestSelect_SessionAffinity` | 相同 session_id 路由到同一实例 |
| `TestSelect_Fallback` | 未知 session_id fallback 到 min-request |
| `TestSelect_InstanceGone` | 绑定的实例被注销后 fallback |
| `TestReset` | Reset 清除所有 session 映射 |

#### `internal/scheduler/policy/factory_test.go`

| 用例 | 描述 |
|------|------|
| `TestBuild_AllRegistered` | round_robin / min_load / min_request / session_aware 全部可创建 |
| `TestBuild_Unknown` | 未注册策略名返回错误 |

### 3.3 Gateway 组件

#### `internal/gateway/server_test.go`

| 用例 | 描述 |
|------|------|
| `TestHandleChatCompletion_NotServing` | 非 SERVING 阶段返回 503 |
| `TestHandleChatCompletion_AllocateFail` | Allocate 失败返回 503 |
| `TestHandleChatCompletion_ProxyFail` | Proxy 失败返回 502 |
| `TestHandleChatCompletion_Success` | 正常链路：Allocate → Forward → Release 全通过 |
| `TestHandleChatCompletion_ReleaseWithMetrics` | Release 携带 CostMetrics |

> 通过 mock `InstanceAllocator`、`Proxy`、`StepChecker` 接口实现测试隔离。

#### `internal/gateway/allocator_test.go`

| 用例 | 描述 |
|------|------|
| `TestLocalAllocator_Allocate` | 直调函数返回正确实例 |
| `TestLocalAllocator_Release` | 直调函数释放成功 |
| `TestRemoteAllocator_Allocate` | mock gRPC client 返回正确实例 |
| `TestRemoteAllocator_Release` | mock gRPC client 释放成功 |

#### `internal/gateway/proxy_test.go`

| 用例 | 描述 |
|------|------|
| `TestHasScheme` | `http://host` → true, `https://host` → true, `host:port` → false |
| `TestForward_SetsTargetURL` | Forward 将请求转发到正确的目标地址 |
| `TestForward_SSEFlush` | SSE 响应被实时 flush（FlushInterval == -1） |
| `TestForward_BackendDown` | 后端不可达返回 502 |

#### `internal/gateway/scheduler_client_test.go`

| 用例 | 描述 |
|------|------|
| `TestRegister_ExponentialBackoff` | 注册失败后退避 500ms→1s→2s→...→10s |
| `TestHeartbeat_UpdatesCache` | 心跳成功更新本地 StepPhase / StepID |
| `TestHeartbeat_3FailsReregister` | 连续 3 次失败触发自动重注册 |
| `TestIsServing` | phase == SERVING 返回 true，其他返回 false |

### 3.4 兼容层 + 基础包

#### `internal/compat/v2_adapter_test.go`

| 用例 | 描述 |
|------|------|
| `TestPushInstances_Conversion` | V2 实例格式 → domain.Instance 字段映射正确 |
| `TestStartInfer_Mapping` | model_version → step_id 映射 |
| `TestResponseEnvelope` | 响应格式 `{"status":{"code":0,"message":"success"}}` |
| `TestInvalidInput` | 缺少必要字段返回错误 |

#### `internal/domain/models_test.go`

| 用例 | 描述 |
|------|------|
| `TestNodeState_AtomicActiveRequests` | 多 goroutine 并发 Add + Load 不出错 |
| `TestNodeState_AtomicActualLoad` | float64 via unsafe.Pointer 并发读写无 torn read |
| `TestNodeState_Clone` | Clone 返回独立副本，修改副本不影响原始 |
| `TestStepPhase_String` | IDLE/SERVING/DRAINING/UNKNOWN 字符串正确 |

#### `internal/config/config_test.go`

| 用例 | 描述 |
|------|------|
| `TestMode_Enum` | gateway / scheduler / hybrid 三个合法值 |

#### `pkg/jsonutil/jsonutil_test.go`

| 用例 | 描述 |
|------|------|
| `TestMarshalUnmarshal` | struct → JSON → struct 往返一致 |

### 3.5 覆盖率目标

| 包 | 目标覆盖率 | 原因 |
|----|-----------|------|
| `internal/scheduler/server.go` | ≥ 90% | 系统核心，状态机 + Event-Loop |
| `internal/scheduler/policy/*` | ≥ 90% | 调度核心，直接影响 GPU 利用率 |
| `internal/scheduler/node_state_store.go` | ≥ 95% | 数据结构简单，应全覆盖 |
| `internal/gateway/*` | ≥ 80% | 数据面关键路径 |
| `internal/compat/*` | ≥ 80% | 兼容层，映射逻辑需验证 |
| `pkg/*` | ≥ 80% | 基础设施 |

---

## 4. 并发安全测试

> 项目大量使用 `atomic` + `unsafe.Pointer`，必须用 race detector 验证。

### 4.1 全量 Race 检测

```bash
# 所有包开启 race detector
go test -race -count=1 ./internal/... ./pkg/...
```

纳入 CI，每次提交必须通过。

### 4.2 专项并发用例

| 场景 | 方法 | 验证点 |
|------|------|--------|
| 并发 Allocate/Release | 100 goroutine 同时 Allocate + Release 1000 次 | ActiveRequests 最终归零 |
| 并发 Step 转换 | 快速交替 StartStep/EndStep | 状态机无非法状态 |
| 并发 Gateway 注册/注销 | 10 goroutine 同时注册 / 心跳 / 过期 | 无 panic，registry 一致 |
| NodeState 原子读写 | 多 goroutine 写 + 读 `ActualLoad`（float64） | 无 torn read |
| GetNodes 快照隔离 | 一边遍历 GetNodes 返回值，一边 Register/Unregister | 遍历不 panic |

---

## 5. 性能基准测试（Benchmark）

> 验证设计目标中的量化指标，并作为回归基线。

### 5.1 Scheduler 调度吞吐

| Benchmark | 目标指标 | 对应设计目标 |
|-----------|---------|-------------|
| `BenchmarkAllocate_100Nodes` | 单次 Allocate 延迟基线 | — |
| `BenchmarkAllocate_1000Nodes` | 1000 节点单次延迟 | 规模扩展对比 |
| `BenchmarkAllocate_10000Nodes` | 1w 节点单次延迟 | "维护 1w 实例" |
| `BenchmarkAllocate_ScaleNodes` | 节点数 10/100/1000/10000 扩展对比 | 复杂度验证 |
| `BenchmarkBatchAllocate_Burst` | 1k/10k/100k 瞬时请求延迟 | "10w 瞬时请求 40ms 内完成" |
| `BenchmarkEventLoop_Throughput` | event/sec | channel 131072 buffer 验证 |
| `BenchmarkAllocateReleaseCycle` | Allocate→Release 完整周期 | 端到端延迟 |
| `BenchmarkAllocate_Contention` | 多并发 worker 吞吐 | 并发竞争场景 |
| `BenchmarkAllocate_DedupHit` | 幂等去重命中路径 | 重试场景优化验证 |

### 5.2 策略选择性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkMinLoad_Select` vs `BenchmarkRoundRobin_Select` | 策略间延迟对比 |
| `BenchmarkMinLoad_BatchSelect_ScaleNodes` | 100 / 1000 / 10000 节点，验证 O(N+K*logN) |
| `BenchmarkMinLoad_BatchSelect_ScaleRequests` | 100 / 1000 / 4096 / 8192 请求扩展 |
| `BenchmarkMinLoad_BatchSelect_WithLoad` | 预置负载场景批量选择 |
| `BenchmarkSessionAware_Hit` vs `BenchmarkSessionAware_Miss` | 缓存命中 vs 未命中延迟差 |
| `BenchmarkAllPolicies_Select_Comparison` | 四策略横向对比 |
| `BenchmarkSessionAware_Reset` | Step 间状态重置开销 |

### 5.3 Gateway 代理吞吐

| Benchmark | 目标指标 |
|-----------|---------|
| `BenchmarkSSEProxy_Throughput` | 单 Gateway SSE 吞吐（不同 chunk 数） |
| `BenchmarkSSEProxy_ConcurrentConnections` | 10/100/1000 并发连接 |
| `BenchmarkSSEProxy_TokenRate` | 2000 tokens/sec/request 无数据丢失 |
| `BenchmarkProxyLatency_P50_P99` | 代理附加延迟 (overhead) |
| `BenchmarkHandleChatCompletion_E2E` | 完整网关路径延迟 |
| `BenchmarkHandleChatCompletion_SSE_E2E` | SSE 模式完整路径延迟 |
| `BenchmarkHandleChatCompletion_Concurrent` | 多 worker 并发吞吐 |
| `BenchmarkGenerateRequestID` | RequestID 生成开销 |
| `BenchmarkProxy_NonStreamingVsStreaming` | 非流式 vs 流式响应对比 |

### 5.4 NodeStateStore 性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkNodeStateStore_GetNodes` | 快照拷贝开销（100/1000/10000 节点） |
| `BenchmarkNodeStateStore_GetSnapshot` | 深拷贝快照开销 |
| `BenchmarkNodeStateStore_AcquireRelease` | 原子计数器操作 |
| `BenchmarkNodeStateStore_ResetAll` | 批量重置开销 |
| `BenchmarkNodeStateStore_RegisterBatch` | 批量注册吞吐 |

### 5.5 GatewayRegistry 性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkGatewayRegistry_Register` | 注册开销 |
| `BenchmarkGatewayRegistry_Heartbeat` | 心跳更新开销 |
| `BenchmarkGatewayRegistry_ConcurrentRegister` | 并发注册性能 |
| `BenchmarkGatewayRegistry_GetAll` | 快照拷贝开销 |

### 5.6 Step 状态机性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkStepStateMachine_StartEnd` | 完整 Step 周期开销 |
| `BenchmarkStepStateMachine_WithRequests` | 带请求的 Step 周期 |
| `BenchmarkStepStateMachine_PolicySwitch` | 策略切换开销 |

### 5.7 幂等去重性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkIdempotency_DedupStress` | 高并发去重压力 |
| `BenchmarkIdempotency_DedupMemoryPressure` | 大量唯一 RequestID 内存压力 |
| `BenchmarkRelease_Idempotent` | 重复 Release 快速路径 |

### 5.8 负载均衡公平性

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkLoadBalance_Fairness` | 请求分布变异系数（σ/μ） |
| `BenchmarkLoadBalance_FairnessBatch` | 批量选择公平性 |

### 5.9 StepNotifier 广播性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkStepNotifier_Broadcast` | 广播开销（1/10/100 Gateway） |
| `BenchmarkStepNotifier_PayloadSize` | 不同负载大小开销 |

### 5.10 Domain 原子操作性能

| Benchmark | 关注点 |
|-----------|--------|
| `BenchmarkNodeState_LoadActiveRequests` | 原子读性能 |
| `BenchmarkNodeState_StoreActiveRequests` | 原子写性能 |
| `BenchmarkNodeState_AddActiveRequests` | 原子加性能 |
| `BenchmarkNodeState_AddActiveRequests_Concurrent` | 并发原子加性能 |
| `BenchmarkNodeState_LoadActualLoad` | float64 原子读性能 |
| `BenchmarkNodeState_StoreActualLoad` | float64 原子写性能 |
| `BenchmarkNodeState_Clone` | 快照拷贝性能 |
| `BenchmarkNodeState_Clone_Concurrent` | 并发 Clone 性能 |
| `BenchmarkNodeState_MixedAccess` | 混合读写模式 |
| `BenchmarkNodeState_MixedAccess_Concurrent` | 并发混合读写 |
| `BenchmarkNodeState_SpecialFloatValues` | 特殊浮点值处理 |
| `BenchmarkStepPhase_String` | 状态枚举转字符串 |
| `BenchmarkInstance_LabelsAccess` | 标签访问性能 |
| `BenchmarkRouteContext_Creation` | RouteContext 分配 |
| `BenchmarkCostMetrics_Creation` | CostMetrics 分配 |

### 5.11 运行方式

```bash
# 本地运行，输出 benchmark 结果
go test -bench=. -benchmem -benchtime=10s ./internal/scheduler/...
go test -bench=. -benchmem -benchtime=10s ./internal/scheduler/policy/...

# 对比两次结果（检测性能回归）
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -count=5 ./internal/scheduler/... > old.txt
# ... 修改代码 ...
go test -bench=. -count=5 ./internal/scheduler/... > new.txt
benchstat old.txt new.txt
```

---

## 6. 集成测试

### 6.1 现有覆盖（hybrid 单节点模式）

已有 79 个测试，覆盖：

| 类别 | 文件 | 用例数 |
|------|------|--------|
| 基础 Chat | `chat_test.go` | 8 |
| SSE 协议 | `sse_test.go` | 6 |
| SSE 边界 | `sse_edge_test.go` | 12 |
| 错误处理 | `error_test.go` | 7 |
| 错误边界 | `error_edge_test.go` | 12 |
| 行为差异 | `continuation_test.go` | 6 |
| 多后端兼容 | `multi_backend_test.go` | ~15 |
| V2 兼容层 | `compat_test.go` | ~8 |
| 基线对比 | `baseline_test.go` | 11 |

### 6.2 需补充：分布式模式测试

> 生产部署是 1 Scheduler + N Gateway，此场景当前**零覆盖**。
>
> **本地可做**——多进程监听不同端口即可。

#### 测试拓扑

```
测试进程 (go test)
    │
    ├── 启动 Scheduler 进程 (mode=scheduler, listen=:随机, grpc=:随机)
    ├── 启动 Gateway-1 进程 (mode=gateway, scheduler-addr=上面的 grpc)
    ├── 启动 Gateway-2 进程 (mode=gateway, scheduler-addr=上面的 grpc)
    └── 启动 Mock Backend ×N
```

#### 用例清单

新增文件：`tests/integration/distributed_test.go`

| 用例 | 描述 | 验证点 |
|------|------|--------|
| `TestDistributed_MultiGatewayLoadBalance` | 100 请求通过 2 个 Gateway 发出 | 后端各实例收到的请求数均匀 |
| `TestDistributed_GatewayRegistration` | Gateway 启动后注册 | Scheduler 状态接口能看到所有 Gateway |
| `TestDistributed_GatewayCrashRecovery` | kill Gateway-1 → 等心跳超时 | Scheduler 清理幽灵负载，Gateway-2 不受影响 |
| `TestDistributed_GatewayGracefulShutdown` | SIGTERM Gateway-1 | 在途请求完成后退出 |
| `TestDistributed_StepStatePush` | StartStep → 两个 Gateway 都收到 SERVING | 状态同步一致 |
| `TestDistributed_HeartbeatSelfHealing` | 模拟 gRPC 断连 → 3 次心跳失败 | Gateway 自动重注册成功 |
| `TestDistributed_StepCycleIsolation` | Step-1 期间分配 → EndStep → Step-2 | Step-2 的 NodeState 已重置 |

### 6.3 需补充：缺失的差异测试

| 编号 | 场景 | 说明 |
|------|------|------|
| D11 | 多轮推理 Session 隔离 | session_aware 策略下验证 |
| D12 | max_tokens 边界行为 | 极大 / 极小 / 零值 |
| D13 | 后端返回非标准 SSE 字段 | 透传验证 |
| D16 | 超大 payload (>10MB) | 内存 / 超时行为 |

---

## 7. 故障注入 / 混沌测试

### 7.1 本地可做（进程级）

| 场景 | 对应稳定性问题 | 方法 | 验证点 |
|------|--------------|------|--------|
| Scheduler 崩溃 | P0-1 无状态恢复 | kill 进程 → 重启 | Gateway 自愈重连；需重新注册实例 |
| Gateway 崩溃 | P1-6 不主动注销 | kill -9 进程 | Scheduler 心跳超时后清理幽灵负载 |
| HTTP handler panic | P0-2 无 panic recovery | 构造触发 panic 的请求 | 记录当前行为（应进程崩溃），加 recovery 后验证不崩 |
| 后端实例宕机 | P0-3 无健康检测 | mock backend 返回 connection refused | 记录当前行为（持续分配），加健康检测后验证摘除 |
| 后端 mid-stream 断开 | — | mock DisconnectAfterChunks | Gateway 返回错误，Release 正确执行 |
| 全部后端宕机 | — | 所有 mock 停止 | Allocate 返回 503，无请求泄漏 |

### 7.2 独立环境（网络级）

| 场景 | 方法 | 验证点 |
|------|------|--------|
| 网络分区（Gateway↔Scheduler） | `iptables -A OUTPUT -p tcp --dport $GRPC_PORT -j DROP` | Gateway 心跳失败 → 3 次后重注册 → 恢复后状态同步 |
| 网络延迟 | `tc qdisc add dev eth0 root netem delay 100ms 20ms` | 调度延迟退化程度可控 |
| 网络丢包 | `tc qdisc add dev eth0 root netem loss 10%` | gRPC 重试有效，请求成功率 > 95% |

---

## 8. 压力测试 / 耐久测试（独立环境）

### 8.1 环境架构

```
┌──────────────────────────────────────────────────────────────────┐
│                     测试环境 (Docker Compose)                     │
│                                                                  │
│  ┌───────────────┐  gRPC  ┌─────────────┐ ┌─────────────┐      │
│  │  Scheduler    │◄──────│  Gateway-1   │ │  Gateway-2   │      │
│  │  (16c 32g)    ├─push─>│  (8c 64g)    │ │  (8c 64g)    │      │
│  └───────┬───────┘       └──────┬───────┘ └──────┬───────┘      │
│          │                      │                 │              │
│  ┌───────┴──────────────────────┴─────────────────┴──────────┐  │
│  │                   Mock Backend Fleet                       │  │
│  │            (M 进程, 每进程模拟 K 个实例)                      │  │
│  │            total = M × K (目标 1,000 ~ 10,000)             │  │
│  └───────────────────────────────────────────────────────────┘  │
│                                                                  │
│  ┌────────────────┐       ┌──────────────────────────────────┐  │
│  │  Bench Client  │──────>│ Gateway-1/2 /v1/chat/completions │  │
│  │  (4c 8g)       │       └──────────────────────────────────┘  │
│  └────────┬───────┘                                              │
│           │ /metrics                                             │
│  ┌────────┴───────┐       ┌──────────────┐                      │
│  │  Test Driver   │       │ Prometheus   │                      │
│  │  (场景编排)     │       │ + Grafana    │                      │
│  └────────────────┘       └──────────────┘                      │
│                                                                  │
└──────────────────────────────────────────────────────────────────┘
```

### 8.2 组件设计

#### 8.2.1 Mock Backend Fleet

**目的**：模拟 1,000~10,000 个推理后端，单进程复用。

```
bench/mockfleet/
├── main.go          # 独立二进制，启动 M 个 mock listener
└── Dockerfile
```

| 参数 | 说明 | 默认值 |
|------|------|--------|
| `-instances` | 模拟实例数 | 1000 |
| `-base-port` | 起始端口 | 9000 |
| `-chunk-count` | 每请求 SSE chunk 数 | 50 |
| `-token-delay` | chunk 间延迟 | 500us (≈2000 tokens/s) |
| `-response-delay` | 首 chunk 延迟（模拟 prefill） | 50ms |

核心 handler 逻辑直接复用现有 `tests/integration/mockbackend/` 的 `Scenario` + `handleStreaming`，但脱离 `testing.T` 依赖，独立编译为长运行进程。

#### 8.2.2 Bench Client（压测客户端）

**目的**：自研 Go 压测客户端，精确控制 SSE 消费 + Step 生命周期。

> 不用 k6 / vegeta 的原因：需要精确控制 SSE 流消费校验、Step 生命周期联动、负载均衡公平性统计。

```
bench/client/
├── main.go           # CLI 入口
├── config.go         # 压测参数
├── runner.go         # 核心引擎
├── sse_consumer.go   # SSE 流消费 + 校验
├── reporter.go       # 实时统计 + 最终报告
└── Dockerfile
```

**CLI 接口**：

```bash
# 10w 瞬时突发
bench-client --target http://gateway:8080 \
  --scenario burst --concurrency 100000

# 1w SSE 并发长连接
bench-client --target http://gateway:8080 \
  --scenario sustained-sse --concurrency 10000 --duration 30m --ramp-up 60s

# 24h 耐久测试
bench-client --target http://gateway:8080 \
  --scenario soak --concurrency 500 --duration 24h --step-cycle 30s

# 尖峰压测
bench-client --target http://gateway:8080 \
  --scenario spike --base-qps 1000 --spike-qps 100000 \
  --spike-duration 5s --spike-interval 60s
```

**采集指标**：

| 类别 | 指标 | 类型 |
|------|------|------|
| 计数 | total_requests / success / errors | Counter |
| 延迟 | e2e_latency (端到端) | HDR Histogram |
| 延迟 | ttft_latency (首 Token 延迟) | HDR Histogram |
| 延迟 | allocate_latency (请求到首字节) | HDR Histogram |
| 吞吐 | total_chunks / total_bytes | Counter |
| 并发 | active_conns / peak_conns | Gauge |
| 错误 | connect_errors / timeout_errors / http_5xx / sse_parse_errors | Counter |

所有指标同时暴露 Prometheus `/metrics` 端点供 Grafana 实时展示。

**输出报告格式**：

```
╔═══════════════════════════════════════════════════════╗
║              OneRouter Bench Report                    ║
╠═══════════════════════════════════════════════════════╣
║ Scenario:    burst-100k                               ║
║ Duration:    42.3s                                    ║
║ Concurrency: 100,000                                  ║
╠═══════════════════════════════════════════════════════╣
║ Requests:    100,000   Success: 99,847   Err: 153     ║
║ Throughput:  2,364 req/s                              ║
║                                                       ║
║ E2E Latency: P50=120ms  P95=450ms  P99=1.2s          ║
║ TTFT:        P50=2.1ms  P95=15ms   P99=45ms          ║
║ Allocate:    P50=0.3ms  P95=1.2ms  P99=5ms           ║
║                                                       ║
║ SSE Chunks:  4,992,350  (avg 50/req)                  ║
║ Chunk Rate:  118,010 chunks/s                         ║
║                                                       ║
║ Errors:                                               ║
║   connect_refused: 12                                 ║
║   timeout:         89                                 ║
║   http_503:        52                                 ║
╠═══════════════════════════════════════════════════════╣
║ Load Balance (per instance):                          ║
║   σ = 3.2  (mean=100, min=94, max=108)  ✓ FAIR       ║
╚═══════════════════════════════════════════════════════╝
```

#### 8.2.3 Test Driver（场景编排器）

**目的**：自动化编排测试场景、控制实例注册 / Step 生命周期、执行断言。

```
bench/driver/
├── main.go           # 场景编排入口
├── scenarios.go      # 预定义场景集
├── assertions.go     # 通过/失败判定
└── Dockerfile
```

#### 8.2.4 监控栈

| 组件 | 镜像 | 用途 |
|------|------|------|
| Prometheus | `prom/prometheus:v2.51.0` | 采集所有组件的 `/metrics` |
| Grafana | `grafana/grafana:11.0.0` | 实时大盘可视化 |

预置 Grafana Dashboard：

| 大盘 | 面板内容 |
|------|---------|
| **全链路概览** | QPS、延迟 P50/P95/P99、错误率、活跃连接 |
| **Scheduler 详情** | event channel 深度、批次大小、调度延迟 |
| **负载均衡** | 每实例请求分布热力图 |
| **资源监控** | 内存、goroutine 数、GC pause |
| **Bench Client** | 并发数变化、TTFT 趋势、错误分类 |

### 8.3 Docker Compose 编排

```
bench/
├── docker-compose.yml            # 一键拉起全部组件
├── docker-compose.soak.yml       # 24h 耐久测试 override
├── Makefile                      # 快捷命令
├── prometheus.yml                # Prometheus 采集配置
├── grafana/
│   ├── dashboards/
│   │   └── onerouter.json          # 预置大盘
│   └── provisioning/
│       └── datasources.yml
├── mockfleet/
│   ├── main.go
│   └── Dockerfile
├── client/
│   ├── main.go
│   ├── config.go
│   ├── runner.go
│   ├── sse_consumer.go
│   ├── reporter.go
│   └── Dockerfile
└── driver/
    ├── main.go
    ├── scenarios.go
    ├── assertions.go
    └── Dockerfile
```

**快捷命令**：

```makefile
# bench/Makefile

bench-all:          # 全部场景（~15 分钟）
bench-burst:        # 10w 瞬时突发
bench-sse:          # 1w SSE 并发
bench-soak:         # 24h 耐久
bench-spike:        # 尖峰压测
bench-fairness:     # 负载均衡公平性
monitor:            # 打印 Prometheus / Grafana 地址
clean:              # 清理全部容器
```

### 8.4 压测场景定义

#### 场景 1：Scheduler 10w 瞬时突发

| 项 | 值 |
|----|----|
| 目标组件 | Scheduler (16c 32g) |
| 并发数 | 100,000 |
| 实例数 | 10,000 |
| 爬坡 | 0（瞬时全量） |
| 持续时间 | 发完即停 |
| **通过标准** | 调度全部完成 < 5s；P99 < 5s；错误率 < 1% |

#### 场景 2：Gateway 1w SSE 并发

| 项 | 值 |
|----|----|
| 目标组件 | 单 Gateway (8c 64g) |
| 并发连接 | 10,000 |
| 每连接 chunk 数 | 50（token-delay=500us ≈ 2000 tokens/s） |
| 爬坡 | 60s 线性 |
| 持续时间 | 30 min |
| **通过标准** | 峰值并发 ≥ 9,500；错误率 < 0.1%；内存稳定 |

#### 场景 3：24h 耐久测试

| 项 | 值 |
|----|----|
| 目标组件 | Scheduler + 2 Gateway（全链路） |
| 并发连接 | 500 |
| Step 循环间隔 | 每 30s 一次 start/end |
| 持续时间 | 24 h |
| **通过标准** | 内存增长 < 20%；goroutine 净增 < 50；错误率 < 0.1%；无 Step 卡死 |

#### 场景 4：尖峰压测

| 项 | 值 |
|----|----|
| 目标组件 | Scheduler + 2 Gateway |
| 基础 QPS | 1,000 |
| 尖峰 QPS | 100,000 |
| 尖峰持续 | 5s |
| 尖峰间隔 | 60s |
| 持续时间 | 10 min |
| **通过标准** | 尖峰期间不崩溃；尖峰后 P50 延迟在 10s 内恢复基线 |

#### 场景 5：负载均衡公平性

| 项 | 值 |
|----|----|
| 目标组件 | Scheduler |
| 实例数 | 10,000 |
| 请求数 | 50,000 |
| **通过标准** | 请求分布变异系数（σ/μ）< 10% |

#### 场景 6：Gateway 崩溃恢复

| 项 | 值 |
|----|----|
| 目标组件 | Scheduler + 2 Gateway |
| 操作 | Gateway-2 发出 50 请求（不释放）→ kill -9 → 等待 |
| **通过标准** | 心跳超时后 Scheduler 的 active_requests 归零 |

### 8.5 资源需求

| 组件 | CPU | 内存 | 数量 |
|------|-----|------|------|
| Scheduler | 16c | 32g | 1 |
| Gateway | 8c | 64g | 2 |
| Mock Fleet | 2c | 4g | 1 |
| Bench Client | 4c | 8g | 1 |
| Prometheus + Grafana | 2c | 4g | 1 |
| **总计** | **40c** | **176g** | — |

### 8.6 部署选项

| 方式 | 适用场景 | 特点 |
|------|---------|------|
| **单台高配物理机 + Docker Compose** | 日常压测 | 简单，网络延迟极低（loopback） |
| **3~4 台云实例 + Docker Compose** | 多机验证 | 真实网络延迟，可模拟网络分区 |
| **K8s 集群** | CI/CD 自动化 | 可复用现有集群，自动化回归 |

建议路径：**单机跑通 → 多机验证网络场景 → 接入 CI 自动化**。

---

## 9. 资源泄漏检测

### 9.1 Goroutine 泄漏

```go
func TestNoGoroutineLeak(t *testing.T) {
    before := runtime.NumGoroutine()
    // 执行 1000 个完整 Allocate → Proxy → Release 周期
    // ...
    runtime.GC()
    time.Sleep(100 * time.Millisecond)
    after := runtime.NumGoroutine()
    assert.InDelta(t, before, after, 5)
}
```

### 9.2 内存泄漏

```go
func TestNoMemoryLeak(t *testing.T) {
    var before, after runtime.MemStats
    runtime.GC()
    runtime.ReadMemStats(&before)
    // 执行 1000 个 Step 周期（start → 100请求 → end）
    // ...
    runtime.GC()
    runtime.ReadMemStats(&after)
    growth := float64(after.HeapInuse-before.HeapInuse) / float64(before.HeapInuse)
    assert.Less(t, growth, 0.2) // 增长 < 20%
}
```

### 9.3 文件描述符泄漏

SSE 长连接场景尤其重要。在耐久测试中定期采集 `/proc/self/fd` 计数，确保不持续增长。

### 9.4 sync.Pool 有效性

通过 pprof allocs profile 验证 `allocResult` channel 确实被 Pool 复用，而非每次重新分配。

```bash
go tool pprof -alloc_objects http://scheduler:8080/debug/pprof/allocs
```

---

## 10. CI 集成

### 10.1 每次提交必过（本地 + CI）

```yaml
# ci.yml 补充
test:
  command: |
    go test -race -count=1 -timeout=120s ./internal/... ./pkg/...
    cd tests/integration && go test -count=1 -timeout=180s ./...
```

| 阶段 | 内容 | 超时 |
|------|------|------|
| lint | `golangci-lint run` | 2min |
| unit + race | `go test -race ./internal/... ./pkg/...` | 2min |
| integration (hybrid) | `cd tests/integration && go test ./...` | 3min |
| integration (distributed) | `cd tests/integration && go test -run TestDistributed ./...` | 5min |

### 10.2 定期执行（独立环境）

| 频率 | 内容 |
|------|------|
| 每日 | bench-burst + bench-sse（验证性能无回归） |
| 每周 | bench-soak 24h（验证稳定性） |
| 每次发版前 | 全量场景（burst + sse + soak + spike + fairness + crash-recovery） |

---

## 11. 实施优先级

```
Phase 1 — 立即做（1~2 周）                              环境：本地
  ├── [单测] scheduler/server_test.go（状态机 + Event-Loop）
  ├── [单测] policy/*_test.go（4 个策略）
  ├── [单测] node_state_store_test.go
  ├── [并发] go test -race ./internal/...
  ├── [性能] BenchmarkBatchSelect_10000Nodes
  └── [CI]   接入 race + 单测

Phase 2 — 短期做（2~4 周）                              环境：本地
  ├── [单测] gateway/*_test.go
  ├── [单测] compat/v2_adapter_test.go
  ├── [单测] domain/models_test.go
  ├── [集成] 分布式模式测试（1 Scheduler + 2 Gateway）
  ├── [性能] BenchmarkAllocate 系列
  ├── [故障] 进程级故障注入
  └── [泄漏] Goroutine / 内存泄漏短周期检测

Phase 3 — 持续做（4~8 周）                              环境：独立
  ├── [环境] Docker Compose 搭建（bench/ 目录）
  ├── [组件] mockfleet 独立二进制
  ├── [组件] bench-client 核心引擎
  ├── [压测] 10w 瞬时突发
  ├── [压测] 1w SSE 并发
  ├── [监控] Prometheus + Grafana 大盘

Phase 4 — 长期做                                       环境：独立
  ├── [耐久] 24h Soak 测试
  ├── [编排] Test Driver 场景自动化
  ├── [混沌] 网络级故障注入
  ├── [多机] 3~4 台云实例全链路
  └── [CI]   定期自动化回归
```

---

## 12. 目录结构总览

```
RL-Router/
├── internal/
│   ├── scheduler/
│   │   ├── server.go
│   │   ├── server_test.go              ← Phase 1 新增
│   │   ├── node_state_store.go
│   │   ├── node_state_store_test.go    ← Phase 1 新增
│   │   ├── gateway_registry.go
│   │   ├── gateway_registry_test.go    ← Phase 1 新增
│   │   ├── step_notifier.go
│   │   ├── step_notifier_test.go       ← Phase 1 新增
│   │   └── policy/
│   │       ├── round_robin_test.go     ← Phase 1 新增
│   │       ├── min_load_test.go        ← Phase 1 新增
│   │       ├── min_request_test.go     ← Phase 1 新增
│   │       ├── session_aware_test.go     ← Phase 1 新增
│   │       └── factory_test.go         ← Phase 1 新增
│   ├── gateway/
│   │   ├── server_test.go              ← Phase 2 新增
│   │   ├── allocator_test.go           ← Phase 2 新增
│   │   ├── proxy_test.go              ← Phase 2 新增
│   │   └── scheduler_client_test.go    ← Phase 2 新增
│   ├── compat/
│   │   └── v2_adapter_test.go          ← Phase 2 新增
│   ├── domain/
│   │   └── models_test.go              ← Phase 2 新增
│   └── config/
│       └── config_test.go              ← Phase 2 新增
│
├── pkg/
│   └── jsonutil/
│       └── jsonutil_test.go            ← Phase 2 新增
│
├── tests/
│   └── integration/
│       ├── distributed_test.go         ← Phase 2 新增
│       └── ...（现有 79 个测试）
│
└── bench/                              ← Phase 3 新增
    ├── docker-compose.yml
    ├── docker-compose.soak.yml
    ├── Makefile
    ├── prometheus.yml
    ├── grafana/
    │   ├── dashboards/
    │   │   └── onerouter.json
    │   └── provisioning/
    │       └── datasources.yml
    ├── mockfleet/
    │   ├── main.go
    │   └── Dockerfile
    ├── client/
    │   ├── main.go
    │   ├── config.go
    │   ├── runner.go
    │   ├── sse_consumer.go
    │   ├── reporter.go
    │   └── Dockerfile
    └── driver/
        ├── main.go
        ├── scenarios.go
        ├── assertions.go
        └── Dockerfile
```
