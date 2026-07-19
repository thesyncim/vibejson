package simdjson

import (
	"encoding/binary"
	"math/bits"
	"strings"
	"unsafe"
)

func (cursor *decoderCursor) matchObjectFieldExpected(first bool, expected *typedField) bool {
	src := cursor.src
	i := cursor.i
	n := len(src)
	if i < n && src[i] <= ' ' {
		i = skipSpaceIndent(src, i)
	}
	if i >= n || expected.keyMask == 0 {
		return false
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	if first {
		if fastByteAt(base, i) != '"' {
			return false
		}
	} else {
		if fastByteAt(base, i) != ',' {
			return false
		}
		i++
		if i < n && src[i] <= ' ' {
			i = skipSpaceIndent(src, i)
		}
		if i >= n || fastByteAt(base, i) != '"' {
			return false
		}
	}

	keyStart := i + 1
	if keyStart+8 > n {
		return false
	}
	word := loadUint64LE(unsafe.Add(base, keyStart))
	if (word^expected.key)&expected.keyMask != 0 {
		return false
	}
	keyEnd := keyStart + int(expected.keyLen)
	if expected.keyLen <= 6 {
		cursor.i = keyEnd + 2
		if cursor.i < n && fastByteAt(base, cursor.i) <= ' ' {
			cursor.skipSpace()
		}
		return true
	}
	if keyEnd+2 > n || loadUint16LE(unsafe.Add(base, keyEnd)) != quoteColonLE {
		return false
	}
	if expected.keyLen > 7 && !matchStringAt(src, keyStart+8, expected.name[8:]) {
		return false
	}
	cursor.i = keyEnd + 2
	if cursor.i < n && fastByteAt(base, cursor.i) <= ' ' {
		cursor.skipSpace()
	}
	return true
}

//go:noinline
func (cursor *decoderCursor) nextObjectFieldExpectedSlow(first bool, expected *typedField) (key string, matched, ok bool, err error) {
	i := cursor.i
	if i < len(cursor.src) && cursor.src[i] <= ' ' {
		i = cursor.skipSpaceAt(i)
	}
	if i >= len(cursor.src) {
		key, ok, err = cursor.NextObjectField(first)
		return key, false, ok, err
	}
	if first {
		if cursor.src[i] == '}' {
			cursor.i = i + 1
			cursor.depth--
			return "", false, false, nil
		}
		if cursor.src[i] != '"' {
			key, ok, err = cursor.NextObjectField(first)
			return key, false, ok, err
		}
	} else {
		switch cursor.src[i] {
		case '}':
			cursor.i = i + 1
			cursor.depth--
			return "", false, false, nil
		case ',':
			i++
			if i < len(cursor.src) && cursor.src[i] <= ' ' {
				i = cursor.skipSpaceAt(i)
			}
			if i >= len(cursor.src) || cursor.src[i] != '"' {
				key, ok, err = cursor.NextObjectField(first)
				return key, false, ok, err
			}
		default:
			key, ok, err = cursor.NextObjectField(first)
			return key, false, ok, err
		}
	}

	keyStart := i + 1
	if keyStart+8 > len(cursor.src) {
		key, ok, err = cursor.NextObjectField(first)
		return key, false, ok, err
	}
	word := binary.LittleEndian.Uint64(cursor.src[keyStart:])
	if mask := expected.keyMask; mask != 0 {
		// The masked compare covers the key bytes and the closing quote. When
		// the exact compare misses and folding applies, retry with the ASCII
		// case bits of the key's letters masked out.
		diff := (word ^ expected.key) & mask
		foldedHead := diff != 0
		if diff != 0 && diff&^expected.keyFold == 0 && cursor.flags&decoderCaseSensitive == 0 {
			diff = 0
		}
		if diff == 0 {
			keyEnd := keyStart + int(expected.keyLen)
			if expected.keyLen <= 6 {
				cursor.i = keyEnd + 2
				if cursor.i < len(cursor.src) && cursor.src[cursor.i] <= ' ' {
					cursor.skipSpace()
				}
				return "", true, true, nil
			}
			matchedName := expected.keyLen <= 7
			if !matchedName && keyEnd+1 < len(cursor.src) && cursor.src[keyEnd] == '"' {
				matchedName = !foldedHead && matchStringAt(cursor.src, keyStart+8, expected.name[8:])
				if !matchedName && cursor.flags&decoderCaseSensitive == 0 {
					actual := unsafe.String(unsafe.SliceData(cursor.src[keyStart:keyEnd]), keyEnd-keyStart)
					matchedName = strings.EqualFold(actual, expected.name)
				}
			}
			if matchedName && keyEnd+1 < len(cursor.src) && cursor.src[keyEnd+1] == ':' {
				cursor.i = keyEnd + 2
				if cursor.i < len(cursor.src) && cursor.src[cursor.i] <= ' ' {
					cursor.skipSpace()
				}
				return "", true, true, nil
			}
		}
	}
	special := stringSpecialMask(word)
	keyEnd := keyStart
	if special != 0 {
		keyEnd += bits.TrailingZeros64(special) / 8
	} else {
		keyEnd = scanStringSpecial(cursor.src, keyStart+8)
	}
	if keyEnd >= len(cursor.src) || cursor.src[keyEnd] != '"' || keyEnd+1 >= len(cursor.src) || cursor.src[keyEnd+1] != ':' {
		key, ok, err = cursor.NextObjectField(first)
		return key, false, ok, err
	}
	cursor.i = keyEnd + 2
	if cursor.i < len(cursor.src) && cursor.src[cursor.i] <= ' ' {
		cursor.skipSpace()
	}
	keyLen := keyEnd - keyStart
	key = unsafe.String(unsafe.SliceData(cursor.src[keyStart:keyEnd]), keyLen)
	return key, false, true, nil
}

func resetMissingTypedFields(node *typedNode, dst unsafe.Pointer, seen uint64) {
	if seen == node.allSet || node.allSet == 0 {
		return
	}
	missing := node.allSet &^ seen
	for i := range node.fields {
		field := &node.fields[i]
		if missing&field.seen == 0 {
			continue
		}
		target := dst
		if field.hop >= 0 {
			target = resolveResetHops(dst, node.fieldHops[field.hop])
			if target == nil {
				continue
			}
		}
		resetTyped(field.node, unsafe.Add(target, field.offset))
	}
}

// nextObjectFieldRawStructural is the escaped-key fallback. The raw key parser
// must see the colon that the structural tape deliberately omits, so it uses
// ordinary whitespace scanning and then realigns the tape on the value start.
//
//go:noinline
func (cursor *decoderCursor) nextObjectFieldRawStructural(first bool, field *typedField) (key string, matched, ok bool, err error) {
	structuralActive := cursor.state.structuralActive
	cursor.state.structuralActive = false
	if field != nil {
		key, matched, ok, err = cursor.nextObjectFieldExpectedSlow(first, field)
	} else {
		key, ok, err = cursor.NextObjectField(first)
	}
	cursor.state.structuralActive = structuralActive
	if err == nil && ok && structuralActive {
		_ = cursor.state.structural.position(cursor.i, len(cursor.src))
	}
	return key, matched, ok, err
}
