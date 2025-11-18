#!/usr/bin/env bash
# Usage:
#   scripts/profile_mimchasher_ncu.sh
# 该脚本会调用 ncu (NVIDIA Compute Profiler) 对 `go run -tags icicle ./examples/mimchasher/main.go -count=1 -v`
# 的执行过程进行 GPU 内核级别的性能分析，特别关注内存布局和访问模式。
# ncu 可以分析内存合并、bank conflicts、内存带宽利用率等。

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."
GPU_NAME="$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -n 1 || echo "")"
if [[ "$GPU_NAME" == *"5070"* ]]; then
  OUTPUT_DIR="${ROOT_DIR}/logs/gpu-5070/profiles/"
elif [[ "$GPU_NAME" == *"4090"* ]]; then
  OUTPUT_DIR="${ROOT_DIR}/logs/gpu-4090/profiles/"
else
  OUTPUT_DIR="${ROOT_DIR}/logs/others/profiles/"
fi
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUT_PREFIX="${OUTPUT_DIR}/mimchasher_ncu_${TIMESTAMP}"
REPORT_FILE="${OUT_PREFIX}.ncu-rep"
CSV_FILE="${OUT_PREFIX}_memory.csv"
LOG_FILE="${OUT_PREFIX}.log"

mkdir -p "${OUTPUT_DIR}"

cd "${ROOT_DIR}"

# 加载环境变量配置（如果存在 .envrc）
if [ -f "${ROOT_DIR}/.envrc" ]; then
  echo "[env] 加载 .envrc 配置..."
  set +e  # 临时关闭错误退出，因为 source 可能失败
  source "${ROOT_DIR}/.envrc" 2>/dev/null || true
  set -e  # 恢复错误退出
fi

# 检查 ncu 是否可用
if ! command -v ncu &> /dev/null; then
  echo "[错误] ncu 未找到。请安装 NVIDIA Nsight Compute:"
  echo "  https://developer.nvidia.com/nsight-compute"
  exit 1
fi

echo "[ncu] Profiling Go workload with NVIDIA Compute Profiler"
echo "[ncu] Output prefix: ${OUT_PREFIX}"
echo "[ncu] Log file: ${LOG_FILE}"
echo "[ncu] 分析内存布局和访问模式..."
echo "[ncu] 注意：ncu 会分析每个 CUDA 内核的内存访问模式、合并情况、bank conflicts 等"

# ncu 命令选项说明：
# --set full: 启用所有分析指标
# --section MemoryWorkloadAnalysis: 重点分析内存工作负载
# --section MemoryWorkloadAnalysis_Chart: 内存工作负载图表
# --section MemoryWorkloadAnalysis_Tables: 内存工作负载表格
# --section Occupancy: 占用率分析
# --section SpeedOfLight: 性能瓶颈分析
# --export: 导出报告文件
# --target-processes all: 分析所有进程
# --print-gpu-trace: 打印 GPU 跟踪信息
# --force-overwrite: 覆盖已存在的文件
# --log-file: 指定日志文件

# 使用更轻量的配置，避免分析所有内核导致卡顿
# 如果只需要内存分析，可以只启用 MemoryWorkloadAnalysis 相关选项
ncu \
  --set default \
  --section MemoryWorkloadAnalysis \
  --section MemoryWorkloadAnalysis_Chart \
  --section MemoryWorkloadAnalysis_Tables \
  --export "${REPORT_FILE}" \
  --target-processes all \
  --force-overwrite \
  --log-file "${LOG_FILE}" \
  --print-gpu-trace \
  go run -tags icicle ./examples/mimchasher/main.go -count=1 -v 2>&1 | tee -a "${LOG_FILE}"

# 检查报告文件是否生成
if [ -f "${REPORT_FILE}" ]; then
  echo "[ncu] 报告文件已生成: ${REPORT_FILE}"
  
  # 尝试导出 CSV 格式的内存分析数据
  echo "[ncu] 导出内存分析 CSV..."
  ncu --import "${REPORT_FILE}" \
      --section MemoryWorkloadAnalysis_Tables \
      --csv \
      --page raw \
      > "${CSV_FILE}" 2>/dev/null || {
    echo "[警告] CSV 导出失败，但报告文件仍可用于分析"
  }
  
  if [ -f "${CSV_FILE}" ]; then
    echo "[ncu] CSV 文件已生成: ${CSV_FILE}"
  fi
else
  echo "[警告] 报告文件未生成，请检查日志: ${LOG_FILE}"
fi

cat <<EOF
完成。关键输出：
  - Nsight Compute 报告: ${REPORT_FILE}
  - 内存分析 CSV: ${CSV_FILE}
  - 运行日志: ${LOG_FILE}

查看内存布局分析：
  1. 使用 GUI 查看详细报告（推荐）:
     ncu-ui ${REPORT_FILE}
     在 GUI 中可以查看：
     - Memory Workload Analysis: 内存访问模式、合并情况
     - Memory Workload Analysis Chart: 内存访问可视化
     - Memory Workload Analysis Tables: 详细的内存统计表格
     - Occupancy: SM 占用率
     - Speed of Light: 性能瓶颈分析

  2. 使用命令行查看摘要:
     ncu --import ${REPORT_FILE} --section MemoryWorkloadAnalysis --print-summary per-kernel

  3. 查看内存访问模式:
     ncu --import ${REPORT_FILE} --section MemoryWorkloadAnalysis_Tables --print-summary per-kernel

  4. 导出特定部分到 CSV:
     ncu --import ${REPORT_FILE} --section MemoryWorkloadAnalysis_Tables --csv --page raw > memory_analysis.csv

  5. 查看运行日志:
     cat ${LOG_FILE}

提示：
  - Memory Workload Analysis 会显示内存访问的合并情况（coalesced vs uncoalesced）
  - Memory Workload Analysis Chart 提供内存访问的可视化图表
  - 检查 "Memory Throughput" 和 "Memory Bandwidth Utilization" 来评估内存性能
  - 查看 "L1/TEX Cache Hit Rate" 来了解缓存效率
EOF

