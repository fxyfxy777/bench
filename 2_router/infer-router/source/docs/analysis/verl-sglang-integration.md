# verl ↔ sglang 集成深度分析

> 本文档系统性记录 verl 如何调用 sglang 的全部细节，包含精确代码位置、单机/多机调用链路、sglang 内部 Scheduler、sglang-router 的完整能力、verl 为何不用它，以及部署最佳实践。为 OneRouter 设计提供参考。

## 一、架构总览

verl 调用 sglang 存在两种截然不同的架构路径：

| 维度 | 架构 1：进程内直调（RL rollout 主路径） | 架构 2：HTTP REST（独立生成场景） |
|------|----------------------------------------|----------------------------------|
| 场景 | RL 训练中的 rollout 生成 | 独立 generation server |
| 协议 | Python 进程内调用，零拷贝 | HTTP REST（FastAPI） |
| 库 | 直接 `await tokenizer_manager.generate_request()` | `aiohttp` / `requests` |
| 输入格式 | `GenerateReqInput(input_ids=...)` 原始 token IDs | OpenAI 格式 `{"messages": [...]}` |
| 跨节点 | Ray gRPC object transfer（透明） | 标准 HTTP |
| 权重同步 | CUDA IPC + NCCL（零拷贝 GPU 显存共享） | 不涉及 |

架构 1 是 verl RL 训练的核心路径，架构 2 仅用于独立的推理评估场景。

## 二、核心代码位置索引

### verl 侧

> 以下路径相对于 `/Users/xingki/project/verl/`

| 文件 | 核心组件 | 关键行号 |
|------|---------|---------|
| `verl/workers/rollout/sglang_rollout/async_sglang_server.py` | `SGLangHttpServer` — Ray GPU actor，启动 sglang 子进程并直调 tokenizer_manager | L130-142: `__init__`（多节点 master 协调）<br>L153-304: `launch_server()`（构造 ServerArgs + `_launch_subprocesses`）<br>L263-276: 获取 `tokenizer_manager`<br>L282-304: 启动 uvicorn HTTP server<br>L343-437: `generate()`（构造 `GenerateReqInput` 并直调）<br>L382-401: 构造请求（`input_ids` + `sampling_params`）<br>L402-436: 解析响应为 `TokenOutput` |
| 同上 | `SGLangReplica` — 管理跨节点的单个 sglang 副本 | L469: class 定义<br>L482-573: `launch_servers()`（per-node 创建 SGLangHttpServer actor）<br>L506-553: 多节点 NCCL 协调 |
| `verl/workers/rollout/sglang_rollout/sglang_rollout.py` | `ServerAdapter` — 权重同步和内存管理适配器 | L180-203: `release/resume_memory_occupation`<br>L205-267: `update_weights()`（CUDA IPC 权重同步）<br>L222-241: LoRA adapter 热加载 |
| `verl/experimental/agent_loop/agent_loop.py` | 请求调度和负载均衡 | L56: `DEFAULT_ROUTING_CACHE_SIZE = 10000`<br>L59-91: `GlobalRequestLoadBalancer`（least-inflight + sticky session）<br>L101-168: `AsyncLLMServerManager`（管理多 sglang server）<br>L138-168: `generate()`（acquire → remote call → release）<br>L985: `AgentLoopManager` class<br>L1044-1064: `num_replicas` 计算<br>L1132-1149: `generate_sequences()`（chunk + asyncio.gather）<br>L1177-1185: straggler 指标记录 |
| `verl/workers/engine_workers.py` | `ActorRolloutRefWorker` — 训推切换流程 | L642-708: `update_weights()`（release → sync weights → resume） |
| `verl/workers/rollout/sglang_rollout/http_server_engine.py` | HTTP 方式调用 sglang（备用路径） | L68-70: 超时/重试默认值（60s/3次/2s）<br>L244: TODO: Enable SGLang router<br>L288-346: `_make_request()`（同步 requests 库）<br>L422-479: `generate()`（HTTP POST /generate）<br>L656-716: `_make_async_request()`（异步 aiohttp） |
| `verl/trainer/main_generation_server.py` | 独立生成场景的 HTTP 调用 | L42: `num_replicas` 计算<br>L66-79: `submit_request()`（aiohttp POST /v1/chat/completions） |
| `verl/workers/rollout/replica.py` | `RolloutReplica` — 副本基类 | L83: class 定义<br>L119-129: `world_size` / `nnodes` 计算<br>L302-392: `RolloutReplicaRegistry`（工厂） |
| `verl/workers/config/rollout.py` | `RolloutConfig` — 配置参数 | L203: `data_parallel_size`（默认 1）<br>L205: `tensor_model_parallel_size`（默认 2）<br>L206: `pipeline_model_parallel_size`（默认 1）<br>L258: `enable_prefix_caching`（默认 True） |
| `verl/trainer/ppo/ray_trainer.py` | `RayPPOTrainer` — 训练主循环 | L235: class 定义<br>L877-883: 创建 `AgentLoopManager`<br>L1290: `fit()` 训练循环<br>L1374-1378: 调用 `generate_sequences()` |
| `verl/trainer/main_ppo.py` | 资源池分配 | L224-263: `init_resource_pool_mgr()`（hybrid 模式共享 GPU） |
| `verl/experimental/reward_loop/router/inner_sglang_router.py` | sglang-router 集成（未启用） | L22: `from sglang_router.launch_server import RouterArgs, launch_router` |
| `verl/experimental/reward_loop/reward_model.py` | sglang-router 标注 "not ready" | L90: `# TODO (dyy): sglang router is not ready yet.` |

