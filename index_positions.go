package simdjson

import (
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

// buildIndexPositions writes a production index from a forward-only packed
// structural stream. Stage 1 emits exact quote boundaries; stage 2 consumes
// each position once and patches strings and containers at their closers.
func buildIndexPositions(src []byte, storage []IndexEntry) (entries []IndexEntry, ok bool) {
	n := len(src)
	base := sliceBase(src)
	full := storage[:cap(storage)]
	var entBase *byte
	if cap(storage) != 0 {
		entBase = (*byte)(unsafe.Pointer(unsafe.SliceData(full)))
	}

	var stream simdkernels.Stage1IndexStream
	var grammar simdkernels.Stage2IndexState
	simdkernels.Stage2IndexReset(&grammar)
	var slab [simdkernels.Stage2IndexSlabLen]uint64
	var positions [simdkernels.Stage1ChunkBlocks*64 + 64]uint32
	var meta simdkernels.Stage1IndexMeta
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	escapeState := indexBitmapEscapeState{entry: -1}
	utf8RunStart, utf8RunEnd := -1, 0
	skipEscape := -1
	fullBlocks := n / 64

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
			unsafe.SliceData(src), n, positions[:written], &slab, entBase, cap(storage), &grammar,
		)
		if grammar.Bad != 0 {
			return false
		}
		if !indexBitmapFinish(full, grammar.EntryOff,
			&recs, block*64, count, escapeBlocks, &escapeState) {
			return false
		}
		return true
	}

	for block := 0; block < fullBlocks; block += simdkernels.Stage1ChunkBlocks {
		count := min(simdkernels.Stage1ChunkBlocks, fullBlocks-block)
		meta.Sample = block == 0
		written := simdkernels.Stage1IndexBlocksMeta(
			(*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, positions[:], &meta,
		)
		if stream.Bad {
			return nil, false
		}
		if block == 0 && n >= validBitmapSampleBlocks*64 && !validBitmapSampleCommit(
			int(meta.WsCount), int(meta.EmitCount), int(meta.InStrCount), int(meta.EscCount),
		) {
			return nil, false
		}
		if block == 0 {
			if meta.EmitCount >= 64 && meta.InStrCount > 6*meta.EmitCount {
				grammar.ObjectStringFast = 1
			}
		}
		if !consume(block, count, written) {
			return nil, false
		}
	}

	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		meta.Sample = false
		written := simdkernels.Stage1IndexBlocksMeta(
			&tail[0], 1, uint32(fullBlocks*64), &stream, positions[:], &meta,
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

func indexFallbackNumberMode(src []byte) uint8 {
	if len(src) < validBitmapSampleBlocks*64 {
		return tapeNumberScalar
	}
	var stream simdkernels.Stage1IndexStream
	var meta simdkernels.Stage1IndexMeta
	meta.Sample = true
	var positions [simdkernels.Stage1ChunkBlocks*64 + 64]uint32
	written := simdkernels.Stage1IndexBlocksMeta(
		unsafe.SliceData(src), validBitmapSampleBlocks, 0, &stream, positions[:], &meta,
	)
	if stream.Bad {
		return tapeNumberScalar
	}
	return indexPositionsFallbackNumberMode(src, positions[:written], &meta)
}

func indexPositionsFallbackNumberMode(src []byte, positions []uint32, meta *simdkernels.Stage1IndexMeta) uint8 {
	// Only inspect candidate starts when the sample is both dense and string
	// rich. This excludes short-decimal numeric arrays before the probe loop.
	if meta.EmitCount < 512 || meta.InStrCount < 512 {
		return tapeNumberScalar
	}
	base := sliceBase(src)
	numbers, long := 0, 0
	for _, pos := range positions {
		i := int(pos)
		c := fastByteAt(base, i)
		if c == '-' {
			i++
		} else if !isDigit(c) {
			continue
		}
		numbers++
		if i+8 <= len(src) && nonDigitMask8(loadUint64LE(unsafe.Add(base, i))) == 0 {
			long++
		}
	}
	if long >= 8 && long*2 >= numbers {
		return tapeNumberSWAR
	}
	return tapeNumberScalar
}
