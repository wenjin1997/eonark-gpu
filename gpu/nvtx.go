//go:build icicle

package gpu

/*
#cgo CFLAGS: -I/usr/local/cuda/include -I/usr/local/cuda/targets/x86_64-linux/include -I/opt/nvidia/nsight-systems/targets/x86_64-linux/include -I/opt/nvidia/nsight-systems/target-linux-x64/include -DNVTX_SUPPRESS_DEPRECATED_WARNING
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
	nvtxColorCommit      = 0xFF00FF00
	nvtxColorDeviceStage = 0xFF1E90FF
)

func nvtxRangeStart(name string, color uint32) C.nvtxRangeId_t {
	cname := C.CString(name)
	id := C.goNvtxRangeStart(cname, C.uint(color))
	C.free(unsafe.Pointer(cname))
	return id
}

func nvtxRangeEnd(id C.nvtxRangeId_t) {
	if id != 0 {
		C.nvtxRangeEnd(id)
	}
}

func nvtxScope(name string, color uint32) func() {
	id := nvtxRangeStart(name, color)
	return func() {
		nvtxRangeEnd(id)
	}
}
