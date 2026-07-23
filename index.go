package simdjson

import (
	"math/bits"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// The structural index ("the tape").
//
// An Index is the flattened form of one JSON document: a contiguous array of
// fixed-size entries, written in document order, one entry per structural
// value — a header entry for each container, one entry for each scalar, and
// one entry for each object key. The source is never copied or rewritten;
// entries carry byte coordinates into it, so the tape is a navigation layer
// over the original text rather than a decoded copy of it. Building the tape
// is also the validation pass: an Index exists only for well-formed input.
//
// Every entry is four uint32 words, 16 bytes, no padding:
//
//	 0       4       8       12      16
//	+-------+-------+-------+-------+
//	| start |  end  | next  | info  |
//	+-------+-------+-------+-------+
//
//	start  offset of the value's first source byte (strings: the open quote)
//	end    one past the value's last byte (strings: past the close quote)
//	next   entries from this one to the next value at the same nesting level
//	info   count, kind, and flags in one packed word (diagram at the consts)
//
// A small document and its tape:
//
//	{"a":1,"b":[true,"x"]}
//
//	 #  kind    span     next  count  flags
//	 0  Object  [0,22)   7     2
//	 1  String  [1,4)    1            key       "a"
//	 2  Number  [5,6)    1            integer   1
//	 3  String  [7,10)   1            key       "b"
//	 4  Array   [11,21)  3     2
//	 5  Bool    [12,16)  1                      true
//	 6  String  [17,20)  1                      "x"
//
// next is the structure. For a container it is the size of the container's
// subtree in entries, so header+next is the first entry past the container: a
// skip link that steps over any value in O(1) regardless of its size. For
// scalars it is 1. Navigation is two rules — a container's first child is
// header+1, a value's next sibling is value+value.next — and every traversal
// primitive in the package reduces to them.
//
// The next word is written up to three ways over an entry's life, which is
// the key story of this layer:
//
//  1. While a container is open during the diagnostic build, its next word
//     temporarily holds its parent's entry number: the builder's scope stack
//     is threaded through the tape itself (pushContainer/finishContainer),
//     so building needs no side allocation.
//  2. When the container closes, the word is overwritten with the subtree
//     size — the skip link above.
//  3. For object keys the word is dead after building: navigation always
//     steps key -> key+1 (the value) -> value+value.next, never through a
//     key's own next. The optional enrichment pass (index_keyhash.go)
//     therefore repurposes it to hold a hash of the key's content, which the
//     accelerated lookup paths compare instead of key bytes.
//
// Flat containers: when every direct member value of a container is a single
// entry (scalars and empty containers), members sit at a fixed stride, and
// the identity header.next == count+1 (arrays) or 2*count+1 (objects)
// detects that layout from the header alone. Every accelerated path —
// indexed element access, the vectorized lookup scan (index_tapescan.go),
// and the shape layer (shape.go) — is gated on this identity.
//
// Three engines build the same tape, tried fastest first by
// buildIndexOptions:
//
//   - buildIndexPositions (index_positions.go): the SIMD stage-1 engine for
//     large documents, deriving entries from structural-character bitmaps.
//     It only shortcuts acceptance; any decline falls through.
//   - parseFast/walkFast (this file): the portable happy path, an
//     allocation-free iterative state machine with a fixed 64-frame scope
//     stack. It reports invalid or oversized input without diagnosing it.
//   - parse (this file): the diagnostic builder. Slower, bounded only by the
//     caller's depth option, and the sole authority on error text: whatever
//     a fast engine declines is re-parsed here so errors are exact.
//
// Costs and limits: building is one pass, O(len(src)); navigation is O(1)
// per step. Coordinates are uint32, so a document and its entry storage are
// capped at 4 GiB (document.ErrIndexTooLarge). Entry storage is caller-owned
// — RequiredIndexEntries sizes it exactly — and overflowing it reports
// document.ErrIndexFull rather than allocating.
//
// Terminology, used consistently across the layer: the "tape" is the entry
// array an Index wraps; an "entry" is one 16-byte record on it; a "member"
// is an object's key/value pair; "enrichment" is the optional key-hash pass
// (index_keyhash.go); an "arena" is append-only chunked storage whose bytes
// never move (intern.go, docset.go, shape.go); a "shape" is a compiled
// flat-object layout (shape.go); a "shape tape" is a document stored as
// value entries only, its keys deduplicated into the shape
// (docset_shape.go).

// Each flag qualifies one kind and is zero elsewhere: escaped and key apply to
// strings, integer to numbers.
const (
	tapeFlagEscaped = 1 << iota // string contains at least one escape sequence
	tapeFlagKey                 // string is an object key
	tapeFlagInt                 // number is a plain integer: optional minus, then digits only
)

// tapeFlagObjectKeysHashed marks, on an Object header entry only, that the
// object's key entries carry precomputed content hashes in their next word
// (see enrichKeyHashes and Node.Get). It reuses the escaped bit position: no
// accessor interprets the escaped, key, or integer bit for an Object kind, so
// the bit is free to repurpose there. The integer bit is deliberately avoided
// because IsInteger tests it without a prior kind check.
const tapeFlagObjectKeysHashed = tapeFlagEscaped

// keysHashed reports whether this Object header was enriched with per-key
// hashes. It is meaningful only on an Object entry.
func (e *IndexEntry) keysHashed() bool {
	return e.flags()&tapeFlagObjectKeysHashed != 0
}

// The info word packs a container's direct element count together with the
// entry's kind and flags, so an entry stays four uint32 words (16 bytes) with
// no padding. count occupies the low 26 bits; kind the next 3; flags the top 3:
//
//	 31     29 28    26 25                        0
//	+---------+--------+--------------------------+
//	|  flags  |  kind  |          count           |
//	+---------+--------+--------------------------+
//
// count is meaningful only for containers, where it holds the number of direct
// members; scalars leave it zero. Its 26-bit width caps a single container at
// infoMaxCount direct members. The builders reject any input that would exceed
// that (see [document.ErrIndexTooLarge]); reaching the cap needs a source
// larger than 128 MiB packed entirely into one container, so it never arises
// in practice.
const (
	infoCountBits         = 26
	infoKindBits          = 3
	infoCountMask  uint32 = 1<<infoCountBits - 1
	infoKindShift         = infoCountBits
	infoKindMask   uint32 = (1<<infoKindBits - 1) << infoKindShift
	infoFlagsShift        = infoCountBits + infoKindBits
	infoMaxCount   uint32 = infoCountMask
)

// IndexEntry is one compact structural entry in an Index. Its fields are private
// so callers can provide reusable storage without being coupled to the layout;
// kind, flags, and count share one packed word behind accessor methods, so the
// layout can change without touching every reader.
type IndexEntry struct {
	start uint32
	end   uint32
	next  uint32
	info  uint32
}

// Kind returns the entry's JSON kind.
func (e *IndexEntry) Kind() document.Kind {
	return document.Kind((e.info & infoKindMask) >> infoKindShift)
}

// flags returns the entry's tape flags (escaped, key, integer).
func (e *IndexEntry) flags() uint8 {
	return uint8(e.info >> infoFlagsShift)
}

// Count returns a container's direct element count. It is meaningful only for
// arrays and objects; other kinds report zero.
func (e *IndexEntry) Count() uint32 {
	return e.info & infoCountMask
}

// packInfo composes an info word from its parts. The caller guarantees count
// fits in infoCountBits; the builders check this before an entry is written.
func packInfo(count uint32, kind document.Kind, flags uint8) uint32 {
	return count&infoCountMask | uint32(kind)<<infoKindShift | uint32(flags)<<infoFlagsShift
}

// setCount replaces the entry's element count, preserving kind and flags.
func (e *IndexEntry) setCount(count uint32) {
	e.info = e.info&^infoCountMask | count&infoCountMask
}

// bumpCount adds one to the entry's element count in place. count occupies the
// low bits of info, so an increment cannot disturb kind or flags unless it
// overflows the count field, which the builders prevent.
func (e *IndexEntry) bumpCount() {
	e.info++
}

// Index is an immutable, zero-copy navigation index over validated JSON.
// Building an Index scans the complete document and writes one compact entry
// per structural value. It is intended for repeated or out-of-order access to
// one document; use GetRaw for a single pointer lookup and Parse when the
// document should own its backing storage.
//
// An Index aliases both its source and entry storage. Neither may be modified
// or reused while the Index or any Node obtained from it is in use. Concurrent
// reads are safe when both remain immutable.
type Index struct {
	src     []byte
	entries []IndexEntry
}

// buildIndexOptions is the engine router behind BuildIndex and
// BuildIndexOptions: bitmap engine, then fast walk, then diagnostic parse,
// per the routing rules in the file comment; enrichment runs last on
// whichever tape was accepted.
func buildIndexOptions(src []byte, storage []IndexEntry, opts document.IndexOptions) (Index, error) {
	if uint64(len(src)) > uint64(^uint32(0)) || uint64(cap(storage)) > uint64(^uint32(0)) {
		return Index{}, document.ErrIndexTooLarge
	}
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	// The position engine (index_positions.go) takes large documents. It only
	// shortcuts acceptance: any decline falls through to the portable builder
	// below, which decides the exact error. The depth gate keeps callers'
	// tighter limits with the builder that enforces them.
	fallbackNumberMode := uint8(tapeNumberScalar)
	if maxDepth >= fastWalkMaxDepth &&
		len(src) >= validBitmapMinBytes && len(src) < indexBitmapMaxBytes {
		if entries, ok := buildIndexPositions(src, storage); ok {
			index := Index{src: src, entries: entries}
			if opts.HashKeys {
				enrichKeyHashes(&index)
			}
			return index, nil
		}
		fallbackNumberMode = indexFallbackNumberMode(src)
	}
	b := tapeBuilder{
		src:      src,
		base:     byteSourceOf(src).pointerAt(0),
		entries:  storage[:0],
		parent:   noTapeParent,
		maxDepth: maxDepth,
	}
	var status tapeParseStatus
	if fallbackNumberMode == tapeNumberSWAR {
		status = b.parseFastSWAR()
	} else {
		status = b.parseFast()
	}
	switch status {
	case tapeParseOK:
	case tapeParseFull:
		return Index{}, document.ErrIndexFull
	default:
		b.entries = storage[:0]
		b.i = 0
		b.sp = 0
		b.parent = noTapeParent
		if err := b.parse(); err != nil {
			return Index{}, err
		}
	}
	index := Index{src: src, entries: b.entries}
	if opts.HashKeys {
		enrichKeyHashes(&index)
	}
	return index, nil
}

// RequiredIndexEntries validates src and returns the exact storage length
// BuildIndex needs. Ordinary documents are counted without heap allocation.
func RequiredIndexEntries(src []byte) (int, error) {
	l, err := countLayout(src, defaultMaxDepth)
	if err != nil {
		return 0, err
	}
	return 1 + l.values + 2*l.members, nil
}

// Len returns the number of structural entries in the index.
func (t Index) Len() int {
	return len(t.entries)
}

// Root returns the document's top-level node.
func (t Index) Root() Node {
	return nodeFromStorage(t.src, t.entries)
}

// Pointer returns a JSON Pointer target. CompilePointer plus PointerCompiled is
// preferable on hot paths because pointer compilation may allocate.
func (t Index) Pointer(pointer string) (Node, bool, error) {
	return t.Root().Pointer(pointer)
}

// PointerCompiled returns a precompiled JSON Pointer target without allocating.
func (t Index) PointerCompiled(pointer CompiledPointer) (Node, bool, error) {
	return t.Root().PointerCompiled(pointer)
}

// A tapeBuilder holds the state shared by the two portable engines: the
// source, its base pointer for the read kernels, the destination entries
// (aliasing caller storage, extended only within its capacity), and the byte
// cursor i. parent and sp belong to the diagnostic engine: parent is the
// entry number of the innermost open container — the head of the scope stack
// threaded through container next words — and sp is the open depth, checked
// against maxDepth.
type tapeBuilder struct {
	src      []byte
	base     unsafe.Pointer
	entries  []IndexEntry
	parent   uint32
	i        int
	sp       int
	maxDepth int
}

// noTapeParent marks the scope stack empty: no container is open.
const noTapeParent uint32 = ^uint32(0)

// The number modes select the digit scanner for the portable walk after the
// bitmap engine declines: tapeNumberSWAR takes the word-at-a-time scanner on
// inputs whose number density rewards it (see indexFallbackNumberMode).
const (
	tapeNumberScalar uint8 = iota
	tapeNumberSWAR
)

// tapeParseStatus is a fast engine's three-way verdict: ok, invalid (retry
// through the diagnostic parser, which produces the exact error), or full
// (caller storage exhausted, reported as document.ErrIndexFull directly —
// a retry could not succeed either).
type tapeParseStatus uint8

const (
	tapeParseOK tapeParseStatus = iota
	tapeParseInvalid
	tapeParseFull
)

// parseFast is the happy-path tape builder: an iterative walk with an inline
// one-word fast path for short clean strings. It reports full or invalid input
// so BuildIndex can fall back to the diagnostic parser; it also defers any
// document nested past fastWalkMaxDepth to that parser, so the walk carries a
// small fixed scope stack instead of an unbounded one.
func (b *tapeBuilder) parseFast() tapeParseStatus {
	b.skipSpace()
	if b.i >= len(b.src) {
		return tapeParseInvalid
	}
	if status := b.walkFast(); status != tapeParseOK {
		return status
	}
	b.skipSpace()
	if b.i != len(b.src) {
		return tapeParseInvalid
	}
	return tapeParseOK
}

// stringFast records one string entry starting at the opening quote.
func (b *tapeBuilder) stringFast(start int, flags uint8) tapeParseStatus {
	scanStart := start + 1
	if start+9 <= len(b.src) {
		if m := stringSpecialMask(loadUint64LE(byteSourceFromPointer(b.base).pointerAt(start + 1))); m != 0 {
			j := start + 1 + bits.TrailingZeros64(m)/8
			if b.src[j] == '"' {
				if len(b.entries) == cap(b.entries) {
					return tapeParseFull
				}
				entry := len(b.entries)
				b.entries = b.entries[:entry+1]
				b.entries[entry] = IndexEntry{start: uint32(start), end: uint32(j + 1), next: 1, info: packInfo(0, document.String, flags)}
				b.i = j + 1
				return tapeParseOK
			}
			scanStart = j
		} else {
			scanStart += 8
		}
	}
	end, escaped, ok := scanJSONStringFastFrom(b.src, b.base, scanStart)
	if !ok {
		return tapeParseInvalid
	}
	if escaped {
		flags |= tapeFlagEscaped
	}
	if len(b.entries) == cap(b.entries) {
		return tapeParseFull
	}
	entry := len(b.entries)
	b.entries = b.entries[:entry+1]
	b.entries[entry] = IndexEntry{start: uint32(start), end: uint32(end), next: 1, info: packInfo(0, document.String, flags)}
	b.i = end
	return tapeParseOK
}

// fastWalkMaxDepth bounds the container nesting the iterative walk handles
// inline. Its open-scope stack lives in one fixed on-stack frame so the walk
// stays allocation-free; the cap keeps that frame small. Anything deeper
// diverts to the diagnostic parser, which is bounded only by maxDepth.
const fastWalkMaxDepth = 64

// Provenance: CPP-WALK-001.
// walkFast adapts the state-machine shape of C++ simdjson 4.6.4
// json_iterator::walk_document at commit
// 1bcf71bd85059ab6574ea1159de9298dcc1212c5,
// src/generic/stage2/json_iterator.h; Apache-2.0, see LICENSE-SIMDJSON. Local
// changes build a Go-owned tape, preserve exact error offsets, and fuse local
// primitive scanners.
//
// walkFast is the iterative core of parseFast. It is a labeled state machine over an explicit stack of
// open containers, so each nested value is reached by a jump rather than a
// recursive call and its prologue. Each open scope records the container's
// entry index (to backpatch its span, count, and next once it closes), its
// running direct-member count, and whether it is an array; the byte at b.i on
// entry is the significant start of the document's root value.
//
// The token guards lean on nextSignificantFast reporting c==0 at end of input:
// that sentinel is not a structural byte, so a comparison against a real token
// rejects it without a separate length check. Guards that instead feed the
// position straight into a byte read keep an explicit i >= n check to stay in
// bounds.
func (b *tapeBuilder) walkFast() tapeParseStatus {
	n := len(b.src)
	base := b.base

	var entryStack [fastWalkMaxDepth]uint32
	var countStack [fastWalkMaxDepth]uint32
	var arrayStack [fastWalkMaxDepth]bool
	sp := 0

	// Nesting past the stack, or past the caller's own limit, diverts to the
	// diagnostic parser, which enforces maxDepth and reports the error.
	depthLimit := b.maxDepth
	if depthLimit > fastWalkMaxDepth {
		depthLimit = fastWalkMaxDepth
	}

	i := b.i
	var c byte

value:
	switch fastByteAt(base, i) {
	case '{':
		if sp >= depthLimit {
			return tapeParseInvalid
		}
		if len(b.entries) == cap(b.entries) {
			return tapeParseFull
		}
		entry := uint32(len(b.entries))
		b.entries = b.entries[:entry+1]
		b.entries[entry] = IndexEntry{start: uint32(i), info: packInfo(0, document.Object, 0)}
		i, c = nextSignificantFast(base, n, i+1)
		if c == '}' {
			b.entries[entry].end = uint32(i + 1)
			b.entries[entry].next = uint32(len(b.entries)) - entry
			i++
			goto scopeEnd
		}
		entryStack[sp] = entry
		countStack[sp] = 0
		arrayStack[sp] = false
		sp++
		goto objectKey
	case '[':
		if sp >= depthLimit {
			return tapeParseInvalid
		}
		if len(b.entries) == cap(b.entries) {
			return tapeParseFull
		}
		entry := uint32(len(b.entries))
		b.entries = b.entries[:entry+1]
		b.entries[entry] = IndexEntry{start: uint32(i), info: packInfo(0, document.Array, 0)}
		i, c = nextSignificantFast(base, n, i+1)
		if i >= n {
			// A non-empty array reads src[i] as its first value start below, so
			// the end-of-input position must be rejected before that read.
			return tapeParseInvalid
		}
		if c == ']' {
			b.entries[entry].end = uint32(i + 1)
			b.entries[entry].next = uint32(len(b.entries)) - entry
			i++
			goto scopeEnd
		}
		entryStack[sp] = entry
		countStack[sp] = 0
		arrayStack[sp] = true
		sp++
		// i and c already point at the first element's significant byte.
		goto value
	case '"':
		if status := b.stringFast(i, 0); status != tapeParseOK {
			return status
		}
		i = b.i
		goto scopeEnd
	case 't':
		if i+4 > n || loadUint32LE(byteSourceFromPointer(base).pointerAt(i)) != wordTrueLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Bool, 0); status != tapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	case 'f':
		if i+5 > n || loadUint32LE(byteSourceFromPointer(base).pointerAt(i+1)) != wordAlseLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+5, document.Bool, 0); status != tapeParseOK {
			return status
		}
		i += 5
		goto scopeEnd
	case 'n':
		if i+4 > n || loadUint32LE(byteSourceFromPointer(base).pointerAt(i)) != wordNullLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Null, 0); status != tapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	default:
		ch := fastByteAt(base, i)
		if ch != '-' && !isDigit(ch) {
			return tapeParseInvalid
		}
		end, integer, ok := scanNumberFastTagged(base, n, i)
		if !ok {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, end, document.Number, numberFlags(integer)); status != tapeParseOK {
			return status
		}
		i = end
		goto scopeEnd
	}

	// objectKey consumes a quoted key and its colon, then falls into value to
	// read the member value. c holds the byte at i.
