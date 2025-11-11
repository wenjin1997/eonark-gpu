#!/usr/bin/env bash
# Usage:
#   scripts/profile_mimchasher.sh
# 该脚本会调用 nsys profile 对 `go run -tags icicle ./examples/mimchasher/main.go -count=1 -v`
# 的执行过程进行系统级采样，生成 GPU/CPU 分析报告，并额外导出只包含 `patch_gpu.go`
# 相关符号的 CPU 采样数据，便于聚焦 GPU 关键函数。

set -euo pipefail

ROOT_DIR="/home/jade/jade/eonark-gpu"
OUTPUT_DIR="${ROOT_DIR}/logs/profiles"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUT_PREFIX="${OUTPUT_DIR}/mimchasher_${TIMESTAMP}"
REPLAY_FILE="${OUT_PREFIX}.nsys-rep"
CPU_REPORT_ALL="${OUT_PREFIX}_cpu_all.csv"
CPU_REPORT_PATCH="${OUT_PREFIX}_cpu_patch_gpu.csv"
SUMMARY_REPORT="${OUT_PREFIX}_summary"

mkdir -p "${OUTPUT_DIR}"

cd "${ROOT_DIR}"

echo "[nsys] Profiling Go workload, output前缀: ${OUT_PREFIX}"
nsys profile \
  --output "${OUT_PREFIX}" \
  --force-overwrite=true \
  --sample=process-tree \
  --cpuctxsw=system-wide \
  --trace=cuda,osrt,nvtx,gpu \
  --cuda-memory-usage=true \
  --gpu-metrics-device=all \
  go run -tags icicle ./examples/mimchasher/main.go -count=1 -v

echo "[nsys] 生成概览报告 (${SUMMARY_REPORT}.txt)"
nsys stats \
  --report summary,gpu-kern-summary,cuda-api,gpu-kern-exec \
  --format csv \
  --force-overwrite \
  --force-export=true \
  --output "${SUMMARY_REPORT}" \
  "${REPLAY_FILE}" || echo "警告: 某些报告类型可能不可用（可能没有 GPU 数据）"

echo "[nsys] 导出 CPU 采样报告 (全部函数 -> ${CPU_REPORT_ALL})"
nsys stats \
  --report cpu-profiling \
  --format csv \
  --force-export=true \
  "${REPLAY_FILE}" > "${CPU_REPORT_ALL}" || echo "警告: CPU 采样报告生成失败"

echo "[nsys] 过滤出 patch_gpu.go 相关采样 -> ${CPU_REPORT_PATCH}"
grep "patch_gpu.go" "${CPU_REPORT_ALL}" > "${CPU_REPORT_PATCH}" || true

cat <<EOF
完成。关键输出：
  - Nsight Systems 原始数据: ${REPLAY_FILE}
  - 概览报告 (CSV): ${SUMMARY_REPORT}.txt
  - CPU 采样 (全部): ${CPU_REPORT_ALL}
  - CPU 采样 (patch_gpu.go 筛选): ${CPU_REPORT_PATCH}

可以使用以下命令打开 GUI 查看时间线:
  nsys-ui ${REPLAY_FILE}
EOF

