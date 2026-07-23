# CRDT OR-Map 与 Gossip 协议深度解析

> 以 smg-mesh 实现为例，从零开始讲清楚原理、实现和工程取舍。

---

## 一、要解决什么问题？

假设你有 3 台服务器（Gateway A、B、C），每台都维护一份 worker 列表（哪些 GPU 实例可用、是否健康）。

**问题来了**：如果用户往 Gateway A 注册了一个新 worker，B 和 C 怎么知道？

最直觉的方案——用一个中心数据库（比如 Redis、etcd）:

```
Gateway A ──┐
Gateway B ──┼──→ Redis ──→ 所有人读同一份数据
Gateway C ──┘
```

但这引入了单点故障：Redis 挂了，所有 Gateway 都不知道 worker 状态了。

**去中心化方案**：每个节点自己存一份数据，节点之间互相同步。
这就是 smg-mesh 做的事：每个 Gateway 都有完整的数据副本，
通过 **Gossip 协议**互相传播变更，用 **CRDT** 保证最终一致。

```
Gateway A ←──gossip──→ Gateway B
    ↑                      ↑
    └──────gossip──────────┘
              ↕
          Gateway C
```

但这带来一个核心挑战：**两个节点同时修改同一个 key 怎么办？**

---

## 二、Lamport Clock——给操作排序

在分布式系统中，不同机器的物理时钟不完全同步（即使用 NTP，也有毫秒级误差）。
所以我们不能用"墙上钟"来判断哪个操作先发生。

**Lamport Clock**（兰伯特逻辑时钟）是一种逻辑时钟，核心规则只有两条：

### 规则 1：本地操作时，计数器 +1

```
节点 A 的时钟: 0
A 执行 insert("k1", "v1") → 时钟变为 1
A 执行 insert("k2", "v2") → 时钟变为 2
```

### 规则 2：收到远程消息时，取 max(本地, 远程) + 1

```
A 的时钟: 5
B 的时钟: 3
B 收到 A 的消息(timestamp=5) → B 的时钟变为 max(3, 5) + 1 = 6
```

smg-mesh 的实现 (`replica.rs:56-97`):

```rust
// 本地操作：原子 CAS 递增（无锁）
pub fn tick(&self) -> u64 {
    // compare_exchange 循环，保证并发安全
    // current + 1，永不回退
}

// 收到远程消息：取较大值 + 1
pub fn update(&self, remote_timestamp: u64) -> u64 {
    // max(current, remote_timestamp) + 1
}
```

**为什么用 CAS 循环而不是简单的 fetch_add？**
因为 `update()` 需要先比较再修改（`max` 操作），这不是一个原子操作。
CAS（Compare-And-Swap）循环保证了在并发环境下的正确性：
"如果当前值还是我读到的那个，就改成新值；否则重来"。

### Lamport Clock 的局限

它只能保证**因果序**：如果 A happened-before B，那么 `timestamp(A) < timestamp(B)`。
但反过来不成立：`timestamp(A) < timestamp(B)` 不代表 A 真的先发生。
两个没有因果关系的并发操作（比如 A 和 B 同时各自 insert），它们的 timestamp 大小是**无意义的**——
但我们仍然需要一个**确定性的偏序**来解决冲突，所以引入了 `(timestamp, replica_id)` 的组合排序。

---

## 三、ReplicaId——全局唯一身份

每个节点启动时生成一个全局唯一的 ID：

```rust
pub struct ReplicaId(Uuid);

impl ReplicaId {
    pub fn new() -> Self {
        Self(Uuid::now_v7())  // 时间排序的 UUID v7
    }
}
```

为什么用 UUID v7？它的前 48 位是时间戳，后面是随机数。
这意味着**先启动的节点 ReplicaId 更小**，可以作为 tiebreaker：
当两个操作的 Lamport timestamp 相同时，ReplicaId 小的"赢"。

**排序规则**：`(timestamp, replica_id)` — 先比 timestamp，相同时比 replica_id。

这个组合保证了：任意两个操作，排序结果是**全局确定性的**——
不管在哪个节点上比较，结论都一样。这是 CRDT 正确性的基石。

---

## 四、CRDT OR-Map——无冲突复制数据类型

### 4.1 什么是 CRDT？

