#!/usr/bin/env bash
# Usage:
#   scripts/profile_mimchasher_intercept.sh
# 该脚本使用 LD_PRELOAD 拦截 CUDA API 调用，分析内存布局和分配模式。
# 特别关注：Icicle 在哪里分配内存？分配了多大？数据是否被小块多次传输？

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
OUT_PREFIX="${OUTPUT_DIR}/mimchasher_intercept_${TIMESTAMP}"
LOG_FILE="${OUT_PREFIX}.log"
SUMMARY_FILE="${OUT_PREFIX}_summary.txt"

mkdir -p "${OUTPUT_DIR}"

cd "${ROOT_DIR}"

# 加载环境变量配置（如果存在 .envrc）
if [ -f "${ROOT_DIR}/.envrc" ]; then
  echo "[env] 加载 .envrc 配置..."
  set +e  # 临时关闭错误退出，因为 source 可能失败
  source "${ROOT_DIR}/.envrc" 2>/dev/null || true
  set -e  # 恢复错误退出
fi

# 检查并构建拦截库
INTERCEPT_DIR="${SCRIPT_DIR}/cuda_intercept"
INTERCEPT_LIB="${INTERCEPT_DIR}/libcuda_intercept.so"

if [ ! -f "${INTERCEPT_LIB}" ]; then
  echo "[构建] 拦截库不存在，正在构建..."
  cd "${INTERCEPT_DIR}"
  bash build.sh
  cd "${ROOT_DIR}"
fi

if [ ! -f "${INTERCEPT_LIB}" ]; then
  echo "[错误] 拦截库构建失败: ${INTERCEPT_LIB}"
  exit 1
fi

echo "[拦截] 使用 LD_PRELOAD 拦截 CUDA API 调用"
echo "[拦截] 拦截库: ${INTERCEPT_LIB}"
echo "[拦截] 日志文件: ${LOG_FILE}"
echo "[拦截] 摘要文件: ${SUMMARY_FILE}"
echo ""
echo "[拦截] 将记录以下信息："
echo "  - cudaMalloc: 分配大小、地址"
echo "  - cudaMemcpy: 传输方向(H2D/D2H)、大小、源/目标地址"
echo "  - cudaFree: 释放的地址"
echo "  - cudaDeviceSynchronize: 同步点"
echo ""

# 设置环境变量
export LD_PRELOAD="${INTERCEPT_LIB}"
export CUDA_INTERCEPT_LOG="${LOG_FILE}"

# 运行程序并捕获输出
echo "[拦截] 开始运行程序..."
echo "==========================================" >> "${LOG_FILE}"
echo "Program: go run -tags icicle ./examples/mimchasher/main.go -count=1 -v" >> "${LOG_FILE}"
echo "Timestamp: $(date)" >> "${LOG_FILE}"
echo "==========================================" >> "${LOG_FILE}"

# 运行程序，同时将输出也保存到日志
go run -tags icicle ./examples/mimchasher/main.go -count=1 -v 2>&1 | tee -a "${LOG_FILE}"

