//go:build go1.28 || !goexperiment.simd || !amd64

package kernels

func stage1BatchAvailable() bool { return false }
func stage1IndexBlocksAVX2(p *byte, n int, base uint32, st *Stage1IndexStream, out []uint32, mode int, vm *Stage1ValidMeta, im *Stage1IndexMeta) int {
	panic("unreachable AVX2")
}
func stage1BlocksAVX2(p *byte, n int, st *Stage1Stream, out *[Stage1ChunkBlocks]Stage1Rec) {
	panic("unreachable AVX2")
}
