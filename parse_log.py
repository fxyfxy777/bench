#!/usr/bin/env python3
"""
解析 benchmark_serving 的日志文件，写入 xlsx。
已存在的 xlsx 文件会追加到最后一行。

用法:
    python parse_log.py debug/BlaclWell_0508_DP4EP4TP1_logprob.log
    python parse_log.py debug/*.log                          # 多个文件
    python parse_log.py debug/*.log -o results/my_report.xlsx  # 指定输出
"""

import argparse
import re
import sys
from datetime import datetime
from pathlib import Path

try:
    import openpyxl
    from openpyxl.styles import Alignment, Font, PatternFill
    from openpyxl.utils import get_column_letter
except ImportError:
    sys.exit("[error] 请先安装: pip install openpyxl")


# ── 指标展示配置（同 run_bench.py）──────────────────────────────────────────────
PRIORITY_COLS = [
    ("Benchmark duration (s)",          "任务耗时 (s)"),
    ("Mean Input Length",               "输入长度 (tok)"),
    ("Mean Output Length",              "输出长度 (tok)"),
    ("Request throughput (req/s)",      "QPS (req/s)"),
    ("Total Token throughput (tok/s)",  "TPS (tok/s)"),
    ("Output token throughput (tok/s)", "OTPS (tok/s)"),
    ("Mean Decode",                     "解码速度 (tok/s)"),
    ("Mean TTFT (ms)",                  "TTFT (ms)"),
    ("Mean E2EL (ms)",                  "整句均值时延 (ms)"),
]
PRIORITY_KEYS  = [k for k, _ in PRIORITY_COLS]
PRIORITY_LABEL = {k: v for k, v in PRIORITY_COLS}

EXTRA_ORDER = [
    "Successful requests",
    "Total input tokens",
    "Total generated tokens",
]

# 只输出以上指定的列，按此顺序
ALL_KEYS = PRIORITY_KEYS + EXTRA_ORDER


def parse_log(text: str) -> dict:
    """解析日志中 '============ Serving Benchmark Result ============' 之间的指标"""
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


def build_ordered_keys(all_results: list) -> list:
    """优先列在前，其余按日志出现顺序追加"""
    seen_keys = []
    for r in all_results:
        for k in r.get("metrics", {}):
            if k not in seen_keys:
                seen_keys.append(k)

    ordered_keys = []
    # 优先列
    for k in ALL_KEYS:
        if k in seen_keys and k not in ordered_keys:
            ordered_keys.append(k)
    # 剩余按日志顺序
    for k in seen_keys:
        if k not in ordered_keys:
            ordered_keys.append(k)
    return ordered_keys


def write_excel(all_results: list, out_path: Path):
    """写入 xlsx，如果文件已存在则追加行"""
    ordered_keys = build_ordered_keys(all_results)

    fixed_cols  = ["实验名称", "运行时间"]
    metric_cols = [PRIORITY_LABEL.get(k, k) for k in ordered_keys]
    header = fixed_cols + metric_cols

    # 如果文件已存在，加载并追加
    if out_path.exists():
        wb = openpyxl.load_workbook(out_path)
        ws = wb.active
        start_row = ws.max_row + 1
        print(f"[Excel] 文件已存在，从第 {start_row} 行追加", flush=True)
    else:
        wb = openpyxl.Workbook()
        ws = wb.active
        ws.title = "Results"
        # 写表头
        header_fill = PatternFill("solid", fgColor="2E75B6")
        header_font = Font(bold=True, color="FFFFFF")
        for ci, h in enumerate(header, 1):
            cell = ws.cell(row=1, column=ci, value=h)
            cell.fill = header_fill
            cell.font = header_font
            cell.alignment = Alignment(horizontal="center", wrap_text=True)
        start_row = 2

    # 写数据行
    ok_fill = PatternFill("solid", fgColor="E2EFDA")
    for ri, r in enumerate(all_results, start_row):
        ws.cell(row=ri, column=1, value=r["name"])
        ws.cell(row=ri, column=2, value=r["time"])
        for ci, k in enumerate(ordered_keys, len(fixed_cols) + 1):
            ws.cell(row=ri, column=ci, value=r.get("metrics", {}).get(k))
        for ci in range(1, len(header) + 1):
            ws.cell(row=ri, column=ci).fill = ok_fill

    # 自动列宽
    for ci, h in enumerate(header, 1):
        col_letter = get_column_letter(ci)
        max_len = max(
            len(str(h)),
            *(len(str(ws.cell(row=ri, column=ci).value or ""))
              for ri in range(2, ws.max_row + 1)),
        ) if ws.max_row > 1 else len(str(h))
        ws.column_dimensions[col_letter].width = min(max_len + 2, 30)

    ws.freeze_panes = "C2"
    wb.save(out_path)
    print(f"[Excel] 已保存: {out_path}")


def main():
    parser = argparse.ArgumentParser(description="解析 benchmark log 写入 xlsx")
    parser.add_argument("logs", nargs="+", help="日志文件路径")
    parser.add_argument("-o", "--output", default="infer_logs.xlsx",
                        help="输出 xlsx 路径（默认 infer_logs.xlsx）")
    args = parser.parse_args()

    out_path = Path(args.output)
    out_path.parent.mkdir(parents=True, exist_ok=True)

    all_results = []
    for log_path in args.logs:
        p = Path(log_path)
        if not p.exists():
            print(f"[WARN] 文件不存在: {p}", flush=True)
            continue
        text = p.read_text(errors="replace")
        metrics = parse_log(text)
        if not metrics:
            print(f"[WARN] 未找到指标: {p}", flush=True)
            continue
        all_results.append({
            "name": p.stem,
            "time": datetime.now().strftime("%Y-%m-%d %H:%M:%S"),
            "metrics": metrics,
        })
        print(f"[OK] {p.name}: {len(metrics)} 个指标", flush=True)

    if not all_results:
        print("没有有效的日志可解析")
        return

    write_excel(all_results, out_path)


if __name__ == "__main__":
    main()