CRDT（Conflict-free Replicated Data Type）= 无冲突复制数据类型。

核心承诺：**多个副本独立修改后，合并结果一定收敛到相同状态——不需要协调、不需要锁、不需要共识协议。**

传统数据库用锁或 Paxos/Raft 来保证一致性（强一致性，代价是延迟和可用性）。
CRDT 走了另一条路：设计一种特殊的数据结构，让合并操作本身就是无冲突的（最终一致性，代价是收敛延迟）。

### 4.2 OR-Map 是什么？

OR-Map = **Observed-Remove Map**（观察-删除 映射表）。

它是一个 key-value 表，支持三种操作：Insert、Remove、Merge。

**为什么叫"Observed-Remove"？**
在分布式系统中，删除操作很棘手。考虑这个场景：

```
节点 A: insert("k1", "v1")   →  然后把操作同步给 B
节点 B: 收到同步，也有 k1="v1"
节点 A: remove("k1")         →  A 本地删掉了
节点 B: insert("k1", "v2")   →  B 不知道 A 删了，又写了新值
```

如果简单地传播"删除 k1"这个命令，B 的新值也会被误删。

OR-Map 的解决方案：**删除操作只移除它"观察到"的版本**。
每次 insert 都产生一个带 `(timestamp, replica_id)` 的新版本。
remove 操作创建一个**墓碑**（tombstone），只压制时间戳更小的版本。
如果 B 的 insert 时间戳更大，墓碑压不掉它——新值存活。

### 4.3 smg-mesh 的 OR-Map 实现

数据结构 (`crdt.rs:56-64`):

```rust
pub struct CrdtOrMap {
    store: KvStore,                              // 实际数据: key → Vec<u8>
    metadata: Arc<DashMap<String, Vec<ValueMetadata>>>, // 版本元数据
    key_locks: Arc<DashMap<String, Arc<Mutex<()>>>>,    // Per-key 锁
    replica_id: ReplicaId,                       // 本节点身份
    clock: LamportClock,                         // 逻辑时钟
    operation_log: Arc<RwLock<OperationLog>>,     // 操作日志
}
```

四层架构，各司其职：

```
┌─────────────────────────────────┐
│  用户接口: insert / remove / get │  ← 和普通 HashMap 一样用
├─────────────────────────────────┤
│  版本元数据: ValueMetadata       │  ← 记录每个 key 的版本历史
│  (timestamp, replica_id, tombstone) │
├─────────────────────────────────┤
│  操作日志: OperationLog          │  ← 记录所有操作，用于节点间同步
│  [Insert{k,v,ts,rid}, Remove{k,ts,rid}] │
├─────────────────────────────────┤
│  底层存储: KvStore (DashMap)     │  ← 只存活跃数据，不存墓碑
└─────────────────────────────────┘
```

### 4.4 Insert 流程

当用户调用 `map.insert("worker1", data)`:

```
1. 获取 key 锁（细粒度，不阻塞其他 key）
2. Lamport Clock tick → 得到 timestamp（比如 7）
3. 构造 ValueMetadata{timestamp=7, replica_id=A, is_tombstone=false}
4. 与现有版本比较：
   - 如果已有相同 (timestamp, replica_id) → 跳过（幂等）
   - 如果已有更新版本 → 跳过（不覆盖新数据）
   - 否则 → 写入 store + 追加到 operation_log
5. 释放 key 锁
6. 尝试清理 key 锁（如果 key 已删且无人持锁）
```

核心判断逻辑 (`crdt.rs:419-451`):

```rust
fn record_insert_metadata(&self, key: &str, timestamp: u64, replica_id: ReplicaId) -> bool {
    match self.metadata.entry(key.to_string()) {
        Occupied(mut entry) => {
            let versions = entry.get_mut();
            // 幂等检查：完全相同的操作已存在
            if versions.iter().any(|v| v.matches_version(timestamp, replica_id)) {
                return false;  // 重复操作，跳过
            }
            // 有更新版本存在
            if current_winner.is_newer_than(timestamp, replica_id) {
                return false;  // 旧操作，跳过
            }
            // 新版本，写入
            versions.push(new_metadata);
            compact_key_metadata(versions);  // 只保留最新版本
            true
        }
        Vacant(entry) => {
            entry.insert(vec![new_metadata]);  // 首次写入
            true
        }
    }
}
```