# 提取摘要信息
echo "[拦截] 提取摘要信息..."
{
  echo "=== CUDA API 拦截摘要 ==="
  echo "生成时间: $(date)"
  echo "日志文件: ${LOG_FILE}"
  echo ""
  
  # 提取统计信息（从日志末尾的 Summary 部分）
  if grep -q "=== Summary ===" "${LOG_FILE}"; then
    grep -A 20 "=== Summary ===" "${LOG_FILE}" | tail -n +2
  else
    echo "警告: 未找到摘要信息"
  fi
  
  echo ""
  echo "=== 内存分配模式分析 ==="
  
  # 分析分配大小分布
  echo ""
  echo "分配大小分布（前10个最大的分配）:"
  grep "cudaMalloc:" "${LOG_FILE}" | grep -v "FAILED" | \
    sed 's/.*size=\([0-9]*\) bytes.*/\1/' | \
    sort -rn | head -10 | \
    awk '{printf "  %d bytes (%.2f KB, %.2f MB)\n", $1, $1/1024, $1/1024/1024}'
  
  echo ""
  echo "传输大小分布（前10个最大的传输）:"
  grep "cudaMemcpy:" "${LOG_FILE}" | grep -v "FAILED" | \
    sed 's/.*size=\([0-9]*\) bytes.*/\1/' | \
    sort -rn | head -10 | \
    awk '{printf "  %d bytes (%.2f KB, %.2f MB)\n", $1, $1/1024, $1/1024/1024}'
  
  echo ""
  echo "传输方向统计:"
  H2D_COUNT=$(grep -c "cudaMemcpy.*H2D" "${LOG_FILE}" || echo "0")
  D2H_COUNT=$(grep -c "cudaMemcpy.*D2H" "${LOG_FILE}" || echo "0")
  D2D_COUNT=$(grep -c "cudaMemcpy.*D2D" "${LOG_FILE}" || echo "0")
  echo "  Host to Device (H2D): ${H2D_COUNT}"
  echo "  Device to Host (D2H): ${D2H_COUNT}"
  echo "  Device to Device (D2D): ${D2D_COUNT}"
  
  echo ""
  echo "同步点统计:"
  SYNC_COUNT=$(grep -c "cudaDeviceSynchronize\|cudaStreamSynchronize" "${LOG_FILE}" || echo "0")
  echo "  总同步次数: ${SYNC_COUNT}"
  
  echo ""
  echo "=== 小传输分析（可能的内存碎片化） ==="
  SMALL_TRANSFERS=$(grep "cudaMemcpy.*H2D" "${LOG_FILE}" | \
    sed 's/.*size=\([0-9]*\) bytes.*/\1/' | \
    awk '$1 < 1024*1024' | wc -l)
  echo "小于 1MB 的 H2D 传输次数: ${SMALL_TRANSFERS}"
  
  echo ""
  echo "=== 详细日志 ==="
  echo "查看完整日志: cat ${LOG_FILE}"
  
} > "${SUMMARY_FILE}"

cat "${SUMMARY_FILE}"

cat <<EOF

完成。关键输出：
  - 拦截日志: ${LOG_FILE}
  - 摘要报告: ${SUMMARY_FILE}

⚠️  重要提示：
  如果拦截库没有捕获到 CUDA API 调用，可能的原因：
  1. Icicle 通过 CGO 静态链接 CUDA，LD_PRELOAD 无法拦截
  2. Icicle 使用 Rust/C++ 绑定，绕过了标准的 CUDA Runtime API
  3. 内存分配在库内部完成，不经过标准的 CUDA API

替代方案：
  1. 查看程序日志中的 [ICICLE] memory 信息（已在日志中）
  2. 使用 nsys 进行系统级分析: scripts/profile_mimchasher.sh
  3. 使用 ncu 进行内核级分析: scripts/profile_mimchasher_ncu.sh
  4. 在代码中添加内存跟踪（修改 icicle 包装器）

分析建议：
  1. 查看摘要报告了解总体情况:
     cat ${SUMMARY_FILE}

  2. 查看所有内存分配:
     grep "cudaMalloc:" ${LOG_FILE}

  3. 查看所有内存传输:
     grep "cudaMemcpy:" ${LOG_FILE}

  4. 查看同步点:
     grep "cudaDeviceSynchronize\|cudaStreamSynchronize" ${LOG_FILE}

  5. 分析小传输（可能的内存碎片化）:
     grep "cudaMemcpy.*H2D" ${LOG_FILE} | grep "size=.*bytes" | \
       awk -F'size=' '{print \$2}' | awk '{print \$1}' | \
       awk '\$1 < 1024*1024' | wc -l

  6. 查找最大的分配:
     grep "cudaMalloc:" ${LOG_FILE} | sort -t'=' -k2 -rn | head -10

提示：
  - 如果看到大量小传输（<1MB），可能存在内存碎片化问题
  - 检查分配大小是否合理，避免过度分配
  - 注意 H2D 和 D2H 的传输比例，过多的 D2H 可能影响性能
  - 同步点过多可能表示 GPU 利用率不足
EOF

