curl -X PUT http://127.0.0.1:41000/api/v2/instances \
  -H "Content-Type: application/json" \
  -d '{
    "instances": [{
      "id": "127.0.0.1:30100",
      "host": "127.0.0.1",
      "infer_port": 30100,
      "gpu_num": 4,
      "backend_type": "sglang"
    }]
  }'