objectKey:
	if c != '"' {
		return tapeParseInvalid
	}
	if status := b.stringFast(i, tapeFlagKey); status != tapeParseOK {
		return status
	}
	i, c = nextSignificantFast(base, n, b.i)
	if c != ':' {
		return tapeParseInvalid
	}
	i = skipSpaceFast(base, n, i+1)
	if i >= n {
		return tapeParseInvalid
	}
	goto value

	// scopeEnd runs after a complete value ending at i. With no scope open the
	// document's root value is done; otherwise it advances the innermost
	// container, either to its next member or past its closing bracket.
scopeEnd:
	if sp == 0 {
		b.i = i
		return tapeParseOK
	}
	{
		i, c = nextSignificantFast(base, n, i)
		top := sp - 1
		entry := entryStack[top]
		if arrayStack[top] {
			if c == ',' {
				countStack[top]++
				i = skipSpaceFast(base, n, i+1)
				if i >= n {
					return tapeParseInvalid
				}
				goto value
			}
			if c != ']' {
				return tapeParseInvalid
			}
		} else {
			if c == ',' {
				countStack[top]++
				i, c = nextSignificantFast(base, n, i+1)
				goto objectKey
			}
			if c != '}' {
				return tapeParseInvalid
			}
		}
		count := countStack[top] + 1
		if count > infoMaxCount {
			return tapeParseInvalid
		}
		b.entries[entry].end = uint32(i + 1)
		b.entries[entry].setCount(count)
		b.entries[entry].next = uint32(len(b.entries)) - entry
		i++
		sp--
		goto scopeEnd
	}
}

