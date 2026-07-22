#!/bin/bash
# 按你的要求：先启动 infer-router/start_router.sh，再启动 infer-router/register_router.sh
# 两个脚本都依赖相对路径（./bin/infer-router、config.yaml 等），必须切到 infer-router 目录下执行
set -e
cd /root/paddlejob/inference-public/fanxiangyu/infer-router
bash start_router.sh
bash register_router.sh
