//go:build !go1.27 || go1.28 || !goexperiment.simd || (!arm64 && !amd64) || (amd64 && !amd64.v3)

package kernels

// Stage1IndexBlocks classifies consecutive blocks and writes punctuation,
// scalar starts, and both quote boundaries as absolute source positions.
func Stage1IndexBlocks(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32) int {
	return stage1IndexBlocksPortable(p, nblocks, base, st, out, stage1PortableIndexFull, nil, nil)
}

// Stage1IndexBlocksMeta is Stage1IndexBlocks with per-block validation facts
// and optional first-chunk density totals.
func Stage1IndexBlocksMeta(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, meta *Stage1IndexMeta) int {
	return stage1IndexBlocksPortable(p, nblocks, base, st, out, stage1PortableIndexFull, nil, meta)
}
