//go:build icicle

package nvtx

/*
#cgo CFLAGS: -I/usr/local/cuda/include -I/usr/local/cuda/targets/x86_64-linux/include -I/opt/nvidia/nsight-systems/targets/x86_64-linux/include -I/opt/nvidia/nsight-systems/target-linux-x64/include -DNVTX_SUPPRESS_DEPRECATED_WARNING
#cgo LDFLAGS: -L/usr/local/cuda/lib64 -L/usr/local/cuda/targets/x86_64-linux/lib -lcudart
#include <nvtx3/nvToolsExt.h>
#include <stdlib.h>

nvtxRangeId_t goNvtxRangeStart(const char* name, unsigned int color) {
	nvtxEventAttributes_t attr = {0};
	attr.version = NVTX_VERSION;
	attr.size = NVTX_EVENT_ATTRIB_STRUCT_SIZE;
	attr.colorType = NVTX_COLOR_ARGB;
	attr.color = color;
	attr.messageType = NVTX_MESSAGE_TYPE_ASCII;
	attr.message.ascii = name;
	return nvtxRangeStartEx(&attr);
}
*/
import "C"

import (
	"unsafe"
)

const (
	// ColorCopyToDevice - 绿色，用于 H2D 拷贝
	ColorCopyToDevice = 0xFF00FF00
	// ColorCopyFromDevice - 红色，用于 D2H 拷贝
	ColorCopyFromDevice = 0xFFFF0000
	// ColorCommit - 青色，用于 KZG commit 操作
	ColorCommit = 0xFF00FFFF
	// ColorDeviceStage - 蓝色，用于设备阶段操作
	ColorDeviceStage = 0xFF1E90FF
	// ColorNTT - 黄色，用于 NTT 操作
	ColorNTT = 0xFFFFFF00
	// ColorMSM - 棕色，用于 MSM 操作
	ColorMSM = 0xFFA52A2A
	// ColorRunOnDevice - 橙色，用于 RunOnDevice 操作
	ColorRunOnDevice = 0xFFFFA500
	// ColorFunction - 紫色，用于普通函数操作
	ColorFunction = 0xFF800080
	// ColorSubFunction - 浅紫色，用于子函数操作
	ColorSubFunction = 0xFFDA70D6
)

// RangeStart 开始一个带颜色的 NVTX 范围，返回 range ID
func RangeStart(name string, color uint32) C.nvtxRangeId_t {
	cname := C.CString(name)
	id := C.goNvtxRangeStart(cname, C.uint(color))
	C.free(unsafe.Pointer(cname))
	return id
}

// RangeEnd 结束一个 NVTX 范围
func RangeEnd(id C.nvtxRangeId_t) {
	if id != 0 {
		C.nvtxRangeEnd(id)
	}
}

// RangePush 推送一个 NVTX 范围（使用默认颜色）
func RangePush(name string) {
	cname := C.CString(name)
	C.nvtxRangePushA(cname)
	C.free(unsafe.Pointer(cname))
}

// RangePop 弹出一个 NVTX 范围
func RangePop() {
	C.nvtxRangePop()
}

// Scope 返回一个函数，调用时自动结束 NVTX 范围（用于 defer）
func Scope(name string, color uint32) func() {
	id := RangeStart(name, color)
	return func() {
		RangeEnd(id)
	}
}