**compact_key_metadata** 做了什么？每次操作后只保留一个最新版本。
这是一个空间优化——理论上 OR-Map 需要保留所有版本来正确处理并发删除，
但 smg-mesh 采用 LWW（Last Writer Wins）简化：只保留 `(timestamp, replica_id)` 最大的版本。

### 4.5 Remove 流程

删除操作与 insert 对称，但创建的是**墓碑**（tombstone）:

```
1. 获取 key 锁
2. Lamport Clock tick → timestamp（比如 8）
3. 构造 tombstone: ValueMetadata{timestamp=8, replica_id=A, is_tombstone=true}
4. 与现有版本比较：
   - 如果已有更新版本 → 跳过
   - 否则 → 从 store 删除数据 + 追加 Remove 操作到 log
5. 释放 key 锁
```

关键设计：**store 中不存墓碑**（被删的 key 从 KvStore 移除），
但 **metadata 中保留墓碑**（防止旧的 insert 操作在合并时"复活"数据）。

### 4.6 Merge 流程——核心中的核心

当节点 A 收到节点 B 的 operation log，合并过程：

```rust
pub fn merge(&self, log: &OperationLog) {
    // 第 1 步：收集本地已有的所有操作 ID
    let seen_operations: HashSet<(ReplicaId, u64)> = { ... };

    // 第 2 步：合并日志，找出新操作
    let unseen_operations: Vec<Operation> = {
        local_log.merge(log);       // 去重合并
        local_log.compact();        // 只保留每个 key 的最新操作
        // 过滤出本地没见过的操作
        // 按 (timestamp, replica_id) 排序  ← 确定性顺序！
    };

    // 第 3 步：按确定性顺序逐个应用
    for operation in &unseen_operations {
        self.apply_operation(operation);
    }
}
```

**为什么要排序？**
两个节点收到的操作可能顺序不同（网络传输无序），
但排序后再应用，保证了**无论操作到达顺序如何，最终状态相同**。

**为什么要去重？**
网络可能重传，同一个操作可能被收到多次。
Operation 用 `(replica_id, timestamp)` 作为唯一 ID，去重保证幂等。

### 4.7 Operation Log——操作的记录与传输

```rust
pub enum Operation {
    Insert { key: String, value: Vec<u8>, timestamp: u64, replica_id: ReplicaId },
    Remove { key: String, timestamp: u64, replica_id: ReplicaId },
}

pub struct OperationLog {
    operations: Vec<Operation>,  // 追加写入
}
```

Operation Log 是节点间同步的载体：

```
节点 A                          节点 B
  │                                │
  │  operation_log.to_bytes() ──→  │  A 序列化 log 发送给 B
  │                                │  B 调用 merge(&log) 合并
  │  ←── B 也可以反向同步 ────────  │
```

**compact()** 操作：只保留每个 key 的最新操作，防止 log 无限增长。

```
compact 前: [Insert(k1,v1,ts=1), Insert(k1,v2,ts=3), Remove(k1,ts=5)]
compact 后: [Remove(k1,ts=5)]   ← 只保留最新的
```

### 4.8 Per-Key Lock 的设计

smg-mesh 使用**细粒度的 per-key 锁**而非全局锁：

```rust
key_locks: Arc<DashMap<String, Arc<Mutex<()>>>>
```

好处：修改 key "worker1" 时不阻塞对 "worker2" 的操作。

但锁也需要清理（否则会泄漏）。`try_cleanup_key_lock` 在操作完成后尝试清理：

```rust
fn try_cleanup_key_lock(&self, key: &str, key_lock: &Arc<Mutex<()>>) {
    // 只有当：
    //   1. key 在 store 中不存在（已删除）
    //   2. metadata 显示 key 是墓碑或未知状态
    //   3. 锁的引用计数 <= 2（只有 DashMap 和当前调用者持有）
    //   4. 锁没被别人持有
    // 才移除锁
}
```

这四个条件保证了不会在"别人正在用锁"的时候把锁删掉。

---

## 五、Gossip 协议——去中心化的消息传播

### 5.1 类比：办公室八卦

Gossip 协议的名字来源于"八卦/流言"的传播方式：

