/**
 * CUDA API 拦截库
 * 
 * 使用 LD_PRELOAD 拦截 CUDA 内存分配和传输函数，记录：
 * - cudaMalloc: 分配大小、地址
 * - cudaMemcpy: 传输方向、大小、源/目标地址
 * - cudaFree: 释放的地址
 * - cudaDeviceSynchronize: 同步点
 * 
 * 编译: gcc -shared -fPIC -o libcuda_intercept.so cuda_intercept.c -ldl -lcuda
 * 使用: LD_PRELOAD=./libcuda_intercept.so your_program
 */

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>
#include <unistd.h>
#include <pthread.h>
#include <cuda_runtime.h>
#include <cuda.h>

// 取消 CUDA 头文件中的宏定义，避免与我们的函数冲突
#undef cuMemAlloc
#undef cuMemAlloc_v2
#undef cuMemFree
#undef cuMemFree_v2
#undef cuMemAllocManaged
#undef cuMemcpyHtoD
#undef cuMemcpyHtoD_v2
#undef cuMemcpyDtoH
#undef cuMemcpyDtoH_v2
#undef cuMemcpyDtoD
#undef cuMemcpyDtoD_v2
#undef cuCtxSynchronize

// 线程安全的输出锁
static pthread_mutex_t log_mutex = PTHREAD_MUTEX_INITIALIZER;

// 日志文件指针
static FILE *log_file = NULL;
static int log_initialized = 0;

// 统计信息
static struct {
    unsigned long long total_malloc_bytes;
    unsigned long long total_memcpy_bytes;
    unsigned long long total_free_bytes;
    unsigned long long active_allocations;
    unsigned long long max_active_allocations;
    unsigned long long active_bytes;
    unsigned long long max_active_bytes;
    unsigned long long malloc_count;
    unsigned long long memcpy_count;
    unsigned long long free_count;
    unsigned long long sync_count;
} stats = {0};

// 分配记录（简单链表）
struct allocation_record {
    void *ptr;
    size_t size;
    struct allocation_record *next;
};

static struct allocation_record *allocations = NULL;
static pthread_mutex_t alloc_mutex = PTHREAD_MUTEX_INITIALIZER;

// 初始化日志
static void init_log() {
    if (log_initialized) return;
    
    const char *log_path = getenv("CUDA_INTERCEPT_LOG");
    if (log_path) {
        log_file = fopen(log_path, "w");
    } else {
        log_file = stderr;
    }
    
    if (log_file) {
        fprintf(log_file, "=== CUDA API Intercept Log ===\n");
        fprintf(log_file, "PID: %d\n", getpid());
        fprintf(log_file, "Time format: [seconds.microseconds] since start\n\n");
        fflush(log_file);
    }
    
    log_initialized = 1;
}

// 获取高精度时间戳（秒）
static double get_timestamp() {
    static struct timespec start_time = {0};
    static int initialized = 0;
    
    struct timespec now;
    clock_gettime(CLOCK_MONOTONIC, &now);
    
    if (!initialized) {
        start_time = now;
        initialized = 1;
    }
    
    double elapsed = (now.tv_sec - start_time.tv_sec) + 
                     (now.tv_nsec - start_time.tv_nsec) / 1e9;
    return elapsed;
}

// 记录分配
static void record_allocation(void *ptr, size_t size) {
    pthread_mutex_lock(&alloc_mutex);
    
    struct allocation_record *rec = malloc(sizeof(struct allocation_record));
    rec->ptr = ptr;
    rec->size = size;
    rec->next = allocations;
    allocations = rec;
    
    stats.active_allocations++;
    stats.active_bytes += size;
    
    if (stats.active_allocations > stats.max_active_allocations) {
        stats.max_active_allocations = stats.active_allocations;
    }
    if (stats.active_bytes > stats.max_active_bytes) {
        stats.max_active_bytes = stats.active_bytes;
    }
    
    pthread_mutex_unlock(&alloc_mutex);
}

