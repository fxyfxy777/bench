# adaptive_block：自适应 Block 调度策略设计

## 1. 背景与目标

合版训练需支持单轮、多轮、长对话等混合场景，请求 token 长度差异可达 10x+。现有调度策略无法根据请求的预估 token 消耗进行 block 级精确调度，容易导致后端 Preemption（代价极大）或频繁 Cache 驱逐（TTFT 退化）。

### 1.1 目标

根据上游提供的预估输入/输出 token 长度，结合后端 FastDeploy 的 KV block 实时状态，做出最优实例选择：

1. **避免 Preemption** — 代价最大：in-flight 请求被杀死，`num_computed_tokens=0`，从头重算
2. **减少 Cache 驱逐** — 代价中等：丢失 prefix cache，未来请求 TTFT 增加
3. **最小化重调度** — session 绑定实例只在 block 紧急时才迁移

### 1.2 设计哲学

- **不感知业务类型**：无 request_type 枚举，不用上游适配新场景
- **统一 block-fit 算法**：通过 est_input_tokens + est_output_tokens + session_id 自然驱动调度差异
- 短请求 block 需求少 → 可去任何实例（简单负载均衡）
- 长请求 block 需求大 → 自然被路由到有余量的实例
- 多轮请求有 session_id → 自动启用亲和（减少 cache miss）

---

## 2. FastDeploy KV Cache 机制分析

### 2.1 GPU Block 三级安全区

| 安全区间 | 条件 | 后果 | 严重度 |
|----------|------|------|--------|
| 安全区 | `needed < free_gpu_blocks - decode_reserve` | 无驱逐，直接分配 | OK |
| 驱逐区 | `free < needed <= available` | LRU 驱逐 prefix cache，未来请求 TTFT 升高 | 中等 |
| 危险区 | `needed > available` | Preemption：杀死 in-flight 请求，从头重算 | 灾难 |

关键指标含义：

- `free_gpu_block_num`：真正空闲的 block，可直接分配，零成本
- `available_gpu_block_num`：free + 可驱逐（prefix cache 中 ref_count=0 的节点）。分配需触发 LRU 驱逐
- `max_gpu_block_num`：实例启动时确定的 GPU block 总量

**外部调度器应确保请求落在安全区，极端情况允许驱逐区，绝不允许危险区。**

### 2.2 CPU Cache 淘汰机制

CPU cache 是"最后防线"——GPU 驱逐的数据移到 CPU 不丢失，只有 CPU 也被淘汰才需要从头重计算 prefill。

| 特性 | 说明 |
|------|------|
| 淘汰策略 | **纯 LRU**（`last_access_time` 最老的先淘汰） |
| 触发时机 | 仅在需要新 host block 但 pool 满时（`allocate_host_blocks` 时 free < needed） |
| 保护机制 | `ref_count > 0`（请求 in-flight）时不可驱逐 |
| 无 pin 机制 | 不能锁定特定 session 的 cache 防止淘汰 |
| 隐性保护 | 最近被 `touch()` 的 session 排在 LRU 尾部，最后被淘汰 |

**关键风险**：多轮 session 在请求之间（idle），其 cache 的 `ref_count = 0`，完全暴露在 LRU 淘汰风险下。如果一个实例上绑定的 session 过多，老 session 的 CPU cache 会被新 session 挤掉。

### 2.3 CPU Cache 安全容量公式

```
实例可安全持有的 session 数 = max_cpu_block_num / (avg_session_tokens / block_size) / safety_factor
```

参数说明：
- `avg_session_tokens`：session 累积的 prompt token 数（随轮次增长）
- `block_size`：默认 64 tokens/block
- `safety_factor`：1.3~1.5（考虑 write_through backup 开销）

示例：100 个 session，每个 4096 tokens，block_size=64：
```
blocks_per_session = ceil(4096/64) = 64
所需 CPU blocks = 100 * 64 * 1.3 = 8320 blocks
```

### 2.4 多级存储调度含义

| 场景 | 代价 | 外部调度器对策 |
|------|------|--------------|
| GPU 分配在安全区 | 零额外开销 | 默认目标 |
| GPU 驱逐但 CPU cache 存在 | CPU→GPU swap 延迟（ms级） | 可接受，放宽 GPU 安全阈值 |
| CPU cache 也被淘汰 | 全量 prefill 重计算（百ms~s级） | **必须避免**：控制 session 密度 |
| Preemption | in-flight 请求从头重算 | **绝对禁止**：门控拒绝 |

