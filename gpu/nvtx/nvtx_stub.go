//go:build !icicle

package nvtx

// Stub implementations when icicle build tag is not present

const (
	ColorCommit      = 0xFF00FF00
	ColorDeviceStage = 0xFF1E90FF
)

type nvtxRangeId uint64

func RangeStart(name string, color uint32) nvtxRangeId {
	return 0
}

func RangeEnd(id nvtxRangeId) {
	// No-op
}

func RangePush(name string) {
	// No-op
}

func RangePop() {
	// No-op
}

func Scope(name string, color uint32) func() {
	return func() {
		// No-op
	}
}