// numberFlags returns the tape flags for a number whose plain-integer
// classification the scanner just reported.
func numberFlags(integer bool) uint8 {
	if integer {
		return tapeFlagInt
	}
	return 0
}

// emitScalar records a scalar entry spanning [start,end).
func (b *tapeBuilder) emitScalar(start, end int, kind document.Kind, flags uint8) tapeParseStatus {
	if len(b.entries) == cap(b.entries) {
		return tapeParseFull
	}
	entry := len(b.entries)
	b.entries = b.entries[:entry+1]
	b.entries[entry] = IndexEntry{start: uint32(start), end: uint32(end), next: 1, info: packInfo(0, kind, flags)}
	return tapeParseOK
}

// parse is the diagnostic tape builder: it produces the same tape as
// walkFast, with exact error reporting and the caller's full maxDepth. The
// outer loop opens one value per iteration; the inner loop closes completed
// containers and advances their parents, with the scope stack threaded
// through the open containers' next words (pushContainer/finishContainer)
// instead of held in a side allocation.
func (b *tapeBuilder) parse() error {
	b.skipSpace()
	completed := false
	for {
		if !completed {
			kind, entry, err := b.value()
			if err != nil {
				return err
			}
			if kind != document.Array && kind != document.Object {
				completed = true
			} else {
				if b.sp >= b.maxDepth {
					return syntaxError(b.src, b.i-1, "maximum nesting depth exceeded")
				}
				b.pushContainer(entry)
				b.skipSpace()
				close := byte(']')
				if kind == document.Object {
					close = '}'
				}
				if b.i < len(b.src) && b.src[b.i] == close {
					b.i++
					b.finishContainer()
					completed = true
				} else {
					if kind == document.Object {
						if err := b.objectKey(); err != nil {
							return err
						}
					}
					continue
				}
			}
		}

		for completed {
			if b.sp == 0 {
				b.skipSpace()
				if b.i != len(b.src) {
					return syntaxError(b.src, b.i, "unexpected data after top-level value")
				}
				return nil
			}
			frame := &b.entries[b.parent]
			if frame.Count() == infoMaxCount {
				return document.ErrIndexTooLarge
			}
			frame.bumpCount()
			b.skipSpace()
			if b.i >= len(b.src) {
				if frame.Kind() == document.Array {
					return syntaxError(b.src, b.i, "unterminated array")
				}
				return syntaxError(b.src, b.i, "unterminated object")
			}
			if frame.Kind() == document.Array {
				switch b.src[b.i] {
				case ',':
					b.i++
					completed = false
				case ']':
					b.i++
					b.finishContainer()
				default:
					return syntaxError(b.src, b.i, "expected comma or closing bracket in array")
				}
			} else {
				switch b.src[b.i] {
				case ',':
					b.i++
					if err := b.objectKey(); err != nil {
						return err
					}
					completed = false
				case '}':
					b.i++
					b.finishContainer()
				default:
					return syntaxError(b.src, b.i, "expected comma or closing brace in object")
				}
			}
		}
	}
}

