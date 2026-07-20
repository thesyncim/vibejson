//go:build go1.27 && !go1.28 && goexperiment.simd && amd64.v3

package kernels

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// Stage1IndexBlocks classifies consecutive blocks and writes punctuation,
// scalar starts, and both quote boundaries as absolute source positions.
func Stage1IndexBlocks(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32) int {
	return stage1IndexBlocksAMD64(p, nblocks, base, st, out, nil)
}

// Stage1IndexBlocksMeta is Stage1IndexBlocks with per-block validation facts
// and optional first-chunk density totals.
func Stage1IndexBlocksMeta(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, meta *Stage1IndexMeta) int {
	return stage1IndexBlocksAMD64(p, nblocks, base, st, out, meta)
}

// stage1IndexBlocksAMD64 keeps the vector classifier and packed-position
// producer in one loop. Calling Stage1Block here would rebuild its vector
// constants for every 64-byte block, which costs more than the portable SWAR
// classifier in this composed route even though the isolated block kernel is
// faster.
func stage1IndexBlocksAMD64(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, meta *Stage1IndexMeta) int {
	if nblocks <= 0 || nblocks > Stage1ChunkBlocks {
		panic("simdjson: stage1 packed block count outside [1, Stage1ChunkBlocks]")
	}
	if len(out) < nblocks*64+64 {
		panic("simdjson: stage1 packed output lacks overwrite slack")
	}
	if meta != nil {
		sample := meta.Sample
		*meta = Stage1IndexMeta{Sample: sample}
	}

	src := unsafe.Pointer(p)
	dst := unsafe.Pointer(unsafe.SliceData(out))
	carry := st.Carry
	follows := st.Follows
	previousIn := st.PreviousIn
	bad := st.Bad
	nonASCII := st.NonASCII
	hasEscapes := st.Escaped
	written := 0

	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x20)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	loTable := archsimd.LoadUint8x16Array(&stage1ClassLo)
	hiTable := archsimd.LoadUint8x16Array(&stage1ClassHi)
	wsBits := archsimd.BroadcastUint8x16(stage1WhitespaceBits)
	structBits := archsimd.BroadcastUint8x16(stage1StructuralBits)
	zero := archsimd.BroadcastUint8x16(0)
	highBit := archsimd.BroadcastUint8x16(0x80)

	for block := 0; block < nblocks; block++ {
		bp := unsafe.Add(src, block*64)
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(bp))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 16)))
		v2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 32)))
		v3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 48)))

		hi0 := v0.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		hi1 := v1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		hi2 := v2.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		hi3 := v3.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)

		c0 := loTable.PermuteOrZero(v0.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi0.BitsToInt8()))
		c1 := loTable.PermuteOrZero(v1.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi1.BitsToInt8()))
		c2 := loTable.PermuteOrZero(v2.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi2.BitsToInt8()))
		c3 := loTable.PermuteOrZero(v3.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZero(hi3.BitsToInt8()))

		whitespace := stage1Movemask64(
			c0.And(wsBits).NotEqual(zero),
			c1.And(wsBits).NotEqual(zero),
			c2.And(wsBits).NotEqual(zero),
			c3.And(wsBits).NotEqual(zero),
		)
		structural := stage1Movemask64(
			c0.And(structBits).NotEqual(zero),
			c1.And(structBits).NotEqual(zero),
			c2.And(structBits).NotEqual(zero),
			c3.And(structBits).NotEqual(zero),
		)
		quoteRaw := stage1Movemask64(v0.Equal(quote), v1.Equal(quote), v2.Equal(quote), v3.Equal(quote))
		backslash := stage1Movemask64(v0.Equal(slash), v1.Equal(slash), v2.Equal(slash), v3.Equal(slash))
		control := stage1Movemask64(v0.Less(ctrl), v1.Less(ctrl), v2.Less(ctrl), v3.Less(ctrl))
		blockNonASCII := v0.Or(v1).Or(v2).Or(v3).And(highBit).NotEqual(zero).ToBits() != 0

		escaped := Stage1Escaped(backslash, &carry)
		quotes := quoteRaw &^ escaped
		inString := Stage1PrefixXOR(quotes, &carry)
		outside := ^(inString | quotes)
		openers := quotes & inString
		cand := ^(whitespace | structural | quoteRaw | inString)
		starts := cand &^ (cand<<1 | follows)
		follows = cand >> 63
		emit := (structural|starts)&outside | openers
		closers := (inString<<1 | previousIn) &^ inString
		previousIn = inString >> 63
		mask := emit | closers

		escInString := escaped & inString
		if control&(inString|outside&^whitespace) != 0 {
			bad = true
		}
		if escInString != 0 {
			hasEscapes = true
		}
		if blockNonASCII {
			nonASCII = true
		}
		if meta != nil {
			meta.EscInStr[block] = escInString
			meta.InStr[block] = inString
			if blockNonASCII {
				meta.NonASCII |= 1 << block
			}
			if meta.Sample {
				meta.WsCount += uint32(bits.OnesCount64(whitespace))
				meta.EmitCount += uint32(bits.OnesCount64(emit))
				meta.InStrCount += uint32(bits.OnesCount64(inString))
				meta.EscCount += uint32(bits.OnesCount64(escInString))
			}
		}

		blockBase := base + uint32(block*64)
		for mask != 0 {
			*(*uint32)(unsafe.Add(dst, uintptr(written)*4)) = blockBase + uint32(bits.TrailingZeros64(mask))
			written++
			mask &= mask - 1
		}
	}

	st.Carry = carry
	st.Follows = follows
	st.PreviousIn = previousIn
	st.Bad = bad
	st.NonASCII = nonASCII
	st.Escaped = hasEscapes
	return written
}
