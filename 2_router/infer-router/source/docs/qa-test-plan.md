# OneRouter (RL-Router) QA 测试用例评审文档

> 目标读者：QA 工程师
> 用途：搭建 CI/CE 流程、设计测试用例、快速上手项目
> 约定：`$SCHEDULER` = scheduler 地址 (默认 `localhost:8080`)，`$GATEWAY` = gateway 地址

---

## 目录

1. [项目架构概览](#1-项目架构概览)
2. [部署模式详解](#2-部署模式详解)
3. [对外 API 测试用例](#3-对外-api-测试用例)
4. [模式差异矩阵测试](#4-模式差异矩阵测试)
5. [容错机制测试](#5-容错机制测试)
6. [配置与启动指南](#6-配置与启动指南)
7. [CI/CE 流程建议](#7-cice-流程建议)

---

## 1. 项目架构概览

### 1.1 系统定位

OneRouter 是面向 GPU 推理场景的流量调度系统，服务于强化学习训练中的 rollout 推理请求路由。核心目标：**极致负载均衡 + 万卡规模 + 半年长稳运行**。

### 1.2 核心组件

```
┌─────────────────────────────────────────────────────────┐
│                   训练框架 (PaddleRL)                     │
│           POST /v1/steps/start & /v1/instances           │
└─────────────────────┬───────────────────────────────────┘
                      │ HTTP
                      ▼
              ┌───────────────┐
              │   Scheduler   │  ← 全局负载管理、event-loop 串行处理
              │  :8080 (HTTP) │  ← 实例注册、调度策略、Step 状态机
              │  :9090 (gRPC) │
              └───────┬───────┘
                      │ gRPC (Allocate / Release / Register / Heartbeat)
                      ▼
        ┌─────────────────────────────┐
        │      Gateway (N 个节点)      │  ← 请求代理转发、SSE 流式支持
        │  :8081 (HTTP)               │  ← 与 Scheduler 心跳同步
        └─────────────┬───────────────┘
                      │ HTTP (反向代理)
                      ▼
        ┌─────────────────────────────┐
        │    Backend 推理实例 (万级)    │
        │  sglang / vllm / fastdeploy │
        └─────────────────────────────┘
```

### 1.3 Step 状态机

每一轮推理 (step) 有严格的状态隔离：

```
IDLE ──StartStep──▶ SERVING ──EndStep──▶ DRAINING ──(activeCount=0)──▶ IDLE
 │                    │                     │
 │  拒绝 Allocate     │  接受 Allocate      │  拒绝新 Allocate
 │  拒绝 EndStep      │  拒绝 StartStep     │  等待 in-flight Release
```

- **IDLE**: 无活跃 step，等待 StartStep
- **SERVING**: 活跃 step，接受分配请求
- **DRAINING**: step 已结束，等待所有 in-flight 请求 release 后自动转 IDLE

### 1.4 调度策略

| 策略名 | 说明 |
| ------ | ---- |
| `min_load` | 最小加权负载 (默认)，综合 ActiveRequests 和 GPU 数量 |
| `round_robin` | 简单轮询 |
| `min_request` | 最少活跃请求数 |
| `session_aware` | KV Cache 感知 + Session 亲和性 |

### 1.5 关键目录结构

```
RL-Router/
├── cmd/router/main.go              # 入口
├── internal/
│   ├── app/app.go                   # 顶层容器，按 Mode 组装
│   ├── config/config.go             # 配置结构
│   ├── domain/models.go             # 领域模型
│   ├── gateway/
│   │   ├── server.go                # Gateway 数据面
│   │   ├── proxy.go                 # 反向代理 (SSE)
│   │   ├── allocator.go             # Local/Remote Allocator
│   │   └── scheduler_client.go      # Gateway→Scheduler gRPC 客户端
│   ├── scheduler/
│   │   ├── server.go                # Event-loop 核心
│   │   ├── http_handler.go          # 控制面 HTTP API
│   │   ├── grpc_handler.go          # gRPC handler
│   │   ├── policy/                  # 调度策略
│   │   ├── store/                   # 实例状态存储
│   │   ├── registry/                # Gateway 注册表
│   │   ├── notifier/                # Step 状态推送
│   │   └── collector/               # 后端指标采集
│   └── compat/v2_adapter.go         # V2 兼容层
├── api/proto/router.proto           # gRPC 定义
├── tests/integration/               # 集成测试
└── config.example.yaml              # 配置示例
```

---

## 2. 部署模式详解

OneRouter 支持 3 种部署模式，通过 `--mode` 参数控制。

### 2.1 Hybrid 模式 (单节点，默认)

**适用场景**: 开发测试、小规模部署、单机 all-in-one

| 项目 | 值 |
| ---- | -- |
| 启动命令 | `./router --mode=hybrid` 或 `./router` (默认) |
| HTTP 端口 | `:8080` (所有 API) |
| gRPC 端口 | `:9090` (供外部 gateway 连接) |
| 注册的 API | **全量** (infra + gateway + scheduler + v2 compat + admin) |
| Allocator | `LocalAllocator` (进程内直调，无 gRPC 开销) |
| Gateway 标识 | `"hybrid"` |

**启动时行为**:

1. 创建 Scheduler (event-loop) + Gateway 组件
2. 状态机进入 `IDLE`，等待 `POST /v1/steps/start`
3. 无心跳循环 (Scheduler 和 Gateway 在同一进程)

**Step 开始时行为** (`POST /v1/steps/start`):

1. 重置所有实例 `ActiveRequests` 为 0
2. 清空 `allocDedup` 和 `releaseDedup` 去重表
3. 重置 `allocCounter = 0`
4. 状态转为 `SERVING`
5. `StepNotifier.BroadcastStepState()` 推送到所有已注册的外部 gateway (如有)

### 2.2 Scheduler + Gateway 分离模式

**适用场景**: 生产环境，1 Scheduler + N Gateway

#### Scheduler 节点

| 项目 | 值 |
| ---- | -- |
| 启动命令 | `./router --mode=scheduler --listen=:8080 --grpc=:9090` |
| 注册的 API | infra + scheduler 控制面 + v2 (不含 chat) + admin |
| **不注册** | `/v1/chat/completions`, `/v1/internal/step-state`, `/api/v2/chat/completions` |

#### Gateway 节点

| 项目 | 值 |
| ---- | -- |
| 启动命令 | `./router --mode=gateway --listen=:8081 --scheduler-addr=<scheduler>:9090` |
| 注册的 API | infra + gateway 数据面 + `/health` + `/api/v2/chat/completions` + admin |
| **不注册** | `/v1/steps/*`, `/v1/instances`, `/api/v2/start_infer` 等 |
| Allocator | `RemoteAllocator` (gRPC 调用 Scheduler) |

**Gateway 启动时行为**:

1. 通过 gRPC `Register` 向 Scheduler 注册 (指数退避重试 500ms → 10s)
2. 注册成功后启动心跳循环 (默认 5s 间隔, ±20% jitter)
3. 初始随机延迟，避免多 gateway 心跳同步

**Step 开始时行为** (分离模式):

1. Scheduler 侧：同 Hybrid 模式
2. `StepNotifier` 通过 HTTP `POST /v1/internal/step-state` 主动推送状态到所有 Gateway
3. Gateway 立即更新本地缓存的 `stepPhase` / `stepID`，无需等待下次心跳
4. 心跳作为兜底同步机制

---

## 3. 对外 API 测试用例

### 3.1 基础设施 API (所有模式)

#### T-INF-01: Liveness 探针

```bash
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/healthz
```

预期响应:

```
200
```

Body 为空。

#### T-INF-02: Readiness 探针 — IDLE 状态 (scheduler/hybrid)

```bash
curl -s http://localhost:8080/readyz
```

预期响应 (HTTP 503):

```json
{
  "status": "not_ready",
  "checks": {
    "step": "IDLE (step_id=0)"
  }
}
```

#### T-INF-03: Readiness 探针 — SERVING 状态 (scheduler/hybrid)

前置: 已执行 `POST /v1/steps/start`

```bash
curl -s http://localhost:8080/readyz
```

预期响应 (HTTP 200):

```json
{
  "status": "ready",
  "checks": {
    "step": "SERVING (step_id=1)"
  }
}
```

#### T-INF-04: Readiness 探针 — Gateway 模式 (scheduler 未连接)

```bash
curl -s http://localhost:8081/readyz
```

预期响应 (HTTP 503):

```json
{
  "status": "not_ready",
  "checks": {
    "scheduler_conn": "not_serving"
  }
}
```

#### T-INF-05: Prometheus 指标

```bash
curl -s http://localhost:8080/metrics | head -20
```

预期: 返回 Prometheus text format，包含 `onerouter_` 前缀指标。

---

### 3.2 Step 生命周期 API (scheduler/hybrid)

#### T-STEP-01: 启动 Step — 正常 (IDLE → SERVING)

```bash
curl -s -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d '{"step_id": 1}'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "step_id": 1,
  "phase": "SERVING"
}
```

#### T-STEP-02: 启动 Step — 非 IDLE 状态拒绝

前置: 当前已在 SERVING (step_id=1)

```bash
curl -s -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d '{"step_id": 2}'
```

预期响应 (HTTP 409):

```json
{
  "success": false,
  "message": "cannot start step: current phase is SERVING, expected IDLE",
  "step_id": 2
}
```

#### T-STEP-03: 启动 Step — 动态切换策略

```bash
curl -s -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d '{"step_id": 1, "policy": "round_robin"}'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "step_id": 1,
  "phase": "SERVING",
  "policy": "round_robin"
}
```

#### T-STEP-04: 结束 Step — 正常 (无 pending → IDLE)

```bash
curl -s -X POST http://localhost:8080/v1/steps/end \
  -H "Content-Type: application/json" \
  -d '{"step_id": 1}'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "step_id": 1,
  "phase": "IDLE",
  "pending_requests": 0
}
```

#### T-STEP-05: 结束 Step — step_id 不匹配

```bash
curl -s -X POST http://localhost:8080/v1/steps/end \
  -H "Content-Type: application/json" \
  -d '{"step_id": 99}'
```

预期响应 (HTTP 409):

```json
{
  "success": false,
  "message": "step_id mismatch: requested 99, current 1",
  "step_id": 99
}
```

#### T-STEP-06: 结束 Step — 非 SERVING 状态

```bash
curl -s -X POST http://localhost:8080/v1/steps/end \
  -H "Content-Type: application/json" \
  -d '{"step_id": 1}'
```

预期响应 (HTTP 409):

```json
{
  "success": false,
  "message": "cannot end step: current phase is IDLE, expected SERVING",
  "step_id": 1
}
```

#### T-STEP-07: 查询当前 Step 状态

```bash
curl -s http://localhost:8080/v1/steps/current
```

预期响应 (HTTP 200):

```json
{
  "step_id": 1,
  "phase": "SERVING",
  "registered_gateways": {
    "10.0.0.2:8081": {
      "gateway_addr": "10.0.0.2:8081",
      "last_heartbeat": "2025-01-01T00:00:00Z",
      "active_connections": 0
    }
  }
}
```

#### T-STEP-08: DRAINING → IDLE 自动转换

场景: EndStep 时有 pending 请求，全部 release 后自动转 IDLE

```bash
# 1. 启动 step 并发送请求 (请求挂起中)
# 2. 结束 step → 进入 DRAINING
curl -s -X POST http://localhost:8080/v1/steps/end \
  -d '{"step_id": 1}'
# 预期: phase=DRAINING, pending_requests>0

# 3. 所有请求完成 release 后，查询状态
curl -s http://localhost:8080/v1/steps/current
# 预期: phase=IDLE
```

#### T-STEP-09: 启动 Step — 无效 JSON body

```bash
curl -s -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d 'invalid json'
```

预期响应 (HTTP 400):

```json
{
  "success": false,
  "message": "invalid request body: ..."
}
```

---

### 3.3 运行时状态 API (scheduler/hybrid)

#### T-STATUS-01: 获取全量运行时状态 (IDLE)

```bash
curl -s http://localhost:8080/v1/status | python3 -m json.tool
```

预期响应 (HTTP 200):

```json
{
  "step_id": 0,
  "phase": "IDLE",
  "policy": "round_robin",
  "active_count": 0,
  "alloc_counter": 0,
  "instances": [],
  "registered_gateways": {}
}
```

**验证点**: IDLE 阶段所有计数器为零，instances 和 gateways 为空。

#### T-STATUS-02: SERVING 状态下查看实例负载

```bash
# 先注册实例并开始 step
curl -X PUT http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"instances":[{"id":"inst-1","host":"10.0.0.1","port":8000,"gpu_num":8}]}'

curl -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d '{"step_id": 1}'

# 查看运行时状态
curl -s http://localhost:8080/v1/status | python3 -m json.tool
```

预期响应 (HTTP 200):

```json
{
  "step_id": 1,
  "phase": "SERVING",
  "policy": "min_load",
  "active_count": 0,
  "alloc_counter": 0,
  "instances": [
    {
      "id": "inst-1",
      "endpoint": "10.0.0.1:8000",
      "gpu_num": 8,
      "active_requests": 0,
      "healthy": true,
      "actual_load": 0.0
    }
  ],
  "registered_gateways": {}
}
```

**验证点**: phase 为 SERVING，instances 包含实例详细负载信息 (active_requests, healthy, actual_load)。

#### T-STATUS-03: 验证 alloc_counter 递增

```bash
# 发送多次分配请求后检查 alloc_counter
curl -s http://localhost:8080/v1/status | jq '.alloc_counter'
# 预期: 等于已分配的请求总数
```

**验证点**: 每次成功 Allocate 后 alloc_counter 递增，StartStep 时重置为 0。

#### T-STATUS-04: 验证 registered_gateways 包含已注册网关

在 Scheduler+Gateway 分离部署模式下，Gateway 启动后会自动注册到 Scheduler。

```bash
curl -s http://localhost:8080/v1/status | jq '.registered_gateways'
```

预期响应片段:

```json
{
  "10.0.0.3:8081": {
    "gateway_addr": "10.0.0.3:8081",
    "last_heartbeat": "2025-01-01T00:00:05Z",
    "active_connections": 0
  }
}
```

**验证点**: 已注册的 gateway 出现在 registered_gateways 中，last_heartbeat 持续更新。

#### T-STATUS-05: QA 幽灵清理验证

用 `/v1/status` 替代手工检查，验证 Gateway 断连后幽灵分配被清理:

```bash
# 1. 分配请求使 active_count > 0
# 2. 杀掉 gateway 进程
# 3. 等待心跳超时 (默认 15s)
# 4. 轮询 /v1/status 验证清理
curl -s http://localhost:8080/v1/status | jq '{active_count, instances: [.instances[] | {id, active_requests}]}'
# 预期: active_count=0, 所有实例 active_requests=0
```

#### T-STATUS-06: gateway 模式不可用

```bash
# 以 gateway 模式启动
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/v1/status
# 预期: 404
```

---

### 3.4 实例管理 API (scheduler/hybrid)

#### T-INST-01: 注册实例

```bash
curl -s -X POST http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{
    "instances": [
      {"id": "inst-1", "host": "10.0.0.1", "port": 8000, "gpu_num": 8},
      {"id": "inst-2", "host": "10.0.0.2", "port": 8000, "gpu_num": 4}
    ]
  }'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "registered_count": 2
}
```

#### T-INST-02: 全量同步实例 (PUT)

```bash
curl -s -X PUT http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{
    "instances": [
      {"id": "inst-1", "host": "10.0.0.1", "port": 8000, "gpu_num": 8},
      {"id": "inst-3", "host": "10.0.0.3", "port": 8000, "gpu_num": 2}
    ]
  }'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "added": 1,
  "removed": 1,
  "updated": 1
}
```

说明: inst-2 被移除，inst-3 新增，inst-1 被更新。

#### T-INST-03: 反注册实例 (幂等)

```bash
curl -s -X DELETE http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"ids": ["inst-1", "inst-nonexistent"]}'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "unregistered_count": 1
}
```

说明: `inst-nonexistent` 不存在也不报错，幂等操作。

#### T-INST-04: 列出所有实例

```bash
curl -s http://localhost:8080/v1/instances
```

预期响应 (HTTP 200):

```json
{
  "instances": [
    {
      "id": "inst-1",
      "endpoint": "10.0.0.1:8000",
      "metrics_endpoint": "10.0.0.1:8000",
      "gpu_num": 8,
      "active_requests": 0
    }
  ]
}
```

#### T-INST-05: 注册实例 — port 非法值

```bash
curl -s -X POST http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"instances": [{"id": "bad", "host": "10.0.0.1", "port": 0, "gpu_num": 1}]}'
```

预期响应 (HTTP 400):

```json
{
  "success": false,
  "message": "instances[0]: port must be between 1 and 65535"
}
```

#### T-INST-06: 注册实例 — gpu_num=0

```bash
curl -s -X POST http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"instances": [{"id": "bad", "host": "10.0.0.1", "port": 8000, "gpu_num": 0}]}'
```

预期响应 (HTTP 400):

```json
{
  "success": false,
  "message": "instances[0]: gpu_num must be greater than 0"
}
```

#### T-INST-07: 空列表同步 → 清空所有实例

```bash
curl -s -X PUT http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{"instances": []}'
```

预期响应 (HTTP 200):

```json
{
  "success": true,
  "added": 0,
  "removed": 2,
  "updated": 0
}
```

---

### 3.5 Chat Completions API (gateway/hybrid)

#### T-CHAT-01: 正常转发请求

前置: 已注册实例 + 已 StartStep

```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llm",
    "messages": [{"role": "user", "content": "hello"}]
  }'
```

预期响应 (HTTP 200): 后端推理引擎返回的 JSON (OpenAI chat completions 格式)

```json
{
  "id": "chatcmpl-xxx",
  "object": "chat.completion",
  "choices": [
    {
      "index": 0,
      "message": {"role": "assistant", "content": "Hello!"},
      "finish_reason": "stop"
    }
  ]
}
```

#### T-CHAT-02: SSE 流式响应

```bash
curl -s -N -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "llm",
    "messages": [{"role": "user", "content": "hello"}],
    "stream": true
  }'
```

预期: 收到多个 `data: {...}\n\n` 格式的 SSE 事件，最后以 `data: [DONE]\n\n` 结束。

验证点:

- `Content-Type: text/event-stream`
- 每个 chunk 可独立解析为 JSON
- token 实时返回 (逐 chunk，非攒批)

#### T-CHAT-03: 非 SERVING 状态拒绝

前置: Step 状态为 IDLE

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "hello"}]}'
```

预期响应 (HTTP 503):

```
scheduler not serving
503
```

#### T-CHAT-04: 无可用实例

前置: 已 StartStep 但未注册任何实例

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "hello"}]}'
```

预期响应 (HTTP 503):

```
no available backend
503
```

#### T-CHAT-05: 后端返回非 200 — 原样透传

前置: 后端实例返回 HTTP 400

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llm", "messages": []}'
```

预期: 透传后端的 HTTP 400 + body。

#### T-CHAT-06: 后端连接拒绝

前置: 后端实例端口未监听

```bash
curl -s -w "\n%{http_code}" -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "hello"}]}'
```

预期响应 (HTTP 502):

```
proxy error: dial tcp 10.0.0.1:8000: connect: connection refused
502
```

#### T-CHAT-07: X-Trace-ID 透传

```bash
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "X-Trace-ID: my-trace-123" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "hello"}]}'
```

验证: 后端实例收到的请求中包含 `X-Trace-ID: my-trace-123` header。

#### T-CHAT-08: 并发负载均衡验证

```bash
# 注册 3 个实例后，并发 100 请求
for i in $(seq 1 100); do
  curl -s -X POST http://localhost:8080/v1/chat/completions \
    -H "Content-Type: application/json" \
    -d '{"model":"llm","messages":[{"role":"user","content":"test"}]}' &
