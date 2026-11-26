//go:build !go1.28 && goexperiment.simd && arm64

package kernels

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// stage1IndexBlocksMetaNoSample is the production steady-state full-index
// specialization. Keeping metadata mandatory and density sampling disabled
// removes generic mode branches, four popcounts, and dead document-wide vector
// state from every 64-byte block after the first chunk.
func stage1IndexBlocksMetaNoSample(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, indexMeta *Stage1IndexMeta) int {
	if uint(nblocks-1) >= uint(Stage1ChunkBlocks) {
		panic("vibejson: stage1 packed block count outside [1, Stage1ChunkBlocks]")
	}
	if len(out) < nblocks*64+64 {
		panic("vibejson: Stage1IndexBlocks output lacks overwrite slack")
	}
	indexMeta.NonASCII = 0
	indexMeta.WsCount = 0
	indexMeta.EmitCount = 0
	indexMeta.InStrCount = 0
	indexMeta.EscCount = 0
	indexMeta.Sample = false
	blocks := unsafe.Slice((*[64]byte)(unsafe.Pointer(p)), nblocks)
	out = out[:nblocks*64+64]

	zip := archsimd.LoadUint8x16Array(&stage1ZipIndex)
	weights := archsimd.LoadUint8x16Array(&stage1Weights)
	quoteB := archsimd.BroadcastUint8x16('"')
	slashB := archsimd.BroadcastUint8x16('\\')
	ctrlB := archsimd.BroadcastUint8x16(0x20)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	loTable := archsimd.LoadUint8x16Array(&stage1ClassLo)
	hiTable := archsimd.LoadUint8x16Array(&stage1ClassHi)
	zero := archsimd.BroadcastUint8x16(0)
	nibShift := archsimd.BroadcastInt8x16(-4)

	carryEsc := st.Carry.Escaped
	carryStr := st.Carry.InString
	follows := st.Follows
	previousIn := st.PreviousIn
	nonASCII := st.NonASCII
	var badBits, escapeBits uint64
	if st.Bad {
		badBits = 1
	}
	if st.Escaped {
		escapeBits = 1
	}
	written := 0
	pendingMask := uint64(0)
	pendingBase := base

	const (
		evenBits = uint64(0x5555555555555555)
		highBit  = uint64(0x8000000000000000)
	)

	for block := range blocks {
		b := &blocks[block]
		r0 := archsimd.LoadUint8x16Array((*[16]uint8)(b[0:16]))
		r1 := archsimd.LoadUint8x16Array((*[16]uint8)(b[16:32]))
		r2 := archsimd.LoadUint8x16Array((*[16]uint8)(b[32:48]))
		r3 := archsimd.LoadUint8x16Array((*[16]uint8)(b[48:64]))

		v0 := r0.LookupOrZero(zip)
		v1 := r1.LookupOrZero(zip)
		v2 := r2.LookupOrZero(zip)
		v3 := r3.LookupOrZero(zip)
		hi := v0.Or(v1).Or(v2).Or(v3)

		quoteRaw, backslash := stage1MovemaskPair(
			stage1MovemaskSum(v0.Equal(quoteB), v1.Equal(quoteB), v2.Equal(quoteB), v3.Equal(quoteB), weights),
			stage1MovemaskSum(v0.Equal(slashB), v1.Equal(slashB), v2.Equal(slashB), v3.Equal(slashB), weights),
		)
		control := stage1Movemask64(v0.Less(ctrlB), v1.Less(ctrlB), v2.Less(ctrlB), v3.Less(ctrlB), weights)

		c0 := loTable.LookupOrZero(v0.And(lowNibble)).And(hiTable.LookupOrZero(v0.Shift(nibShift)))
		c1 := loTable.LookupOrZero(v1.And(lowNibble)).And(hiTable.LookupOrZero(v1.Shift(nibShift)))
		c2 := loTable.LookupOrZero(v2.And(lowNibble)).And(hiTable.LookupOrZero(v2.Shift(nibShift)))
		c3 := loTable.LookupOrZero(v3.And(lowNibble)).And(hiTable.LookupOrZero(v3.Shift(nibShift)))

		// Keeping this loop-invariant vector live across the loop makes the
		// current arm64 allocator spill it. VMOVI rematerialization is one
		// register-only instruction and lets the final compare clobber it.
		wsMax := archsimd.BroadcastUint8x16(stage1WhitespaceBits)
		structuralSum := stage1MovemaskSum(
			c0.Greater(wsMax), c1.Greater(wsMax), c2.Greater(wsMax), c3.Greater(wsMax), weights,
		)
		sigSum := stage1MovemaskSum(
			c0.Greater(zero), c1.Greater(zero), c2.Greater(zero), c3.Greater(zero), weights,
		)
		sig, structural := stage1MovemaskPair(sigSum, structuralSum)
		ws := sig &^ structural

		var escaped uint64
		if backslash == 0 {
			escaped = carryEsc
			carryEsc = 0
		} else {
			backslash &^= carryEsc
			followsEscape := backslash<<1 | carryEsc
			oddSequenceStarts := backslash & ^evenBits & ^followsEscape
			sum, overflow := bits.Add64(oddSequenceStarts, backslash, 0)
			carryEsc = overflow
			escaped = (evenBits ^ sum<<1) & followsEscape
		}

		quotes := quoteRaw &^ escaped
		m := quotes
		m ^= m << 1
		m ^= m << 2
		m ^= m << 4
		m ^= m << 8
		m ^= m << 16
		m ^= m << 32
		inStr := m ^ carryStr
		carryStr = uint64(int64(inStr) >> 63)

		outside := ^(inStr | quotes)
		openers := quotes & inStr
		cand := ^(sig | quoteRaw | inStr)
		starts := cand &^ (cand<<1 | follows)
		follows = cand >> 63
		emit := (structural|starts)&outside | openers
		closers := (inStr<<1 | previousIn) &^ inStr
		currentMask := emit | closers
		previousIn = inStr >> 63
		badBits |= control & (inStr | outside&^ws)
		escInStr := escaped & inStr
		escapeBits |= escInStr
		indexMeta.EscInStr[block] = escInStr
		indexMeta.InStr[block] = inStr
		if hi.ReduceMax() >= 0x80 {
			indexMeta.NonASCII |= 1 << block
		}

		mask := pendingMask
		emitBase := pendingBase
		pendingMask = currentMask
		pendingBase = base
		base += 64
		if mask == 0 {
			continue
		}
		n := bits.OnesCount64(mask)
		o := out[written : written+64]
		_ = o[63]
		// AArch64 has a one-cycle CLZ but implements trailing-zero count as
		// RBIT+CLZ. Reverse once, then clear each leading bit with a shifted
		// high bit, matching simdjson's ARM bit_indexer
		// (Provenance: CPP-STAGE1-001). The masked shift is
		// intentional: speculative writes after the real count may toggle a
		// garbage bit, but land only in the caller-provided overwrite slack.
		rev := bits.Reverse64(mask)
		lz := bits.LeadingZeros64(rev)
		o[0] = emitBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[1] = emitBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[2] = emitBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[3] = emitBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		if n > 4 {
			lz = bits.LeadingZeros64(rev)
			o[4] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[5] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[6] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[7] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
		}
		if n > 8 {
			lz = bits.LeadingZeros64(rev)
			o[8] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[9] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[10] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[11] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
		}
		if n > 12 {
			lz = bits.LeadingZeros64(rev)
			o[12] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[13] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[14] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[15] = emitBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			for i := 16; rev != 0 && i < len(o); i++ {
				lz = bits.LeadingZeros64(rev)
				o[i] = emitBase + uint32(lz)
				rev ^= highBit >> (uint(lz) & 63)
			}
		}
		written += n
	}

	if pendingMask != 0 {
		n := bits.OnesCount64(pendingMask)
		rev := bits.Reverse64(pendingMask)
		o := out[written : written+64]
		_ = o[63]
		lz := bits.LeadingZeros64(rev)
		o[0] = pendingBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[1] = pendingBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[2] = pendingBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		lz = bits.LeadingZeros64(rev)
		o[3] = pendingBase + uint32(lz)
		rev ^= highBit >> (uint(lz) & 63)
		if n > 4 {
			lz = bits.LeadingZeros64(rev)
			o[4] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[5] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[6] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[7] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
		}
		if n > 8 {
			lz = bits.LeadingZeros64(rev)
			o[8] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[9] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[10] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[11] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
		}
		if n > 12 {
			lz = bits.LeadingZeros64(rev)
			o[12] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[13] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[14] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			lz = bits.LeadingZeros64(rev)
			o[15] = pendingBase + uint32(lz)
			rev ^= highBit >> (uint(lz) & 63)
			for i := 16; rev != 0 && i < len(o); i++ {
				lz = bits.LeadingZeros64(rev)
				o[i] = pendingBase + uint32(lz)
				rev ^= highBit >> (uint(lz) & 63)
			}
		}
		written += n
	}

	st.Carry.Escaped = carryEsc
	st.Carry.InString = carryStr
	st.Follows = follows
	st.PreviousIn = previousIn
	st.Bad = badBits != 0
	st.NonASCII = nonASCII || indexMeta.NonASCII != 0
	st.Escaped = escapeBits != 0
	return written
}
