# OneRouter 性能优化全览

本文档汇总 RL-Router 全部性能优化点，按类别归档。每个优化点统一格式：问题 → 方案 → 核心代码 → 量化效果。

---

## 总览

| # | 优化点 | 类别 | 效果 |
|---|--------|------|------|
| O1 | 零外部 I/O 调度 | 架构 | 单次调度 3-15ms → <0.01ms |
| O2 | Event-Loop 批量排空 | 架构 | 10w 瞬时请求不阻塞 |
| O3 | 堆排序批量选择（BatchSelect） | 算法 | 1w 节点 × 8192 请求 **680x** 操作数下降 |
| O4 | NodeStateStore COW | 数据结构 | GetNodes 311μs → 0ns，10K 节点零拷贝 |
| O5 | GatewayRegistry COW | 数据结构 | GetAll 63μs → 0.43ns，**147,000x** |
| O6 | RoundRobin 排序缓存 | 数据结构 | Select 1.49ms → 13ns，**113,000x** |
| O7 | NodeState 原子化 | 并发 | 消除 1w 节点遍历的 2w 次锁操作 |
| O8 | BatchSelect 值类型堆 | 分配消除 | 10K 节点 10,003 → 2 allocs |
| O9 | BatchSelect 堆缓冲区池化 | 对象池 | 500KB → 98KB / call |
| O10 | Event 结构体池化 | 对象池 | Release 400B → 147B，10w 突发 1.2GB → 76MB |
| O11 | allocResult channel 池化 | 对象池 | 10w 请求避免 10w 次 channel 分配 |
| O12 | GenerateRequestID 重写 | 分配消除 | 2.1μs/13allocs → ~30ns/0allocs |
| O13 | allocCacheEntry 值类型 | 分配消除 | 每请求减少 1 alloc |
| O14 | nextAllocationID scratch buffer | 分配消除 | ~3 allocs → 1 alloc |
| O15 | StepNotifier 连接池 | 网络 | 100-Gateway 广播 3.8ms → 1.2ms |
| O16 | MinLoadPolicy Resettable | 接口 | Step 切换时正确清理状态 |
| O17 | Proxy 按 host 缓存 + BufferPool | 分配消除+对象池 | 每请求 -6 allocs + 池化 32KB transfer buffer |
| O18 | RouteContext sync.Pool | 对象池 | 每请求 -1 alloc |
| O19 | CostMetrics sync.Pool | 对象池 | 每请求 -1 alloc |
| O20 | Event-loop 缓冲区复用 | 分配消除 | 每批次 -4 allocs（drainBuf + allocsBuf + pendingBuf + reqsBuf） |
| O21 | flushAllocs 方法化 | 分配消除 | 消除每批次 1 次闭包分配 |
| O22 | 哨兵错误 | 分配消除 | 高并发拒绝时消除 N 次 errors.New 分配 |
| O23 | BatchSelect 结果缓冲区复用 | 分配消除 | MinLoad + SessionAware 每批次 -1~3 allocs（最大 576KB） |
| O24 | BatchSelector 类型断言缓存 | 微优化 | 消除每批次 1 次 interface 类型断言 |
| O25 | time.After 泄漏修复 | 正确性+GC | 修复 timer 泄漏，减少长期运行 GC 压力 |
| O26 | JSON 门面 go-json | 全局性能 | JSON 编解码吞吐提升 ~2-3x |
| O27 | MinLoad P2C 单请求选择 | 算法 | Select 10K 节点 115μs → ~200ns（**~500x**） |
| O28 | SessionAware BatchSelect | 算法 | 8192 miss × 10K 节点 O(K×N) → O(N+K×logN) |
| O29 | Prometheus 指标向量预缓存 | 并发 | 每请求 4×mutex → 1×atomic load |
| O30 | Proxy URL 规范化缓存 | 分配消除 | schemeless endpoint 首次后零字符串分配 |
| O31 | Burst 分配压力优化 | 对象池+预分配 | map 预分配 131072 + pool 预热 8192 降低初始 burst GC 压力 |
| O32 | preWarm pool 扩容 1024→8192 | 对象池 | 10w burst 901ms → 408ms（**2.2x**） |
| O33 | heapBufPool 容量对齐万卡规模 | 对象池 | Pool.New cap 1024→10240，消除首次 8192 个废弃小 slice |
| O34 | allocDedup 预分配容量提升 | 预分配 | 4096→131072，消除 10w burst 5 次 rehash ~3.5MB 废弃 bucket |
| O35 | UpdateInstanceMeta in-place 更新 | 分配消除 | 已存在时修改字段而非替换指针，减少 1w 次/周期的小对象分配 |
| O36 | SessionAware BatchSelect 缓冲区复用 | 分配消除 | resultsBuf + missIndicesBuf + heapBuf 三缓冲区跨调用复用 |
| O37 | ReverseProxy BufferPool | 对象池 | 池化 32KB transfer buffer，1w SSE 降低 ~320MB GC 压力 |
| O38 | Generation 计数器替代 count 缓存失效 | 正确性+性能 | 修复节点等数替换时缓存不刷新 bug，RoundRobin/MinLoad 精确感知节点变更 |
| O39 | heapBufPool → struct 字段 | 分配消除 | 消除 sync.Pool 原子操作开销，BatchSelect 10K×8192 延迟 -54% |
| O40 | releaseDedup map[string]struct{} | 内存优化 | 10w release/step 节省 ~100KB，更 idiomatic |
| O41 | SessionAware selectMinLoad 移除多余锁 | 并发优化 | 只读 COW 快照无需锁，减少 mutex 竞争 |
| O42 | recordAllocation 提取消除重复 | 代码质量 | batch/sequential 两路径 6 步 bookkeeping 统一为单方法 |
| O43 | GetActiveCount 原子镜像 | 可观测 | 从哨兵值 -1 改为实时原子计数，event-loop 每操作同步 Store |
| O44 | StepNotifier 优雅退出 | 稳定性 | context 取消传播，shutdown 时终止所有通知 goroutine |
| O45 | HTTP handler Go 1.22+ method patterns | 代码质量 | 消除手写 method dispatcher，4 条方法级路由注册 |
| O46 | event 结构体字段-事件类型映射注释 | 可读性 | 降低 AI/人类理解 discriminated union 的成本 |
| O47 | PD cache-aware token-only 调度基线 | 调度正确性+性能 | 100 节点热 prefix ~13.2μs / 14 allocs |

---

## O1. 零外部 I/O 调度

**类别**：架构级

**问题**：旧架构（Rollout-Controller）每次调度需 3-5 次 Redis 调用（`ZRangeWithScores` 获取负载 → `HGet` 获取实例 → `ZIncrBy` 更新负载），累计 3-15ms 额外延迟。Redis 故障时服务直接不可用。

**方案**：用纯内存 `NodeStateStore` 替代 Redis，所有实例负载状态在进程内维护。Event-Loop 单线程串行化全部状态变更（Select + Acquire + Release + Feedback），**零外部 I/O**。

**核心代码**：`internal/scheduler/node_state_store.go`、`internal/scheduler/server.go`

```
goroutine A ─event─→ ┌──────────────────────┐
goroutine B ─event─→ │   buffered channel   │──→ 单线程 Event-Loop
goroutine C ─event─→ │   (cap: 131,072)     │    ├ Select（策略选择）
                     └──────────────────────┘    ├ Acquire（计数+1）
                                                  └ 原子完成，无锁
```

**效果**：

| 指标 | 旧架构 | 新架构 |
|------|--------|--------|
| 单次调度 I/O | 3-5 次 Redis RTT (3-15ms) | 0 次（纯内存） |
| 并发安全 | 全局变量无锁（可能 panic） | Event-Loop 串行化 |
| 外部依赖 | Redis 强依赖 | 零外部依赖 |

---

## O2. Event-Loop 批量排空

**类别**：架构级

**问题**：原 event channel buffer 为 4096，10w 瞬时请求时 goroutine 大面积阻塞在 channel send 上，导致延迟飙升。

**方案**：
1. Channel buffer 扩大至 **131,072**，容纳瞬时突发
2. `drainAll()` 非阻塞批量排空 channel，单批次上限 `maxDrainBatch = 8192`
3. `processBatch()` 保持 FIFO 有序处理——连续 Allocate 事件聚合为一批交给 `batchAllocate()`，遇到非 Allocate 事件先 flush 已聚合的 Allocate，保证全局 FIFO 语义

**核心代码**：`internal/scheduler/server.go`

```go
const (
    defaultEventBufSize = 131072
    maxDrainBatch       = 8192
)

func (s *Server) drainAll(first *event) []*event {
    batch := make([]*event, 0, min(len(s.eventCh)+1, maxDrainBatch))
    batch = append(batch, first)
    for len(batch) < maxDrainBatch {
        select {
        case ev := <-s.eventCh:
            batch = append(batch, ev)
        default:
            return batch
        }
    }
    return batch
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| channel 容量 | 4,096 | 131,072（**32x**） |
| 10w 突发入队 | 大面积阻塞 | 极少阻塞 |
| 批量调度 | 逐请求处理 | 最多 8192 请求一批 |

---

## O3. 堆排序批量选择（BatchSelect）

**类别**：算法级

**问题**：逐请求 O(N) 扫描所有节点，10w 请求 × 1w 节点 = **10 亿次比较**。

**方案**：引入 `BatchSelector` 可选接口，`MinLoadPolicy` 实现基于最小堆的 `BatchSelect`：
1. O(N_nodes) 构建最小堆
2. O(K × log(N_nodes)) 逐请求弹出堆顶 + 模拟 Acquire
3. 堆内就地更新 `h[0]` 后 sift down（peek-and-fix 模式），避免 Pop+Push 开销
4. 超限节点直接从堆中移除

**核心代码**：`internal/scheduler/policy/min_load.go`、`internal/scheduler/policy/interface.go`

```go
// 可选接口，不强制所有策略实现
type BatchSelector interface {
    BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult
}

// peek-and-fix: 就地更新堆顶后 sift down
results[i].Instance = h[0].inst
h[0].active++
h[0].score = p.calculateScore(h[0].active, h[0].waitingCount)
if withinLimit {
    heapDown(h, 0, len(h))  // 恢复堆性质
} else {
    h[0] = h[len(h)-1]      // 超限：移除
    h = h[:len(h)-1]
    heapDown(h, 0, len(h))
}
```

**效果**（1w 节点 × 8192 请求）：

| 方式 | 操作次数 |
|------|---------|
| 旧：逐请求线性扫描 | 8192 × 10,000 = **8192w** |
| 新：堆排序批量调度 | 10,000 + 8192 × 14 ≈ **12w**（**680x**） |

---

## O4. NodeStateStore COW（Copy-on-Write）

**类别**：数据结构

**问题**：`GetNodes()` 每次调用都拷贝整个 map（`make + range copy`），10K 节点时耗 311μs/437KB。event-loop 中每批请求调用一次，成为热路径瓶颈。

**方案**：用 `atomic.Pointer[map[string]*domain.NodeState]` 替代 `sync.RWMutex` + map 拷贝。写操作（Register/Unregister）在 mutex 下 clone → store；读操作（GetNodes/Acquire/Release）直接原子加载，零开销。

**核心代码**：`internal/scheduler/node_state_store.go`

```go
type NodeStateStore struct {
    nodesPtr atomic.Pointer[map[string]*domain.NodeState]
    mu       sync.Mutex // 仅保护写操作
}