### 2.5 Write Policy 对 CPU 占用的影响

| 策略 | CPU 占用 | 说明 |
|------|----------|------|
| `write_through` (threshold=1) | 最高 | 每个被复用的 prefix 立即 backup 到 CPU |
| `write_through_selective` (default, threshold=2) | 中等 | 仅命中 ≥2 次的 prefix 才 backup |
| `write_back` | 最低 | 仅在 GPU 驱逐时才写 CPU |

不同 write policy 下，safety_factor 建议：
- `write_through`：1.5（CPU 被 backup 大量占用）
- `write_through_selective`：1.3
- `write_back`：1.1（CPU 只在压力下才被使用）

---

## 3. 数据模型变更

### 3.1 RouteContext 扩展

文件：`internal/domain/models.go`

```go
type RouteContext struct {
    // ... existing fields ...
    EstInputTokens  int64 `json:"est_input_tokens,omitzero"`   // 预估输入token数
    EstOutputTokens int64 `json:"est_output_tokens,omitzero"`  // 预估输出token数
}
```

不加 request_type——上游只传 token 估算 + session_id，调度侧通过 token 量和 session_id 是否存在自动决策。

### 3.2 Proto 扩展

文件：`api/proto/router.proto`

```protobuf
message AllocateRequest {
  // ... existing fields 1-6 ...
  int64 est_input_tokens = 7;   // 预估输入token数
  int64 est_output_tokens = 8;  // 预估输出token数
}
```

### 3.3 InstanceMetaUpdate 扩展

文件：`internal/scheduler/policy/interface.go`

```go
type InstanceMetaUpdate struct {
    // ... existing fields ...
    FreeBlocks   int64 // 真正空闲block (fastdeploy:free_gpu_block_num)
    MaxBlocks    int64 // GPU block总量 (fastdeploy:max_gpu_block_num)
    RunningCount int64 // 正在运行的请求数 (fastdeploy:num_requests_running)
    MaxCPUBlocks int64 // CPU block总量 (fastdeploy:max_cpu_block_num), 0=未启用
}
```

### 3.4 PolicyConfig 新增字段

文件：`internal/scheduler/policy/factory.go`

```go
// adaptive_block policy 配置
BlockSize           int64   `json:"block_size,omitempty"`              // tokens/block, default 64
SafetyMarginRatio   float64 `json:"safety_margin_ratio,omitempty"`     // block预留安全系数, default 1.3
EmergencyBlockRatio float64 `json:"emergency_block_ratio,omitempty"`   // 紧急迁移阈值(free/max), default 0.10
DecodeReserveRatio  float64 `json:"decode_reserve_ratio,omitempty"`    // 为decode增长预留的比例, default 0.15
WeightBlockFit      float64 `json:"weight_block_fit_ab,omitempty"`     // block-fit评分权重, default 0.5
WeightLoadAB        float64 `json:"weight_load_ab,omitempty"`          // 负载评分权重, default 0.3
WeightWaitingAB     float64 `json:"weight_waiting_ab,omitempty"`       // 等待队列权重, default 0.2
CPUCacheRelaxFactor float64 `json:"cpu_cache_relax_factor,omitempty"`  // 有CPU cache时GPU安全阈值放宽系数, default 0.7
CPUCacheMaxRatio    float64 `json:"cpu_cache_max_ratio,omitempty"`     // session占CPU block上限比例, default 0.8
DefaultEstOutput    int64   `json:"default_est_output_ab,omitempty"`   // 输出token默认估算, default 512
TokensPerChar       float64 `json:"tokens_per_char,omitempty"`         // 字符→token推算率, default 0.6
```

---

## 4. Gateway 层元数据提取

### 4.1 从 extra_body 解析

上游通过 OpenAI SDK 的 `extra_body` 传递估算信息：

```python
# 上游调用示例
client.chat.completions.create(
    model="xxx",
    messages=[...],
    extra_body={
        "est_input_tokens": 2048,
        "est_output_tokens": 512,
        "session_id": "session-abc-123"
    }
)
```

Gateway 新增解析函数 `extractBlockEstimate(bodyBytes []byte) (estInput, estOutput int64)`：
- 解析 `extra_body.est_input_tokens` / `extra_body.est_output_tokens`
- 兼容顶层字段（OpenAI SDK 的 extra_body 会 merge 到请求顶层 JSON）

### 4.2 自动推算（字段缺失时）

