#!/usr/bin/env bash
# Usage:
#   scripts/run_mimchasher.sh
# 该脚本直接运行 `go run -tags icicle ./examples/mimchasher/main.go -count=1 -v`
# 并将输出保存到日志文件中，不生成 nsys-rep 和 sqlite 文件。

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

echo "[run] 运行 Go workload，日志文件: ${LOG_FILE}"
echo "[run] 开始执行..."

# 直接运行程序，同时输出到终端和日志文件
go run -tags icicle ./examples/mimchasher/main.go -count=1 -v 2>&1 | tee "${LOG_FILE}"

cat <<EOF

完成。输出文件：
  - 运行日志: ${LOG_FILE}

查看日志：
  cat ${LOG_FILE}
  或
  less ${LOG_FILE}
EOF