```
第 1 秒：小明知道一个新消息，随机告诉了小红
第 2 秒：小明告诉了小刚，小红告诉了小李
第 3 秒：4 个人各自再告诉 1 个人 → 8 人知道
...
第 N 秒：几乎所有人都知道了
```

这就是**指数传播**：每轮每个知道消息的人告诉 1 个随机的人，
O(log N) 轮后全员知晓。不需要中心广播站，不需要知道所有人的名单。

### 5.2 smg-mesh 的 Gossip 实现

核心循环 (`controller.rs:69-175`):

```
每秒执行一次:
  1. 从已知存活节点中随机选 1 个 peer
  2. 向 peer 发送 Ping + 本地 StateSync（携带已知的所有节点状态）
  3. peer 回复自己知道的节点状态
  4. 合并双方的状态信息
  5. 如果 peer 存活，建立 sync_stream 长连接
```

这个循环看起来简单，但蕴含了精巧的设计。

### 5.3 SWIM 故障检测

SWIM（Scalable Weakly-consistent Infection-style process group Membership）
是一种经典的分布式故障检测协议。smg-mesh 在 Gossip 中实现了 SWIM：

```
             直接 Ping
Gateway A ──────────────→ Gateway B
             ✓ 成功 → B 存活

             ✗ 失败 → 不急着判死，委托别人帮忙探测
                │
                ▼
Gateway A 随机选 3 个节点，请求它们代为 Ping B (PingReq)
                │
  ┌─────────────┼─────────────┐
  ▼             ▼             ▼
Gateway C    Gateway D    Gateway E
  │             │             │
  └───Ping B────┴───Ping B────┘
         │
         ├─ 任一成功 → B 存活（可能只是 A→B 的网络断了）
         │
         └─ 全部失败 → B 标记为 Suspected
                           │
                           ├─ 下一轮仍不通 → B 标记为 Down
                           └─ 下一轮恢复 → B 标记为 Alive
```

**为什么不直接判死？**
网络是不可靠的。A 到 B 不通，可能只是 A 和 B 之间的链路问题，
B 对其他节点可能完全正常。PingReq 通过第三方验证，大幅降低误判率。

状态转换：`Alive → Suspected → Down`

### 5.4 Sync Stream——增量状态同步

Gossip Ping 只传播**成员关系**（谁活着谁挂了）。
实际的业务数据（worker 状态、策略配置等）通过 **sync_stream** 同步：

```
Gateway A ←══════════════════════════→ Gateway B
              gRPC 双向流（长连接）
              每秒发送 IncrementalUpdate
```

IncrementalUpdate 包含最近变更的数据，比如：

```
WorkerStateUpdate { worker_id: "w1", health: true, load: 0.5, version: 3 }
PolicyStateUpdate { model_id: "m1", policy_type: "cache_aware", version: 2 }
TreeStateUpdate   { model_id: "m1", operations: [...], version: 5 }
```

接收端用**版本号 LWW**（Last Writer Wins）决定是否接受：

```rust
// apply_remote_worker_state 核心逻辑
if remote_state.version > local_state.version {
    // 远程更新，接受
    local_state = remote_state;
} else {
    // 远程更旧或相同，跳过
}
```

**连接去重**：如果 A 和 B 都想建 sync_stream，只需要一条连接。
smg-mesh 用字典序决定谁主动连：`if self_name < peer_name { 我来连 }`。

---

## 六、在 smg-mesh 中的具体应用

### 6.1 五类 State Store

smg-mesh 用 CRDT OR-Map 承载了 5 类状态：

```
┌──────────────────────────────────────────────────────┐
│                   StateStores                         │
│                                                       │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────┐  │
│  │ Membership   │  │   Worker     │  │  Policy    │  │
│  │ Store        │  │   Store      │  │  Store     │  │
│  │ 节点成员关系  │  │ GPU 实例状态  │  │ 路由策略   │  │
│  │ (name,addr,  │  │ (id,url,     │  │ (model_id, │  │
│  │  status)     │  │  health,load)│  │  type,cfg) │  │
│  └──────┬───────┘  └──────┬───────┘  └─────┬──────┘  │
│         │                 │                │          │
│         └─────────────────┼────────────────┘          │
│                           │                           │
│                    CrdtOrMap (共用)                    │
│                                                       │
│  ┌──────────────┐  ┌──────────────────────────────┐   │
│  │   App        │  │       RateLimit              │   │
│  │   Store      │  │       Store                  │   │
│  │ 应用配置     │  │  限流计数（分片 CRDT）         │   │
│  └──────────────┘  └──────────────────────────────┘   │
└──────────────────────────────────────────────────────┘
```

