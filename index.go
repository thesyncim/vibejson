package vibejson

import (
	"math/bits"
	"unsafe"

	"github.com/thesyncim/vibejson/document"
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
// never move (intern.go, segment.go, shape.go); a "shape" is a compiled
// flat-object layout (shape.go); a "shape tape" is a document stored as
// value entries only, its keys deduplicated into the shape
// (segment_shape.go).

// Each flag qualifies one kind and is zero elsewhere: escaped and key apply to
// strings, integer to numbers.
const (
	TapeFlagEscaped = 1 << iota // string contains at least one escape sequence
	TapeFlagKey                 // string is an object key
	TapeFlagInt                 // number is a plain integer: optional minus, then digits only
)

// TapeFlagObjectKeysHashed marks, on an Object header entry only, that the
// object's key entries carry precomputed content hashes in their next word
// (see enrichKeyHashes and Node.Get). It reuses the escaped bit position: no
// accessor interprets the escaped, key, or integer bit for an Object kind, so
// the bit is free to repurpose there. The integer bit is deliberately avoided
// because IsInteger tests it without a prior kind check.
const TapeFlagObjectKeysHashed = TapeFlagEscaped

// KeysHashed reports whether this Object header was enriched with per-key
// hashes. It is meaningful only on an Object entry.
func (e *IndexEntry) KeysHashed() bool {
	return e.Flags()&TapeFlagObjectKeysHashed != 0
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
	InfoCountBits         = 26
	InfoKindBits          = 3
	InfoCountMask  uint32 = 1<<InfoCountBits - 1
	InfoKindShift         = InfoCountBits
	InfoKindMask   uint32 = (1<<InfoKindBits - 1) << InfoKindShift
	InfoFlagsShift        = InfoCountBits + InfoKindBits
	InfoMaxCount   uint32 = InfoCountMask
)

// IndexEntry is one compact structural entry in an Index. Its fields are private
// so callers can provide reusable storage without being coupled to the layout;
// kind, flags, and count share one packed word behind accessor methods, so the
// layout can change without touching every reader.
type IndexEntry struct {
	Start uint32
	End   uint32
	Next  uint32
	Info  uint32
}

// Kind returns the entry's JSON kind.
func (e *IndexEntry) Kind() document.Kind {
	return document.Kind((e.Info & InfoKindMask) >> InfoKindShift)
}

// Flags returns the entry's tape Flags (escaped, key, integer).
func (e *IndexEntry) Flags() uint8 {
	return uint8(e.Info >> InfoFlagsShift)
}

// Count returns a container's direct element count. It is meaningful only for
// arrays and objects; other kinds report zero.
func (e *IndexEntry) Count() uint32 {
	return e.Info & InfoCountMask
}

// PackInfo composes an info word from its parts. The caller guarantees count
// fits in infoCountBits; the builders check this before an entry is written.
func PackInfo(count uint32, kind document.Kind, flags uint8) uint32 {
	return count&InfoCountMask | uint32(kind)<<InfoKindShift | uint32(flags)<<InfoFlagsShift
}

// SetCount replaces the entry's element count, preserving kind and Flags.
func (e *IndexEntry) SetCount(count uint32) {
	e.Info = e.Info&^InfoCountMask | count&InfoCountMask
}

// BumpCount adds one to the entry's element count in place. count occupies the
// low bits of Info, so an increment cannot disturb kind or Flags unless it
// overflows the count field, which the builders prevent.
func (e *IndexEntry) BumpCount() {
	e.Info++
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
	Src     []byte
	Entries []IndexEntry
}

// buildIndexOptions is the engine router behind BuildIndex and
// BuildIndexOptions: bitmap engine, then fast walk, then diagnostic parse,
// per the routing rules in the file comment; enrichment runs last on
// whichever tape was accepted.
func buildIndexOptions(Src []byte, storage []IndexEntry, opts document.IndexOptions) (Index, error) {
	if uint64(len(Src)) > uint64(^uint32(0)) || uint64(cap(storage)) > uint64(^uint32(0)) {
		return Index{}, document.ErrIndexTooLarge
	}
	maxDepth := maxDepthOrDefault(opts.MaxDepth)
	// The position engine (index_positions.go) takes large documents. It only
	// shortcuts acceptance: any decline falls through to the portable builder
	// below, which decides the exact error. The depth gate keeps callers'
	// tighter limits with the builder that enforces them.
	fallbackNumberMode := uint8(tapeNumberScalar)
	if maxDepth >= fastWalkMaxDepth &&
		len(Src) >= ValidBitmapMinBytes && len(Src) < indexBitmapMaxBytes {
		if Entries, ok := buildIndexPositions(Src, storage); ok {
			index := Index{Src: Src, Entries: Entries}
			if opts.HashKeys {
				EnrichKeyHashes(&index)
			}
			return index, nil
		}
		fallbackNumberMode = indexFallbackNumberMode(Src)
	}
	b := TapeBuilder{
		Src:      Src,
		Base:     ByteSourceOf(Src).PointerAt(0),
		Entries:  storage[:0],
		Parent:   NoTapeParent,
		MaxDepth: maxDepth,
	}
	var status tapeParseStatus
	if fallbackNumberMode == tapeNumberSWAR {
		status = b.parseFastSWAR()
	} else {
		status = b.parseFast()
	}
	switch status {
	case TapeParseOK:
	case TapeParseFull:
		return Index{}, document.ErrIndexFull
	default:
		b.Entries = storage[:0]
		b.I = 0
		b.sp = 0
		b.Parent = NoTapeParent
		if err := b.parse(); err != nil {
			return Index{}, err
		}
	}
	index := Index{Src: Src, Entries: b.Entries}
	if opts.HashKeys {
		EnrichKeyHashes(&index)
	}
	return index, nil
}

// RequiredIndexEntries validates src and returns the exact storage length
// BuildIndex needs. Ordinary documents are counted without heap allocation.
//
// It is a complete second pass over src, not bookkeeping: counting a document
// costs about twice what building its tape does, so sizing-then-building
// triples the price of a build. Use it where a buffer is sized once — a
// caller that must hand back exactly sized storage, or a setup step outside a
// loop. Where documents are indexed repeatedly into retained storage, build
// directly into that storage and grow and retry on [document.ErrIndexFull]
// instead.
func RequiredIndexEntries(src []byte) (int, error) {
	l, err := countLayout(src, DefaultMaxDepth)
	if err != nil {
		return 0, err
	}
	return 1 + l.values + 2*l.members, nil
}

// Len returns the number of structural entries in the index.
func (t Index) Len() int {
	return len(t.Entries)
}

// Root returns the document's top-level node.
func (t Index) Root() Node {
	return NodeFromEntries(t.Src, t.Entries)
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

// A TapeBuilder holds the state shared by the two portable engines: the
// source, its base pointer for the read kernels, the destination entries
// (aliasing caller storage, extended only within its capacity), and the byte
// cursor i. parent and sp belong to the diagnostic engine: parent is the
// entry number of the innermost open container — the head of the scope stack
// threaded through container next words — and sp is the open depth, checked
// against maxDepth.
type TapeBuilder struct {
	Src      []byte
	Base     unsafe.Pointer
	Entries  []IndexEntry
	Parent   uint32
	I        int
	sp       int
	MaxDepth int
}

// NoTapeParent marks the scope stack empty: no container is open.
const NoTapeParent uint32 = ^uint32(0)

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
	// TapeParseOK reports that a fast tape walk completed successfully.
	TapeParseOK tapeParseStatus = iota
	tapeParseInvalid
	// TapeParseFull reports that caller-provided tape storage was exhausted.
	TapeParseFull
)

// parseFast is the happy-path tape builder: an iterative walk with an inline
// one-word fast path for short clean strings. It reports full or invalid input
// so BuildIndex can fall back to the diagnostic parser; it also defers any
// document nested past fastWalkMaxDepth to that parser, so the walk carries a
// small fixed scope stack instead of an unbounded one.
func (b *TapeBuilder) parseFast() tapeParseStatus {
	b.skipSpace()
	if b.I >= len(b.Src) {
		return tapeParseInvalid
	}
	if status := b.WalkFast(); status != TapeParseOK {
		return status
	}
	b.skipSpace()
	if b.I != len(b.Src) {
		return tapeParseInvalid
	}
	return TapeParseOK
}

// stringFast records one string entry starting at the opening quote.
func (b *TapeBuilder) stringFast(start int, flags uint8) tapeParseStatus {
	scanStart := start + 1
	if start+9 <= len(b.Src) {
		if m := stringSpecialMask(loadUint64LE(byteSourceFromPointer(b.Base).PointerAt(start + 1))); m != 0 {
			j := start + 1 + bits.TrailingZeros64(m)/8
			if b.Src[j] == '"' {
				if len(b.Entries) == cap(b.Entries) {
					return TapeParseFull
				}
				entry := len(b.Entries)
				b.Entries = b.Entries[:entry+1]
				b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: PackInfo(0, document.String, flags)}
				b.I = j + 1
				return TapeParseOK
			}
			scanStart = j
		} else {
			scanStart += 8
		}
	}
	end, escaped, ok := scanJSONStringFastFrom(b.Src, b.Base, scanStart)
	if !ok {
		return tapeParseInvalid
	}
	if escaped {
		flags |= TapeFlagEscaped
	}
	if len(b.Entries) == cap(b.Entries) {
		return TapeParseFull
	}
	entry := len(b.Entries)
	b.Entries = b.Entries[:entry+1]
	b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: PackInfo(0, document.String, flags)}
	b.I = end
	return TapeParseOK
}