done
wait
```

验证: 通过 `GET /v1/steps/current` 或 Prometheus 指标确认请求被均匀分发到 3 个实例。

---

### 3.6 V2 兼容 API (rollout-controller 向后兼容)

> V2 API 响应信封格式：始终返回 HTTP 200 + `{"status":{"code":N,"message":"..."}}`
> `code=0` 表示成功，`code=1` 表示失败

#### T-V2-01: 健康检查

```bash
curl -s -o /dev/null -w "%{http_code}" http://localhost:8080/health
```

预期: `200`

#### T-V2-02: 启动推理 (V2)

```bash
curl -s -X POST http://localhost:8080/api/v2/start_infer \
  -H "Content-Type: application/json" \
  -d '{"model_version": 1}'
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 0,
    "message": "success"
  }
}
```

#### T-V2-03: 启动推理 — model_version 缺失

```bash
curl -s -X POST http://localhost:8080/api/v2/start_infer \
  -H "Content-Type: application/json" \
  -d '{}'
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 1,
    "message": "model_version is required"
  }
}
```

#### T-V2-04: 停止推理 (V2)

```bash
curl -s -X POST http://localhost:8080/api/v2/stop_infer
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 0,
    "message": "success"
  }
}
```

#### T-V2-05: 停止推理 — 无活跃 step

```bash
curl -s -X POST http://localhost:8080/api/v2/stop_infer
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 1,
    "message": "no active step to stop"
  }
}
```

#### T-V2-06: V2 格式实例同步

```bash
curl -s -X PUT http://localhost:8080/api/v2/instances \
  -H "Content-Type: application/json" \
  -d '{
    "instances": [
      {
        "id": "inst-1",
        "host": "10.0.0.1",
        "infer_port": 8000,
        "metrics_port": 9100,
        "gpu_num": 8,
        "resource_type": "gpu",
        "gpu_type": "A100"
      }
    ]
  }'
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 0,
    "message": "success"
  }
}
```

注意: `resource_type`, `gpu_type`, `token_per_blocks` 字段被忽略，仅兼容接收。

#### T-V2-07: V2 Chat Completions (路径改写)

```bash
curl -s -X POST http://localhost:8080/api/v2/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model": "llm", "messages": [{"role": "user", "content": "hello"}]}'
```

预期: 行为与 `/v1/chat/completions` 完全一致，请求路径被改写为 `/v1/chat/completions` 转发到后端。

#### T-V2-08: Session 结束通知

```bash
curl -s -X POST http://localhost:8080/api/v2/session_finish \
  -H "Content-Type: application/json" \
  -d '{"session_id": "sess-abc-123"}'
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 0,
    "message": "success"
  }
}
```

#### T-V2-09: session_id 为空

```bash
curl -s -X POST http://localhost:8080/api/v2/session_finish \
  -H "Content-Type: application/json" \
  -d '{"session_id": ""}'