### 6.2 Worker 健康同步

当 Gateway A 检测到某个 GPU 实例 "worker1" 不健康：

```
时间线:
  t=0  Gateway A 本地检测到 worker1 health=false
  t=0  A 调用 sync_worker_state("worker1", health=false, version=5)
       → CRDT OR-Map insert，Lamport timestamp 递增
       → 追加到 operation_log

  t≈1s  A 通过 sync_stream 发送 IncrementalUpdate 给 B
  t≈1s  B 收到，调用 apply_remote_worker_state
       → 检查 version: 5 > B 本地的 version: 3 → 接受更新
       → B 也知道 worker1 不健康了

  t≈2s  B 通过 sync_stream 转发给 C（如果 B 和 C 有连接）
  t≈2s  C 也知道了
```

整个过程约 1-2 秒，所有 Gateway 达成一致。

### 6.3 全局 Rate Limit

普通限流器的问题：3 个 Gateway 各配 100 QPS，总量就是 300 QPS 而非 100 QPS。

smg-mesh 的分片方案：

```
          Gateway A               Gateway B               Gateway C
    ┌───────────────────┐  ┌───────────────────┐  ┌───────────────────┐
    │ 我的 shard:        │  │ 我的 shard:        │  │ 我的 shard:        │
    │ "global::actor:A"  │  │ "global::actor:B"  │  │ "global::actor:C"  │
    │ count = 35         │  │ count = 40         │  │ count = 25         │
    └────────┬──────────┘  └────────┬──────────┘  └────────┬──────────┘
             │                      │                      │
             └──────────────────────┼──────────────────────┘
                                    │
                             gossip 同步各 shard
                                    │
                                    ▼
                     aggregate_counter("global") =
                     sum(shard_A=35, shard_B=40, shard_C=25) = 100
                     → 达到限制，拒绝新请求
```

每个节点只递增自己的 shard（无冲突），查询时聚合所有 shard。
Consistent Hash Ring 决定哪些 key 由哪个节点"拥有"，节点故障时自动转移 ownership。

### 6.4 冷启动状态机

新节点加入集群时，不能立即开始服务（状态还没同步完）：

```
NotReady → Joining → SnapshotPull → Converging → Ready
  │           │           │             │           │
  启动        连接到       从 peer 拉     等待状态     可以接受
              集群        全量快照       不再变化     流量了
                          (60s 超时)    (10s 窗口
                                        5次稳定)
```

**Converging 检测**用状态哈希实现：

```rust
fn calculate_state_hash(&self) -> u64 {
    // 对所有 store 的 len() 做哈希
    membership.len().hash(&mut hasher);
    worker.len().hash(&mut hasher);
    policy.len().hash(&mut hasher);
    app.len().hash(&mut hasher);
}
```

如果连续 5 次检查（跨越 10 秒窗口），哈希值都没变 → 状态已收敛 → 切换到 Ready。

---

## 七、一个完整的例子

三个 Gateway 的场景，从空集群到正常服务：