// fastWalkMaxDepth bounds the container nesting the iterative walk handles
// inline. Its open-scope stack lives in one fixed on-stack frame so the walk
// stays allocation-free; the cap keeps that frame small. Anything deeper
// diverts to the diagnostic parser, which is bounded only by maxDepth.
const fastWalkMaxDepth = 64

// Provenance: CPP-WALK-001.
// WalkFast adapts the state-machine shape of C++ simdjson 4.6.4
// json_iterator::walk_document at commit
// 1bcf71bd85059ab6574ea1159de9298dcc1212c5,
// src/generic/stage2/json_iterator.h; Apache-2.0, see LICENSE-SIMDJSON. Local
// changes build a Go-owned tape, preserve exact error offsets, and fuse local
// primitive scanners.
//
// WalkFast is the iterative core of parseFast. It is a labeled state machine over an explicit stack of
// open containers, so each nested value is reached by a jump rather than a
// recursive call and its prologue. Each open scope records the container's
// entry index (to backpatch its span, count, and next once it closes), its
// running direct-member count, and whether it is an array; the byte at b.I on
// entry is the significant start of the document's root value.
//
// The token guards lean on nextSignificantFast reporting c==0 at end of input:
// that sentinel is not a structural byte, so a comparison against a real token
// rejects it without a separate length check. Guards that instead feed the
// position straight into a byte read keep an explicit i >= n check to stay in
// bounds.
func (b *TapeBuilder) WalkFast() tapeParseStatus {
	n := len(b.Src)
	base := b.Base

	var entryStack [fastWalkMaxDepth]uint32
	var countStack [fastWalkMaxDepth]uint32
	var arrayStack [fastWalkMaxDepth]bool
	sp := 0

	// Nesting past the stack, or past the caller's own limit, diverts to the
	// diagnostic parser, which enforces maxDepth and reports the error.
	depthLimit := b.MaxDepth
	if depthLimit > fastWalkMaxDepth {
		depthLimit = fastWalkMaxDepth
	}

	i := b.I
	var c byte

value:
	switch fastByteAt(base, i) {
	case '{':
		if sp >= depthLimit {
			return tapeParseInvalid
		}
		if len(b.Entries) == cap(b.Entries) {
			return TapeParseFull
		}
		entry := uint32(len(b.Entries))
		b.Entries = b.Entries[:entry+1]
		b.Entries[entry] = IndexEntry{Start: uint32(i), Info: PackInfo(0, document.Object, 0)}
		i, c = nextSignificantFast(base, n, i+1)
		if c == '}' {
			b.Entries[entry].End = uint32(i + 1)
			b.Entries[entry].Next = uint32(len(b.Entries)) - entry
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
		if len(b.Entries) == cap(b.Entries) {
			return TapeParseFull
		}
		entry := uint32(len(b.Entries))
		b.Entries = b.Entries[:entry+1]
		b.Entries[entry] = IndexEntry{Start: uint32(i), Info: PackInfo(0, document.Array, 0)}
		i, c = nextSignificantFast(base, n, i+1)
		if i >= n {
			// A non-empty array reads src[i] as its first value start below, so
			// the end-of-input position must be rejected before that read.
			return tapeParseInvalid
		}
		if c == ']' {
			b.Entries[entry].End = uint32(i + 1)
			b.Entries[entry].Next = uint32(len(b.Entries)) - entry
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
		if status := b.stringFast(i, 0); status != TapeParseOK {
			return status
		}
		i = b.I
		goto scopeEnd
	case 't':
		if i+4 > n || loadUint32LE(byteSourceFromPointer(base).PointerAt(i)) != wordTrueLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Bool, 0); status != TapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	case 'f':
		if i+5 > n || loadUint32LE(byteSourceFromPointer(base).PointerAt(i+1)) != wordAlseLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+5, document.Bool, 0); status != TapeParseOK {
			return status
		}
		i += 5
		goto scopeEnd
	case 'n':
		if i+4 > n || loadUint32LE(byteSourceFromPointer(base).PointerAt(i)) != wordNullLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Null, 0); status != TapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	default:
		ch := fastByteAt(base, i)
		if ch != '-' && !IsDigit(ch) {
			return tapeParseInvalid
		}
		end, integer, ok := scanNumberFastTagged(base, n, i)
		if !ok {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, end, document.Number, numberFlags(integer)); status != TapeParseOK {
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
	if status := b.stringFast(i, TapeFlagKey); status != TapeParseOK {
		return status
	}
	i, c = nextSignificantFast(base, n, b.I)
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
		b.I = i
		return TapeParseOK
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
		if count > InfoMaxCount {
			return tapeParseInvalid
		}
		b.Entries[entry].End = uint32(i + 1)
		b.Entries[entry].SetCount(count)
		b.Entries[entry].Next = uint32(len(b.Entries)) - entry
		i++
		sp--
		goto scopeEnd
	}
}