```

预期响应 (HTTP 200):

```json
{
  "status": {
    "code": 1,
    "message": "session_id is required"
  }
}
```

---

### 3.7 Admin API (所有模式)

#### T-ADM-01: 查询日志级别

```bash
curl -s http://localhost:8080/v1/admin/log-level
```

预期响应 (HTTP 200):

```json
{
  "level": "info",
  "access_level": "info",
  "modules": {
    "grpc": "info",
    "grpc-client": "info"
  }
}
```

#### T-ADM-02: 动态调整全局日志级别

```bash
curl -s -X PUT http://localhost:8080/v1/admin/log-level \
  -H "Content-Type: application/json" \
  -d '{"level": "debug"}'
```

预期响应 (HTTP 200): 空 body

验证: 后续日志输出 debug 级别内容。

#### T-ADM-03: 按模块调整日志级别

```bash
curl -s -X PUT http://localhost:8080/v1/admin/log-level \
  -H "Content-Type: application/json" \
  -d '{"module": "grpc", "level": "warn"}'
```

预期响应 (HTTP 200): 空 body

#### T-ADM-04: 非法日志级别

```bash
curl -s -w "\n%{http_code}" -X PUT http://localhost:8080/v1/admin/log-level \
  -H "Content-Type: application/json" \
  -d '{"level": "invalid_level"}'