// value parses one value's opening token at the cursor: scalars are emitted
// complete, containers as still-open headers whose entry number the caller
// pushes on the scope stack.
func (b *tapeBuilder) value() (document.Kind, int, error) {
	b.skipSpace()
	if b.i >= len(b.src) {
		return document.Invalid, 0, syntaxError(b.src, b.i, "expected value")
	}
	start := b.i
	switch b.src[b.i] {
	case 'n':
		if !matchStringAt(b.src, b.i, "null") {
			return document.Invalid, 0, syntaxError(b.src, b.i, "invalid literal")
		}
		b.i += 4
		return b.scalar(document.Null, start, 0)
	case 't':
		if !matchStringAt(b.src, b.i, "true") {
			return document.Invalid, 0, syntaxError(b.src, b.i, "invalid literal")
		}
		b.i += 4
		return b.scalar(document.Bool, start, 0)
	case 'f':
		if !matchStringAt(b.src, b.i, "false") {
			return document.Invalid, 0, syntaxError(b.src, b.i, "invalid literal")
		}
		b.i += 5
		return b.scalar(document.Bool, start, 0)
	case '"':
		end, escaped, err := b.string()
		if err != nil {
			return document.Invalid, 0, err
		}
		flags := uint8(0)
		if escaped {
			flags |= tapeFlagEscaped
		}
		return b.scalarAt(document.String, start, end, flags)
	case '[':
		b.i++
		entry, err := b.add(IndexEntry{start: uint32(start), info: packInfo(0, document.Array, 0)})
		return document.Array, entry, err
	case '{':
		b.i++
		entry, err := b.add(IndexEntry{start: uint32(start), info: packInfo(0, document.Object, 0)})
		return document.Object, entry, err
	default:
		if fastByteAt(b.base, b.i) != '-' && !isDigit(fastByteAt(b.base, b.i)) {
			return document.Invalid, 0, syntaxError(b.src, b.i, "unexpected byte while parsing value")
		}
		end, integer, ok := scanNumberFastTagged(b.base, len(b.src), b.i)
		if !ok {
			_, msg := scanNumber(b.src, b.i)
			return document.Invalid, 0, syntaxError(b.src, start, msg)
		}
		b.i = end
		return b.scalar(document.Number, start, numberFlags(integer))
	}
}

