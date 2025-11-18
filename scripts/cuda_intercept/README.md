# CUDA API 拦截库

使用 LD_PRELOAD 技术拦截 CUDA API 调用，分析内存分配和传输模式。

## 功能

拦截以下 CUDA API：
- `cudaMalloc`: 记录分配大小和地址
- `cudaMallocManaged`: 记录统一内存分配
- `cudaFree`: 记录释放的地址
- `cudaMemcpy`: 记录传输方向(H2D/D2H/D2D)、大小、源/目标地址
- `cudaMemcpyAsync`: 记录异步传输
- `cudaDeviceSynchronize`: 记录同步点
- `cudaStreamSynchronize`: 记录流同步点

## 编译

```bash
cd scripts/cuda_intercept
bash build.sh
```

这将生成 `libcuda_intercept.so` 共享库。

## 使用方法

### 方法 1: 使用提供的脚本（推荐）

```bash
scripts/profile_mimchasher_intercept.sh
```

脚本会自动：
1. 检查并构建拦截库（如果不存在）
2. 设置 LD_PRELOAD 环境变量
3. 运行程序并收集日志
4. 生成摘要报告

### 方法 2: 手动使用

```bash
# 设置环境变量
export LD_PRELOAD=/path/to/libcuda_intercept.so
export CUDA_INTERCEPT_LOG=/path/to/log.txt  # 可选，默认输出到 stderr

# 运行程序
your_program
```

## 输出格式

### 日志条目示例

```
[12.345678] cudaMalloc: ptr=0x7f8a1c000000 size=1048576 bytes (1024.00 KB) -> no error
[12.345789] cudaMemcpy: H2D src=0x7f8a1b000000 dst=0x7f8a1c000000 size=1048576 bytes (1024.00 KB) -> no error
[12.456789] cudaDeviceSynchronize: -> no error
[12.567890] cudaFree: ptr=0x7f8a1c000000 size=1048576 bytes (1024.00 KB) -> no error
```

### 摘要信息

程序退出时会自动打印摘要，包括：
- 总分配/释放次数和大小
- 总传输次数和大小
- 同步点次数
- 峰值内存使用
- 当前活跃分配

## 分析内存布局

拦截库可以帮助回答以下问题：

1. **Icicle 在哪里分配内存？**
   - 查看所有 `cudaMalloc` 调用，记录分配地址

2. **分配了多大？**
   - 查看分配大小统计
   - 识别最大的分配

3. **数据是否被小块多次传输？**
   - 统计小于某个阈值（如 1MB）的传输次数
   - 分析传输大小分布

4. **内存碎片化情况？**
   - 查看大量小分配 vs 少量大分配
   - 检查分配/释放模式

## 注意事项

- 拦截库会轻微影响程序性能（主要是日志记录开销）
- 线程安全：所有日志记录都使用互斥锁保护
- 内存跟踪：使用简单链表跟踪活跃分配，对于大量分配可能有性能影响
- 编译要求：需要 CUDA 开发库（cuda.h, cuda_runtime.h）

## 故障排除

### 编译错误：找不到 CUDA 头文件

确保 CUDA 已正确安装，并且头文件在标准位置：
- `/usr/local/cuda/include`
- `/opt/cuda/include`

或者修改 `build.sh` 中的 `CUDA_INCLUDE` 路径。

### 运行时错误：无法加载库

确保：
1. 库文件存在且可执行
2. 所有依赖库（libcuda.so, libcudart.so）可用
3. LD_PRELOAD 路径正确（使用绝对路径更安全）

### 没有输出

检查：
1. `CUDA_INTERCEPT_LOG` 环境变量是否设置
2. 程序是否实际调用了 CUDA API
3. 库是否正确加载（使用 `ldd libcuda_intercept.so` 检查依赖）