// numberFlags returns the tape flags for a number whose plain-integer
// classification the scanner just reported.
func numberFlags(integer bool) uint8 {
	if integer {
		return TapeFlagInt
	}
	return 0
}

// emitScalar records a scalar entry spanning [start,end).
func (b *TapeBuilder) emitScalar(start, end int, kind document.Kind, flags uint8) tapeParseStatus {
	if len(b.Entries) == cap(b.Entries) {
		return TapeParseFull
	}
	entry := len(b.Entries)
	b.Entries = b.Entries[:entry+1]
	b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: PackInfo(0, kind, flags)}
	return TapeParseOK
}

// parse is the diagnostic tape builder: it produces the same tape as
// WalkFast, with exact error reporting and the caller's full maxDepth. The
// outer loop opens one value per iteration; the inner loop closes completed
// containers and advances their parents, with the scope stack threaded
// through the open containers' next words (pushContainer/finishContainer)
// instead of held in a side allocation.
func (b *TapeBuilder) parse() error {
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
				if b.sp >= b.MaxDepth {
					return syntaxError(b.Src, b.I-1, "maximum nesting depth exceeded")
				}
				b.pushContainer(entry)
				b.skipSpace()
				close := byte(']')
				if kind == document.Object {
					close = '}'
				}
				if b.I < len(b.Src) && b.Src[b.I] == close {
					b.I++
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
				if b.I != len(b.Src) {
					return syntaxError(b.Src, b.I, "unexpected data after top-level value")
				}
				return nil
			}
			frame := &b.Entries[b.Parent]
			if frame.Count() == InfoMaxCount {
				return document.ErrIndexTooLarge
			}
			frame.BumpCount()
			b.skipSpace()
			if b.I >= len(b.Src) {
				if frame.Kind() == document.Array {
					return syntaxError(b.Src, b.I, "unterminated array")
				}
				return syntaxError(b.Src, b.I, "unterminated object")
			}
			if frame.Kind() == document.Array {
				switch b.Src[b.I] {
				case ',':
					b.I++
					completed = false
				case ']':
					b.I++
					b.finishContainer()
				default:
					return syntaxError(b.Src, b.I, "expected comma or closing bracket in array")
				}
			} else {
				switch b.Src[b.I] {
				case ',':
					b.I++
					if err := b.objectKey(); err != nil {
						return err
					}
					completed = false
				case '}':
					b.I++
					b.finishContainer()
				default:
					return syntaxError(b.Src, b.I, "expected comma or closing brace in object")
				}
			}
		}
	}
}

