#!/usr/bin/env bash
# Usage:
#   scripts/run_recursion.sh
# 该脚本会调用 nsys profile 对 `go test -tags icicle ./circuits/recursion -run Test_Recursion -count=1 -v`
# 的执行过程进行系统级采样，生成 .nsys-rep 和 .sqlite 文件。
# 使用增强的 GPU 追踪选项来捕获动态库中的 GPU 调用。

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."
GPU_NAME="$(nvidia-smi --query-gpu=name --format=csv,noheader 2>/dev/null | head -n 1 || echo "")"
if [[ "$GPU_NAME" == *"5070"* ]]; then
  OUTPUT_DIR="${ROOT_DIR}/logs/gpu-5070/"
elif [[ "$GPU_NAME" == *"4090"* ]]; then
  OUTPUT_DIR="${ROOT_DIR}/logs/gpu-4090/"
else
  OUTPUT_DIR="${ROOT_DIR}/logs/others/"
fi
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUT_PREFIX="${OUTPUT_DIR}/Test_Recursion_${TIMESTAMP}"
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
nsys profile \
  --output "${OUT_PREFIX}" \
  --force-overwrite=true \
  --sample=process-tree \
  --cpuctxsw=process-tree \
  --trace=cuda,osrt,nvtx \
  --cuda-memory-usage=true \
  --stats=true \
  go test -tags icicle ./circuits/recursion -run Test_Recursion -count=1 -v 2>&1 | tee "${LOG_FILE}"

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

# 分析 CopyToDevice 和 CopyFromDevice 时间
if [ -f "${SQLITE_FILE}" ]; then
  echo ""
  echo "[分析] 开始分析 CopyToDevice 和 CopyFromDevice 时间..."
  ANALYZE_SCRIPT="${SCRIPT_DIR}/analyze_copy_times.py"
  if [ -f "${ANALYZE_SCRIPT}" ]; then
    {
      echo ""
      echo "============================================================"
      echo "CopyToDevice 和 CopyFromDevice 时间分析"
      echo "============================================================"
      echo "分析时间: $(date '+%Y-%m-%d %H:%M:%S')"
      echo "SQLite 文件: ${SQLITE_FILE}"
      echo ""
      python3 "${ANALYZE_SCRIPT}" "${SQLITE_FILE}" 2>&1
      echo ""
      echo "============================================================"
      echo "分析完成"
      echo "============================================================"
    } | tee -a "${LOG_FILE}"
  else
    echo "[警告] 分析脚本不存在: ${ANALYZE_SCRIPT}" | tee -a "${LOG_FILE}"
  fi
  
  # 分析 RunOnDevice 时间
  echo ""
  echo "[分析] 开始分析 RunOnDevice 时间..."
  ANALYZE_RUNONDEVICE_SCRIPT="${SCRIPT_DIR}/analyze_runondevice_times.py"
  if [ -f "${ANALYZE_RUNONDEVICE_SCRIPT}" ]; then
    {
      echo ""
      echo "============================================================"
      echo "RunOnDevice 时间分析"
      echo "============================================================"
      echo "分析时间: $(date '+%Y-%m-%d %H:%M:%S')"
      echo "SQLite 文件: ${SQLITE_FILE}"
      echo ""
      python3 "${ANALYZE_RUNONDEVICE_SCRIPT}" "${SQLITE_FILE}" 2>&1
      echo ""
      echo "============================================================"
      echo "分析完成"
      echo "============================================================"
    } | tee -a "${LOG_FILE}"
  else
    echo "[警告] 分析脚本不存在: ${ANALYZE_RUNONDEVICE_SCRIPT}" | tee -a "${LOG_FILE}"
  fi
else
  echo "[警告] SQLite 文件不存在，跳过时间分析: ${SQLITE_FILE}" | tee -a "${LOG_FILE}"
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

  2. 使用 nsys stats 提取报告:
     nsys stats --report gpu-kern-summary --format csv ${REPLAY_FILE}
     nsys stats --report cuda-api --format csv ${REPLAY_FILE}

  3. 查看运行日志:
     cat ${LOG_FILE}

EOF