当上游未提供估算值时，从请求体自动推算：

```go
func inferTokenEstimates(bodyBytes []byte, estInput, estOutput *int64) {
    if *estInput <= 0 {
        // 从 messages 内容长度推算
        // 中英混合: 1 token ≈ 1.67 chars (tokensPerChar = 0.6)
        text := extractRequestText(bodyBytes)
        *estInput = max(1, int64(float64(utf8.RuneCountInString(text)) * tokensPerChar))
    }
    if *estOutput <= 0 {
        *estOutput = defaultEstOutput // 默认 512
    }
}
```

### 4.3 接入点

| 文件 | 修改 |
|------|------|
| `internal/gateway/extract_meta.go` | 新增 extractBlockEstimate + inferTokenEstimates |
| `internal/gateway/v2_chat.go` / `lifecycle.go` | 构建 RouteContext 时填充新字段 |
| `internal/gateway/allocator.go` | RemoteAllocator 映射到 proto 字段 |
| `internal/scheduler/grpc_handler.go` | proto → RouteContext 反向映射 |

---

## 5. Collector 增强

### 5.1 新增采集指标

文件：`internal/scheduler/collector/fastdeploy.go`

| Prometheus 指标 | 字段 | 用途 |
|-----------------|------|------|
| `fastdeploy:free_gpu_block_num` | FreeBlocks | 安全区判断（无需驱逐即可分配） |
| `fastdeploy:max_gpu_block_num` | MaxBlocks | 总容量（归一化基准） |
| `fastdeploy:max_cpu_block_num` | MaxCPUBlocks | CPU cache 容量，用于 session 密度控制 |

### 5.2 Collector 传递

- 构建 `InstanceMetaUpdate` 时填充 `FreeBlocks`、`MaxBlocks`、`RunningCount`、`MaxCPUBlocks`
- 当 `MaxBlocks > 0` 时优先用它作为 `TotalBlocks`（地面真值，比 available/(1-usage) 估算更准）

---

## 6. adaptive_block 策略核心设计

### 6.1 实现接口

```
Policy, BatchSelector, MetaUpdater, DriftAware,
SessionRemover, SessionQuerier, GenerationAware, Resettable, ConfigDescriber
```

### 6.2 核心数据结构

```go
type AdaptiveBlockPolicy struct {
    // Session 亲和（有session_id时启用，无则纯block-fit）
    sessionAssign map[string]abSession   // sessionID → session绑定信息
    instanceLoad  map[string]int64       // instanceID → event-loop内部活跃请求计数
    sessionCount  map[string]int64       // instanceID → session数
    sessionBlocks map[string]int64       // instanceID → 该实例所有session累积block估算

    // 实例元数据 (atomic COW, 无锁读)
    metaPtr atomic.Pointer[abMetaMap]

    // 配置 (略, 见 PolicyConfig 定义)
    // 复用buffer (event-loop单线程，无锁)
    resultsBuf []BatchSelectResult
    heapBuf    []abHeapEntry
}

type abSession struct {
    instanceID string
    reqCount   int64
    lastEpoch  uint64
    accTokens  int64  // 该session累积的token数（用于估算其CPU block占用）
}

type abInstanceMeta struct {
    waitingCount    atomic.Int64
    freeBlocks      atomic.Int64  // 真正空闲（安全区容量）
    availableBlocks atomic.Int64  // 含可驱逐（极限容量）
    maxBlocks       atomic.Int64  // GPU block总容量
    maxCPUBlocks    atomic.Int64  // CPU block总容量（0=未启用CPU cache）
    runningCount    atomic.Int64  // 正在运行的请求数
    localDrift      atomic.Int64  // +1 acquire, -1 release
}
```

### 6.3 Block 需求估算

```go
func (p *AdaptiveBlockPolicy) estimateBlocks(req *domain.RouteContext) int64 {
    input := req.EstInputTokens
    output := req.EstOutputTokens
    if input <= 0 { input = 512 }
    if output <= 0 { output = p.DefaultEstOutput }
    return (input + output + p.BlockSize - 1) / p.BlockSize  // ceil division
}
```

公式来源：FastDeploy 的 `get_required_block_number` = `ceil((input_tokens + dec_token_num) / block_size)`

### 6.4 实例评分与准入门控

