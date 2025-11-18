#!/usr/bin/env bash
# Usage:
#   scripts/profile_mimchasher.sh
# 该脚本会调用 nsys profile 对 `go run -tags icicle ./examples/mimchasher/main.go -count=1 -v`
# 的执行过程进行系统级采样，生成 .nsys-rep 和 .sqlite 文件。
# 使用增强的 GPU 追踪选项来捕获动态库中的 GPU 调用。

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
OUT_PREFIX="${OUTPUT_DIR}/mimchasher_${TIMESTAMP}"
REPLAY_FILE="${OUT_PREFIX}.nsys-rep"
SQLITE_FILE="${OUT_PREFIX}.sqlite"
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

echo "[nsys] Profiling Go workload, output前缀: ${OUT_PREFIX}"
echo "[nsys] 日志文件: ${LOG_FILE}"
echo "[nsys] 使用增强的 GPU 追踪选项来捕获动态库中的 GPU 调用..."
echo "[nsys] 注意：GPU 内存分配主要在 setupDevicePointers() 中发生"
echo "[nsys] 如果 nsys 显示的内存使用与 nvidia-smi 不符，可能是 icicle 使用了内存池或特殊的内存管理方式"
nsys profile \
  --output "${OUT_PREFIX}" \
  --force-overwrite=true \
  --sample=process-tree \
  --cpuctxsw=process-tree \
  --trace=cuda,osrt,nvtx \
  --cuda-memory-usage=true \
  --gpu-metrics-devices=all \
  --stats=true \
  go run -tags icicle ./examples/mimchasher/main.go -count=1 -v 2>&1 | tee "${LOG_FILE}"

# 确保 SQLite 文件被导出
if [ -f "${SQLITE_FILE}" ]; then
  echo "[nsys] SQLite 文件已自动生成: ${SQLITE_FILE}"
else
  echo "[nsys] 显式导出 SQLite 文件..."
  # 方法1: 使用 nsys export 命令
  if command -v nsys &> /dev/null; then
    nsys export --type sqlite --output "${SQLITE_FILE}" "${REPLAY_FILE}" 2>/dev/null || {
      # 方法2: 如果 export 失败，尝试使用 stats 强制导出
      echo "[nsys] 尝试使用 stats 导出 SQLite..."
      nsys stats --force-export=true --output "${OUT_PREFIX}_stats" "${REPLAY_FILE}" > /dev/null 2>&1 || true
      # 检查是否生成了 SQLite
      if [ ! -f "${SQLITE_FILE}" ]; then
        echo "[警告] SQLite 文件导出失败，但 .nsys-rep 文件仍可用于分析"
      fi
    }
  fi
  if [ -f "${SQLITE_FILE}" ]; then
    echo "[nsys] SQLite 文件已成功导出: ${SQLITE_FILE}"
  fi
fi

cat <<EOF
完成。关键输出：
  - Nsight Systems 原始数据: ${REPLAY_FILE}
  - SQLite 数据库: ${SQLITE_FILE}
  - 运行日志: ${LOG_FILE}

查看 GPU 调用情况：
  1. 使用 GUI 查看时间线（推荐）:
     nsys-ui ${REPLAY_FILE}
     在 GUI 中查看 "CUDA API" 和 "GPU" 行，可以看到 GPU 内核执行时间

  2. 使用 Python 脚本分析 SQLite 文件:
     python3 scripts/analyze_gpu_utilization.py ${SQLITE_FILE}

  3. 使用 nsys stats 提取报告:
     nsys stats --report gpu-kern-summary --format csv ${REPLAY_FILE}
     nsys stats --report cuda-api --format csv ${REPLAY_FILE}

  4. 查看运行日志:
     cat ${LOG_FILE}

提示：在 nsys-ui 中，GPU 活动会显示在时间线的 "CUDA API" 和 "GPU" 行中。
EOF

