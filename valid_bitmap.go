package simdjson

import (
	"math/bits"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// The bitmap validator is a validation-only consumer of the stage-1 masks:
// whitespace disappears into the block masks, string interiors are skipped
// wholesale by the in-string mask, and the grammar machine touches only
// structural characters, string openers, and the first byte of each scalar.
// On indentation-heavy documents most bytes never reach the walk at all.
//
// Dense compact documents emit a position every few bytes, so a density sample
// of the leading blocks decides which engine runs (see
// validBitmapSampleCommit). The engine only reports validity; Validate re-runs
// the scalar validator for exact error offsets.
//
// Production uses packed structural positions in every supported build:
// Stage1ValidBlocks selects an architecture kernel when available and the
// portable SWAR classifier otherwise, then Stage2PositionsTrusted consumes
// each position once. The record-based per-block and streamed engines below
// remain differential references for the packed route.
// docs/architecture.md records the routing rationale.

const (
	// validBitmapMinBytes keeps small and mid-size inputs on the recursive
	// scanner: below this even the sampling blocks would show up in their
	// latency.
	validBitmapMinBytes = 1 << 16

	// The density dispatch samples the first 2 KiB; the commit rule itself
	// lives in validBitmapSampleCommit. The whitespace leg requires at least
	// 25% outside whitespace and a ws:emit ratio above 3.5 (2ws >= 7emits).
	validBitmapSampleBlocks = 32
	validBitmapSampleMinWs  = 512

	// validBitmapSampleMinInStr is the string leg's floor: 9/16 of the
	// sampled bytes inside strings.
	validBitmapSampleMinInStr = validBitmapSampleBlocks * 64 * 9 / 16

	// validBitmapStreamChunk is the number of blocks classified per batched
	// kernel call. It must divide validBitmapSampleBlocks so the sampling
	// bailout stays chunk-aligned, and cannot exceed
	// simdkernels.Stage1ChunkBlocks.
	validBitmapStreamChunk = 4

	// After the density sample commits, the Go superinstruction walker uses
	// a wider run so its state remains in locals across more blocks. Sampling
	// retains the four-block cadence above, preserving the routing boundary.
	validBitmapStreamChunkGo = 32

	// Adjacent non-ASCII runs absorb short ASCII gaps to amortize SIMD UTF-8
	// setup without rereading large sparse spans.
	validUTF8CoalesceBlocks = 2
)

const (
	vbNumberDefault = iota
	vbNumberNine
	vbNumberShort
)

// Grammar machine states.
const (
	vbValue      = iota // expecting any value
	vbValueOrEnd        // expecting a value or ']' (freshly opened array)
	vbKeyOrEnd          // expecting a key string or '}' (freshly opened object)
	vbKey               // expecting a key string (after a comma)
	vbColon             // expecting ':'
	vbNext              // after a value: ',' or the container's closer
	vbDone              // top-level value complete
)

// vbState is the grammar machine's running state, shared by both engines
// so the JSON grammar lives in exactly one place. containers[d] records
// whether the container at depth d is an object (bit set) or array.
type vbState struct {
	state      int
	depth      int
	numberMode uint8
	inObject   bool
	containers [defaultMaxDepth/64 + 1]uint64
}

// validBitmapSampleCommit decides engine commitment from the byte classes
// of the 2 KiB sample: whitespace outside strings, grammar emits, in-string
// bytes, and escape targets inside strings. Three signals select document
// shapes that can skip enough scalar work:
//
//   - Spacey-structural: skipped whitespace must dominate emitted positions.
//   - String-heavy (the in-string leg): string interiors die inside the
//     masks, but skipped whitespace and string bytes together must still
//     outnumber emitted positions six to one.
//   - Escape-dense (the guard on the in-string leg): every escape target
//     costs a scalar check, so the string-heavy leg rejects more than one
//     escape target per sixteen string bytes.
func validBitmapSampleCommit(ws, emit, inStr, esc int) bool {
	if ws >= validBitmapSampleMinWs && ws*2 >= emit*7 {
		return true
	}
	return inStr >= validBitmapSampleMinInStr && esc*16 <= inStr && ws+inStr >= emit*6
}

func validBitmapNumberMode(inStr int) uint8 {
	if inStr >= validBitmapSampleMinInStr {
		return vbNumberShort
	}
	return vbNumberNine
}

// validBitmap reports strict validity through the packed stage-1 position
// stream and Go stage-2 grammar machine. Both backends are available in every
// supported build; decided=false means the density sample chose the recursive
// scanner instead and the result is then meaningless.
func validBitmap(src []byte) (valid, decided bool) {
	if len(src) < validBitmapSampleBlocks*64 {
		return validBitmapStreamed(src)
	}
	return validPositionsStreamed(src)
}

// validBitmapPerBlock classifies one block at a time. It is the reference
// shape the streamed engine reproduces mask-for-mask and verdict-for-verdict.
func validBitmapPerBlock(src []byte) (valid, decided bool) {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))

	var carry simdkernels.Stage1Carry
	var m simdkernels.Stage1Masks
	var g vbState
	g.state = vbValue

	follows := uint64(0) // bit 0: last byte of the previous block was a scalar byte
	// UTF-8 runs: [utf8RunStart, utf8RunEnd) brackets the current run of
	// blocks holding non-ASCII bytes, validated per run below.
	utf8RunStart, utf8RunEnd := -1, 0
	wsSample, emitSample, inStrSample, escSample := 0, 0, 0, 0
	skipEscape := -1 // low-surrogate escape already consumed by its high half

	nBlocks := (n + 63) / 64
	for block := 0; block < nBlocks; block++ {
		pos := block * 64
		if pos+64 <= n {
			simdkernels.Stage1Block((*[64]byte)(unsafe.Add(base, pos)), &m)
		} else {
			// Space padding is whitespace: it emits nothing and cannot
			// invalidate the tail block.
			var tail [64]byte
			for i := range tail {
				tail[i] = ' '
			}
			copy(tail[:], src[pos:])
			simdkernels.Stage1Block(&tail, &m)
		}

		escaped := simdkernels.Stage1Escaped(m.Backslash, &carry)
		quotes := m.Quote &^ escaped
		inStr := simdkernels.Stage1PrefixXOR(quotes, &carry)
		closers := quotes &^ inStr
		openers := quotes & inStr
		outside := ^(inStr | closers)

		// Raw control bytes are illegal inside strings, and outside them
		// only the three control whitespace bytes may appear.
		if m.Control&inStr != 0 {
			return false, true
		}
		if m.Control&outside&^m.Whitespace != 0 {
			return false, true
		}
		// UTF-8 is checked per run of non-ASCII blocks while the bytes are
		// still cache-warm, instead of a second full pass over the document.
		// A multi-byte sequence cannot cross a pure-ASCII block (every lead
		// and continuation byte has the high bit set), so a maximal run
		// validates as an independent slice. Nearby runs coalesce through the
		// configured ASCII-gap threshold above; validating that gap is harmless
		// and amortizes per-run kernel setup on alternating layouts.
		if m.NonASCII {
			if utf8RunStart >= 0 && block-utf8RunEnd > validUTF8CoalesceBlocks {
				if !validUTF8Fast(src[utf8RunStart*64 : utf8RunEnd*64]) {
					return false, true
				}
				utf8RunStart = block
			} else if utf8RunStart < 0 {
				utf8RunStart = block
			}
			utf8RunEnd = block + 1
		}

		// Escape targets inside strings must name a legal escape. Keep the
		// sizeable validator off the overwhelmingly common empty-mask path.
		escInStr := escaped & inStr
		if escInStr != 0 {
			if validBitmapEscapes(src, n, pos, escInStr, &skipEscape) {
				return false, true
			}
		}

		// Scalar starts: the first byte of each run outside strings that is
		// neither whitespace, structural, nor a quote.
		cand := ^(m.Whitespace | m.Structural | m.Quote | inStr)
		starts := cand &^ (cand<<1 | follows)
		follows = cand >> 63

		emit := [1]uint64{m.Structural&outside | openers | starts&outside}
		scalar := [1]uint64{cand & outside}
		if block < validBitmapSampleBlocks {
			wsSample += bits.OnesCount64(m.Whitespace & outside)
			emitSample += bits.OnesCount64(emit[0])
			inStrSample += bits.OnesCount64(inStr)
			escSample += bits.OnesCount64(escInStr)
			if block == validBitmapSampleBlocks-1 &&
				validBitmapSampleCommit(wsSample, emitSample, inStrSample, escSample) {
				g.numberMode = validBitmapNumberMode(inStrSample)
			} else if block == validBitmapSampleBlocks-1 {
				return false, false
			}
		}
		if v, done := validBitmapWalkTrusted(src, base, n, pos, emit[:], scalar[:], &g); done {
			return v, true
		}
	}

	if carry.InString != 0 || g.state != vbDone || g.depth != 0 {
		return false, true
	}
	if utf8RunStart >= 0 && !validUTF8Fast(src[utf8RunStart*64:min(utf8RunEnd*64, n)]) {
		return false, true
	}
	return true, true
}

