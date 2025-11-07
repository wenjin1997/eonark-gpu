package gpu

import (
    "fmt"
    "sync/atomic"
    "time"
)

// package-level lightweight profiler for CPU/GPU time accounting

type profilerTotals struct {
    totalNs int64
    gpuNs   int64
    enabled int32
}

var profiler profilerTotals

func ProfilerEnable(on bool) {
    if on {
        atomic.StoreInt32(&profiler.enabled, 1)
    } else {
        atomic.StoreInt32(&profiler.enabled, 0)
    }
}

func ProfilerReset() {
    atomic.StoreInt64(&profiler.totalNs, 0)
    atomic.StoreInt64(&profiler.gpuNs, 0)
}

func profilerIsOn() bool {
    return atomic.LoadInt32(&profiler.enabled) == 1
}

// profilerStart returns a non-zero time when enabled, otherwise zero time.
func profilerStart() time.Time {
    if !profilerIsOn() {
        return time.Time{}
    }
    return time.Now()
}

// profilerAddGPU adds the elapsed GPU duration since start when enabled.
func profilerAddGPU(start time.Time) {
    if !profilerIsOn() || start.IsZero() {
        return
    }
    d := time.Since(start)
    atomic.AddInt64(&profiler.gpuNs, d.Nanoseconds())
}

// profileTotalAdd adds wall time for the whole operation (usually once).
func profileTotalAdd(d time.Duration) {
    if !profilerIsOn() {
        return
    }
    atomic.AddInt64(&profiler.totalNs, d.Nanoseconds())
}

func ProfilerReport() {
    if !profilerIsOn() {
        return
    }
    t := atomic.LoadInt64(&profiler.totalNs)
    g := atomic.LoadInt64(&profiler.gpuNs)
    if t == 0 {
        // fall back to sum when total wasn't explicitly recorded
        t = g
    }
    if t < 0 {
        t = 0
    }
    if g < 0 {
        g = 0
    }
    c := t - g
    if c < 0 {
        c = 0
    }
    var gp, cp float64
    if t > 0 {
        gp = float64(g) / float64(t) * 100
        cp = float64(c) / float64(t) * 100
    }
    fmt.Printf("[Profiler] total=%.3f ms  gpu=%.3f ms (%.2f%%)  cpu=%.3f ms (%.2f%%)\n",
        float64(t)/1e6, float64(g)/1e6, gp, float64(c)/1e6, cp,
    )
}