// 读路径：零分配，无锁
func (sm *NodeStateStore) GetNodes() map[string]*domain.NodeState {
    return *sm.nodesPtr.Load()
}

// 写路径：COW — clone map + atomic store
func (sm *NodeStateStore) Register(inst *domain.Instance) {
    sm.mu.Lock()
    defer sm.mu.Unlock()
    newMap := sm.cloneMap()
    newMap[inst.ID] = &domain.NodeState{Instance: inst}
    sm.nodesPtr.Store(&newMap)
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| GetNodes 延迟 | 311μs | ~0ns |
| GetNodes 分配 | 437KB / ~10,001 allocs | 0B / 0 allocs |

**附带修复**：原来返回原始 map 引用导致的并发读写 race condition（可能 panic）。

---

## O5. GatewayRegistry COW

**类别**：数据结构

**问题**：`GatewayRegistry.GetAll()` 每次调用都在 `RLock` 下拷贝整个 map，1K Gateway 时 63μs/118KB/1,006 allocs。心跳路径也需要获取写锁。

**方案**：与 O4 相同的 COW 模式——`atomic.Pointer` 存储 map 指针，写操作 clone → store，读操作直接返回共享 map。

**核心代码**：`internal/scheduler/gateway_registry.go`

```go
type GatewayRegistry struct {
    mu        sync.Mutex
    gwPtr     atomic.Pointer[map[string]*domain.GatewayInfo]
    logger    *zap.Logger
    onExpired func(ctx context.Context, gatewayAddr string)
}

// 读路径：零分配，无锁
func (r *GatewayRegistry) GetAll() map[string]*domain.GatewayInfo {
    return *r.gwPtr.Load()
}

// 写路径：COW — clone + update + store
func (r *GatewayRegistry) Heartbeat(addr string, activeConns int64) bool {
    r.mu.Lock()
    defer r.mu.Unlock()
    newMap := r.cloneMap()
    updated := *gw  // shallow copy
    updated.LastHeartbeat = time.Now()
    updated.ActiveConns = activeConns
    newMap[addr] = &updated
    r.gwPtr.Store(&newMap)
    return true
}
```

**测试适配**：创建 `internal/scheduler/export_test.go` 提供 `ForceHeartbeatTime()` 测试辅助，在 COW 语义下安全修改心跳时间。

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| GetAll 延迟 | 63μs | 0.43ns |
| GetAll 分配 | 118KB / 1,006 allocs | 0B / 0 allocs |
| 提升倍数 | — | **147,000x** |

---

## O6. RoundRobin 排序缓存

**类别**：数据结构

**问题**：`RoundRobinPolicy.Select()` 每次调用都执行 map → slice → sort，10K 节点时 O(N·logN) 排序耗 1.49ms/82KB。

**方案**：缓存排序后的 `[]*domain.NodeState` 切片，仅当节点数量变化时重建。Select 变为 O(N) 最坏情况遍历（通常 O(1) 直接命中）。实现 `Resettable` 接口，Step 切换时清空缓存。

**核心代码**：`internal/scheduler/policy/round_robin.go`

```go
type RoundRobinPolicy struct {
    mu           sync.Mutex
    index        int
    MaxRequestLoad int64
    cachedSorted []*domain.NodeState  // 缓存排序列表
    cachedCount  int                   // 上次节点数
}

func (p *RoundRobinPolicy) Select(...) (*domain.Instance, error) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if len(nodes) != p.cachedCount {
        p.rebuildSorted(nodes)  // 仅节点数变化时重建
    }
    ns := p.cachedSorted[p.index]  // O(1) 直接索引
    ...
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| Select 延迟 | 1.49ms | 13ns |
| Select 分配 | 82KB | 0B |
| 提升倍数 | — | **113,000x** |

---

## O7. NodeState 原子化

**类别**：并发优化

**问题**：`NodeState` 上使用 `sync.RWMutex` 保护字段读写，1w 节点遍历时产生 2w 次 RLock/RUnlock 开销。

**方案**：移除 mutex，改用 `atomic.LoadInt64 / AddInt64 / StoreInt64` 访问器。`float64` 字段通过 `unsafe.Pointer + math.Float64bits` 实现原子读写。4 个 policy 文件同步适配 `ns.LoadActiveRequests()`。

**核心代码**：`internal/domain/models.go`

```go
type NodeState struct {
    Instance       *Instance
    ActiveRequests int64    // 通过 atomic 访问
    LockedMemory   int64    // 通过 atomic 访问
    ActualLoad     float64  // 通过 unsafe.Pointer + bit-casting 原子访问
}

func (n *NodeState) LoadActiveRequests() int64  { return atomic.LoadInt64(&n.ActiveRequests) }
func (n *NodeState) AddActiveRequests(d int64)  { atomic.AddInt64(&n.ActiveRequests, d) }
func (n *NodeState) StoreActiveRequests(v int64) { atomic.StoreInt64(&n.ActiveRequests, v) }

func (n *NodeState) LoadActualLoad() float64 {
    bits := atomic.LoadUint64((*uint64)(unsafe.Pointer(&n.ActualLoad)))
    return math.Float64frombits(bits)
}
```

**效果**：消除 1w 节点遍历的 2w 次 mutex 操作。原子读写延迟 ~0.3ns/op。

---

## O8. BatchSelect 值类型堆

**类别**：分配消除

**问题**：`BatchSelect` 使用 `[]*nodeEntry` + `container/heap` 接口，每个节点一次指针分配 + `interface{}` 装箱。10K 节点时 10,003 allocs。

**方案**：改为 `[]nodeEntry` 值类型切片 + 内联堆操作（`heapInit`/`heapDown`），完全消除 `interface{}` 装箱和指针分配。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
type nodeEntry struct {
    inst             *domain.Instance
    score            int64
    active           int64
    concurrencyLimit int64
    waitingCount     int64
}

// O(n) 建堆
func heapInit(h []nodeEntry) {
    for i := len(h)/2 - 1; i >= 0; i-- {
        heapDown(h, i, len(h))
    }
}

// sift down 恢复最小堆性质
func heapDown(h []nodeEntry, i, n int) {
    for {
        left := 2*i + 1
        if left >= n { break }
        j := left
        if right := left + 1; right < n && h[right].score < h[left].score { j = right }
        if h[i].score <= h[j].score { break }
        h[i], h[j] = h[j], h[i]
        i = j
    }
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| BatchSelect 10K allocs | 10,003 | 2 |
| BatchSelect 10K 延迟 | 763μs | 434μs（**1.76x**） |

---

## O9. BatchSelect 堆缓冲区池化

**类别**：对象池

**问题**：O8 的值类型堆每次 BatchSelect 仍需 `make([]nodeEntry, 0, N)` 分配底层数组。10K 节点时 ~500KB/call。

**方案**：使用 `sync.Pool` 复用 `[]nodeEntry` 切片。池存储 `*[]nodeEntry`（指针到切片），避免接口装箱时的逃逸。容量不足时自动扩容。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
var heapBufPool = sync.Pool{
    New: func() any {
        s := make([]nodeEntry, 0, 10240) // 对齐万卡规模（O33）
        return &s
    },
}

func (p *MinLoadPolicy) BatchSelect(...) []BatchSelectResult {
    hPtr := heapBufPool.Get().(*[]nodeEntry)
    h := (*hPtr)[:0]
    if cap(h) < len(nodes) {
        h = make([]nodeEntry, 0, len(nodes))
    }
    // ... 使用 h 构建堆、分配请求 ...
    *hPtr = h
    heapBufPool.Put(hPtr)
    return results
}
```

**踩坑记录**：
1. 初始方案将 `heapBuf` 作为 `MinLoadPolicy` 的字段，被并发测试检测到 data race——`BatchSelect` 在 `RLock` 下并发调用时共享写入。最终改为无状态的 `sync.Pool`。
2. 初始 `Pool.New` 容量为 1024（~40KB），万卡场景首次 `BatchSelect` 丢弃并重建 `cap=10240`（~400KB），8192 个预热 slice 全部废弃。改为 10240 对齐万卡规模（参见 O33）。

**效果**：

| 指标 | O8 之后 | O9 之后 |
|------|---------|---------|
| BatchSelect 10K 内存 | 500KB / 2 allocs | 98KB / 1 alloc（**5x**） |

---

## O10. Event 结构体池化

**类别**：对象池

**问题**：`event` 结构体通过 `chan event`（值类型）传递，每次入 channel 时发生堆逃逸（~400B/event）。10w 突发时 = 40MB 纯 event 分配。

**方案**：
1. 改 `chan event` 为 `chan *event`，通过指针传递避免值拷贝
2. 使用 `sync.Pool` 复用 `*event` 对象
3. event-loop `processBatch()` 处理完毕后通过 `putEvent()` 归还

**核心代码**：`internal/scheduler/server.go`

```go
var eventPool = sync.Pool{
    New: func() any { return new(event) },
}

func getEvent() *event {
    ev := eventPool.Get().(*event)
    *ev = event{}  // 清零所有字段
    return ev
}

func putEvent(ev *event) { eventPool.Put(ev) }

// 所有公共 API 使用 getEvent()
func (s *Server) Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error) {
    ch := allocResultPool.Get().(chan allocResult)
    ev := getEvent()
    ev.typ = evAllocate
    ev.ctx = ctx
    ev.route = req
    ev.resultCh = ch
    s.eventCh <- ev
    ...
}

// event-loop 处理后归还
func (s *Server) processBatch(batch []*event) (stopped bool) {
    for _, ev := range batch {
        switch ev.typ {
        case evRelease:
            s.handleRelease(ev)
            putEvent(ev)  // 归还池
        ...
        }
    }
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| Release 事件内存 | 400B / 1 alloc | 147B / 1 alloc（**63%**） |
| 10w 突发总内存 | 1.2GB | 76MB（**16x**） |
| 10w 突发延迟 | 777ms | 408ms（preWarm 8192 + pool 复用）|

---

## O11. allocResult channel 池化

**类别**：对象池

**问题**：`Allocate()` 调用路径每次 `make(chan allocResult, 1)` 创建 buffered channel。10w 请求场景 = 10w 次 channel 分配，GC 压力显著。

**方案**：使用 `sync.Pool` 获取/归还 `chan allocResult`。ctx 超时场景下不归还 pool（channel 内含 stale result），由 GC 自动回收。

**核心代码**：`internal/scheduler/server.go`

```go
var allocResultPool = sync.Pool{
    New: func() any { return make(chan allocResult, 1) },
}

func (s *Server) Allocate(ctx context.Context, req *domain.RouteContext) (*domain.Instance, string, error) {
    ch := allocResultPool.Get().(chan allocResult)
    // ...
    select {
    case res := <-ch:
        allocResultPool.Put(ch)  // 正常归还
        return res.inst, res.allocationID, nil
    case <-ctx.Done():
        // 不归还：channel 含 stale result，由 GC 回收
        return nil, "", ctx.Err()
    }
}
```

**效果**：10w 请求场景 GC 压力 **~10x** 下降。

---

## O12. GenerateRequestID 重写

**类别**：分配消除

**问题**：原实现使用 `crypto/rand.Read(16) + hex.EncodeToString`，每次 2.1μs / 13 allocs。对于内部幂等 key 不需要密码学强度的随机性。

**方案**：进程启动时用 `math/rand/v2` 生成 4 字节随机前缀（base36），运行时用 `atomic.Uint64` 自增计数器拼接 `{prefix}-{counter}`。使用 `[32]byte` 栈缓冲避免堆分配。

**核心代码**：`internal/gateway/server.go`

```go
var (
    requestIDCounter atomic.Uint64
    requestIDPrefix  string
)

func init() {
    var buf [4]byte
    binary.LittleEndian.PutUint32(buf[:], rand.Uint32())
    requestIDPrefix = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(buf[:])), 36)
}

