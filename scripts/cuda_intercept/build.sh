#!/usr/bin/env bash
# 编译 CUDA 拦截库

set -euo pipefail

SCRIPT_DIR="$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")" >/dev/null 2>&1 && pwd)"
OUTPUT_DIR="${SCRIPT_DIR}"

# 检查 CUDA 是否可用
if ! command -v nvcc &> /dev/null; then
    echo "[错误] nvcc 未找到。请确保 CUDA 已安装并配置在 PATH 中。"
    exit 1
fi

# 检查 CUDA 头文件
CUDA_INCLUDE="/usr/local/cuda/include"
CUDA_LIB="/usr/local/cuda/targets/x86_64-linux/lib"
if [ ! -d "${CUDA_INCLUDE}" ]; then
    # 尝试其他常见位置
    CUDA_INCLUDE="/opt/cuda/include"
    CUDA_LIB="/opt/cuda/lib64"
    if [ ! -d "${CUDA_INCLUDE}" ]; then
        echo "[警告] 未找到 CUDA 头文件目录，尝试使用系统默认路径"
        CUDA_INCLUDE=""
        CUDA_LIB=""
    fi
fi

LIBRARY_NAME="libcuda_intercept.so"
SOURCE_FILE="${SCRIPT_DIR}/cuda_intercept.c"
OUTPUT_FILE="${OUTPUT_DIR}/${LIBRARY_NAME}"

echo "[构建] 编译 CUDA 拦截库..."
echo "[构建] 源文件: ${SOURCE_FILE}"
echo "[构建] 输出文件: ${OUTPUT_FILE}"

# 编译共享库
# 注意：libcuda 是驱动库，通常由系统提供，不需要显式链接
if [ -n "${CUDA_INCLUDE}" ] && [ -n "${CUDA_LIB}" ]; then
    gcc -shared -fPIC -o "${OUTPUT_FILE}" "${SOURCE_FILE}" \
        -I"${CUDA_INCLUDE}" \
        -L"${CUDA_LIB}" \
        -Wl,-rpath,"${CUDA_LIB}" \
        -ldl -lcudart \
        -Wall -Wextra -O2
else
    # 尝试使用系统默认路径
    gcc -shared -fPIC -o "${OUTPUT_FILE}" "${SOURCE_FILE}" \
        -ldl -lcudart \
        -Wall -Wextra -O2
fi

if [ $? -eq 0 ]; then
    echo "[构建] 成功！库文件: ${OUTPUT_FILE}"
    echo "[构建] 使用方法:"
    echo "  export LD_PRELOAD=${OUTPUT_FILE}"
    echo "  export CUDA_INTERCEPT_LOG=/path/to/log.txt  # 可选，默认输出到 stderr"
    echo "  your_program"
else
    echo "[错误] 编译失败"
    exit 1
fi

