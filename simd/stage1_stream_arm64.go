//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package simd

import (
	"math/bits"
	"simd/archsimd"
	"unsafe"
)

// Stage1BlocksGP classifies nblocks consecutive 64-byte blocks at p and
// derives one Stage1Rec per block. Classification runs in NEON; each
// mask crosses to a general-purpose register through the ADDP movemask
// tree, and the escape chain, prefix-XOR, and derived-mask math run in
// scalar code. This is the C++ simdjson pipeline shape (json_scanner ->
// to_bitmask -> GP bit math), batched so vector constants load once per
// chunk instead of once per block.
//
// nblocks must be in [1, Stage1ChunkBlocks]; slicing the output up front
// hoists the nil and range checks so the loop body carries neither.
func Stage1BlocksGP(p *byte, nblocks int, st *Stage1Stream, out *[Stage1ChunkBlocks]Stage1Rec) {
	base := unsafe.Pointer(p)
	recs := out[:nblocks]

	zip := archsimd.LoadUint8x16Array(&stage1ZipIndex)
	weights := archsimd.LoadUint8x16Array(&stage1Weights)
	quoteB := archsimd.BroadcastUint8x16('"')
	slashB := archsimd.BroadcastUint8x16('\\')
	ctrlB := archsimd.BroadcastUint8x16(0x20)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	loTable := archsimd.LoadUint8x16Array(&stage1ClassLo)
	hiTable := archsimd.LoadUint8x16Array(&stage1ClassHi)
	wsMax := archsimd.BroadcastUint8x16(stage1WhitespaceBits)
	zero := archsimd.BroadcastUint8x16(0)
	// Variable-shift USHL with a broadcast -4 keeps the high-nibble shift
	// count in a register across the loop; the immediate-count form
	// rematerializes its splat constant every block.
	nibShift := archsimd.BroadcastInt8x16(-4)

	carryEsc := st.Carry.Escaped
	carryStr := st.Carry.InString
	follows := st.Follows

	const evenBits = uint64(0x5555555555555555)

	for i := range recs {
		bp := unsafe.Add(base, i*64)
		r0 := archsimd.LoadUint8x16Array((*[16]uint8)(bp))
		r1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 16)))
		r2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 32)))
		r3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(bp, 48)))

		v0 := r0.LookupOrZero(zip)
		v1 := r1.LookupOrZero(zip)
		v2 := r2.LookupOrZero(zip)
		v3 := r3.LookupOrZero(zip)

		// The interleave permutes bytes, so the maximum over the zipped
		// vectors equals the maximum over the raw block; taking it here
		// lets the raw vectors die at the lookups above instead of
		// staying live across all five reduction trees.
		hi := v0.Or(v1).Or(v2).Or(v3)

		c0 := loTable.LookupOrZero(v0.And(lowNibble)).And(hiTable.LookupOrZero(v0.Shift(nibShift)))
		c1 := loTable.LookupOrZero(v1.And(lowNibble)).And(hiTable.LookupOrZero(v1.Shift(nibShift)))
		c2 := loTable.LookupOrZero(v2.And(lowNibble)).And(hiTable.LookupOrZero(v2.Shift(nibShift)))
		c3 := loTable.LookupOrZero(v3.And(lowNibble)).And(hiTable.LookupOrZero(v3.Shift(nibShift)))

		// Class values are one-hot: whitespace classes are 1 and 2,
		// structural classes 8 through 64, everything else 0. One
		// unsigned compare per vector therefore tests each group — "any
		// class" is c > 0 and "structural" is c > stage1WhitespaceBits —
		// and whitespace falls out of the two masks with one scalar op,
		// saving a weighted AND per vector over testing the two bit
		// groups separately. The paired reduction shares its final ADDP
		// between the two masks.
		sig, structural := stage1MovemaskPair(
			stage1MovemaskSum(c0.Greater(zero), c1.Greater(zero), c2.Greater(zero), c3.Greater(zero), weights),
			stage1MovemaskSum(c0.Greater(wsMax), c1.Greater(wsMax), c2.Greater(wsMax), c3.Greater(wsMax), weights),
		)
		ws := sig &^ structural
		quoteRaw, backslash := stage1MovemaskPair(
			stage1MovemaskSum(v0.Equal(quoteB), v1.Equal(quoteB), v2.Equal(quoteB), v3.Equal(quoteB), weights),
			stage1MovemaskSum(v0.Equal(slashB), v1.Equal(slashB), v2.Equal(slashB), v3.Equal(slashB), weights),
		)
		control := stage1Movemask64(v0.Less(ctrlB), v1.Less(ctrlB), v2.Less(ctrlB), v3.Less(ctrlB), weights)

		// Escape chain in GP (the production Stage1Escaped, inlined so the
		// carry stays in a register across blocks).
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

		// Prefix-XOR in GP (the production Stage1PrefixXOR shape).
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

		// outside excludes interiors, opening quotes (inside inStr), and
		// closing quotes: inStr | (quotes &^ inStr) == inStr | quotes.
		outside := ^(inStr | quotes)
		openers := quotes & inStr
		cand := ^(sig | quoteRaw | inStr)
		starts := cand &^ (cand<<1 | follows)
		follows = cand >> 63

		rec := &recs[i]
		rec.Emit = (structural|starts)&outside | openers
		rec.Scalar = cand & outside
		rec.EscInStr = escaped & inStr
		rec.Bad = control&(inStr|outside&^ws) != 0
		rec.WsOut = ws & outside
		rec.InStr = inStr
		// Per-block non-ASCII: the validator brackets UTF-8 runs by block,
		// so each record carries its own flag rather than a document-wide
		// OR.
		rec.NonASCII = hi.ReduceMax() >= 0x80
	}

	st.Carry.Escaped = carryEsc
	st.Carry.InString = carryStr
	st.Follows = follows
}
