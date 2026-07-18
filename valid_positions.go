package simdjson

import (
	"math/bits"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// The packed stage-2 machine and the recursive validator reject the same
// opening delimiter when it would exceed the configured nesting limit.
const _ = uint(simdkernels.Stage2MaxDepth-defaultMaxDepth) + uint(defaultMaxDepth-simdkernels.Stage2MaxDepth)

// validPositionsStreamed validates through a packed, forward-only structural
// stream. Stage1ValidBlocks writes only grammar events; Stage2PositionsGo
// consumes them immediately and compacts scalar starts in place, so storage is
// fixed per chunk and the path allocates nothing.
func validPositionsStreamed(src []byte) (valid, decided bool) {
	if commit, invalid, numberMode, coarseNonASCII := validPositionsSample(src); invalid {
		return false, true
	} else if !commit {
		return false, false
	} else {
		return validPositionsCommitted(src, numberMode, coarseNonASCII), true
	}
}

func validPositionsSample(src []byte) (commit, invalid bool, numberMode uint8, coarseNonASCII bool) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	var stream simdkernels.Stage1Stream
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	simdkernels.Stage1BlocksGP((*byte)(base), validBitmapSampleBlocks, &stream, &recs)

	ws, emit, inStr, esc := 0, 0, 0, 0
	hasNonASCII := false
	for i := 0; i < validBitmapSampleBlocks; i++ {
		rec := &recs[i]
		if rec.Bad {
			return false, true, 0, false
		}
		ws += bits.OnesCount64(rec.WsOut)
		emit += bits.OnesCount64(rec.Emit)
		inStr += bits.OnesCount64(rec.InStr)
		esc += bits.OnesCount64(rec.EscInStr)
		hasNonASCII = hasNonASCII || rec.NonASCII
	}
	coarseNonASCII = true
	if hasNonASCII {
		const highBits = uint64(0x8080808080808080)
		highBytes := 0
		for i := 0; i < validBitmapSampleBlocks*64; i += 8 {
			highBytes += bits.OnesCount64(loadUint64LE(unsafe.Add(base, i)) & highBits)
			if highBytes > 64 {
				coarseNonASCII = false
				break
			}
		}
	}
	return validBitmapSampleCommit(ws, emit, inStr, esc), false, validBitmapNumberMode(inStr), coarseNonASCII
}

func validPositionsCommitted(src []byte, numberMode uint8, coarseNonASCII bool) bool {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	fullBlocks := n / 64
	var stream simdkernels.Stage1IndexStream
	var grammar simdkernels.Stage2State
	simdkernels.Stage2Reset(&grammar)
	var kinds [simdkernels.Stage2KindsLen]byte
	const consumeBlocks = 4 * simdkernels.Stage1ChunkBlocks
	var positions [consumeBlocks*64 + 64]uint32
	var meta simdkernels.Stage1ValidMeta
	utf8RunStart, utf8RunEnd := -1, 0
	skipEscape := -1

	consume := func(written int) bool {
		if written == 0 {
			return true
		}
		// The output aliases the input intentionally. The machine reads a
		// position before writing a scalar at an index no greater than the
		// consumed input index, so unread positions are never overwritten.
		nscalars := simdkernels.Stage2PositionsTrusted(unsafe.SliceData(src), positions[:written], &kinds, positions[:], &grammar)
		if grammar.Bad != 0 {
			return false
		}
		for _, scalar := range positions[:nscalars] {
			if !validScalarTokenAtMode(src, base, n, int(scalar), numberMode) {
				return false
			}
		}
		return true
	}

	for batch := 0; batch < fullBlocks; batch += consumeBlocks {
		written := 0
		batchEnd := min(batch+consumeBlocks, fullBlocks)
		for block := batch; block < batchEnd; block += simdkernels.Stage1ChunkBlocks {
			count := min(simdkernels.Stage1ChunkBlocks, batchEnd-block)
			chunkWritten := 0
			if coarseNonASCII {
				chunkWritten = simdkernels.Stage1ValidBlocksCoarse(
					(*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, positions[written:], &meta,
				)
				if meta.NonASCII != 0 {
					meta.NonASCII = sparseNonASCIIMask(unsafe.Add(base, block*64), count)
				}
			} else {
				chunkWritten = simdkernels.Stage1ValidBlocks(
					(*byte)(unsafe.Add(base, block*64)), count, uint32(block*64), &stream, positions[written:], &meta,
				)
			}
			written += chunkWritten
			for i := 0; i < count; i++ {
				current := block + i
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
				if esc := meta.EscInStr[i]; esc != 0 && validBitmapEscapes(src, n, current*64, esc, &skipEscape) {
					return false
				}
			}
		}
		if !consume(written) {
			return false
		}
	}
	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
		copy(tail[:], src[fullBlocks*64:])
		written := 0
		if coarseNonASCII {
			written = simdkernels.Stage1ValidBlocksCoarse(
				&tail[0], 1, uint32(fullBlocks*64), &stream, positions[:], &meta,
			)
		} else {
			written = simdkernels.Stage1ValidBlocks(
				&tail[0], 1, uint32(fullBlocks*64), &stream, positions[:], &meta,
			)
		}
		if meta.NonASCII&1 != 0 {
			if utf8RunStart >= 0 && fullBlocks-utf8RunEnd > validUTF8CoalesceBlocks {
				if !validUTF8Fast(src[utf8RunStart*64 : utf8RunEnd*64]) {
					return false
				}
				utf8RunStart = fullBlocks
			} else if utf8RunStart < 0 {
				utf8RunStart = fullBlocks
			}
			utf8RunEnd = fullBlocks + 1
		}
		if esc := meta.EscInStr[0]; esc != 0 && validBitmapEscapes(src, n, fullBlocks*64, esc, &skipEscape) {
			return false
		}
		if !consume(written) {
			return false
		}
	}

	if stream.Bad || stream.Carry.InString != 0 || !simdkernels.Stage2Finish(&grammar) {
		return false
	}
	if utf8RunStart >= 0 && !validUTF8Fast(src[utf8RunStart*64:min(utf8RunEnd*64, n)]) {
		return false
	}
	return true
}

