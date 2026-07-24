python -m sglang_router.launch_router \
  --host 0.0.0.0 \
  --port 41000 \
  --worker-urls http://127.0.0.1:30100 \
  --policy cache_aware \
  --dp-aware