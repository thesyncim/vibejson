package vibejson

import (
	"encoding/binary"
	"unsafe"

	simdkernels "github.com/thesyncim/vibejson/x/kernels"
)

// buildIndexPositions writes a production index from a forward-only packed
// structural stream. Stage 1 emits exact quote boundaries; stage 2 consumes
// each position once and patches strings and containers at their closers.
func buildIndexPositions(src []byte, storage []IndexEntry) (entries []IndexEntry, fallbackNumberMode uint8, ok bool) {
	n := len(src)
	fullBlocks := n / 64

	var stream simdkernels.Stage1IndexStream
	var positions [simdkernels.Stage1ChunkBlocks*64 + 64]uint32
	var meta simdkernels.Stage1IndexMeta

	firstCount := 0
	firstWritten := 0
	if fullBlocks > 0 {
		firstCount = min(simdkernels.Stage1ChunkBlocks, fullBlocks)
		meta.Sample = true
		firstWritten = simdkernels.Stage1IndexBlocksMeta(
			src, firstCount, 0, &stream, positions[:], &meta,
		)
		if stream.Bad {
			return nil, tapeNumberScalar, false
		}
		if n >= validBitmapSampleBlocks*64 && !validBitmapSampleCommit(
			int(meta.WsCount), int(meta.EmitCount), int(meta.InStrCount), int(meta.EscCount),
		) {
			return nil, indexPositionsFallbackNumberMode(src, positions[:firstWritten], &meta), false
		}
	}
	entries, ok = buildIndexPositionsCommit(src, storage, fullBlocks, firstCount, firstWritten, &stream, &positions, &meta)
	return entries, tapeNumberScalar, ok
}

//go:noinline
func buildIndexPositionsCommit(
	src []byte,
	storage []IndexEntry,
	fullBlocks, firstCount, firstWritten int,
	stream *simdkernels.Stage1IndexStream,
	positions *[simdkernels.Stage1ChunkBlocks*64 + 64]uint32,
	meta *simdkernels.Stage1IndexMeta,
) (entries []IndexEntry, ok bool) {
	n := len(src)
	full := storage[:cap(storage)]
	var rawEntries [][4]uint32
	if cap(storage) != 0 {
		rawEntries = unsafe.Slice((*[4]uint32)(unsafe.Pointer(unsafe.SliceData(full))), cap(storage))
	}

	var grammar simdkernels.Stage2IndexState
	simdkernels.Stage2IndexReset(&grammar)
	var slab [simdkernels.Stage2IndexSlabLen]uint64
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	escapeState := indexBitmapEscapeState{entry: -1}
	utf8RunStart, utf8RunEnd := -1, 0
	skipEscape := -1

	consume := func(block, count, written int) bool {
		var escapeBlocks uint32
		for i := 0; i < count; i++ {
			current := block + i
			recs[i].EscInStr = meta.EscInStr[i]
			recs[i].InStr = meta.InStr[i]
			if meta.NonASCII&(1<<i) != 0 {
				if utf8RunStart >= 0 && current-utf8RunEnd > validUTF8CoalesceBlocks {
					if !validUTF8Fast(src[utf8RunStart*64 : utf8RunEnd*64]) {
						return false
					}
					utf8RunStart = current
				} else if utf8RunStart < 0 {
					utf8RunStart = current
				}
				utf8RunEnd = current + 1
			}
			if esc := meta.EscInStr[i]; esc != 0 {
				escapeBlocks |= 1 << i
				if validBitmapEscapes(src, n, current*64, esc, &skipEscape) {
					return false
				}
			}
		}

		simdkernels.Stage2IndexPositionsFused(
			src, positions[:written], &slab, rawEntries, &grammar,
		)
		if grammar.Bad != 0 {
			return false
		}
		if escapeBlocks != 0 || escapeState.seen {
			if !indexBitmapFinish(full, grammar.EntryOff,
				&recs, block*64, count, escapeBlocks, &escapeState) {
				return false
			}
		}
		return true
	}

	if firstCount > 0 {
		if meta.EmitCount >= 64 && meta.InStrCount > 6*meta.EmitCount {
			grammar.ObjectStringFast = 1
		}
		if !consume(0, firstCount, firstWritten) {
			return nil, false
		}
	}

	for block := firstCount; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		meta.Sample = false
		written := simdkernels.Stage1IndexBlocksMeta(
			src[block*64:], count, uint32(block*64), stream, positions[:], meta,
		)
		if stream.Bad || !consume(block, count, written) {
			return nil, false
		}
	}

	if fullBlocks*64 != n {
		tail := spaceBlock64
		copy(tail[:], src[fullBlocks*64:])
		meta.Sample = false
		written := simdkernels.Stage1IndexBlocksMeta(
			tail[:], 1, uint32(fullBlocks*64), stream, positions[:], meta,
		)
		if stream.Bad || !consume(fullBlocks, 1, written) {
			return nil, false
		}
	}

	if stream.Carry.InString != 0 || !simdkernels.Stage2IndexFinish(&grammar) {
		return nil, false
	}
	if utf8RunStart >= 0 && !validUTF8Fast(src[utf8RunStart*64:min(utf8RunEnd*64, n)]) {
		return nil, false
	}
	return full[:grammar.EntryOff/16], true
}

func indexPositionsFallbackNumberMode(src []byte, positions []uint32, meta *simdkernels.Stage1IndexMeta) uint8 {
	// Only inspect candidate starts when the sample is both dense and string
	// rich. This excludes short-decimal numeric arrays before the probe loop.
	if meta.EmitCount < 512 || meta.InStrCount < 512 {
		return tapeNumberScalar
	}
	numbers, long := 0, 0
	for _, pos := range positions {
		i := int(pos)
		c := src[i]
		if c == '-' {
			i++
		} else if !IsDigit(c) {
			continue
		}
		numbers++
		if i+8 <= len(src) && nonDigitMask8(binary.LittleEndian.Uint64(src[i:i+8])) == 0 {
			long++
		}
	}
	if long >= 8 && long*2 >= numbers {
		return tapeNumberSWAR
	}
	return tapeNumberScalar
}