// 移除分配记录
static size_t remove_allocation(void *ptr) {
    pthread_mutex_lock(&alloc_mutex);
    
    struct allocation_record **prev = &allocations;
    struct allocation_record *curr = allocations;
    size_t size = 0;
    
    while (curr) {
        if (curr->ptr == ptr) {
            size = curr->size;
            *prev = curr->next;
            free(curr);
            stats.active_allocations--;
            stats.active_bytes -= size;
            break;
        }
        prev = &curr->next;
        curr = curr->next;
    }
    
    pthread_mutex_unlock(&alloc_mutex);
    return size;
}

// 打印统计摘要
static void print_summary() {
    pthread_mutex_lock(&log_mutex);
    
    init_log();
    
    if (log_file) {
        fprintf(log_file, "\n=== Summary ===\n");
        fprintf(log_file, "Total cudaMalloc calls: %llu\n", stats.malloc_count);
        fprintf(log_file, "Total allocated: %.2f MB\n", stats.total_malloc_bytes / (1024.0 * 1024.0));
        fprintf(log_file, "Total cudaMemcpy calls: %llu\n", stats.memcpy_count);
        fprintf(log_file, "Total copied: %.2f MB\n", stats.total_memcpy_bytes / (1024.0 * 1024.0));
        fprintf(log_file, "Total cudaFree calls: %llu\n", stats.free_count);
        fprintf(log_file, "Total freed: %.2f MB\n", stats.total_free_bytes / (1024.0 * 1024.0));
        fprintf(log_file, "Total cudaDeviceSynchronize calls: %llu\n", stats.sync_count);
        fprintf(log_file, "Max active allocations: %llu\n", stats.max_active_allocations);
        fprintf(log_file, "Current active allocations: %llu\n", stats.active_allocations);
        fprintf(log_file, "Current active memory: %.2f MB\n", stats.active_bytes / (1024.0 * 1024.0));
        fprintf(log_file, "Peak memory usage: %.2f MB\n", stats.max_active_bytes / (1024.0 * 1024.0));
        fflush(log_file);
    }
    
    pthread_mutex_unlock(&log_mutex);
}

// 在程序启动时初始化（用于验证库是否被加载）
static void __attribute__((constructor)) init() {
    init_log();
    if (log_file) {
        fprintf(log_file, "[INIT] CUDA intercept library loaded successfully\n");
        fprintf(log_file, "[INIT] PID: %d\n", getpid());
        fflush(log_file);
    }
}

// 在程序退出时打印摘要
static void __attribute__((destructor)) cleanup() {
    print_summary();
    if (log_file && log_file != stderr) {
        fclose(log_file);
    }
}

