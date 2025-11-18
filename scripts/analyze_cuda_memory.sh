#!/usr/bin/env bash
# 使用 CUDA 工具分析内存使用情况
# 替代 LD_PRELOAD 方案

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
ROOT_DIR="${SCRIPT_DIR}/.."

# 检查是否有 nvprof 或 ncu
if command -v ncu &> /dev/null; then
    echo "[分析] 使用 Nsight Compute (ncu) 分析内存..."
    # ncu 可以分析内存访问，但主要是针对内核的
    echo "[提示] ncu 主要用于内核分析，内存分配信息有限"
elif command -v nvprof &> /dev/null; then
    echo "[分析] 使用 nvprof 分析..."
    echo "[提示] nvprof 已弃用，建议使用 nsys 或 ncu"
else
    echo "[错误] 未找到 CUDA 分析工具"
    exit 1
fi

# 使用 nvidia-smi 监控内存使用
echo "[分析] 使用 nvidia-smi 监控 GPU 内存使用..."
echo "[提示] 这将显示 GPU 内存使用情况，但无法显示详细的分配信息"
echo ""
echo "运行程序时，GPU 内存使用会显示在程序的日志中（通过 icicle_runtime.GetAvailableMemory()）"
echo ""
echo "建议："
echo "1. 查看程序日志中的 [ICICLE] memory 信息"
echo "2. 使用 nsys 进行系统级分析（已有脚本：scripts/profile_mimchasher.sh）"
echo "3. 在代码中添加更详细的内存跟踪（修改 icicle 包装器）"