```

预期: HTTP 400

---

### 3.8 gRPC API (gateway → scheduler)

> gRPC 测试可使用 `grpcurl` 工具

#### T-GRPC-01: Allocate — 正常分配

```bash
grpcurl -plaintext -d '{
  "trace_id": "test-trace-1",
  "request_id": "req-001",
  "gateway_id": "gw-1"
}' localhost:9090 router.SchedulerService/Allocate
```

预期响应:

```json
{
  "instanceId": "inst-1",
  "endpoint": "10.0.0.1:8000",
  "allocationId": "s1-a1"
}
```

#### T-GRPC-02: Allocate — request_id 去重

```bash
# 发送相同 request_id
grpcurl -plaintext -d '{
  "trace_id": "test-trace-1",
  "request_id": "req-001",
  "gateway_id": "gw-1"
}' localhost:9090 router.SchedulerService/Allocate
```

预期: 返回与第一次完全相同的 `instanceId` + `allocationId`。

#### T-GRPC-03: Release — 正常释放

```bash
grpcurl -plaintext -d '{
  "instance_id": "inst-1",
  "gateway_id": "gw-1",
  "allocation_id": "s1-a1",
  "duration_ms": 1500
}' localhost:9090 router.SchedulerService/Release
```

预期: 返回空 `{}`，成功释放。

#### T-GRPC-04: Release — 重复释放幂等

```bash
# 重复发送相同 allocation_id
grpcurl -plaintext -d '{
  "instance_id": "inst-1",
  "gateway_id": "gw-1",
  "allocation_id": "s1-a1"
}' localhost:9090 router.SchedulerService/Release
```

预期: 返回空 `{}`，静默成功，`activeCount` 不重复递减。

#### T-GRPC-05: Register — Gateway 注册

```bash
grpcurl -plaintext -d '{
  "gateway_id": "gw-1",
  "gateway_addr": "10.0.0.2:8081"
}' localhost:9090 router.SchedulerService/Register
```

预期响应:

```json
{
  "success": true,
  "phase": "STEP_IDLE",
  "stepId": "0"
}
```

#### T-GRPC-06: Heartbeat — 正常心跳

```bash
grpcurl -plaintext -d '{
  "gateway_id": "gw-1",
  "timestamp_ms": 1700000000000
}' localhost:9090 router.SchedulerService/Heartbeat
```

预期响应:

```json
{
  "success": true,
  "phase": "STEP_SERVING",
  "stepId": "1"
}
```

#### T-GRPC-07: Heartbeat — 未注册 gateway

```bash
grpcurl -plaintext -d '{
  "gateway_id": "unknown-gw",
  "timestamp_ms": 1700000000000
}' localhost:9090 router.SchedulerService/Heartbeat
```

预期响应:

```json
{
  "success": false,
  "phase": "STEP_IDLE",
  "stepId": "0"
}
```

说明: `success=false` 触发 gateway 端重注册逻辑。

---

## 4. 模式差异矩阵测试

验证不同模式下 API 注册的正确性。对每种模式启动实例，遍历所有 API 路径，验证返回 200/404/405。

| API 路径 | Method | hybrid | scheduler | gateway |
| -------- | ------ | ------ | --------- | ------- |
| `/healthz` | GET | 200 | 200 | 200 |
| `/readyz` | GET | 200/503 | 200/503 | 200/503 |
| `/metrics` | GET | 200 | 200 | 200 |
| `/v1/chat/completions` | POST | 200 | **404** | 200 |
| `/v1/internal/step-state` | POST | 200 | **404** | 200 |
| `/v1/steps/start` | POST | 200 | 200 | **404** |
| `/v1/steps/end` | POST | 200 | 200 | **404** |
| `/v1/steps/current` | GET | 200 | 200 | **404** |
| `/v1/instances` | POST | 200 | 200 | **404** |
| `/v1/instances` | PUT | 200 | 200 | **404** |
| `/v1/instances` | DELETE | 200 | 200 | **404** |
| `/v1/instances` | GET | 200 | 200 | **404** |
| `/v1/status` | GET | 200 | 200 | **404** |
| `/health` | GET | 200 | 200 | 200 |
| `/api/v2/start_infer` | POST | 200 | 200 | **404** |
| `/api/v2/stop_infer` | POST | 200 | 200 | **404** |
| `/api/v2/instances` | PUT | 200 | 200 | **404** |
| `/api/v2/chat/completions` | POST | 200 | **404** | 200 |
| `/api/v2/session_finish` | POST | 200 | 200 | **404** |
| `/v1/admin/log-level` | GET | 200 | 200 | 200 |
| `/v1/admin/log-level` | PUT | 200 | 200 | 200 |
| gRPC `:9090` | - | Y | Y | **不启动** |

### 自动化测试脚本思路

```bash
#!/bin/bash
# 对每种模式分别启动，逐一请求，比对状态码
MODES=("hybrid" "scheduler" "gateway")
for mode in "${MODES[@]}"; do
  # 启动对应模式
  # 遍历上表所有 API，记录 HTTP status code
  # 与预期矩阵比对
