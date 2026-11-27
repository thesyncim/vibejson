package vibejson

import (
	"encoding/binary"
	"math/bits"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/scanner"
)

// The structural index is a fixed-width tape over validated source bytes.
// Entries store a source span, skip count, and packed metadata.

// Flags apply only to their associated entry kinds.
const (
	tapeFlagEscaped = document.TapeFlagEscaped // string contains at least one escape sequence
	tapeFlagKey     = document.TapeFlagKey     // string is an object key
	tapeFlagInt     = document.TapeFlagInt     // number is a plain integer: optional minus, then digits only
)

// tapeFlagObjectKeysHashed marks an Object header whose key entries carry
// precomputed hashes. It reuses the string-only escaped bit.
const tapeFlagObjectKeysHashed = document.TapeFlagObjectKeysHashed

// Info word bitfield layout constants.
const (
	infoCountBits  = document.InfoCountBits
	infoKindBits   = document.InfoKindBits
	infoCountMask  = document.InfoCountMask
	infoKindShift  = document.InfoKindShift
	infoKindMask   = document.InfoKindMask
	infoFlagsShift = document.InfoFlagsShift
	infoMaxCount   = document.InfoMaxCount
)

// IndexEntry is one compact structural entry. Its offsets and metadata must not
// be modified while the Index or a derived Node is in use.
type IndexEntry = document.IndexEntry

// packInfo composes an info word from its parts. The caller guarantees count
// fits in infoCountBits; the builders check this before an entry is written.
func packInfo(count uint32, kind document.Kind, flags uint8) uint32 {
	return document.PackInfo(count, kind, flags)
}

// Index is an immutable, zero-copy navigation index over validated JSON.
// It aliases both source and entry storage, which must remain immutable.
type Index struct {
	Src     []byte
	Entries []IndexEntry
}

// buildIndexOptions routes bitmap, fast, and diagnostic builders.
func buildIndexOptions(Src []byte, storage []IndexEntry, opts document.IndexOptions) (Index, error) {
	if uint64(len(Src)) > uint64(^uint32(0)) || uint64(cap(storage)) > uint64(^uint32(0)) {
		return Index{}, document.ErrIndexTooLarge
	}
	maxDepth := maxDepthOrDefault(opts.MaxDepth)
	// The position engine only accepts eligible large documents; the portable
	// builder supplies the exact fallback error.
	fallbackNumberMode := uint8(tapeNumberScalar)
	if maxDepth >= fastWalkMaxDepth &&
		len(Src) >= validBitmapMinBytes && len(Src) < indexBitmapMaxBytes {
		Entries, mode, ok := buildIndexPositions(Src, storage)
		if ok {
			index := Index{Src: Src, Entries: Entries}
			if opts.HashKeys {
				enrichKeyHashes(&index)
			}
			return index, nil
		}
		fallbackNumberMode = mode
	}
	b := tapeBuilder{
		Src:      Src,
		Entries:  storage[:0],
		Parent:   noTapeParent,
		MaxDepth: maxDepth,
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
		b.Entries = storage[:0]
		b.I = 0
		b.sp = 0
		b.Parent = noTapeParent
		if err := b.parse(); err != nil {
			return Index{}, err
		}
	}
	index := Index{Src: Src, Entries: b.Entries}
	if opts.HashKeys {
		enrichKeyHashes(&index)
	}
	return index, nil
}

// RequiredIndexEntries validates src and returns the exact storage length for
// BuildIndex. It performs a complete counting pass.
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
	return nodeFromEntries(t.Src, t.Entries)
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

// tapeBuilder holds source, caller-provided entries, and parser state.
type tapeBuilder struct {
	Src      []byte
	Entries  []IndexEntry
	Parent   uint32
	I        int
	sp       int
	MaxDepth int
}

// noTapeParent marks the scope stack empty: no container is open.
const noTapeParent uint32 = ^uint32(0)

// Number modes select the digit scanner used by the portable walk.
const (
	tapeNumberScalar uint8 = iota
	tapeNumberSWAR
)

// tapeParseStatus is the fast builder's success, fallback, or storage verdict.
type tapeParseStatus uint8

const (
	// tapeParseOK reports that a fast tape walk completed successfully.
	tapeParseOK tapeParseStatus = iota
	tapeParseInvalid
	// tapeParseFull reports that caller-provided tape storage was exhausted.
	tapeParseFull
)