```
t=0    Gateway A 启动 → 状态: NotReady
       Gateway B 启动 → 状态: NotReady
       Gateway C 启动 → 状态: NotReady

t=1    A 开始 gossip，发现 B → A 和 B 互相建立 sync_stream
       A: NotReady → Joining → SnapshotPull

t=2    C 通过 gossip 发现 A → 建立 sync_stream
       A、B、C 开始互相同步空状态

t=3    外部注册 worker1 到 A：
       A: insert("worker1", {url: "gpu-01:8000", health: true})
       → Lamport clock tick → timestamp=1
       → operation_log: [Insert("worker1", ..., ts=1, replica=A)]

t=4    A 通过 sync_stream 把 operation_log 发给 B
       B 调用 merge(log_A)：
       → 发现 Insert("worker1"...) 是 unseen 操作
       → apply_insert → B 也有了 worker1

t=5    B 通过 sync_stream 转发给 C → C 也有了 worker1

t=6    外部同时注册 worker2 到 B 和 worker3 到 C：
       B: insert("worker2", ..., ts=2, replica=B)
       C: insert("worker3", ..., ts=1, replica=C)    ← C 的时钟独立

t=7    B 和 C 互相同步 → 各自 merge 对方的 log
       → B 有 worker1,2,3
       → C 有 worker1,2,3
       → A 还缺 worker3（等下一轮同步）

t=8    A 收到 C 的 sync → merge → A 也有 worker1,2,3
       所有节点状态一致 ✓

t=10   A 的 ConvergenceTracker 检测到连续 5 次哈希不变
       A: Converging → Ready → 开始接受流量

--- 故障场景 ---

t=100  worker1 的 GPU 出故障
       A 检测到 worker1 unhealthy
       A: sync_worker_state("worker1", health=false, version=5)

t=101  B 和 C 通过 sync_stream 收到更新
       → apply_remote_worker_state: version=5 > 本地 version=4 → 接受
       → 所有 Gateway 停止向 worker1 分配流量

--- 并发冲突场景 ---

t=200  A 和 B 同时修改 worker1 的 load:
       A: insert("worker1", {load: 0.8}, ts=50, replica=A)
       B: insert("worker1", {load: 0.3}, ts=50, replica=B)
       → 两个操作 timestamp 相同！
       → 比较 replica_id: A 的 UUID vs B 的 UUID
       → UUID 大的赢（确定性，两边结论一致）
       → merge 后两个节点的 worker1 状态完全相同 ✓
```

---

## 八、优点

### 8.1 高可用性

- **无单点故障**：任何一个节点挂掉，其他节点照常工作
- **分区容忍**：网络分区时，两边各自独立服务；恢复后 merge 自动合并
- **自愈**：新节点加入自动拉取全量状态；故障节点恢复后自动追赶

### 8.2 高性能

- **零协调写入**：每个节点本地写完就算成功，不需要等其他节点确认
- **Per-key 锁**：不同 key 的操作完全并行
- **DashMap**：底层用分片的 concurrent hash map，读写接近无锁
- **CAS 循环**：Lamport Clock 用 atomic CAS，比 Mutex 开销小一个数量级

### 8.3 正确性保证

- **最终一致性**：数学上可证明，只要网络最终连通，所有节点状态收敛
- **幂等合并**：同一个 operation log 合并多次 = 合并一次
- **交换律**：先合并 A 再合并 B = 先合并 B 再合并 A
- **结合律**：`merge(merge(A,B), C) = merge(A, merge(B,C))`

这三个性质（幂等、交换、结合）是 CRDT 正确性的数学基础。

### 8.4 运维友好

- **无需外部依赖**：不需要 Redis、etcd、ZooKeeper
- **渐进部署**：新节点加入不影响现有节点
- **自动故障检测**：SWIM 协议自动发现不可达节点

---

## 九、局限性

### 9.1 最终一致性 ≠ 强一致性

这是最根本的局限。考虑一个场景：

```
t=0  A: worker1.load = 0.3  (还没同步给 B)
t=0  B: worker1.load = 0.3  (旧值)
t=0  用户请求到 A → 看到 load 低 → 分配给 worker1
t=0  用户请求到 B → 也看到 load 低 → 也分配给 worker1
     → worker1 被分配了 2 个请求，但两个 Gateway 都以为只有 1 个
```

在同步间隔（约 1 秒）内，不同 Gateway 看到的是不同的快照。
对于负载均衡来说，这意味着**短时间内可能出现负载不均**。

**smg-mesh 的现状更严重**：model_gateway 的负载字段写死 `0.0`，
即使框架层支持同步，应用层也没用起来。

### 9.2 墓碑无法完全避免"复活"

虽然 compact 优化了空间，但也引入了风险：

