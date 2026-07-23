#!/bin/bash
cd /tmp/sse-bench
# Run with 1K and 10K requests only (skip 100K to keep runtime reasonable)
# We modify the source temporarily - but instead just use the binary with specific params
./proxybench -c 500 -backends 4 2>&1 | grep -v "engine\.\|transport\.\|\[Debug\]\|\[Info\]\|\[Error\]"