### sglang 侧

> 以下路径相对于 `/Users/xingki/project/sglang/python/sglang/srt/`

| 文件 | 核心组件 | 关键行号 |
|------|---------|---------|
| `entrypoints/http_server.py` | FastAPI 应用，全部 HTTP 端点 | L488-489: `GET /health`, `/health_generate`<br>L673: `POST /generate`（原生生成）<br>L1082: `POST /update_weights_from_tensor`（权重同步）<br>L1185: `POST /release_memory_occupation`（释放显存）<br>L1197: `POST /resume_memory_occupation`（恢复显存）<br>L1233: `POST /load_lora_adapter`<br>L1251: `POST /load_lora_adapter_from_tensors`<br>L1266: `POST /unload_lora_adapter`<br>L1396: `POST /v1/completions`<br>L1404: `POST /v1/chat/completions`<br>L1506: `GET /v1/models` |
| `entrypoints/engine.py` | Engine 启动入口 | L565-579: `_launch_scheduler_processes()`<br>L602: `_launch_subprocesses()` classmethod |
| `managers/io_struct.py` | 请求/响应数据结构 | L48-50: `BaseReq`<br>L123-229: `GenerateReqInput`（text/input_ids/sampling_params/return_logprob 等）<br>L1339: `UpdateWeightsFromTensorReqInput`<br>L1487-1508: `ReleaseMemoryOccupationReqInput` / `ResumeMemoryOccupationReqInput`<br>L1762: `LoadLoRAAdapterFromTensorsReqInput` |
| `managers/scheduler.py` | Scheduler — 连续批处理调度器 | L262: class 定义（11 个 mixin）<br>L445: `init_ipc_channels()`（ZMQ 通道）<br>L544: `init_tp_model_worker()`<br>L786-800: `init_running_status()`（waiting_queue/running_batch）<br>L1355: `recv_requests()`（ZMQ 非阻塞接收）<br>L1494: `process_input_requests()`（类型分发）<br>L2024: `get_next_batch_to_run()`（核心调度逻辑）<br>L2080: `get_new_batch_prefill()`<br>L2356: `update_running_batch()`<br>L2443: `run_batch()`（驱动 GPU 前向） |
| `managers/data_parallel_controller.py` | DataParallelController — DP 内部路由 | L62-76: `LoadBalanceMethod` 枚举<br>L79-105: `DPBudget`（per-rank 负载追踪）<br>L108: class 定义<br>L185: `handle_load_update_req()`<br>L495-499: `maybe_external_dp_rank_routing()`（外部 DP rank 指定）<br>L502: `round_robin_scheduler`<br>L518: `follow_bootstrap_room_scheduler`<br>L539: `total_requests_scheduler`<br>L545: `total_tokens_scheduler`<br>L551-559: event loop |
| `managers/tokenizer_manager.py` | TokenizerManager — 请求入口 | L316-317: ZMQ PUSH socket<br>L1667-1672: 负载反馈 `WatchLoadUpdateReq` |
| `managers/tp_worker.py` | TpModelWorker — TP 执行器 | L447: `forward_batch_generation()` |
| `model_executor/model_runner.py` | ModelRunner — GPU 计算 | L285: class 定义<br>L1475-1504: `update_weights_from_tensor()`<br>L2492: `forward()`<br>L2794-2801: `LocalSerializedTensor`（"only serializes a pointer"） |
| `weight_sync/utils.py` | 权重同步核心逻辑 | L14-101: `update_weights()`（CUDA IPC 序列化 + dist.gather_object + HTTP 发送元数据） |
| `utils/patch_torch.py` | PyTorch CUDA IPC 补丁 | L40-51: patch `reduce_tensor` / `rebuild_cuda_tensor` 使用 GPU UUID |

## 三、单机调用链路

### 3.1 请求生成链路

```
RayPPOTrainer.fit()                                          [ray_trainer.py:1374]
    │
    │ gen_batch = DataProto (size B)
    │ gen_batch.repeat(n)  →  size B*n
    ▼
AgentLoopManager.generate_sequences()                        [agent_loop.py:1132]
    │
    │ prompts.chunk(num_workers)  →  均分为 W 份
    ▼
┌──────────────────────────────────────────────────────┐
│  AgentLoopWorker_0 ... AgentLoopWorker_W             │  [agent_loop.py:398]
│  (Ray CPU actors, round-robin 分布在各 node)           │
│      │                                               │
│      │ asyncio.gather(per-sample tasks)               │
│      ▼                                               │
│  AsyncLLMServerManager.generate()                    │  [agent_loop.py:138]
│      │                                               │
│      │ ① GlobalRequestLoadBalancer.acquire_server()   │  [agent_loop.py:59]
│      │    → sticky session (LRU cache, 10K entries)   │
│      │    → fallback: min(inflight_requests)          │
│      │    协议: Ray actor call (gRPC)                  │
│      ▼                                               │
│  SGLangHttpServer.generate()                         │  [async_sglang_server.py:343]
│      │                                               │
│      │ ★ 进程内直调，无 HTTP ★                         │
│      │ GenerateReqInput(                              │
│      │     input_ids=prompt_ids,    # torch.Tensor    │
│      │     sampling_params={...},                     │
│      │     return_logprob=True,                       │
│      │ )                                              │
│      ▼                                               │
│  tokenizer_manager.generate_request()                │  [tokenizer_manager.py]
│      │                                               │
│      │ 协议: ZMQ PUSH                                 │
│      ▼                                               │
│  [DataParallelController]  (仅 dp_size > 1 时存在)     │  [data_parallel_controller.py:108]
│      │                                               │
│      │ 负载均衡: round_robin / total_requests / ...    │
│      │ 协议: ZMQ PUSH                                 │
│      ▼                                               │
│  Scheduler                                           │  [scheduler.py:262]
│      │                                               │
│      │ 连续批处理: waiting_queue → prefill/decode batch │
│      ▼                                               │
│  TpModelWorker.forward_batch_generation()            │  [tp_worker.py:447]
│      │                                               │
│      ▼                                               │
│  ModelRunner.forward() + .sample()                   │  [model_runner.py:2492]
│      │                                               │
│      │ GPU 前向计算 + 采样                              │
│      ▼                                               │
│  TokenOutput(token_ids, log_probs, stop_reason)      │
│      │                                               │
│      │ 原路返回: GPU → Scheduler → [ZMQ] →             │
│      │ DetokenizerManager → [ZMQ] → TokenizerManager  │
│      │ → SGLangHttpServer → Ray → AgentLoopWorker     │
└──────────────────────────────────────────────────────┘
    │
    ▼
AgentLoopManager: DataProto.concat(outputs)  →  合并输出，size B*n
```

