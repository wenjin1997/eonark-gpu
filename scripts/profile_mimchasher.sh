#!/usr/bin/env bash
# Usage:
#   scripts/profile_mimchasher.sh
# 该脚本会调用 nsys profile 对 `go run -tags icicle ./examples/mimchasher/main.go -count=1 -v`
# 的执行过程进行系统级采样，生成 .nsys-rep 和 .sqlite 文件。
# 使用增强的 GPU 追踪选项来捕获动态库中的 GPU 调用。

set -euo pipefail

ROOT_DIR="/home/jade/jade/eonark-gpu"
OUTPUT_DIR="${ROOT_DIR}/logs/profiles"
TIMESTAMP="$(date +%Y%m%d_%H%M%S)"
OUT_PREFIX="${OUTPUT_DIR}/mimchasher_${TIMESTAMP}"
REPLAY_FILE="${OUT_PREFIX}.nsys-rep"
SQLITE_FILE="${OUT_PREFIX}.sqlite"

mkdir -p "${OUTPUT_DIR}"

cd "${ROOT_DIR}"

echo "[nsys] Profiling Go workload, output前缀: ${OUT_PREFIX}"
echo "[nsys] 使用增强的 GPU 追踪选项来捕获动态库中的 GPU 调用..."
nsys profile \
  --output "${OUT_PREFIX}" \
  --force-overwrite=true \
  --sample=process-tree \
  --cpuctxsw=process-tree \
  --trace=cuda,osrt,nvtx \
  --cuda-memory-usage=true \
  --cuda-trace-all-apis=true \
  go run -tags icicle ./examples/mimchasher/main.go -count=1 -v

# 确保 SQLite 文件被导出（nsys profile 会自动生成，这里只是确认）
SQLITE_FILE="${OUT_PREFIX}.sqlite"
if [ -f "${SQLITE_FILE}" ]; then
  echo "[nsys] SQLite 文件已生成: ${SQLITE_FILE}"
else
  echo "[nsys] 强制导出 SQLite 文件..."
  nsys stats --force-export=true "${REPLAY_FILE}" > /dev/null 2>&1 || true
fi

cat <<EOF
完成。关键输出：
  - Nsight Systems 原始数据: ${REPLAY_FILE}
  - SQLite 数据库: ${SQLITE_FILE}

查看 GPU 调用情况：
  1. 使用 GUI 查看时间线（推荐）:
     nsys-ui ${REPLAY_FILE}
     在 GUI 中查看 "CUDA API" 和 "GPU" 行，可以看到 GPU 内核执行时间

  2. 使用 Python 脚本分析 SQLite 文件:
     python3 scripts/analyze_gpu_utilization.py ${SQLITE_FILE}

  3. 使用 nsys stats 提取报告:
     nsys stats --report gpu-kern-summary --format csv ${REPLAY_FILE}
     nsys stats --report cuda-api --format csv ${REPLAY_FILE}

提示：在 nsys-ui 中，GPU 活动会显示在时间线的 "CUDA API" 和 "GPU" 行中。
EOF