// validBitmapStreamed is validBitmapPerBlock on the batched stage-1
// kernel: a chunk of blocks is classified per kernel call and the grammar
// machine consumes precomputed per-block records. The per-block logic and
// verdicts match validBitmapPerBlock exactly, including the per-run UTF-8
// bracketing driven by each record's NonASCII flag.
func validBitmapStreamed(src []byte) (valid, decided bool) {
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))

	var st simdkernels.Stage1Stream
	var recs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
	var g vbState
	g.state = vbValue

	utf8RunStart, utf8RunEnd := -1, 0
	wsSample, emitSample, inStrSample, escSample := 0, 0, 0, 0
	skipEscape := -1

	fullBlocks := n / 64
	nBlocks := (n + 63) / 64
	for chunk := 0; chunk < nBlocks; {
		step := validBitmapStreamChunkGo
		if chunk < validBitmapSampleBlocks {
			step = validBitmapStreamChunk
		}
		cnt := nBlocks - chunk
		if cnt > step {
			cnt = step
		}
		if chunk+cnt <= fullBlocks {
			simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), cnt, &st, &recs)
		} else {
			// The chunk contains the padded tail block. Space padding is
			// whitespace: it emits nothing and cannot invalidate the block.
			full := fullBlocks - chunk
			if full > 0 {
				simdkernels.Stage1BlocksGP((*byte)(unsafe.Add(base, chunk*64)), full, &st, &recs)
			}
			var tail [64]byte
			for i := range tail {
				tail[i] = ' '
			}
			copy(tail[:], src[fullBlocks*64:])
			var tailRecs [simdkernels.Stage1ChunkBlocks]simdkernels.Stage1Rec
			simdkernels.Stage1BlocksGP(&tail[0], 1, &st, &tailRecs)
			recs[full] = tailRecs[0]
		}

		// Per-block checks run over the whole chunk first, then the grammar
		// machine consumes the chunk's emit masks in one call. Validity is a
		// conjunction of order-independent checks and the walk only ever
		// concludes with a rejection, so regrouping cannot change the verdict;
		// it amortizes the walk's call and state traffic over the chunk.
		var emits [validBitmapStreamChunkGo]uint64
		var scalars [validBitmapStreamChunkGo]uint64
		for i := 0; i < cnt; i++ {
			block := chunk + i
			pos := block * 64
			rec := &recs[i]

			if rec.Bad {
				return false, true
			}
			// UTF-8 is checked per run of non-ASCII blocks while the bytes
			// are still cache-warm; see validBitmapPerBlock for the
			// coalescing rationale.
			if rec.NonASCII {
				if utf8RunStart >= 0 && block-utf8RunEnd > validUTF8CoalesceBlocks {
					if !validUTF8Fast(src[utf8RunStart*64 : utf8RunEnd*64]) {
						return false, true
					}
					utf8RunStart = block
				} else if utf8RunStart < 0 {
					utf8RunStart = block
				}
				utf8RunEnd = block + 1
			}
			if esc := rec.EscInStr; esc != 0 {
				if validBitmapEscapes(src, n, pos, esc, &skipEscape) {
					return false, true
				}
			}

			emits[i] = rec.Emit
			scalars[i] = rec.Scalar
			if block < validBitmapSampleBlocks {
				wsSample += bits.OnesCount64(rec.WsOut)
				emitSample += bits.OnesCount64(rec.Emit)
				inStrSample += bits.OnesCount64(rec.InStr)
				escSample += bits.OnesCount64(rec.EscInStr)
				if block == validBitmapSampleBlocks-1 &&
					validBitmapSampleCommit(wsSample, emitSample, inStrSample, escSample) {
					g.numberMode = validBitmapNumberMode(inStrSample)
				} else if block == validBitmapSampleBlocks-1 {
					return false, false
				}
			}
		}
		if v, done := validBitmapWalkTrusted(src, base, n, chunk*64, emits[:cnt], scalars[:cnt], &g); done {
			return v, true
		}
		chunk += cnt
	}

	if st.Carry.InString != 0 || g.state != vbDone || g.depth != 0 {
		return false, true
	}
	if utf8RunStart >= 0 && !validUTF8Fast(src[utf8RunStart*64:min(utf8RunEnd*64, n)]) {
		return false, true
	}
	return true, true
}

