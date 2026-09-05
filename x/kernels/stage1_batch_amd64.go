//go:build !go1.28 && goexperiment.simd && amd64

package kernels

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// The fused producer retains classification constants across each chunk.
//
//go:noinline
func stage1IndexBlocksAVX2(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32,
	mode int, validMeta *Stage1ValidMeta, indexMeta *Stage1IndexMeta) int {
	if nblocks <= 0 || nblocks > Stage1ChunkBlocks {
		panic("vibejson: stage1 packed block count outside [1, Stage1ChunkBlocks]")
	}
	if len(out) < nblocks*64+64 {
		panic("vibejson: stage1 packed output lacks overwrite slack")
	}
	if mode == stage1PortableIndexValid && validMeta == nil {
		panic("vibejson: Stage1ValidBlocks requires metadata storage")
	}
	if validMeta != nil {
		validMeta.NonASCII = 0
	}
	if indexMeta != nil {
		sample := indexMeta.Sample
		*indexMeta = Stage1IndexMeta{Sample: sample}
	}

	src := unsafe.Pointer(p)
	carry := st.Carry
	follows := st.Follows
	previousIn := st.PreviousIn
	bad := st.Bad
	nonASCII := st.NonASCII
	hasEscapes := st.Escaped
	written := 0
	outBase := unsafe.Pointer(unsafe.SliceData(out))

	loTable := archsimd.LoadUint8x32Array(&stage1ClassLoAVX2)
	hiTable := archsimd.LoadUint8x32Array(&stage1ClassHiAVX2)
	// Colon remains a separator, but no longer needs a position or comparison.
	if mode == stage1PortableIndexCursor {
		loTable = archsimd.LoadUint8x32Array(&stage1CursorLoAVX2)
		hiTable = archsimd.LoadUint8x32Array(&stage1CursorHiAVX2)
	}

	lowNibble := archsimd.BroadcastUint8x32(15)
	wsBits := archsimd.BroadcastUint8x32(stage1WhitespaceBits)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(32)
	zero := archsimd.BroadcastUint8x32(0)

	for block := 0; block < nblocks; block++ {
		bytes := (*[64]byte)(unsafe.Add(src, block*64))
		var masks Stage1Masks

		v0 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Pointer(bytes)))
		v1 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Add(unsafe.Pointer(bytes), 32)))
		hi0 := v0.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		hi1 := v1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		c0 := loTable.PermuteOrZeroGrouped(v0.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZeroGrouped(hi0.BitsToInt8()))
		c1 := loTable.PermuteOrZeroGrouped(v1.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZeroGrouped(hi1.BitsToInt8()))
		masks.Whitespace = uint64(c0.And(wsBits).NotEqual(zero).ToBits()) | uint64(c1.And(wsBits).NotEqual(zero).ToBits())<<32
		masks.Structural = uint64(c0.BitsToInt8().Greater(wsBits.BitsToInt8()).ToBits()) | uint64(c1.BitsToInt8().Greater(wsBits.BitsToInt8()).ToBits())<<32
		masks.Quote = uint64(v0.Equal(quote).ToBits()) | uint64(v1.Equal(quote).ToBits())<<32
		masks.Backslash = uint64(v0.Equal(slash).ToBits()) | uint64(v1.Equal(slash).ToBits())<<32
		masks.Control = uint64(v0.Less(ctrl).ToBits()) | uint64(v1.Less(ctrl).ToBits())<<32
		masks.NonASCII = v0.Or(v1).BitsToInt8().Less(archsimd.BroadcastInt8x32(0)).ToBits() != 0

		escaped := Stage1Escaped(masks.Backslash, &carry)
		quotes := masks.Quote &^ escaped
		inString := Stage1PrefixXOR(quotes, &carry)
		outside := ^(inString | quotes)
		openers := quotes & inString
		cand := ^(masks.Whitespace | masks.Structural | masks.Quote | inString)
		starts := cand &^ (cand<<1 | follows)
		follows = cand >> 63
		emit := (masks.Structural|starts)&outside | openers
		closers := (inString<<1 | previousIn) &^ inString
		previousIn = inString >> 63

		mask := emit
		if mode != stage1PortableIndexValid {
			mask |= closers
		}

		escInString := escaped & inString
		if masks.Control&(inString|outside&^masks.Whitespace) != 0 {
			bad = true
		}
		if escInString != 0 {
			hasEscapes = true
		}
		if masks.NonASCII {
			nonASCII = true
		}

		if validMeta != nil {
			validMeta.EscInStr[block] = escInString
			if masks.NonASCII {
				validMeta.NonASCII |= 1 << block
			}
		}
		if indexMeta != nil {
			indexMeta.EscInStr[block] = escInString
			indexMeta.InStr[block] = inString
			if masks.NonASCII {
				indexMeta.NonASCII |= 1 << block
			}
			if indexMeta.Sample {
				indexMeta.WsCount += uint32(stage1SamplePopcount(masks.Whitespace))
				indexMeta.EmitCount += uint32(stage1SamplePopcount(emit))
				indexMeta.InStrCount += uint32(stage1SamplePopcount(inString))
				indexMeta.EscCount += uint32(stage1SamplePopcount(escInString))
			}
		}

		blockBase := base + uint32(block*64)
		for mask != 0 {
			// At most 64 positions per block; entry validation proves output capacity.
			*(*uint32)(unsafe.Add(outBase, uintptr(written)*4)) = blockBase + uint32(bits.TrailingZeros64(mask))
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
	archsimd.ClearAVXUpperBits()
	return written
}