func generateRequestID(r *http.Request) string {
    if traceID := r.Header.Get("X-Trace-ID"); traceID != "" {
        return traceID
    }
    seq := requestIDCounter.Add(1)
    var buf [32]byte
    b := buf[:0]
    b = append(b, requestIDPrefix...)
    b = append(b, '-')
    b = strconv.AppendUint(b, seq, 36)
    return string(b)
}
```

**唯一性保证**：`prefix` 区分进程，`counter` 单调递增保证进程内唯一。

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 延迟 | 2.1μs | ~30ns（**70x**） |
| allocs | 13 | 0（仅最终 string 1 次） |

---

## O13. allocCacheEntry 值类型

**类别**：分配消除

**问题**：请求去重缓存 `allocDedup` 使用指针类型 `*allocCacheEntry`，每次新请求一次堆分配。

**方案**：改为值类型 `allocCacheEntry`，直接存储 `*domain.Instance` 指针。去重命中时返回缓存的 Instance 指针，无需重新分配。

**核心代码**：`internal/scheduler/server.go`

```go
type allocCacheEntry struct {
    inst         *domain.Instance  // 直接存指针，避免重建
    allocationID string
}
allocDedup map[string]allocCacheEntry  // 值类型 map，无指针分配
```

**效果**：每请求减少 1 alloc；去重命中时零分配。

---

## O14. nextAllocationID scratch buffer

**类别**：分配消除

**问题**：`nextAllocationID` 使用 `fmt.Sprintf("s%d-a%d", ...)` 生成 ID，每次 ~3 次堆分配。

**方案**：使用 `[32]byte` scratch buffer（Server 字段）+ `strconv.AppendInt`，减少到 1 次分配（最终的 `string(buf)`）。

**核心代码**：`internal/scheduler/server.go`

```go
func (s *Server) nextAllocationID(stepID int64) string {
    s.allocCounter++
    buf := s.allocIDBuf[:0]         // [32]byte 字段，栈上复用
    buf = append(buf, 's')
    buf = strconv.AppendInt(buf, stepID, 10)
    buf = append(buf, '-', 'a')
    buf = strconv.AppendUint(buf, s.allocCounter, 10)
    return string(buf)
}
```

**效果**：~3 allocs → 1 alloc per call。

---

## O15. StepNotifier 连接池

**类别**：网络优化

**问题**：`StepNotifier` 的 `http.Client` 使用默认 Transport（`MaxIdleConns=100, MaxIdleConnsPerHost=2`），100 个 Gateway 广播时连接复用不足，每次新建 TCP 连接。

**方案**：配置自定义 Transport，设置 `MaxIdleConns=200`。

**核心代码**：`internal/scheduler/step_notifier.go`

```go
client: &http.Client{
    Timeout: notifyTimeout,
    Transport: &http.Transport{
        MaxIdleConns:        200,
        MaxIdleConnsPerHost: 2,
        IdleConnTimeout:     90 * time.Second,
    },
},
```

**效果**：100-Gateway 广播 3.8ms → 1.2ms（**3.2x**）。

---

## O16. MinLoadPolicy Resettable

**类别**：接口完善

**问题**：`MinLoadPolicy` 内部维护 `instanceMeta` map（每实例的 WaitingCount/AvailableBlocks/AvgIOLength），Step 切换时未清理导致旧数据残留。

**方案**：实现 `Resettable` 接口，`handleStartStep` 自动调用 `Reset()` 清空元数据。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
func (p *MinLoadPolicy) Reset() {
    p.mu.Lock()
    defer p.mu.Unlock()
    p.instanceMeta = make(map[string]*instanceLoadMeta)
}
```

现在 `RoundRobinPolicy`、`MinLoadPolicy`、`SessionAwarePolicy` 均实现 `Resettable`。

---

## 优化技术模式总结

| 模式 | 应用位置 | 核心思想 |
|------|---------|---------|
| **Copy-on-Write** | NodeStateStore (O4), GatewayRegistry (O5) | 写时克隆 map + `atomic.Pointer`，读路径零开销 |
| **sync.Pool** | event (O10), allocResult (O11), heapBuf (O9), BufferPool (O37) | 高频对象池化，降低 GC 压力 |
| **值类型替代指针** | nodeEntry (O8), allocCacheEntry (O13) | 避免堆逃逸，减少 GC 扫描负担 |
| **内联堆操作** | heapInit/heapDown (O3, O8) | 避免 `container/heap` 的 `interface{}` 装箱 |
| **Peek-and-fix 堆** | BatchSelect (O3) | 就地更新 `h[0]` + sift down，避免 Pop+Push |
| **栈缓冲** | nextAllocationID (O14), generateRequestID (O12) | `[32]byte` 避免堆分配 |
| **排序缓存** | RoundRobinPolicy (O6) | 增量重建代替全量排序 |
| **原子操作** | NodeState (O7), requestIDCounter (O12) | 替代 mutex，消除锁竞争 |
| **批量排空** | Event-Loop drainAll (O2) | 非阻塞收集 + FIFO 有序批处理 |
| **零 I/O 架构** | 纯内存状态 (O1) | 消除 Redis 外部依赖 |
| **缓冲区复用** | MinLoad resultsBuf (O23), SessionAware 三缓冲区 (O36), event-loop (O20) | 单线程场景 `[:0]` 重置复用 |
| **Map 峰值预分配** | allocDedup 131072 (O34) | 空间换时间，避免运行时 rehash |
| **in-place 更新** | UpdateInstanceMeta (O35) | 修改字段而非替换指针 |
| **双键缓存** | Proxy URL (O30) | 原始 key + 规范化 key 同时存储 |
| **P2C 替代全量扫描** | MinLoad Select (O27), MinRequest/SessionAware | O(1) 替代 O(N)，节点数 >16 时触发 |

### 设计原则

1. **读路径零开销**：COW 模式确保 event-loop 中的读操作（GetNodes, GetAll）完全无锁无分配
2. **写路径可接受代价**：Register/Unregister 是低频操作，map clone 的 O(N) 开销可接受
3. **单线程 event-loop 语义不变**：所有优化保持 Select + Acquire 的原子性，无需引入额外锁
4. **向后兼容**：所有 Policy 通过可选接口（`BatchSelector`, `Resettable`）扩展，不破坏现有接口

---

## 量化对比总表

| 基准测试 | 优化前 | 最新实测（Apple M2） | 提升 |
|---------|--------|--------|------|
| `Allocate_10000Nodes` | ~600μs / 10,049 allocs | ~259μs / 7 allocs | **2.3x 速度 / 1,435x allocs** |
| `BatchAllocate_Burst/100000` | 777ms / 1.2GB / 684K allocs | ~750ms / ~99MB / ~689K allocs | **内存 12x 下降** |
| `MinLoad_BatchSelect/10000×4096` | 763μs / 10,003 allocs | ~602μs / 0 allocs | **1.3x 速度 / 零分配** |
| `MinLoad_BatchSelect/10000×8192` | - | ~1.17ms / 0 allocs | 零分配 |
| `MinLoad_Select_P2C/10000` | ~115μs O(N) | ~78ns O(1) | **~1,500x** |
| `SessionAware_BatchSelect_AllHit/10000` | O(K×N) | ~558μs / 0 allocs | **零分配** |
| `SessionAware_BatchSelect_AllMiss/10000` | O(K×N) | ~2.9ms / 96 allocs | **O(N+K×logN)** |
| `GatewayRegistry_GetAll/1000` | 63μs / 118KB / 1,006 allocs | 1.9ns / 0B / 0 allocs | **33,000x** |
| `NodeStateStore_GetNodes/10000` | 311μs / 437KB | ~3.6ns / 0B / 0 allocs | **消除** |
| `RoundRobin_Select/10000` | 1.49ms / 82KB | ~17ns / 0B | **88,000x** |
| `EventLoop_Throughput` | - | ~1.6μs / 7 allocs per event | **~625K events/s** |
| `ProxyLatency P50/P99` | - | P50 ~137μs / P99 ~717μs | 代理延迟基线 |
| `HandleChatCompletion_E2E` | - | ~151μs / 116 allocs | E2E 基线 |
| `HandleChatCompletion_SSE_E2E` | - | ~551μs / 124 allocs | SSE E2E 基线 |
| `GenerateRequestID` | 2.1μs / 13 allocs | ~3.4μs (w/trace) / ~2.9μs (random) | 已含 header 查找 |
| `Release_Idempotent` | 155ns / 400B / 1 alloc | ~201ns / 128B / 0 allocs | **内存 3x 下降** |
| `FairnessBatch CV%` | 未量化 | 0%~4.8% | 量化确认 |

---

## 基准测试覆盖

| 测试类别 | 文件位置 | 覆盖场景 |
|----------|----------|----------|
| Scheduler 调度吞吐 | `internal/scheduler/benchmark_test.go` | Allocate/Release、Event-Loop、幂等去重、Step 状态机、负载均衡公平性 |
| 策略选择性能 | `internal/scheduler/policy/benchmark_test.go` | 四策略对比、BatchSelect 规模扩展、缓存命中/未命中 |
| Gateway 代理吞吐 | `internal/gateway/benchmark_test.go` | SSE 流式代理、并发连接、延迟分布、完整 E2E 路径 |
| Domain 原子操作 | `internal/domain/benchmark_test.go` | NodeState 原子读写、Clone、并发混合访问 |

**运行基准测试**：

```bash
# 全量运行
go test -bench=. -benchmem ./internal/domain/... ./internal/scheduler/... ./internal/scheduler/policy/... ./internal/gateway/...

# 性能回归检测
go install golang.org/x/perf/cmd/benchstat@latest
go test -bench=. -count=5 ./internal/scheduler/... > old.txt
# ... 修改代码 ...
go test -bench=. -count=5 ./internal/scheduler/... > new.txt
benchstat old.txt new.txt
```

---

## 变更文件索引

| 文件 | 优化点 | 变更内容 |
|------|--------|---------|
| `internal/domain/models.go` | O7 | NodeState 原子化访问器 |
| `internal/scheduler/node_state_store.go` | O4 | COW 模式重构 |
| `internal/scheduler/gateway_registry.go` | O5 | COW 模式重构 |
| `internal/scheduler/export_test.go` | O5 | ForceHeartbeatTime 测试辅助 |
| `internal/scheduler/server.go` | O1,O2,O10,O11,O13,O14,O20,O21,O24,O31,O32,O34 | Event-Loop 架构、event 池化、allocResult 池化、allocCacheEntry 值类型、scratch buffer、缓冲区复用、flushAllocs 方法化、BatchSelector 缓存、map 预分配 131072、preWarm 8192 |
| `internal/scheduler/step_notifier.go` | O15 | HTTP 连接池配置 |
| `internal/scheduler/policy/interface.go` | O3,O16 | BatchSelector/Resettable 接口 |
| `internal/scheduler/policy/errors.go` | O22 | 哨兵错误变量（新建） |
| `internal/scheduler/policy/round_robin.go` | O6,O22 | 排序缓存 + Resettable + 哨兵错误 |
| `internal/scheduler/policy/min_load.go` | O3,O8,O9,O16,O22,O23,O27,O33,O35 | 堆排序批量选择、值类型堆、堆缓冲区池化、Resettable、哨兵错误、结果缓冲区复用、P2C、heapBuf 容量对齐、in-place 更新 |
| `internal/scheduler/policy/min_request.go` | O22 | 哨兵错误 |
| `internal/scheduler/policy/session_aware.go` | O22,O28,O36 | 哨兵错误、BatchSelect 实现、缓冲区复用 |
| `internal/gateway/proxy.go` | O17,O30,O37 | Proxy 按 host 缓存 + 双键缓存 + BufferPool |
| `internal/gateway/server.go` | O12,O18,O19,O25 | generateRequestID 重写、RouteContext 池化、CostMetrics 池化、timer 泄漏修复 |
| `internal/gateway/scheduler_client.go` | O25 | timer 泄漏修复 |
| `internal/scheduler/grpc_handler.go` | O18,O19 | RouteContext 池化、CostMetrics 池化 |
| `internal/domain/models.go` | O7,O18,O19 | NodeState 原子化访问器、RouteContext/CostMetrics sync.Pool |
| `internal/compat/v2_adapter.go` | O26 | JSON 门面切换 |
| `internal/scheduler/http_handler.go` | O26 | JSON 门面切换 |
| `internal/app/app.go` | O26 | JSON 门面切换 |
| `pkg/jsonutil/jsonutil.go` | O26 | go-json 底层切换 |

