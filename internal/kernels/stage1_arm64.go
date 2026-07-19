//go:build go1.27 && !go1.28 && goexperiment.simd && arm64

package kernels

import (
	"simd/archsimd"
	"unsafe"
)

// Provenance: CPP-STAGE1-001. See docs/provenance.md for the exact upstream
// reference and local changes.
//
// stage1ZipIndex interleaves a vector's halves: lane 2i takes byte i and
// lane 2i+1 takes byte 8+i. All classification below runs on interleaved
// lanes; with the paired weights the 16-bit pairwise-add tree in
// stage1MovemaskSum emits mask bits back in source order. One table lookup
// per vector buys each of the five reductions a four-instruction ADDP tree
// in place of the three-instruction concat-add idiom per level. (The cited
// upstream implementation skips the interleave by pairwise-adding bytes, but the
// Go SIMD API exposes pairwise addition only at 16-bit width, where lane
// pairs straddle two source bytes and need the interleave to land in
// order.)
var stage1ZipIndex = [16]uint8{0, 8, 1, 9, 2, 10, 3, 11, 4, 12, 5, 13, 6, 14, 7, 15}

// stage1Weights carries one distinct bit per interleaved lane pair: even
// lanes accumulate into the low byte of each 16-bit sum and odd lanes into
// the high byte, so the tree reassembles source order.
var stage1Weights = [16]uint8{1, 1, 2, 2, 4, 4, 8, 8, 16, 16, 32, 32, 64, 64, 128, 128}

// Stage1Block classifies one full 64-byte block starting at p.
func Stage1Block(p *[64]byte, m *Stage1Masks) {
	base := unsafe.Pointer(p)
	r0 := archsimd.LoadUint8x16Array((*[16]uint8)(base))
	r1 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 16)))
	r2 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 32)))
	r3 := archsimd.LoadUint8x16Array((*[16]uint8)(unsafe.Add(base, 48)))

	zip := archsimd.LoadUint8x16Array(&stage1ZipIndex)
	v0 := r0.LookupOrZero(zip)
	v1 := r1.LookupOrZero(zip)
	v2 := r2.LookupOrZero(zip)
	v3 := r3.LookupOrZero(zip)

	// The interleave permutes bytes, so the maximum over the zipped
	// vectors equals the maximum over the raw block; taking it here lets
	// the raw vectors die at the lookups above.
	hi := v0.Or(v1).Or(v2).Or(v3)

	weights := archsimd.LoadUint8x16Array(&stage1Weights)
	quote := archsimd.BroadcastUint8x16('"')
	slash := archsimd.BroadcastUint8x16('\\')
	ctrl := archsimd.BroadcastUint8x16(0x20)
	lowNibble := archsimd.BroadcastUint8x16(0x0f)
	loTable := archsimd.LoadUint8x16Array(&stage1ClassLo)
	hiTable := archsimd.LoadUint8x16Array(&stage1ClassHi)
	wsMax := archsimd.BroadcastUint8x16(stage1WhitespaceBits)
	zero := archsimd.BroadcastUint8x16(0)

	c0 := loTable.LookupOrZero(v0.And(lowNibble)).And(hiTable.LookupOrZero(v0.ShiftAllRight(4)))
	c1 := loTable.LookupOrZero(v1.And(lowNibble)).And(hiTable.LookupOrZero(v1.ShiftAllRight(4)))
	c2 := loTable.LookupOrZero(v2.And(lowNibble)).And(hiTable.LookupOrZero(v2.ShiftAllRight(4)))
	c3 := loTable.LookupOrZero(v3.And(lowNibble)).And(hiTable.LookupOrZero(v3.ShiftAllRight(4)))

	// Class values are one-hot: whitespace classes are 1 and 2, structural
	// classes 8 through 64, everything else 0. One unsigned compare per
	// vector therefore tests each group — "any class" is c > 0 and
	// "structural" is c > stage1WhitespaceBits — and whitespace falls out
	// of the two masks with one scalar op, saving a weighted AND per
	// vector over testing the two bit groups separately.
	sig, structural := stage1MovemaskPair(
		stage1MovemaskSum(c0.Greater(zero), c1.Greater(zero), c2.Greater(zero), c3.Greater(zero), weights),
		stage1MovemaskSum(c0.Greater(wsMax), c1.Greater(wsMax), c2.Greater(wsMax), c3.Greater(wsMax), weights),
	)
	m.Whitespace = sig &^ structural
	m.Structural = structural
	m.Quote, m.Backslash = stage1MovemaskPair(
		stage1MovemaskSum(v0.Equal(quote), v1.Equal(quote), v2.Equal(quote), v3.Equal(quote), weights),
		stage1MovemaskSum(v0.Equal(slash), v1.Equal(slash), v2.Equal(slash), v3.Equal(slash), weights),
	)
	m.Control = stage1Movemask64(v0.Less(ctrl), v1.Less(ctrl), v2.Less(ctrl), v3.Less(ctrl), weights)
	m.NonASCII = hi.ReduceMax() >= 0x80
}

// stage1MovemaskSum folds four 16-lane compare masks over interleaved
// lanes into eight 16-bit partial sums: weighting gives every lane a
// distinct bit within its half-and-parity group, and three pairwise adds
// (ADDP on 16-bit lanes, which never carry across the byte boundary
// because the weights are disjoint) accumulate each group of eight source
// bytes into one 16-bit lane. One more pairwise add completes a 64-bit
// source-order mask; stage1Movemask64 and stage1MovemaskPair supply it.
func stage1MovemaskSum(m0, m1, m2, m3 archsimd.Mask8x16, weights archsimd.Uint8x16) archsimd.Uint16x8 {
	b0 := m0.ToInt8x16().ToBits().And(weights).ReshapeToUint16s()
	b1 := m1.ToInt8x16().ToBits().And(weights).ReshapeToUint16s()
	b2 := m2.ToInt8x16().ToBits().And(weights).ReshapeToUint16s()
	b3 := m3.ToInt8x16().ToBits().And(weights).ReshapeToUint16s()
	s01 := b0.ConcatAddPairs(b1)
	s23 := b2.ConcatAddPairs(b3)
	return s01.ConcatAddPairs(s23)
}

// stage1Movemask64 completes a single mask's reduction. The final
// pairwise add concatenates its operands, so a lone mask rides in both
// halves and lane 0 holds the result.
func stage1Movemask64(m0, m1, m2, m3 archsimd.Mask8x16, weights archsimd.Uint8x16) uint64 {
	s := stage1MovemaskSum(m0, m1, m2, m3, weights)
	t := s.ConcatAddPairs(s)
	return t.ReshapeToUint64s().GetElem(0)
}

// stage1MovemaskPair completes two masks' reductions with one pairwise
// add: the concatenating ADDP folds sa into the low 64-bit lane and sb
// into the high one, so paired masks split the final tree level that a
// lone mask spends on duplicating itself.
func stage1MovemaskPair(sa, sb archsimd.Uint16x8) (uint64, uint64) {
	t := sa.ConcatAddPairs(sb).ReshapeToUint64s()
	return t.GetElem(0), t.GetElem(1)
}
