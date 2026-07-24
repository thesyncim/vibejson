package vibejson

import (
	"unsafe"

	"github.com/thesyncim/vibejson/document"
)

func (b *TapeBuilder) parseFastSWAR() tapeParseStatus {
	b.skipSpace()
	if b.I >= len(b.Src) {
		return tapeParseInvalid
	}
	if status := b.walkFastSWAR(); status != TapeParseOK {
		return status
	}
	b.skipSpace()
	if b.I != len(b.Src) {
		return tapeParseInvalid
	}
	return TapeParseOK
}

// walkFastSWAR is the long-integer specialization of WalkFast. BuildIndex
// selects it once per document from the stage-1 sample, keeping the scalar
// walker's per-number dispatch unchanged.
func (b *TapeBuilder) walkFastSWAR() tapeParseStatus {
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
		if i+4 > n || loadUint32LE(unsafe.Add(base, i)) != wordTrueLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Bool, 0); status != TapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	case 'f':
		if i+5 > n || loadUint32LE(unsafe.Add(base, i+1)) != wordAlseLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+5, document.Bool, 0); status != TapeParseOK {
			return status
		}
		i += 5
		goto scopeEnd
	case 'n':
		if i+4 > n || loadUint32LE(unsafe.Add(base, i)) != wordNullLE {
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
		end, integer, ok := scanNumberFastTaggedSWAR(base, n, i)
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
