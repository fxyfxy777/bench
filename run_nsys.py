#!/usr/bin/env python3
"""
nsys launch 模式自动化采集工具

流程：
  1. nsys launch --session=NAME ... python api_server  （后台，不采集）
  2. 等服务 ready（监听日志）
  3. nsys start -t cuda,nvtx --session=NAME --output=PATH  （开始采集）
  4. 跑 infer
  5. nsys stop --session=NAME  （停止采集，自动写 .nsys-rep）
  6. kill 服务

用法:
    python run_nsys.py --config fd_bench_bf16_nsys.yaml
    python run_nsys.py --config fd_bench_bf16_nsys.yaml --kill

YAML 每个 experiment 需要：
  nsys_session: <session名>
  nsys_output:  <输出路径，相对于 yaml 所在目录>
  server:  包含 nsys launch --session=NAME 的启动命令
  infer:   benchmark 命令
"""

import glob
import os
import signal
import subprocess
import sys
import time
from pathlib import Path

import yaml

NSYS_BIN = "../nsys-2025.5.1/bin/nsys"

READY_MARKER = "Application startup complete"
ERROR_MARKERS = [
    "error: unrecognized arguments",
    "Traceback (most recent call last)",
    "CUDA out of memory",
    "Address already in use",
]


def wait_for_server(log_file: Path, timeout: int) -> str:
    """返回 'ready' | 'error' | 'timeout'"""
    print(f"  等待服务就绪（{log_file.name}），超时 {timeout}s ...", flush=True)
    deadline = time.time() + timeout
    while time.time() < deadline:
        try:
            text = log_file.read_text(errors="replace")
            if READY_MARKER in text:
                print("  ✓ 服务已就绪", flush=True)
                return "ready"
            for marker in ERROR_MARKERS:
                if marker in text:
                    print(f"  ✗ 启动报错: {marker!r}", flush=True)
                    return "error"
        except FileNotFoundError:
            pass
        remaining = int(deadline - time.time())
        print(f"  ... 等待中，剩余 {remaining}s", end="\r", flush=True)
        time.sleep(3)
    print("\n  ✗ 等待超时", flush=True)
    return "timeout"


def start_server(cmd: str, log_file: Path) -> subprocess.Popen:
    with open(log_file, "w") as f:
        proc = subprocess.Popen(
            ["bash", "-c", cmd],
            stdout=f, stderr=f,
            preexec_fn=os.setsid,
        )
    return proc


def nsys_sessions_list(cwd: Path):
    """打印当前 nsys session 列表，便于调试"""
    r = subprocess.run(
        [NSYS_BIN, "sessions", "list"],
        capture_output=True, text=True, cwd=str(cwd),
    )
    output = (r.stdout + r.stderr).strip()
    print(f"  [nsys sessions list]\n{output}", flush=True)
    return output


def nsys_start(session: str, output: str, cwd: Path) -> bool:
    """返回 True 表示采集已成功启动"""
    print(f"  nsys start -t cuda,nvtx --session={session} --output={output}", flush=True)
    r = subprocess.run(
        [NSYS_BIN, "start", "-t", "cuda,nvtx", f"--session={session}", f"--output={output}"],
        cwd=str(cwd),
    )
    if r.returncode != 0:
        print(f"  ✗ nsys start 失败（returncode={r.returncode}）", flush=True)
        return False
    print("  ✓ nsys 采集已开始", flush=True)
    return True


def nsys_stop(session: str, cwd: Path):
    print(f"  nsys stop --session={session}", flush=True)
    r = subprocess.run(
        [NSYS_BIN, "stop", f"--session={session}"],
        cwd=str(cwd),
    )
    if r.returncode != 0:
        print(f"  ✗ nsys stop 失败（returncode={r.returncode}），可能采集未启动", flush=True)
    else:
        print("  ✓ nsys 采集已停止，.nsys-rep 文件写出中...", flush=True)


def kill_server(port: int, proc: subprocess.Popen, extra_ports: list, cuda_devices: str):
    all_ports = [port] + [port + off for off in extra_ports]
    print(f"  kill 服务（端口 {all_ports}）...", flush=True)
    killed = set()

    for p in all_ports:
        r = subprocess.run(f"lsof -ti :{p}", shell=True, capture_output=True, text=True)
        for pid in r.stdout.strip().splitlines():
            pid = pid.strip()
            if pid:
                subprocess.run(f"kill -9 {pid}", shell=True)
                killed.add(pid)

    if cuda_devices:
        for gid in [x.strip() for x in str(cuda_devices).split(",") if x.strip()]:
            r = subprocess.run(
                f"nvidia-smi --query-compute-apps=pid --format=csv,noheader --id={gid}",
                shell=True, capture_output=True, text=True,
            )
            for pid in r.stdout.strip().splitlines():
                pid = pid.strip()
                if pid and pid not in killed:
                    subprocess.run(f"kill -9 {pid}", shell=True)
                    killed.add(pid)

    if proc and proc.poll() is None:
        try:
            os.killpg(os.getpgid(proc.pid), signal.SIGKILL)
        except (ProcessLookupError, OSError):
            pass

    for sock in glob.glob("/dev/shm/fd_*.sock"):
        try:
            os.remove(sock)
        except OSError:
            pass

    print(f"  已 kill PID: {', '.join(sorted(killed)) or '(无)'}", flush=True)