// 获取原始函数指针（Driver API 版本，不 abort）
#define GET_ORIGINAL_FUNC_DRIVER(name) \
    static typeof(name) *orig_##name = NULL; \
    if (!orig_##name) { \
        orig_##name = (typeof(name)*) dlsym(RTLD_NEXT, #name); \
        if (!orig_##name) { \
            if (log_file) { \
                fprintf(log_file, "[WARN] Failed to get original " #name ", may not be used\n"); \
                fflush(log_file); \
            } \
            return CUDA_ERROR_NOT_FOUND; \
        } \
    }

// 获取原始函数指针
#define GET_ORIGINAL_FUNC(name) \
    static typeof(name) *orig_##name = NULL; \
    if (!orig_##name) { \
        orig_##name = (typeof(name)*) dlsym(RTLD_NEXT, #name); \
        if (!orig_##name) { \
            fprintf(stderr, "Failed to get original " #name "\n"); \
            abort(); \
        } \
    }

// 拦截 cudaMalloc
cudaError_t cudaMalloc(void **devPtr, size_t size) {
    GET_ORIGINAL_FUNC(cudaMalloc);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    cudaError_t result = orig_cudaMalloc(devPtr, size);
    
    if (result == cudaSuccess) {
        stats.total_malloc_bytes += size;
        stats.malloc_count++;
        record_allocation(*devPtr, size);
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMalloc: ptr=%p size=%zu bytes (%.2f KB) -> %s\n",
                    ts, *devPtr, size, size / 1024.0, 
                    cudaGetErrorString(result));
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMalloc: FAILED size=%zu bytes -> %s\n",
                    ts, size, cudaGetErrorString(result));
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaMallocManaged
cudaError_t cudaMallocManaged(void **devPtr, size_t size, unsigned int flags) {
    GET_ORIGINAL_FUNC(cudaMallocManaged);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    cudaError_t result = orig_cudaMallocManaged(devPtr, size, flags);
    
    if (result == cudaSuccess) {
        stats.total_malloc_bytes += size;
        stats.malloc_count++;
        record_allocation(*devPtr, size);
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMallocManaged: ptr=%p size=%zu bytes (%.2f KB) flags=0x%x -> %s\n",
                    ts, *devPtr, size, size / 1024.0, flags,
                    cudaGetErrorString(result));
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMallocManaged: FAILED size=%zu bytes -> %s\n",
                    ts, size, cudaGetErrorString(result));
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaFree
cudaError_t cudaFree(void *devPtr) {
    GET_ORIGINAL_FUNC(cudaFree);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    size_t size = remove_allocation(devPtr);
    cudaError_t result = orig_cudaFree(devPtr);
    
    if (result == cudaSuccess) {
        stats.total_free_bytes += size;
        stats.free_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaFree: ptr=%p size=%zu bytes (%.2f KB) -> %s\n",
                    ts, devPtr, size, size / 1024.0,
                    cudaGetErrorString(result));
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaFree: FAILED ptr=%p -> %s\n",
                    ts, devPtr, cudaGetErrorString(result));
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaMemcpy
cudaError_t cudaMemcpy(void *dst, const void *src, size_t count, enum cudaMemcpyKind kind) {
    GET_ORIGINAL_FUNC(cudaMemcpy);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    const char *kind_str;
    switch (kind) {
        case cudaMemcpyHostToDevice: kind_str = "H2D"; break;
        case cudaMemcpyDeviceToHost: kind_str = "D2H"; break;
        case cudaMemcpyDeviceToDevice: kind_str = "D2D"; break;
        case cudaMemcpyHostToHost: kind_str = "H2H"; break;
        default: kind_str = "UNKNOWN"; break;
    }
    
    cudaError_t result = orig_cudaMemcpy(dst, src, count, kind);
    
    if (result == cudaSuccess) {
        stats.total_memcpy_bytes += count;
        stats.memcpy_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMemcpy: %s src=%p dst=%p size=%zu bytes (%.2f KB) -> %s\n",
                    ts, kind_str, src, dst, count, count / 1024.0,
                    cudaGetErrorString(result));
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMemcpy: FAILED %s size=%zu bytes -> %s\n",
                    ts, kind_str, count, cudaGetErrorString(result));
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaMemcpyAsync
cudaError_t cudaMemcpyAsync(void *dst, const void *src, size_t count, 
                            enum cudaMemcpyKind kind, cudaStream_t stream) {
    GET_ORIGINAL_FUNC(cudaMemcpyAsync);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    const char *kind_str;
    switch (kind) {
        case cudaMemcpyHostToDevice: kind_str = "H2D"; break;
        case cudaMemcpyDeviceToHost: kind_str = "D2H"; break;
        case cudaMemcpyDeviceToDevice: kind_str = "D2D"; break;
        case cudaMemcpyHostToHost: kind_str = "H2H"; break;
        default: kind_str = "UNKNOWN"; break;
    }
    
    cudaError_t result = orig_cudaMemcpyAsync(dst, src, count, kind, stream);
    
    if (result == cudaSuccess) {
        stats.total_memcpy_bytes += count;
        stats.memcpy_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMemcpyAsync: %s src=%p dst=%p size=%zu bytes (%.2f KB) stream=%p -> %s\n",
                    ts, kind_str, src, dst, count, count / 1024.0, stream,
                    cudaGetErrorString(result));
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cudaMemcpyAsync: FAILED %s size=%zu bytes -> %s\n",
                    ts, kind_str, count, cudaGetErrorString(result));
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaDeviceSynchronize
cudaError_t cudaDeviceSynchronize(void) {
    GET_ORIGINAL_FUNC(cudaDeviceSynchronize);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    cudaError_t result = orig_cudaDeviceSynchronize();
    
    stats.sync_count++;
    
    if (log_file) {
        fprintf(log_file, "[%.6f] cudaDeviceSynchronize: -> %s\n",
                ts, cudaGetErrorString(result));
        fflush(log_file);
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cudaStreamSynchronize
cudaError_t cudaStreamSynchronize(cudaStream_t stream) {
    GET_ORIGINAL_FUNC(cudaStreamSynchronize);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    cudaError_t result = orig_cudaStreamSynchronize(stream);
    
    stats.sync_count++;
    
    if (log_file) {
        fprintf(log_file, "[%.6f] cudaStreamSynchronize: stream=%p -> %s\n",
                ts, stream, cudaGetErrorString(result));
        fflush(log_file);
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

/**
 * CUDA Driver API 拦截函数
 * 追加到 cuda_intercept.c 文件末尾
 */

// ==================== CUDA Driver API 拦截 ====================

// 拦截 cuMemAlloc (Driver API)
CUresult cuMemAlloc(CUdeviceptr *dptr, size_t bytesize) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemAlloc);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuMemAlloc(dptr, bytesize);
    
    if (result == CUDA_SUCCESS) {
        stats.total_malloc_bytes += bytesize;
        stats.malloc_count++;
        record_allocation((void*)(uintptr_t)*dptr, bytesize);
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemAlloc: ptr=%p size=%zu bytes (%.2f KB) -> CUDA_SUCCESS\n",
                    ts, (void*)(uintptr_t)*dptr, bytesize, bytesize / 1024.0);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemAlloc: FAILED size=%zu bytes -> error=%d\n",
                    ts, bytesize, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuMemAllocManaged (Driver API)
CUresult cuMemAllocManaged(CUdeviceptr *dptr, size_t bytesize, unsigned int flags) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemAllocManaged);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuMemAllocManaged(dptr, bytesize, flags);
    
    if (result == CUDA_SUCCESS) {
        stats.total_malloc_bytes += bytesize;
        stats.malloc_count++;
        record_allocation((void*)(uintptr_t)*dptr, bytesize);
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemAllocManaged: ptr=%p size=%zu bytes (%.2f KB) flags=0x%x -> CUDA_SUCCESS\n",
                    ts, (void*)(uintptr_t)*dptr, bytesize, bytesize / 1024.0, flags);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemAllocManaged: FAILED size=%zu bytes -> error=%d\n",
                    ts, bytesize, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuMemFree (Driver API)
CUresult cuMemFree(CUdeviceptr dptr) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemFree);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    size_t size = remove_allocation((void*)(uintptr_t)dptr);
    CUresult result = orig_cuMemFree(dptr);
    
    if (result == CUDA_SUCCESS) {
        stats.total_free_bytes += size;
        stats.free_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemFree: ptr=%p size=%zu bytes (%.2f KB) -> CUDA_SUCCESS\n",
                    ts, (void*)(uintptr_t)dptr, size, size / 1024.0);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemFree: FAILED ptr=%p -> error=%d\n",
                    ts, (void*)(uintptr_t)dptr, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuMemcpyHtoD (Host to Device)
CUresult cuMemcpyHtoD(CUdeviceptr dstDevice, const void *srcHost, size_t ByteCount) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemcpyHtoD);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuMemcpyHtoD(dstDevice, srcHost, ByteCount);
    
    if (result == CUDA_SUCCESS) {
        stats.total_memcpy_bytes += ByteCount;
        stats.memcpy_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyHtoD: src=%p dst=%p size=%zu bytes (%.2f KB) -> CUDA_SUCCESS\n",
                    ts, srcHost, (void*)(uintptr_t)dstDevice, ByteCount, ByteCount / 1024.0);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyHtoD: FAILED size=%zu bytes -> error=%d\n",
                    ts, ByteCount, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuMemcpyDtoH (Device to Host)
CUresult cuMemcpyDtoH(void *dstHost, CUdeviceptr srcDevice, size_t ByteCount) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemcpyDtoH);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuMemcpyDtoH(dstHost, srcDevice, ByteCount);
    
    if (result == CUDA_SUCCESS) {
        stats.total_memcpy_bytes += ByteCount;
        stats.memcpy_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyDtoH: src=%p dst=%p size=%zu bytes (%.2f KB) -> CUDA_SUCCESS\n",
                    ts, (void*)(uintptr_t)srcDevice, dstHost, ByteCount, ByteCount / 1024.0);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyDtoH: FAILED size=%zu bytes -> error=%d\n",
                    ts, ByteCount, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuMemcpyDtoD (Device to Device)
CUresult cuMemcpyDtoD(CUdeviceptr dstDevice, CUdeviceptr srcDevice, size_t ByteCount) {
    GET_ORIGINAL_FUNC_DRIVER(cuMemcpyDtoD);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuMemcpyDtoD(dstDevice, srcDevice, ByteCount);
    
    if (result == CUDA_SUCCESS) {
        stats.total_memcpy_bytes += ByteCount;
        stats.memcpy_count++;
        
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyDtoD: src=%p dst=%p size=%zu bytes (%.2f KB) -> CUDA_SUCCESS\n",
                    ts, (void*)(uintptr_t)srcDevice, (void*)(uintptr_t)dstDevice, ByteCount, ByteCount / 1024.0);
            fflush(log_file);
        }
    } else {
        if (log_file) {
            fprintf(log_file, "[%.6f] cuMemcpyDtoD: FAILED size=%zu bytes -> error=%d\n",
                    ts, ByteCount, result);
            fflush(log_file);
        }
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}