### 3.2 权重同步链路

每个 training step 之后，需要把新权重从训练进程同步到 sglang 推理进程：

```
ActorRolloutRefWorker.update_weights()                 [engine_workers.py:642]
    │
    │ ① release_memory_occupation(tags=["weights"])
    │     → sglang 释放权重显存给训练使用
    │
    │ ② actor.engine.get_per_tensor_param()
    │     → 获取训练后的新权重 tensor
    │
    │ ③ ServerAdapter.update_weights(weights)           [sglang_rollout.py:205]
    │       │
    │       │ 分桶迭代: get_named_tensor_buckets()
    │       ▼
    │   sgl_update_weights()                            [weight_sync/utils.py:14]
    │       │
    │       │ Step 1: MultiprocessingSerializer.serialize(tensor)
    │       │   → ForkingPickler 产生 CUDA IPC handle（~100 bytes 指针，非数据）
    │       │   → patch_torch.py 用 GPU UUID 替代 device index
    │       │
    │       │ Step 2: dist.gather_object() 汇聚到 TP rank 0
    │       │   → 所有 TP rank 的 IPC handle 集中
    │       │
    │       │ Step 3: rank 0 构造 UpdateWeightsFromTensorReqInput
    │       │   → POST /update_weights_from_tensor
    │       │   → HTTP body 仅含 base64 编码的 IPC handle 元数据
    │       │   → sglang 端通过 IPC handle 直接读取 GPU 显存，零拷贝
    │       │
    │       ▼
    │   flush_cache()  → 清空 KV cache
    │
    │ ④ resume_memory_occupation(tags=["kv_cache"])
    │     → sglang 恢复 KV cache 显存
    │
    ▼
  完成，可以开始下一轮 rollout
```

**关键约束**：CUDA IPC handle 是内核级文件描述符，仅在**同一物理节点**的进程间有效。这是 verl 无法使用外部网关做权重同步的根本原因。

### 3.3 内存释放/恢复链路

训练和推理在 hybrid 模式下共享 GPU 显存，通过两个 API 协调：

```
训练阶段开始:
  rollout.release(tags=["kv_cache", "weights"])
    → POST /release_memory_occupation {"tags": ["kv_cache", "weights"]}
    → sglang 释放全部推理显存
    → 训练框架可以使用这部分显存

推理阶段开始:
  rollout.resume(tags=["weights"])
    → POST /resume_memory_occupation {"tags": ["weights"]}
    → sglang 恢复权重显存（此时加载新权重）

  rollout.update_weights(...)  # 见 3.2 权重同步链路

  rollout.resume(tags=["kv_cache"])
    → POST /resume_memory_occupation {"tags": ["kv_cache"]}
    → sglang 恢复 KV cache 显存
    → 可以开始接收推理请求
```

## 四、多机调用链路

### 4.1 整体架构

```
┌──────────────────────────────────────────────────────────────────────┐
│                         Driver Node                                  │
│                                                                      │
│  RayPPOTrainer.fit()                                                 │
│       │                                                              │
│       ▼                                                              │
│  AgentLoopManager.generate_sequences()                               │
│       │                                                              │
│       │ prompts.chunk(num_workers)                                    │
│       ▼                                                              │
│  ┌─────────────┐  ┌─────────────┐  ┌─────────────┐                  │
│  │ AgentLoop   │  │ AgentLoop   │  │ AgentLoop   │  (CPU actors,     │
│  │ Worker_0    │  │ Worker_1    │  │ Worker_2    │   跨节点分布)      │
│  └──────┬──────┘  └──────┬──────┘  └──────┬──────┘                  │
│         └────────────┬───┘───────────────┘                           │
│                      ▼                                               │
│           GlobalRequestLoadBalancer                                  │
│           (集中式 Ray actor)                                          │
│           ┌───────────────────────────────────┐                      │
│           │ 策略: least-inflight + sticky LRU  │                      │
│           │ inflight: {srv_0:3, srv_1:1, ...}  │                      │
│           │ sticky: LRU{req_id → srv_id}       │                      │
│           └───────────────────────────────────┘                      │
└──────────────────────────────────────────────────────────────────────┘
         │
         │ Ray remote call: server.generate.remote(...)
         │ 协议: Ray gRPC object transfer（跨节点透明）
         ▼
┌──────────────────────┐  ┌──────────────────────┐  ┌─────────────────┐
│      Node 0          │  │      Node 1          │  │     Node 2      │
│                      │  │                      │  │                 │
│  SGLangHttpServer_0  │  │  SGLangHttpServer_1  │  │  SGLangHttp     │
│  (Ray GPU actor)     │  │  (Ray GPU actor)     │  │  Server_2       │
│       │              │  │       │              │  │       │         │
│  tokenizer_manager   │  │  tokenizer_manager   │  │  tokenizer_mgr  │
│  .generate_request() │  │  .generate_request() │  │  .generate()    │
│       │              │  │       │              │  │       │         │
│  [ZMQ] → Scheduler   │  │  [ZMQ] → Scheduler   │  │ [ZMQ]→Scheduler │
│       │              │  │       │              │  │       │         │
│   GPU TP group       │  │   GPU TP group       │  │  GPU TP group   │
│  [GPU0, GPU1, ...]   │  │  [GPU0, GPU1, ...]   │  │ [GPU0,GPU1,...] │
└──────────────────────┘  └──────────────────────┘  └─────────────────┘
```

