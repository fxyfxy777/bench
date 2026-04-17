# LLM 推理性能压测工具

自动化压测框架，支持 FastDeploy (FD) 和 SGLang 两种推理框架。

## 目录结构

```
bench/
├── run_bench.py                  # 主压测脚本
├── fd_bench.yaml                 # FastDeploy 配置（FP16）
├── fd_bench_fp8.yaml             # FastDeploy 配置（FP8 + MTP）
├── fd_bench_bf16.yaml            # FastDeploy 配置（BF16）
├── fd_bench_bf16_多轮.yaml       # FastDeploy 配置（BF16 + 多轮）
├── fd_bench_bf16_长输入.yaml     # FastDeploy 配置（BF16 + 长输入）
├── fd_bench_bf16_swe.yaml         # FastDeploy 配置（BF16 + SWE）
├── fd_bench_debug.yaml            # FastDeploy 调试配置
├── sglang_bench.yaml             # SGLang 配置（FP8）
├── sglang_bench_bf16.yaml        # SGLang 配置（BF16）
├── sglang_bench_bf16_swe.yaml    # SGLang 配置（BF16 + SWE）
└── results/                      # 压测结果目录
    ├── check_gpu_occupy.sh      # GPU 占用脚本（Bash）
    └── 卡gpu利用率.py            # GPU 压力测试脚本
```

## 快速开始

### 1. FastDeploy 压测

```bash
# 交互式选择实验
python run_bench.py --config fd_bench.yaml

# 冒烟测试（每个实验只跑 10 条数据）
python run_bench.py --config fd_bench.yaml --smoke-test

# Kill 当前服务
python run_bench.py --config fd_bench.yaml --kill
```

### 2. SGLang 压测

```bash
python run_bench.py --config sglang_bench.yaml
```

### 3. GPU 占用工具

防止其他人占用 GPU，GPU 空闲时自动启动压测脚本。

```bash
cd results
./check_gpu_occupy.sh
```

## 配置说明

### 数据集

| 数据集 | 输入长度 | 输出长度 | 说明 |
|--------|---------|---------|------|
| 中短 | ~2k | ~1k | `0724_ShareGPT_range_1800_3000_num_5000_FD.jsonl` (n=5000) |
| 多轮 | ~8k | ~200 | `20260302_browsecomp_plus_processed_num_830_fd.jsonl` (n=830) |
| 长输入 | ~36k | ~50 | `260226_loogle_longdep_NarrativeQA_mix_prefix_input_avg_35k_num_6748_with_only_max_tokens_for_fastdeploy.jsonl` (n=6748) |
| 长输出 | ~680 | ~18k | `eb5_ppo_data_v1126_reasoning_merge_data_v1_for_release_gbs384_text_shuffled_250_with_output_tokens.jsonl` (n=250) |

### GPU 分配

- **FastDeploy**: GPU 0,1,2,3
- **SGLang**: GPU 4,5,6,7

## 关键指标

| 指标 | 说明 |
|------|------|
| Request throughput (req/s) | 请求吞吐量 |
| Output token throughput (tok/s) | 输出 token 吞吐量 |
| Total Token throughput (tok/s) | 总 token 吞吐量（含输入） |
| Mean TTFT (ms) | 首token 平均时延 |
| Mean E2EL (ms) | 整句平均时延 |
| Mean Decode (tok/s) | 平均解码速度 |

## 依赖

```bash
# Python 依赖
pip install openpyxl pyyaml

# FastDeploy 依赖
# 需要配置 FastDeploy 路径

# SGLang 依赖
# 需要配置 SGLang 路径和 Conda 环境
```

## 注意事项

1. **路径问题**：配置文件中的相对路径基于配置文件所在目录
2. **GPU 占用**：建议使用 `check_gpu_occupy.sh` 防止被抢占
3. **并发控制**：压测脚本会自动 kill 旧进程，但建议先确认 GPU 状态

## 输出结果

压测结果保存在 `results/{框架}_{时间戳}/` 目录：

- `*.xlsx` - Excel 格式的性能汇总
- `*_server.log` - 服务启动日志
- `*_infer.log` - 压测执行日志
- `*_fd_log/` - FastDeploy 内部日志（FD 框架）

## 问题排查

### 压测失败

1. 检查 GPU 显存：`nvidia-smi`
2. 查看服务日志：`results/*/..._server.log`
3. Kill 残留进程：`python run_bench.py --kill`

### 路径错误

如果遇到 "No such file or directory" 错误：
- 确认从 `bench/` 目录运行脚本
- 检查配置文件中的相对路径是否正确