// scalar emits a scalar entry ending at the cursor.
func (b *tapeBuilder) scalar(kind document.Kind, start int, flags uint8) (document.Kind, int, error) {
	return b.scalarAt(kind, start, b.i, flags)
}

// scalarAt emits a complete scalar entry spanning [start, end).
func (b *tapeBuilder) scalarAt(kind document.Kind, start, end int, flags uint8) (document.Kind, int, error) {
	entry, err := b.add(IndexEntry{start: uint32(start), end: uint32(end), next: 1, info: packInfo(0, kind, flags)})
	return kind, entry, err
}

// objectKey parses one member key string and its colon, emitting the key
// entry, and leaves the cursor at the member value.
func (b *tapeBuilder) objectKey() error {
	b.skipSpace()
	if b.i >= len(b.src) || b.src[b.i] != '"' {
		return syntaxError(b.src, b.i, "expected object key string")
	}
	start := b.i
	end, escaped, err := b.string()
	if err != nil {
		return err
	}
	flags := uint8(tapeFlagKey)
	if escaped {
		flags |= tapeFlagEscaped
	}
	if _, err := b.add(IndexEntry{start: uint32(start), end: uint32(end), next: 1, info: packInfo(0, document.String, flags)}); err != nil {
		return err
	}
	b.skipSpace()
	if b.i >= len(b.src) || b.src[b.i] != ':' {
		return syntaxError(b.src, b.i, "expected colon after object key")
	}
	b.i++
	return nil
}