```
t=0  A: insert("k1", v1, ts=1) → compact 后只保留 ts=1
t=1  A: remove("k1", ts=5)     → compact 后只保留 tombstone ts=5
t=2  compact 后又一轮 compact → 如果只保留最新操作
t=3  一个很旧的 B 终于连上来，带着 insert("k1", v1, ts=3)
     → ts=3 < ts=5，被 tombstone 压住 ✓ 没问题

但如果:
t=4  进一步 compact → tombstone 也被清除了（log 只保留最新操作）
t=5  另一个更旧的 C 带着 insert("k1", v1, ts=2) 连上来
     → 本地没有 tombstone 了 → insert 成功 → 数据"复活"了！
```

smg-mesh 通过以下方式缓解（但不完全解决）：

- metadata 中保留 tombstone（不随 operation log compact 清除）
- 冷启动时全量快照拉取，减少旧操作到达的概率

### 9.3 存储开销

- 每个 key 都有 metadata（timestamp + replica_id + tombstone）
- Operation log 持续增长，需要定期 compact
- 所有节点存全量数据（无分片），内存用量 = 数据总量 × 节点数

对于 smg-mesh 的场景（最多几千个 worker + 几十条策略），完全可以接受。
但如果数据量达到百万级，需要引入分片。

### 9.4 Clock Skew 风险

Lamport Clock 本身不受物理时钟影响，但 smg-mesh 的 `ConvergenceTracker`
用了 `Instant::now()`（单调时钟）来判断收敛窗口。
如果系统极端繁忙导致 Instant 被延迟读取，可能影响收敛判断的准确性。

### 9.5 LWW 的丢失更新

LWW（Last Writer Wins）意味着并发写入时，timestamp 小的那个更新被静默丢弃：

```
A: insert("k1", "Alice的修改", ts=5)
B: insert("k1", "Bob的修改", ts=6)
→ merge 后 k1 = "Bob的修改"
→ Alice 的修改永久丢失，没有任何提示
```

对于 smg-mesh 的场景（worker 状态是事实数据，不是用户输入），
LWW 是合理的——更新的数据就是更准确的数据。
但对于需要保留所有修改的场景（比如文档协同编辑），LWW 不适用。

### 9.6 Gossip 的传播延迟

O(log N) 轮才能全员知晓，每轮 1 秒。10 个节点约 3-4 秒，100 个节点约 6-7 秒。
对于实时性要求极高的场景（比如"必须在 100ms 内摘除故障节点"），Gossip 不够快。

smg-mesh 用 sync_stream 长连接（而非纯 gossip）来加速，
直连的节点之间延迟约 1 秒。但间接传播（A→B→C）仍有累积延迟。

---

## 十、与 OneRouter 中心化架构的对比

| 维度 | smg-mesh (CRDT + Gossip) | OneRouter (中心 Scheduler) |
| ---- | ----------------------- | ------------------------- |
| 一致性 | 最终一致（1-2s 延迟） | 强一致（event-loop 串行化） |
| 可用性 | 任何节点挂都能继续服务 | Scheduler 挂 = 全局不可用 |
| 负载精度 | 近似（各 Gateway 视角有差异） | 精确（Scheduler 全局视图） |
| 实现复杂度 | 高（CRDT + Gossip + 冷启动 + 分区检测） | 中（event-loop + gRPC） |
| 适用场景 | 在线推理（高 QPS、容忍近似） | RL 训练（低 QPS、需要精确） |
| 扩展性 | 天然水平扩展 | 需要 HA 方案 |

两种方案不是非此即彼。最佳实践是**中心化核心 + 去中心化降级**：
正常时 Scheduler 精确调度，Scheduler 故障时 Gateway 降级为 Gossip + ConsistentHash。

---

## 附录：smg-mesh 源码导读路径

如果你想深入阅读源码，建议按以下顺序：

```
1. replica.rs          → 理解 ReplicaId 和 LamportClock（最简单）
2. kv_store.rs         → 理解底层 KV 存储（DashMap 包装）
3. operation.rs        → 理解 Operation 和 OperationLog
4. crdt.rs             → 理解 CrdtOrMap（核心，需要反复读）
5. stores.rs           → 理解 5 类 StateStore 如何包装 CrdtOrMap
6. sync.rs             → 理解应用层如何使用 CRDT store
7. controller.rs       → 理解 Gossip 循环和 SWIM 故障检测
8. node_state_machine.rs → 理解冷启动状态机
9. partition.rs        → 理解网络分区检测
```

所有文件在 `smg/crates/mesh/src/` 下。