### 4.2 跨节点通信机制

| 通信路径 | 协议 | 说明 |
|----------|------|------|
| Trainer → AgentLoopWorker | Ray actor call (gRPC) | `prompts.chunk(N)` 均分 |
| AgentLoopWorker → LoadBalancer | Ray actor call (gRPC) | `acquire_server()` / `release_server()` |
| LoadBalancer → SGLangHttpServer | **Ray actor call (gRPC)** | `server.generate.remote(...)` 跨节点透明 |
| SGLangHttpServer 内部 | Python 进程内调用 | `tokenizer_manager.generate_request()` |
| TokenizerManager → Scheduler | ZMQ IPC | 同进程组内 |
| 权重同步 | **CUDA IPC**（同机）/ **NCCL**（跨机）+ `dist.gather_object` | 零拷贝 GPU 显存共享 |

### 4.3 多节点 TP（cross-node TP=16）

当 `tensor_model_parallel_size > gpus_per_node` 时，单个 sglang 副本需要跨多个节点：

```python
# replica.py:119-129
self.world_size = TP * DP * PP
self.gpus_per_replica_node = min(gpus_per_node, self.world_size)
self.nnodes = self.world_size // self.gpus_per_replica_node
# 例: TP=16, gpus_per_node=8 → nnodes=2
```

启动协调流程（`SGLangReplica.launch_servers()`）：

1. Node 0 的 `SGLangHttpServer` 分配 `master_address` 和 `master_port`
2. 所有节点的 server 收到 master 信息后并行启动：`asyncio.gather(*[server.launch_server.remote(...)])`
3. 各节点通过 `dist_init_addr` 建立 NCCL 通信组
4. **仅 node_rank=0 启动 HTTP server**（`async_sglang_server.py:279`），其他节点只参与 TP 计算

### 4.4 副本数计算

```python
# agent_loop.py:1044-1064
rollout_world_size = TP * DP * PP
total_gpus = n_gpus_per_node * nnodes  # (或 worker_group.world_size for hybrid)
num_replicas = total_gpus // rollout_world_size
```

| 配置 | total_gpus | TP | DP | PP | num_replicas | 说明 |
|------|-----------|----|----|-----|-------------|------|
| 单机 4 GPU | 4 | 2 | 1 | 1 | 2 | 2 个独立副本 |
| 单机 8 GPU | 8 | 2 | 1 | 1 | 4 | 4 个独立副本 |
| 2 节点 16 GPU | 16 | 2 | 1 | 1 | 8 | 8 个独立副本 |
| 2 节点 16 GPU | 16 | 16 | 1 | 1 | 1 | 1 个副本跨 2 节点 |

## 五、sglang 内部 Scheduler 详解

> **注意**：sglang 的 Scheduler 和 OneRouter 的 Scheduler 是完全不同的概念。sglang Scheduler 是**单 TP group 内的 GPU 批处理调度器**，而 OneRouter Scheduler 是**跨多个后端实例的请求路由调度器**。

### 5.1 定位

sglang Scheduler 管理一个 Tensor Parallel GPU group 的连续批处理（continuous batching）调度。每个 TP group 对应一个 Scheduler 进程。

定义在 `scheduler.py:262`，由 11 个 mixin 组成：

- `SchedulerOutputProcessorMixin` — 输出处理
- `SchedulerUpdateWeightsMixin` — 权重更新
- `SchedulerProfilerMixin` — 性能剖析
- `SchedulerMetricsMixin` — Prometheus 指标
- `SchedulerDisaggregationDecodeMixin` / `PrefillMixin` — PD 分离
- `SchedulerPPMixin` — Pipeline Parallelism
- `SchedulerDPAttnMixin` — DP Attention
- `SchedulerDllmMixin` — Diffusion LLM

### 5.2 进程架构