done
```

---

## 5. 容错机制测试

### 5.1 Gateway 自愈

#### T-FT-01: Scheduler 重启后 Gateway 自动重注册

**步骤**:

1. 启动 scheduler + gateway 分离模式
2. 确认 gateway 注册成功 (`GET /v1/steps/current` 可见 gateway)
3. 重启 scheduler 进程
4. 观察 gateway 日志

**预期**:

- Gateway 日志出现 `heartbeat failed` (最多 3 次)
- 然后出现 `too many heartbeat failures, attempting re-register`
- 最终出现 `registered with scheduler`
- 整个恢复过程 < 30s

#### T-FT-02: 心跳被拒后立即重注册

**步骤**:

1. 启动分离模式，gateway 已注册
2. 通过某种手段使 scheduler 的 registry 丢失该 gateway (如重启 scheduler)
3. 等待下次心跳

**预期**: 心跳返回 `success=false`，gateway 立即触发重注册。

#### T-FT-03: 网络分区恢复

**步骤**:

1. 使用 iptables 模拟 gateway→scheduler 网络断开
2. 等待 gateway 心跳超时 (3 次失败)
3. 恢复网络

**预期**: Gateway 自动重注册，后续请求正常处理。

### 5.2 幽灵分配清理

#### T-FT-04: Gateway 异常退出后 Scheduler 清理

**步骤**:

1. 分离模式，gateway 发送请求 (allocate 但未 release)
2. `kill -9` gateway 进程
3. 等待 heartbeat_timeout (默认 15s)

**预期**:

- Scheduler 日志出现 `gateway removed after heartbeat timeout`
- 该 gateway 的所有 allocation 被清理
- `activeCount` 正确递减

#### T-FT-05: 清理触发 DRAINING → IDLE

**步骤**:

1. 有 in-flight 请求，执行 EndStep → 进入 DRAINING
2. `kill -9` gateway (gateway 持有的 allocation 是唯一 pending)
3. 等待 heartbeat_timeout

**预期**: Scheduler 清理后 `activeCount=0`，自动从 DRAINING 转入 IDLE。

### 5.3 Step 状态隔离

#### T-FT-06: StartStep 重置负载

**步骤**:

1. Step 1 运行期间，实例 A 有 5 个 active_requests
2. EndStep 1 → StartStep 2

**预期**: Step 2 开始后所有实例 `active_requests = 0`。

#### T-FT-07: 去重表隔离

**步骤**:

1. Step 1 中发送 `request_id=req-001` → 获得 allocation
2. EndStep 1 → StartStep 2
3. Step 2 中发送相同 `request_id=req-001`

**预期**: Step 2 返回全新的 allocation (不命中 Step 1 的去重缓存)。

#### T-FT-08: 非法状态操作

```bash
# SERVING 时尝试 StartStep
curl -s -X POST http://localhost:8080/v1/steps/start -d '{"step_id":2}'
# 预期: 409

