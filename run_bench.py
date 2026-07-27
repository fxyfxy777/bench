#!/usr/bin/env python3
"""
SGLang 服务压测编排工具（简化版）

目录结构:
    bench/
      server/<name>.sh   起服务脚本（用户提供，端口/参数等全部自包含）
      router/<name>.sh   注册 router 脚本（可选）
      client/<name>.sh   压测脚本（用户提供）
      results/<name>_<timestamp>/
        1_server.log
        1_server_script.sh    # 起服务脚本的快照（防止 server/ 下的脚本被后续实验覆盖后无法追溯）
        2_router.log
        2_router_script.sh
        3_client.log
        3_client_script.sh
        metrics.json         # 含起服务/测试脚本内容+解析出的参数、sglang版本+commit、测试结果指标

流程（对 experiments.json 里每一条）:
    1. 起服务       -> 等待就绪（health + 日志双重判据，日志出现错误标记直接判失败）
    2. 探测 sglang 版本/commit（读取正在跑的 sglang 进程的真实 python 解释器）
    3. 注册 router（如果配了）
    4. 起测试       -> 带超时保护
    5. 解析结果并落盘 metrics.json（同时从三份脚本原文提取 --flag/KEY=VALUE 参数）
    6. 可选上报 swanlab：
       - config（卡片）= 起服务/router/测试脚本原文 + 解析出的参数 + sglang版本/commit/branch
       - log（表格/图表）= 纯数值测试结果（优先指标 + 其余 benchmark 输出，按默认顺序）
    kill 步骤（finally 块，保证无论成功/失败/超时都执行）-> pkill -f sglang

注意：client/server/router 脚本如果自己用 `&` 后台化并把输出重定向到固定文件，
run_bench.py 会立刻拿到一个空的 stdout（因为主进程秒退），无法正确捕获压测输出/等待完成。
请确保脚本以前台方式运行到结束（去掉末尾的 `&` 和 `> xxx.log 2>&1`），
输出交给 run_bench.py 统一捕获到 results/<run>/xxx.log。

用法:
    python run_bench.py                  # 交互式菜单，输入序号选择要跑的实验（直接回车=全部）
    python run_bench.py --name xxx       # 直接跑指定 name，跳过菜单
    python run_bench.py --no-swanlab     # 跳过 swanlab 上报
    python run_bench.py --kill           # 测试开始前先清理一次残留进程/端口
    python run_bench.py --report-only <run_dir_name>   # 不重跑，只用已有 metrics.json 补报 swanlab
    python run_bench.py --report-only all               # 补报 results/ 下所有实验
"""

import argparse
import json
import os
import re
import signal
import subprocess
import sys
import time
import urllib.request
from datetime import datetime
from pathlib import Path


class GracefulExit(Exception):
    """收到 SIGTERM/SIGINT 时抛出，让当前实验的 try/finally 正常走完清理逻辑再退出"""


def _handle_signal(signum, frame):
    # 忽略掉后续同名信号，避免清理过程（finally 块）本身又被同一个信号打断，
    # 导致清理逻辑跑一半就被中断（比如只 pkill 了一个 pattern 就退出）
    signal.signal(signum, signal.SIG_IGN)
    raise GracefulExit(f"received signal {signum}")


# Python 默认只把 SIGINT 转成异常（KeyboardInterrupt），SIGTERM 默认直接杀死进程、
# 不会走任何 finally 块——这也是之前排查时反复出现“进程被外部信号打断，端口/PID
# 残留没清理”的根因。这里把 SIGTERM 也转成异常，保证不管是 Ctrl+C 还是被
# kill/系统信号打断，run_one_experiment 的 finally 清理都会执行。
signal.signal(signal.SIGTERM, _handle_signal)
signal.signal(signal.SIGINT, _handle_signal)

BENCH_DIR = Path(__file__).resolve().parent
SERVER_DIR = BENCH_DIR / "1_server"
ROUTER_DIR = BENCH_DIR / "2_router"
CLIENT_DIR = BENCH_DIR / "3_client"
RESULTS_DIR = BENCH_DIR / "results"

EXPERIMENTS_FILE = BENCH_DIR / "experiments.json"


def load_experiments() -> list:
    with open(EXPERIMENTS_FILE, encoding="utf-8") as f:
        return json.load(f)