```
HTTP Request
    │
    ▼
TokenizerManager (异步进程, uvloop)           [tokenizer_manager.py]
    │  tokenize text → token IDs
    │  创建 TokenizedGenerateReqInput
    │  ZMQ PUSH → scheduler_input_ipc_name
    ▼
[DataParallelController]  (仅 dp_size > 1)    [data_parallel_controller.py]
    │  负载均衡选择 dp_rank
    │  ZMQ PUSH → per-worker IPC socket
    ▼
Scheduler (per TP group 独立进程)              [scheduler.py]
    │  连续批处理: waiting_queue → prefill/decode batch
    │  驱动 GPU 前向: TpModelWorker → ModelRunner
    │  ZMQ PUSH → detokenizer_ipc_name
    ▼
DetokenizerManager (进程)                     [detokenizer_manager.py]
    │  token IDs → text (增量解码)
    │  ZMQ PUSH → tokenizer_ipc_name
    ▼
TokenizerManager
    │  resolve async future → HTTP Response
    ▼
HTTP Response (streaming or complete)
```

### 5.3 连续批处理机制

核心数据结构（`scheduler.py:786-800`）：

- `waiting_queue: List[Req]` — 已到达但未调度的请求
- `running_batch: ScheduleBatch` — 持续运行的解码批次（continuous batching 的核心状态）
- `cur_batch / last_batch` — 当前/上一次前向的批次

每次迭代的调度逻辑（`get_next_batch_to_run`, L2024）：

1. 将上一次 prefill 的请求合并到 `running_batch`
2. 尝试从 `waiting_queue` 创建新的 prefill batch（受 KV cache 可用空间、`max_prefill_tokens`、`chunked_prefill_size`、`max_running_requests` 约束）
3. 如果有新 prefill batch → 执行 prefill
4. 否则 → 对 `running_batch` 执行 decode step
5. 显存不足时 → retract（驱逐请求回 waiting_queue）

关键优化：

- **Chunked Prefill**：长上下文请求分块 prefill，避免独占 GPU
- **Mixed Chunked Prefill**：prefill 和 decode token 合并到同一 forward pass
- **Overlap Scheduling**：CPU 调度（构建下一批次）与 GPU 计算重叠执行

### 5.4 ZMQ 通信通道

| Socket | 类型 | 方向 | 用途 |
|--------|------|------|------|
| `recv_from_tokenizer` | `zmq.PULL` | 入站 | 接收 tokenize 后的请求 |
| `recv_from_rpc` | `zmq.DEALER` | 双向 | 接收 RPC 请求（权重更新等） |
| `send_to_tokenizer` | `zmq.PUSH` | 出站 | 发送控制输出（abort、健康检查结果） |
| `send_to_detokenizer` | `zmq.PUSH` | 出站 | 发送生成结果供解码 |
| `send_metrics_from_scheduler` | `zmq.PUSH` | 出站 | 发送指标数据 |

仅 rank-0 worker（pp_rank==0, attn_tp_rank==0）创建 ZMQ socket，其他 TP rank 通过 NCCL `broadcast_pyobj` 接收数据。

### 5.5 DataParallelController 的 4 种负载均衡

当 `dp_size > 1` 时，`DataParallelController` 在多个 Scheduler 之间做请求分发：

| 方法 | 说明 | 适用场景 |
|------|------|---------|
| `round_robin` | 轮询，跳过不活跃 worker | 默认（非 PD 模式） |
| `total_requests` | 选最少活跃请求数的 worker | 负载不均时 |
| `total_tokens` | 选最少 token 数的 worker（请求数为 tie-breaker） | token 长度差异大时 |
| `follow_bootstrap_room` | 按 `bootstrap_room % N` 路由 | PD 分离场景 |

负载反馈通过 `DPBudget` 类追踪，Scheduler 在 batch 输出中携带负载信息 → TokenizerManager 提取后通过 `WatchLoadUpdateReq` 回传给 Controller。

### 5.6 与 OneRouter Scheduler 的概念对比

| 维度 | sglang Scheduler | OneRouter Scheduler |
|------|-----------------|-------------------|
| 粒度 | 单 TP group 内 GPU 批处理 | 跨多个后端实例的请求路由 |
| 调度对象 | token batch（prefill/decode） | HTTP 请求（chat/completions） |
| 状态 | waiting_queue + running_batch + KV cache | instance 负载 + session 分配 |
| 目标 | GPU 利用率最大化（连续批处理） | 全局负载均衡（最优实例选择） |
| 实现 | 单线程 event-loop + ZMQ | 单线程 event-loop + gRPC |

两者是互补关系：OneRouter Scheduler 选择最优的 sglang 实例，sglang Scheduler 在实例内部做 GPU 批处理调度。

## 六、sglang-router（sgl-model-gateway）完整能力分析

### 6.1 定位

sgl-model-gateway 是 sglang 社区用 **Rust** 实现的**独立推理网关**（又称 `smg`），通过 Python binding `sglang_router` 包提供 Python 接口。它不是一个简单的负载均衡器，而是具备生产级能力的推理网关。

### 6.2 两层路由架构

```
                        ┌──────────────────────────────────┐
                        │   Layer 1: sgl-model-gateway     │
                        │   (Rust, 跨 sglang server 实例)   │
                        │                                  │
                        │   8 种路由策略                     │
                        │   熔断器 / 重试 / 健康检查          │
                        │   K8s 服务发现                     │
                        │   Mesh 多网关 CRDT 同步            │
                        └──────────┬───────────────────────┘
                                   │
                    ┌──────────────┼──────────────┐
                    ▼              ▼              ▼
         ┌──────────────┐ ┌──────────────┐ ┌──────────────┐
         │ sglang srv 0 │ │ sglang srv 1 │ │ sglang srv 2 │
         │              │ │              │ │              │
         │  Layer 2:    │ │  Layer 2:    │ │  Layer 2:    │
         │  DP Ctrl     │ │  DP Ctrl     │ │  DP Ctrl     │
         │  (Python)    │ │  (Python)    │ │  (Python)    │
         │  ┌───┬───┐  │ │  ┌───┬───┐  │ │  ┌───┬───┐  │
         │  │DP0│DP1│  │ │  │DP0│DP1│  │ │  │DP0│DP1│  │
         │  └───┴───┘  │ │  └───┴───┘  │ │  └───┴───┘  │
         └──────────────┘ └──────────────┘ └──────────────┘
```