# IDLE 时尝试 EndStep
curl -s -X POST http://localhost:8080/v1/steps/end -d '{"step_id":1}'
# 预期: 409
```

### 5.4 请求幂等性

#### T-FT-09: 重复 Allocate

```bash
# 发送两次相同 request_id
grpcurl -plaintext -d '{"request_id":"dup-1","gateway_id":"gw-1"}' \
  localhost:9090 router.SchedulerService/Allocate
grpcurl -plaintext -d '{"request_id":"dup-1","gateway_id":"gw-1"}' \
  localhost:9090 router.SchedulerService/Allocate
```

**预期**: 两次返回完全相同的 `instance_id` + `allocation_id`。

#### T-FT-10: 重复 Release

```bash
# 发送两次相同 allocation_id
grpcurl -plaintext -d '{"instance_id":"inst-1","allocation_id":"s1-a1","gateway_id":"gw-1"}' \
  localhost:9090 router.SchedulerService/Release
grpcurl -plaintext -d '{"instance_id":"inst-1","allocation_id":"s1-a1","gateway_id":"gw-1"}' \
  localhost:9090 router.SchedulerService/Release
```

**预期**: 两次均成功，但 `activeCount` 只递减 1 次。

#### T-FT-11: Release 重试机制

**验证方法**: 模拟 gateway→scheduler gRPC Release 调用失败

**预期**: Gateway 自动重试最多 3 次，指数退避 100ms → 200ms → 400ms。查看 gateway 日志确认 `release failed, retrying`。

### 5.5 代理容错

#### T-FT-12: 后端连接拒绝

前置: 注册实例 endpoint 指向未监听端口

**预期**: HTTP 502 + `proxy error: dial tcp ...: connect: connection refused`

#### T-FT-13: 后端响应超时

前置: 注册实例指向一个非常慢的后端

**预期**: 随 client context deadline 返回 502。

#### T-FT-14: 后端返回非 200

前置: 后端返回 HTTP 500 + error body

**预期**: 原样透传 HTTP 500 状态码和 body 给客户端。

### 5.6 优雅退出

#### T-FT-15: SIGTERM 优雅退出

**步骤**:

1. 发送一个慢请求 (SSE 持续 10s)
2. `kill <pid>` (发送 SIGTERM)
3. 观察行为

**预期**:

- 不再接受新连接
- 等待 in-flight 请求完成
- 在 shutdown_grace (15s) 内完成退出

#### T-FT-16: 超时强退

**步骤**:

1. 发送一个超长请求 (> 15s)
2. 发送 SIGTERM

**预期**: 等待 15s 后强制退出，日志出现 `graceful shutdown timed out`。

#### T-FT-17: gRPC GracefulStop

**步骤**:

1. gRPC 有 in-flight RPC (如慢 Allocate)
2. 发送 SIGTERM

**预期**: gRPC `GracefulStop` 等待 RPC 完成，超时后 `Stop()` 强制停止。

### 5.7 Panic Recovery

#### T-FT-18: HTTP handler panic

**预期**: Recovery middleware 捕获 panic，返回 HTTP 500，进程继续运行，日志记录 panic stack。

#### T-FT-19: gRPC handler panic

**预期**: Recovery interceptor 捕获 panic，返回 gRPC `Internal` 错误，进程继续运行。

### 5.8 StepNotifier 容错

#### T-FT-20: Gateway 推送失败重试

**步骤**: 注册一个不可达的 gateway 地址，执行 StartStep

**预期**: 日志出现 3 次 `gateway notification failed`，间隔 100ms/200ms/300ms。

#### T-FT-21: 推送失败不阻塞 Scheduler

**步骤**: 同上

**预期**: StartStep 立即返回成功 (推送异步执行)，不影响后续 Allocate。

#### T-FT-22: 心跳兜底同步

**步骤**:

1. 断开 gateway 的 HTTP 接收 (让推送失败)
2. 等待下次心跳

**预期**: 心跳响应携带最新 `phase` + `step_id`，gateway 本地缓存更新。

---

## 6. 配置与启动指南

### 6.1 编译构建

```bash
# 方式 1: Makefile
make build
# 产出: bin/router