# ── 统一探活/超时配置（不逐服务配置） ────────────────────────────────────────
SERVER_HEALTH_URL = "http://127.0.0.1:30100/health"
SERVER_READY_LOG_PATTERNS = [
    "The server is fired up and ready to roll",
    "Uvicorn running",
]
SERVER_ERROR_LOG_PATTERNS = [
    "Traceback (most recent call last)",
    "CUDA out of memory",
    "Address already in use",
]
SERVER_READY_TIMEOUT_SEC = 1800
SERVER_POLL_INTERVAL_SEC = 5

# 不同 router 实现的健康检查路径不一样（infer-router 用 /v1/status，sglang_router 用 /health），
# 依次探测，任意一个 200 即认为就绪，避免因为换了 router 实现就要改代码/配置
ROUTER_HEALTH_PATHS = ["/v1/status", "/health", "/healthz"]
ROUTER_HEALTH_BASE_URL = "http://127.0.0.1:41000"
ROUTER_READY_TIMEOUT_SEC = 180
ROUTER_POLL_INTERVAL_SEC = 5

CLIENT_TIMEOUT_SEC = 7200

KILL_PATTERNS = ["sglang", "infer-router"]
# 之前用到的端口：sglang server(30100)、router listen(41000)/grpc(41500)/admin(41800)
KILL_PORTS = [30100, 41000, 41500, 41800]

SWANLAB_PROJECT = "sglang-bench"
SWANLAB_KEY_FILE = BENCH_DIR / ".swanlab_key"


def load_swanlab_api_key() -> str | None:
    """从 bench/.swanlab_key 读取 API key（每次跑不用再手动 swanlab login）"""
    if not SWANLAB_KEY_FILE.exists():
        return None
    key = SWANLAB_KEY_FILE.read_text(encoding="utf-8").strip()
    return key or None

# ── 指标解析（复用既有 benchmark_serving 输出格式） ──────────────────────────
def parse_metrics(text: str) -> dict:
    metrics = {}
    in_summary = False
    for line in text.splitlines():
        stripped = line.strip()
        if "Serving Benchmark Result" in stripped:
            in_summary = True
            continue
        if not in_summary:
            continue
        if re.match(r"^=+$", stripped):
            break
        m = re.match(r"^(.+?):\s+([\d.]+)\s*$", stripped)
        if m:
            try:
                metrics[m.group(1).strip()] = float(m.group(2))
            except ValueError:
                pass
    return metrics


# ── 脚本参数提取（从 server/router/client 脚本原文中解析 --flag value / KEY=VALUE） ──
def _cast_value(raw: str):
    v = raw.strip().strip("'\"")
    if re.fullmatch(r"-?\d+", v):
        return int(v)
    try:
        return float(v)
    except ValueError:
        return v


def extract_params(text: str) -> dict:
    """从脚本原文里提取 --flag value / --flag=value 以及 KEY=VALUE 形式的参数（不保证覆盖所有写法）"""
    params = {}
    for raw_line in text.splitlines():
        line = raw_line.strip()
        if not line or line.startswith("#"):
            continue
        if not line.startswith("--"):
            m = re.match(r"^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)=(.+?)\\?$", line)
            if m:
                val = m.group(2).strip().rstrip("\\").strip()
                if val:
                    params[m.group(1)] = _cast_value(val)
        for fm in re.finditer(r"--([a-zA-Z][\w-]*)(?:[=\s]+('[^']*'|\"[^\"]*\"|\S+))?", line):
            flag = fm.group(1).replace("-", "_")
            val = fm.group(2)
            if val is None or val.startswith("--") or val == "\\":
                params[flag] = True
            else:
                params[flag] = _cast_value(val.strip("\\").strip())
    return params


# ── 进程与就绪判断 ────────────────────────────────────────────────────────────
def start_background(cmd: str, log_file: Path, cwd: Path) -> subprocess.Popen:
    # 子进程 stdout 一旦重定向到文件（而非 tty），libc 会从行缓冲切换成全缓冲，
    # 输出要攒够一个 buffer（通常几 KB）才 flush，导致日志文件长时间没有实时更新。
    # 用 stdbuf -oL -eL 强制子进程的 stdout/stderr 保持行缓冲，日志才能实时写入。
    with open(log_file, "w", buffering=1) as f:
        proc = subprocess.Popen(
            ["stdbuf", "-oL", "-eL", "bash", cmd],
            stdout=f,
            stderr=subprocess.STDOUT,
            cwd=str(cwd),
            preexec_fn=os.setsid,
        )
    return proc


