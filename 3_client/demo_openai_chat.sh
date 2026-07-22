#!/bin/bash
# 基于 client_backup.sh 改的可直接被 run_bench.py 编排的版本：
# - 去掉了原脚本末尾的 `&`（后台化）和 `> xxx.log 2>&1`（重定向到固定文件）
#   因为 run_bench.py 自己管理子进程的后台/超时/输出捕获，脚本自己再后台化+重定向
#   会导致 run_bench.py 立刻拿到空 stdout（bash 主进程秒退），无法正确捕获压测输出
python /root/paddlejob/inference-public/bingoo/code/FastDeploy/benchmarks/benchmark_serving.py \
    --backend openai-chat \
    --model EB45T \
    --endpoint /v1/chat/completions \
    --ip-list 127.0.0.1:41000 \
    --dataset-name EBChat \
    --dataset-path /root/paddlejob/inference-public/bingoo/data/swe_data_256k_128.json \
    --percentile-metrics ttft,tpot,itl,e2el,s_ttft,res_ttft,s_itl,s_e2el,s_decode,input_len,s_input_len,reasoning_len,output_len \
    --metric-percentiles 80,95,99,99.9,99.95,99.99 \
    --hyperparameter-path /root/paddlejob/inference-public/bingoo/data/request.yaml \
    --num-prompts 1 \
    --drop-ratio 0.1 \
    --pd-metrics \
    --max-concurrency 1 \
    --multi-turn \
    --save-result