```go
func (p *AdaptiveBlockPolicy) scoreInstance(meta *abInstanceMeta, activeLoad, blocksNeeded int64) (score float64, eligible bool) {
    free := meta.freeBlocks.Load()
    maxB := meta.maxBlocks.Load()
    running := meta.runningCount.Load()
    drift := meta.localDrift.Load()
    waiting := meta.waitingCount.Load()

    // 1. Decode reserve: 为已运行请求的decode增长预留block
    decodeReserve := int64(float64(running+drift) * p.DecodeReserveRatio *
        float64(p.DefaultEstOutput) / float64(p.BlockSize))

    // 2. 有效可用 = free - decode预留 - drift估算消耗
    driftBlockConsume := drift * blocksNeeded
    effectiveFree := free - decodeReserve - driftBlockConsume

    // 3. 安全系数（有CPU cache时放宽，因为驱逐到CPU代价小）
    margin := p.SafetyMarginRatio
    if meta.maxCPUBlocks.Load() > 0 {
        margin *= p.CPUCacheRelaxFactor  // e.g. 1.3 * 0.7 = 0.91
    }

    // 4. 准入门控：effectiveFree 必须能装下请求（含安全余量）
    required := int64(float64(blocksNeeded) * margin)
    if effectiveFree < required {
        return 0, false  // INELIGIBLE: block不足
    }

    // 5. 评分 (越低越好)
    normBlockFit := 1.0 - float64(effectiveFree-blocksNeeded)/float64(max(maxB, 1))
    normLoad := float64(activeLoad) / float64(max(p.MaxSessionLoad, 1))
    normWaiting := math.Min(float64(max(0, waiting+drift))/float64(max(p.MaxSessionLoad, 1)), 1.0)

    score = p.WeightBlockFit*normBlockFit + p.WeightLoad*normLoad + p.WeightWaiting*normWaiting
    return score, true
}
```

评分含义：score 越低 = 实例越适合。block 余量越大 → `normBlockFit` 越低 → 自然优先选择。

### 6.5 统一调度逻辑

```go
func (p *AdaptiveBlockPolicy) Select(ctx context.Context, req *domain.RouteContext,
    nodes map[string]*domain.NodeState) (*domain.Instance, error) {

    blocksNeeded := p.estimateBlocks(req)

    // 有 session_id → 走亲和逻辑
    if req.SessionID != "" {
        return p.selectWithAffinity(req, nodes, blocksNeeded)
    }
    // 无 session_id → 纯 block-fit 选择最优实例
    return p.selectBestFit(req, nodes, blocksNeeded)
}
```

无类型分支——session_id 存在与否自然决定是否走亲和路径。

### 6.6 Session 亲和逻辑

```go
func (p *AdaptiveBlockPolicy) selectWithAffinity(req, nodes, blocksNeeded) (*Instance, error) {
    if entry, ok := p.sessionAssign[req.SessionID]; ok {
        // 已有绑定 → 检查绑定实例
        meta := getMeta(entry.instanceID)

        if !p.isEmergency(meta, blocksNeeded) {
            // STAY: 绑定实例有足够block，保持亲和（最小化重调度）
            return nodes[entry.instanceID].Instance, nil
        }
        // MIGRATE: block紧急，选择最优实例并重新绑定
        return p.migrateSession(req, nodes, blocksNeeded)
    }

    // 新 session → CPU 容量检查 + 选择最优实例并绑定
    inst := p.selectBestFitForNewSession(req, nodes, blocksNeeded)
    p.bindSession(req.SessionID, inst.ID, blocksNeeded)
    return inst, nil
}
```

#### 紧急迁移判断

```go
func (p *AdaptiveBlockPolicy) isEmergency(meta *abInstanceMeta, blocksNeeded int64) bool {
    free := meta.freeBlocks.Load()
    maxB := meta.maxBlocks.Load()
    avail := meta.availableBlocks.Load()

    // 条件1: free占比低于紧急阈值（即将触发大量驱逐）
    if maxB > 0 && float64(free)/float64(maxB) < p.EmergencyBlockRatio {
        return true
    }
    // 条件2: 请求block需求超过available（会触发Preemption）
    if avail < blocksNeeded {
        return true
    }
    return false
}
```

**迁移保守性**：只有真正紧急（会导致 Preemption 或 free 比例极低）才迁移。即使另一个实例"更好"，只要当前实例能装下请求就保持亲和。

### 6.7 CPU Cache 容量保护（Session 密度控制）

新 session 绑定时，额外检查目标实例的 CPU cache 是否有足够容量：