// validBitmapEscapes validates every escape-target byte named in escInStr
// (offsets are block-relative bits over pos). \u needs four hex digits,
// read directly from src across block boundaries; a matched surrogate pair
// records its low half in skipEscape so the next block skips it. It reports
// whether any escape is malformed.
func validBitmapEscapes(src []byte, n, pos int, escInStr uint64, skipEscape *int) (bad bool) {
	if uint(n) > uint(len(src)) {
		return true
	}
	src = src[:n:n]
	base := unsafe.Pointer(unsafe.SliceData(src))
	for e := escInStr; e != 0; e &= e - 1 {
		j := pos + bits.TrailingZeros64(e)
		if uint(j) >= uint(len(src)) {
			return true
		}
		if j == *skipEscape {
			continue
		}
		switch src[j] {
		case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		case 'u':
			if len(src)-j < 5 {
				return true
			}
			u, ok := hex4Fixed((*[4]byte)(unsafe.Add(base, j+1)))
			if !ok {
				return true
			}
			// Surrogate halves must pair, matching the scalar validator.
			if 0xDC00 <= u && u <= 0xDFFF {
				return true
			}
			if 0xD800 <= u && u <= 0xDBFF {
				if len(src)-j < 11 {
					return true
				}
				pair := (*[6]byte)(unsafe.Add(base, j+5))
				if pair[0] != '\\' || pair[1] != 'u' {
					return true
				}
				lo, ok := hex4Fixed((*[4]byte)(unsafe.Add(unsafe.Pointer(pair), 2)))
				if !ok || lo < 0xDC00 || lo > 0xDFFF {
					return true
				}
				*skipEscape = j + 6
			}
		default:
			return true
		}
	}
	return false
}

