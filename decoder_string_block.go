package vibejson

import "math/bits"

// newDecoderStringBlock allocates a pointer-free (noscan) byte slab for
// result-owned strings. Rounding up to power-of-two capacity classes up to
// 4 MiB matches runtime size classes while keeping the entire slab invisible
// to GC pointer scanning.
func newDecoderStringBlock(capacity int) []byte {
	const (
		minBlockCap = 16
		maxClassCap = 4 << 20
	)
	if capacity <= minBlockCap {
		return make([]byte, 0, minBlockCap)
	}
	if capacity <= maxClassCap {
		rounded := 1 << bits.Len(uint(capacity-1))
		return make([]byte, 0, rounded)
	}
	return make([]byte, 0, capacity)
}
