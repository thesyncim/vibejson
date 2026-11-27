package vibejson

import (
	"encoding/binary"

	"github.com/thesyncim/vibejson/document"
)

func (b *tapeBuilder) parseFastSWAR() tapeParseStatus {
	b.skipSpace()
	if b.I >= len(b.Src) {
		return tapeParseInvalid
	}
	if status := b.walkFastSWAR(); status != tapeParseOK {
		return status
	}
	b.skipSpace()
	if b.I != len(b.Src) {
		return tapeParseInvalid
	}
	return tapeParseOK
}

// walkFastSWAR is the long-integer specialization of WalkFast. BuildIndex
// selects it once per document from the stage-1 sample, keeping the scalar
// walker's per-number dispatch unchanged.
func (b *tapeBuilder) walkFastSWAR() tapeParseStatus {
	src := b.Src
	n := len(src)

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
	switch src[i] {
	case '{':
		if sp >= depthLimit {
			return tapeParseInvalid
		}
		if len(b.Entries) == cap(b.Entries) {
			return tapeParseFull
		}
		entry := uint32(len(b.Entries))
		b.Entries = b.Entries[:entry+1]
		b.Entries[entry] = IndexEntry{Start: uint32(i), Info: packInfo(0, document.Object, 0)}
		i, c = nextSignificantFast(src, i+1)
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
			return tapeParseFull
		}
		entry := uint32(len(b.Entries))
		b.Entries = b.Entries[:entry+1]
		b.Entries[entry] = IndexEntry{Start: uint32(i), Info: packInfo(0, document.Array, 0)}
		i, c = nextSignificantFast(src, i+1)
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
		if status := b.stringFast(i, 0); status != tapeParseOK {
			return status
		}
		i = b.I
		goto scopeEnd
	case 't':
		if n-i < 4 || binary.LittleEndian.Uint32(src[i:]) != wordTrueLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Bool, 0); status != tapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	case 'f':
		if n-i < 5 || binary.LittleEndian.Uint32(src[i+1:]) != wordAlseLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+5, document.Bool, 0); status != tapeParseOK {
			return status
		}
		i += 5
		goto scopeEnd
	case 'n':
		if n-i < 4 || binary.LittleEndian.Uint32(src[i:]) != wordNullLE {
			return tapeParseInvalid
		}
		if status := b.emitScalar(i, i+4, document.Null, 0); status != tapeParseOK {
			return status
		}
		i += 4
		goto scopeEnd
	default:
		ch := src[i]
		if ch != '-' && !isDigit(ch) {
			return tapeParseInvalid
		}
		end, integer, ok := scanNumberFastTaggedSWAR(src, i)
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
	i, c = nextSignificantFast(src, b.I)
	if c != ':' {
		return tapeParseInvalid
	}
	i = skipSpaceFast(src, i+1)
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
		return tapeParseOK
	}
	{
		i, c = nextSignificantFast(src, i)
		top := sp - 1
		entry := entryStack[top]
		if arrayStack[top] {
			if c == ',' {
				countStack[top]++
				i = skipSpaceFast(src, i+1)
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
				i, c = nextSignificantFast(src, i+1)
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