func hex4Fixed(src *[4]byte) (uint16, bool) {
	a := hexNibbleTable[src[0]]
	b := hexNibbleTable[src[1]]
	c := hexNibbleTable[src[2]]
	d := hexNibbleTable[src[3]]
	return uint16(a)<<12 | uint16(b)<<8 | uint16(c)<<4 | uint16(d), a|b|c|d < 0x10
}

// validBitmapEmitsInBounds proves that every set bit names a source byte.
// Full chunks pass the first comparison; only the padded final chunk inspects
// masks. Once proved, the grammar walkers can dereference extracted positions
// without repeating a bounds branch at every fused transition.
func validBitmapEmitsInBounds(n, pos int, emits []uint64) bool {
	if pos >= 0 && pos <= n && len(emits) <= (n-pos)/64 {
		return true
	}
	for i := len(emits) - 1; i >= 0; i-- {
		wordBase := pos + i*64
		if wordBase >= n {
			if emits[i] != 0 {
				return false
			}
			continue
		}
		if wordBase < 0 {
			return false
		}
		if rel := uint(n - wordBase); rel < 64 && emits[i]>>rel != 0 {
			return false
		}
		break
	}
	return true
}

// validBitmapWalk feeds a run of consecutive blocks' emit masks to the
// grammar machine, advancing g. pos is the byte offset of the first mask;
// each subsequent mask covers the next 64 bytes. done reports that
// validation has concluded (valid carries the verdict, which is always a
// rejection — a valid document is decided only after the last block);
// otherwise the caller proceeds to the next run. Both engines share this
// walk so the grammar lives in exactly one place; the streamed engine
// hands it a whole chunk per call, amortizing the call and the machine's
// state traffic over several blocks.
func validBitmapWalk(src []byte, base unsafe.Pointer, n, pos int, emits, scalarMasks []uint64, g *vbState) (valid, done bool) {
	if !validBitmapEmitsInBounds(n, pos, emits) {
		return false, true
	}
	return validBitmapWalkTrusted(src, base, n, pos, emits, scalarMasks, g)
}

