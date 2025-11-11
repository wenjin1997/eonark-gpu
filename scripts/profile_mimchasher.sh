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
  --trace=cuda,osrt,nvtx \
  --cuda-memory-usage=true \
  --cuda-trace-all-apis=true \
  go run -tags icicle ./examples/mimchasher/main.go -count=1 -v

echo "[nsys] 生成概览报告 (${SUMMARY_REPORT}.txt)"
nsys stats \
  --report summary,gpu-kern-summary,cuda-api,gpu-kern-exec \
  --format csv \
  --force-overwrite \
  --force-export=true \
  --output "${SUMMARY_REPORT}" \
  "${REPLAY_FILE}" || echo "警告: 某些报告类型可能不可用（可能没有 GPU 数据）"

echo "[nsys] 导出 GPU 内核执行报告"
GPU_KERN_REPORT="${OUT_PREFIX}_gpu_kernels.csv"
nsys stats \
  --report gpu-kern-summary \
  --format csv \
  --force-export=true \
  "${REPLAY_FILE}" > "${GPU_KERN_REPORT}" 2>&1 || echo "警告: GPU 内核报告生成失败"

echo "[nsys] 导出 CUDA API 调用报告"
CUDA_API_REPORT="${OUT_PREFIX}_cuda_api.csv"
nsys stats \
  --report cuda-api \
  --format csv \
  --force-export=true \
  "${REPLAY_FILE}" > "${CUDA_API_REPORT}" 2>&1 || echo "警告: CUDA API 报告生成失败"

echo "[nsys] 导出 CPU 采样报告 (全部函数 -> ${CPU_REPORT_ALL})"
nsys stats \
  --report cpu-profiling \
  --format csv \
  --force-export=true \
  "${REPLAY_FILE}" > "${CPU_REPORT_ALL}" || echo "警告: CPU 采样报告生成失败"

echo "[nsys] 过滤出 patch_gpu.go 相关采样 -> ${CPU_REPORT_PATCH}"
grep "patch_gpu.go" "${CPU_REPORT_ALL}" > "${CPU_REPORT_PATCH}" || true

echo "[nsys] 分析 GPU vs CPU 时间占比"
GPU_TIME_ANALYSIS="${OUT_PREFIX}_gpu_time_analysis.txt"
{
  echo "=== GPU vs CPU 时间占比分析 ==="
  echo ""
  
  # 尝试从 GPU 内核报告中提取总时间
  if [ -f "${GPU_KERN_REPORT}" ] && ! grep -q "ERROR\|could not be found" "${GPU_KERN_REPORT}"; then
    echo "GPU 内核执行时间（从 gpu-kern-summary 报告）:"
    # 查找包含 "Total Time" 或类似的行
    grep -i "total\|sum\|duration" "${GPU_KERN_REPORT}" | head -5 || echo "  未找到总时间信息"
    echo ""
  else
    echo "警告: GPU 内核报告不可用，可能没有 GPU 活动"
    echo ""
  fi
  
  # 从 CUDA API 报告中提取信息
  if [ -f "${CUDA_API_REPORT}" ] && ! grep -q "ERROR\|could not be found" "${CUDA_API_REPORT}"; then
    echo "CUDA API 调用统计（从 cuda-api 报告）:"
    grep -v "^#" "${CUDA_API_REPORT}" | head -10 || echo "  未找到 API 调用信息"
    echo ""
  else
    echo "警告: CUDA API 报告不可用"
    echo ""
  fi
  
  echo "提示:"
  echo "  - CPU 采样报告显示的是 CPU 执行时间，不包括 GPU 等待时间"
  echo "  - GPU 内核执行时间在 GPU 内核报告中"
  echo "  - 要查看完整的 GPU/CPU 占比，请使用 nsys-ui 打开 ${REPLAY_FILE}"
  echo "  - 在 GUI 中，GPU 活动显示在 'CUDA API' 和 'GPU' 时间线行中"
  echo ""
  echo "建议:"
  echo "  1. 运行: nsys-ui ${REPLAY_FILE}"
  echo "  2. 在时间线视图中查看 'GPU' 行的活动条"
  echo "  3. 选中 GPU 活动区域，查看详细信息"
} > "${GPU_TIME_ANALYSIS}"
cat "${GPU_TIME_ANALYSIS}"

cat <<EOF
完成。关键输出：
  - Nsight Systems 原始数据: ${REPLAY_FILE}
  - 概览报告 (CSV): ${SUMMARY_REPORT}.txt
  - GPU 内核执行报告: ${GPU_KERN_REPORT}
  - CUDA API 调用报告: ${CUDA_API_REPORT}
  - GPU/CPU 时间占比分析: ${GPU_TIME_ANALYSIS}
  - CPU 采样 (全部): ${CPU_REPORT_ALL}
  - CPU 采样 (patch_gpu.go 筛选): ${CPU_REPORT_PATCH}

查看 GPU 调用情况：
  1. 使用 GUI 查看时间线（推荐）:
     nsys-ui ${REPLAY_FILE}
     在 GUI 中查看 "CUDA API" 和 "GPU" 行，可以看到 GPU 内核执行时间

  2. 查看 GPU 内核报告:
     cat ${GPU_KERN_REPORT}

  3. 查看 CUDA API 调用:
     cat ${CUDA_API_REPORT}

提示：在 nsys-ui 中，GPU 活动会显示在时间线的 "CUDA API" 和 "GPU" 行中。
     如果看不到 GPU 活动，可能是程序没有实际调用 GPU，或者 GPU 调用被 icicle 库封装了。
EOF

