# GC 热点分析报告

> 全量代码审计发现的潜在高频 GC 分配点，按严重程度分三档。尚未实施优化，仅作记录和排期参考。

---

## 评估背景

项目需支撑 **1w SSE 并发 + 10w 瞬时调度请求**。远程模式下单请求约 **13 次堆分配**，1w QPS 即每秒 **~13w 次堆分配**，GC 压力显著。

---

## Tier 1 — CRITICAL（每请求热路径）

| # | 文件 | 行号 | 问题 | 每请求分配 | 优化方向 |
|---|------|------|------|-----------|---------|
| G1 | `internal/gateway/proxy.go` | 38-50, 98-111 | **每请求新建 `httputil.ReverseProxy` + 3 个闭包**（Director / ErrorHandler / ModifyResponse） | 4 allocs | 按 host 缓存 ReverseProxy 或共享自定义 Transport |
| G2 | `internal/gateway/proxy.go` | 33, 93 | **每请求 `url.Parse()`**，后端 endpoint 有限可缓存 | 1 alloc | `sync.Map` 缓存已解析的 `*url.URL` |
| G3 | `internal/gateway/server.go` | 127-131 | **每请求 `&domain.RouteContext{}`** | 1 alloc | `sync.Pool` 复用 |
| G4 | `internal/gateway/server.go` | 152 | **每请求 `&domain.CostMetrics{}`** | 1 alloc | `sync.Pool` 复用 |
| G5 | `internal/scheduler/grpc_handler.go` | 29-34 | **每 gRPC Allocate `&domain.RouteContext{}`** | 1 alloc | `sync.Pool` 复用 |
| G6 | `internal/scheduler/grpc_handler.go` | 41-45 | **每 gRPC Allocate `&routerpb.AllocateResponse{}`** | 1 alloc | protobuf 结构体难池化，可接受 |
| G7 | `internal/scheduler/grpc_handler.go` | 49-53 | **每 gRPC Release `&domain.CostMetrics{}`** | 1 alloc | `sync.Pool` 复用 |
| G8 | `internal/gateway/allocator.go` | 59-64 | **每远程 Allocate `&routerpb.AllocateRequest{}`** | 1 alloc | protobuf 结构体难池化，可接受 |
| G9 | `internal/gateway/allocator.go` | 68-71 | **每远程 Allocate `&domain.Instance{}`** | 1 alloc | `sync.Pool` 复用 |
| G10 | `internal/gateway/allocator.go` | 75-81 | **每远程 Release `&routerpb.ReleaseRequest{}`** | 1 alloc | protobuf 结构体难池化，可接受 |

**合计**：代理 + gRPC 路径每请求约 **13 次堆分配**。

---

## Tier 2 — HIGH（批处理路径 / 高频操作）

| # | 文件 | 行号 | 问题 | 影响 | 优化方向 |
|---|------|------|------|------|---------|
| G11 | `internal/scheduler/server.go` | 156 | **`drainAll()` 每批次 `make([]*event, ...)`** | 1 alloc/batch | Server 字段复用（event-loop 单线程，无需同步） |
| G12 | `internal/scheduler/server.go` | 174 | **`processBatch()` 每批次 `make([]*event, ...)`** | 1 alloc/batch | 同上 |
| G13 | `internal/scheduler/server.go` | 239 | **`batchAllocate()` 每批次 `make([]int, ...)`** | 1 alloc/batch | 同上 |
| G14 | `internal/scheduler/server.go` | 266 | **`batchAllocate()` 每批次 `make([]*RouteContext, ...)`** | 1 alloc/batch | 同上 |
| G15 | `internal/scheduler/policy/min_load.go` | 137 | **`BatchSelect()` 每批次 `make([]BatchSelectResult, ...)`** | 1 alloc/batch（8192 元素 struct slice） | 调用者传入复用 slice |
| G16 | `internal/scheduler/server.go` | 230-231 | **非 SERVING 阶段 `fmt.Errorf` 每请求分配错误字符串** | burst 期间大量 error 分配 | 预定义哨兵错误 `var ErrNotServing = errors.New(...)` |
| G17 | `internal/gateway/server.go` | 141, 160, 161, 167 | **Prometheus `WithLabelValues(inst.ID)` 每请求 4 次** | 1w 实例 = 1w 时间序列 + mutex/map 查找 | 按 group/pool 聚合，降低标签基数 |
| G18 | `internal/scheduler/gateway_registry.go` | 99-105 | **每次心跳 COW 全量 clone map**（含 `*GatewayInfo` 重分配） | N entries/heartbeat | 心跳只更新时间戳时用原子字段，避免 clone |