def http_ok(url: str, timeout: int = 5) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as resp:
            return resp.status == 200
    except Exception:
        return False


def wait_server_ready(log_file: Path, proc: subprocess.Popen) -> str:
    """返回 'ready' | 'error' | 'timeout'"""
    deadline = time.time() + SERVER_READY_TIMEOUT_SEC
    while time.time() < deadline:
        if proc.poll() is not None:
            print(f"  [server] 进程已退出（returncode={proc.returncode}），判定失败", flush=True)
            return "error"
        text = log_file.read_text(errors="replace") if log_file.exists() else ""
        for pat in SERVER_ERROR_LOG_PATTERNS:
            if pat in text:
                print(f"  [server] 日志出现错误标记: {pat!r}", flush=True)
                return "error"
        log_ready = any(pat in text for pat in SERVER_READY_LOG_PATTERNS)
        health_ready = http_ok(SERVER_HEALTH_URL)
        if log_ready and health_ready:
            print("  [server] 就绪（health 200 + 日志标记均满足）", flush=True)
            return "ready"
        remaining = int(deadline - time.time())
        print(f"  [server] 等待就绪中，剩余 {remaining}s ...", end="\r", flush=True)
        time.sleep(SERVER_POLL_INTERVAL_SEC)
    print("\n  [server] 等待超时", flush=True)
    return "timeout"


def wait_router_ready() -> str:
    deadline = time.time() + ROUTER_READY_TIMEOUT_SEC
    while time.time() < deadline:
        for path in ROUTER_HEALTH_PATHS:
            if http_ok(ROUTER_HEALTH_BASE_URL + path):
                print(f"  [router] 就绪（探活路径: {path}）", flush=True)
                return "ready"
        remaining = int(deadline - time.time())
        print(f"  [router] 等待就绪中，剩余 {remaining}s ...", end="\r", flush=True)
        time.sleep(ROUTER_POLL_INTERVAL_SEC)
    print("\n  [router] 等待超时", flush=True)
    return "timeout"


def run_router(script: Path, log_file: Path, cwd: Path) -> bool:
    # router 脚本有两种写法：
    # 1) 脚本内部自己用 `&` 把进程后台化（如 infer-router 的 router.sh），脚本本身几秒内退出
    # 2) 脚本前台常驻运行不退出（如 sglang_router.sh 直接 `python -m sglang_router.launch_router`）
    # 用 subprocess.run 阻塞等待会导致第2种情况永远卡在这里、走不到探活逻辑，
    # 所以统一用 Popen 非阻塞启动，然后靠 wait_router_ready() 探活判断是否成功
    print(f"  [router] 后台启动脚本，日志: {log_file.resolve()}", flush=True)
    proc = start_background(str(script), log_file, cwd=cwd)
    status = wait_router_ready()
    if status != "ready":
        if proc.poll() is not None:
            print(f"  [router] 进程已退出（returncode={proc.returncode}），详见 {log_file.resolve()}", flush=True)
        return False
    return True


def run_client(script: Path, log_file: Path, cwd: Path) -> tuple[str, str]:
    """返回 (status, output)；status: 'ok' | 'timeout' | 'crashed'"""
    with open(log_file, "w", buffering=1) as lf:
        proc = subprocess.Popen(
            ["stdbuf", "-oL", "-eL", "bash", str(script)],
            stdout=subprocess.PIPE,
            stderr=subprocess.STDOUT,
            text=True,
            cwd=str(cwd),
            preexec_fn=os.setsid,
        )
        lines = []
        deadline = time.time() + CLIENT_TIMEOUT_SEC
        try:
            while True:
                line = proc.stdout.readline()
                if line:
                    sys.stdout.write(line)
                    lf.write(line)
                    lines.append(line)
                elif proc.poll() is not None:
                    break
                if time.time() > deadline:
                    raise TimeoutError
        except TimeoutError:
            print(f"\n  [client] 超时（>{CLIENT_TIMEOUT_SEC}s），kill 压测进程", flush=True)
            try:
                os.killpg(os.getpgid(proc.pid), 9)
            except (ProcessLookupError, OSError):
                pass
            return "timeout", "".join(lines)
        proc.wait()
    status = "ok" if proc.returncode == 0 else "crashed"
    return status, "".join(lines)


