# MODEL_PATH="/ssd1/xuanyuan/models/DeepSeek-V4-Flash/"

MODEL_PATH="/root/paddlejob/inference-public/Models/DeepSeek-V4-Flash-FP8/"
# export SGLANG_DG_CACHE_DIR=~/.cache/deep_gemm
# export SGLANG_NCCL_ALL_GATHER_IN_OVERLAP_SCHEDULER_SYNC_BATCH=1
# nsys profile \
#     -c cudaProfilerApi \
#     -t cuda,nvtx \
#     --cuda-graph-trace=node \
#     -f true \
#     -o nsys/sglang_dsv4_dp4tp4ep4_c24_nccl \
# SGLANG_TORCH_PROFILER_DIR="/ssd1/xuanyuan/debug/nsys"
# CPU_GOVERNOR=$(head -n 1 /sys/devices/system/cpu/cpu55/cpufreq/scaling_governor)
# CPU_GOVERNOR=${CPU_GOVERNOR//[^a-zA-Z0-9._-]/_}
# echo "cpu55 scaling governor: ${CPU_GOVERNOR}"

# # export SGLANG_WLOAD_PROFILE=1
# export SGLANG_WLOAD_ASYNC_COPY=1

# 环境变量	默认值	作用
# SGLANG_TORCH_PROFILER_DIR	/tmp	trace 输出目录
# SGLANG_PROFILE_WITH_STACK	True	记录 Python 调用栈
# SGLANG_PROFILE_RECORD_SHAPES	True	记录 tensor shape
# nsys profile \
#     -c cudaProfilerApi \
#     -t cuda,nvtx \
#     --cuda-graph-trace=node \
#     -f true \
#     -o nsys/sglang_dsv4_tp4ep4_c24_nccl_0617_cpu_1940 \
#!/bin/bash
export SGLANG_DSV4_FP4_EXPERTS=0
export SGLANG_OPT_FP8_WO_A_GEMM=0
export SGLANG_ENABLE_JIT_DEEPGEMM=1
export SGLANG_SHARED_EXPERTS_SKIP_QUANT=1
export SGLANG_ATTN_SKIP_QUANT=1
export SGLANG_TOPK_TRANSFORM_512_TORCH=1
export CUDA_LAUNCH_BLOCKING=0
export SGLANG_OPT_FUSE_WQA_WKV=0
export NVSHMEM_QP_DEPTH=2050
export SGLANG_DEEPEP_NUM_MAX_DISPATCH_TOKENS_PER_RANK=1024


sglang serve \
  --trust-remote-code \
  --model-path "$MODEL_PATH" \
  --tp 4 \
  --dp 4 \
  --ep 4 \
  --enable-dp-attention \
  --moe-a2a-backend deepep \
  --deepep-config '{"normal_dispatch":{"num_sms":64},"normal_combine":{"num_sms":64}}' \
  --host 0.0.0.0 \
  --port 30100 \
  --nccl-port 18201 \
  --tool-call-parser deepseekv4 \
  --reasoning-parser deepseek-v4 \
  --chunked-prefill-size 16384 \
  --mem-fraction-static 0.8 \
  --moe-runner-backend deep_gemm \
  --fp8-gemm-backend deep_gemm \
  --speculative-algo EAGLE \
  --speculative-num-steps 2 \
  --speculative-eagle-topk 1 \
  --speculative-num-draft-tokens 3 \
  --speculative-verify-strategy target_match \
  --kv-cache-dtype bf16 \
  --enable-fp32-lm-head \
  --enable-fp32-moe-gate \
  --enable-deepseek-v4-bf16-indexer \
  --context-length 262144 \
  --tokenizer-worker-num 32 \
  --detokenizer-worker-num 16 \
  --allow-auto-truncate \
  --enable-cache-report \
  --enable-metrics


# 
