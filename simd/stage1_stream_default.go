//go:build !go1.27 || !goexperiment.simd || !arm64

package simd

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

// Stage1StreamEnabled reports whether this build provides batched and packed
// stage-1 producers. Non-arm64 builds use the architecture's Stage1Block
// classifier when present and the portable SWAR classifier otherwise.
//
// Deprecated: These producers are available on every supported build; this
// function always returns true.
func Stage1StreamEnabled() bool { return true }

// Stage1BlocksGP is the portable equivalent of the batched SIMD classifier.
// It preserves carry state across blocks and emits the same Stage1Rec records.
func Stage1BlocksGP(p *byte, nblocks int, st *Stage1Stream, out *[Stage1ChunkBlocks]Stage1Rec) {
	if nblocks <= 0 || nblocks > Stage1ChunkBlocks {
		panic("simd: Stage1BlocksGP block count outside [1, Stage1ChunkBlocks]")
	}
	base := unsafe.Pointer(p)
	recs := out[:nblocks]
	for i := range recs {
		var masks Stage1Masks
		Stage1Block((*[64]byte)(unsafe.Add(base, i*64)), &masks)
		Stage1RecFromMasks(&masks, st, &recs[i])
	}
}

const (
	stage1PortableIndexFull = iota
	stage1PortableIndexCursor
	stage1PortableIndexValid
)

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

// Stage1CursorBlocks emits the colon-elided forward cursor stream.
func Stage1CursorBlocks(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32) int {
	return stage1IndexBlocksPortable(p, nblocks, base, st, out, stage1PortableIndexCursor, nil, nil)
}

// Stage1ValidBlocks emits the minimal validation stream: punctuation, opening
// quotes, and scalar starts. Closing quotes are omitted.
func Stage1ValidBlocks(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, meta *Stage1ValidMeta) int {
	return stage1IndexBlocksPortable(p, nblocks, base, st, out, stage1PortableIndexValid, meta, nil)
}

// Stage1ValidBlocksCoarse emits the validation stream with a chunk-coarse
// non-ASCII mask: if any block contains a high byte, every block is marked.
func Stage1ValidBlocksCoarse(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32, meta *Stage1ValidMeta) int {
	written := stage1IndexBlocksPortable(p, nblocks, base, st, out, stage1PortableIndexValid, meta, nil)
	if meta.NonASCII != 0 {
		meta.NonASCII = ^uint32(0) >> uint(Stage1ChunkBlocks-nblocks)
	}
	return written
}

func stage1IndexBlocksPortable(p *byte, nblocks int, base uint32, st *Stage1IndexStream, out []uint32,
	mode int, validMeta *Stage1ValidMeta, indexMeta *Stage1IndexMeta) int {
	if nblocks <= 0 || nblocks > Stage1ChunkBlocks {
		panic("simd: stage1 packed block count outside [1, Stage1ChunkBlocks]")
	}
	if len(out) < nblocks*64+64 {
		panic("simd: stage1 packed output lacks overwrite slack")
	}
	if mode == stage1PortableIndexValid && validMeta == nil {
		panic("simd: Stage1ValidBlocks requires metadata storage")
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

	for block := 0; block < nblocks; block++ {
		bytes := (*[64]byte)(unsafe.Add(src, block*64))
		var masks Stage1Masks
		Stage1Block(bytes, &masks)

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
		if mode == stage1PortableIndexCursor {
			mask &^= stage1BlockByteMaskPortable(bytes, ':')
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
				indexMeta.WsCount += uint32(bits.OnesCount64(masks.Whitespace))
				indexMeta.EmitCount += uint32(bits.OnesCount64(emit))
				indexMeta.InStrCount += uint32(bits.OnesCount64(inString))
				indexMeta.EscCount += uint32(bits.OnesCount64(escInString))
			}
		}

		blockBase := base + uint32(block*64)
		for mask != 0 {
			out[written] = blockBase + uint32(bits.TrailingZeros64(mask))
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

func stage1BlockByteMaskPortable(block *[64]byte, value byte) uint64 {
	var mask uint64
	for word := 0; word < 8; word++ {
		x := binary.LittleEndian.Uint64(block[word*8:])
		mask |= stage1CompressHighBytes(stage1ByteEqExact(x, value)) << uint(word*8)
	}
	return mask
}