def kill_sglang():
    for pattern in KILL_PATTERNS:
        print(f"  [kill] pkill -f {pattern}", flush=True)
        subprocess.run(f"pkill -f {pattern}", shell=True)
    for port in KILL_PORTS:
        r = subprocess.run(f"lsof -ti :{port}", shell=True, capture_output=True, text=True)
        pids = [p.strip() for p in r.stdout.strip().splitlines() if p.strip()]
        if pids:
            print(f"  [kill] 端口 {port} 被占用（pid={','.join(pids)}），kill -9", flush=True)
            subprocess.run(f"kill -9 {' '.join(pids)}", shell=True)


# ── sglang 版本/commit 探测（读取当前正在跑的 sglang 进程的真实解释器） ──────
def _run_quiet(cmd: list, timeout: int = 10) -> str | None:
    try:
        r = subprocess.run(cmd, capture_output=True, text=True, timeout=timeout)
        return r.stdout.strip() if r.returncode == 0 else None
    except (subprocess.TimeoutExpired, FileNotFoundError):
        return None


def find_sglang_pid() -> int | None:
    r = subprocess.run("pgrep -f sglang", shell=True, capture_output=True, text=True)
    pids = [p.strip() for p in r.stdout.strip().splitlines() if p.strip()]
    return int(pids[0]) if pids else None


def get_sglang_version_info() -> dict:
    """探测当前正在跑的 sglang 进程使用的 python 解释器，取版本号 + git commit
    优先读可编辑安装（源码 clone）的 .git；如果是普通 pip 安装（含 pip install git+...），
    再尝试读 dist-info/direct_url.json 里的 vcs_info.commit_id 兜底。
    """
    info = {"version": "unknown", "commit": "unknown", "commit_short": "unknown", "branch": "unknown"}
    pid = find_sglang_pid()
    if pid is None:
        print("  [version] 未找到运行中的 sglang 进程，跳过版本探测", flush=True)
        return info
    try:
        python_bin = os.readlink(f"/proc/{pid}/exe")
    except OSError:
        print(f"  [version] 无法读取 pid={pid} 的解释器路径", flush=True)
        return info

    # 用 importlib.metadata 直接读版本，避免触发 `import sglang` -> `import torch` 全家桶
    # （后者冷启动可能 20~30s，超过 _run_quiet 的 timeout 被静默吞掉 → version 变 unknown）
    ver = _run_quiet(
        [python_bin, "-c", "import importlib.metadata as m; print(m.version('sglang'))"]
    )
    if ver:
        info["version"] = ver

    pip_bin = str(Path(python_bin).parent / "pip")
    out = _run_quiet([pip_bin, "show", "sglang"])
    repo_dir = None
    location = None
    if out:
        for line in out.splitlines():
            if line.startswith("Editable project location:"):
                repo_dir = line.split(":", 1)[1].strip()
            elif line.startswith("Location:"):
                location = line.split(":", 1)[1].strip()

    if repo_dir and Path(repo_dir, ".git").is_dir():
        commit = _run_quiet(["git", "-C", repo_dir, "rev-parse", "HEAD"])
        if commit:
            info["commit"] = commit
            info["commit_short"] = commit[:9]
        branch = _run_quiet(["git", "-C", repo_dir, "rev-parse", "--abbrev-ref", "HEAD"])
        if branch:
            info["branch"] = branch
    elif location:
        # 非可编辑安装：依次尝试 dist-info 里的三种记录源
        # 1) direct_url.json 的 vcs_info.commit_id  —— pip install git+... 会写
        # 2) direct_url.json 的 archive_info        —— pip install ./xxx.whl 不含 vcs 信息，仅记录来源
        # 3) scm_version.json 的 node / branch      —— setuptools_scm 打包时写入的 git 元数据（本地 wheel 场景兜底）
        for dist_info in Path(location).glob("sglang-*.dist-info"):
            direct_url_file = dist_info / "direct_url.json"
            if direct_url_file.exists():
                try:
                    data = json.loads(direct_url_file.read_text())
                except json.JSONDecodeError:
                    data = {}
                commit_id = data.get("vcs_info", {}).get("commit_id")
                if commit_id:
                    info["commit"] = commit_id
                    info["commit_short"] = commit_id[:9]
                    info["branch"] = data.get("vcs_info", {}).get("requested_revision", "unknown")

            # scm_version.json 兜底（wheel 安装场景）
            if info["commit"] == "unknown":
                scm_file = dist_info / "scm_version.json"
                if scm_file.exists():
                    try:
                        scm = json.loads(scm_file.read_text())
                    except json.JSONDecodeError:
                        scm = {}
                    node = scm.get("node") or ""
                    # setuptools_scm 的 node 形如 "g<hash>"，去掉前缀 g
                    if node.startswith("g"):
                        node = node[1:]
                    if node:
                        info["commit"] = node
                        info["commit_short"] = node[:9]
                    branch = scm.get("branch")
                    if branch and info["branch"] == "unknown":
                        info["branch"] = branch
            break
    print(f"  [version] sglang version={info['version']} commit={info['commit_short']}", flush=True)
    return info