// 拦截 cuCtxSynchronize (Driver API 同步)
CUresult cuCtxSynchronize(void) {
    GET_ORIGINAL_FUNC_DRIVER(cuCtxSynchronize);
    
    pthread_mutex_lock(&log_mutex);
    init_log();
    
    double ts = get_timestamp();
    CUresult result = orig_cuCtxSynchronize();
    
    stats.sync_count++;
    
    if (log_file) {
        const char *status = (result == CUDA_SUCCESS) ? "CUDA_SUCCESS" : "ERROR";
        fprintf(log_file, "[%.6f] cuCtxSynchronize: -> %s (error=%d)\n",
                ts, status, result);
        fflush(log_file);
    }
    
    pthread_mutex_unlock(&log_mutex);
    return result;
}


// ==================== CUDA Driver API v2 版本拦截 ====================
// 现代 CUDA 代码通常使用 _v2 版本

// 拦截 cuMemAlloc_v2
CUresult cuMemAlloc_v2(CUdeviceptr *dptr, size_t bytesize) {
    return cuMemAlloc(dptr, bytesize);
}

// 拦截 cuMemFree_v2
CUresult cuMemFree_v2(CUdeviceptr dptr) {
    return cuMemFree(dptr);
}

// 拦截 cuMemcpyHtoD_v2
CUresult cuMemcpyHtoD_v2(CUdeviceptr dstDevice, const void *srcHost, size_t ByteCount) {
    return cuMemcpyHtoD(dstDevice, srcHost, ByteCount);
}

// 拦截 cuMemcpyDtoH_v2
CUresult cuMemcpyDtoH_v2(void *dstHost, CUdeviceptr srcDevice, size_t ByteCount) {
    return cuMemcpyDtoH(dstHost, srcDevice, ByteCount);
}

// 拦截 cuMemcpyDtoD_v2
CUresult cuMemcpyDtoD_v2(CUdeviceptr dstDevice, CUdeviceptr srcDevice, size_t ByteCount) {
    return cuMemcpyDtoD(dstDevice, srcDevice, ByteCount);
}
