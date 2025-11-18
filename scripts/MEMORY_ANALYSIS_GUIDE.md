# CUDA 内存布局分析指南

## 问题：LD_PRELOAD 无法拦截 Icicle 的 CUDA 调用

LD_PRELOAD 拦截库已创建并可以加载，但没有捕获到任何 CUDA API 调用。这是因为：

1. **Icicle 通过 CGO 调用**：Go 的 CGO 可能使用静态链接或直接调用，绕过了动态链接
2. **Rust/C++ 绑定**：Icicle 底层是 Rust/C++ 库，可能不经过标准的 CUDA Runtime API
3. **库内部分配**：内存分配可能在库内部完成，不暴露给外部拦截

## 替代方案

### 方案 1：使用程序日志中的内存信息（推荐）

程序已经在日志中输出了内存使用情况：

```bash
# 查看日志中的内存信息
grep "\[ICICLE\] memory" logs/gpu-5070/profiles/mimchasher_intercept_*.log
```

输出示例：
```
[ICICLE] memory: used=4156 MiB / total=12227 MiB (34.0%)
```

### 方案 2：使用 Nsight Systems (nsys) 进行系统级分析

```bash
scripts/profile_mimchasher.sh
```

nsys 可以：
- 显示 GPU 活动时间线
- 显示内存传输
- 显示内核执行时间

### 方案 3：使用 Nsight Compute (ncu) 进行内核级分析

```bash
scripts/profile_mimchasher_ncu.sh
```

ncu 可以：
- 分析内核内存访问模式
- 显示内存合并情况
- 分析内存带宽利用率

### 方案 4：在代码中添加内存跟踪

修改 `gpu/bls12381/bls12381_gpu.go` 中的 `printDeviceAndMemory()` 函数，添加更详细的内存跟踪：

```go
// 在每次内存操作前后记录
if mem, err := icicle_runtime.GetAvailableMemory(); err == icicle_runtime.Success {
    // 记录内存变化
    fmt.Printf("[MEMORY] Before: used=%.0f MiB\n", float64(mem.Total-mem.Free)/1024/1024)
}
// ... 执行操作 ...
if mem, err := icicle_runtime.GetAvailableMemory(); err == icicle_runtime.Success {
    fmt.Printf("[MEMORY] After: used=%.0f MiB\n", float64(mem.Total-mem.Free)/1024/1024)
}
```

### 方案 5：使用 nvidia-smi 监控

```bash
# 在另一个终端运行
watch -n 0.1 nvidia-smi
```

### 方案 6：修改 Icicle 包装器（高级）

如果需要详细的内存分配信息，可以：

1. 找到 Icicle 的 Go 包装器源码
2. 在 `CopyToDevice`、`Malloc` 等函数中添加日志
3. 记录每次分配的大小和地址

## 当前可用的信息

从程序日志中，您已经可以看到：

1. **内存使用情况**：`[ICICLE] memory: used=4156 MiB / total=12227 MiB (34.0%)`
2. **传输大小**：`OnDeviceCommit() || host 拷贝到 device 开始 (len=8388610)`
3. **MSM 规模**：`OnDeviceCommit() || 输入验证: nScalars=8388610`

这些信息可以帮助分析：
- 峰值内存使用：约 4156 MiB（34%）
- 数据传输大小：每次约 8388610 个元素（约 32 MB，假设每个元素 4 字节）
- MSM 操作规模：约 8.4M 个标量

## 建议

对于内存布局分析，建议：

1. **短期**：使用程序日志中的内存信息 + nsys 时间线分析
2. **中期**：在关键位置添加更详细的内存跟踪代码
3. **长期**：如果确实需要详细的分配信息，考虑修改 Icicle 包装器或使用其提供的调试接口