# ── swanlab 上报：config = 起服务/router/测试参数（卡片里看配置），log = 纯数值测试结果（表格/图表对比） ──
# 优先指标：数据量/并发来自 client 脚本参数，其余来自 benchmark 输出解析结果
PRIORITY_METRIC_SPEC = [
    ("数据量", "client_params", "num_prompts"),
    ("并发", "client_params", "max_concurrency"),
    ("任务总耗时(s)", "metrics", "Benchmark duration (s)"),
    ("QPS", "metrics", "Request throughput (req/s)"),
    ("TPS", "metrics", "Total Token throughput (tok/s)"),
    ("解码速度(Median Decode)", "metrics", "Median Decode"),
    ("TTFT(ms)", "metrics", "Mean TTFT (ms)"),
    ("ETE(ms)", "metrics", "Mean E2EL (ms)"),
    ("InputTokens", "metrics", "Mean Input Length"),
    ("CachedTokens", "metrics", "Mean Cached Tokens"),
    ("OutputTokens", "metrics", "Mean Output Length"),
]
# 上面这些 metrics 字段已经被优先指标覆盖，剩余 metrics 按默认顺序原样上报，不再重复
_PRIORITY_METRIC_KEYS = {src_key for _, src, src_key in PRIORITY_METRIC_SPEC if src == "metrics"}


def build_swanlab_config(result: dict) -> dict:
    """起服务/router/测试脚本的原文 + 解析出的参数 + sglang 版本信息，进 swanlab 的配置卡片"""
    return {
        "server_script": result.get("server_script", ""),
        "server_param": result.get("server_params", {}),
        "router_script": result.get("router_script", ""),
        "router_param": result.get("router_params", {}),
        "client_script": result.get("client_script", ""),
        "client_param": result.get("client_params", {}),
        "sglang_version": result.get("sglang_version", {}).get("version", "unknown"),
        "sglang_commit": result.get("sglang_version", {}).get("commit", "unknown"),
        "sglang_commit_short": result.get("sglang_version", {}).get("commit_short", "unknown"),
        "sglang_branch": result.get("sglang_version", {}).get("branch", "unknown"),
    }


def build_swanlab_metrics(result: dict) -> dict:
    """纯数值测试结果，进 swanlab 的 log（可画图/跨实验对比表格）"""
    log_dict = {}
    client_params = result.get("client_params", {})
    metrics = result.get("metrics", {})

    # 1. 优先指标（数据量/并发 + 核心结果指标）
    for label, src, key in PRIORITY_METRIC_SPEC:
        src_dict = client_params if src == "client_params" else metrics
        if key in src_dict:
            log_dict[label] = src_dict[key]

    # CacheRatio 需要单独算：CachedTokens / InputTokens
    cached = metrics.get("Mean Cached Tokens")
    input_len = metrics.get("Mean Input Length")
    if cached is not None and input_len:
        log_dict["CacheRatio"] = cached / input_len

    # 2. 剩余 benchmark 输出指标，按其在原始输出里的默认顺序原样上报（跳过已被优先指标覆盖的）
    for k, v in metrics.items():
        if k in _PRIORITY_METRIC_KEYS:
            continue
        log_dict[k] = v

    return log_dict