// value parses one value's opening token at the cursor: scalars are emitted
// complete, containers as still-open headers whose entry number the caller
// pushes on the scope stack.
func (b *TapeBuilder) value() (document.Kind, int, error) {
	b.skipSpace()
	if b.I >= len(b.Src) {
		return document.Invalid, 0, syntaxError(b.Src, b.I, "expected value")
	}
	start := b.I
	switch b.Src[b.I] {
	case 'n':
		if !matchStringAt(b.Src, b.I, "null") {
			return document.Invalid, 0, syntaxError(b.Src, b.I, "invalid literal")
		}
		b.I += 4
		return b.scalar(document.Null, start, 0)
	case 't':
		if !matchStringAt(b.Src, b.I, "true") {
			return document.Invalid, 0, syntaxError(b.Src, b.I, "invalid literal")
		}
		b.I += 4
		return b.scalar(document.Bool, start, 0)
	case 'f':
		if !matchStringAt(b.Src, b.I, "false") {
			return document.Invalid, 0, syntaxError(b.Src, b.I, "invalid literal")
		}
		b.I += 5
		return b.scalar(document.Bool, start, 0)
	case '"':
		end, escaped, err := b.string()
		if err != nil {
			return document.Invalid, 0, err
		}
		flags := uint8(0)
		if escaped {
			flags |= TapeFlagEscaped
		}
		return b.scalarAt(document.String, start, end, flags)
	case '[':
		b.I++
		entry, err := b.add(IndexEntry{Start: uint32(start), Info: PackInfo(0, document.Array, 0)})
		return document.Array, entry, err
	case '{':
		b.I++
		entry, err := b.add(IndexEntry{Start: uint32(start), Info: PackInfo(0, document.Object, 0)})
		return document.Object, entry, err
	default:
		if fastByteAt(b.Base, b.I) != '-' && !IsDigit(fastByteAt(b.Base, b.I)) {
			return document.Invalid, 0, syntaxError(b.Src, b.I, "unexpected byte while parsing value")
		}
		end, integer, ok := scanNumberFastTagged(b.Base, len(b.Src), b.I)
		if !ok {
			_, msg := scanNumber(b.Src, b.I)
			return document.Invalid, 0, syntaxError(b.Src, start, msg)
		}
		b.I = end
		return b.scalar(document.Number, start, numberFlags(integer))
	}
}