// string scans the string starting at the cursor, preferring the vector
// scanner and deferring to the diagnostic scanner for the exact error.
func (b *tapeBuilder) string() (end int, escaped bool, err error) {
	end, escaped, ok := scanJSONStringFast(b.src, b.base, b.i, len(b.src) <= 64)
	if ok {
		b.i = end
		return end, escaped, nil
	}
	s := rawSeeker{src: b.src, i: b.i, maxDepth: b.maxDepth}
	_, _, escaped, err = s.parseStringRaw()
	if err != nil {
		return 0, false, err
	}
	b.i = s.i
	return b.i, escaped, nil
}

// add appends one entry within the caller's storage capacity.
func (b *tapeBuilder) add(entry IndexEntry) (int, error) {
	if len(b.entries) == cap(b.entries) {
		return 0, document.ErrIndexFull
	}
	index := len(b.entries)
	b.entries = b.entries[:index+1]
	b.entries[index] = entry
	return index, nil
}

// finishContainer closes the innermost open container: it pops the scope
// stack from the header's next word and overwrites that word with the
// subtree size, completing the entry (rewrite 1 -> 2 of the next-word story
// in the file comment).
func (b *tapeBuilder) finishContainer() {
	entry := b.parent
	e := &b.entries[entry]
	b.parent = e.next
	b.sp--
	e.end = uint32(b.i)
	e.next = uint32(len(b.entries)) - entry
}

// pushContainer opens a container: the header's next word temporarily holds
// the previous scope head, forming the linked stack finishContainer pops.
func (b *tapeBuilder) pushContainer(entry int) {
	b.entries[entry].next = b.parent
	b.parent = uint32(entry)
	b.sp++
}

// skipSpace advances the cursor past insignificant whitespace.
func (b *tapeBuilder) skipSpace() {
	b.i = skipSpaceFast(b.base, len(b.src), b.i)
}
