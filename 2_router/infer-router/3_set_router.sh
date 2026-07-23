curl -X POST http://127.0.0.1:41000/api/v2/start_infer \
  -H "Content-Type: application/json" \
  -d '{
    "model_version": 1,
    "load_balance_policy": "cache_aware",
    "cache_threshold": 0.3,
    "max_request_load": 150,
    "max_request_text_len": 262144
  }'