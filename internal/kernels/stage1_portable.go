package kernels

import (
	"math/bits"
)

// Provenance: CPP-STAGE1-001.
// The backslash-run carry and prefix-XOR string-mask kernels adapt the C++
// simdjson stage-1 pipeline at verified reference commit
// 1bcf71bd85059ab6574ea1159de9298dcc1212c5; Apache-2.0, see
// LICENSE-SIMDJSON. Exact upstream paths and local changes are recorded in
// docs/provenance.md. The scalar classification table and its packing are local.

// The stage-1 masks and carry kernels are architecture-neutral. SIMD builds
// provide a vector Stage1Block; scalar builds use the table-driven classifier
// below. Build tags bind callers directly to one implementation.

// Stage1Masks holds the per-64-byte-block classification. Bit i of each
// mask describes byte i of the block.
type Stage1Masks struct {
	Whitespace uint64 // space, tab, line feed, carriage return
	Structural uint64 // { } [ ] : ,
	Quote      uint64 // every raw quote; escaped quotes are removed by stage 1
	Backslash  uint64 // every backslash
	Control    uint64 // bytes below 0x20 (whitespace included)
	NonASCII   bool   // any byte at or above 0x80 in the block
}

// Stage1Carry threads block-boundary state between consecutive blocks.
// The zero value is the document-start state.
type Stage1Carry struct {
	Escaped  uint64 // bit 0: first byte of next block is escaped
	InString uint64 // all-ones when the next block starts inside a string
}

// Stage1BracketMasks holds the per-64-byte-block classification a
// non-validating structural skip consumes: string delimiters plus the two
// bracket classes, with opens and closes separated so the consumer can track
// nesting depth by popcount. Bit i of each mask describes byte i of the
// block.
type Stage1BracketMasks struct {
	Quote     uint64 // every raw quote; escape resolution is the consumer's
	Backslash uint64 // every backslash
	Open      uint64 // { and [
	Close     uint64 // } and ]
}

const (
	stage1EvenBits      = uint64(0x5555555555555555)
	stage1ByteLow7      = uint64(0x7f7f7f7f7f7f7f7f)
	stage1ByteHigh      = uint64(0x8080808080808080)
	stage1CompressBytes = uint64(0x0002040810204081)
)

// stage1PortableClass packs six one-bit byte classifications into separate
// bytes of one uint64. Shifting an entry by a lane index and ORing eight
// entries therefore builds six independent eight-lane masks at once. The
// 2 KiB table stays hot and halves the scalar classifier cost versus both the
// bytewise switch and repeated SWAR equality/compression.
var stage1PortableClass = func() (table [256]uint64) {
	for i := range table {
		c := byte(i)
		var class uint64
		if c == ' ' || c == '\t' || c == '\n' || c == '\r' {
			class |= 1
		}
		if c == '{' || c == '}' || c == '[' || c == ']' || c == ':' || c == ',' {
			class |= 1 << 8
		}
		if c == '"' {
			class |= 1 << 16
		}
		if c == '\\' {
			class |= 1 << 24
		}
		if c < 0x20 {
			class |= 1 << 32
		}
		if c >= 0x80 {
			class |= 1 << 40
		}
		if c == '{' || c == '[' {
			class |= 1 << 48
		}
		if c == '}' || c == ']' {
			class |= 1 << 56
		}
		table[i] = class
	}
	return table
}()

func stage1Pack8(block *[64]byte, offset int) uint64 {
	return stage1PortableClass[block[offset+0]] |
		stage1PortableClass[block[offset+1]]<<1 |
		stage1PortableClass[block[offset+2]]<<2 |
		stage1PortableClass[block[offset+3]]<<3 |
		stage1PortableClass[block[offset+4]]<<4 |
		stage1PortableClass[block[offset+5]]<<5 |
		stage1PortableClass[block[offset+6]]<<6 |
		stage1PortableClass[block[offset+7]]<<7
}