# 方式 2: 直接 go build
go build -o bin/router ./cmd/router

# 代理设置 (网络不通时)
export http_proxy=agent.baidu.com:8188
export https_proxy=agent.baidu.com:8188
export no_proxy=127.0.0.1,0.0.0.0,localhost,bcebos.com,baidu.com,baidu-int.com
```

### 6.2 三种模式启动命令

#### Hybrid (单节点，开发测试推荐)

```bash
# 最小启动 (所有默认值)
./bin/router

# 等价于
./bin/router --mode=hybrid --listen=:8080 --grpc=:9090 --policy=min_load

# 使用配置文件
./bin/router --config=config.yaml

# 开启 debug 日志
./bin/router --log-level=debug
```

#### Scheduler 节点

```bash
./bin/router --mode=scheduler \
  --listen=:8080 \
  --grpc=:9090 \
  --policy=min_load \
  --heartbeat-timeout=15s
```

#### Gateway 节点

```bash
./bin/router --mode=gateway \
  --listen=:8081 \
  --scheduler-addr=scheduler-host:9090 \
  --heartbeat-interval=5s
```

### 6.3 完整配置参数表

| 参数 | CLI Flag | 默认值 | 必填 | 说明 |
| ---- | -------- | ------ | ---- | ---- |
| `mode` | `--mode` | `hybrid` | N | `gateway` / `scheduler` / `hybrid` |
| `listen_addr` | `--listen` | `:8080` | N | HTTP 监听地址 |
| `grpc_addr` | `--grpc` | `:9090` | N | gRPC 监听地址 |
| `scheduler_addr` | `--scheduler-addr` | `""` | **gateway 模式必填** | 远端 scheduler gRPC 地址 |
| `policy` | `--policy` | `min_load` | N | `round_robin`, `min_load`, `min_request`, `session_aware` |
| `advertise_addr` | `--advertise-addr` | 自动检测 | N | Gateway 对外可达地址 |
| `heartbeat_interval` | `--heartbeat-interval` | `5s` | N | Gateway→Scheduler 心跳间隔 |
| `heartbeat_timeout` | `--heartbeat-timeout` | `15s` | N | Scheduler 判定 gateway 超时阈值 |
| `shutdown_grace` | `--shutdown-grace` | `15s` | N | 优雅退出超时 |
| `enable_h2c` | `--enable-h2c` | `false` | N | 前端 HTTP/2 cleartext |
| `backend_h2c` | `--backend-h2c` | `false` | N | 后端 h2c 连接 |
| `log.level` | `--log-level` | `info` | N | `debug`, `info`, `warn`, `error` |
| `log.format` | `--log-format` | `console` | N | `json` (生产) 或 `console` (开发) |
| `log.log_dir` | `--log-dir` | `""` | N | 日志目录，空=stderr |

### 6.4 配置文件方式

参考 `config.example.yaml`:

```yaml
mode: hybrid
listen_addr: ":8080"
grpc_addr: ":9090"
policy: min_load
heartbeat_interval: 5s
heartbeat_timeout: 15s
shutdown_grace: 15s

log:
  level: info
  format: json
  log_dir: /var/log/onerouter

metrics:
  enabled: true
```

CLI flag 优先级 > 配置文件 > 默认值。

### 6.5 Quick Start: 完整测试流程

```bash
# ===== 1. 编译 =====
make build

# ===== 2. 启动 (hybrid 模式) =====
./bin/router --log-level=debug &
ROUTER_PID=$!
sleep 1

# ===== 3. 检查健康 =====
echo "--- healthz ---"
curl -s http://localhost:8080/healthz
echo ""

echo "--- readyz (should be not_ready) ---"
curl -s http://localhost:8080/readyz | python3 -m json.tool
echo ""

# ===== 4. 注册后端实例 =====
echo "--- register instances ---"
curl -s -X PUT http://localhost:8080/v1/instances \
  -H "Content-Type: application/json" \
  -d '{
    "instances": [
      {"id":"inst-1","host":"10.0.0.1","port":8000,"gpu_num":8},
      {"id":"inst-2","host":"10.0.0.2","port":8000,"gpu_num":4}
    ]
  }' | python3 -m json.tool
echo ""

# ===== 5. 启动 Step =====
echo "--- start step ---"
curl -s -X POST http://localhost:8080/v1/steps/start \
  -H "Content-Type: application/json" \
  -d '{"step_id":1}' | python3 -m json.tool
echo ""

echo "--- readyz (should be ready) ---"
curl -s http://localhost:8080/readyz | python3 -m json.tool
echo ""

# ===== 6. 发送推理请求 =====
echo "--- chat completions ---"
curl -s -X POST http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"llm","messages":[{"role":"user","content":"hello"}]}'
echo ""

# ===== 7. 查看状态 =====
echo "--- current step ---"
curl -s http://localhost:8080/v1/steps/current | python3 -m json.tool
echo ""

# ===== 8. 结束 Step =====
echo "--- end step ---"
curl -s -X POST http://localhost:8080/v1/steps/end \
  -H "Content-Type: application/json" \
  -d '{"step_id":1}' | python3 -m json.tool
echo ""

