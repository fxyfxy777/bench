# Slime 部署架构与数据链路详解

本文档详细阐述 slime 在单机、多机场景下的部署架构，以及 rollout 推理转发的完整数据链路。

---

## 目录

- [Slime 部署架构与数据链路详解](#slime-部署架构与数据链路详解)
  - [目录](#目录)
  - [1. 整体架构概览](#1-整体架构概览)
  - [2. 核心组件](#2-核心组件)
    - [2.1 训练入口](#21-训练入口)
    - [2.2 Ray Placement Group -- GPU 资源分配](#22-ray-placement-group----gpu-资源分配)
    - [2.3 Training Actor -- Megatron 训练](#23-training-actor----megatron-训练)
    - [2.4 RolloutManager -- 推理编排](#24-rolloutmanager----推理编排)
    - [2.5 SGLangEngine -- 推理引擎](#25-sglangengine----推理引擎)
    - [2.6 sglang\_router -- 负载均衡](#26-sglang_router----负载均衡)
  - [3. 部署模式](#3-部署模式)
    - [3.1 单机 Colocate 部署](#31-单机-colocate-部署)
    - [3.2 多机 Non-Colocate 部署](#32-多机-non-colocate-部署)
    - [3.3 多机 Colocate 部署](#33-多机-colocate-部署)
    - [3.4 部署关键参数一览](#34-部署关键参数一览)
  - [4. 数据链路](#4-数据链路)
    - [4.1 同步训练主循环](#41-同步训练主循环)
    - [4.2 异步训练主循环](#42-异步训练主循环)
    - [4.3 Rollout 推理转发链路](#43-rollout-推理转发链路)
      - [4.3.1 全链路概览](#431-全链路概览)
      - [4.3.2 HTTP 推理请求详解](#432-http-推理请求详解)
      - [4.3.3 HTTP 客户端层](#433-http-客户端层)
      - [4.3.4 并发控制与过采样](#434-并发控制与过采样)
      - [4.3.5 Sample 数据结构](#435-sample-数据结构)
      - [4.3.6 训练数据转换](#436-训练数据转换)
    - [4.4 权重同步链路](#44-权重同步链路)
      - [4.4.1 NCCL 分布式同步（Non-Colocate 主路径）](#441-nccl-分布式同步non-colocate-主路径)
      - [4.4.2 Tensor IPC 同步（Colocate 主路径）](#442-tensor-ipc-同步colocate-主路径)
      - [4.4.3 两种路径对比](#443-两种路径对比)
    - [4.5 内存 offload/onload 链路](#45-内存-offloadonload-链路)
  - [5. 容错机制](#5-容错机制)
  - [6. 关键文件索引](#6-关键文件索引)

---

## 1. 整体架构概览

Slime 是一个基于 **Ray** 编排的 RL 训练框架。训练侧使用 **Megatron**，推理侧使用 **SGLang**，通过 **sglang_router**（即 sgl-model-gateway 的 Python binding，Rust 实现）做推理请求的负载均衡。

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           Ray Cluster                                    │
│                                                                          │
│  ┌─────────────────────┐                                                 │
│  │     train.py         │  训练主循环                                     │
│  │  (Ray Driver)        │  for rollout_id in range(num_rollout):         │
│  │                      │    generate -> train -> update_weights          │
│  └──────────┬───────────┘                                                │
│             │                                                            │
│     ┌───────┴────────┐                                                   │
│     │                │                                                   │
│     ▼                ▼                                                   │
│  ┌──────────┐  ┌──────────────┐                                          │
│  │ RayTrain │  │ Rollout      │                                          │
│  │ Group    │  │ Manager      │─── HTTP POST /generate ──┐               │
│  │(Megatron)│  │ (Ray Actor)  │                          │               │
│  └────┬─────┘  └──────────────┘                          ▼               │
│       │                                          ┌──────────────┐        │
│       │         weight sync                      │ sglang_router│        │
│       │         (NCCL / IPC)                     │ (Rust daemon)│        │
│       │              │                           └──────┬───────┘        │
│       │              │                      ┌───────────┼───────────┐    │
│       │              │                      ▼           ▼           ▼    │
│       │              │                  ┌────────┐ ┌────────┐ ┌────────┐ │
│       └──────────────┼─────────────────►│SGLang  │ │SGLang  │ │SGLang  │ │
│                      └─────────────────►│Engine 0│ │Engine 1│ │Engine 2│ │
│                                         └────────┘ └────────┘ └────────┘ │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## 2. 核心组件

### 2.1 训练入口

Slime 提供两个训练入口：

| 入口 | 文件 | 特点 |
|------|------|------|
| 同步训练 | `train.py` | rollout 与 training 串行，支持 colocate |
| 异步训练 | `train_async.py` | rollout 与 training 流水线重叠，**不支持** colocate |

**同步训练** (`train.py:9-105`) 的核心流程：

```python
# train.py:12-24
pgs = create_placement_groups(args)                          # 分配 GPU
rollout_manager = create_rollout_manager(args, pgs["rollout"])  # 启动 SGLang 引擎
actor_model, critic_model = create_training_models(args, pgs, rollout_manager)

# train.py:73-103  主循环
for rollout_id in range(start_rollout_id, num_rollout):
    rollout_data_ref = ray.get(rollout_manager.generate.remote(rollout_id))  # ① 推理
    actor_model.async_train(rollout_id, rollout_data_ref)                    # ② 训练
    actor_model.update_weights()                                              # ③ 权重同步
```

**异步训练** (`train_async.py:10-85`) 将下一轮 rollout 与当前训练重叠：

```python
# train_async.py:11
assert not args.colocate  # 异步模式不支持 colocate

# train_async.py:36-44
rollout_data_next_future = rollout_manager.generate.remote(rollout_id)   # 非阻塞
# ... 在等待期间进行当前轮训练 ...
rollout_data_ref = ray.get(rollout_data_next_future)                     # 需要时再阻塞
```

### 2.2 Ray Placement Group -- GPU 资源分配

所有 GPU 通过一个 Ray Placement Group 统一分配（`slime/ray/placement_group.py`）。

**核心函数**：`create_placement_groups(args)`（`placement_group.py:79`）

```python
# placement_group.py:41-44  创建 placement group
bundles = [{"GPU": 1, "CPU": 1} for _ in range(num_gpus)]
pg = placement_group(bundles, strategy="PACK")
```

所有 bundle 按 `(node_ip, gpu_id)` 排序以确保确定性分配（`placement_group.py:20`），然后按功能切分为三段：

```
Colocate 模式:
  ┌──────────────────────────────────┐
  │ actor GPUs (+ critic GPUs)       │ ← training 和 rollout 共享
  └──────────────────────────────────┘
  total = actor_num_nodes * actor_num_gpus_per_node

Non-Colocate 模式:
  ┌──────────────────────┬───────────────────┐
  │ actor (+ critic) GPUs│  rollout GPUs      │
  └──────────────────────┴───────────────────┘
  total = actor_GPUs + rollout_num_gpus
```

**Colocate 模式的资源分数**（`placement_group.py:128`, `rollout.py:99`）：
- Training actor: `num_gpus_per_actor = 0.4`
- SGLang engine: `num_gpus = 0.2`
- 允许同一物理 GPU 上同时调度 training actor 和 inference engine

### 2.3 Training Actor -- Megatron 训练

**相关文件**：
- `slime/ray/actor_group.py` -- `RayTrainGroup` 管理训练 actor 组
- `slime/ray/train_actor.py` -- `TrainRayActor` 基类

**初始化流程**（`actor_group.py:46-98`）：

```python
# actor_group.py:46-65  为每个 GPU 创建一个 Ray actor
for rank in range(world_size):
    actor = MegatronTrainRayActor.options(
        num_gpus=num_gpus_per_actor,   # 0.4 (colocate) 或 1 (non-colocate)
        scheduling_strategy=PlacementGroupSchedulingStrategy(...)
    ).remote()
```

每个 training actor 内部（`train_actor.py:29-50`）：
1. 设置 `MASTER_ADDR`, `MASTER_PORT`, `WORLD_SIZE`, `RANK`, `LOCAL_RANK` 环境变量
2. Rank 0 自动发现空闲端口
3. 调用 `torch.distributed.init_process_group(backend="nccl")` 初始化分布式通信
4. 初始化 Megatron 的模型并行组（TP, PP, CP, EP, DP）

### 2.4 RolloutManager -- 推理编排

**文件**：`slime/ray/rollout.py` -- `RolloutManager`（`rollout.py:349`），核心 Ray actor。

**初始化**（`rollout.py:352-392`）：
1. 加载数据源（`RolloutDataSource`，`rollout.py:358`）
2. 加载 rollout 函数（默认 `sglang_rollout.generate_rollout`，`rollout.py:362`）
3. 初始化全局 HTTP 客户端（`init_http_client(args)`，`rollout.py:375`）
4. 启动 SGLang 引擎和 router（`start_rollout_servers(args, pg)`，`rollout.py:376`）
5. 启动健康监控线程（`RolloutHealthMonitor`，`rollout.py:383`）

**关键数据结构**：

```
RolloutManager                        # Ray actor, 无 GPU
  └─ dict[str, RolloutServer]         # 每个 model 一个
       └─ RolloutServer               # rollout.py:210
            ├─ router_ip, router_port  # 该 model 的 router 地址
            └─ list[ServerGroup]       # 一个或多个引擎组
                 └─ ServerGroup        # rollout.py:38
                      ├─ all_engines: list[SGLangEngine]
                      ├─ worker_type: regular/prefill/decode/encoder/placeholder
                      ├─ num_gpus_per_engine  # 引擎 TP 大小
                      └─ needs_offload        # 是否需要 offload
```

**推理调度**（`rollout.py:478-491`）：

```python
# rollout.py:478
def generate(self, rollout_id):
    data, metrics = self._get_rollout_data(rollout_id)    # 调用 rollout 函数
    data = self._convert_samples_to_train_data(data)       # Sample -> RolloutBatch
    data = self._split_train_data_by_dp(data, dp_size)     # 按 DP rank 均衡切分
    return data
```

### 2.5 SGLangEngine -- 推理引擎

**文件**：`slime/backends/sglang_utils/sglang_engine.py`

`SGLangEngine`（`sglang_engine.py:97`）是一个 Ray actor，每个实例封装一个 SGLang HTTP 服务器进程。

**启动流程**（`sglang_engine.py:114-214`）：

```
SGLangEngine.init()
  ├─ _compute_server_args()          # 计算 ServerArgs（sglang_engine.py:496）
  │    ├─ tp_size = num_gpus_per_engine / pp_size
  │    ├─ base_gpu_id = 由 placement group 决定
  │    ├─ enable_memory_saver = True (if offload)
  │    └─ 应用 sglang_overrides（来自 YAML 配置）
  │
  ├─ _init_normal()                  # 启动本地引擎（sglang_engine.py:190）
  │    ├─ launch_server_process()    # multiprocessing.Process 启动 SGLang HTTP 服务
  │    │    └─ target: sglang.srt.entrypoints.http_server.launch_server
  │    │    └─ 轮询 /health_generate 等待就绪（sglang_engine.py:53-92）
  │    └─ POST /workers 注册到 router（sglang_engine.py:197-214）
  │
  └─ _init_external()               # 连接外部引擎（sglang_engine.py:166）
       └─ 仅验证已有 SGLang 实例
```

**多节点引擎**：当 `num_gpus_per_engine > num_gpus_per_node` 时，一个引擎跨多个节点。此时创建多个 Ray actor，但只有 `node_rank=0` 的 actor 运行 HTTP 服务并注册 router，其他节点作为 worker。

### 2.6 sglang_router -- 负载均衡

**本质**：`sglang_router` Python 包是 `sgl-model-gateway`（Rust 实现）的 PyO3 binding。

**集成方式**（`rollout.py:907-958`）：

```python
# rollout.py:925-953
from sglang_router.launch_router import RouterArgs

router_args = RouterArgs.from_cli_args(args, use_router_prefix=True)
router_args.host = router_ip
router_args.port = router_port
router_args.disable_health_check = True      # slime 自己做健康检查
router_args.disable_circuit_breaker = True   # PD 模式下 RDMA 超时是暂态的

# 以 daemon 子进程启动 Rust router
process = multiprocessing.Process(target=run_router, args=(router_args,))
process.daemon = True
process.start()
```

`run_router()`（`http_utils.py:117-121`）在子进程内调用 Rust 代码：

```python
# http_utils.py:117-121
def run_router(args):
    from sglang_router.launch_router import launch_router
    router = launch_router(args)  # Rust tokio runtime 接管，阻塞运行
```

**调用链**：Python `launch_router()` → `Router.from_args(RouterArgs)` → `_Router(**kwargs)`（PyO3 Rust binding）→ `router.start()`（tokio runtime，阻塞）。

**负载均衡策略**（`sglang_router/router_args.py:29`）：
- `session_aware`（默认）：基于 KV Cache 前缀匹配度路由，最大化 cache 命中
- `random`, `round_robin`, `power_of_two`, `bucket`, `consistent_hashing`, `prefix_hash`

**每个 model 一个 router 进程**（`rollout.py:1008-1012`），是单点但可接受：
- Rust + tokio 高性能，微秒级请求转发，不是吞吐瓶颈
- 生命周期绑定 RolloutManager daemon 进程
- RL 训练场景（小时~天级作业）对可用性要求低于在线服务

---

## 3. 部署模式

### 3.1 单机 Colocate 部署

**典型场景**：小模型（4B ~ 30B），单机 8 卡

**参考脚本**：`scripts/run-qwen3-4B.sh`

```bash
# 启动 Ray 单节点
ray start --head --node-ip-address 127.0.0.1 --num-gpus 8

# 关键参数
--actor-num-nodes 1
--actor-num-gpus-per-node 8
--colocate                           # 训练和推理共享 GPU
--rollout-num-gpus-per-engine 2      # 每个 SGLang 引擎 2 卡（TP=2）
--tensor-model-parallel-size 2       # Megatron TP=2
```

**进程与 GPU 分布图**：

```
Node 0 (8 GPUs)
┌─────────────────────────────────────────────────────────────────┐
│ GPU 0        GPU 1        GPU 2        GPU 3                    │
│ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐            │
│ │Train(0.4)│ │Train(0.4)│ │Train(0.4)│ │Train(0.4)│            │
│ │SGLang    │ │SGLang    │ │SGLang    │ │SGLang    │            │
│ │Eng0(0.2) │ │Eng0(0.2) │ │Eng1(0.2) │ │Eng1(0.2) │            │
│ └──────────┘ └──────────┘ └──────────┘ └──────────┘            │
│                                                                 │
│ GPU 4        GPU 5        GPU 6        GPU 7                    │
│ ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐            │
│ │Train(0.4)│ │Train(0.4)│ │Train(0.4)│ │Train(0.4)│            │
│ │SGLang    │ │SGLang    │ │SGLang    │ │SGLang    │            │
│ │Eng2(0.2) │ │Eng2(0.2) │ │Eng3(0.2) │ │Eng3(0.2) │            │
│ └──────────┘ └──────────┘ └──────────┘ └──────────┘            │
│                                                                 │
│ RolloutManager (CPU)    sglang_router (Rust daemon)             │
└─────────────────────────────────────────────────────────────────┘

8 Training Actors (Megatron, TP=2, DP=4)
4 SGLang Engines (TP=2 each)
1 sglang_router
1 RolloutManager
```

**内存时分复用**：训练和推理不会同时使用 GPU 显存，通过 offload 机制交替占用。

```
时间线:
  ┌─offload train─┐  ┌─推理(SGLang)──┐  ┌─offload rollout─┐  ┌─训练(Megatron)─┐
  │释放 Megatron   │  │SGLang onload  │  │释放 SGLang       │  │Megatron onload │
  │权重到 CPU      │  │权重+KV Cache  │  │权重+KV Cache    │  │权重到 GPU      │
  │               │  │执行推理       │  │到 CPU           │  │执行 GRPO 训练  │
  └───────────────┘  └──────────────┘  └────────────────┘  └───────────────┘
```

### 3.2 多机 Non-Colocate 部署

**典型场景**：大模型（235B MoE 等），训练和推理各占一半节点

**参考脚本**：`scripts/run-qwen3-235B-A22B.sh`

```bash
# 启动 Ray 集群：head + workers via SSH
ray start --head --node-ip-address ${MASTER_ADDR} --num-gpus 8
for WORKER_IP in $(awk '{print $1}' /root/mpi_rack_hostfile); do
  ssh root@"${WORKER_IP}" \
    "ray start --address=${MASTER_ADDR}:6379 --num-gpus 8"
done

# 关键参数
--actor-num-nodes 8                  # 8 节点训练
--actor-num-gpus-per-node 8
--rollout-num-gpus 64                # 另外 64 卡推理
--rollout-num-gpus-per-engine 32     # 每个引擎 32 卡（TP/EP 跨 4 节点）
# 不加 --colocate
```

**进程与 GPU 分布图**：

```
                    ┌──── 训练节点（64 GPUs）────┐  ┌──── 推理节点（64 GPUs）────┐
                    │                           │  │                           │
Node 0 (Head)       │  8 Training Actors        │  │                           │
Node 1              │  8 Training Actors        │  │                           │
Node 2              │  8 Training Actors        │  │                           │
Node 3              │  8 Training Actors        │  │                           │
Node 4              │  8 Training Actors        │  │                           │
Node 5              │  8 Training Actors        │  │                           │
Node 6              │  8 Training Actors        │  │                           │
Node 7              │  8 Training Actors        │  │                           │
                    │                           │  │                           │
Node 8              │                           │  │  SGLang Engine 0          │
Node 9              │                           │  │  (32 GPUs, TP跨4节点)     │
Node 10             │                           │  │                           │
Node 11             │                           │  │                           │
                    │                           │  │                           │
Node 12             │                           │  │  SGLang Engine 1          │
Node 13             │                           │  │  (32 GPUs, TP跨4节点)     │
Node 14             │                           │  │                           │
Node 15             │                           │  │                           │
                    └───────────────────────────┘  └───────────────────────────┘

Megatron: 64 GPUs, TP=4, PP=4, CP=2, EP=16
SGLang:   64 GPUs, 2 Engines x 32GPUs, DP=4, EP=32
总计:     128 GPUs (16 节点)

RolloutManager + sglang_router 运行在 Head 节点
```

**权重同步通过 NCCL**：训练完成后，PP source rank 通过 NCCL broadcast 将权重发送到所有 SGLang engine GPU。

### 3.3 多机 Colocate 部署

**典型场景**：超大模型（DeepSeek-R1 671B、GLM-5 744B），GPU 不够分开部署

**参考脚本**：`scripts/run-deepseek-r1.sh`（16 节点 128 GPU）、`scripts/run-glm5-744B-A40B.sh`（32 节点 256 GPU）

```bash
# 关键参数
--actor-num-nodes 16
--actor-num-gpus-per-node 8
--colocate                             # 共享 GPU
--rollout-num-gpus-per-engine 64       # 单个引擎跨 64 GPU（8 节点）
```

**进程与 GPU 分布图**（以 16 节点为例）：

```
┌─────────────────────────────────────────────────────────┐
│                 所有 128 GPUs 共享                        │
│                                                         │
│  Node 0-15: 每个 GPU 上同时运行                          │
│    - 1 个 Training Actor (num_gpus=0.4)                  │
│    - 1 个 SGLang Engine 的一部分 (num_gpus=0.2)          │
│                                                         │
│  128 Training Actors (Megatron)                         │
│  2 SGLang Engines (每个 64 GPU, 跨 8 节点)              │
│                                                         │
│  交替使用：推理时 offload 训练，训练时 offload 推理       │
└─────────────────────────────────────────────────────────┘
```

### 3.4 部署关键参数一览

以下参数定义在 `slime/utils/arguments.py:37-108`：

| 参数 | 默认值 | 说明 |
|------|--------|------|
| `--actor-num-nodes` | 1 | 训练节点数 |
| `--actor-num-gpus-per-node` | 8 | 每节点训练 GPU 数 |
| `--critic-num-nodes` | None | Critic 节点数（PPO 时使用） |
| `--rollout-num-gpus` | None | 推理总 GPU 数（non-colocate） |
| `--rollout-num-gpus-per-engine` | 1 | 每个 SGLang 引擎的 GPU 数（即推理 TP） |
| `--num-gpus-per-node` | 8 | 物理每节点 GPU 数 |
| `--colocate` | False | 训练/推理共享 GPU |
| `--offload-train` | None | 推理时卸载训练显存到 CPU |
| `--offload-rollout` | None | 训练时卸载推理显存到 CPU |
| `--update-weight-buffer-size` | 512MB | 权重同步 NCCL buffer 大小 |

引擎数量计算：

```
num_engines = rollout_num_gpus / min(rollout_num_gpus_per_engine, num_gpus_per_node)

示例：
  64 rollout GPUs, 32 GPUs/engine, 8 GPUs/node
  → num_engines = 64 / 8 = 8 个 Ray actor (但只有 2 个引擎，每个 4 节点)
```

---

## 4. 数据链路

### 4.1 同步训练主循环

完整的单轮 rollout-train 数据链路（`train.py:73-103`）：

```
                                时间 →
    ┌─────────┐  ┌───────────┐  ┌─────────┐  ┌──────────┐  ┌──────────┐
    │ offload │  │ generate  │  │ offload │  │  train   │  │ update   │
    │ train   │  │ (rollout) │  │ rollout │  │(Megatron)│  │ weights  │
    └────┬────┘  └─────┬─────┘  └────┬────┘  └─────┬────┘  └─────┬────┘
         │             │             │              │             │
         ▼             ▼             ▼              ▼             ▼
    释放 Megatron   SGLang 推理   释放 SGLang     GRPO/PPO       Megatron →
    权重到 CPU     生成 response  显存到 CPU      梯度更新       SGLang 权重

    ← (offload_train 开启时) →  ← (offload_rollout 开启时) →
```

对应代码：

```python
# train.py:73-103 (简化)
for rollout_id in range(...):
    # ① 推理
    rollout_data_ref = ray.get(rollout_manager.generate.remote(rollout_id))

    # ② offload 推理（可选）
    if offload_rollout:
        rollout_manager.offload()

    # ③ 训练
    actor_model.async_train(rollout_id, rollout_data_ref)

    # ④ 保存 checkpoint（可选）
    if save_interval and rollout_id % save_interval == 0:
        actor_model.save_model()

    # ⑤ offload 训练 + onload 推理 + 权重同步
    offload_train(rollout_id)                  # 释放 Megatron 显存
    rollout_manager.onload_weights()           # 恢复 SGLang 权重
    actor_model.update_weights()               # NCCL/IPC 同步权重
    rollout_manager.onload_kv()                # 恢复 KV Cache + CUDA Graph
```

### 4.2 异步训练主循环

异步模式（`train_async.py`）通过流水线重叠 rollout 和 training：

```
时间线:
  Rollout:  ├─ gen(0) ─┤  ├─ gen(1) ─┤  ├─ gen(2) ─┤
  Training:              ├─ train(0) ─┤  ├─ train(1) ─┤
  Update:                       ├─ update ─┤     ├─ update ─┤
```

对应代码（`train_async.py:36-73`）：

```python
# 提前启动第一轮 rollout（非阻塞）
rollout_data_next_future = rollout_manager.generate.remote(start_rollout_id)

for rollout_id in range(...):
    rollout_data_ref = ray.get(rollout_data_next_future)        # 阻塞等待当前轮
    rollout_data_next_future = rollout_manager.generate.remote(rollout_id + 1)  # 立即启动下一轮

    actor_model.async_train(rollout_id, rollout_data_ref)       # 与下轮 rollout 并行

    if rollout_id % update_weights_interval == 0:
        ray.get(rollout_data_next_future)                       # 更新权重前先等推理完成
        actor_model.update_weights()
        rollout_data_next_future = rollout_manager.generate.remote(...)  # 用新权重重新推理
```

### 4.3 Rollout 推理转发链路

这是 slime 最核心的数据链路，从 prompt 到 training data 的完整流程。

#### 4.3.1 全链路概览

```
DataSource.get_samples()                    # slime/rollout/data_source.py:90
    │  返回 list[list[Sample]]
    ▼
generate_rollout_async()                    # slime/rollout/sglang_rollout.py:356
    │  异步生成主循环
    ├─ submit_generate_tasks()              # sglang_rollout.py:94
    │   │  为每组 prompt 创建 asyncio.Task
    │   ▼
    │  generate_and_rm_group()              # sglang_rollout.py:278
    │   │  并行处理同一 prompt 的 N 个样本
    │   ▼
    │  generate_and_rm()                    # sglang_rollout.py:214
    │   ├─ acquire semaphore               # 并发控制
    │   ├─ generate()                       # sglang_rollout.py:110  ← HTTP 调用
    │   └─ async_rm()                       # reward 打分
    │
    ├─ dynamic_filter()                     # 过滤不合格样本
    │
    └─ abort()                              # 收集够后取消剩余请求
    │
    ▼
_convert_samples_to_train_data()            # rollout.py:682
    │  Sample → RolloutBatch (tensor dict)
    ▼
_split_train_data_by_dp()                   # rollout.py:750
    │  按 DP rank 均衡切分（Karmarkar-Karp 算法）
    ▼
ray.put() → ObjectRef                       # 放入 Ray Object Store
    │
    ▼
Training Actors 消费                         # actor.train.remote(rollout_data_ref)
```

#### 4.3.2 HTTP 推理请求详解

`generate()` 函数（`sglang_rollout.py:110`）是实际发起推理请求的地方：

```python
# sglang_rollout.py:110 (简化)
async def generate(sample, ...):
    # 1. Tokenize
    input_ids = tokenizer.encode(sample.prompt)

    # 2. 构建 payload
    payload = {
        "input_ids": input_ids,
        "sampling_params": {
            "temperature": args.rollout_temperature,
            "top_p": args.rollout_top_p,
            "max_new_tokens": args.rollout_max_response_len,
            "stop": stop_token_ids,
        },
        "return_logprob": True,
        "logprob_start_len": len(input_ids) - 1,
    }

    # 3. HTTP POST 到 router
    url = f"http://{sglang_router_ip}:{sglang_router_port}/generate"
    output = await post(url, payload)

    # 4. 解析响应
    sample.tokens.extend(output["meta_info"]["output_token_ids"])
    sample.rollout_log_probs = output["meta_info"]["output_token_logprobs"]
    sample.response = output["text"]
```

**请求转发路径**：

```
generate()
  │  async POST http://router:port/generate
  ▼
sglang_router (Rust)                        # session_aware 负载均衡
  │  选择最优 engine（KV Cache 匹配度最高 + 负载均衡）
  ▼
SGLang Engine HTTP Server                   # sglang.srt.entrypoints.http_server
  │  GPU 推理
  ▼
返回 JSON: {text, meta_info: {output_token_ids, output_token_logprobs, ...}}
```

#### 4.3.3 HTTP 客户端层

`post()` 函数（`http_utils.py:275`）支持两种模式：

**本地模式**（默认）：

```python
# http_utils.py:201-218  初始化
concurrency = sglang_server_concurrency * num_engines
_http_client = httpx.AsyncClient(
    limits=httpx.Limits(max_connections=concurrency, max_keepalive_connections=concurrency)
)
```

**分布式模式**（`--use-distributed-post`）：

```python
# http_utils.py:221-270  在每个节点创建 Ray actor 做 HTTP POST
for node_ip in unique_node_ips:
    actor = _HttpPosterActor.options(
        scheduling_strategy=NodeAffinitySchedulingStrategy(node_id, soft=False)
    ).remote(concurrency)
    _post_actors.append(actor)

# post() 时 round-robin 分发到各节点的 actor
```

分布式模式适用于跨节点场景，避免所有 HTTP 请求从单个节点发出导致网络瓶颈。

#### 4.3.4 并发控制与过采样

```python
# sglang_rollout.py:62-66  并发控制
semaphore = asyncio.Semaphore(
    args.sglang_server_concurrency * num_engines
)

# sglang_rollout.py:356-420  过采样循环
while collected < target_data_size:
    samples = data_source.get_samples(over_sampling_batch_size)
    state.submit_generate_tasks(samples)             # 提交异步任务
    # ... 等待任务完成 ...
    completed_groups = dynamic_filter(groups)         # 过滤不合格样本
    collected += len(completed_groups)

# 收集够后终止剩余请求
abort()  # POST /abort_request 到所有 worker
```

#### 4.3.5 Sample 数据结构

`Sample`（`slime/utils/types.py:9`）是贯穿整条链路的核心数据对象：

```python
@dataclass
class Sample:
    group_index: int                    # prompt 组索引
    index: int                          # 组内样本索引
    prompt: str | list[dict]            # 输入 prompt
    tokens: list[int]                   # prompt + response token ids
    response: str                       # 生成的文本
    response_length: int                # response 长度
    reward: float | dict                # reward 分数
    loss_mask: list[int]                # 训练 loss mask
    rollout_log_probs: list[float]      # 生成时的 log probabilities
    rollout_routed_experts: list        # MoE routed experts（routing replay 用）
    status: Status                      # PENDING/COMPLETED/TRUNCATED/ABORTED/FAILED
    session_id: str                     # 用于 consistent hashing 路由
    weight_versions: list[str]          # 推理时使用的权重版本
    metadata: dict                      # 自定义元数据
```

#### 4.3.6 训练数据转换

`_convert_samples_to_train_data()`（`rollout.py:682`）将 `list[Sample]` 转为 `RolloutBatch`：

```python
RolloutBatch = {
    "tokens":                list[list[int]],       # prompt + response token ids
    "response_lengths":      list[int],
    "rewards":               list[float],
    "loss_masks":            list[list[int]],
    "rollout_log_probs":     list[list[float]],
    "rollout_routed_experts": list[...],            # MoE 路由信息
    "weight_versions":       list[list[str]],
    ...
}
```

`_split_train_data_by_dp()`（`rollout.py:750`）使用 Karmarkar-Karp 算法（`slime/utils/seqlen_balancing.py`）按序列长度均衡切分到各 DP rank，然后通过 `ray.put()` 放入 Object Store。

### 4.4 权重同步链路

训练完成后，Megatron 权重需要同步到所有 SGLang 引擎。根据部署模式选择不同路径。

#### 4.4.1 NCCL 分布式同步（Non-Colocate 主路径）

**文件**：`slime/backends/megatron_utils/update_weight/update_weight_from_distributed.py`

```
Training Rank 0 (PP source)
    │
    ├─ pause_generation()                   # 暂停所有引擎的推理
    ├─ flush_cache()                        # 清空 KV Cache
    │
    ├─ for each parameter:
    │   ├─ all_gather across TP ranks       # 收集 TP 分片
    │   ├─ (MoE) all_gather across EP ranks # 收集 EP 分片
    │   ├─ convert Megatron → HF format     # 格式转换
    │   ├─ buffer accumulation              # 累积到 update_weight_buffer_size
    │   └─ NCCL broadcast to all engines    # 广播到所有引擎 GPU
    │        │
    │        ├─ dist.broadcast() via group "slime-pp_{rank}"
    │        │   world: [train_rank_0, engine_gpu_0, engine_gpu_1, ...]
    │        └─ engine 侧: POST /update_weights_from_distributed
    │
    ├─ (optional) post_process: int4/fp4 量化
    └─ continue_generation()                # 恢复推理
```

**NCCL 组建立**（`update_weight_from_distributed.py:252`）：

```python
# 创建 NCCL 进程组，包含 training rank 0 和所有 engine GPU
group_name = f"slime-pp_{pp_rank}"
world_size = 1 + sum(engine_gpu_counts)  # train rank + 所有引擎 GPU
dist.init_process_group(backend="nccl", group_name=group_name,
                        init_method=f"tcp://{master_addr}:{master_port}")
```

#### 4.4.2 Tensor IPC 同步（Colocate 主路径）

**文件**：`slime/backends/megatron_utils/update_weight/update_weight_from_tensor.py`

```
所有 Training Ranks (同一 GPU 上)
    │
    ├─ for each HF weight chunk:
    │   ├─ serialize via FlattenedTensorBucket    # 序列化权重
    │   ├─ Gloo gather_object to gather source    # CPU 侧收集所有 TP 分片
    │   └─ engine.update_weights_from_tensor()    # CUDA IPC 传输到同 GPU 的 SGLang
    │        └─ POST /update_weights_from_tensor
    │
    └─ (如有远程引擎) → fallback 到 NCCL broadcast
```

#### 4.4.3 两种路径对比

| 维度 | NCCL 分布式同步 | Tensor IPC 同步 |
|------|-----------------|-----------------|
| 适用场景 | Non-Colocate（独立 GPU） | Colocate（共享 GPU） |
| 传输协议 | NCCL broadcast（GPU Direct） | CUDA IPC + Gloo gather（CPU） |
| 额外通信组 | `slime-pp_{rank}` NCCL group | Gloo gather group |
| MoE 处理 | EP all_gather → convert → broadcast | HF weight iterator → serialize → IPC |
| Fallback | 无 | 远程引擎 fallback 到 NCCL |

### 4.5 内存 offload/onload 链路

Colocate 模式下，训练和推理交替占用 GPU 显存。通过 SGLang 的 `torch_memory_saver` 机制和 HTTP 控制端点实现。

```
Rollout 阶段:
  offload_train()
    └─ actor.sleep.remote()                      # actor_group.py:127
         └─ release Megatron 权重/优化器到 CPU

  onload_weights()
    └─ engine.resume_memory_occupation(tags=[WEIGHTS])  # 恢复 SGLang 模型权重

  onload_kv()
    └─ engine.resume_memory_occupation(tags=[KV_CACHE, CUDA_GRAPH])  # 恢复 KV Cache

  generate()                                     # SGLang 执行推理

Training 阶段:
  offload_rollout()
    └─ engine.release_memory_occupation()         # sglang_engine.py:352
         ├─ POST /flush_cache                     # 清空 KV Cache
         └─ POST /release_memory_occupation       # 释放权重+KV显存

  onload_train()
    └─ actor.wake_up.remote()                    # actor_group.py:123
         └─ 恢复 Megatron 权重/优化器到 GPU

  train()                                        # Megatron 执行 GRPO/PPO 训练
```

---

## 5. 容错机制

**文件**：`slime/utils/health_monitor.py`

`RolloutHealthMonitor`（`health_monitor.py:10`）是每个 `ServerGroup` 的后台守护线程。

**工作流程**：

```
RolloutHealthMonitor (daemon thread)
    │
    ├─ 每 check_interval 秒（默认 30s）:
    │   ├─ 遍历所有 engines
    │   ├─ engine.health_generate.remote(timeout=30s)
    │   │     └─ GET /health_generate
    │   └─ 如果失败:
    │       ├─ engine.shutdown()
    │       ├─ ray.kill(engine_actor)
    │       └─ all_engines[i] = None            # 标记为死亡
    │
    ├─ 在每次 weight update 前:
    │   recover_updatable_engines()              # rollout.py:527
    │     ├─ 找到 all_engines 中为 None 的位置
    │     ├─ ServerGroup.start_engines() 重新启动
    │     ├─ 重建 NCCL 组
    │     └─ 恢复权重（从磁盘或 NCCL）
    │
    └─ 在 generate()/offload() 期间暂停
       在 onload() 完成后恢复
```

---

## 6. 关键文件索引

| 文件 | 职责 |
|------|------|
| `train.py` | 同步训练主循环入口 |
| `train_async.py` | 异步流水线训练入口 |
| `slime/ray/placement_group.py` | GPU 分配、Placement Group 创建 |
| `slime/ray/actor_group.py` | `RayTrainGroup` 训练 Actor 组管理 |
| `slime/ray/train_actor.py` | `TrainRayActor` 基类，分布式通信初始化 |
| `slime/ray/rollout.py` | `RolloutManager`、`ServerGroup`、`RolloutServer`、Router 启动 |
| `slime/rollout/sglang_rollout.py` | 默认 rollout 函数：异步推理 + reward 打分 |
| `slime/rollout/data_source.py` | `RolloutDataSource` 数据加载与采样 |
| `slime/backends/sglang_utils/sglang_engine.py` | `SGLangEngine` Ray actor，封装 SGLang HTTP 服务 |
| `slime/backends/sglang_utils/sglang_config.py` | YAML 多模型部署配置解析 |
| `slime/backends/megatron_utils/update_weight/update_weight_from_distributed.py` | NCCL 分布式权重同步 |
| `slime/backends/megatron_utils/update_weight/update_weight_from_tensor.py` | Tensor IPC 权重同步 |
| `slime/utils/http_utils.py` | HTTP 客户端（本地/分布式）、Router 启动 |
| `slime/utils/health_monitor.py` | 引擎健康检查与自动恢复 |
| `slime/utils/arguments.py` | 所有命令行参数定义 |
| `slime/utils/types.py` | `Sample` 数据结构 |
| `slime/utils/seqlen_balancing.py` | Karmarkar-Karp 序列长度均衡算法 |
| `scripts/run-qwen3-4B.sh` | 单机 colocate 部署示例 |
| `scripts/run-qwen3-235B-A22B.sh` | 多机 non-colocate 部署示例 |
| `scripts/run-deepseek-r1.sh` | 多机 colocate 部署示例（16 节点） |
| `scripts/run-glm5-744B-A40B.sh` | 超大规模 colocate 部署示例（32 节点） |
