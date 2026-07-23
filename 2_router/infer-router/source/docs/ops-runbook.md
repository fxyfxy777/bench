# OneRouter 运维排查手册

> 面向 SRE / 运维人员的操作手册。覆盖日常巡检、故障排查、告警规则、运行时调优。
>
> 对应代码版本：2026-03-08（observability 增强后）

---

## 目录

1. [可观测性基础设施概览](#1-可观测性基础设施概览)
2. [日志架构与文件说明](#2-日志架构与文件说明)
3. [Prometheus 指标全表](#3-prometheus-指标全表)
4. [HTTP API 速查](#4-http-api-速查)
5. [场景 1：系统 hang 住 / 无响应](#5-场景-1系统-hang-住--无响应)
6. [场景 2：请求结果不符合预期](#6-场景-2请求结果不符合预期)
7. [场景 3：大量请求下如何判断系统健康](#7-场景-3大量请求下如何判断系统健康)
8. [场景 4：Gateway 断连 / 重注册](#8-场景-4gateway-断连--重注册)
9. [场景 5：后端实例故障](#9-场景-5后端实例故障)
10. [场景 6：Scheduler 重启恢复](#10-场景-6scheduler-重启恢复)
11. [场景 7：磁盘满 / 日志轮转异常](#11-场景-7磁盘满--日志轮转异常)
12. [场景 8：Panic 导致进程不稳定](#12-场景-8panic-导致进程不稳定)
13. [场景 9：Event-loop 背压 / 性能退化](#13-场景-9event-loop-背压--性能退化)
14. [运行时调优操作手册](#14-运行时调优操作手册)
15. [推荐告警规则](#15-推荐告警规则)
16. [日常巡检 Checklist](#16-日常巡检-checklist)

---

## 1. 可观测性基础设施概览

```
                 ┌─────────────────────────────────────────┐
                 │              OneRouter 进程              │
                 │                                          │
                 │   ┌──────────┐      ┌──────────┐        │
                 │   │ Root     │      │ Access   │        │
                 │   │ Logger   │      │ Logger   │        │
                 │   │(同步)    │      │(异步)    │        │
                 │   └────┬─────┘      └────┬─────┘        │
                 │        │                  │              │
                 │   ┌────┴────┐        ┌───┴────┐         │
                 │   │ stderr  │        │ Buffered│         │
                 │   │ +       │        │ Writer  │         │
                 │   │ router  │        │ → access│         │
                 │   │ .log    │        │   .log  │         │
                 │   └─────────┘        └────────┘         │
                 │                                          │
                 │   ┌─────────────────────────────────┐     │
                 │   │ Admin Port (:8081 by default)   │     │
                 │   │  Prometheus ── /metrics ─→ Scraper│    │
                 │   │  Health ────── /healthz, /readyz │    │
                 │   │  Admin ─────── /v1/admin/log-level│   │
                 │   │  Debug ─────── /debug/pprof/*    │    │
                 │   └─────────────────────────────────┘     │
                 └──────────────────────────────────────────┘
```

### 双 Logger 架构

| Logger | 用途 | 写入目标 | 模式 |
|--------|------|---------|------|
| **Root** (控制面) | 启动、状态变更、心跳、panic、event-loop alive | stderr + `router.log` | 同步写入，保证可靠 |
| **Access** (请求面) | 请求进出、分配、代理、释放 | `access.log` | BufferedWriteSyncer 异步，可选采样 |

**关键特性**：两个 Logger 完全独立，互不 Tee。Access 异步不影响 Root 同步写入——即使 access.log 写入阻塞，router.log 的 "event-loop alive" 心跳仍正常输出。

---

## 2. 日志架构与文件说明

### 2.1 日志文件

| 文件 | 来源 Logger | 内容 | 轮转 |
|------|-----------|------|------|
| `router.log` | Root | 进程启动/关闭、step 状态变更、gateway 注册/过期、event-loop alive、panic recovery、gRPC 调用 | lumberjack: MaxSize/MaxBackups/MaxAge |
| `access.log` | Access | REQUEST_START、REQUEST_COMPLETE、SLOW_REQUEST、ALLOCATE、PROXY_FORWARD、PROXY_COMPLETE、RELEASE | 同上 |
| stderr | Root (始终) | 与 router.log 相同（Tee 输出） | 无轮转 |

### 2.2 日志配置

```yaml
log:
  level: info              # Root logger 级别
  access_level: info       # Access logger 级别
  format: json             # 生产建议 json，开发用 console
  slow_request: 30s        # 慢请求告警阈值
  log_dir: /var/log/onerouter  # 日志目录；空 = stderr-only
  max_size_mb: 500         # 单文件上限 MB
  max_backups: 5           # 保留历史文件数
  max_age_days: 30         # 历史文件保留天数
  compress: true           # gzip 压缩历史文件
  sample_initial: 0        # 每秒前 N 条全量记录（0+0=禁用采样）
  sample_thereafter: 0     # 之后 1/N 采样（0+0=禁用采样）
```

**GPU 调度场景建议**：`sample_initial=0, sample_thereafter=0`（禁用采样），QPS < 1000 时磁盘开销 < 10MB/分钟。

### 2.3 结构化日志 Schema

#### Event 类型（16 种）

| Event | Logger | 级别 | 含义 |
|-------|--------|------|------|
| `REQUEST_START` | Access | Debug | HTTP 请求进入 |
| `REQUEST_COMPLETE` | Access | Info | HTTP 请求完成 |
| `SLOW_REQUEST` | Access | Warn | 请求超过 slow_request 阈值 |
| `ALLOCATE` | Access | Debug/Warn | 请求实例分配 |
| `ALLOCATE_SLOW` | Access | Warn | 分配耗时超过阈值 |
| `PROXY_FORWARD` | Access | Debug | 开始转发到后端 |
| `PROXY_COMPLETE` | Access | Debug/Error | 代理转发完成 |
| `RELEASE` | Access | Debug/Warn/Error | 释放实例分配 |
| `BATCH_PROCESSED` | Access | Debug | event-loop 批次处理完成 |
| `STEP_START` | Root | Info | 推理步骤开始 |
| `STEP_END` | Root | Info | 推理步骤结束 |
| `GATEWAY_REGISTER` | Root | Info | Gateway 注册/续期 |
| `GATEWAY_EXPIRED` | Root | Info | Gateway 心跳超时被清理 |
| `GRPC_CALL` | Root | Debug/Warn | gRPC 调用日志 |
| `SCHEDULER_ALIVE` | Root | Info | event-loop 30s 心跳 |
| `PANIC_RECOVERED` | Root | Error | panic 被 recovery 中间件捕获 |

#### Status 状态码（4 种）

| Status | 含义 |
|--------|------|
| `OK` | 成功 |
| `FAIL` | 失败 |
| `TIMEOUT` | 超时 |
| `RETRY` | 重试中 |

#### Reason 错误归因（10 种）

| Reason | 含义 | 常见原因 |
|--------|------|---------|
| `UPSTREAM_TIMEOUT` | 后端超时 | 推理请求耗时过长 |
| `UPSTREAM_CONN_REFUSED` | 后端连接拒绝 | 实例宕机/未启动 |
| `UPSTREAM_5XX` | 后端 5xx | 推理引擎内部错误 |
| `NO_HEALTHY_BACKEND` | 无可用后端 | 所有实例不健康 |
| `ALL_OVERLOADED` | 全部过载 | 所有实例负载满 |
| `SCHEDULER_SLOW` | 调度慢 | event-loop 背压 |
| `SCHEDULER_NOT_SERVING` | 调度未就绪 | step 不在 SERVING 阶段 |
| `RELEASE_FAILED` | 释放失败 | 网络中断/scheduler 不可达 |
| `INTERNAL_ERROR` | 内部错误 | bug |
| `PROXY_ERROR` | 代理错误 | 连接级别错误 |

### 2.4 关键日志字段

| 字段 | 含义 | 出现位置 |
|------|------|---------|
| `trace_id` | 外部传入的请求追踪 ID | 所有 Access 日志 |
| `request_id` | 请求唯一标识 | 所有 Access 日志 |
| `gw` | Gateway 地址标识 | 所有 Access 日志 |
| `event` | 事件类型 | 所有结构化日志 |
| `status` | 结果状态 | REQUEST_COMPLETE, ALLOCATE, PROXY_COMPLETE, RELEASE |
| `reason` | 错误归因 | 所有 Warn/Error 级别日志 |
| `instance` | 后端实例 ID | ALLOCATE, PROXY_*, RELEASE |
| `allocation_id` | 分配幂等 ID | ALLOCATE, RELEASE |
| `backend_status` | 后端 HTTP 响应码 | PROXY_COMPLETE |
| `proxy_latency` | 代理转发耗时 | PROXY_COMPLETE |
| `latency` | 请求总耗时 | REQUEST_COMPLETE |
| `bytes` | 响应字节数 | REQUEST_COMPLETE |
| `phase` | step 阶段 | STEP_START, STEP_END, SCHEDULER_ALIVE |
| `step_id` | 当前 step ID | STEP_START, STEP_END, SCHEDULER_ALIVE |
| `active_requests` | 当前在途请求数 | SCHEDULER_ALIVE |
| `channel_depth` | event channel 深度 | SCHEDULER_ALIVE |
| `instances` | 实例总数 | SCHEDULER_ALIVE |

---

## 3. Prometheus 指标全表

### 3.1 请求指标

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `rl_router_requests_total` | Counter | `instance_id`, `status` | 路由请求总数（status: ok/error） |
| `rl_router_active_requests` | Gauge | `instance_id` | 每实例在途请求数 |
| `rl_router_request_duration_ms` | Histogram | `instance_id` | 请求延迟分布（ms）。桶：10, 20, 40, ..., 20480 |

### 3.2 调度指标

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `rl_router_step_phase` | Gauge | — | 当前 step 阶段（0=IDLE, 1=SERVING, 2=DRAINING） |
| `rl_router_step_id` | Gauge | — | 当前 step ID |
| `rl_router_event_channel_depth` | Gauge | — | event channel 当前深度（容量 131072） |
| `rl_router_event_loop_batch_size` | Histogram | — | 每批处理事件数。桶：1, 2, 4, ..., 8192 |
| `rl_router_alloc_dedup_hits_total` | Counter | — | Allocate 幂等去重命中次数 |
| `rl_router_release_dedup_hits_total` | Counter | — | Release 幂等去重命中次数 |

### 3.3 网关指标

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `rl_router_registered_gateways` | Gauge | — | 已注册 Gateway 数量 |
| `rl_router_heartbeat_total` | Counter | `gateway_id` | 每 Gateway 心跳次数 |
| `rl_router_release_retries_total` | Counter | — | Gateway Release 重试总次数 |

### 3.4 代理指标

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `rl_router_proxy_backend_status_total` | Counter | `instance_id`, `status_class` | 后端 HTTP 状态码分布（2xx/3xx/4xx/5xx） |

### 3.5 稳定性指标

| 指标名 | 类型 | 标签 | 说明 |
|--------|------|------|------|
| `rl_router_panic_recoveries_total` | Counter | `layer` | panic recovery 计数（layer: http/grpc） |

---

## 4. HTTP API 速查

> **Admin Port**：健康探针、指标、日志调整、pprof 等管理端点默认监听在独立的 admin 端口 `:8081`（通过 `--admin-listen` 指定）。如需恢复旧行为（所有端点共用主端口），启动时传入 `--admin-listen ""`。

### 4.1 基础设施（admin 端口，默认 :8081）

| 端点 | 方法 | 说明 | 响应 |
|------|------|------|------|
| `/healthz` | GET | Liveness 探针 | 200 空 body |
| `/readyz` | GET | Readiness 探针 | 200/503 + JSON |
| `/metrics` | GET | Prometheus 指标 | text/plain |
| `/debug/pprof/*` | GET | Go pprof 调试端点 | pprof 格式 |

`/readyz` 响应示例：
```json
{"status":"ready","checks":{"step":"serving (step_id=42)"}}
{"status":"not_ready","checks":{"scheduler_conn":"not_serving"}}
```

### 4.2 运行时管理（admin 端口，默认 :8081）

| 端点 | 方法 | 说明 |
|------|------|------|
| `/v1/admin/log-level` | PUT | 动态调整日志级别 |

```bash
# 调整控制面日志到 debug
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"level":"debug"}'

# 调整访问面日志到 debug
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"access_level":"debug"}'

# 调整特定模块
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"module":"access","level":"warn"}'
```

### 4.3 数据面（主端口，默认 :8080）

| 端点 | 方法 | 说明 | 模式 |
|------|------|------|------|
| `/v1/chat/completions` | POST | OpenAI 兼容推理入口 | gateway/hybrid |
| `/v1/internal/step-state` | POST | Step 状态推送（scheduler→gateway） | gateway |

### 4.4 控制面（主端口，默认 :8080）

| 端点 | 方法 | 说明 | 模式 |
|------|------|------|------|
| `/v1/instances` | PUT | 注册/更新实例列表 | scheduler/hybrid |
| `/v1/steps/start` | POST | 开始新推理步骤 | scheduler/hybrid |
| `/v1/steps/end` | POST | 结束当前推理步骤 | scheduler/hybrid |

### 4.5 gRPC 服务

| Service | Method | 说明 |
|---------|--------|------|
| `RouterService` | `Register` | Gateway 注册 |
| `RouterService` | `Heartbeat` | Gateway 心跳 |
| `RouterService` | `Allocate` | 请求实例分配 |
| `RouterService` | `Release` | 释放实例分配 |

---

## 5. 场景 1：系统 hang 住 / 无响应

### 排查决策树

```
1. 进程是否存活？
   │
   ├── curl http://<addr>/healthz
   │   ├── 可达 (200) → 进程存活，继续 2
   │   └── 不可达 → 进程崩溃或网络不通
   │       └── 检查 router.log 最后几行
   │           ├── 有 PANIC_RECOVERED → panic 已恢复但后续可能状态异常
   │           ├── 无 PANIC_RECOVERED → OOM / SIGKILL / 其他致命错误
   │           └── 检查 dmesg / journalctl -u onerouter
   │
   ├── 2. Event-loop 是否存活？
   │   │
   │   ├── grep "event-loop alive" router.log | tail -5
   │   │   ├── 30s 内有 → event-loop 正常
   │   │   │   ├── 检查 channel_depth 字段
   │   │   │   │   ├── > 100000 → 背压严重，跳转场景 9
   │   │   │   │   └── < 1000 → event-loop 空闲，问题在其他地方
   │   │   │   └── 检查 phase 字段
   │   │   │       ├── SERVING → 正常
   │   │   │       ├── IDLE → 未开始 step
   │   │   │       └── DRAINING → 等待在途请求完成
   │   │   │
   │   │   └── 超过 60s 无 → event-loop 疑似死锁
   │   │       ├── curl /readyz → 检查 readiness
   │   │       └── goroutine dump（如果有 pprof）:
   │   │           curl http://<addr>:8081/debug/pprof/goroutine?debug=2
   │   │
   ├── 3. 请求是否能到达 Gateway？
   │   │
   │   ├── grep "REQUEST_START" access.log | tail -5
   │   │   ├── 有新条目 → 请求到达了，检查 hang 在哪个阶段
   │   │   │   ├── 看最后的 phase 日志（切 Debug 后看得到）
   │   │   │   │   ├── "phase: allocate" 后无 "phase: proxy" → hang 在分配
   │   │   │   │   ├── "phase: proxy" 后无 "phase: release" → hang 在后端
   │   │   │   │   └── "phase: release" 后无 REQUEST_COMPLETE → hang 在释放
   │   │   │   │
   │   │   │   └── 动态切 Debug 看详细阶段：
   │   │   │       curl -X PUT http://<addr>/v1/admin/log-level \
   │   │   │         -d '{"access_level":"debug"}'
   │   │   │
   │   │   └── 无新条目 → 请求没到达 Gateway
   │   │       ├── 检查网络/DNS/TLS
   │   │       ├── 检查 /readyz → scheduler 是否 SERVING
   │   │       └── 检查上游客户端日志
   │
   └── 4. Prometheus 快速检查
       ├── rl_router_step_phase != 1 → Scheduler 不在 SERVING
       ├── rl_router_active_requests 持续不降 → 请求堆积在后端
       ├── rl_router_event_channel_depth 接近 131072 → event-loop 背压
       └── rate(rl_router_requests_total[5m]) == 0 → 无流量
```

### 常用排查命令

```bash
# 1. 检查进程存活
curl -s http://localhost:8081/healthz && echo "alive" || echo "dead"

# 2. 检查 event-loop 心跳（最近 5 条）
grep "event-loop alive" /var/log/onerouter/router.log | tail -5

# 3. 检查最近请求
grep "REQUEST_COMPLETE" /var/log/onerouter/access.log | tail -10

# 4. 检查慢请求
grep "SLOW_REQUEST" /var/log/onerouter/access.log | tail -10

# 5. 动态开启 debug 日志
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"access_level":"debug"}'

# 6. 检查 Readiness
curl -s http://localhost:8081/readyz | python3 -m json.tool
```

---

## 6. 场景 2：请求结果不符合预期

### 核心思路

Router 是透明代理，不解析也不修改请求/响应体。通过 `trace_id` + `backend_status` 字段可以精确区分问题出在哪一层。

### 排查决策树

```
1. 通过 trace_id 或 request_id 找到完整请求链
   │
   ├── grep "<trace_id>" access.log
   │   ├── 有 REQUEST_START → 请求到达了 Gateway
   │   └── 无 → 请求未到达或被采样丢弃
   │       └── 切到 Prometheus 确认：rl_router_requests_total
   │
   ├── 2. 检查分配阶段
   │   ├── event=ALLOCATE, status=OK → 分配成功
   │   │   └── 记下 instance 和 allocation_id
   │   └── event=ALLOCATE, status=FAIL → 分配失败
   │       ├── reason=NO_HEALTHY_BACKEND → 无可用后端
   │       ├── reason=SCHEDULER_NOT_SERVING → step 未开始
   │       └── reason=ALL_OVERLOADED → 全部过载
   │
   ├── 3. 检查代理阶段
   │   ├── event=PROXY_COMPLETE, status=OK
   │   │   ├── backend_status=200 → 后端正常返回
   │   │   │   └── Router 原样转发，问题在客户端解析
   │   │   ├── backend_status=4xx → 后端拒绝请求
   │   │   │   └── 检查请求体格式，可能是模型不支持的参数
   │   │   └── backend_status=5xx → 后端内部错误
   │   │       └── 检查推理引擎日志（vLLM/SGLang/FastDeploy）
   │   │
   │   └── event=PROXY_COMPLETE, status=FAIL
   │       ├── reason=PROXY_ERROR → 连接级错误
   │       │   ├── backend_status=0 → 连接被拒/超时
   │       │   └── 检查后端实例是否存活
   │       └── proxy_latency → 判断是超时还是即时失败
   │
   └── 4. 延迟归因
       ├── REQUEST_COMPLETE.latency ≈ PROXY_COMPLETE.proxy_latency
       │   └── 慢在后端推理
       ├── latency >> proxy_latency
       │   └── 慢在调度（分配/释放），检查 event-loop 背压
       └── 无 PROXY_COMPLETE 日志
           └── 请求 hang 在分配阶段
```

### 举证命令示例

```bash
# 1. 通过 trace_id 找完整请求链
grep "abc-123-trace" /var/log/onerouter/access.log

# 2. 找特定实例的后端错误
grep "PROXY_COMPLETE" /var/log/onerouter/access.log | grep '"status":"FAIL"' | tail -20

# 3. 统计后端状态码分布（Prometheus）
# rl_router_proxy_backend_status_total

# 4. 找到特定时段的错误请求
grep "REQUEST_COMPLETE" /var/log/onerouter/access.log | grep '"status":"FAIL"' | \
  awk -F'"' '/2026-03-08T14:/{print}'

# 5. 证明 Router 未篡改响应
# 如果 backend_status=200 且客户端收到异常结果，
# 则问题在后端返回的内容本身或客户端解析
```

---

## 7. 场景 3：大量请求下如何判断系统健康

### 核心思路

Access 日志可能被采样，但 **Prometheus 指标是全量的**。用指标判断全局健康，用日志排查个例。

### 健康判断清单

```
1. Prometheus 全量指标（不受采样影响）
   │
   ├── 错误率
   │   └── rate(rl_router_requests_total{status="error"}[5m])
   │       / rate(rl_router_requests_total[5m])
   │       ├── < 1% → 健康
   │       ├── 1-5% → 需关注
   │       └── > 5% → 需立即排查
   │
   ├── 延迟分布
   │   └── histogram_quantile(0.99, rl_router_request_duration_ms)
   │       ├── < 5000ms → 正常
   │       └── > 10000ms → 后端慢或调度慢
   │
   ├── 在途请求
   │   └── sum(rl_router_active_requests)
   │       ├── 稳定 → 正常
   │       └── 持续增长 → 请求堆积，后端处理不过来
   │
   ├── 后端状态码
   │   └── rate(rl_router_proxy_backend_status_total{status_class="5xx"}[5m])
   │       └── > 0 → 有后端错误，需检查具体实例
   │
   └── Event-loop 健康
       ├── rl_router_event_channel_depth
       │   ├── < 1000 → 正常
       │   └── > 100000 → 严重背压
       └── rl_router_event_loop_batch_size (histogram)
           └── 均值 > 1000 → 高吞吐，正常

2. router.log 宏观状态（不受采样影响）
   │
   └── 每 30s 一条 "event-loop alive"
       ├── phase → 当前阶段
       ├── active_requests → 全局在途请求数
       ├── channel_depth → event channel 深度
       └── instances → 注册实例数

3. 如需恢复全量日志（排查特定问题）
   │
   ├── 方式 1：运行时切换（无需重启）
   │   └── PUT /v1/admin/log-level {"access_level":"debug"}
   │
   └── 方式 2：修改配置重启
       └── sample_initial=0, sample_thereafter=0
```

### 关键 PromQL 查询

```promql
# 整体错误率
sum(rate(rl_router_requests_total{status="error"}[5m]))
/ sum(rate(rl_router_requests_total[5m]))

# P99 延迟
histogram_quantile(0.99,
  sum(rate(rl_router_request_duration_ms_bucket[5m])) by (le)
)

# 每实例在途请求数
rl_router_active_requests

# 后端 5xx 错误实例
sum by (instance_id) (
  rate(rl_router_proxy_backend_status_total{status_class="5xx"}[5m])
)

# Event-loop channel 使用率
rl_router_event_channel_depth / 131072

# Gateway 存活数
rl_router_registered_gateways

# 幂等去重率（高去重率说明网络重试频繁）
rate(rl_router_alloc_dedup_hits_total[5m])
rate(rl_router_release_dedup_hits_total[5m])
```

---

## 8. 场景 4：Gateway 断连 / 重注册

### 自愈流程

```
Gateway 断连
    │
    ├── Gateway 侧（scheduler_client.go）
    │   ├── 心跳连续 3 次失败 → 自动重注册
    │   ├── 心跳被 scheduler 拒绝 → 自动重注册
    │   └── 重注册指数退避：500ms → 1s → 2s → ... → 10s
    │
    └── Scheduler 侧（gateway_registry.go）
        ├── 心跳超时（默认 15s）→ 标记 Gateway 过期
        ├── 过期 → 清理该 Gateway 的所有幽灵分配
        └── 日志：GATEWAY_EXPIRED + "cleaned up ghost allocations"
```

### 排查命令

```bash
# 1. Scheduler 侧：检查 Gateway 过期和清理
grep "GATEWAY_EXPIRED\|ghost allocations" /var/log/onerouter/router.log | tail -10

# 2. Gateway 侧：检查重注册
grep "re-registered\|register failed\|heartbeat failed" /var/log/onerouter/router.log | tail -10

# 3. Prometheus：Gateway 数量变化
# rl_router_registered_gateways

# 4. Prometheus：心跳次数（每 gateway）
# rl_router_heartbeat_total

# 5. 确认恢复：readyz 检查
curl -s http://<gateway-addr>:8081/readyz | python3 -m json.tool
```

### 常见问题

| 现象 | 原因 | 解决 |
|------|------|------|
| Gateway 反复注册/过期 | 网络抖动或 scheduler 负载高 | 增大 heartbeat_timeout |
| ghost allocations 清理后 active_requests 不降 | 其他 Gateway 的请求仍在途 | 正常，等请求完成 |
| 重注册后 step 状态不同步 | 注册时 scheduler 推送当前 phase | 检查 "step state updated" 日志 |

---

## 9. 场景 5：后端实例故障

### 排查决策树

```
后端实例疑似故障
    │
    ├── 1. 检查 PROXY_COMPLETE 日志
    │   ├── grep "PROXY_COMPLETE" access.log | grep "instance=<id>"
    │   │   ├── backend_status=0 频繁出现 → 连接级故障
    │   │   ├── backend_status=5xx 频繁出现 → 推理引擎故障
    │   │   └── proxy_latency 异常高 → 实例响应慢
    │   │
    │   └── 2. Prometheus 确认
    │       ├── rl_router_proxy_backend_status_total{instance_id="<id>",status_class="5xx"}
    │       ├── rl_router_active_requests{instance_id="<id>"} → 是否请求堆积
    │       └── rl_router_request_duration_ms{instance_id="<id>"} → 延迟分布
    │
    └── 3. 当前状态（注意：无自动摘除机制，P0 待实现）
        ├── 故障实例仍会被分配流量
        └── 临时方案：通过 /v1/instances PUT 更新实例列表，排除故障实例
```

> **注意**：当前版本（2026-03-08）尚未实现后端实例自动健康检测和摘除。如果后端实例故障，需要手动更新实例列表。这是 P0 待实现项。

---

## 10. 场景 6：Scheduler 重启恢复

### 影响范围

Scheduler 重启后丢失所有内存状态：
- 实例注册列表
- 分配状态
- Step 阶段
- Gateway 注册

### 恢复流程

```
Scheduler 重启
    │
    ├── 1. 上游系统需重新发送 /v1/instances PUT 注册实例
    │
    ├── 2. 上游系统需重新发送 /v1/steps/start 开始新 step
    │
    ├── 3. Gateway 自动重注册（心跳失败触发）
    │   └── 预期延迟：5-15s（心跳间隔 + 失败检测 + 重试退避）
    │
    └── 4. 验证恢复
        ├── curl /readyz → checks.step = "serving"
        ├── rl_router_registered_gateways > 0
        └── rl_router_step_phase == 1
```

> **注意**：Scheduler 无持久化 / 无 HA，这是 P0 待实现项。半年长跑任务中 Scheduler 崩溃一次即全量中断。

---

## 11. 场景 7：磁盘满 / 日志轮转异常

### 预防配置

```yaml
log:
  log_dir: /var/log/onerouter
  max_size_mb: 500       # 单文件 500MB
  max_backups: 5         # 保留 5 个历史文件
  max_age_days: 30       # 最多保留 30 天
  compress: true         # gzip 压缩（~10x 压缩比）
```

**磁盘占用估算**：
- router.log: ~500MB × 6 = ~3GB（当前 + 5 个历史）
- access.log: ~500MB × 6 = ~3GB
- 压缩后：~600MB（总计）
- 最大占用：~6.6GB（轮转瞬间，新旧文件并存）

### 磁盘满排查

```bash
# 1. 检查日志目录大小
du -sh /var/log/onerouter/

# 2. 检查每个文件大小
ls -lh /var/log/onerouter/

# 3. 检查文件系统使用率
df -h /var/log/onerouter/

# 4. 紧急清理（手动删除最老的轮转文件）
ls -lt /var/log/onerouter/*.gz | tail -5
# rm <oldest files>

# 5. 紧急降低日志量（运行时调整）
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"access_level":"warn"}'
```

### 日志轮转异常

| 现象 | 可能原因 | 解决 |
|------|---------|------|
| 文件持续增长不轮转 | lumberjack 未正确初始化 | 检查 log_dir 配置 |
| 磁盘满后无日志输出 | stderr 未受影响但 file 写入失败 | 清理空间后自动恢复 |
| 压缩文件过多 | max_backups 配置过大 | 调小 max_backups |

> **关键特性**：即使 access.log 磁盘满写入失败，Root logger 仍通过 stderr 输出 "event-loop alive" 心跳，不会静默丢失。

---

## 12. 场景 8：Panic 导致进程不稳定

### 防护机制

| 层 | 中间件 | 触发行为 |
|----|--------|---------|
| HTTP | `RecoveryMiddleware` | 捕获 panic → Error 日志 + stack → 返回 500 |
| gRPC | `RecoveryUnaryServerInterceptor` | 捕获 panic → Error 日志 + stack → 返回 Internal |

中间件链顺序：
- HTTP: AccessLog (outermost) → Recovery → mux
- gRPC: Recovery → Logging → Handler

### 排查命令

```bash
# 1. 检查是否有 panic recovery
grep "PANIC_RECOVERED" /var/log/onerouter/router.log

# 2. 查看完整 stack trace
grep -A 50 "PANIC_RECOVERED" /var/log/onerouter/router.log

# 3. Prometheus：panic 计数
# rl_router_panic_recoveries_total{layer="http"}
# rl_router_panic_recoveries_total{layer="grpc"}

# 4. 按时间统计 panic 频率
grep "PANIC_RECOVERED" /var/log/onerouter/router.log | \
  awk '{print $1}' | cut -c1-13 | sort | uniq -c
```

### Panic 后的影响

| 情况 | 影响 | 恢复 |
|------|------|------|
| HTTP handler panic | 单个请求返回 500，进程继续运行 | 自动恢复 |
| gRPC handler panic | 单个 RPC 返回 Internal，进程继续运行 | 自动恢复 |
| 高频 panic | 内存/CPU 波动，stack dump 开销 | 需修复根因 |
| init/main panic | 进程退出（recovery 仅覆盖请求处理） | 需人工重启 |

---

## 13. 场景 9：Event-loop 背压 / 性能退化

### 排查决策树

```
请求变慢 / 分配延迟增加
    │
    ├── 1. 检查 event channel 深度
    │   ├── rl_router_event_channel_depth
    │   │   ├── < 1000 → event-loop 空闲，瓶颈在别处
    │   │   ├── 1000-100000 → 中等负载
    │   │   └── > 100000 → 严重背压
    │   │       ├── 检查 batch_size 是否在增长
    │   │       └── 检查是否有慢操作（policy select 时间）
    │   │
    │   └── router.log 中 "event-loop alive" 的 channel_depth 字段
    │
    ├── 2. 检查批处理大小
    │   └── rl_router_event_loop_batch_size
    │       ├── 均值 < 10 → 低 QPS
    │       ├── 均值 100-1000 → 正常高 QPS
    │       └── 均值 > 5000 → 极高负载，考虑增加 Gateway
    │
    ├── 3. 检查实例数量
    │   └── router.log "event-loop alive" 的 instances 字段
    │       ├── 匹配预期 → 正常
    │       └── < 预期 → 有实例掉线
    │
    └── 4. 检查去重率
        ├── rate(rl_router_alloc_dedup_hits_total[5m])
        │   └── 高 → 大量重复请求（客户端重试过快）
        └── rate(rl_router_release_dedup_hits_total[5m])
            └── 高 → Gateway release 重试频繁
```

### 性能调优建议

| 指标 | 阈值 | 建议 |
|------|------|------|
| channel_depth > 100000 | 持续 1 分钟 | 增加 Gateway 分散压力 |
| batch_size 均值 > 5000 | 持续 5 分钟 | 考虑横向扩展 |
| active_requests 持续增长 | 无回落 | 后端处理能力不足 |
| alloc_dedup_hits 飙升 | rate > 100/s | 客户端重试策略过于激进 |
| release_retries 飙升 | rate > 10/s | Gateway→Scheduler 网络不稳定 |

---

## 14. 运行时调优操作手册

### 14.1 动态调整日志级别

```bash
# 查看当前设置（通过 /readyz 间接确认服务状态）
curl -s http://localhost:8081/readyz

# 开启全量 debug 日志
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"level":"debug","access_level":"debug"}'

# 仅对特定模块开启 debug
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"module":"access","level":"debug"}'

# 恢复正常级别
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"level":"info","access_level":"info"}'

# 紧急降噪（只输出 error）
curl -X PUT http://localhost:8081/v1/admin/log-level \
  -d '{"level":"error","access_level":"error"}'
```

### 14.2 手动更新实例列表

```bash
# 排除故障实例
curl -X PUT http://localhost:8080/v1/instances \
  -H 'Content-Type: application/json' \
  -d '{
    "instances": [
      {"id": "vllm-0", "endpoint": "http://10.0.0.1:8000"},
      {"id": "vllm-2", "endpoint": "http://10.0.0.3:8000"}
    ]
  }'
```

### 14.3 Step 生命周期管理

```bash
# 开始新 step
curl -X POST http://localhost:8080/v1/steps/start \
  -H 'Content-Type: application/json' \
  -d '{"step_id": 42}'

# 结束当前 step
curl -X POST http://localhost:8080/v1/steps/end

# 确认状态
curl -s http://localhost:8081/readyz
```

---

## 15. 推荐告警规则

### Alertmanager 规则

```yaml
groups:
  - name: onerouter
    rules:
      # P0: 进程不可用
      - alert: OneRouterDown
        expr: up{job="onerouter"} == 0
        for: 30s
        labels:
          severity: critical
        annotations:
          summary: "OneRouter 进程不可用"

      # P0: Event-loop 严重背压
      - alert: EventLoopBackpressure
        expr: rl_router_event_channel_depth > 100000
        for: 1m
        labels:
          severity: critical
        annotations:
          summary: "Event-loop channel 深度 {{ $value }}，接近 131072 上限"

      # P0: 错误率飙升
      - alert: HighErrorRate
        expr: >
          sum(rate(rl_router_requests_total{status="error"}[5m]))
          / sum(rate(rl_router_requests_total[5m])) > 0.05
        for: 2m
        labels:
          severity: critical
        annotations:
          summary: "请求错误率 {{ $value | humanizePercentage }}，超过 5%"

      # P1: 请求堆积
      - alert: RequestPileup
        expr: sum(rl_router_active_requests) > 5000
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "在途请求 {{ $value }}，持续 5 分钟未下降"

      # P1: P99 延迟过高
      - alert: HighP99Latency
        expr: >
          histogram_quantile(0.99,
            sum(rate(rl_router_request_duration_ms_bucket[5m])) by (le)
          ) > 30000
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "P99 延迟 {{ $value }}ms，超过 30s"

      # P1: Gateway 数量下降
      - alert: GatewayLost
        expr: rl_router_registered_gateways < 1
        for: 30s
        labels:
          severity: warning
        annotations:
          summary: "注册 Gateway 数量 {{ $value }}，可能有 Gateway 断连"

      # P1: Scheduler 不在 SERVING
      - alert: SchedulerNotServing
        expr: rl_router_step_phase != 1
        for: 2m
        labels:
          severity: warning
        annotations:
          summary: "Scheduler step_phase={{ $value }}（非 SERVING），新请求将被拒绝"

      # P2: 后端 5xx 错误
      - alert: BackendErrors
        expr: >
          sum by (instance_id) (
            rate(rl_router_proxy_backend_status_total{status_class="5xx"}[5m])
          ) > 1
        for: 5m
        labels:
          severity: warning
        annotations:
          summary: "实例 {{ $labels.instance_id }} 后端 5xx 错误率 {{ $value }}/s"

      # P2: Panic 发生
      - alert: PanicRecovered
        expr: increase(rl_router_panic_recoveries_total[5m]) > 0
        labels:
          severity: warning
        annotations:
          summary: "{{ $labels.layer }} 层发生 {{ $value }} 次 panic"

      # P2: Release 重试频繁
      - alert: ReleaseRetrySpike
        expr: rate(rl_router_release_retries_total[5m]) > 10
        for: 5m
        labels:
          severity: info
        annotations:
          summary: "Release 重试率 {{ $value }}/s，Gateway→Scheduler 通信可能不稳定"

      # P2: 指标上报中断
      - alert: MetricsStale
        expr: absent(rl_router_step_phase)
        for: 1m
        labels:
          severity: warning
        annotations:
          summary: "rl_router_step_phase 指标消失，Prometheus 抓取可能中断"
```

---

## 16. 日常巡检 Checklist

### 每日巡检

| # | 检查项 | 命令/指标 | 正常标准 |
|---|--------|----------|---------|
| 1 | 进程存活 | `curl /healthz` | 200 |
| 2 | Readiness | `curl /readyz` | status=ready |
| 3 | Event-loop 心跳 | `grep "event-loop alive" router.log \| tail -1` | 30s 内有输出 |
| 4 | Gateway 数量 | `rl_router_registered_gateways` | 等于预期 Gateway 数 |
| 5 | Step 阶段 | `rl_router_step_phase` | 1 (SERVING) |
| 6 | 错误率 | `rate(requests_total{status=error}[1h])` | < 1% |
| 7 | P99 延迟 | `histogram_quantile(0.99, ...)` | < 30s |
| 8 | Panic 计数 | `rl_router_panic_recoveries_total` | 0 |
| 9 | 磁盘空间 | `df -h /var/log/onerouter/` | < 80% |
| 10 | 日志轮转 | `ls -lt /var/log/onerouter/*.gz` | 有按预期生成的 .gz 文件 |

### 每周巡检

| # | 检查项 | 说明 |
|---|--------|------|
| 1 | 去重率趋势 | alloc_dedup / release_dedup 是否有上升趋势 |
| 2 | 延迟趋势 | P99/P95 是否有缓慢上升 |
| 3 | 后端错误分布 | 是否有特定实例持续 5xx |
| 4 | 日志磁盘占用趋势 | 是否需要调整 max_backups / max_size_mb |
| 5 | Release 重试趋势 | Gateway→Scheduler 通信质量 |

---

## 附录：快速命令参考

```bash
# === 健康检查（admin 端口 :8081） ===
curl -s http://localhost:8081/healthz
curl -s http://localhost:8081/readyz | python3 -m json.tool

# === 日志搜索 ===
# 按 trace_id 搜索
grep "trace_id_value" /var/log/onerouter/access.log

# 搜索错误
grep '"status":"FAIL"' /var/log/onerouter/access.log | tail -20

# 搜索慢请求
grep "SLOW_REQUEST" /var/log/onerouter/access.log | tail -10

# 搜索 panic
grep "PANIC_RECOVERED" /var/log/onerouter/router.log

# 搜索 Gateway 过期
grep "GATEWAY_EXPIRED" /var/log/onerouter/router.log

# Event-loop 状态
grep "event-loop alive" /var/log/onerouter/router.log | tail -5

# === 运行时调整（admin 端口 :8081） ===
# 开启 debug
curl -X PUT http://localhost:8081/v1/admin/log-level -d '{"access_level":"debug"}'

# 恢复 info
curl -X PUT http://localhost:8081/v1/admin/log-level -d '{"access_level":"info"}'

# === Prometheus PromQL ===
# 总 QPS
sum(rate(rl_router_requests_total[5m]))

# 错误率
sum(rate(rl_router_requests_total{status="error"}[5m])) / sum(rate(rl_router_requests_total[5m]))

# P99 延迟
histogram_quantile(0.99, sum(rate(rl_router_request_duration_ms_bucket[5m])) by (le))

# 在途请求
sum(rl_router_active_requests)

# Event-loop channel 使用率
rl_router_event_channel_depth / 131072

# 后端 5xx
sum by (instance_id) (rate(rl_router_proxy_backend_status_total{status_class="5xx"}[5m]))
```
