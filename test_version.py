#!/usr/bin/env python3
"""
单测：验证 get_framework_version 能正确获取 FD / SGLang 的版本和 commit 信息
"""

import os
import re
import subprocess
from pathlib import Path


# ── 被测函数 ──────────────────────────────────────────────────────────────────
def get_framework_version(framework: str, experiments: list) -> dict:
    """
    从实验配置的 server 命令中自动提取框架源码路径，
    返回 {"version": ..., "commit": ..., "commit_short": ..., "extra": ...}
    """
    info = {
        "version": "unknown",
        "commit": "unknown",
        "commit_short": "unknown",
        "extra": "",
    }

    # 从第一个有 server 命令的实验中提取信息
    first_server_cmd = ""
    for exp in experiments:
        cmd = exp.get("server", "").strip()
        if cmd:
            first_server_cmd = cmd
            break
    if not first_server_cmd:
        return info

    # ── FastDeploy ────────────────────────────────────────────────────────
    if framework == "fd":
        repo_path = _extract_repo_path(
            first_server_cmd,
            pattern=r'PYTHONPATH="?([^":\s]+FastDeploy)',
        )
        if not repo_path:
            return info

        info.update(_git_info(repo_path))

        # 额外读取 version.txt
        version_txt = Path(repo_path) / "fastdeploy" / "version.txt"
        if version_txt.exists():
            info["extra"] = version_txt.read_text().strip()

    # ── SGLang ────────────────────────────────────────────────────────────
    elif framework == "sglang":
        # 方式 1: 从 PYTHONPATH 提取（优先，如 /workspace3/fxy/sglang/python）
        repo_path = _extract_repo_path(
            first_server_cmd,
            pattern=r'PYTHONPATH=([^":\s]+sglang/python)',
        )
        if repo_path:
            repo_path = str(Path(repo_path).parent)  # python/ → 上级目录

        # 方式 2: 从 python 二进制找 pip show
        if not repo_path:
            python_bin = _extract_python_bin(first_server_cmd)
            if python_bin:
                repo_path = _sglang_repo_from_pip(python_bin)

        if repo_path:
            info.update(_git_info(repo_path))

        # 获取 pip 版本号
        python_bin = _extract_python_bin(first_server_cmd)
        if python_bin:
            ver = _run_cmd(
                [python_bin, "-c",
                 "from sglang.version import __version__; print(__version__)"]
            )
            if ver:
                info["version"] = ver

    return info


# ── 工具函数 ──────────────────────────────────────────────────────────────────
def _extract_repo_path(cmd: str, pattern: str) -> str | None:
    """从 server 命令中用正则提取路径"""
    for line in cmd.splitlines():
        m = re.search(pattern, line)
        if m:
            path = m.group(1)
            if Path(path).is_dir():
                return path
    return None


def _extract_python_bin(cmd: str) -> str | None:
    """从 server 命令中提取 python 二进制路径"""
    for line in cmd.splitlines():
        m = re.search(r'(/\S+/bin/python\S*)', line)
        if m:
            path = m.group(1)
            if Path(path).exists():
                return path
    return None


def _sglang_repo_from_pip(python_bin: str) -> str | None:
    """通过 pip show sglang 找到 editable install 的源码路径"""
    pip_bin = str(Path(python_bin).parent / "pip")
    out = _run_cmd([pip_bin, "show", "sglang"])
    if not out:
        return None
    for line in out.splitlines():
        if line.startswith("Editable project location:"):
            project_dir = line.split(":", 1)[1].strip()
            repo_dir = str(Path(project_dir).parent)
            if Path(repo_dir / Path(".git")).is_dir():
                return repo_dir
    return None


def _git_info(repo_path: str) -> dict:
    """获取 git commit 信息"""
    result = {}
    commit = _run_cmd(["git", "-C", repo_path, "rev-parse", "HEAD"])
    if commit:
        result["commit"] = commit
        result["commit_short"] = commit[:9]

    desc = _run_cmd(
        ["git", "-C", repo_path, "describe", "--tags", "--always"]
    )
    if desc:
        result["version"] = desc

    branch = _run_cmd(
        ["git", "-C", repo_path, "rev-parse", "--abbrev-ref", "HEAD"]
    )
    if branch:
        result["branch"] = branch

    return result


def _run_cmd(cmd: list, timeout: int = 10) -> str | None:
    """运行命令并返回 stdout，失败返回 None"""
    try:
        r = subprocess.run(
            cmd, capture_output=True, text=True, timeout=timeout
        )
        if r.returncode == 0:
            return r.stdout.strip()
    except (subprocess.TimeoutExpired, FileNotFoundError):
        pass
    return None


# ── 测试用例 ──────────────────────────────────────────────────────────────────
def test_fd_version():
    """测试 FastDeploy 版本获取"""
    experiments = [
        {
            "name": "test_fd",
            "server": (
                'export PYTHONPATH="/workspace3/fxy/FastDeploy:$PYTHONPATH"\n'
                "/root/miniconda3/envs/fxy_py12/bin/python -m fastdeploy.entrypoints.openai.api_server \\\n"
                "    --model /workspace3/fxy/models/GLM-4.5-Air --port 2786"
            ),
        }
    ]
    info = get_framework_version("fd", experiments)
    print(f"\n[FD] version info:")
    print(f"  version      : {info['version']}")
    print(f"  commit       : {info['commit']}")
    print(f"  commit_short : {info['commit_short']}")
    if info.get("branch"):
        print(f"  branch       : {info['branch']}")
    if info.get("extra"):
        print(f"  version.txt  :\n    " + info["extra"].replace("\n", "\n    "))

    assert info["commit"] != "unknown", "FD commit should be detected"
    assert len(info["commit"]) == 40, f"FD commit should be 40-char SHA, got: {info['commit']}"
    assert info["commit_short"] == info["commit"][:9]
    assert info["version"] != "unknown", "FD version should be detected"
    print("  ✓ FD version test PASSED")


def test_sglang_version():
    """测试 SGLang 版本获取"""
    experiments = [
        {
            "name": "test_sglang",
            "server": (
                "export PYTHONPATH=/workspace3/fxy/sglang/python:$PYTHONPATH\n"
                "/root/miniconda3/envs/fxy_sglang/bin/python -m sglang.launch_server \\\n"
                "    --model-path /workspace2/fanxiangyu/models/GLM-4.5-Air-FP8 --port 3015"
            ),
        }
    ]
    info = get_framework_version("sglang", experiments)
    print(f"\n[SGLang] version info:")
    print(f"  version      : {info['version']}")
    print(f"  commit       : {info['commit']}")
    print(f"  commit_short : {info['commit_short']}")
    if info.get("branch"):
        print(f"  branch       : {info['branch']}")

    assert info["commit"] != "unknown", "SGLang commit should be detected"
    assert len(info["commit"]) == 40, f"SGLang commit should be 40-char SHA, got: {info['commit']}"
    assert info["commit_short"] == info["commit"][:9]
    assert info["version"] != "unknown", "SGLang version should be detected"
    print("  ✓ SGLang version test PASSED")


if __name__ == "__main__":
    test_fd_version()
    test_sglang_version()
    print("\n✓ All tests PASSED")