// stage1BlockPortable classifies one 64-byte block. The eight groups are
// intentionally unrolled: this removes loop/index bookkeeping from the scalar
// fallback and lets the compiler keep the packed class words in registers.
func stage1BlockPortable(block *[64]byte, out *Stage1Masks) {
	p0 := stage1Pack8(block, 0)
	p1 := stage1Pack8(block, 8)
	p2 := stage1Pack8(block, 16)
	p3 := stage1Pack8(block, 24)
	p4 := stage1Pack8(block, 32)
	p5 := stage1Pack8(block, 40)
	p6 := stage1Pack8(block, 48)
	p7 := stage1Pack8(block, 56)

	*out = Stage1Masks{
		Whitespace: stage1Plane(p0, 0, 0) | stage1Plane(p1, 0, 8) | stage1Plane(p2, 0, 16) | stage1Plane(p3, 0, 24) |
			stage1Plane(p4, 0, 32) | stage1Plane(p5, 0, 40) | stage1Plane(p6, 0, 48) | stage1Plane(p7, 0, 56),
		Structural: stage1Plane(p0, 8, 0) | stage1Plane(p1, 8, 8) | stage1Plane(p2, 8, 16) | stage1Plane(p3, 8, 24) |
			stage1Plane(p4, 8, 32) | stage1Plane(p5, 8, 40) | stage1Plane(p6, 8, 48) | stage1Plane(p7, 8, 56),
		Quote: stage1Plane(p0, 16, 0) | stage1Plane(p1, 16, 8) | stage1Plane(p2, 16, 16) | stage1Plane(p3, 16, 24) |
			stage1Plane(p4, 16, 32) | stage1Plane(p5, 16, 40) | stage1Plane(p6, 16, 48) | stage1Plane(p7, 16, 56),
		Backslash: (p0 >> 24 & 0xff) | (p1 >> 24 & 0xff << 8) | (p2 >> 24 & 0xff << 16) | (p3 >> 24 & 0xff << 24) |
			(p4 >> 24 & 0xff << 32) | (p5 >> 24 & 0xff << 40) | (p6 >> 24 & 0xff << 48) | (p7 >> 24 & 0xff << 56),
		Control: (p0 >> 32 & 0xff) | (p1 >> 32 & 0xff << 8) | (p2 >> 32 & 0xff << 16) | (p3 >> 32 & 0xff << 24) |
			(p4 >> 32 & 0xff << 32) | (p5 >> 32 & 0xff << 40) | (p6 >> 32 & 0xff << 48) | (p7 >> 32 & 0xff << 56),
		NonASCII: (p0|p1|p2|p3|p4|p5|p6|p7)&(0xff<<40) != 0,
	}
}

func stage1Plane(packed uint64, plane, shift uint) uint64 {
	return (packed >> plane & 0xff) << shift
}