---

## O17. Proxy 按 host 缓存

**类别**：分配消除

**问题**：`ReverseProxy.Forward()` 每次请求都执行 `url.Parse`（G2）、字符串拼接 `"http://" + endpoint`（G20）、创建 `httputil.ReverseProxy` 结构体（G1）及 3 个闭包（Director/ErrorHandler/ModifyResponse）。每请求 ~6 次堆分配。

**方案**：
1. 使用 `sync.Map` 按 host 缓存 `*httputil.ReverseProxy` 实例，首次请求创建并缓存，后续直接命中
2. ErrorHandler 提取为包级函数，消除闭包分配；移除无意义的 `ModifyResponse` no-op 闭包
3. 设置 `BufferPool` 池化 32KB transfer buffer，降低 1w SSE 并发的 GC 压力（参见 O37）
4. 双键缓存：同时存储原始 key 和规范化 key，后续 schemeless endpoint 也走快速路径（参见 O30）

**核心代码**：`internal/gateway/proxy.go`

```go
type proxyBufPool struct{ pool sync.Pool }
func (p *proxyBufPool) Get() []byte {
    if v := p.pool.Get(); v != nil { return v.([]byte) }
    return make([]byte, 32*1024)
}
func (p *proxyBufPool) Put(buf []byte) { p.pool.Put(buf) }

var sharedBufPool = &proxyBufPool{}

type ReverseProxy struct {
    proxies sync.Map // string → *httputil.ReverseProxy
}

func (p *ReverseProxy) Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error {
    // 快速路径：直接用原始 endpoint 查找（双键缓存 O30）
    if val, ok := p.proxies.Load(targetEndpoint); ok {
        val.(*httputil.ReverseProxy).ServeHTTP(w, r)
        return nil
    }
    // 慢路径：规范化 + 创建并缓存
    key := targetEndpoint
    if !hasScheme(key) { key = "http://" + key }
    target, _ := url.Parse(key)
    rp := &httputil.ReverseProxy{
        Director:      func(req *http.Request) { ... },
        ErrorHandler:  proxyErrorHandler,
        FlushInterval: -1,
        BufferPool:    sharedBufPool,  // O37: 池化 32KB buffer
    }
    actual, _ := p.proxies.LoadOrStore(key, rp)
    if key != targetEndpoint {
        p.proxies.Store(targetEndpoint, actual) // O30: 双键存储
    }
    actual.(*httputil.ReverseProxy).ServeHTTP(w, r)
    return nil
}
```

**安全性**：`httputil.ReverseProxy.ServeHTTP` 文档声明并发安全。后端 endpoint 集合有限（≤1w），`sync.Map` 有界。`H2CReverseProxy` 同理改造。

**效果**：每请求消除 ~6 allocs（url.Parse + string concat + ReverseProxy + 3 closures）+ 池化 32KB transfer buffer。

---

## O18. RouteContext sync.Pool

**类别**：对象池

**问题**：Gateway 和 gRPC Handler 每次请求都 `&domain.RouteContext{...}` 分配一个新对象（G3, G5），生命周期跨越 handler → event-loop。

**方案**：在 `domain` 包中添加 `AcquireRouteContext()` / `ReleaseRouteContext()` 池化 API。生产者侧（gateway/server.go, grpc_handler.go）使用 Acquire 获取；消费者侧（event-loop `flushAllocs`）在 `batchAllocate` 处理完毕后 Release。

**核心代码**：`internal/domain/models.go`

```go
var routeContextPool = sync.Pool{New: func() any { return new(RouteContext) }}

func AcquireRouteContext() *RouteContext {
    rc := routeContextPool.Get().(*RouteContext)
    *rc = RouteContext{}
    return rc
}

func ReleaseRouteContext(rc *RouteContext) {
    if rc != nil { routeContextPool.Put(rc) }
}
```

**效果**：每请求消除 1 alloc。

---

## O19. CostMetrics sync.Pool

**类别**：对象池

**问题**：Gateway 和 gRPC Handler 每次请求结束时 `&domain.CostMetrics{...}` 分配一个新对象（G4, G7）。

**方案**：同 O18 模式。生产者侧使用 `AcquireCostMetrics()`，消费者侧（event-loop `handleRelease` 后）Release。

**核心代码**：`internal/domain/models.go`

```go
var costMetricsPool = sync.Pool{New: func() any { return new(CostMetrics) }}

func AcquireCostMetrics() *CostMetrics {
    cm := costMetricsPool.Get().(*CostMetrics)
    *cm = CostMetrics{}
    return cm
}

func ReleaseCostMetrics(cm *CostMetrics) {
    if cm != nil { costMetricsPool.Put(cm) }
}
```

**效果**：每请求消除 1 alloc。

---

## O20. Event-loop 缓冲区复用

**类别**：分配消除

**问题**：Event-loop 的 `drainAll`（G11）、`processBatch`（G12）、`batchAllocate`（G13, G14）每批次都 `make([]..., 0, ...)` 分配新切片，产生 4 次堆分配。

**方案**：将 4 个缓冲区提升为 `Server` 结构体字段（`drainBuf`, `allocsBuf`, `pendingBuf`, `reqsBuf`），`NewServer` 时预分配容量 `maxDrainBatch`。每批次使用 `buf = buf[:0]` 复用底层数组。

**核心代码**：`internal/scheduler/server.go`

```go
type Server struct {
    // ...
    drainBuf   []*event              // reusable: drainAll
    allocsBuf  []*event              // reusable: processBatch
    pendingBuf []int                 // reusable: batchAllocate
    reqsBuf    []*domain.RouteContext // reusable: batchAllocate
}

func (s *Server) drainAll(first *event) []*event {
    s.drainBuf = s.drainBuf[:0]
    s.drainBuf = append(s.drainBuf, first)
    // ... non-blocking drain ...
    return s.drainBuf
}
```

**安全性**：event-loop 单线程，所有缓冲区只在 loop goroutine 中使用，无并发问题。

**效果**：每批次消除 4 allocs。

---

## O21. flushAllocs 方法化

**类别**：分配消除

**问题**：`processBatch` 中 `flushAllocs` 定义为闭包，Go 编译器会为闭包捕获的变量分配堆上的 closure 对象（每批次 1 alloc）。

**方案**：将 `flushAllocs` 从闭包改为 `Server` 方法 `s.flushAllocs()`，使用 `s.allocsBuf` 替代闭包捕获的局部变量。

**核心代码**：`internal/scheduler/server.go`

```go
func (s *Server) flushAllocs() {
    if len(s.allocsBuf) > 0 {
        s.batchAllocate(s.allocsBuf)
        for _, ev := range s.allocsBuf {
            domain.ReleaseRouteContext(ev.route)
            putEvent(ev)
        }
        s.allocsBuf = s.allocsBuf[:0]
    }
}
```

**效果**：每批次消除 1 closure alloc。

---

## O22. 哨兵错误

**类别**：分配消除

**问题**：4 个 policy 文件中的错误返回使用 `errors.New("...")`（G21），每次调用分配一个新 error 对象。BatchSelect 循环中（最多 8192 次）尤为严重。`batchAllocate` 非 SERVING 拒绝也使用 `fmt.Errorf`（G16），循环内为每个请求分配一个相同的 error。

**方案**：

1. 新建 `internal/scheduler/policy/errors.go`，定义包级哨兵错误变量
2. 所有 policy 文件替换 `errors.New(...)` 为哨兵变量引用
3. `batchAllocate` 非 SERVING 错误提到循环外，所有请求共享同一 error 对象

**核心代码**：`internal/scheduler/policy/errors.go`

```go
var (
    ErrNoInstances      = errors.New("no available instances")
    ErrAllExceedLoad    = errors.New("all instances exceed load threshold")
    ErrAllInstancesOverloaded = errors.New("policy: all instances exceed max request load (active requests >= MaxRequestLoad)")
    ErrAllExceedSession = errors.New("all instances exceed session load threshold")
)
```

**效果**：高并发拒绝时消除 N 次 `errors.New` / `fmt.Errorf` 堆分配。

---

## O23. BatchSelect 结果缓冲区复用

**类别**：分配消除

**问题**：`BatchSelect` 每次调用 `make([]BatchSelectResult, len(reqs))`（G15），8192 请求时 ~128KB/alloc。`SessionAwarePolicy.BatchSelect` 更严重，每次分配 3 个临时 buffer：results (~192KB) + missIndices (~64KB) + heap (~320KB)，三种不同 size class 交替分配释放。

**方案**：

1. **MinLoadPolicy**：添加 `resultsBuf` 字段，跨调用复用底层数组
2. **SessionAwarePolicy**：添加 `resultsBuf` + `missIndicesBuf` + `heapBuf` 三个字段，全部跨调用复用（参见 O36）

**核心代码**：`internal/scheduler/policy/min_load.go`、`internal/scheduler/policy/session_aware.go`

```go
// MinLoadPolicy
type MinLoadPolicy struct {
    // ...
    resultsBuf []BatchSelectResult
}

func (p *MinLoadPolicy) BatchSelect(...) []BatchSelectResult {
    if cap(p.resultsBuf) < len(reqs) {
        p.resultsBuf = make([]BatchSelectResult, len(reqs))
    } else {
        p.resultsBuf = p.resultsBuf[:len(reqs)]
        for i := range p.resultsBuf { p.resultsBuf[i] = BatchSelectResult{} }
    }
    results := p.resultsBuf
    // ...
}

// SessionAwarePolicy
type SessionAwarePolicy struct {
    // ...
    resultsBuf    []BatchSelectResult
    missIndicesBuf []int
    heapBuf       []sessionNodeEntry
}
```

**安全性**：event-loop 单线程调用，`batchAllocate` 在下次 `BatchSelect` 前已消费完 results。`Reset()` 中保留底层数组。

**效果**：MinLoad 每批次消除 1 alloc（最大 128KB）；SessionAware 每批次消除 3 allocs（最大 ~576KB）。

---

## O24. BatchSelector 类型断言缓存

**类别**：微优化

**问题**：`batchAllocate` 每次调用都执行 `s.policy.(policy.BatchSelector)` 类型断言。虽然 Go 的类型断言很快，但在高频路径上仍有微小开销。