### 6.3 8 种路由策略

| 策略 | 实现文件 | 核心机制 |
|------|---------|---------|
| `random` | `policies/random.rs` | 均匀随机选择健康 worker |
| `round_robin` | `policies/round_robin.rs` | 原子计数器轮询 |
| **`session_aware`** (默认) | `policies/session_aware.rs` + `policies/tree.rs` | **近似 Radix Tree** 做 KV cache 前缀匹配；负载均衡时退化为 shortest-queue |
| `power_of_two` | `policies/power_of_two.rs` | 随机选 2 个，取负载低者；使用缓存的 token 级负载 |
| `consistent_hashing` | `policies/consistent_hashing.rs` | blake3 hash ring，O(log n) 查找；支持 `X-SMG-Routing-Key` header |
| `prefix_hash` | `policies/prefix_hash.rs` | token 前缀哈希 + 负载均衡 walk |
| `bucket` | `policies/bucket.rs` | 桶路由，周期性边界调整 |
| `manual` | `policies/manual.rs` | 显式 session-to-worker 绑定；支持 Random/MinLoad/MinGroup 分配模式 |

**`session_aware` 策略深入**：

- 数据结构：per-model 的 `DashMap<String, Arc<Tree>>`，每棵树是字符级（非 token 级）的近似 Radix Tree
- 节点定义：`children: DashMap<char, NodeRef>` + `tenant_last_access_time: DashMap<TenantId, u64>`（per-worker LRU）
- 路由逻辑：
  - 计算请求文本与 tree 的最长前缀匹配率
  - `match_rate > cache_threshold` → 路由到最佳匹配 worker
  - 否则 → 路由到负载最低 worker（shortest-queue）
- 负载均衡保护：同时超过 `balance_abs_threshold`（默认 64）和 `balance_rel_threshold`（默认 1.5x）时强制 shortest-queue
- 后台驱逐：`PeriodicTask` 定期驱逐 LRU 叶节点，防止内存膨胀

### 6.4 高级特性

#### 熔断器（Circuit Breaker）

文件：`core/circuit_breaker.rs`

三态模型（Closed → Open → HalfOpen），**无锁原子操作**实现：

- `consecutive_failures: AtomicU32` — 连续失败计数
- `consecutive_successes: AtomicU32` — 半开状态连续成功计数
- 默认配置：5 次连续失败触发断路，2 次成功恢复，30s 超时后尝试半开

所有路由策略通过 `get_healthy_worker_indices()` 过滤熔断 worker：

```rust
workers.iter().filter(|w| w.is_healthy() && w.circuit_breaker().can_execute())
```

#### 请求重试

文件：`core/retry.rs`

- 可重试状态码：408 / 429 / 500 / 502 / 503 / 504
- **每次重试选不同后端**（`select_worker_for_model` 重新调用）
- 指数退避 + jitter：`delay = initial_backoff * multiplier^attempt`（±jitter_factor）
- 默认：5 次重试，50ms 初始退避，30s 最大退避

#### K8s 服务发现

文件：`service_discovery.rs`

- 使用 `kube` crate 的 Watcher（非轮询）实时发现 pod 变化
- 支持 label selector 匹配（Regular / Prefill / Decode 分别配置）
- Pod Ready condition 判断就绪性
- 自动发现同 namespace 的其他 router 节点（用于 Mesh）

#### Mesh 多网关 CRDT 同步

文件：`routers/mesh/handlers.rs`

- 使用外部 `smg-mesh` crate（Gossip + CRDT 协议）
- 同步内容：worker 状态、策略状态、Radix Tree 操作、rate limit 计数器
- API 端点：`/mesh/status`、`/mesh/workers`、`/mesh/policies`、`/mesh/config`
- 目的：多个 gateway 实例间状态一致，支持水平扩展

#### PD 分离（Prefill-Decode Disaggregation）

文件：`routers/http/pd_router.rs` + `pd_types.rs`

- Worker 分类为 `Prefill` / `Decode` / `Regular`
- Prefill 请求携带 `bootstrap_host/port/room` 用于与 Decode 协调
- 支持独立的 prefill / decode 路由策略
- HTTP 和 gRPC 双路径支持

#### 其他

- **gRPC 代理**：`routers/grpc/` — 完整 gRPC pipeline，非仅 HTTP
- **Multi-Model 路由**：per-model workers、policies、hash rings，`WorkerRegistry` 维护 `ModelIndex`
- **Session 路由**：`ManualPolicy` 粘性会话 + `ConsistentHashingPolicy` hash ring
- **负载监控**：in-gateway 原子计数器（inflight）+ 后端轮询 `GET /get_load`（token 级）
- **TLS/mTLS**：worker 通信 + 客户端接入双向 TLS
- **Rate Limiting**：per-gateway + 分布式（通过 Mesh）
- **Auth**：API Key + JWT

### 6.5 RouterArgs 关键参数

> 完整参数在 `sgl-model-gateway/bindings/python/src/sglang_router/router_args.py`

