package benchmarks

// Stage-1 consumer study: can a full grammar + entry-write consumer run at
// or below 1.1 ns/position over real corpus masks? This is the fenced
// walker's re-entry precondition sharpened by the gap study
// (stage1_gap_bench_test.go): the bare in-place cursor with a trivial
// switch consumer already costs 1.04-1.10 ns/pos on object corpora, so a
// passing consumer must do the whole job — JSON grammar acceptance,
// container kind/depth matching, and a 16-byte index entry store per
// position — for no more than the trivial switch costs.
//
// Candidates, each measured in isolation over precomputed emit masks:
//
//   ConsumerPairBranchless    branchless pair-legality table machine
//   ConsumerPairEntries       the same plus unconditional 16-byte entry writes
//   ConsumerPairEntriesFlat   flatten to positions, then the branchless body
//   ConsumerDFAEntries        classic DFA LUT (state = table[state|class])
//   ConsumerSwitchEntries     production-shaped switch FSM plus entry writes
//
// Further pair-machine refinements — Reg (register bit-stack kinds),
// Branchy (branched container block), and Golf (Branchy minus codegen slop)
// are documented at their definitions.
//
// The pair machine is built around dependency-chain economics rather than
// instruction minimality:
//
//   - Grammar legality is checked per consecutive token PAIR from a 256-byte
//     table and OR-folded into a sticky bad flag. The table load feeds only
//     the accumulator, never an address or a branch, so unlike a DFA there
//     is no load-to-address loop-carried chain.
//   - The key/value distinction for quotes (the one place JSON needs more
//     than pairs) refines the row: a quote after '{' or after ','-in-object
//     is a key. The refinement reads the PREVIOUS RAW class, which no prior
//     iteration's refinement feeds, so refinement is not loop-carried.
//   - Container kinds live in a byte slab indexed by depth. The only
//     loop-carried chains are depth (one add), inObj (two CSELs), the bad
//     OR, and the mask clear — all single-cycle; the kind load at a close
//     hangs off the depth chain and only stalls the inObj CSEL at closes.
//   - Everything else is branchless: depth underflow via the sign bit,
//     comma-at-depth-0 and depth-overflow via CSET compares, closer kind
//     match via XOR against the current top kind.
//
// Correctness: every variant is differentially tested against a literal
// grammar-only port of the former in-place mask walk (minus scalar body
// scanning — scalar bodies are per-byte work outside the per-position bar)
// on the full corpus, the JSONTestSuite documents plain and wrapped,
// and 20k structured mutations; consumers must also accept everything
// simdjson.Valid accepts.
//
// Rerun from this directory (the differential harness runs as the tests):
//
//	GOEXPERIMENT=simd "$TIP_GO" test -run='^TestConsumer' \
//	  -bench='^BenchmarkConsumer' -benchtime=300ms -count=6 -cpu=1 .

import (
	"bytes"
	"encoding/json"
	"math/bits"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/simdjson"
)

// consumerKindsDummy is the slot the arithmetic-select machine stores to
// when the position is not an open, so consecutive iterations never
// store-forward into the next iteration's kind load. Underflowed depths
// mask to nearby high slots; by then bad is already set.
const consumerKindsDummy = consumerKindsLen - 1

