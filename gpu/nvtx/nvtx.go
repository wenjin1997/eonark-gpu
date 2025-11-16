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
	ColorCommit      = 0xFF00FF00
	ColorDeviceStage = 0xFF1E90FF
)

func RangeStart(name string, color uint32) C.nvtxRangeId_t {
	cname := C.CString(name)
	id := C.goNvtxRangeStart(cname, C.uint(color))
	C.free(unsafe.Pointer(cname))
	return id
}

func RangeEnd(id C.nvtxRangeId_t) {
	if id != 0 {
		C.nvtxRangeEnd(id)
	}
}

func RangePush(name string) {
	cname := C.CString(name)
	C.nvtxRangePushA(cname)
	C.free(unsafe.Pointer(cname))
}

func RangePop() {
	C.nvtxRangePop()
}

func Scope(name string, color uint32) func() {
	id := RangeStart(name, color)
	return func() {
		RangeEnd(id)
	}
}