```go
func (p *AdaptiveBlockPolicy) canBindNewSession(instanceID string, sessionBlockEstimate int64) bool {
    meta := getMeta(instanceID)
    cpuBlocks := meta.maxCPUBlocks.Load()
    if cpuBlocks == 0 {
        return true  // 未启用CPU cache，不做限制（降级到纯GPU block判断）
    }

    // 该实例上所有session的累积block估算
    currentSessionBlocks := p.sessionBlocks[instanceID]
    newTotal := currentSessionBlocks + sessionBlockEstimate

    // 超过CPU容量的指定比例 → 拒绝绑定，路由到其他实例
    return newTotal < int64(float64(cpuBlocks) * p.CPUCacheMaxRatio)
}
```

效果：
- 长对话 session 因 accTokens 多，`sessionBlockEstimate` 大，自然分散到更多实例
- 短对话 session block 消耗少，可以密集部署
- 一旦实例的 session 总 block 接近 CPU 容量上限，新 session 自动被路由到其他实例
- **彻底避免 CPU cache 淘汰导致的重计算**

Session 累积 token 跟踪：

```go
func (p *AdaptiveBlockPolicy) bindSession(sessionID, instanceID string, blocksNeeded int64) {
    accTokens := blocksNeeded * p.BlockSize
    p.sessionAssign[sessionID] = abSession{
        instanceID: instanceID,
        reqCount:   1,
        lastEpoch:  p.currentEpoch,
        accTokens:  accTokens,
    }
    p.sessionCount[instanceID]++
    p.sessionBlocks[instanceID] += blocksNeeded
}

// 每次同session请求到来时更新累积token
func (p *AdaptiveBlockPolicy) updateSessionTokens(sessionID string, blocksNeeded int64) {
    entry := p.sessionAssign[sessionID]
    oldBlocks := entry.accTokens / p.BlockSize
    entry.accTokens += blocksNeeded * p.BlockSize
    entry.reqCount++
    p.sessionAssign[sessionID] = entry
    p.sessionBlocks[entry.instanceID] += (blocksNeeded - oldBlocks) // 增量更新
}
```

### 6.8 BatchSelect (Heap-based, O(N + K*logN))

复用 min_load 的 inline heap 模式：

1. 构建 min-heap（按 compositeScore 排序），跳过 block 不足 / CPU 容量不足的实例
2. 遍历 batch 请求：
   - 有 session_id 且已绑定 → 走亲和逻辑（不走 heap）
   - 否则取 heap 顶部
3. 每次分配后 drift++，模拟更新 heap entry 的 effectiveFree
4. Session LRU 周期性驱逐（清理 cold session 释放 sessionBlocks 计数）

### 6.9 DriftAware + MetaUpdater

- `TrackAcquire/Release`：localDrift ±1（同现有模式）
- `BatchUpdateInstanceMeta`：atomic COW 模式更新 freeBlocks/availableBlocks/maxBlocks/maxCPUBlocks/runningCount，重置 localDrift（校准）

### 6.10 Feedback 利用（可选增强）

Release 时收到实际 token 消耗（PromptTokens / CompletionTokens），可用 EMA 校准 DefaultEstOutput 使估算越来越准。MVP 阶段可仅记录指标不做自适应。

---

## 7. 集成点

| 位置 | 操作 |
|------|------|
| `internal/scheduler/policy/factory.go` | 注册 `NameAdaptiveBlock = "adaptive_block"` + builder |
| `internal/scheduler/server_step.go` | 无需改动（resolveStepPolicy 通过 Build() 自动支持） |
| `internal/scheduler/grpc_handler.go` | AllocateRequest → RouteContext 新增 2 字段映射 |
| `internal/scheduler/policy/branch_metrics.go` | 新增 branch counters |

### Prometheus 分支指标

| 指标 | 含义 |
|------|------|
| `adaptive_block_session_stay` | 亲和保持（当前实例有足够 block） |
| `adaptive_block_session_migrate` | 紧急迁移（block 不足强制迁移） |
| `adaptive_block_new_session` | 新 session 绑定 |
| `adaptive_block_cpu_capacity_reject` | 因 CPU cache 容量不足拒绝新 session 绑定 |
| `adaptive_block_block_gated` | 因 GPU block 不足被排除 |
| `adaptive_block_no_session_select` | 无 session 纯 block-fit 选择 |

---

## 8. 关键文件清单