//go:noinline
func stage1BlocksAVX2(p *byte, nblocks int, st *Stage1Stream, out *[Stage1ChunkBlocks]Stage1Rec) {
	if nblocks <= 0 || nblocks > Stage1ChunkBlocks {
		panic("vibejson: invalid block count")
	}

	loTable := archsimd.LoadUint8x32Array(&stage1ClassLoAVX2)
	hiTable := archsimd.LoadUint8x32Array(&stage1ClassHiAVX2)
	lowNibble := archsimd.BroadcastUint8x32(15)
	wsBits := archsimd.BroadcastUint8x32(stage1WhitespaceBits)
	quote := archsimd.BroadcastUint8x32('"')
	slash := archsimd.BroadcastUint8x32('\\')
	ctrl := archsimd.BroadcastUint8x32(32)
	zero := archsimd.BroadcastUint8x32(0)

	for i := 0; i < nblocks; i++ {
		bytes := (*[64]byte)(unsafe.Add(unsafe.Pointer(p), i*64))
		var masks Stage1Masks

		v0 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Pointer(bytes)))
		v1 := archsimd.LoadUint8x32Array((*[32]byte)(unsafe.Add(unsafe.Pointer(bytes), 32)))
		hi0 := v0.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		hi1 := v1.ReshapeToUint16s().ShiftAllRight(4).ReshapeToUint8s().And(lowNibble)
		c0 := loTable.PermuteOrZeroGrouped(v0.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZeroGrouped(hi0.BitsToInt8()))
		c1 := loTable.PermuteOrZeroGrouped(v1.And(lowNibble).BitsToInt8()).And(hiTable.PermuteOrZeroGrouped(hi1.BitsToInt8()))
		masks.Whitespace = uint64(c0.And(wsBits).NotEqual(zero).ToBits()) | uint64(c1.And(wsBits).NotEqual(zero).ToBits())<<32
		masks.Structural = uint64(c0.BitsToInt8().Greater(wsBits.BitsToInt8()).ToBits()) | uint64(c1.BitsToInt8().Greater(wsBits.BitsToInt8()).ToBits())<<32
		masks.Quote = uint64(v0.Equal(quote).ToBits()) | uint64(v1.Equal(quote).ToBits())<<32
		masks.Backslash = uint64(v0.Equal(slash).ToBits()) | uint64(v1.Equal(slash).ToBits())<<32
		masks.Control = uint64(v0.Less(ctrl).ToBits()) | uint64(v1.Less(ctrl).ToBits())<<32
		masks.NonASCII = v0.Or(v1).BitsToInt8().Less(archsimd.BroadcastInt8x32(0)).ToBits() != 0

		r := &out[i]
		escaped := Stage1Escaped(masks.Backslash, &st.Carry)
		quotes := masks.Quote &^ escaped
		inString := Stage1PrefixXOR(quotes, &st.Carry)

		// inString includes opening quotes and excludes closing quotes. Combining
		// it with every unescaped quote therefore excludes both quote boundaries
		// and the entire string body in one mask.
		outside := ^(inString | quotes)
		openers := quotes & inString

		// Raw quotes are excluded rather than only unescaped quotes: an escaped
		// quote inside a string must not become a scalar candidate. Since cand also
		// excludes inString, it is already a strict subset of outside.
		cand := ^(masks.Whitespace | masks.Structural | masks.Quote | inString)
		starts := cand &^ (cand<<1 | st.Follows)
		st.Follows = cand >> 63

		r.Emit = (masks.Structural|starts)&outside | openers
		r.Scalar = cand
		r.EscInStr = escaped & inString
		r.Bad = masks.Control&(inString|outside&^masks.Whitespace) != 0
		r.WsOut = masks.Whitespace & outside
		r.InStr = inString
		r.NonASCII = masks.NonASCII
	}

	archsimd.ClearAVXUpperBits()
}