// parseFast is the iterative fast builder. Invalid or deep input falls back to
// the diagnostic parser.
func (b *tapeBuilder) parseFast() tapeParseStatus {
	b.skipSpace()
	if b.I >= len(b.Src) {
		return tapeParseInvalid
	}
	if status := b.WalkFast(); status != tapeParseOK {
		return status
	}
	b.skipSpace()
	if b.I != len(b.Src) {
		return tapeParseInvalid
	}
	return tapeParseOK
}

// stringFast records one string entry starting at the opening quote.
func (b *tapeBuilder) stringFast(start int, flags uint8) tapeParseStatus {
	src := b.Src
	scanStart := uint(start) + 1
	if scanStart+8 <= uint(len(src)) {
		w := src[scanStart : scanStart+8]
		if m := stringSpecialMask(binary.LittleEndian.Uint64(w)); m != 0 {
			off := uint(bits.TrailingZeros64(m)) >> 3
			j := scanStart + off
			if w[off] == '"' {
				if len(b.Entries) == cap(b.Entries) {
					return tapeParseFull
				}
				entry := len(b.Entries)
				b.Entries = b.Entries[:entry+1]
				b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: packInfo(0, document.String, flags)}
				b.I = int(j + 1)
				return tapeParseOK
			}
			scanStart = j
		} else {
			scanStart += 8
		}
	}
	end, escaped, ok := scanJSONStringFastFrom(src, int(scanStart))
	if !ok {
		return tapeParseInvalid
	}
	if escaped {
		flags |= tapeFlagEscaped
	}
	if len(b.Entries) == cap(b.Entries) {
		return tapeParseFull
	}
	entry := len(b.Entries)
	b.Entries = b.Entries[:entry+1]
	b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, document.String, flags)}
	b.I = end
	return tapeParseOK
}

// fastWalkMaxDepth bounds the fixed on-stack scope stack.
const fastWalkMaxDepth = 64