// scalar emits a scalar entry ending at the cursor.
func (b *TapeBuilder) scalar(kind document.Kind, start int, flags uint8) (document.Kind, int, error) {
	return b.scalarAt(kind, start, b.I, flags)
}

// scalarAt emits a complete scalar entry spanning [start, end).
func (b *TapeBuilder) scalarAt(kind document.Kind, start, end int, flags uint8) (document.Kind, int, error) {
	entry, err := b.add(IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: PackInfo(0, kind, flags)})
	return kind, entry, err
}

// objectKey parses one member key string and its colon, emitting the key
// entry, and leaves the cursor at the member value.
func (b *TapeBuilder) objectKey() error {
	b.skipSpace()
	if b.I >= len(b.Src) || b.Src[b.I] != '"' {
		return syntaxError(b.Src, b.I, "expected object key string")
	}
	start := b.I
	end, escaped, err := b.string()
	if err != nil {
		return err
	}
	flags := uint8(TapeFlagKey)
	if escaped {
		flags |= TapeFlagEscaped
	}
	if _, err := b.add(IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: PackInfo(0, document.String, flags)}); err != nil {
		return err
	}
	b.skipSpace()
	if b.I >= len(b.Src) || b.Src[b.I] != ':' {
		return syntaxError(b.Src, b.I, "expected colon after object key")
	}
	b.I++
	return nil
}

// string scans the string starting at the cursor, preferring the vector
// scanner and deferring to the diagnostic scanner for the exact error.
func (b *TapeBuilder) string() (end int, escaped bool, err error) {
	end, escaped, ok := scanJSONStringFast(b.Src, b.Base, b.I, len(b.Src) <= 64)
	if ok {
		b.I = end
		return end, escaped, nil
	}
	s := rawSeeker{src: b.Src, i: b.I, maxDepth: b.MaxDepth}
	_, _, escaped, err = s.parseStringRaw()
	if err != nil {
		return 0, false, err
	}
	b.I = s.i
	return b.I, escaped, nil
}

// add appends one entry within the caller's storage capacity.
func (b *TapeBuilder) add(entry IndexEntry) (int, error) {
	if len(b.Entries) == cap(b.Entries) {
		return 0, document.ErrIndexFull
	}
	index := len(b.Entries)
	b.Entries = b.Entries[:index+1]
	b.Entries[index] = entry
	return index, nil
}

// finishContainer closes the innermost open container: it pops the scope
// stack from the header's next word and overwrites that word with the
// subtree size, completing the entry (rewrite 1 -> 2 of the next-word story
// in the file comment).
func (b *TapeBuilder) finishContainer() {
	entry := b.Parent
	e := &b.Entries[entry]
	b.Parent = e.Next
	b.sp--
	e.End = uint32(b.I)
	e.Next = uint32(len(b.Entries)) - entry
}

// pushContainer opens a container: the header's next word temporarily holds
// the previous scope head, forming the linked stack finishContainer pops.
func (b *TapeBuilder) pushContainer(entry int) {
	b.Entries[entry].Next = b.Parent
	b.Parent = uint32(entry)
	b.sp++
}

// SkipSpace advances the cursor past insignificant whitespace.
func (b *TapeBuilder) skipSpace() {
	b.I = skipSpaceFast(b.Base, len(b.Src), b.I)
}