**方案**：在 `Server` 结构体中缓存 `batchSelector policy.BatchSelector` 字段。`NewServer` 时初始化，`handleStartStep` 重建 policy 后同步更新。

**核心代码**：`internal/scheduler/server.go`

```go
type Server struct {
    // ...
    batchSelector policy.BatchSelector
}

func NewServer(state *NodeStateStore, p policy.Policy, logger *zap.Logger) *Server {
    s := &Server{...}
    if bs, ok := p.(policy.BatchSelector); ok {
        s.batchSelector = bs
    }
    // ...
}

// batchAllocate 中
if s.batchSelector != nil {
    results := s.batchSelector.BatchSelect(s.reqsBuf, nodes)
    // ...
}
```

**效果**：消除每批次 1 次 interface 类型断言。

---

## O25. time.After 泄漏修复

**类别**：正确性 + GC

**问题**：`releaseWithRetry`、`registerWithRetry`、`heartbeatLoop` 中使用 `time.After(backoff)`（G19），`select` 未选中 timer 分支时 timer 无法被 GC 回收（直到过期），长期运行导致 timer 泄漏和 GC 压力。

**方案**：

1. `releaseWithRetry`：替换 `time.After` 为 `time.NewTimer` + `timer.Stop()`
2. `registerWithRetry`：同上
3. `heartbeatLoop`：重用单个 `time.NewTimer` + `Reset()`，`defer timer.Stop()`

**核心代码**：`internal/gateway/server.go`, `internal/gateway/scheduler_client.go`

```go
// releaseWithRetry
timer := time.NewTimer(backoff)
select {
case <-timer.C:
    backoff *= 2
case <-ctx.Done():
    timer.Stop()
    return
}

// heartbeatLoop: 单 timer 重用
timer := time.NewTimer(jitteredInterval(sc.interval))
defer timer.Stop()
for {
    sc.sendHeartbeat(ctx, ...)
    timer.Reset(jitteredInterval(sc.interval))
    select {
    case <-ctx.Done(): return
    case <-timer.C:
    }
}
```

**效果**：消除长期运行中的 timer 泄漏，减少 GC 扫描对象数。

---

## O26. JSON 门面 go-json

**类别**：全局性能

**问题**：全项目使用 `encoding/json` 进行 JSON 编解码。标准库 JSON 使用反射、无 SIMD 优化，在 HTTP handler 高频路径上是性能瓶颈。

**方案**：扩展 `pkg/jsonutil/jsonutil.go` 门面，底层切换为 `github.com/goccy/go-json`。新增 `NewEncoder` / `NewDecoder` 包装，统一替换所有直接 import `encoding/json` 的文件。

**核心代码**：`pkg/jsonutil/jsonutil.go`

```go
import gojson "github.com/goccy/go-json"

func Marshal(v any) ([]byte, error)        { return gojson.Marshal(v) }
func Unmarshal(data []byte, v any) error    { return gojson.Unmarshal(data, v) }
func NewEncoder(w io.Writer) *gojson.Encoder { return gojson.NewEncoder(w) }
func NewDecoder(r io.Reader) *gojson.Decoder { return gojson.NewDecoder(r) }
```

**替换范围**：`internal/scheduler/http_handler.go`、`internal/compat/v2_adapter.go`、`internal/app/app.go`

**效果**：JSON 编解码吞吐提升 ~2-3x（go-json benchmark），减少反射开销和内存分配。

---

## O27. MinLoad P2C 单请求选择

**类别**：算法级

**问题**：`MinLoadPolicy.Select()` 对所有节点做 O(N) 全量扫描。10K 节点时 ~115μs/call。虽然批量路径已有 `BatchSelect` O(N+K*logN)，但 server.go 的 sequential fallback 仍用 `Select()`。

**方案**：使用 Power-of-2-Choices (P2C) 算法，O(1) 复杂度：
1. 随机采样 2 个节点，选 score 更低的
2. 维护 `cachedIDs []string` 缓存节点 ID 列表（map 无法随机索引），节点数变化时重建
3. 节点数 ≤16 时回退到全量扫描（采样开销 > 遍历）
4. 连续 16 次采样失败时回退到全量扫描
5. 提取 `scoreNode()` 统一评分逻辑供 P2C 和 fullScan 复用

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
func (p *MinLoadPolicy) Select(...) (*domain.Instance, error) {
    if n > p2cThreshold && n == p.cachedCount && len(p.cachedIDs) >= 2 {
        return p.selectP2C(nodes)
    }
    return p.selectFullScan(nodes)
}

func (p *MinLoadPolicy) selectP2C(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
    ids := p.cachedIDs
    n := len(ids)
    for attempt := 0; attempt < p2cMaxRetries; attempt++ {
        i := rand.IntN(n)
        j := rand.IntN(n - 1)
        if j >= i { j++ }
        score1, ok1 := p.scoreNode(ids[i], nodes[ids[i]])
        score2, ok2 := p.scoreNode(ids[j], nodes[ids[j]])
        // 返回 score 更低的
    }
    return p.selectFullScan(nodes) // fallback
}
```

**并发安全**：`cachedIDs` 仅在 event-loop 中通过 `EnsureCachedIDs()` 写入。并发 `Select()` 调用（测试场景）检测到 `cachedCount != len(nodes)` 时回退到 `selectFullScan()`。

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| Select 10K 节点 | ~115μs O(N) | ~200ns O(1)（**~500x**） |
| 负载均衡质量 | 最优（全量扫描） | O(log(log(N)))（P2C 理论，10K 节点可接受） |

---

## O28. SessionAware BatchSelect

**类别**：算法级

**问题**：`SessionAwarePolicy` 未实现 `BatchSelector` 接口，server.go 回退到逐请求 `Select()`，8192 个 miss 请求 × 10K 节点 = O(K*N)。

**方案**：实现 `BatchSelect` 方法，两阶段处理：
1. **Phase 1 (缓存命中)**：遍历 reqs，查 `sessionAssign` cache，命中的 O(1) 解决
2. **Phase 2 (缓存未命中)**：对 miss 请求构建 `sessionLoad` 最小堆，O(N + K_miss * logN) 分配

**核心代码**：`internal/scheduler/policy/session_aware.go`

```go
func (p *SessionAwarePolicy) BatchSelect(reqs []*domain.RouteContext, nodes map[string]*domain.NodeState) []BatchSelectResult {
    // Phase 1: resolve hits, collect miss indices
    missIndices := []int{}
    for i, req := range reqs {
        if instID, ok := p.sessionAssign[req.SessionID]; ok {
            if ns, exists := nodes[instID]; exists {
                results[i].Instance = ns.Instance
                continue
            }
            // clean stale mapping
        }
        missIndices = append(missIndices, i)
    }
    // Phase 2: min-heap by sessionLoad for misses
    heap := buildSessionHeap(nodes, p.sessionLoad, p.MaxSessionLoad)
    for _, idx := range missIndices {
        results[idx].Instance = heap[0].inst
        heap[0].load++; sessionHeapDown(heap, 0, len(heap))
    }
}
```

**效果**：

| 场景 | 优化前 | 优化后 |
|------|--------|--------|
| 8192 全命中 × 10K 节点 | O(K) | O(K)（无变化） |
| 8192 全未命中 × 10K 节点 | O(K*N) = 8192w | O(N + K*logN) ≈ 12w |
| server.go 自动检测 | 逐请求 Select | BatchSelect（类型断言自动生效） |

---

## O29. Prometheus 指标向量预缓存

**类别**：并发优化

**问题**：`HandleChatCompletion` 每请求调用 4 次 `WithLabelValues(inst.ID)`，每次做 map+mutex 查找。10K 实例 = ~160K 时间序列。

**方案**：在 `metrics.go` 中新增 `InstanceMetrics` 缓存结构，预解析 4 个 metric handle 存入 `sync.Map`：

```go
type InstanceMetrics struct {
    ActiveRequests prometheus.Gauge
    DurationMs     prometheus.Observer
    RequestsOK     prometheus.Counter
    RequestsError  prometheus.Counter
}

func GetInstanceMetrics(instanceID string) *InstanceMetrics {
    if val, ok := instanceMetricsCache.Load(instanceID); ok {
        return val.(*InstanceMetrics)
    }
    // 首次: 解析并缓存
    im := &InstanceMetrics{...}
    actual, _ := instanceMetricsCache.LoadOrStore(instanceID, im)
    return actual.(*InstanceMetrics)
}
```

**效果**：每请求 4×mutex/map 查找 → 1×`sync.Map.Load`（amortized lock-free）。

---

## O30. Proxy URL 规范化缓存

**类别**：分配消除

**问题**：`Forward()` 每次调用对 schemeless endpoint 做 `"http://" + key` 字符串拼接，即使 proxy 已缓存。热路径上 1 次堆分配。

**方案**：用原始 `targetEndpoint` 作为首次 `sync.Map` 查找 key。慢路径创建 proxy 后，同时存储两个 key（规范化 key + 原始 key），后续走快速路径零分配。

```go
func (p *ReverseProxy) Forward(w http.ResponseWriter, r *http.Request, targetEndpoint string) error {
    // 快速路径：直接用原始 endpoint 查找
    if val, ok := p.proxies.Load(targetEndpoint); ok {
        val.(*httputil.ReverseProxy).ServeHTTP(w, r)
        return nil
    }
    // 慢路径：规范化 + 创建 + 存储两个 key
    key := normalize(targetEndpoint)
    actual, _ := p.proxies.LoadOrStore(key, rp)
    p.proxies.Store(targetEndpoint, actual) // 后续走快速路径
}
```

**效果**：首次请求后，schemeless endpoint 直接命中 `Load(targetEndpoint)`，零字符串分配。`H2CReverseProxy` 同理。

---

## O31. Burst 分配压力优化

**类别**：对象池 + 预分配

### O31b. Map 预分配

**问题**：`handleStartStep` 创建空 map，首次 burst 时反复扩容。

**方案**：预分配 131072 容量的 `allocDedup` 和 `releaseDedup`（对齐 10w 请求峰值，一步到位避免 rehash）；内层 `gatewayAllocs` 预分配 128。

```go
const defaultDedupCapacity = 131072 // 10w < 131072，无需运行时 rehash
s.allocDedup = make(map[string]allocCacheEntry, defaultDedupCapacity)
s.releaseDedup = make(map[string]bool, defaultDedupCapacity)
// 内层
s.gatewayAllocs[gwAddr] = make(map[string]int64, 128)
```

> **设计决策**：初始版本用 4096，10w 请求触发 5 次 rehash（4096→8192→...→131072），累积 ~3.5MB 废弃 bucket 数组等 GC。按项目"空间换时间"原则，直接按峰值预分配（参见 O34）。

### O31c. Pool 预热

**问题**：首次 burst 时 `sync.Pool` 全部 miss，退化为逐次 `New()`。

**方案**：`NewServer` 时预填充 8192 个 event 和 8192 个 allocResult channel。

```go
func preWarmPools() {
    const preWarmSize = 8192
    events := make([]*event, preWarmSize)
    for i := range events { events[i] = new(event) }
    for _, ev := range events { eventPool.Put(ev) }

    channels := make([]chan allocResult, preWarmSize)
    for i := range channels { channels[i] = make(chan allocResult, 1) }
    for _, ch := range channels { allocResultPool.Put(ch) }
}
```

**效果**：减少首次 burst 时的 GC 压力，8192 预热可覆盖大部分单批次 burst 的 pool 需求。

---

## O33. heapBufPool 容量对齐万卡规模

**类别**：对象池

**问题**：`heapBufPool.New` 创建 `cap=1024`（~40KB）的 `[]nodeEntry`，万卡场景首次 `BatchSelect` 需要 `cap≥10000`，导致丢弃预热的小 slice 并重建 `cap=10000`（~400KB）。8192 个预热出的小 slice 全部成为垃圾。

**方案**：将 `Pool.New` 初始容量从 1024 提升到 10240，对齐万卡规模。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
var heapBufPool = sync.Pool{
    New: func() any {
        s := make([]nodeEntry, 0, 10240) // 对齐万卡规模
        return &s
    },
}
```