// WalkFast is the allocation-free iterative tape builder. It adapts the
// state-machine shape of simdjson's stage 2 walk; see LICENSE-SIMDJSON.
func (b *tapeBuilder) WalkFast() tapeParseStatus {
	src := b.Src
	n := uint(len(src))
	entIdx := uint(len(b.Entries))
	entCap := uint(cap(b.Entries))
	b.Entries = b.Entries[:entCap]
	entries := b.Entries

	var entryStack [fastWalkMaxDepth]uint32
	var countStack [fastWalkMaxDepth]uint32
	var arrayStack [fastWalkMaxDepth]bool
	sp := 0

	// Deeper input is handled by the diagnostic parser.
	depthLimit := b.MaxDepth
	if depthLimit > fastWalkMaxDepth {
		depthLimit = fastWalkMaxDepth
	}

	i := uint(b.I)
	var c byte

value:
	if i >= n {
		b.Entries = b.Entries[:entIdx]
		return tapeParseInvalid
	}
	switch ch := src[i]; ch {
	case '{':
		if sp >= depthLimit {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		entry := uint32(entIdx)
		entries[entIdx] = IndexEntry{Start: uint32(i), Info: packInfo(0, document.Object, 0)}
		entIdx++
		i++
		if i < n && src[i] > ' ' {
			c = src[i]
		} else {
			ni, nc := nextSignificantFast(src, int(i))
			i, c = uint(ni), nc
		}
		if c == '}' {
			entries[entry].End = uint32(i + 1)
			entries[entry].Next = uint32(entIdx) - entry
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
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		entry := uint32(entIdx)
		entries[entIdx] = IndexEntry{Start: uint32(i), Info: packInfo(0, document.Array, 0)}
		entIdx++
		i++
		if i < n && src[i] > ' ' {
			c = src[i]
		} else {
			ni, nc := nextSignificantFast(src, int(i))
			i, c = uint(ni), nc
		}
		if i >= n {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if c == ']' {
			entries[entry].End = uint32(i + 1)
			entries[entry].Next = uint32(entIdx) - entry
			i++
			goto scopeEnd
		}
		entryStack[sp] = entry
		countStack[sp] = 0
		arrayStack[sp] = true
		sp++
		goto value
	case '"':
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		start := i
		scanStart := i + 1
		if scanStart+16 <= n {
			w0 := src[scanStart : scanStart+8]
			if m0 := stringSpecialMask(binary.LittleEndian.Uint64(w0)); m0 != 0 {
				off := uint(bits.TrailingZeros64(m0)) >> 3
				j := scanStart + off
				if w0[off] == '"' {
					entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: packInfo(0, document.String, 0)}
					entIdx++
					i = j + 1
					goto scopeEnd
				}
				scanStart = j
			} else {
				w1 := src[scanStart+8 : scanStart+16]
				if m1 := stringSpecialMask(binary.LittleEndian.Uint64(w1)); m1 != 0 {
					off := uint(bits.TrailingZeros64(m1)) >> 3
					j := scanStart + 8 + off
					if w1[off] == '"' {
						entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: packInfo(0, document.String, 0)}
						entIdx++
						i = j + 1
						goto scopeEnd
					}
					scanStart = j
				} else {
					scanStart += 16
					var endIdx int
					if n-scanStart >= 32 {
						endIdx = scanner.IndexStringSpecialLong(src, int(scanStart))
					} else {
						endIdx = scanStringSpecial(src, int(scanStart))
					}
					if uint(endIdx) < n && src[uint(endIdx)] == '"' {
						entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(endIdx + 1), Next: 1, Info: packInfo(0, document.String, 0)}
						entIdx++
						i = uint(endIdx + 1)
						goto scopeEnd
					}
					scanStart = uint(endIdx)
				}
			}
		} else if scanStart+8 <= n {
			w := src[scanStart : scanStart+8]
			if m := stringSpecialMask(binary.LittleEndian.Uint64(w)); m != 0 {
				off := uint(bits.TrailingZeros64(m)) >> 3
				j := scanStart + off
				if w[off] == '"' {
					entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: packInfo(0, document.String, 0)}
					entIdx++
					i = j + 1
					goto scopeEnd
				}
				scanStart = j
			} else {
				scanStart += 8
			}
		}
		end, escaped, ok := scanJSONStringFastFrom(src, int(scanStart))
		if !ok {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		var flags uint8
		if escaped {
			flags = tapeFlagEscaped
		}
		entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, document.String, flags)}
		entIdx++
		i = uint(end)
		goto scopeEnd
	case 't':
		if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordTrueLE {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		entries[entIdx] = IndexEntry{Start: uint32(i), End: uint32(i + 4), Next: 1, Info: packInfo(0, document.Bool, 0)}
		entIdx++
		i += 4
		goto scopeEnd
	case 'f':
		if i+5 > n || binary.LittleEndian.Uint32(src[i+1:i+5]) != wordAlseLE {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		entries[entIdx] = IndexEntry{Start: uint32(i), End: uint32(i + 5), Next: 1, Info: packInfo(0, document.Bool, 0)}
		entIdx++
		i += 5
		goto scopeEnd
	case 'n':
		if i+4 > n || binary.LittleEndian.Uint32(src[i:i+4]) != wordNullLE {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		entries[entIdx] = IndexEntry{Start: uint32(i), End: uint32(i + 4), Next: 1, Info: packInfo(0, document.Null, 0)}
		entIdx++
		i += 4
		goto scopeEnd
	default:
		if entIdx >= entCap {
			b.Entries = b.Entries[:entIdx]
			return tapeParseFull
		}
		start := i
		if ch == '-' {
			i++
			if i >= n {
				b.Entries = b.Entries[:entIdx]
				return tapeParseInvalid
			}
			ch = src[i]
		}
		if isOneNine(ch) {
			j := i + 1
			for j < n && isDigit(src[j]) {
				j++
			}
			if j == n {
				entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j), Next: 1, Info: packInfo(0, document.Number, tapeFlagInt)}
				entIdx++
				i = j
				goto scopeEnd
			}
			nc := src[j]
			if nc != '.' && nc != 'e' && nc != 'E' {
				entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j), Next: 1, Info: packInfo(0, document.Number, tapeFlagInt)}
				entIdx++
				i = j
				goto scopeEnd
			}
			if nc == '.' && j+1 < n && isDigit(src[j+1]) {
				j += 2
				for j < n && isDigit(src[j]) {
					j++
				}
				if j == n || (src[j] != 'e' && src[j] != 'E') {
					entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j), Next: 1, Info: packInfo(0, document.Number, 0)}
					entIdx++
					i = j
					goto scopeEnd
				}
			} else if (nc == 'e' || nc == 'E') && j+1 < n && isDigit(src[j+1]) {
				j += 2
				for j < n && isDigit(src[j]) {
					j++
				}
				entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j), Next: 1, Info: packInfo(0, document.Number, 0)}
				entIdx++
				i = j
				goto scopeEnd
			}
		} else if ch == '0' {
			j := i + 1
			if j == n || (src[j] != '.' && src[j] != 'e' && src[j] != 'E' && !isDigit(src[j])) {
				entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j), Next: 1, Info: packInfo(0, document.Number, tapeFlagInt)}
				entIdx++
				i = j
				goto scopeEnd
			}
		} else {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		end, integer, ok := scanNumberFastTagged(src, int(start))
		if !ok {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, document.Number, numberFlags(integer))}
		entIdx++
		i = uint(end)
		goto scopeEnd
	}

	// objectKey consumes a quoted key and its colon, then falls into value to
	// read the member value. c holds the byte at i.