| 类别 | 参数 | 默认值 | 说明 |
|------|------|--------|------|
| 路由 | `policy` | `"session_aware"` | 路由策略 |
| 路由 | `cache_threshold` | `0.3` | cache-aware 匹配率阈值 |
| 路由 | `balance_abs_threshold` | `64` | 负载均衡绝对阈值 |
| 路由 | `balance_rel_threshold` | `1.5` | 负载均衡相对阈值 |
| 重试 | `retry_max_retries` | `5` | 最大重试次数 |
| 重试 | `retry_initial_backoff_ms` | `50` | 初始退避 |
| 熔断 | `cb_failure_threshold` | `10` | 连续失败触发断路 |
| 熔断 | `cb_timeout_duration_secs` | `60` | 断路超时 |
| 健康 | `health_check_interval_secs` | `60` | 健康检查间隔 |
| 健康 | `health_check_endpoint` | `"/health"` | 健康检查端点 |
| 并发 | `max_concurrent_requests` | `-1`（无限） | 最大并发 |

### 6.6 部署方式

```bash
# 方式 1: 共同启动（最简单）
python -m sglang_router.launch_server \
    --model-path meta-llama/Llama-3.1-8B-Instruct \
    --dp-size 4 --tp-size 2 \
    --host 0.0.0.0 --port 30000 \
    --router-policy session_aware

# 方式 2: 独立启动（worker 已存在）
python -m sglang_router.launch_router \
    --worker-urls http://node1:8000 http://node2:8000 \
    --policy session_aware \
    --host 0.0.0.0 --port 30000

# 方式 3: 动态注册（无预设 worker）
python -m sglang_router.launch_router --policy session_aware --port 30000
curl -X POST http://localhost:30000/workers -d '{"url": "http://worker1:8000"}'
```

## 七、verl 为什么不用 sglang-router

### 7.1 五个根本性不兼容

#### 1. 权重同步依赖 CUDA IPC，无法跨网络

verl 的权重同步通过 `MultiprocessingSerializer` 把 CUDA tensor 序列化为 **IPC handle**（内核级文件描述符，~100 bytes），而非实际数据。sglang 端通过 `cudaIpcOpenMemHandle()` 直接读取训练进程的 GPU 显存。

```python
# sglang/srt/model_executor/model_runner.py:2794-2801
class LocalSerializedTensor:
    """torch.Tensor that gets serialized by MultiprocessingSerializer
    (which only serializes a pointer and not the data)."""
    values: List[bytes]  # CUDA IPC handles, NOT tensor data
```

IPC handle 仅在**同一物理节点**的进程间有效。外部网关无法转发这些内核级句柄。

#### 2. 内存生命周期需同 GPU 紧耦合

`release_memory_occupation` / `resume_memory_occupation` 是训练和推理在**同一 GPU** 上交替使用显存的同步协议。网关无法感知也无法协调这种 GPU 级的内存分配。

#### 3. sgl-model-gateway 不暴露训练侧端点

sgl-model-gateway 的路由表（`server.rs:555-698`）仅包含推理请求端点：

- `/generate`、`/v1/chat/completions`、`/v1/completions`、`/v1/embeddings`
- `/health`、`/health_generate`
- `/flush_cache`、`/get_loads`

**不包含**：`/update_weights_from_tensor`、`/release_memory_occupation`、`/resume_memory_occupation`、`/load_lora_adapter_from_tensors`。

#### 4. verl 主路径根本不走 HTTP

verl 的 RL rollout 主路径是**进程内直调** `tokenizer_manager.generate_request()`，完全绕过 HTTP。在这种架构下，外部 HTTP 网关没有介入点。

#### 5. Ray actor 模型已知精确位置

verl 通过 `NodeAffinitySchedulingStrategy` 精确控制每个 `SGLangHttpServer` actor 的物理位置。每个 sglang 实例的 GPU 分配在初始化时就确定了，不需要服务发现或动态路由。

### 7.2 verl 现有调度的已知不足

尽管无法直接使用 sglang-router，verl 自己也意识到其调度能力不足：

| 不足 | 证据 |
|------|------|
| **TODO: 接入 sglang-router** | `http_server_engine.py:244` — `TODO: @ChangyiYang Enable SGLang router for this http server engine` |
| **sglang-router "not ready yet"** | `reward_model.py:90` — sglang router 的导入被注释掉，标注 "not ready yet" |
| **零 KV cache 感知** | `GlobalRequestLoadBalancer` 仅用 `min(inflight_requests)` 调度，对 KV cache 状态完全无感知 |
| **零 straggler 处理** | `asyncio.gather()` 等待所有任务完成，被最慢请求拖住；仅记录指标不做处理（`agent_loop.py:1177`） |
| **无请求级超时** | 生成路径无 timeout（仅 HTTP 传输层有 60s 超时） |
| **无请求重试** | 生成失败不会换后端重试 |
| **O(n) 调度** | `min(dict)` 遍历所有 server，万卡场景下成为瓶颈 |
| **粘性会话仅隐式利用 prefix cache** | sticky session 让多轮对话落在同一 server，但 cache 被驱逐后 LB 不知道 |

### 7.3 verl 的 reward loop 尝试接入 sglang-router

在 `verl/experimental/reward_loop/router/inner_sglang_router.py` 中，verl 已经写了 sglang-router 的集成代码（通过 Python binding `sglang_router.launch_server` 导入），但目前仅用于 reward loop 场景，且主路径中被注释掉标注 "not ready yet"。这说明 verl 团队认可 sglang-router 的价值，只是集成工作尚未完成。