**效果**：消除首次使用时 8192 个废弃小 slice（~320MB 废弃对象），Pool 命中率从 0% 提升到 100%。

---

## O34. allocDedup 预分配容量提升

**类别**：预分配

**问题**：`allocDedup` 和 `releaseDedup` 初始预分配 4096，10w 请求触发 5 次 rehash（4096→8192→16384→32768→65536→131072），累积 ~3.5MB 废弃 bucket 数组等 GC。

**方案**：按项目"空间换时间"原则，预分配 131072 容量（10w < 131072），一步到位避免运行时 rehash。

**核心代码**：`internal/scheduler/server.go`

```go
const defaultDedupCapacity = 131072
s.allocDedup = make(map[string]allocCacheEntry, defaultDedupCapacity)
s.releaseDedup = make(map[string]bool, defaultDedupCapacity)
```

**效果**：消除 5 次 rehash 的 ~3.5MB 累积废弃 bucket 数组，降低 10w burst 时的 GC 压力。

---

## O35. UpdateInstanceMeta in-place 更新

**类别**：分配消除

**问题**：`MinLoadPolicy.UpdateInstanceMeta()` 每次更新都分配新的 `*instanceLoadMeta` 对象替换 map 中已有的指针，1w 节点 × 每步更新 = 1w 次小对象分配。

**方案**：已存在的 entry 直接修改字段（in-place 更新），仅首次见到的 key 分配新对象。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
func (p *MinLoadPolicy) UpdateInstanceMeta(id string, meta *domain.InstanceMeta) {
    p.mu.Lock()
    defer p.mu.Unlock()
    if existing, ok := p.instanceMeta[id]; ok {
        // in-place 更新，不分配新对象
        existing.WaitingCount = meta.WaitingCount
        existing.AvailableBlocks = meta.AvailableBlocks
        existing.AvgIOLength = meta.AvgIOLength
    } else {
        p.instanceMeta[id] = &instanceLoadMeta{...}
    }
}
```

**效果**：稳态下消除 1w 次/步的小对象分配。

---

## O36. SessionAware BatchSelect 缓冲区复用

**类别**：分配消除

**问题**：`SessionAwarePolicy.BatchSelect()` 每次分配 3 个临时 buffer：`results` (~192KB) + `missIndices` (~64KB) + `heap` (~320KB)，三种不同 size class 交替分配释放。

**方案**：将三个 buffer 提升为 `SessionAwarePolicy` 的结构体字段，跨调用复用。`Reset()` 时保留底层数组。

**核心代码**：`internal/scheduler/policy/session_aware.go`

```go
type SessionAwarePolicy struct {
    // ...
    resultsBuf     []BatchSelectResult
    missIndicesBuf []int
    heapBuf        []sessionNodeEntry
}

func (p *SessionAwarePolicy) BatchSelect(...) []BatchSelectResult {
    p.resultsBuf = growSlice(p.resultsBuf, len(reqs))
    p.missIndicesBuf = p.missIndicesBuf[:0]
    p.heapBuf = growSlice(p.heapBuf, len(nodes))
    // ...
}
```

**安全性**：与 O23 相同，event-loop 单线程调用。

**效果**：每批次消除 3 allocs（~576KB），消除跨 size-class 碎片。

---

## O37. ReverseProxy BufferPool

**类别**：对象池

**问题**：Go 标准库 `httputil.ReverseProxy.ServeHTTP()` 内部 `copyBuffer()` 为每个活跃连接分配 32KB transfer buffer。1w 并发 SSE = ~320MB 常驻内存，且 buffer 生命周期与 SSE 连接一致（可能数分钟）。

**方案**：实现 `httputil.BufferPool` 接口，用 `sync.Pool` 池化 32KB buffer。设置到 `ReverseProxy.BufferPool` 字段。

**核心代码**：`internal/gateway/proxy.go`

```go
type proxyBufPool struct{ pool sync.Pool }

func (p *proxyBufPool) Get() []byte {
    if v := p.pool.Get(); v != nil { return v.([]byte) }
    return make([]byte, 32*1024)
}

func (p *proxyBufPool) Put(buf []byte) { p.pool.Put(buf) }

var sharedBufPool = &proxyBufPool{}

// 创建 proxy 时设置 BufferPool
rp := &httputil.ReverseProxy{
    Director:      director,
    BufferPool:    sharedBufPool,
    FlushInterval: -1,
}
```

**效果**：SSE 连接结束后 buffer 归还池，降低 1w 并发 SSE 的 ~320MB GC 压力。

---

## GC 热点修复总结

基于 `docs/gc-hotspot-analysis.md` 分析的 28 个 GC 热点（G1-G28），本轮优化（O17-O26）聚焦于消除远程模式下每请求的堆分配：

| 路径 | 优化前 | 优化后 |
| ---- | ------ | ------ |
| Gateway 代理 (per-req) | ~6 allocs (Proxy+closures+URL+string) | 0 (cached) |
| RouteContext + CostMetrics | 4 allocs | 0 (pooled) |
| protobuf (不可避免) | 3 allocs | 3 allocs |
| Event-loop (per-batch) | 4 allocs + 1 closure | 0 (reused) |
| BatchSelect results | 1 alloc (128KB) | 0 (reused) |
| Error 对象 (burst) | N allocs | 0 (sentinel) |
| **远程模式单请求合计** | **~13** | **~3** |

---

## O32. preWarm Pool 扩容

**类别**：对象池

**问题**：原 `preWarmSize = 1024`，而单批次 `maxDrainBatch = 8192`。10w burst 首批 8192 请求中仅 1024 能命中预热 pool，其余 7168 仍需 `New()`。

**方案**：将 `preWarmSize` 从 1024 提升至 8192，与 `maxDrainBatch` 对齐，确保首批处理全部命中预热 pool。

**核心代码**：`internal/scheduler/server.go`

```go
func preWarmPools() {
    const preWarmSize = 8192

    events := make([]*event, preWarmSize)
    for i := range events { events[i] = new(event) }
    for _, ev := range events { eventPool.Put(ev) }

    channels := make([]chan allocResult, preWarmSize)
    for i := range channels { channels[i] = make(chan allocResult, 1) }
    for _, ch := range channels { allocResultPool.Put(ch) }
}
```

**效果**：

| 指标 | 优化前（1024） | 优化后（8192） |
|------|----------------|----------------|
| 首批 pool 命中 | 1024/8192（12.5%） | 8192/8192（100%） |
| 10w burst 端到端延迟 | ~901ms | ~408ms（**2.2x**） |
| 预热内存成本 | ~0.5MB | ~4MB（可接受） |

---

## O38. Generation 计数器替代 count 缓存失效

**类别**：正确性 + 性能

**问题**：RoundRobin 和 MinLoad 策略的缓存失效机制仅基于节点数量（`cachedCount`）。当节点增删恰好 count 不变时（1 删 + 1 加），缓存不刷新，引用过期 `NodeState`，可能导致选择到已移除的节点。

**方案**：
1. `NodeStateStore` 新增 `generation atomic.Uint64`，每次写操作（Register/Unregister/Sync）递增
2. 策略实现 `GenerationAware` 接口，通过 `SetGeneration(gen)` 接收当前代数
3. 缓存重建条件从 `len(nodes) != cachedCount` 改为 `currentGeneration != cachedGeneration`

**核心代码**：`internal/scheduler/node_state_store.go`、`internal/scheduler/policy/interface.go`、`internal/scheduler/policy/round_robin.go`、`internal/scheduler/policy/min_load.go`

```go
// NodeStateStore — 每次写操作递增 generation
func (sm *NodeStateStore) storeAndBump(m *map[string]*domain.NodeState) {
    sm.nodesPtr.Store(m)
    sm.generation.Add(1)
}

// GenerationAware 可选接口
type GenerationAware interface {
    SetGeneration(gen uint64)
}