def report_swanlab(name: str, result: dict, run_dir: Path):
    try:
        import swanlab
        api_key = load_swanlab_api_key()
        if api_key:
            swanlab.login(api_key=api_key)
        run = swanlab.init(
            project=SWANLAB_PROJECT,
            experiment_name=run_dir.name,
            logdir=str(run_dir),
            config=build_swanlab_config(result),
        )
        run.log(build_swanlab_metrics(result))
        run.finish()
        print(f"  [swanlab] 已上报: project={SWANLAB_PROJECT} name={name}", flush=True)
    except Exception as e:
        print(f"  [swanlab] 上报失败（不影响本次实验结果落盘，若是未登录/无API Key，请先执行 `swanlab login` 或把 key 写入 {SWANLAB_KEY_FILE}）: {e}", flush=True)


# ── 单个实验的完整生命周期 ────────────────────────────────────────────────────
def run_one_experiment(exp: dict, use_swanlab: bool) -> dict:
    name = exp["name"]
    ts = datetime.now().strftime("%Y%m%d_%H%M%S")
    run_dir = RESULTS_DIR / f"{ts}_{name}"
    run_dir.mkdir(parents=True, exist_ok=True)

    result = {"name": name, "time": ts, "status": "pending", "metrics": {}}
    server_proc = None

    print(f"\n{'─'*60}\n实验: {name}  结果目录: {run_dir}\n{'─'*60}", flush=True)

    try:
        # 1. 起服务
        server_script = SERVER_DIR / exp["server"].split("/")[-1]
        server_log = run_dir / "1_server.log"
        result["server_script"] = server_script.read_text(errors="replace")
        result["server_params"] = extract_params(result["server_script"])
        (run_dir / "1_server_script.sh").write_text(result["server_script"])
        print(f"[1/6] 起服务: {server_script.resolve()}", flush=True)
        server_proc = start_background(str(server_script), server_log, cwd=BENCH_DIR)
        ready = wait_server_ready(server_log, server_proc)
        if ready != "ready":
            result["status"] = f"server_{ready}"
            return result

        # 2. 探测 sglang 版本/commit（server 已确认存活，进程树里能找到）
        print("[2/6] 探测 sglang 版本信息...", flush=True)
        result["sglang_version"] = get_sglang_version_info()

        # 3. 注册 router（可选）
        if exp.get("router"):
            router_script = ROUTER_DIR / exp["router"].split("/")[-1]
            router_log = run_dir / "2_router.log"
            result["router_script"] = router_script.read_text(errors="replace")
            result["router_params"] = extract_params(result["router_script"])
            (run_dir / "2_router_script.sh").write_text(result["router_script"])
            print(f"[3/6] 注册 router: {router_script.resolve()}", flush=True)
            if not run_router(router_script, router_log, cwd=BENCH_DIR):
                result["status"] = "router_failed"
                return result
        else:
            result["router_script"] = ""
            result["router_params"] = {}
            print("[3/6] 无 router 脚本，跳过", flush=True)

        # 4. 起测试
        client_script = CLIENT_DIR / exp["client"].split("/")[-1]
        client_log = run_dir / "3_client.log"
        result["client_script"] = client_script.read_text(errors="replace")
        result["client_params"] = extract_params(result["client_script"])
        (run_dir / "3_client_script.sh").write_text(result["client_script"])
        print(f"[4/6] 起测试: {client_script.resolve()}", flush=True)
        run_status, output = run_client(client_script, client_log, cwd=BENCH_DIR)
        result["run_status"] = run_status

        # 5. 解析结果
        metrics = parse_metrics(output)
        result["metrics"] = metrics
        result["parse_status"] = "parsed" if metrics else "parse_failed"
        result["status"] = "ok" if run_status == "ok" else run_status

        (run_dir / "metrics.json").write_text(json.dumps(result, ensure_ascii=False, indent=2))
        print(f"[5/6] 结果已写入: {run_dir / 'metrics.json'}", flush=True)

        # 6. swanlab 上报
        if use_swanlab and metrics:
            print("[6/6] 上报 swanlab...", flush=True)
            report_swanlab(name, result, run_dir)
        else:
            print("[6/6] 跳过 swanlab 上报", flush=True)

        return result
    finally:
        print("[kill] 清理服务与测试进程...", flush=True)
        kill_sglang()
        if server_proc and server_proc.poll() is None:
            print(f"  [kill] killpg server_proc pid={server_proc.pid}", flush=True)
            try:
                os.killpg(os.getpgid(server_proc.pid), 9)
                print(f"  [kill] killpg server_proc pid={server_proc.pid} 完成", flush=True)
            except (ProcessLookupError, OSError) as e:
                print(f"  [kill] killpg server_proc pid={server_proc.pid} 失败: {e!r}", flush=True)