// consumerPairGrammar runs the branchless pair machine over the emit
// masks, grammar verdict only. kinds must be a slab of consumerKindsLen
// bytes with kinds[0] == 0.
//
// The loop-carried chains are depth (+= delta), inObj (a two-gate
// arithmetic select), the sticky bad OR, and the emit-mask clear; every
// load feeds only data, not addresses of later iterations, which is the
// property that separates this machine from a DFA. Kind pushes go through
// a select on the store ADDRESS (real slot at opens, dummy otherwise), so
// the slab never store-forwards into the loop chain.
func consumerPairGrammar(src []byte, emit []uint64, kinds []byte) bool {
	base := unsafe.Pointer(unsafe.SliceData(src))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ct := &consumerClassTab
	pt := &consumerPairBad

	var bad uint64
	depth := int64(0)
	inObj := uint64(0)
	prevRow := uint64(ccRowStart) << 4
	prevIsO, prevIsM := uint64(0), uint64(0)
	maxD := int64(demoMaxDepth + 1)

	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := ct[*(*byte)(unsafe.Add(base, uintptr(j)))]
			cls := fw & 7

			// Pair legality: sticky OR of a table byte. The row was
			// refined and pre-shifted on the previous iteration.
			// Violations accumulate into a fresh per-position local so the
			// only loop-carried OR is the final fold into bad; writing the
			// checks straight into bad would chain six serial ORs through
			// one register per position.
			v := uint64(pt[(prevRow|inObj<<3|cls)&255])

			// Row refinement for the NEXT pair: a quote after '{' or
			// after ','-in-object is a key. Reads the previous RAW class
			// flags, so no refinement chain forms.
			keyCtx := prevIsO | prevIsM&inObj
			row := cls | (fw>>3&1&keyCtx)<<3

			// Comma needs an enclosing container: depth-1 underflows to
			// the sign bit exactly when depth is zero (negative depths
			// have bad set already).
			v |= fw >> 8 & 1 & (uint64(depth-1) >> 63)

			// A closer must match the kind on top of the stack, read from
			// the register — no load.
			isClose := fw >> 5 & 1
			v |= isClose & (inObj ^ fw>>7&1)

			// Depth: one-add chain; underflow via the sign bit, overflow
			// against the production limit.
			depth += int64(fw<<50) >> 62
			v |= uint64(depth) >> 63
			var ov uint64
			if depth == maxD {
				ov = 1
			}
			bad |= v | ov

			// Container kind slab: unconditional load of the kind at the
			// new depth (the enclosing kind after a close), arithmetic
			// select of the new top, and a push store whose address is
			// selected between the real slot (opens) and the dummy.
			isOpen := fw >> 4 & 1
			isObjOpen := fw >> 6 & 1
			di := uintptr(depth) & (consumerKindsLen - 1)
			lv := uint64(*(*byte)(unsafe.Add(kb, di)))
			inObj = isClose&lv | isOpen&isObjOpen | ((isOpen|isClose)^1)&inObj
			soff := di ^ ((di ^ consumerKindsDummy) & (uintptr(isOpen) - 1))
			*(*byte)(unsafe.Add(kb, soff)) = byte(isObjOpen)

			prevRow = row << 4
			prevIsO = isObjOpen
			prevIsM = fw >> 8 & 1
		}
		pos += 64
	}

	bad |= uint64(consumerEOFBad[prevRow>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0
}

// consumerPairEntries is the pair machine plus one 16-byte entry per
// position (start in the first word's low half, the demo info word in the
// second word's high half — the production IndexEntry layout) written as
// two unconditional 8-byte stores; entries must have 2*(positions+64)
// words.
func consumerPairEntries(src []byte, emit []uint64, kinds []byte, entries []uint64) (ok bool, n int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	ct := &consumerClassTab
	pt := &consumerPairBad

	var bad uint64
	depth := int64(0)
	inObj := uint64(0)
	prevRow := uint64(ccRowStart) << 4
	prevIsO, prevIsM := uint64(0), uint64(0)
	maxD := int64(demoMaxDepth + 1)

	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := ct[*(*byte)(unsafe.Add(base, uintptr(j)))]
			cls := fw & 7

			v := uint64(pt[(prevRow|inObj<<3|cls)&255])

			keyCtx := prevIsO | prevIsM&inObj
			row := cls | (fw>>3&1&keyCtx)<<3

			v |= fw >> 8 & 1 & (uint64(depth-1) >> 63)

			isClose := fw >> 5 & 1
			v |= isClose & (inObj ^ fw>>7&1)

			depth += int64(fw<<50) >> 62
			v |= uint64(depth) >> 63
			var ov uint64
			if depth == maxD {
				ov = 1
			}
			bad |= v | ov

			isOpen := fw >> 4 & 1
			isObjOpen := fw >> 6 & 1
			di := uintptr(depth) & (consumerKindsLen - 1)
			lv := uint64(*(*byte)(unsafe.Add(kb, di)))
			inObj = isClose&lv | isOpen&isObjOpen | ((isOpen|isClose)^1)&inObj
			soff := di ^ ((di ^ consumerKindsDummy) & (uintptr(isOpen) - 1))
			*(*byte)(unsafe.Add(kb, soff)) = byte(isObjOpen)

			*(*uint64)(ep) = j                                // start; end filled by later legs
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1) // next 0, info kind
			ep = unsafe.Add(ep, 16)

			prevRow = row << 4
			prevIsO = isObjOpen
			prevIsM = fw >> 8 & 1
		}
		pos += 64
	}

	bad |= uint64(consumerEOFBad[prevRow>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerPairEntriesReg is the pair machine with the container-kind
// stack held entirely in a register as a rotating bit stack: rotate left
// one at an open (pushing the kind into bit 0), rotate right one at a
// close. No memory traffic and no kind load at closes; the loop-carried
// stack chain is rotate+clear+set (three cycles). The register bounds
// tracked depth at 63, so documents that ever reach depth 64 return
// deep=true and must take a fallback walker; depth underflow and the
// production depth limit are subsumed by bad and deep respectively.
func consumerPairEntriesReg(src []byte, emit []uint64, entries []uint64) (ok bool, n int, deep bool) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	ct := &consumerClassTab
	pt := &consumerPairBad

	var bad, dp uint64
	ks := uint64(0) // rotating kind stack; bit 0 = current container kind
	depth := int64(0)
	prevRow := uint64(ccRowStart) << 4
	prevIsO, prevIsM := uint64(0), uint64(0)

	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := ct[*(*byte)(unsafe.Add(base, uintptr(j)))]
			cls := fw & 7
			inObj := ks & 1

			v := uint64(pt[(prevRow|inObj<<3|cls)&255])

			keyCtx := prevIsO | prevIsM&inObj
			row := cls | (fw>>3&1&keyCtx)<<3

			v |= fw >> 8 & 1 & (uint64(depth-1) >> 63)

			isClose := fw >> 5 & 1
			v |= isClose & ((ks ^ fw>>7) & 1)

			depth += int64(fw<<50) >> 62
			v |= uint64(depth) >> 63
			dp |= uint64(depth) >> 6
			bad |= v

			isOpen := fw >> 4 & 1
			isObjOpen := fw >> 6 & 1
			ks = bits.RotateLeft64(ks, int(isOpen)-int(isClose))
			ks = ks&^isOpen | isObjOpen

			*(*uint64)(ep) = j
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
			ep = unsafe.Add(ep, 16)

			prevRow = row << 4
			prevIsO = isObjOpen
			prevIsM = fw >> 8 & 1
		}
		pos += 64
	}

	bad |= uint64(consumerEOFBad[prevRow>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16, dp != 0
}

// consumerPairEntriesBranchy keeps the pair-table grammar core branchless
// but handles container opens and closes in a branched block, the way the
// production walker does: no per-position stack arithmetic at all, one
// predictable branch that only trips at container boundaries. kinds and
// depth semantics match consumerPairGrammar.
func consumerPairEntriesBranchy(src []byte, emit []uint64, kinds []byte, entries []uint64) (ok bool, n int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	ct := &consumerClassTab
	pt := &consumerPairBad

	var bad uint64
	depth := int64(0)
	inObj := uint64(0)
	prevRow := uint64(ccRowStart) << 4
	prevIsO, prevIsM := uint64(0), uint64(0)
	maxD := int64(demoMaxDepth + 1)

	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := ct[*(*byte)(unsafe.Add(base, uintptr(j)))]
			cls := fw & 7

			v := uint64(pt[(prevRow|inObj<<3|cls)&255])

			keyCtx := prevIsO | prevIsM&inObj
			row := cls | (fw>>3&1&keyCtx)<<3

			v |= fw >> 8 & 1 & (uint64(depth-1) >> 63)
			bad |= v

			if fw&(3<<4) != 0 {
				if fw&(1<<4) != 0 { // open
					depth++
					var ov uint64
					if depth == maxD {
						ov = 1
					}
					bad |= ov
					inObj = fw >> 6 & 1
					*(*byte)(unsafe.Add(kb, uintptr(depth)&(consumerKindsLen-1))) = byte(inObj)
				} else { // close
					bad |= inObj ^ fw>>7&1
					depth--
					bad |= uint64(depth) >> 63
					inObj = uint64(*(*byte)(unsafe.Add(kb, uintptr(depth)&(consumerKindsLen-1))))
				}
			}

			*(*uint64)(ep) = j
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
			ep = unsafe.Add(ep, 16)

			prevRow = row << 4
			prevIsO = fw >> 6 & 1
			prevIsM = fw >> 8 & 1
		}
		pos += 64
	}

	bad |= uint64(consumerEOFBad[prevRow>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerPairEntriesFlat runs the same branchless body over materialized
// positions instead of in-place mask bits, so the inner loop's trip count
// is one predictable span. The caller times flattening separately or
// composes it.
func consumerPairEntriesFlat(src []byte, positions []uint32, kinds []byte, entries []uint64) (ok bool, n int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	pb := unsafe.Pointer(unsafe.SliceData(positions))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	ct := &consumerClassTab
	pt := &consumerPairBad

	var bad uint64
	depth := int64(0)
	inObj := uint64(0)
	prevRow := uint64(ccRowStart) << 4
	prevIsO, prevIsM := uint64(0), uint64(0)
	maxD := int64(demoMaxDepth + 1)

	for i := 0; i < len(positions); i++ {
		j := uint64(*(*uint32)(unsafe.Add(pb, uintptr(i)*4)))
		fw := ct[*(*byte)(unsafe.Add(base, uintptr(j)))]
		cls := fw & 7

		v := uint64(pt[(prevRow|inObj<<3|cls)&255])

		keyCtx := prevIsO | prevIsM&inObj
		row := cls | (fw>>3&1&keyCtx)<<3

		v |= fw >> 8 & 1 & (uint64(depth-1) >> 63)

		isClose := fw >> 5 & 1
		v |= isClose & (inObj ^ fw>>7&1)

		depth += int64(fw<<50) >> 62
		v |= uint64(depth) >> 63
		var ov uint64
		if depth == maxD {
			ov = 1
		}
		bad |= v | ov

		isOpen := fw >> 4 & 1
		isObjOpen := fw >> 6 & 1
		di := uintptr(depth) & (consumerKindsLen - 1)
		lv := uint64(*(*byte)(unsafe.Add(kb, di)))
		inObj = isClose&lv | isOpen&isObjOpen | ((isOpen|isClose)^1)&inObj
		soff := di ^ ((di ^ consumerKindsDummy) & (uintptr(isOpen) - 1))
		*(*byte)(unsafe.Add(kb, soff)) = byte(isObjOpen)

		*(*uint64)(ep) = j
		*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
		ep = unsafe.Add(ep, 16)

		prevRow = row << 4
		prevIsO = isObjOpen
		prevIsM = fw >> 8 & 1
	}

	bad |= uint64(consumerEOFBad[prevRow>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerDFATab is the classic branchless DFA: next = t[state<<4 |
// inObj<<3 | cls]. States: 0 value, 1 value-or-end, 2 key-or-end, 3 key,
// 4 colon, 5 next (which at depth 0 doubles as done), 7 reject (sink).
// Depth, kind matching, and comma-at-depth-0 use the same auxiliary
// checks as the pair machine. Its defining cost is the load-to-address
// loop-carried chain: each transition load feeds the next transition's
// table index.
var consumerDFATab = func() (t [128]uint8) {
	for i := range t {
		t[i] = 7
	}
	set := func(st uint8, cls uint64, next uint8) {
		t[uint64(st)<<4|0<<3|cls] = next
		t[uint64(st)<<4|1<<3|cls] = next
	}
	for _, st := range []uint8{0, 1} { // value, value-or-end
		set(st, ccO, 2)
		set(st, ccA, 1)
		set(st, ccQ, 5)
		set(st, ccS, 5)
	}
	set(1, ccB, 5) // empty array close
	set(2, ccQ, 4) // key-or-end
	set(2, ccC, 5)
	set(3, ccQ, 4) // key
	set(4, ccL, 0) // colon
	set(5, ccC, 5) // next
	set(5, ccB, 5)
	t[5<<4|0<<3|ccM] = 0 // comma in array: value
	t[5<<4|1<<3|ccM] = 3 // comma in object: key
	return
}()

// consumerDFAEntries is the DFA-LUT consumer with entry writes.
func consumerDFAEntries(src []byte, emit []uint64, kinds []byte, entries []uint64) (ok bool, n int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep

	var bad uint64
	state := uint64(0)
	depth := int64(0)
	inObj := uint64(0)

	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := consumerClassTab[*(*byte)(unsafe.Add(base, uintptr(j)))]
			cls := fw & 7

			state = uint64(consumerDFATab[(state<<4|inObj<<3|cls)&127])

			v := fw >> 8 & 1 & (uint64(depth-1) >> 63)

			isClose := fw >> 5 & 1
			v |= isClose & (inObj ^ fw>>7&1)

			depth += int64(fw<<50) >> 62
			v |= uint64(depth) >> 63
			var ov uint64
			if depth == demoMaxDepth+1 {
				ov = 1
			}
			bad |= v | ov

			di := uintptr(depth) & (consumerKindsLen - 1)
			lv := uint64(*(*byte)(unsafe.Add(kb, di)))
			isOpen := fw >> 4 & 1
			isObjOpen := fw >> 6 & 1
			if isClose != 0 {
				inObj = lv
			}
			if isOpen != 0 {
				inObj = isObjOpen
			}
			*(*byte)(unsafe.Add(kb, di)) = byte(inObj)

			*(*uint64)(ep) = j
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
			ep = unsafe.Add(ep, 16)
		}
		pos += 64
	}
	if state != 5 || depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerSwitchEntries is the historical in-place comparison: the former
// mask-walk switch FSM (scalar bodies excluded, as everywhere in this study)
// with the same 16-byte entry writes added.
func consumerSwitchEntries(src []byte, emit []uint64, containers []uint64, entries []uint64) (ok bool, n int) {
	const (
		vValue = iota
		vValueOrEnd
		vKeyOrEnd
		vKey
		vColon
		vNext
		vDone
	)
	base := unsafe.Pointer(unsafe.SliceData(src))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	state, depth := vValue, 0

	pos := 0
	for _, m := range emit {
		for m != 0 {
			j := pos + bits.TrailingZeros64(m)
			m &= m - 1
			c := *(*byte)(unsafe.Add(base, uintptr(j)))
			fw := consumerClassTab[c]
			*(*uint64)(ep) = uint64(j)
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
			ep = unsafe.Add(ep, 16)
			switch c {
			case '{':
				if state != vValue && state != vValueOrEnd {
					return false, 0
				}
				if depth >= demoMaxDepth {
					return false, 0
				}
				depth++
				containers[depth>>6] |= 1 << (depth & 63)
				state = vKeyOrEnd
			case '[':
				if state != vValue && state != vValueOrEnd {
					return false, 0
				}
				if depth >= demoMaxDepth {
					return false, 0
				}
				depth++
				containers[depth>>6] &^= 1 << (depth & 63)
				state = vValueOrEnd
			case '}':
				if depth == 0 || containers[depth>>6]&(1<<(depth&63)) == 0 ||
					(state != vKeyOrEnd && state != vNext) {
					return false, 0
				}
				depth--
				state = vNext
				if depth == 0 {
					state = vDone
				}
			case ']':
				if depth == 0 || containers[depth>>6]&(1<<(depth&63)) != 0 ||
					(state != vValueOrEnd && state != vNext) {
					return false, 0
				}
				depth--
				state = vNext
				if depth == 0 {
					state = vDone
				}
			case ':':
				if state != vColon {
					return false, 0
				}
				state = vValue
			case ',':
				if state != vNext {
					return false, 0
				}
				if containers[depth>>6]&(1<<(depth&63)) != 0 {
					state = vKey
				} else {
					state = vValue
				}
			case '"':
				switch state {
				case vKeyOrEnd, vKey:
					state = vColon
				case vValue, vValueOrEnd:
					state = vNext
					if depth == 0 {
						state = vDone
					}
				default:
					return false, 0
				}
			default:
				if state != vValue && state != vValueOrEnd {
					return false, 0
				}
				state = vNext
				if depth == 0 {
					state = vDone
				}
			}
		}
		pos += 64
	}
	return state == vDone && depth == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerClassTabPtr and consumerPairBadPtr hold the table bases as
// loaded pointer values: the compiler rematerializes ADRP+ADD pairs for
// direct global-array indexing inside register-pressured loops, but a
// pointer loaded from a variable stays in a register.
var (
	consumerClassTabPtr = unsafe.Pointer(&consumerClassTab[0])
	consumerPairBadPtr  = unsafe.Pointer(&consumerPairBad[0])
)

// consumerPairEntriesGolf is consumerPairEntriesBranchy with the codegen
// slop shaved off: table bases held as pointer values, the inObj bits
// pre-fused into the saved row index, the key-context bits pre-shifted at
// save time, comma-at-depth-0 as a single AND against a mask register
// maintained in the container block, and the depth limit tracked as a
// headroom counter so no wide constant lives in the loop.
func consumerPairEntriesGolf(src []byte, emit []uint64, kinds []byte, entries []uint64) (ok bool, n int) {
	base := unsafe.Pointer(unsafe.SliceData(src))
	kb := unsafe.Pointer(unsafe.SliceData(kinds))
	ep := unsafe.Pointer(unsafe.SliceData(entries))
	ep0 := ep
	ct := consumerClassTabPtr
	pt := consumerPairBadPtr

	var bad uint64
	depth := int64(0)
	hr := int64(demoMaxDepth) + 1 // headroom to just past the production depth limit
	inObj8 := uint64(0)           // current container kind, pre-shifted to bit 3
	prevRowIO := uint64(ccRowStart)<<4 | 0<<3
	keyRow8 := uint64(0) // 1<<3 when the previous token opens a key context
	dzMask8 := uint64(1) << 8
	pos := uint64(0)
	for _, m := range emit {
		for m != 0 {
			j := pos + uint64(bits.TrailingZeros64(m))
			m &= m - 1
			fw := *(*uint64)(unsafe.Add(ct, uintptr(*(*byte)(unsafe.Add(base, uintptr(j))))<<3))
			cls := fw & 7

			// Pair legality; prevRowIO already carries inObj<<3.
			v := uint64(*(*uint8)(unsafe.Add(pt, uintptr(prevRowIO|cls))))
			// Comma at depth 0: fw bit 8 is isM; dzMask8 holds 1<<8 only
			// while depth is 0, so any nonzero AND marks bad.
			bad |= v | fw&dzMask8

			// Row refinement: fw bit 3 is isQ; keyRow8 was pre-shifted on
			// the previous iteration.
			row := cls | fw&8&keyRow8

			if fw&(3<<4) != 0 {
				if fw&(1<<4) != 0 { // open
					depth++
					hr--
					if hr == 0 {
						bad |= 1
					}
					inObj8 = fw >> 3 & 8
					*(*byte)(unsafe.Add(kb, uintptr(depth)&(consumerKindsLen-1))) = byte(inObj8)
				} else { // close
					bad |= (inObj8>>3 ^ fw>>7) & 1
					depth--
					hr++
					bad |= uint64(depth) >> 63
					inObj8 = uint64(*(*byte)(unsafe.Add(kb, uintptr(depth)&(consumerKindsLen-1))))
				}
				dzMask8 = 0
				if depth == 0 {
					dzMask8 = 1 << 8
				}
			}

			*(*uint64)(ep) = j
			*(*uint64)(unsafe.Add(ep, 8)) = fw &^ (1<<58 - 1)
			ep = unsafe.Add(ep, 16)

			// Save the next pair's context: row and inObj fused into one
			// index; key context = '{' (bit 6) or ','-in-object (bit 8 by
			// inObj), pre-shifted to bit 3.
			prevRowIO = row<<4 | inObj8
			keyRow8 = fw>>3&8 | fw>>5&inObj8
		}
		pos += 64
	}

	bad |= uint64(consumerEOFBad[prevRowIO>>4&15])
	if depth != 0 {
		bad |= 1
	}
	return bad == 0, int(uintptr(ep)-uintptr(ep0)) / 16
}

// consumerOracleWalk is the correctness oracle: a literal grammar-only port
// of the former in-place mask walk (a scalar start is a value token), including
// the depth limit and final-state rule. Every study variant must match its
// verdict on identical masks.
func consumerOracleWalk(src []byte, emit []uint64) bool {
	const (
		vValue = iota
		vValueOrEnd
		vKeyOrEnd
		vKey
		vColon
		vNext
		vDone
	)
	var containers [demoMaxDepth/64 + 1]uint64
	state, depth := vValue, 0
	pos := 0
	for _, m := range emit {
		for m != 0 {
			j := pos + bits.TrailingZeros64(m)
			m &= m - 1
			switch src[j] {
			case '{':
				if state != vValue && state != vValueOrEnd {
					return false
				}
				if depth >= demoMaxDepth {
					return false
				}
				depth++
				containers[depth>>6] |= 1 << (depth & 63)
				state = vKeyOrEnd
			case '[':
				if state != vValue && state != vValueOrEnd {
					return false
				}
				if depth >= demoMaxDepth {
					return false
				}
				depth++
				containers[depth>>6] &^= 1 << (depth & 63)
				state = vValueOrEnd
			case '}':
				if depth == 0 || containers[depth>>6]&(1<<(depth&63)) == 0 ||
					(state != vKeyOrEnd && state != vNext) {
					return false
				}
				depth--
				state = vNext
				if depth == 0 {
					state = vDone
				}
			case ']':
				if depth == 0 || containers[depth>>6]&(1<<(depth&63)) != 0 ||
					(state != vValueOrEnd && state != vNext) {
					return false
				}
				depth--
				state = vNext
				if depth == 0 {
					state = vDone
				}
			case ':':
				if state != vColon {
					return false
				}
				state = vValue
			case ',':
				if state != vNext {
					return false
				}
				if containers[depth>>6]&(1<<(depth&63)) != 0 {
					state = vKey
				} else {
					state = vValue
				}
			case '"':
				switch state {
				case vKeyOrEnd, vKey:
					state = vColon
				case vValue, vValueOrEnd:
					state = vNext
					if depth == 0 {
						state = vDone
					}
				default:
					return false
				}
			default:
				if state != vValue && state != vValueOrEnd {
					return false
				}
				state = vNext
				if depth == 0 {
					state = vDone
				}
			}
		}
		pos += 64
	}
	return state == vDone && depth == 0
}

// --- correctness ---

// consumerCheck runs every variant against the oracle on one input's
// masks and reports the first disagreement.
func consumerCheck(t *testing.T, src []byte, label string) {
	t.Helper()
	emit := stage1EmitMasks(src)
	npos := 0
	for _, m := range emit {
		npos += bits.OnesCount64(m)
	}
	kinds := make([]byte, consumerKindsLen)
	entries := make([]uint64, 2*(npos+flattenSlack))
	positions := make([]uint32, npos+flattenSlack)
	containers := make([]uint64, demoMaxDepth/64+1)

	want := consumerOracleWalk(src, emit)

	if got := consumerPairGrammar(src, emit, kinds); got != want {
		t.Fatalf("%s: pair grammar = %v, oracle = %v\n%.200q", label, got, want, src)
	}
	clear(kinds)
	if got, n := consumerPairEntries(src, emit, kinds, entries); got != want || n != npos {
		t.Fatalf("%s: pair entries = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	}
	nflat := flattenPositions(emit, positions)
	clear(kinds)
	if got, n := consumerPairEntriesFlat(src, positions[:nflat], kinds, entries); got != want || n != npos {
		t.Fatalf("%s: pair flat = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	}
	// The register-stack machine flags documents that ever reach depth 64
	// as deep: those take a fallback walker, so only undeep verdicts must
	// match. A wrong-accept from a missed flag would fail here.
	if got, n, deep := consumerPairEntriesReg(src, emit, entries); !deep && (got != want || n != npos) {
		t.Fatalf("%s: pair reg = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	} else if deep && want {
		// A valid document deeper than 63 must still be flagged, never
		// silently misjudged; the fallback would accept it.
		if got2 := consumerOracleWalk(src, emit); !got2 {
			t.Fatalf("%s: fallback disagrees on deep valid document", label)
		}
	}
	clear(kinds)
	if got, n := consumerPairEntriesBranchy(src, emit, kinds, entries); got != want || n != npos {
		t.Fatalf("%s: pair branchy = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	}
	clear(kinds)
	if got, n := consumerPairEntriesGolf(src, emit, kinds, entries); got != want || n != npos {
		t.Fatalf("%s: pair golf = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	}
	clear(kinds)
	if got, n := consumerDFAEntries(src, emit, kinds, entries); got != want || n != npos {
		t.Fatalf("%s: DFA = %v (n=%d), oracle = %v (npos=%d)\n%.200q", label, got, n, want, npos, src)
	}
	clear(containers)
	if got, _ := consumerSwitchEntries(src, emit, containers, entries); got != want {
		t.Fatalf("%s: switch = %v, oracle = %v\n%.200q", label, got, want, src)
	}
	// One-sided anchor against the shipping validator: grammar is a
	// weaker filter, so full validity must imply grammar acceptance.
	if simdjson.Valid(src) && !want {
		t.Fatalf("%s: simdjson.Valid accepts but grammar oracle rejects\n%.200q", label, src)
	}
}

// TestConsumerCorpora checks acceptance and entry production on every
// corpus payload: verdict true, one entry per position, entry starts equal
// the flattened positions, and entry kinds match the byte classes.
func TestConsumerCorpora(t *testing.T) {
	for _, c := range loadGapCorpora(t) {
		consumerCheck(t, c.src, c.label)

		kinds := make([]byte, consumerKindsLen)
		entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
		ok, n := consumerPairEntries(c.src, c.emit, kinds, entries)
		if !ok || n != len(c.positions) {
			t.Fatalf("%s: ok=%v n=%d want %d", c.label, ok, n, len(c.positions))
		}
		for i, p := range c.positions {
			if uint32(entries[2*i]) != p {
				t.Fatalf("%s: entry %d start = %d, want %d", c.label, i, uint32(entries[2*i]), p)
			}
			wantKind := consumerClassTab[c.src[p]] >> 58 & 7
			if got := entries[2*i+1] >> 58 & 7; got != wantKind {
				t.Fatalf("%s: entry %d kind = %d, want %d", c.label, i, got, wantKind)
			}
		}

	}
}

// TestConsumerGrammarCases pins targeted grammar edges: pair rules, kind
// matching, depth limits, and top-level termination.
func TestConsumerGrammarCases(t *testing.T) {
	if len(loadGapCorpora(t)) == 0 {
		t.Skip("stage-1 kernel unavailable")
	}
	cases := []string{
		// accepted
		`{}`, `[]`, `5`, `"a"`, `true`, `{"a":1}`, `[1,2,3]`,
		`{"a":{"b":[1,{"c":"d"}]},"e":[]}`, `[[[[[]]]]]`, `[{},{}]`,
		`{"a":[1,2],"b":{"c":3}}`, ` [ 1 , "x" ] `, `[["a"],["b"]]`,
		// rejected: pair grammar
		``, ` `, `{,}`, `[1 2]`, `{"a" "b"}`, `{"a":}`, `{:1}`, `[,1]`,
		`{"a":1,}`, `[1,]`, `{"a"}`, `"a" "b"`, `1 2`, `{} {}`, `[] []`,
		`{}[]`, `[]{}`, `1,2`, `{"a":1}:`, `[:]`, `,`, `:`, `[}`, `{]`,
		`[{]}`, `{[}]`, `]`, `}`, `[[]`, `{"a":1`, `[1]]`, `{"a":1}}`,
		`{1:2}`, `[";"]`, `{"a",1}`, `{"a"::1}`, `[[1],`, `["a":1]`,
		`{"a":1 "b":2}`, `x`, `[x]`, `{"k":x}`,
		// depth
		strings.Repeat("[", 9999) + strings.Repeat("]", 9999),
		strings.Repeat("[", 10000) + strings.Repeat("]", 10000),
		strings.Repeat("[", 10001) + strings.Repeat("]", 10001),
		strings.Repeat("[", 20000),
	}
	for _, src := range cases {
		consumerCheck(t, []byte(src), "case "+src[:min(len(src), 40)])
	}
}

// TestConsumerSuperFusedEdges targets the supplied variant's new fused
// paths: the pair-table-free ',' and '}' consumption after completed
// object values (legality there rests on the entry guards, not the
// table), close chains across nesting, depth-zero commas and closes,
// and the fused scalar-value consumption with hostile followers. Every
// case runs through consumerCheck, so the variant must match the oracle
// and the original machine must match it entry for entry.
func TestConsumerSuperFusedEdges(t *testing.T) {
	if len(loadGapCorpora(t)) == 0 {
		t.Skip("stage-1 kernel unavailable")
	}
	cases := []string{
		// fused ',' / '}' after completed object values
		`{"a":1,"b":2}`, `{"a":"x","b":"y"}`, `{"a":{"b":1},"c":2}`,
		`{"a":{"b":{"c":{"d":1}}}}`, `{"a":{"b":1}}`, `{"a":[1,2],"b":{}}`,
		`{"a":{},"b":{}}`, `{"a":{"b":[{"c":1}]}}`,
		// rejections the guards must not blur
		`{,}`, `{"a":1,}`, `{"a":1,,"b":2}`, `{"a":1}}`, `{"a":{"b":1}}}`,
		`{"a":{"b":1}]`, `[{"a":1}}`, `[1}`, `{"a":1},`, `,{"a":1}`,
		`{"a":1}{"b":2}`, `{"a":1} 5`, `{"a":}`, `{"a":,}`, `{"a"::1}`,
		// fused scalar values and their followers
		`{"a":5}`, `{"a":true,"b":null}`, `{"a":5,"b":"x"}`, `{"a":x}`,
		`{"a":5[}`, `{"a":5:1}`, `{"a":5"b"}`, `{"a":[}`, `{"a":]}`,
		`{"a":5,"b":[1,{"c":false}],"d":{}}`,
		// deep chains ending at depth zero, then trailing garbage
		strings.Repeat(`{"k":`, 40) + "1" + strings.Repeat("}", 40),
		strings.Repeat(`{"k":`, 40) + "1" + strings.Repeat("}", 41),
		strings.Repeat(`{"k":`, 40) + "1" + strings.Repeat("}", 39) + "]",
	}
	for _, src := range cases {
		consumerCheck(t, []byte(src), "super edge "+src[:min(len(src), 48)])
	}
}

// TestConsumerTestSuite runs the whole JSONTestSuite corpus, plain and
// indentation-wrapped, through every variant against the oracle.
func TestConsumerTestSuite(t *testing.T) {
	if len(loadGapCorpora(t)) == 0 {
		t.Skip("stage-1 kernel unavailable")
	}
	dir := filepath.Join("..", "testdata", "corpora", "JSONTestSuite", "test_parsing")
	files, err := os.ReadDir(dir)
	if err != nil {
		t.Skip("JSONTestSuite corpus not present")
	}
	indent := "\n" + strings.Repeat(" ", 10)
	for _, entry := range files {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		consumerCheck(t, data, entry.Name())

		var wrapped bytes.Buffer
		wrapped.WriteString("[")
		for range 4 {
			wrapped.WriteString(indent)
			wrapped.Write(data)
			wrapped.WriteString(",")
		}
		wrapped.WriteString(indent)
		wrapped.Write(data)
		wrapped.WriteString("\n]")
		consumerCheck(t, wrapped.Bytes(), "wrapped "+entry.Name())
	}
}

// buildConsumerTestDocument generates a structured ~64KiB document for the
// mutation differential, in the shape of the production bitmap mutation
// harness.
func buildConsumerTestDocument(t *testing.T) []byte {
	t.Helper()
	type leaf struct {
		Name   string    `json:"name"`
		Text   string    `json:"text"`
		Value  float64   `json:"value"`
		Count  int64     `json:"count"`
		Flag   bool      `json:"flag"`
		Scores []float64 `json:"scores"`
	}
	type node struct {
		Leaf     leaf              `json:"leaf"`
		Children []leaf            `json:"children"`
		Index    map[string]string `json:"index"`
	}
	rng := rand.New(rand.NewPCG(3, 5))
	texts := []string{
		"plain ascii", "tab\tand\nnewline", `quote " backslash \ slash /`,
		"unicode   line sep é日本語", "", " leading and trailing ",
	}
	var nodes []node
	for len(nodes) < 24 {
		var children []leaf
		for range rng.IntN(5) {
			children = append(children, leaf{
				Name:   texts[rng.IntN(len(texts))],
				Text:   texts[rng.IntN(len(texts))],
				Value:  rng.Float64() * 1e6,
				Count:  rng.Int64(),
				Flag:   rng.IntN(2) == 0,
				Scores: []float64{rng.Float64(), -rng.Float64() * 1e-7, 0, 1e21},
			})
		}
		nodes = append(nodes, node{
			Leaf:     leaf{Name: texts[rng.IntN(len(texts))], Scores: []float64{}},
			Children: children,
			Index:    map[string]string{"a b": texts[rng.IntN(len(texts))], "c\td": "e"},
		})
	}
	doc, err := json.MarshalIndent(nodes, "", "    ")
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

// TestConsumerMutations mutates the structured document 20k times at
// every position class and compares all variants against the oracle.
func TestConsumerMutations(t *testing.T) {
	if testing.Short() {
		t.Skip("mutation differential is not short")
	}
	if len(loadGapCorpora(t)) == 0 {
		t.Skip("stage-1 kernel unavailable")
	}
	doc := buildConsumerTestDocument(t)
	consumerCheck(t, doc, "base document")

	rng := rand.New(rand.NewPCG(7, 13))
	for mutants := 0; mutants < 20_000; mutants++ {
		mutated := append([]byte(nil), doc...)
		switch rng.IntN(4) {
		case 0:
			mutated[rng.IntN(len(mutated))] = byte(rng.IntN(256))
		case 1:
			hostile := []byte(`"\{}[]:,0x eEtfn.+-` + "\x00\x1f\x80\xe2\xff")
			mutated[rng.IntN(len(mutated))] = hostile[rng.IntN(len(hostile))]
		case 2:
			pos := rng.IntN(len(mutated))
			mutated = append(mutated[:pos], mutated[pos+1:]...)
		case 3:
			mutated = mutated[:rng.IntN(len(mutated))]
		}
		consumerCheck(t, mutated, "mutant")
	}
}

// --- benchmarks ---

func BenchmarkConsumerPairBranchless(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				boolSink = consumerPairGrammar(c.src, c.emit, kinds)
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerPairEntries(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n := consumerPairEntries(c.src, c.emit, kinds, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerPairEntriesReg(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n, _ := consumerPairEntriesReg(c.src, c.emit, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerPairEntriesBranchy(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n := consumerPairEntriesBranchy(c.src, c.emit, kinds, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerPairEntriesGolf(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n := consumerPairEntriesGolf(c.src, c.emit, kinds, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// BenchmarkConsumerPairEntriesFlat composes flatten and the branchless
// body: what a position-materializing engine would pay end to end from
// precomputed masks.
func BenchmarkConsumerPairEntriesFlat(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			dst := make([]uint32, len(c.positions)+flattenSlack)
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				n := flattenPositions(c.emit, dst)
				ok, k := consumerPairEntriesFlat(c.src, dst[:n], kinds, entries)
				boolSink = ok
				intSink = k
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerDFAEntries(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			kinds := make([]byte, consumerKindsLen)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n := consumerDFAEntries(c.src, c.emit, kinds, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

func BenchmarkConsumerSwitchEntries(b *testing.B) {
	for _, c := range loadGapCorpora(b) {
		b.Run(c.label, func(b *testing.B) {
			containers := make([]uint64, demoMaxDepth/64+1)
			entries := make([]uint64, 2*(len(c.positions)+flattenSlack))
			b.SetBytes(int64(len(c.src)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				ok, n := consumerSwitchEntries(c.src, c.emit, containers, entries)
				boolSink = ok
				intSink = n
			}
			reportPerPosition(b, len(c.positions))
		})
	}
}

// --- decomposition experiments ---

// consumerDenseRemap rebuilds a corpus with the same token sequence packed
// into consecutive positions: src2 is the emitted bytes concatenated, and
// every mask word is fully set. Comparing against the real corpus isolates
// mask-shape costs (word exits, sparse bit iteration) from token-sequence
// costs (dispatch prediction, grammar work), because the class sequence —
// and therefore the grammar — is unchanged.
func consumerDenseRemap(c gapCorpus) (src2 []byte, emit2 []uint64) {
	src2 = make([]byte, len(c.positions))
	for i, p := range c.positions {
		src2[i] = c.src[p]
	}
	n := len(src2)
	emit2 = make([]uint64, (n+63)/64)
	for i := range emit2 {
		emit2[i] = ^uint64(0)
	}
	if r := n % 64; r != 0 {
		emit2[len(emit2)-1] = 1<<r - 1
	}
	return
}

// consumerUniformRemap keeps the real mask shape but rewrites every
// emitted byte to a digit, so dispatch always takes the scalar handler:
// the complement experiment, isolating mask-shape cost with zero dispatch
// entropy. The grammar rejects, which is fine — the machine never exits
// early.
func consumerUniformRemap(c gapCorpus) (src2 []byte) {
	src2 = append([]byte(nil), c.src...)
	for _, p := range c.positions {
		src2[p] = '5'
	}
	return
}