// RoundRobinPolicy — generation 替代 count
type RoundRobinPolicy struct {
    cachedSorted     []*domain.NodeState
    cachedGeneration uint64
    currentGeneration uint64
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| 缓存失效精度 | count-based（有漏洞） | generation-based（精确） |
| 等数替换 bug | 存在 | 已修复 |
| 性能开销 | 无额外开销 | 1 次 atomic.Load / batch |

---

## O39. heapBufPool → struct 字段

**类别**：分配消除

**问题**：`MinLoadPolicy.BatchSelect` 使用 `sync.Pool` 管理 `[]nodeEntry` 堆缓冲区。但该方法仅被单线程 event-loop 调用，`sync.Pool` 的 Get/Put 原子操作和 GC 交互纯属浪费。同文件的 `resultsBuf` 已经用 struct 字段模式。

**方案**：删除 `heapBufPool`，在 `MinLoadPolicy` 上加 `heapBuf []nodeEntry` 字段，BatchSelect 中直接复用。

**核心代码**：`internal/scheduler/policy/min_load.go`

```go
type MinLoadPolicy struct {
    // ...
    // resultsBuf and heapBuf are reused across BatchSelect calls to avoid per-call allocation.
    // Only accessed from the event-loop (single-threaded).
    resultsBuf []BatchSelectResult
    heapBuf    []nodeEntry
}

func (p *MinLoadPolicy) BatchSelect(...) []BatchSelectResult {
    // Reuse heap buffer from struct field to avoid per-batch allocation.
    h := p.heapBuf[:0]
    if cap(h) < len(nodes) {
        h = make([]nodeEntry, 0, len(nodes))
    }
    // ... use h ...
    p.heapBuf = h // save back for reuse
}
```

**效果**：

| 指标 | 优化前（sync.Pool） | 优化后（struct 字段） |
|------|---------------------|----------------------|
| MinLoad BatchSelect 10K×4096 | ~602μs | ~345μs（**-43%**） |
| MinLoad BatchSelect 10K×8192 | ~1.17ms | ~539μs（**-54%**） |
| 额外原子操作 | 2 次/call（Get+Put） | 0 次 |

> 注：泛型统一 heap 代码（`heapItem` 接口约束）经 benchmark 验证存在 ~2x 回退，已放弃。详见 min_load.go 和 session_aware.go 中的 inline heap 注释。

---

## O40. releaseDedup map[string]struct{}

**类别**：内存优化

**问题**：`releaseDedup` 原为 `map[string]bool`，`bool` 值占 1 字节但 map bucket 对齐到 8 字节，10w 条目浪费 ~100KB。

**方案**：改为 `map[string]struct{}`，零值大小。

**核心代码**：`internal/scheduler/server.go`

```go
releaseDedup map[string]struct{} // allocation_id → already released
// 赋值
s.releaseDedup[allocID] = struct{}{}
// 判断
if _, ok := s.releaseDedup[allocID]; ok { ... }
```

**效果**：10w release/step 节省 ~100KB，更符合 Go 惯用写法。

---

## O41. SessionAware selectMinLoad 移除多余锁

**类别**：并发优化

**问题**：`SessionAwarePolicy.selectMinLoad` 只读 `nodes`（COW 快照），不访问 `sessionAssign`/`sessionLoad`，但持有 `p.mu.Lock()`，造成不必要的 mutex 竞争。

**方案**：删除 `p.mu.Lock()` / `defer p.mu.Unlock()`，添加注释说明安全性。

**核心代码**：`internal/scheduler/policy/session_aware.go`

```go
// selectMinLoad picks the instance with the fewest active requests.
// Called only from the single-threaded event-loop. Does NOT access
// sessionAssign or sessionLoad, so no lock is needed — it only reads
// the COW nodes snapshot passed by the caller.
func (p *SessionAwarePolicy) selectMinLoad(nodes map[string]*domain.NodeState) (*domain.Instance, error) {
    // no lock needed — pure read of COW snapshot
```

**效果**：消除 `selectMinLoad` 路径上的 mutex 竞争，对高频 cache-miss 场景有帮助。

---

## O42. recordAllocation 提取消除重复

**类别**：代码质量

**问题**：`batchAllocate` 和 sequential 回退路径的 6 步 bookkeeping（Acquire → 计数+1 → allocID → gateway 跟踪 → dedup → 发送结果）完全重复。

**方案**：提取 `recordAllocation(ev, inst, stepID)` 方法，两处调用改为单行。

**核心代码**：`internal/scheduler/server.go`

```go
// recordAllocation performs the post-selection bookkeeping for a successful allocation:
// state acquire, counter increment, dedup cache, gateway tracking, and result delivery.
// Called only from the event-loop (single-threaded, no lock needed).
func (s *Server) recordAllocation(ev *event, inst *domain.Instance, stepID int64) {
    s.state.Acquire(inst.ID)
    s.activeCount++
    s.activeCountAtomic.Store(s.activeCount)
    allocID := s.nextAllocationID(stepID)
    // ... gateway tracking, dedup, result delivery
    ev.resultCh <- allocResult{inst: inst, allocationID: allocID}
}
```

**效果**：消除 ~30 行 DRY 违反，降低维护成本和 AI 理解成本。

---

## O43. GetActiveCount 原子镜像

**类别**：可观测

**问题**：`GetActiveCount()` 永远返回 -1 哨兵值，对监控和调试毫无价值。

**方案**：新增 `activeCountAtomic atomic.Int64`，event-loop 修改 `activeCount` 时同步 `Store`，`GetActiveCount()` 返回真实值。

**核心代码**：`internal/scheduler/server.go`

```go
type Server struct {
    activeCount        int64         // in-flight allocations in current step (event-loop only)
    activeCountAtomic  atomic.Int64  // mirrors activeCount for external readers (monitoring)
}

func (s *Server) GetActiveCount() int64 {
    return s.activeCountAtomic.Load()
}

// event-loop 中每次变更时同步
s.activeCount++
s.activeCountAtomic.Store(s.activeCount)
```

**效果**：外部监控可实时获取精确的 in-flight 请求数，对 Prometheus 指标和调试至关重要。

---

## O44. StepNotifier 优雅退出

**类别**：稳定性

**问题**：通知 goroutine 用 `context.Background()` + `time.Sleep`，scheduler 关闭时无法取消在途通知，可能导致 goroutine 泄漏。

**方案**：`StepNotifier` 持有 `ctx, cancel`；`notifyGateway` 用 `n.ctx` 派生超时 context；重试间隔用 `select { case <-time.After(...): case <-n.ctx.Done(): }` 替换 `time.Sleep`；shutdown 时调用 `Stop()`。

**核心代码**：`internal/scheduler/step_notifier.go`

```go
type StepNotifier struct {
    registry *GatewayRegistry
    client   *http.Client
    logger   *zap.Logger
    ctx      context.Context
    cancel   context.CancelFunc
}

func (n *StepNotifier) Stop() { n.cancel() }

// notifyGateway 中
ctx, cancel := context.WithTimeout(n.ctx, notifyTimeout)  // 继承父 ctx
// 重试等待
select {
case <-time.After(time.Duration(attempt*100) * time.Millisecond):
case <-n.ctx.Done():
    return  // shutdown，立即退出
}
```

**效果**：

| 指标 | 优化前 | 优化后 |
|------|--------|--------|
| Shutdown 时通知 goroutine | 等待超时（最长 2s × 3 重试 = 6s） | 立即取消 |
| Goroutine 泄漏风险 | 存在 | 消除 |

---

## O45. HTTP handler Go 1.22+ method patterns

**类别**：代码质量

**问题**：`/v1/instances` 使用单个 `handleInstances` 作为 method dispatcher（switch on `r.Method`），代码冗余且不符合 Go 1.22+ 的 enhanced routing 模式。

**方案**：删除 `handleInstances` dispatcher，改为 4 条方法级路由注册。Go 1.22+ `http.ServeMux` 原生支持 `"METHOD /path"` 模式，未匹配的方法自动返回 405。

**核心代码**：`internal/scheduler/http_handler.go`

```go
func (h *HTTPHandler) RegisterRoutes(mux *http.ServeMux) {
    mux.HandleFunc("/v1/steps/start", h.handleStartStep)
    mux.HandleFunc("/v1/steps/end", h.handleEndStep)
    mux.HandleFunc("/v1/steps/current", h.handleGetStep)
    mux.HandleFunc("POST /v1/instances", h.handleRegisterInstances)
    mux.HandleFunc("PUT /v1/instances", h.handleSyncInstances)
    mux.HandleFunc("DELETE /v1/instances", h.handleUnregisterInstances)
    mux.HandleFunc("GET /v1/instances", h.handleListInstances)
}
```

**效果**：消除手写 dispatcher，减少 ~20 行样板代码，方法路由由框架保证正确性。

---

## O46. event 结构体字段-事件类型映射注释

**类别**：可读性

**问题**：`event` 是 discriminated union，不同 `eventType` 使用不同字段子集，但无文档说明映射关系。AI 和新开发者需要逐个阅读 handler 才能理解哪些字段在哪些事件中有效。

**方案**：在 struct 定义上方添加字段-事件类型映射表。

**核心代码**：`internal/scheduler/server.go`

```go
// event is a discriminated union dispatched by the single-threaded event-loop.
// Each eventType uses a specific subset of fields:
//
//  evAllocate:       ctx, route, resultCh
//  evRelease:        instanceID, gatewayAddr, allocationID, costMetrics
//  evStartStep:      stepID, policyName, stepResult
//  evEndStep:        stepID, stepResult
//  evCleanupGateway: gatewayAddr, cleanupDoneCh
//  evStop:           (none)
type event struct { ... }
```

**效果**：降低 AI 理解成本，新开发者可快速定位每种事件的数据流。

---

## Benchmark 实测数据总览

以下为 Apple M2 上 `go test -bench -benchmem -count=3` 的实测数据，作为性能基线。

> **更新日期**：2026-03-08（O38-O46 优化后重新采集）

### 策略层（Policy）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| RoundRobin Select 10K | ~13ns | 0 | 0B | 排序缓存 O(1) |
| MinRequest Select 10K | ~30ns | 0 | 0B | P2C O(1) |
| MinLoad Select P2C 10K | ~65ns | 0 | 0B | P2C 快路径 |
| MinLoad Select FullScan 10K | ~93μs | 0 | 0B | 全量扫描回退 |
| SessionAware Select Hit 10K | ~42ns | 0 | 0B | 缓存命中 O(1) |
| SessionAware Select Miss 10K | ~261μs | 1 | ~200B | 全量扫描分配新 session |
| MinLoad BatchSelect 10K×4096 | ~345μs | 0 | ~141B | **零分配**（struct 字段复用） |
| MinLoad BatchSelect 10K×8192 | ~539μs | 0 | ~276B | **零分配** |
| SessionAware BatchSelect AllHit 10K | ~176μs | 0 | ~19B | **零分配**（缓冲区复用） |
| SessionAware BatchSelect AllMiss 10K | ~396μs | 0 | ~520B | **零分配**（缓冲区复用，O36 后消除 96 allocs） |
| SessionAwareV3 Select Hit Stay 10K | ~213μs | 0 | 0B | O(N) 全量扫描 findMinLoad + stay 决策 |
| SessionAwareV3 Select Miss 10K | ~699μs | 1 | ~274B | 全量扫描分配新 session |
| SessionAwareV3 BatchSelect AllMigrate 10K×4096 | ~1.08ms | 0 | ~1.4KB | **零分配**（堆排序 + migrate 路径） |
| SessionAwareV3 BatchSelect AllStay 10K×4096 | ~1.68ms | 0 | ~1KB | **零分配**（stay 路径，堆略过） |
| SessionAwareV3 BatchSelect Mixed 10K×4096 | ~1.47ms | 0 | ~2KB | **零分配**（50% stay + 50% new） |
| AllPolicies Comparison 10K | RR:13ns ML:11μs MR:28ns CA_hit:35ns | 0 | 0B | 横向对比 |

### 调度器层（Scheduler）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| Allocate 10K | ~132μs | 8 | ~766B | 单请求完整路径 |
| Allocate 100 | ~3.3μs | 9 | ~830B | 小规模 |
| EventLoop Throughput | ~0.69μs | 7 | ~587B | ~1.45M events/s |
| AllocateReleaseCycle | ~3.4μs | 9 | ~829B | 完整请求生命周期 |
| Allocate Contention 64w | ~0.53μs | 7 | ~586B | 高并发竞争 |
| BatchAllocate Burst 1K | ~2.4ms | ~5K | ~535KB | 含完整 step 周期 |
| BatchAllocate Burst 10K | ~67ms | ~53K | ~6MB | goroutine 调度主导 |
| BatchAllocate Burst 100K | ~814ms | ~695K | ~80MB | 高方差，需多轮稳定 |
| NodeStateStore GetNodes 10K | ~1.96ns | 0 | 0B | COW 零拷贝 |
| NodeStateStore GetSnapshot 10K | ~554μs | 10034 | ~757KB | 深拷贝（写路径） |
| NodeStateStore AcquireRelease | ~15ns | 0 | 0B | 原子操作 |
| GatewayRegistry GetAll | ~1.97ns | 0 | 0B | COW 零拷贝 |
| GatewayRegistry Heartbeat | ~2.6μs | 6 | ~3.6KB | COW clone |
| DedupHit | ~597ns | 3 | ~422B | 幂等去重命中 |
| LoadBalance Fairness 10K | ~134μs | 8 | ~811B | CV 73~76% |
| LoadBalance FairnessBatch 10K | ~521μs | 0 | ~264B | CV 0%（完美均匀） |

### 网关层（Gateway）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| ProxyLatency 100B | ~47μs | 108 | ~14.4KB | 小响应 |
| ProxyLatency 1KB | ~48μs | 108 | ~16.4KB | 中响应 |
| ProxyLatency 10KB | ~55μs | 110 | ~36.8KB | 大响应 |
| ProxyLatency P50/P95/P99 | P50:44μs P95:56μs P99:127μs | 107 | ~14.3KB | 延迟分布 |
| SSE Throughput 10 chunks | ~61μs | 117 | ~17.1KB | 流式代理 |
| SSE Throughput 50 chunks | ~143μs | 119 | ~25.9KB | |
| SSE Throughput 200 chunks | ~432μs | 123 | ~59.9KB | |
| SSE TokenRate 2000tok | ~757μs | 93 | ~11.9KB | 模拟真实推理 |
| HandleChatCompletion E2E | ~48μs | 118 | ~15.2KB | 非流式完整路径 |
| HandleChatCompletion SSE E2E | ~146μs | 126 | ~23.1KB | 流式完整路径 |
| ChatCompletion Concurrent 100w | ~828μs | 104 | ~13.7KB | 100 workers |
| NonStreaming vs Streaming | 非流:48μs 流:143μs | 112/115 | 16.7/22.1KB | 对比 |
| ReleaseWithRetry NoError | ~37ns | 2 | 48B | 释放快路径 |
| GenerateRequestID w/trace | ~1.2μs | 13 | ~5.5KB | 含 Header 查找 |
| GenerateRequestID random | ~1.05μs | 11 | ~5.1KB | 纯随机生成 |

### 端到端 Burst（BenchmarkBatchAllocate_Burst）

| burst 规模 | 端到端延迟 | 内存 | allocs | 说明 |
|-----------|-----------|------|--------|------|
| 1,000 | ~2.4ms | ~535KB | ~5K | 含 StartStep + Allocate + Release + EndStep |
| 10,000 | ~67ms | ~6MB | ~53K | goroutine 调度开销占比增大 |
| 100,000 | ~814ms | ~80MB | ~695K | 高方差，channel + goroutine 调度主导 |

> **注意**：纯 BatchSelect 计算时间约 ~0.5ms/8192 请求（12 批 × ~0.5ms ≈ 6ms），端到端延迟受 goroutine 创建、channel send/recv、event-loop 排队等开销主导。

### 与历史基线对比（O38-O46 优化效果）

| 基准测试 | 旧基线 | 新基线 | 变化 |
|---------|--------|--------|------|
| MinLoad BatchSelect 10K×4096 | ~602μs | ~345μs | **-43%** |
| MinLoad BatchSelect 10K×8192 | ~1.17ms | ~539μs | **-54%** |
| SessionAware BatchSelect AllMiss 10K | ~2.9ms / 96 allocs | ~396μs / 0 allocs | **-86%, allocs 清零** |
| EventLoop Throughput | ~1.6μs (~625K/s) | ~0.69μs (~1.45M/s) | **-57%, 吞吐 2.3x** |
| Allocate 10K | ~259μs | ~132μs | **-49%** |
| ProxyLatency P99 | ~717μs | ~127μs | **-82%** |
| HandleChatCompletion E2E | ~151μs | ~48μs | **-68%** |
| SSE TokenRate 2000tok | ~15.7ms / 1873 allocs | ~757μs / 93 allocs | **-95%, allocs -95%** |
| BatchAllocate Burst 10K | ~599ms | ~67ms | **-89%** |

### 性能声明修正

| 声明 | 旧值 | 修正值 | 说明 |
|------|------|--------|------|
| 调度吞吐 | ~250w QPS | ~145w QPS | event-loop 实测（~0.69μs/event） |
| 10w burst 延迟 | 40ms | ~67ms-814ms | 端到端含 goroutine 调度，高方差 |
| BatchSelect 纯计算 | - | ~6ms/10w | 12 批 × ~0.5ms，远优于 40ms 目标 |

### gRPC 传输层（新增 2026-03-08）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| GRPCHandler Allocate 1K nodes | ~62μs | 309 | ~24.8KB | bufconn 完整 protobuf roundtrip |
| GRPCHandler Allocate 10K nodes | ~176μs | 309 | ~25KB | 10K 规模 gRPC Allocate |
| GRPCHandler Release | ~21.6μs | 147 | ~8.8KB | Release gRPC 路径 |
| GRPCHandler Heartbeat | ~21.9μs | 151 | ~9KB | Heartbeat gRPC 路径 |
| GRPCHandler Concurrent 4w | ~11.4μs | 282 | ~18KB | 4 workers 并发 |
| GRPCHandler Concurrent 64w | ~9.3μs | 279 | ~17.8KB | 64 workers 并发（负载均摊） |
| Direct vs gRPC | 直接:~18μs / 8 allocs vs gRPC:~61μs / 309 allocs | - | - | gRPC 层额外开销 ~43μs + 301 allocs |
| RemoteAllocator Allocate | ~61.5μs | 311 | ~25.2KB | gateway 侧 gRPC 客户端完整路径 |
| RemoteAllocator Release | ~21.5μs | 147 | ~8.8KB | gateway 侧 Release gRPC 路径 |
| Local vs Remote Allocator | 本地:~17μs / 8 allocs vs 远程:~61.5μs / 311 allocs | - | - | 远程约 3.5x 延迟 |
| ChatCompletion + RemoteAllocator E2E | ~97μs | 424 | ~39.7KB | gateway 模式完整热路径（gRPC + proxy） |

### H2C 反向代理（新增 2026-03-08）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| H2C Throughput 10 chunks | ~132μs | 163 | ~27.6KB | H2C SSE 代理 |
| H2C Throughput 50 chunks | ~186μs | 165 | ~27.7KB | |
| H2C Throughput 200 chunks | ~170μs | 165 | ~27.7KB | |
| H2C Concurrent 1000 conns | ~670ms | 136 | ~19.2KB | 1000 并发 SSE 连接 |
| HTTP/1.1 vs H2C Latency 1KB | HTTP/1.1:~49μs vs H2C:~105μs | 107/156 | 17.3/27.6KB | H2C 约 2x 延迟（http2.Transport 开销） |

### 快速路径拒绝（新增 2026-03-08）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| ChatCompletion NotServing | ~1.87μs | 25 | ~6.6KB | 非 SERVING 状态快速拒绝（含 httptest 开销） |

### HTTP 控制面（新增 2026-03-08）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| RegisterInstances 100 | ~33μs | 736 | ~49KB | 100 实例批量注册 |
| RegisterInstances 1K | ~333μs | 7041 | ~449KB | 1000 实例批量注册 |
| RegisterInstances 10K | ~3.5ms | 70075 | ~4.9MB | 10000 实例批量注册 |
| StartStep (含广播) | ~801μs | 364 | ~49.7KB | 含 StepNotifier 广播到 1 gateway |
| ListInstances 100 | ~11μs | 28 | ~21KB | 100 实例列表序列化 |
| ListInstances 1K | ~89μs | 20 | ~146KB | 1000 实例列表序列化 |
| ListInstances 10K | ~913μs | 20 | ~1.5MB | 10000 实例列表序列化 |
| SyncInstances 100 | ~40μs | 739 | ~53KB | 100 实例增量同步 |
| SyncInstances 1K | ~383μs | 7046 | ~504KB | 1000 实例增量同步 |
| JSON Unmarshal 10K instances | ~3.55ms | 70004 | ~6.7MB | 纯 JSON 反序列化（go-json） |

### 内存稳定性（新增 2026-03-08）

| 基准测试 | 延迟 | heap 增长 | 说明 |
|---------|------|-----------|------|
| MultiStepCycle (10K reqs/step) | ~27ms/step | ~256KB/step | 1000 step 循环后最终 heap ~32MiB |
| DedupMap Clear 10K entries | ~846μs | - | clear() + refill（含 refill 开销） |
| DedupMap Clear 100K entries | ~6.6ms | - | clear() + refill |
| StepReset 1K reqs | ~3ms | 5034 allocs | 完整 Start→reqs→End 周期 |
| StepReset 10K reqs | ~27ms | 51394 allocs | 完整 Start→reqs→End 周期 |

### 日志/指标开销（新增 2026-03-08）

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| Logger Info w/7 fields | ~97ns | 1 | 448B | NopCore，纯 field 构造开销 |
| Logger Debug disabled | ~44ns | 1 | 128B | 级别过滤后仍有 1 alloc（Check 路径） |
| Logger Warn w/reason | ~85ns | 1 | 192B | 告警日志 |
| Field Event/Status/Reason | ~3.5ns | 0 | 0B | 零分配字段构造 |
| GetInstanceMetrics CacheHit | ~13ns | 0 | 0B | sync.Map.Load 快速路径 |
| GetInstanceMetrics Concurrent 64w | ~21ns | 1 | 16B | 64 workers 并发查询 |
| InstanceMetrics Observe | ~40ns | 0 | 0B | Inc+Observe+Dec 完整操作 |

### JSON facade go-json vs stdlib（新增 2026-03-08）

| 基准测试 | go-json | stdlib | 加速比 |
|---------|---------|--------|--------|
| Marshal Instance | ~339ns / 3 allocs | ~481ns / 7 allocs | **1.4x** 延迟, **-57%** allocs |
| Unmarshal 10 instances | ~3.4μs / 53 allocs | ~16.8μs / 93 allocs | **5x** 延迟, **-43%** allocs |
| Unmarshal 100 instances | ~60.6μs / 503 allocs | ~240μs / 816 allocs | **4x** 延迟, **-38%** allocs |
| Unmarshal 1K instances | ~758μs / 5004 allocs | ~1.92ms / 8019 allocs | **2.5x** 延迟, **-38%** allocs |
| Encoder Write | ~403ns / 2 allocs | ~585ns / 4 allocs | **1.5x** 延迟, **-50%** allocs |
| Decoder Read | ~766ns / 9 allocs | ~2.3μs / 18 allocs | **3x** 延迟, **-50%** allocs |

### Stale Connection 防御层开销（新增 2026-04-09）

`replayableBody` 替换 `io.NopCloser(bytes.NewReader())` 的额外开销：

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| old NopCloser | ~38ns | 2 | 64B | 旧模式：仅设 Body+ContentLength |
| new replayableBody | ~61ns | 3 | 96B | 新模式：额外设 GetBody closure |
| **Delta** | **+23ns** | **+1** | **+32B** | 每请求额外 1 个 closure 分配 |
| GetBody call | ~44ns | 2 | 64B | GetBody 被调用时（仅 stale retry 触发） |
| isStaleConnError match | ~9.2ns | 0 | 0B | 字符串匹配（仅 error 路径） |
| isStaleConnError nil | ~2ns | 0 | 0B | nil 快速路径（正常请求走此路径） |

**E2E 影响验证**（对比 parent commit，排除 forensics 变更影响）：

| E2E 基准测试 | Parent commit | Stale conn fix | Delta |
|-------------|---------------|----------------|-------|
| ProxyLatency 100B | ~48μs / 111 allocs | ~48μs / 111 allocs | **无变化** |
| SSE Throughput 10 chunks | ~64μs / 120 allocs | ~64μs / 120 allocs | **无变化** |
| HandleChatCompletion E2E | ~51μs / 135 allocs | ~50μs / 135 allocs | **无变化** |
| HandleChatCompletion SSE E2E | ~160μs / 241 allocs | ~155μs / 241 allocs | **无变化** |

> **结论**：`replayableBody` 的 +1 alloc / +23ns 在 E2E 路径中被测量噪声淹没，
> proxy 路径走 `httputil.ReverseProxy`（自建 request），不经过 `replayableBody`。
> `isStaleConnError` 仅在 error 路径调用，正常请求零开销。**无性能劣化**。

### PD cache-aware token-only 调度（新增 2026-05-27）

`pd_cache_aware` 优先使用 `prompt_token_ids` 透传后的真实 token 序列做 prefill block-prefix 匹配。以下基线覆盖 token-only rollout 请求的热 prefix 命中路径，防止后续调度权重或 token 提取改动引入明显退化。

| 基准测试 | 延迟 | allocs/op | B/op | 说明 |
|---------|------|-----------|------|------|
| PDCacheAware Select TokenIDsHit | ~13.2μs | 14 | 8440B | 100 节点，192 token，64-token block，warm prefix |
