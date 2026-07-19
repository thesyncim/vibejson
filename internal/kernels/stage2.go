package kernels

// Stage-2 grammar state and tables are shared by the Go-native packed-position
// validator and production index machines. They judge token-pair
// legality, container kind and depth matching, comma and closer placement,
// and legal document endings while consuming each stage-1 position once.
//
// The grammar lives in three small tables shared by every build:
//
//   - stage2Class maps a source byte to one of eight classes: the six
//     structural bytes, the quote, and "scalar start".
//   - stage2PairBad marks illegal (previous refined class, in-object,
//     current class) triples. Rows are refined previous classes — a quote
//     after '{' or after ','-in-object is a key — columns are raw current
//     classes. Depth-dependent rules (comma or closer at depth zero,
//     closer kind, depth overflow) are enforced by the machine itself.
//   - stage2EOFBad marks refined previous classes that cannot end a
//     document; Stage2Finish folds it into the verdict.
//
// Violations accumulate into a sticky Bad word: the machine never stops
// early, it runs to the end of the positions it was handed and the caller
// checks Bad once per call. Acceptance is decided only by Stage2Finish
// after the last chunk.

// Stage2MaxDepth is the machine's container depth limit. It must equal
// the validator's walk limit; the caller asserts the equality at compile
// time.
const Stage2MaxDepth = 10000

// Stage2KindsLen sizes the container-kind slab: a power of two above
// Stage2MaxDepth plus slack, so depth&(Stage2KindsLen-1) addressing stays
// in bounds even while a too-deep or underflowed document runs on with
// Bad already set.
const Stage2KindsLen = 16384

// Stage2State carries the machine's registers between chunk calls. The
// zero value is NOT the document-start state; call Stage2Reset first.
type Stage2State struct {
	// Bad is the sticky violation accumulator; nonzero means the
	// document is already rejected. The caller checks it after every
	// call to preserve chunk-granular early rejection.
	Bad uint64
	// Depth is the current container nesting depth. It runs on past the
	// limit and below zero once Bad is set.
	Depth int64
	// PrevRowIO is the refined previous class pre-shifted for the pair
	// table: row<<4 | inObj8, where inObj8 is 8 inside an object.
	PrevRowIO uint64
	// KeyRow8 holds 1<<3 when the previous token opens a key context
	// ('{', or ','-in-object), so the next quote refines to a key.
	KeyRow8 uint64
}

// Token class codes. Openers and closers are arranged so the tables stay
// dense; stage2RowStart and stage2RowQk extend the row space for the pair
// table (rows are refined previous classes, columns raw current classes).
const (
	stage2ccO = iota // {
	stage2ccA        // [
	stage2ccC        // }
	stage2ccB        // ]
	stage2ccL        // :
	stage2ccM        // ,
	stage2ccQ        // "
	stage2ccS        // scalar start

	stage2RowStart = 8  // virtual class before the first token
	stage2RowQk    = 14 // ccQ refined to a key quote (ccQ | 1<<3)
)

// Stage2Reset puts st in the document-start state. It does not clear the
// caller's kind slab: the machine requires a slab whose byte 0 reads as array
// (bit 3 clear) at document start. A freshly zeroed slab satisfies this, and
// every deeper slot is written before it is read.
func Stage2Reset(st *Stage2State) {
	*st = Stage2State{PrevRowIO: stage2RowStart << 4}
}

// Stage2Finish folds the end-of-document rules into the verdict: no
// violation accumulated, every container closed, and the final token one
// that may end a document (a closer, a value string, or a scalar).
func Stage2Finish(st *Stage2State) bool {
	bad := st.Bad | uint64(stage2EOFBad[st.PrevRowIO>>4&15])
	if st.Depth != 0 {
		bad |= 1
	}
	return bad == 0
}

// stage2ClsOff maps each source byte to its class in a 128-stride encoding;
// stage2Class below compacts the same canonical mapping for Go dispatch.
var stage2ClsOff = func() (t [256]uint64) {
	for i := range t {
		var cls uint64
		switch byte(i) {
		case '{':
			cls = stage2ccO
		case '[':
			cls = stage2ccA
		case '}':
			cls = stage2ccC
		case ']':
			cls = stage2ccB
		case ':':
			cls = stage2ccL
		case ',':
			cls = stage2ccM
		case '"':
			cls = stage2ccQ
		default:
			cls = stage2ccS
		}
		t[i] = cls * 128
	}
	return
}()

// stage2Class is the compact Go-machine companion to stage2ClsOff. Go's dense
// switches and pair-table indexes want the class itself.
var stage2Class = func() (t [256]uint8) {
	for i := range t {
		t[i] = uint8(stage2ClsOff[i] >> 7)
	}
	return
}()

// stage2ScalarEnd marks bytes that may terminate a JSON literal or number.
// A byte outside the document needs no table entry; the scanner handles EOF
// before consulting this table.
var stage2ScalarEnd = func() (t [256]uint8) {
	for _, c := range [...]byte{' ', '\t', '\n', '\r', ',', ':', '{', '}', '[', ']'} {
		t[c] = 1
	}
	return
}()

// stage2PairBad is the pair-legality table: 1 marks an illegal (previous
// refined class, inObj, current raw class) triple. Index is
// prevRow<<4 | inObj<<3 | cls. Rows not constructible by the machine stay
// illegal. Depth-dependent rules are enforced in the machine; this table
// is pure token-pair grammar.
var stage2PairBad = func() (t [256]uint8) {
	for i := range t {
		t[i] = 1
	}
	legal := func(row uint64, inObj int, cols ...uint64) {
		for _, c := range cols {
			t[row<<4|uint64(inObj)<<3|c] = 0
		}
	}
	both := func(row uint64, cols ...uint64) {
		legal(row, 0, cols...)
		legal(row, 1, cols...)
	}
	value := []uint64{stage2ccO, stage2ccA, stage2ccQ, stage2ccS}
	after := []uint64{stage2ccM, stage2ccC, stage2ccB}
	both(stage2ccO, stage2ccQ, stage2ccC)        // { : key or empty close
	both(stage2ccA, append(value, stage2ccB)...) // [ : any value or empty close
	both(stage2ccC, after...)                    // } : separator or another closer
	both(stage2ccB, after...)                    // ] : separator or another closer
	both(stage2ccL, value...)                    // : : any value
	legal(stage2ccM, 0, value...)                // , in array: any value
	legal(stage2ccM, 1, stage2ccQ)               // , in object: key quote only
	both(stage2ccQ, after...)                    // value string: separator or closer
	both(stage2ccS, after...)                    // scalar: separator or closer
	both(stage2RowQk, stage2ccL)                 // key string: colon only
	both(stage2RowStart, value...)               // document start: any value
	return
}()

// stage2EOFBad marks refined previous classes that cannot end a document
// (indexed by the row code). Completed values — scalars, value strings,
// and closers — are the only legal final tokens; depth==0 is checked
// separately by Stage2Finish.
var stage2EOFBad = func() (t [16]uint8) {
	for i := range t {
		t[i] = 1
	}
	t[stage2ccC], t[stage2ccB], t[stage2ccQ], t[stage2ccS] = 0, 0, 0, 0
	return
}()