def run_infer(cmd: str, log_file: Path, cwd: Path):
    print(f"  运行 infer → {log_file.name}", flush=True)
    with open(log_file, "w") as lf:
        proc = subprocess.Popen(
            ["bash", "-c", cmd],
            stdout=subprocess.PIPE, stderr=subprocess.STDOUT,
            text=True, cwd=str(cwd),
        )
        for line in proc.stdout:
            sys.stdout.write(line)
            sys.stdout.flush()
            lf.write(line)
        proc.wait()


def show_menu(experiments: list) -> list:
    print("\n" + "=" * 50)
    print("  nsys launch 采集")
    print("=" * 50)
    print(f"  [0] 全部运行 ({len(experiments)} 个实验)")
    for i, exp in enumerate(experiments, 1):
        print(f"  [{i:2d}] {exp['name']}")
    print("  [k] Kill 当前服务")
    print("  [q] 退出")
    print("=" * 50)
    raw = input("选择（多个用逗号，如 1,3）: ").strip().lower()

    if raw in ("q", "quit"):
        sys.exit(0)
    if raw == "k":
        return "kill"
    if raw in ("0", "all"):
        return list(range(len(experiments)))
    indices = []
    for part in raw.split(","):
        part = part.strip()
        if part.isdigit():
            idx = int(part) - 1
            if 0 <= idx < len(experiments):
                indices.append(idx)
    return indices


def main():
    for key in ("http_proxy", "https_proxy", "HTTP_PROXY", "HTTPS_PROXY"):
        os.environ.pop(key, None)

    import argparse
    parser = argparse.ArgumentParser()
    parser.add_argument("--config", default="fd_bench_bf16_nsys.yaml")
    parser.add_argument("--kill", action="store_true")
    args = parser.parse_args()

    cfg_path = Path(args.config)
    if not cfg_path.exists():
        sys.exit(f"[error] 配置文件不存在: {cfg_path}")

    with open(cfg_path) as f:
        cfg = yaml.safe_load(f)

    g             = cfg.get("global", {})
    port          = g.get("port", 2788)
    extra_ports   = g.get("extra_ports_offsets", [])
    cuda_devices  = g.get("CUDA_VISIBLE_DEVICES", None)
    ready_timeout = g.get("server_ready_timeout", 600)
    shutdown_wait = g.get("shutdown_wait", 20)
    log_dir       = cfg_path.parent / "debug"
    log_dir.mkdir(exist_ok=True)

    experiments = cfg.get("experiments", [])

    if args.kill:
        kill_server(port, None, extra_ports, cuda_devices)
        return

    selection = show_menu(experiments)
    if selection == "kill":
        kill_server(port, None, extra_ports, cuda_devices)
        return
    if not selection:
        print("未选择任何实验，退出。")
        return

    for idx in selection:
        exp     = experiments[idx]
        name    = exp["name"]
        session = exp.get("nsys_session", name)
        output  = exp.get("nsys_output", f"debug/fd_profile_{name}")

        print(f"\n{'─'*50}")
        print(f"[{idx+1}/{len(experiments)}] 实验: {name}  session={session}")
        print(f"{'─'*50}")

        server_log = log_dir / f"{name}_server.log"
        infer_log  = log_dir / f"{name}_infer.log"

        server_cmd = exp.get("server", "").strip()
        if not server_cmd:
            print("  [skip] server 为空", flush=True)
            continue

        # 1. 预清理
        print("  预清理残留进程...", flush=True)
        kill_server(port, None, extra_ports, cuda_devices)
        time.sleep(3)

        # 2. 后台启动（nsys launch，不采集）
        print(f"  启动服务 → {server_log.name}", flush=True)
        proc = start_server(server_cmd, server_log)

        status = wait_for_server(server_log, ready_timeout)
        if status != "ready":
            print(f"  [SKIP] 服务未就绪（{status}），跳过 {name}")
            kill_server(port, proc, extra_ports, cuda_devices)
            continue

        # 3. 确认 session 存在，再开始采集
        nsys_sessions_list(cwd=cfg_path.parent)
        collecting = nsys_start(session, output, cwd=cfg_path.parent)

        # 4. 运行 infer
        infer_cmd = exp.get("infer", "").strip()
        if infer_cmd:
            run_infer(infer_cmd, infer_log, cwd=cfg_path.parent)
        else:
            print("  [skip] infer 为空", flush=True)

        # 5. 停止 nsys 采集（nsys stop 会自动写 .nsys-rep）
        if collecting:
            nsys_stop(session, cwd=cfg_path.parent)
        else:
            print("  [skip] nsys start 未成功，跳过 nsys stop", flush=True)

        # 6. kill 服务
        kill_server(port, proc, extra_ports, cuda_devices)
        print(f"  等待 GPU 显存释放 {shutdown_wait}s ...", flush=True)
        time.sleep(shutdown_wait)

    print(f"\n{'='*50}")
    print(f"  全部完成，共 {len(selection)} 个实验")
    print(f"  采集文件目录: {cfg_path.parent / 'debug'}")
    print(f"{'='*50}")


if __name__ == "__main__":
    main()

# 后台运行示例:
# echo "0" | nohup python run_nsys.py --config fd_bench_bf16_nsys.yaml > out_nsys.txt 2>&1 &