func sparseNonASCIIMask(base unsafe.Pointer, nblocks int) uint32 {
	const highBits = uint64(0x8080808080808080)
	var mask uint32
	for block := 0; block < nblocks; block++ {
		p := unsafe.Add(base, block*64)
		hi := loadUint64LE(p) |
			loadUint64LE(unsafe.Add(p, 8)) |
			loadUint64LE(unsafe.Add(p, 16)) |
			loadUint64LE(unsafe.Add(p, 24)) |
			loadUint64LE(unsafe.Add(p, 32)) |
			loadUint64LE(unsafe.Add(p, 40)) |
			loadUint64LE(unsafe.Add(p, 48)) |
			loadUint64LE(unsafe.Add(p, 56))
		if hi&highBits != 0 {
			mask |= 1 << block
		}
	}
	return mask
}

func validScalarTokenAtMode(src []byte, base unsafe.Pointer, n, j int, numberMode uint8) bool {
	c := fastByteAt(base, j)
	if c != '-' && '0' <= c && c <= '9' {
		if numberMode == vbNumberShort && j+4 <= n {
			if invalid := nonDigitMask4(loadUint32LE(unsafe.Add(base, j))); invalid != 0 {
				width := bits.TrailingZeros32(invalid) / 8
				if width != 0 && (c != '0' || width == 1) {
					end := j + width
					if isJSONSpaceOrStructural(fastByteAt(base, end)) {
						return true
					}
				}
			}
		}
		if numberMode == vbNumberNine && c != '0' && j+9 <= n &&
			nonDigitMask8(loadUint64LE(unsafe.Add(base, j))) == 0 &&
			isDigit(fastByteAt(base, j+8)) &&
			(j+9 == n || isJSONSpaceOrStructural(fastByteAt(base, j+9))) {
			return true
		}
	}
	return validScalarTokenAt(src, base, n, j)
}

// validScalarTokenAt validates a strict number or literal starting at j and
// requires a legal token boundary at its end.
func validScalarTokenAt(src []byte, base unsafe.Pointer, n, j int) bool {
	var end int
	switch c := fastByteAt(base, j); {
	case c == '-' || '0' <= c && c <= '9':
		var ok bool
		end, ok = scanNumberFast(base, n, j)
		if !ok {
			return false
		}
	case c == 't':
		if !literalTrueAt(src, j) {
			return false
		}
		end = j + 4
	case c == 'f':
		if !literalFalseTailAt(src, j) {
			return false
		}
		end = j + 5
	case c == 'n':
		if !literalNullAt(src, j) {
			return false
		}
		end = j + 4
	default:
		return false
	}
	return end == n || isJSONSpaceOrStructural(fastByteAt(base, end))
}