# ===== 9. 停止 =====
kill $ROUTER_PID
```

### 6.6 V2 兼容流程 (对标 rollout-controller)

```bash
# 注册实例 (V2 字段格式)
curl -s -X PUT http://localhost:8080/api/v2/instances \
  -d '{"instances":[{"id":"inst-1","host":"10.0.0.1","infer_port":8000,"gpu_num":8}]}'

# 启动推理
curl -s -X POST http://localhost:8080/api/v2/start_infer \
  -d '{"model_version":1}'

# 发送请求
curl -s -X POST http://localhost:8080/api/v2/chat/completions \
  -H "Content-Type: application/json" \
  -d '{"model":"llm","messages":[{"role":"user","content":"hello"}]}'

# 停止推理
curl -s -X POST http://localhost:8080/api/v2/stop_infer
```

---

## 7. CI/CE 流程建议

### 7.1 单元测试 (CI - 必须通过)

```bash
# 运行全量单测 + race detector
make test

# 覆盖率报告
make test-cover
```

现有单测覆盖约 65 个测试文件，覆盖模块:

| 模块 | 测试文件 |
| ---- | -------- |
| config | `config_test.go`, `duration_test.go`, `flagoverride_test.go` |
| domain | `models_test.go`, `concurrency_test.go` |
| gateway | `server_test.go`, `proxy_test.go`, `allocator_test.go`, `scheduler_client_test.go` |
| scheduler | `server_test.go`, `http_handler_test.go`, `grpc_handler_test.go` |
| policy | `policy_test.go`, `min_load_batch_test.go`, `concurrency_test.go` |
| store | `node_state_store_test.go` |
| registry | `gateway_registry_test.go`, `gateway_registry_concurrency_test.go` |
| notifier | `step_notifier_test.go` |
| collector | `collector_test.go`, `fastdeploy_test.go`, `concurrency_test.go` |
| compat | `v2_adapter_test.go` |
| app | `app_test.go`, `main_test.go` |

### 7.2 集成测试 (CI - 必须通过)

```bash
cd tests/integration
go test -v -race -timeout=300s ./...
```

集成测试使用内置 mock backend (`tests/integration/mockbackend/`)，**无需外部依赖**。

| 测试文件 | 覆盖场景 |
| -------- | -------- |
| `chat_test.go` | 端到端请求转发 |
| `sse_test.go` | SSE 流式转发 |
| `sse_edge_test.go` | SSE 边界场景 |
| `error_test.go` | 错误场景 |
| `error_edge_test.go` | 错误边界场景 |
| `compat_test.go` | V2 兼容性 |
| `multi_backend_test.go` | 多后端负载均衡 |
| `continuation_test.go` | 连续请求 |
| `baseline_test.go` | 与 rollout-controller 标准答案对比 |

### 7.3 性能测试 (CI - 可选)

```bash
cd tests/integration
go test -v -run "Perf" -timeout=600s ./...
```

| 测试文件 | 覆盖场景 |
| -------- | -------- |
| `matrix_perf_test.go` | 多维度性能矩阵 |
| `scale_perf_test.go` | 万级实例扩展性 |

### 7.4 推荐 CE 阶段

| 阶段 | 优先级 | 测试内容 | 预计耗时 | 方法 |
| ---- | ------ | -------- | -------- | ---- |
| P0 冒烟 | 必须 | healthz + readyz + step lifecycle | 3 min | curl 脚本 (6.5 节) |
| P0 API 矩阵 | 必须 | 全量 API × 3 种模式 (第 4 章) | 10 min | 自动化脚本 |
| P1 容错-自愈 | 高 | gateway 自愈 + 幽灵清理 (5.1-5.2) | 5 min | docker-compose + kill |
| P1 容错-幂等 | 高 | allocate/release 幂等 (5.4) | 3 min | grpcurl 脚本 |
| P1 SSE | 高 | SSE 流式 + 大 token 量 | 5 min | mock backend |
| P1 优雅退出 | 高 | SIGTERM + 超时强退 (5.6) | 3 min | kill + 观察 |
| P2 并发压测 | 中 | 100/1000/10000 并发连接 | 30 min | 集成测试 perf |
| P2 长稳 | 中 | 72h 持续运行，step 循环 | 72h | 监控 + 告警 |
| P2 多策略 | 低 | 4 种调度策略正确性 | 10 min | 脚本 + 分布验证 |

### 7.5 Mock Backend 使用说明

集成测试已内置 mock backend，位于 `tests/integration/mockbackend/`:

- **可配置响应延迟**: 模拟慢后端
- **SSE 流式响应**: 逐 chunk 发送
- **错误场景注入**: 超时、连接拒绝、500 错误
- **多实例并行**: 同时启动多个 mock 后端

QA 也可以独立使用 mock backend 搭建本地测试环境。

### 7.6 关键日志关键词速查

排障时搜索以下关键词:

| 关键词 | 含义 |
| ------ | ---- |
| `registered with scheduler` | Gateway 注册成功 |
| `heartbeat failed` | 心跳失败 |
| `attempting re-register` | 触发重注册 |
| `gateway removed after heartbeat timeout` | 幽灵 gateway 被清理 |
| `allocate failed` | 分配失败 |
| `proxy error` | 代理转发失败 |
| `release failed, retrying` | Release 重试 |
| `step state updated via push` | Gateway 收到推送 |
| `graceful shutdown timed out` | 优雅退出超时 |

### 7.7 Prometheus 关键指标

| 指标名 | 类型 | 说明 |
| ------ | ---- | ---- |
| `onerouter_active_requests` | Gauge | 全局活跃请求数 |
| `onerouter_allocations_total` | Counter | 分配总数 |
| `onerouter_release_retries_total` | Counter | Release 重试次数 |
| `onerouter_proxy_backend_status` | Counter | 后端响应状态码分布 |
| `onerouter_heartbeat_total` | Counter | 心跳次数 (per gateway) |
| `onerouter_registered_gateways` | Gauge | 注册 gateway 数 |
| `onerouter_step_phase` | Gauge | 当前 step phase |
| `onerouter_step_id` | Gauge | 当前 step ID |