def show_menu(experiments: list) -> list:
    """打印实验列表，让用户输入序号选择要跑哪几个，返回选中的下标列表"""
    print("\n" + "=" * 60)
    print("  SGLang 压测实验列表")
    print("=" * 60)
    print(f"  [0] 全部运行 ({len(experiments)} 个实验)")
    for i, exp in enumerate(experiments, 1):
        print(f"  [{i:2d}] {exp['name']}")
    print("  [q] 退出")
    print("=" * 60)
    raw = input("选择（多个用逗号，如 1,3；直接回车=全部）: ").strip().lower()

    if raw in ("q", "quit"):
        sys.exit(0)
    if raw in ("0", "all", ""):
        return list(range(len(experiments)))
    indices = []
    for part in raw.split(","):
        part = part.strip()
        if part.isdigit():
            idx = int(part) - 1
            if 0 <= idx < len(experiments):
                indices.append(idx)
            else:
                print(f"  [warn] 序号 {part} 超出范围，忽略")
        else:
            print(f"  [warn] 无效输入 {part!r}，忽略")
    return indices


def report_swanlab_from_run_dir(run_dir: Path) -> bool:
    """补报：从已存在的 results/<run>/metrics.json 里读结果，重新上报 swanlab（不重跑实验）"""
    metrics_file = run_dir / "metrics.json"
    if not metrics_file.exists():
        print(f"[error] {metrics_file} 不存在，跳过", flush=True)
        return False
    result = json.loads(metrics_file.read_text(encoding="utf-8"))
    if not result.get("metrics"):
        print(f"[warn] {run_dir.name} 没有 metrics 数据，跳过", flush=True)
        return False
    print(f"[补报] {run_dir.name}", flush=True)
    report_swanlab(result.get("name", run_dir.name), result, run_dir)
    return True


def main():
    parser = argparse.ArgumentParser()
    parser.add_argument("--name", help="只跑指定 name 的实验（跳过交互式选择）")
    parser.add_argument("--no-swanlab", action="store_true", help="跳过 swanlab 上报")
    parser.add_argument("--kill", action="store_true", help="测试开始前先执行一次 kill_sglang() 清理残留进程/端口")
    parser.add_argument(
        "--report-only",
        metavar="RUN_DIR",
        help="不重跑实验，只从已有的 results/<run> 目录读取 metrics.json 重新上报 swanlab"
        "（可传目录名或绝对路径；传 all 补报 results/ 下所有实验）",
    )
    args = parser.parse_args()

    RESULTS_DIR.mkdir(parents=True, exist_ok=True)

    if args.report_only:
        if args.report_only == "all":
            run_dirs = sorted(d for d in RESULTS_DIR.iterdir() if d.is_dir())
        else:
            p = Path(args.report_only)
            run_dirs = [p if p.is_absolute() else RESULTS_DIR / p.name]
        for run_dir in run_dirs:
            report_swanlab_from_run_dir(run_dir)
        return

    all_experiments = load_experiments()

    try:
        if args.kill:
            print("[预清理] 执行 kill_sglang()...", flush=True)
            kill_sglang()

        if args.name:
            experiments = [e for e in all_experiments if e["name"] == args.name]
            if not experiments:
                sys.exit(f"[error] 未找到名为 {args.name} 的实验")
        else:
            selection = show_menu(all_experiments)
            if not selection:
                print("未选择任何实验，退出。")
                return
            experiments = [all_experiments[i] for i in selection]

        all_results = []
        for exp in experiments:
            result = run_one_experiment(exp, use_swanlab=not args.no_swanlab)
            all_results.append(result)
            print(f"\n[完成] {exp['name']}: status={result['status']}", flush=True)
    except GracefulExit as e:
        print(f"\n[中断] 收到退出信号（{e}），已执行清理，停止后续实验。", flush=True)
        sys.exit(130)

    print(f"\n{'='*60}\n全部完成，共 {len(all_results)} 个实验\n{'='*60}")
    for r in all_results:
        print(f"  {r['name']}: {r['status']}")


if __name__ == "__main__":
    main()