---

## Tier 3 — MEDIUM（条件性触发 / 周期性操作）

| # | 文件 | 行号 | 问题 | 优化方向 |
|---|------|------|------|---------|
| G19 | `internal/gateway/server.go` | 191 | **`time.After(backoff)` 在重试循环中泄漏 timer** | 改用 `time.NewTimer` + `Stop()` / `Reset()` |
| G20 | `internal/gateway/proxy.go` | 31 | **`"http://" + targetEndpoint` 每请求字符串拼接** | 预计算缓存 |
| G21 | 4 个 policy 文件 | min_load:73,111,139,188; round_robin:37,63; min_request:27,51,54; cache_aware:41,80,83,114 | **`errors.New()` 每次 Select 失败分配新 error** | 改为包级哨兵错误变量 |
| G22 | `internal/scheduler/step_notifier.go` | 76, 79-80 | **`fmt.Sprintf` URL + `http.NewRequestWithContext` 每次重试** | URL 预计算；`bytes.Reader.Reset()` 复用 |
| G23 | `pkg/jsonutil/jsonutil.go` | 8-14 | **JSON 门面使用 `encoding/json` 而非 `go-json`** | 切换到 `github.com/goccy/go-json`，全局受益 |
| G24 | `internal/scheduler/node_state_store.go` | 166-173 | **`GetSnapshot()` 克隆全部 NodeState**，1w 实例 = 1w 次深拷贝 | 如非高频调用可接受；否则增量快照 |
| G25 | `internal/scheduler/server.go` | 405-408 | **`handleStartStep` 3 个 `make(map)` 无 size hint** | 用已知实例数作为 hint |
| G26 | `internal/scheduler/grpc_handler.go` | 81-85 | **每次心跳 `&routerpb.HeartbeatResponse{}`** | 预分配全局空响应（只读） |
| G27 | `internal/gateway/allocator.go` | 66, 85 | **`fmt.Errorf` error wrapping 每次失败** | 高频失败场景需评估，可用哨兵错误 + `%w` 减少分配 |
| G28 | `internal/scheduler/policy/min_load.go` | 221-225 | **policy `Reset()` 重建 `instanceMeta` map**（每 step 切换） | 用已知实例数作为 hint，或增量更新 |

---

## 优化优先级建议

| 优先级 | 编号 | 预期收益 | 改动复杂度 |
|--------|------|---------|-----------|
| **P0** | G1 | 消除 4 allocs/req，影响最大的单一热点 | 中（需管理缓存生命周期） |
| **P0** | G3, G4, G5, G7, G9 | 消除 5 allocs/req，`sync.Pool` 标准模式 | 低 |
| **P1** | G11-G14 | 消除 4 allocs/batch，单线程无需同步 | 低（Server 字段复用） |
| **P1** | G16, G21 | 消除 burst 期间大量 error 分配 | 低（定义包级变量） |
| **P1** | G17 | 降低 Prometheus 锁争用 + 时间序列膨胀 | 中（需重新设计标签维度） |
| **P2** | G2, G20 | 消除每请求字符串/URL 分配 | 低 |
| **P2** | G23 | 全局 JSON 性能提升 | 低（替换 import） |
| **P2** | G19, G22, G25, G26 | 减少周期性/条件性分配 | 低 |

---

## 量化汇总

| 路径 | 当前每请求堆分配 | 优化后目标 |
|------|-----------------|-----------|
| Gateway 代理（proxy + handler） | ~8 次 | ~2 次（protobuf 不可避免） |
| RemoteAllocator gRPC | ~5 次 | ~2 次 |
| Scheduler event-loop（per-batch） | ~5 次 | 0 次（字段复用） |
| **合计（远程模式单请求）** | **~13 次** | **~4 次** |