// validBitmapWalkTrusted consumes masks produced internally by stage 1. Those
// masks are already framed to the source length, so production callers avoid
// the checked wrapper's call and argument spills; adversarial direct callers
// retain the checked validBitmapWalk entry point above.
func validBitmapWalkTrusted(src []byte, base unsafe.Pointer, n, pos int, emits, scalarMasks []uint64, g *vbState) (valid, done bool) {
	// state and depth live in locals across the whole run and are stored
	// back only on the suspension return; the early returns all report
	// done, after which g is never read again.
	//
	// The labeled blocks below express the superinstruction policy portably:
	// after '{' or ',' inside
	// an object only a key quote can follow; after a key only ':'; after
	// ':' a string or scalar value dominates; and after a completed object
	// value the follower set is exactly ',' or '}'. Each block peeks the
	// next emit bit under an exact-byte guard, consumes it inline on a
	// hit, and bails to the generic switch on any miss without consuming,
	// so fusion can never change a verdict; it only skips dispatch and state
	// checks made redundant by the guards. Fusions stay gated to object
	// context.
	state := g.state
	depth := g.depth
	numberMode := g.numberMode
	inObject := g.inObject
	containers := unsafe.Pointer(&g.containers[0])
	haveScalarMasks := len(scalarMasks) == len(emits)
	emitBase := unsafe.Pointer(unsafe.SliceData(emits))
	scalarBase := unsafe.Pointer(unsafe.SliceData(scalarMasks))
	wi := 0
	wordBase := 0
	var emit uint64
	var afterKey, afterColon uint64
	var j int

next:
	for emit == 0 {
		if wi == len(emits) {
			g.state = state
			g.depth = depth
			g.inObject = inObject
			return false, false
		}
		wordBase = pos + wi*64
		emit = *(*uint64)(unsafe.Add(emitBase, uintptr(wi)*8))
		wi++
	}
	j = wordBase + bits.TrailingZeros64(emit)
	emit &= emit - 1
	switch fastByteAt(base, j) {
	case '{':
		if state != vbValue && state != vbValueOrEnd {
			return false, true
		}
		if depth >= defaultMaxDepth {
			return false, true
		}
		depth++
		slot := (*uint64)(unsafe.Add(containers, uintptr(depth>>6)*8))
		*slot |= 1 << (depth & 63)
		inObject = true
		state = vbKeyOrEnd
		goto fusedKey
	case '[':
		if state != vbValue && state != vbValueOrEnd {
			return false, true
		}
		if depth >= defaultMaxDepth {
			return false, true
		}
		depth++
		slot := (*uint64)(unsafe.Add(containers, uintptr(depth>>6)*8))
		*slot &^= 1 << (depth & 63)
		inObject = false
		state = vbValueOrEnd
		goto next
	case '}':
		if depth == 0 || !inObject ||
			(state != vbKeyOrEnd && state != vbNext) {
			return false, true
		}
		depth--
		state = vbNext
		if depth == 0 {
			inObject = false
			state = vbDone
			goto next
		}
		parent := *(*uint64)(unsafe.Add(containers, uintptr(depth>>6)*8))
		inObject = parent&(1<<(depth&63)) != 0
		if inObject {
			goto fusedComma
		}
		goto next
	case ']':
		if depth == 0 || inObject ||
			(state != vbValueOrEnd && state != vbNext) {
			return false, true
		}
		depth--
		state = vbNext
		if depth == 0 {
			inObject = false
			state = vbDone
			goto next
		}
		parent := *(*uint64)(unsafe.Add(containers, uintptr(depth>>6)*8))
		inObject = parent&(1<<(depth&63)) != 0
		if inObject {
			goto fusedComma
		}
		goto next
	case ':':
		if state != vbColon {
			return false, true
		}
		state = vbValue
		goto fusedValue
	case ',':
		if state != vbNext {
			return false, true
		}
		if inObject {
			state = vbKey
			goto fusedKey
		}
		state = vbValue
		goto next
	case '"':
		switch state {
		case vbKeyOrEnd, vbKey:
			state = vbColon
			goto fusedColon
		case vbValue, vbValueOrEnd:
			state = vbNext
			if depth == 0 {
				state = vbDone
				goto next
			}
			if inObject {
				goto fusedComma
			}
			goto next
		default:
			return false, true
		}
	default:
		// Scalar value: strict number or literal, which must end at
		// whitespace, a structural byte, or the document's end.
		if state != vbValue && state != vbValueOrEnd {
			return false, true
		}
		goto scalarValue
	}

scalarValue:
	{
		var end int
		terminated := false
		switch c := fastByteAt(base, j); {
		case c == '-' || '0' <= c && c <= '9':
			// The SIMD sample selects the corpus-heavy integer width. Exact
			// delimiter checks keep every ambiguous number on the full scanner.
			if numberMode == vbNumberShort && c != '-' && j+4 <= n {
				invalid := nonDigitMask4(loadUint32LE(unsafe.Add(base, j)))
				if invalid != 0 {
					width := bits.TrailingZeros32(invalid) / 8
					if width != 0 && (c != '0' || width == 1) {
						end = j + width
						if isJSONSpaceOrStructural(fastByteAt(base, end)) {
							terminated = true
							break
						}
					}
				}
			}
			if numberMode == vbNumberNine && c != '-' && c != '0' && j+9 <= n &&
				nonDigitMask8(loadUint64LE(unsafe.Add(base, j))) == 0 &&
				isDigit(fastByteAt(base, j+8)) &&
				(j+9 == n || isJSONSpaceOrStructural(fastByteAt(base, j+9))) {
				end = j + 9
				terminated = true
				break
			}

			// Less common integer widths use stage 1's complete scalar run to
			// recover the exact token end without a bytewise delimiter scan.
			// Keep this after the corpus-specialized paths so they pay no span
			// lookup overhead.
			spanEnd := 0
			if haveScalarMasks {
				off := j - wordBase
				run := *(*uint64)(unsafe.Add(scalarBase, uintptr(wi-1)*8)) >> uint(off)
				if run&1 != 0 {
					width := bits.TrailingZeros64(^run)
					candidate := j + width
					if width < 64-off || candidate == n ||
						wi < len(scalarMasks) && *(*uint64)(unsafe.Add(scalarBase, uintptr(wi)*8))&1 == 0 {
						spanEnd = candidate
					}
				}
			}
			if spanEnd != 0 && c != '-' {
				width := spanEnd - j
				allDigits := width == 1
				switch {
				case width >= 2 && width <= 4 && j+4 <= n:
					lanes := digitHigh4 >> (uint(4-width) * 8)
					allDigits = nonDigitMask4(loadUint32LE(unsafe.Add(base, j)))&lanes == 0
				case width >= 5 && width <= 8 && j+8 <= n:
					lanes := digitHigh >> (uint(8-width) * 8)
					allDigits = nonDigitMask8(loadUint64LE(unsafe.Add(base, j)))&lanes == 0
				}
				if allDigits {
					if c == '0' && width != 1 {
						return false, true
					}
					end = spanEnd
					terminated = true
					break
				}
			}
			var msg string
			end, msg = scanNumber(src, j)
			if msg != "" {
				return false, true
			}
			if spanEnd != 0 {
				if end != spanEnd {
					return false, true
				}
				terminated = true
			}
		case c == 't':
			if !literalTrueAt(src, j) {
				return false, true
			}
			end = j + 4
		case c == 'f':
			if !literalFalseTailAt(src, j) {
				return false, true
			}
			end = j + 5
		case c == 'n':
			if !literalNullAt(src, j) {
				return false, true
			}
			end = j + 4
		default:
			return false, true
		}
		if !terminated && end < n {
			if c := fastByteAt(base, end); !isJSONSpaceOrStructural(c) {
				return false, true
			}
		}
		state = vbNext
		if depth == 0 {
			state = vbDone
			goto next
		}
		if inObject {
			goto fusedComma
		}
		goto next
	}

	// fusedKey: after '{' or ',' in an object, only a key quote is legal.
fusedKey:
	for emit == 0 {
		if wi == len(emits) {
			g.state = state
			g.depth = depth
			g.inObject = inObject
			return false, false
		}
		wordBase = pos + wi*64
		emit = *(*uint64)(unsafe.Add(emitBase, uintptr(wi)*8))
		wi++
	}
	j = wordBase + bits.TrailingZeros64(emit)
	if fastByteAt(base, j) != '"' {
		goto next
	}
	// Object members overwhelmingly keep the key opener, colon, and value
	// start in one mask word. Consume that forced prefix as one
	// superinstruction so the common path pays neither the empty-word gates
	// nor the label round trips between each token. A mismatch leaves the
	// offending token unconsumed for the generic grammar path to reject.
	afterKey = emit & (emit - 1)
	if afterKey != 0 {
		j = wordBase + bits.TrailingZeros64(afterKey)
		if fastByteAt(base, j) != ':' {
			emit = afterKey
			state = vbColon
			goto next
		}
		afterColon = afterKey & (afterKey - 1)
		emit = afterColon
		state = vbValue
		if afterColon == 0 {
			goto fusedValue
		}
		j = wordBase + bits.TrailingZeros64(afterColon)
		switch fastByteAt(base, j) {
		case '"':
			emit = afterColon & (afterColon - 1)
			state = vbNext
			goto fusedComma
		case '{', '[', '}', ']', ':', ',':
			goto next
		default:
			emit = afterColon & (afterColon - 1)
			goto scalarValue
		}
	}
	emit = 0
	state = vbColon

	// fusedColon: after a key quote, only ':' is legal.
fusedColon:
	for emit == 0 {
		if wi == len(emits) {
			g.state = state
			g.depth = depth
			g.inObject = inObject
			return false, false
		}
		wordBase = pos + wi*64
		emit = *(*uint64)(unsafe.Add(emitBase, uintptr(wi)*8))
		wi++
	}
	j = wordBase + bits.TrailingZeros64(emit)
	if fastByteAt(base, j) != ':' {
		goto next
	}
	emit &= emit - 1
	state = vbValue

	// fusedValue: after the fused ':', a string or scalar value is the
	// dominant case; containers bail to their own cases.
fusedValue:
	for emit == 0 {
		if wi == len(emits) {
			g.state = state
			g.depth = depth
			g.inObject = inObject
			return false, false
		}
		wordBase = pos + wi*64
		emit = *(*uint64)(unsafe.Add(emitBase, uintptr(wi)*8))
		wi++
	}
	j = wordBase + bits.TrailingZeros64(emit)
	switch fastByteAt(base, j) {
	case '"':
		emit &= emit - 1
		state = vbNext
		goto fusedComma
	case '{', '[', '}', ']', ':', ',':
		goto next
	}
	emit &= emit - 1
	goto scalarValue

	// fusedComma: after a completed value inside an object, the follower
	// set is exactly ',' or '}'; nested closes chain until an array or
	// the document's root.
fusedComma:
	for emit == 0 {
		if wi == len(emits) {
			g.state = state
			g.depth = depth
			g.inObject = inObject
			return false, false
		}
		wordBase = pos + wi*64
		emit = *(*uint64)(unsafe.Add(emitBase, uintptr(wi)*8))
		wi++
	}
	j = wordBase + bits.TrailingZeros64(emit)
	switch fastByteAt(base, j) {
	case ',':
		emit &= emit - 1
		state = vbKey
		goto fusedKey
	case '}':
		emit &= emit - 1
		depth--
		state = vbNext
		if depth == 0 {
			inObject = false
			state = vbDone
			goto next
		}
		parent := *(*uint64)(unsafe.Add(containers, uintptr(depth>>6)*8))
		inObject = parent&(1<<(depth&63)) != 0
		if inObject {
			goto fusedComma
		}
		goto next
	}
	goto next
}

func isJSONSpaceOrStructural(c byte) bool {
	switch c {
	case ' ', '\t', '\n', '\r', ',', ':', '{', '}', '[', ']':
		return true
	}
	return false
}