// stage1BlockBracketsPortable classifies one 64-byte block into the skip
// masks with the same packed table as stage1BlockPortable; the open and
// close planes ride in the two bytes the original classification leaves
// free.
func stage1BlockBracketsPortable(block *[64]byte, out *Stage1BracketMasks) {
	p0 := stage1Pack8(block, 0)
	p1 := stage1Pack8(block, 8)
	p2 := stage1Pack8(block, 16)
	p3 := stage1Pack8(block, 24)
	p4 := stage1Pack8(block, 32)
	p5 := stage1Pack8(block, 40)
	p6 := stage1Pack8(block, 48)
	p7 := stage1Pack8(block, 56)

	*out = Stage1BracketMasks{
		Quote: stage1Plane(p0, 16, 0) | stage1Plane(p1, 16, 8) | stage1Plane(p2, 16, 16) | stage1Plane(p3, 16, 24) |
			stage1Plane(p4, 16, 32) | stage1Plane(p5, 16, 40) | stage1Plane(p6, 16, 48) | stage1Plane(p7, 16, 56),
		Backslash: stage1Plane(p0, 24, 0) | stage1Plane(p1, 24, 8) | stage1Plane(p2, 24, 16) | stage1Plane(p3, 24, 24) |
			stage1Plane(p4, 24, 32) | stage1Plane(p5, 24, 40) | stage1Plane(p6, 24, 48) | stage1Plane(p7, 24, 56),
		Open: stage1Plane(p0, 48, 0) | stage1Plane(p1, 48, 8) | stage1Plane(p2, 48, 16) | stage1Plane(p3, 48, 24) |
			stage1Plane(p4, 48, 32) | stage1Plane(p5, 48, 40) | stage1Plane(p6, 48, 48) | stage1Plane(p7, 48, 56),
		Close: stage1Plane(p0, 56, 0) | stage1Plane(p1, 56, 8) | stage1Plane(p2, 56, 16) | stage1Plane(p3, 56, 24) |
			stage1Plane(p4, 56, 32) | stage1Plane(p5, 56, 40) | stage1Plane(p6, 56, 48) | stage1Plane(p7, 56, 56),
	}
}

func stage1ByteEqExact(x uint64, value byte) uint64 {
	return stage1ZeroByteMaskExact(x ^ uint64(value)*0x0101010101010101)
}

func stage1ZeroByteMaskExact(x uint64) uint64 {
	return ^(((x & stage1ByteLow7) + stage1ByteLow7) | x | stage1ByteLow7) & stage1ByteHigh
}

func stage1CompressHighBytes(x uint64) uint64 {
	return x * stage1CompressBytes >> 56
}

// Stage1Escaped resolves the bytes escaped by backslash runs and updates the
// block-boundary carry. The common paths avoid the full odd-run arithmetic:
// blocks without backslashes only forward a pending carry, while isolated
// backslashes directly produce their shifted target mask.
func Stage1Escaped(backslash uint64, carry *Stage1Carry) uint64 {
	carryEscaped := carry.Escaped
	if backslash == 0 {
		carry.Escaped = 0
		return carryEscaped
	}

	// A carry consumes a backslash in lane zero. Remove it before looking for
	// adjacent active backslashes; otherwise a boundary escape can make an
	// isolated run appear dense and can incorrectly start another escape.
	backslash &^= carryEscaped
	followsEscape := backslash<<1 | carryEscaped
	if backslash&followsEscape == 0 {
		carry.Escaped = backslash >> 63
		return followsEscape
	}

	// General odd-length backslash-run resolution from simdjson
	// (Provenance: CPP-STAGE1-001). Adding each
	// odd-positioned run start to the run mask propagates through that run;
	// the shifted sum then selects exactly the escaped target bytes.
	oddSequenceStarts := backslash & ^(stage1EvenBits | followsEscape)
	sequencesStartingOnEven, overflow := bits.Add64(oddSequenceStarts, backslash, 0)
	carry.Escaped = overflow
	return (stage1EvenBits ^ sequencesStartingOnEven<<1) & followsEscape
}

// Stage1PrefixXOR computes for each bit the parity of all bits at or below it;
// with the unescaped-quote mask as input the result marks string interiors
// from each opening quote through the byte before its closing quote. The carry
// flips the whole block when it starts inside a string.
func Stage1PrefixXOR(quotes uint64, carry *Stage1Carry) uint64 {
	// Quote-free blocks are common in long strings, whitespace, and numeric
	// arrays. Their output is exactly the incoming string state and the carry
	// cannot change, avoiding the six-instruction dependency chain below.
	if quotes == 0 {
		return carry.InString
	}

	m := quotes
	m ^= m << 1
	m ^= m << 2
	m ^= m << 4
	m ^= m << 8
	m ^= m << 16
	m ^= m << 32
	m ^= carry.InString
	carry.InString = uint64(int64(m) >> 63)
	return m
}