objectKey:
	if c != '"' {
		b.Entries = b.Entries[:entIdx]
		return tapeParseInvalid
	}
	if entIdx >= entCap {
		b.Entries = b.Entries[:entIdx]
		return tapeParseFull
	}
	{
		start := i
		scanStart := i + 1
		if scanStart+8 <= n {
			w := src[scanStart : scanStart+8]
			if m := stringSpecialMask(binary.LittleEndian.Uint64(w)); m != 0 {
				off := uint(bits.TrailingZeros64(m)) >> 3
				j := scanStart + off
				if w[off] == '"' {
					entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(j + 1), Next: 1, Info: packInfo(0, document.String, tapeFlagKey)}
					entIdx++
					i = j + 1
					if off < 7 && w[off+1] == ':' {
						i++
						if i < n && src[i] > ' ' {
							goto value
						}
						i = uint(skipSpaceFast(src, int(i)))
						if i >= n {
							b.Entries = b.Entries[:entIdx]
							return tapeParseInvalid
						}
						goto value
					}
					goto keyColon
				}
				scanStart = j
			} else {
				scanStart += 8
			}
		}
		end, escaped, ok := scanJSONStringFastFrom(src, int(scanStart))
		if !ok {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		flags := uint8(tapeFlagKey)
		if escaped {
			flags |= tapeFlagEscaped
		}
		entries[entIdx] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, document.String, flags)}
		entIdx++
		i = uint(end)
	}
keyColon:
	if i < n && src[i] == ':' {
		i++
		if i < n && src[i] > ' ' {
			goto value
		}
		i = uint(skipSpaceFast(src, int(i)))
		if i >= n {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		goto value
	}
	{
		ni, nc := nextSignificantFast(src, int(i))
		i, c = uint(ni), nc
	}
	if c != ':' {
		b.Entries = b.Entries[:entIdx]
		return tapeParseInvalid
	}
	i = uint(skipSpaceFast(src, int(i+1)))
	if i >= n {
		b.Entries = b.Entries[:entIdx]
		return tapeParseInvalid
	}
	goto value

	// scopeEnd runs after a complete value ending at i. With no scope open the
	// document's root value is done; otherwise it advances the innermost
	// container, either to its next member or past its closing bracket.
scopeEnd:
	if sp == 0 {
		b.Entries = b.Entries[:entIdx]
		b.I = int(i)
		return tapeParseOK
	}
	if i < n && src[i] > ' ' {
		c = src[i]
	} else {
		ni, nc := nextSignificantFast(src, int(i))
		i, c = uint(ni), nc
	}
	{
		top := sp - 1
		entry := entryStack[top]
		if arrayStack[top] {
			if c == ',' {
				countStack[top]++
				i++
				if i < n && src[i] > ' ' {
					goto value
				}
				i = uint(skipSpaceFast(src, int(i)))
				if i >= n {
					b.Entries = b.Entries[:entIdx]
					return tapeParseInvalid
				}
				goto value
			}
			if c != ']' {
				b.Entries = b.Entries[:entIdx]
				return tapeParseInvalid
			}
		} else {
			if c == ',' {
				countStack[top]++
				i++
				if i < n && src[i] > ' ' {
					c = src[i]
				} else {
					ni, nc := nextSignificantFast(src, int(i))
					i, c = uint(ni), nc
				}
				goto objectKey
			}
			if c != '}' {
				b.Entries = b.Entries[:entIdx]
				return tapeParseInvalid
			}
		}
		count := countStack[top] + 1
		if count > infoMaxCount {
			b.Entries = b.Entries[:entIdx]
			return tapeParseInvalid
		}
		entries[entry].End = uint32(i + 1)
		entries[entry].SetCount(count)
		entries[entry].Next = uint32(entIdx) - entry
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
	if len(b.Entries) == cap(b.Entries) {
		return tapeParseFull
	}
	entry := len(b.Entries)
	b.Entries = b.Entries[:entry+1]
	b.Entries[entry] = IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, kind, flags)}
	return tapeParseOK
}