| 文件 | 操作 | 说明 |
|------|------|------|
| `internal/domain/models.go` | 修改 | RouteContext +2 字段 |
| `api/proto/router.proto` | 修改 | AllocateRequest +2 字段 |
| `internal/scheduler/policy/interface.go` | 修改 | InstanceMetaUpdate +4 字段 |
| `internal/scheduler/policy/factory.go` | 修改 | PolicyConfig +11 字段 + 注册 |
| `internal/scheduler/policy/adaptive_block.go` | **新建** | 策略主体 |
| `internal/scheduler/collector/fastdeploy.go` | 修改 | +3 指标采集 |
| `internal/scheduler/collector/collector.go` | 修改 | 传递新字段 |
| `internal/scheduler/collector/fetcher.go` | 修改 | InstanceMetrics +3 字段 |
| `internal/gateway/extract_meta.go` | 修改 | +extractBlockEstimate +inferTokenEstimates |
| `internal/gateway/v2_chat.go` | 修改 | RouteContext 填充新字段 |
| `internal/gateway/allocator.go` | 修改 | RemoteAllocator 映射 |
| `internal/scheduler/grpc_handler.go` | 修改 | proto→RouteContext 映射 |

---

## 9. 验证方案

### 9.1 单元测试

文件：`internal/scheduler/policy/adaptive_block_test.go`

- Block 估算准确性（各种 token 组合、auto-infer 兜底）
- 评分公式：归一化、门控阈值、CPU cache 放宽效果
- Session 亲和：STAY 判断、紧急迁移、新 session 绑定
- CPU 容量保护：session 密度超限拒绝、sessionBlocks 累积更新
- 边界：空节点集、全部 block 不足、session LRU 驱逐

### 9.2 并发安全测试

文件：`internal/scheduler/policy/adaptive_block_concurrency_test.go`

- 1 goroutine 模拟 event-loop 串行调用 BatchSelect
- N goroutines 并发调用 BatchUpdateInstanceMeta / RemoveSession / Reset

### 9.3 Benchmark

文件：`internal/scheduler/policy/adaptive_block_benchmark_test.go`

- 10K 节点 + 128 batch（目标 <100us）
- MetaUpdate 10K 实例（目标 <50us）

### 9.4 集成验证

1. hybrid 模式启动，`policy: "adaptive_block"`
2. 模拟长短混合请求，验证长请求被路由到高余量实例
3. 模拟 session 请求，验证亲和保持 + 紧急迁移
4. 模拟 block 逐渐耗尽，验证门控正确拒绝 + 不触发 preemption
5. 模拟 session 密度超限，验证新 session 被路由到其他实例
6. Prometheus 指标验证各分支计数正确

### 9.5 Collector 测试

- 验证 `free_gpu_block_num` / `max_gpu_block_num` / `max_cpu_block_num` 正确解析和传递

---

## 10. 实施顺序

| Step | 描述 | 依赖 |
|------|------|------|
| 1 | 数据模型变更（RouteContext + proto + InstanceMetaUpdate + PolicyConfig） | 无 |
| 2 | Collector 增强（新指标采集+传递） | Step 1 |
| 3 | Gateway 元数据提取（extra_body 解析 + auto-infer） | Step 1 |
| 4 | adaptive_block 策略核心实现 | Step 1 |
| 5 | 集成注册 + gRPC 映射 | Step 3, 4 |
| 6 | 单测 + 并发测试 + Benchmark | Step 4 |
| 7 | 更新 design.md | All |

---

## 11. 设计要点总结

1. **不用 request_type 枚举**：纯靠 est_input_tokens + est_output_tokens + session_id 驱动，上游增加场景无需调度侧适配
2. **统一 block-fit 算法**：长请求自然需要更多 block → 自然被路由到高余量实例；短请求自然可以去任何实例
3. **三级安全区门控**：`free > needed*margin`（安全）→ `available > needed`（驱逐）→ `available < needed`（preemption，绝不允许）
4. **Session 亲和由 session_id 自动触发**：有则亲和（最大化 prefix cache 命中），无则纯 block-fit
5. **迁移极度保守**：只有 emergency（free/max < 阈值 或 available < needed）才迁移
6. **CPU cache 容量保护**：跟踪每实例 session 总 block 消耗，超过 CPU 容量上限时拒绝新 session 绑定，避免 CPU cache 淘汰导致重计算
7. **GPU 安全阈值自适应**：有 CPU cache 时放宽 GPU safety_margin（因为驱逐只是 GPU→CPU swap，不丢数据）
8. **Decode reserve**：为已运行请求的 output 增长预留 block 空间，避免 preemption
