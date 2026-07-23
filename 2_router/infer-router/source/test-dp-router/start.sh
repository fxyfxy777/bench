#!/bin/bash
set -euo pipefail

BINARY="./bin/infer-router"
CONFIG="config.yaml"
PIDFILE=".router.pid"
# Auto-detect admin port from config, default 8081
ADMIN_PORT=$(grep -E '^\s*admin_addr:' "$CONFIG" 2>/dev/null | grep -oP ':\K[0-9]+' || echo "8081")
HEALTH_URL="http://127.0.0.1:${ADMIN_PORT}/healthz"
MAX_WAIT=30

echo "[1/5] Checking binary and config..."
if [[ ! -x "$BINARY" ]]; then
    echo "ERROR: $BINARY not found or not executable"
    exit 1
fi
if [[ ! -f "$CONFIG" ]]; then
    echo "ERROR: $CONFIG not found"
    exit 1
fi

if [[ -f "$PIDFILE" ]]; then
    old_pid=$(cat "$PIDFILE")
    if kill -0 "$old_pid" 2>/dev/null; then
        echo "ERROR: router already running (pid=$old_pid)"
        exit 1
    fi
    rm -f "$PIDFILE"
fi

echo "[2/5] Starting infer-router..."
nohup "$BINARY" --config "$CONFIG" > router_stdout.log 2>&1 &
PID=$!
echo "$PID" > "$PIDFILE"
echo "       PID: $PID"

echo "[3/5] Waiting for process to stabilize..."
sleep 1
if ! kill -0 "$PID" 2>/dev/null; then
    echo "ERROR: process exited immediately, check router_stdout.log"
    rm -f "$PIDFILE"
    exit 1
fi

echo "[4/5] Waiting for health check ($HEALTH_URL)..."
elapsed=0
while (( elapsed < MAX_WAIT )); do
    if curl -sf -o /dev/null "$HEALTH_URL" 2>/dev/null; then
        echo "[5/5] Router started successfully (pid=$PID)"
        exit 0
    fi
    sleep 1
    ((elapsed++))
done

echo "ERROR: health check failed after ${MAX_WAIT}s, killing process"
kill "$PID" 2>/dev/null || true
rm -f "$PIDFILE"
exit 1