// parse is the diagnostic tape builder: it produces the same tape as
// WalkFast, with exact error reporting and the caller's full maxDepth. The
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
			if frame.Count() == infoMaxCount {
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
func (b *tapeBuilder) value() (document.Kind, int, error) {
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
			flags |= tapeFlagEscaped
		}
		return b.scalarAt(document.String, start, end, flags)
	case '[':
		b.I++
		entry, err := b.add(IndexEntry{Start: uint32(start), Info: packInfo(0, document.Array, 0)})
		return document.Array, entry, err
	case '{':
		b.I++
		entry, err := b.add(IndexEntry{Start: uint32(start), Info: packInfo(0, document.Object, 0)})
		return document.Object, entry, err
	default:
		if b.Src[b.I] != '-' && !isDigit(b.Src[b.I]) {
			return document.Invalid, 0, syntaxError(b.Src, b.I, "unexpected byte while parsing value")
		}
		end, integer, ok := scanNumberFastTagged(b.Src, b.I)
		if !ok {
			_, msg := scanNumber(b.Src, b.I)
			return document.Invalid, 0, syntaxError(b.Src, start, msg)
		}
		b.I = end
		return b.scalar(document.Number, start, numberFlags(integer))
	}
}

// scalar emits a scalar entry ending at the cursor.
func (b *tapeBuilder) scalar(kind document.Kind, start int, flags uint8) (document.Kind, int, error) {
	return b.scalarAt(kind, start, b.I, flags)
}

// scalarAt emits a complete scalar entry spanning [start, end).
func (b *tapeBuilder) scalarAt(kind document.Kind, start, end int, flags uint8) (document.Kind, int, error) {
	entry, err := b.add(IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, kind, flags)})
	return kind, entry, err
}

// objectKey parses one member key string and its colon, emitting the key
// entry, and leaves the cursor at the member value.
func (b *tapeBuilder) objectKey() error {
	b.skipSpace()
	if b.I >= len(b.Src) || b.Src[b.I] != '"' {
		return syntaxError(b.Src, b.I, "expected object key string")
	}
	start := b.I
	end, escaped, err := b.string()
	if err != nil {
		return err
	}
	flags := uint8(tapeFlagKey)
	if escaped {
		flags |= tapeFlagEscaped
	}
	if _, err := b.add(IndexEntry{Start: uint32(start), End: uint32(end), Next: 1, Info: packInfo(0, document.String, flags)}); err != nil {
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
func (b *tapeBuilder) string() (end int, escaped bool, err error) {
	end, escaped, ok := scanJSONStringFast(b.Src, b.I, len(b.Src) <= 64)
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
func (b *tapeBuilder) add(entry IndexEntry) (int, error) {
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
func (b *tapeBuilder) finishContainer() {
	entry := b.Parent
	e := &b.Entries[entry]
	b.Parent = e.Next
	b.sp--
	e.End = uint32(b.I)
	e.Next = uint32(len(b.Entries)) - entry
}

// pushContainer opens a container: the header's next word temporarily holds
// the previous scope head, forming the linked stack finishContainer pops.
func (b *tapeBuilder) pushContainer(entry int) {
	b.Entries[entry].Next = b.Parent
	b.Parent = uint32(entry)
	b.sp++
}

// skipSpace advances the cursor past insignificant whitespace.
func (b *tapeBuilder) skipSpace() {
	b.I = skipSpaceFast(b.Src, b.I)
}
