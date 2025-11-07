#!/usr/bin/env python3
import argparse
import csv
import re
from datetime import datetime
from pathlib import Path

PATTERN = re.compile(r"^(.*?)(?:\s*|\t*)耗时:\s*([0-9]+\.?[0-9]*)\s*ms$", re.UNICODE)
FILENAME_PATTERN = re.compile(r"mimchasher_(icicle|default)_([0-9]{8}_[0-9]{6})\.log$")


def parse_log(path: Path):
    order = []
    values = {}

    with path.open("r", encoding="utf-8", errors="ignore") as fh:
        for raw in fh:
            line = raw.strip()
            match = PATTERN.match(line)
            if not match:
                continue
            label = match.group(1).strip()
            value = float(match.group(2))
            if label not in values:
                order.append(label)
            values[label] = value

    if not order:
        raise ValueError(f"未在日志中找到耗时行: {path}")

    return order, values


def write_single(log_path: Path, order, values):
    out_path = log_path.with_suffix(".csv")
    with out_path.open("w", newline="", encoding="utf-8") as csvfile:
        writer = csv.writer(csvfile)
        writer.writerow(["item", "milliseconds"])
        for label in order:
            writer.writerow([label, values.get(label, "")])
    print(f"[single] 已写入 {out_path}")
    return out_path


def write_comparison(log_paths, orders, records, output):
    if output is None:
        timestamp = datetime.now().strftime("%Y%m%d_%H%M%S")
        output = log_paths[0].parent / f"comparison_{timestamp}.csv"
    else:
        output = Path(output)

    items = []
    seen = set()
    for order in orders:
        for label in order:
            if label not in seen:
                items.append(label)
                seen.add(label)

    headers = ["item"] + [path.stem for path in log_paths]
    with output.open("w", newline="", encoding="utf-8") as csvfile:
        writer = csv.writer(csvfile)
        writer.writerow(headers)
        for label in items:
            row = [label]
            for record in records:
                value = record.get(label)
                row.append(f"{value:.6f}" if value is not None else "")
            writer.writerow(row)
    print(f"[compare] 已写入 {output}")


def main():
    parser = argparse.ArgumentParser(
        description="解析 mimchasher 日志并导出 CSV，可对比多个日志。"
    )
    parser.add_argument("logs", nargs="*", help="日志文件路径，按比较顺序排列")
    parser.add_argument("-o", "--out", help="当比较多个日志时的输出 CSV 路径")
    parser.add_argument("--auto", action="store_true", help="自动扫描目录并生成 CSV 与对比报表")
    parser.add_argument(
        "--dir",
        default="logs/msm-fft-gpu-compare",
        help="auto 模式使用的日志目录 (默认: logs/msm-fft-gpu-compare)",
    )
    args = parser.parse_args()

    if args.auto:
        directory = Path(args.dir)
        if not directory.is_dir():
            parser.error(f"目录不存在: {directory}")

        log_paths = sorted(directory.glob("*.log"))
        if not log_paths:
            parser.error(f"目录中未找到日志: {directory}")

        groups = {}
        for path in log_paths:
            try:
                order, values = parse_log(path)
            except ValueError as exc:
                print(f"[warn] {exc}")
                continue

            write_single(path, order, values)

            match = FILENAME_PATTERN.search(path.name)
            if match:
                kind, ts = match.groups()
                groups.setdefault(ts, {})[kind] = (path, order, values)

        for ts, group in groups.items():
            if len(group) < 2:
                continue
            kinds = [k for k in sorted(group.keys())]
            paths = [group[k][0] for k in kinds]
            orders = [group[k][1] for k in kinds]
            records = [group[k][2] for k in kinds]
            out_path = Path(args.dir) / f"comparison_{ts}.csv"
            write_comparison(paths, orders, records, out_path)

        return

    if not args.logs:
        parser.error("未提供日志文件；使用 --auto 自动处理目录")

    log_paths = [Path(p) for p in args.logs]
    for p in log_paths:
        if not p.is_file():
            parser.error(f"文件不存在: {p}")

    orders = []
    records = []
    for path in log_paths:
        order, values = parse_log(path)
        orders.append(order)
        records.append(values)

    if len(log_paths) == 1:
        write_single(log_paths[0], orders[0], records[0])
    else:
        write_comparison(log_paths, orders, records, args.out)


if __name__ == "__main__":
    main()