## 八、verl + sglang 部署最佳实践

### 8.1 单机配置

#### 4 GPU 示例

```bash
# Hybrid 模式（训推共享 GPU，默认）
trainer.n_gpus_per_node=4
trainer.nnodes=1
actor_rollout_ref.rollout.tensor_model_parallel_size=2
# → num_replicas = 4 / 2 = 2 个 sglang 副本
```

#### 8 GPU 示例

```bash
trainer.n_gpus_per_node=8
trainer.nnodes=1
actor_rollout_ref.rollout.tensor_model_parallel_size=2
# → num_replicas = 8 / 2 = 4 个 sglang 副本
```

### 8.2 多机配置

#### 2 节点 16 GPU，TP=2

```bash
trainer.n_gpus_per_node=8
trainer.nnodes=2
actor_rollout_ref.rollout.tensor_model_parallel_size=2
# → num_replicas = 16 / 2 = 8 个 sglang 副本（每个副本单节点内）
```

#### 2 节点 16 GPU，cross-node TP=16（大模型）

```bash
trainer.n_gpus_per_node=8
trainer.nnodes=2
actor_rollout_ref.rollout.tensor_model_parallel_size=16
# → num_replicas = 16 / 16 = 1 个 sglang 副本，跨 2 节点
```

### 8.3 Hybrid 模式 vs One-step-off 模式

| 维度 | Hybrid 模式（默认） | One-step-off 模式 |
|------|---------------------|-------------------|
| GPU 分配 | 训推共享同一组 GPU | 训推各占独立 GPU |
| 显存管理 | `release/resume_memory_occupation` 交替 | 各自独立管理 |
| 权重同步 | CUDA IPC（同 GPU 零拷贝） | 需要跨 GPU 传输 |
| 吞吐量 | 串行交替，利用率 ≤ 50% | 流水线并行，利用率更高 |
| 配置 | `hybrid_engine=True`（默认） | `hybrid_engine=False` + `rollout.nnodes/n_gpus_per_node` |
| 适用场景 | GPU 资源有限 | GPU 资源充足、追求高吞吐 |

One-step-off 模式参考配置：

```yaml
# verl/experimental/one_step_off_policy/config/one_step_off_ppo_trainer.yaml
trainer:
  nnodes: 1
  n_gpus_per_node: 6    # 训练 GPU
rollout:
  nnodes: 1
  n_gpus_per_node: 2    # 推理 GPU（独立资源池）
```

### 8.4 关键参数调优

| 参数 | 默认值 | 调优建议 |
|------|--------|---------|
| `tensor_model_parallel_size` | 2 | 确保 `memory_per_gpu * gpu_memory_utilization * TP > 2 * model_params`；TP > 8 时通信开销显著增加 |
| `gpu_memory_utilization` | 0.5 | Hybrid 模式下受限于训练显存需求；One-step-off 模式可推高至 0.8-0.9 |
| `data_parallel_size` | 1 | 增大可提高吞吐但会降低 per-request 延迟 |
| `max_num_seqs` | 1024 | 根据 GPU 显存和请求长度调整 |
| `free_cache_engine` | True | Hybrid 模式必须为 True，释放 KV cache 给训练 |
| `enable_prefix_caching` | True | 多轮对话场景必开 |
| `agent.num_workers` | 8 | AgentLoopWorker 数量，影响请求并发度 |
| `n` (rollout.n) | 1 | 每个 prompt 生成 N 个响应（GRPO 场景需 > 1） |

## 九、对 OneRouter 的启示

### 9.1 verl 方案的局限

| 局限 | 影响 |
|------|------|
| 紧耦合 Ray | 非 Ray 环境（如纯 K8s 部署）无法使用 |
| 无 cache-aware 路由 | 相似请求可能分散到不同实例，KV cache 利用率低 |
| 无重试/熔断 | 后端故障时请求直接失败，无自动恢复 |
| O(n) 调度 | 万卡场景下调度延迟可能成为瓶颈 |
| 无 straggler 处理 | 慢请求拖住整个 batch |

### 9.2 从 sglang-router 可借鉴的设计

| 特性 | OneRouter 适配建议 |
|------|-------------------|
| **session_aware 路由** | Radix Tree 做前缀匹配，在 OneRouter Scheduler 中维护 per-instance 的近似 cache 状态 |
| **熔断器** | 无锁原子操作实现的三态熔断，per-instance 粒度，集成到 Scheduler 的实例选择逻辑 |
| **请求重试（换后端）** | Gateway 层实现：可重试状态码（502/503/504）时选不同 instance 重试 |
| **K8s 服务发现** | Watch-based 实时发现后端实例变化，替代手动注册 |
| **Mesh 多网关同步** | 多 Gateway 实例间通过 Gossip/CRDT 同步路由状态（长期目标） |

### 9.3 OneRouter 的差异化价值

相比 verl 直调和 sglang-router，OneRouter 的独特定位：

1. **独立部署**：不依赖 Ray，可独立于训练框架部署
2. **通用后端**：支持 sglang、vllm、fastdeploy 等多种推理框架
3. **全局负载视图**：Scheduler event-loop 维护所有实例的全局负载状态
4. **RL 训练生命周期感知**：理解 step 隔离、训推切换等 RL 特有语义
5. **中心调度 + 分布网关**：Scheduler 做全局最优决策，Gateway 做高性能转发
