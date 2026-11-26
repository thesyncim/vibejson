package vibejson

import (
	"encoding/binary"
	"unsafe"
)

var oneDigitFractions = [...]float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}

// shortStructuralFloatAt classifies the exact compact one-digit forms by
// their tape-proved width. Uncommon whitespace and wider forms return to the
// caller's full transactional decoder.
func shortStructuralFloatAt(src []byte, start, limit uint) (float64, bool) {
	if limit <= start || start+8 > uint(len(src)) {
		return 0, false
	}
	width := limit - start
	word := binary.LittleEndian.Uint64((*[8]byte)(src[start : start+8])[:])
	if width > 5 {
		switch {
		case byte(word>>8) <= ' ':
			width = 1
		case byte(word>>16) <= ' ':
			width = 2
		case byte(word>>24) <= ' ':
			width = 3
		case byte(word>>32) <= ' ':
			width = 4
		case byte(word>>40) <= ' ':
			width = 5
		default:
			return 0, false
		}
	}
	b0, b1 := byte(word), byte(word>>8)
	d0, d1 := b0-'0', b1-'0'
	switch width {
	case 1:
		if d0 <= 9 {
			return float64(d0), true
		}
	case 2:
		if b0 == '-' && d1 <= 9 {
			return -float64(d1), true
		}
	case 3:
		b2 := byte(word >> 16)
		d2 := b2 - '0'
		if d0 <= 9 && d2 <= 9 {
			switch {
			case b1 == '.':
				return float64(d0) + oneDigitFractions[d2], true
			case b1|0x20 == 'e':
				return float64(d0) * anyPow10[d2], true
			}
		}
	case 4:
		b2, b3 := byte(word>>16), byte(word>>24)
		d3 := b3 - '0'
		if b0 == '-' && d1 <= 9 && d3 <= 9 {
			switch {
			case b2 == '.':
				return -(float64(d1) + oneDigitFractions[d3]), true
			case b2|0x20 == 'e':
				return -float64(d1) * anyPow10[d3], true
			}
		}
		if d0 <= 9 && b1|0x20 == 'e' && (b2 == '+' || b2 == '-') && d3 <= 9 {
			value := float64(d0)
			if b2 == '-' {
				value /= anyPow10[d3]
			} else {
				value *= anyPow10[d3]
			}
			return value, true
		}
	case 5:
		b2, b3, b4 := byte(word>>16), byte(word>>24), byte(word>>32)
		d4 := b4 - '0'
		if b0 == '-' && d1 <= 9 && b2|0x20 == 'e' && (b3 == '+' || b3 == '-') && d4 <= 9 {
			value := float64(d1)
			if b3 == '-' {
				value /= anyPow10[d4]
			} else {
				value *= anyPow10[d4]
			}
			return -value, true
		}
	}
	return 0, false
}

func decodeCompiledFloat64Array3Structural(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	tape := &cursor.state.structural
	positions := tape.positions
	token := tape.index
	start := cursor.i
	if uint(token) >= uint(len(positions)) {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	pos := positions[token:]
	if len(pos) < 7 {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	_ = pos[6] // BCE hint: proves pos[1]..pos[6] in one check.
	first := uint(pos[1])
	comma1 := uint(pos[2])
	second := uint(pos[3])
	comma2 := uint(pos[4])
	third := uint(pos[5])
	closePosition := uint(pos[6])
	src := cursor.src
	if closePosition >= uint(len(src)) || third+8 > uint(len(src)) ||
		comma1 >= closePosition || comma2 >= closePosition {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	s := src[:closePosition+1]
	if s[comma1] != ',' || s[comma2] != ',' || s[closePosition] != ']' ||
		(int(first) != start && !cursor.structuralFirstValueGapOK()) {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	var v0, v1, v2 float64
	var ok0, ok1, ok2 bool
	switch comma1 - first {
	case 1:
		if d := s[first] - '0'; d <= 9 {
			v0, ok0 = float64(d), true
		}
	case 3:
		if d0, d2 := s[first]-'0', s[first+2]-'0'; s[first+1] == '.' && d0 <= 9 && d2 <= 9 {
			v0, ok0 = float64(d0)+oneDigitFractions[d2], true
		}
	case 4:
		if d1, d3 := s[first+1]-'0', s[first+3]-'0'; s[first] == '-' && d1 <= 9 && d3 <= 9 && s[first+2]|0x20 == 'e' {
			v0, ok0 = -float64(d1)*anyPow10[d3], true
		}
	}
	if !ok0 {
		v0, ok0 = shortStructuralFloatAt(src, first, comma1)
	}
	switch comma2 - second {
	case 1:
		if d := s[second] - '0'; d <= 9 {
			v1, ok1 = float64(d), true
		}
	case 3:
		if d0, d2 := s[second]-'0', s[second+2]-'0'; s[second+1] == '.' && d0 <= 9 && d2 <= 9 {
			v1, ok1 = float64(d0)+oneDigitFractions[d2], true
		}
	case 4:
		if d1, d3 := s[second+1]-'0', s[second+3]-'0'; s[second] == '-' && d1 <= 9 && d3 <= 9 && s[second+2]|0x20 == 'e' {
			v1, ok1 = -float64(d1)*anyPow10[d3], true
		}
	}
	if !ok1 {
		v1, ok1 = shortStructuralFloatAt(src, second, comma2)
	}
	switch closePosition - third {
	case 1:
		if d := s[third] - '0'; d <= 9 {
			v2, ok2 = float64(d), true
		}
	case 3:
		if d0, d2 := s[third]-'0', s[third+2]-'0'; s[third+1] == '.' && d0 <= 9 && d2 <= 9 {
			v2, ok2 = float64(d0)+oneDigitFractions[d2], true
		}
	case 4:
		if d1, d3 := s[third+1]-'0', s[third+3]-'0'; s[third] == '-' && d1 <= 9 && d3 <= 9 && s[third+2]|0x20 == 'e' {
			v2, ok2 = -float64(d1)*anyPow10[d3], true
		}
	}
	if !ok2 {
		v2, ok2 = shortStructuralFloatAt(src, third, closePosition)
	}
	if !ok0 || !ok1 || !ok2 {
		tape.index = token
		cursor.i = start
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	values := (*[3]float64)(dst)
	values[0] = v0
	values[1] = v1
	values[2] = v2
	tape.index = token + 6
	cursor.i = int(closePosition) + 1
	cursor.depth--
	return nil
}
func decodeCompiledFloat64ArrayStructural(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	tape := &cursor.state.structural
	positions := tape.positions
	src := cursor.src
	token := tape.index
	for token < len(positions) && int(positions[token]) < cursor.i-1 {
		token++
	}
	if token >= len(positions) || src[positions[token]] != '[' {
		return decodeCompiledFloatArray[float64](cursor, node, dst)
	}
	for index := 0; ; index++ {
		token++
		if token >= len(positions) {
			return cursor.err(len(src), "unterminated array")
		}
		position := int(positions[token])
		if index != 0 {
			if src[position] == ']' {
				tape.index = token
				cursor.i = position + 1
				cursor.depth--
				return nil
			}
			if src[position] != ',' {
				return cursor.err(position, "expected comma or closing bracket in array")
			}
			token++
			if token >= len(positions) {
				return cursor.err(len(src), "unterminated array")
			}
			position = int(positions[token])
		}
		if src[position] == ']' {
			tape.index = token
			cursor.i = position + 1
			cursor.depth--
			if index != 0 {
				return cursor.err(position, "expected value after comma in array")
			}
			if !replace {
				zeroTypedArrayTail(node, dst, index)
			}
			return nil
		}
		tape.index = token
		cursor.i = position
		if index < node.length {
			element := (*float64)(unsafe.Add(dst, uintptr(index)*node.elem.size))
			i := uint(position)
			n := uint(len(src))
			negative := src[i] == '-'
			if negative {
				i++
			}
			if i < n && IsDigit(src[i]) {
				value := float64(src[i] - '0')
				i++
				short := i >= n || !IsDigit(src[i])
				if short && i < n {
					switch src[i] {
					case '.':
						i++
						if i >= n || !IsDigit(src[i]) {
							short = false
						} else {
							value += float64(src[i]-'0') / 10
							i++
							short = i >= n || !IsDigit(src[i])
						}
					case 'e', 'E':
						i++
						exponentNegative := false
						if i < n && (src[i] == '+' || src[i] == '-') {
							exponentNegative = src[i] == '-'
							i++
						}
						if i >= n || !IsDigit(src[i]) {
							short = false
						} else {
							exponent := int(src[i] - '0')
							if exponentNegative {
								value /= anyPow10[exponent]
							} else {
								value *= anyPow10[exponent]
							}
							i++
							short = i >= n || !IsDigit(src[i])
						}
					}
				}
				if short && typedNumberEnd(src, int(i)) {
					if negative {
						value = -value
					}
					*element = value
					cursor.i = int(i)
					continue
				}
			}
			end, value, exact, ok := scanTypedFloat64(src, position)
			if ok && exact && typedNumberEnd(src, end) {
				*element = value
				cursor.i = end
				continue
			}
			if err := cursor.Float(element); err != nil {
				return prependDecodePathIndex(err, index)
			}
			if !typedNumberEnd(src, cursor.i) {
				return cursor.err(cursor.i, "invalid character after number")
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledFloatArrayStructural[T floatValue](cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	if cursor.state == nil || !cursor.state.structuralActive || cursor.state.structural.bad {
		return decodeCompiledFloatArray[T](cursor, node, dst)
	}
	tape := &cursor.state.structural
	positions := tape.positions
	src := cursor.src
	token := tape.index
	// The parent leaves the tape on '['. Synchronize once for uncommon
	// fallback entries, then each element is a fixed token increment.
	for token < len(positions) && int(positions[token]) < cursor.i-1 {
		token++
	}
	if token >= len(positions) || src[positions[token]] != '[' {
		return decodeCompiledFloatArray[T](cursor, node, dst)
	}
	for index := 0; ; index++ {
		token++
		if token >= len(positions) {
			return cursor.err(len(src), "unterminated array")
		}
		position := int(positions[token])
		if index != 0 {
			if src[position] == ']' {
				tape.index = token
				cursor.i = position + 1
				cursor.depth--
				return nil
			}
			if src[position] != ',' {
				return cursor.err(position, "expected comma or closing bracket in array")
			}
			token++
			if token >= len(positions) {
				return cursor.err(len(src), "unterminated array")
			}
			position = int(positions[token])
		}
		if src[position] == ']' {
			tape.index = token
			cursor.i = position + 1
			cursor.depth--
			if index != 0 {
				return cursor.err(position, "expected value after comma in array")
			}
			if !replace {
				zeroTypedArrayTail(node, dst, index)
			}
			return nil
		}
		tape.index = token
		cursor.i = position
		if index < node.length {
			element := (*T)(unsafe.Add(dst, uintptr(index)*node.elem.size))
			if !cursor.floatLong {
				if value, end, ok := shortTypedFloatAt(src, position); ok {
					*element = T(value)
					cursor.i = end
					continue
				}
				cursor.floatLong = true
			}
			if node.elem.size == 8 {
				end, value, exact, ok := scanTypedFloat64(src, position)
				if ok && exact {
					*element = T(value)
					cursor.i = end
					cursor.floatLong = end-position >= 6
					continue
				}
			}
			if err := cursor.Float(element); err != nil {
				return prependDecodePathIndex(err, index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

func decodeCompiledFloatArray[T floatValue](cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	src := cursor.src
	n := uint(len(src))
	// Straight-line coordinate-pair path: once elements are known to run
	// long, a compact [f,f] parses without the per-element loop machinery.
	if node.length == 2 && node.elem.size == 8 && cursor.floatLong {
		if i := uint(cursor.i); i < n && (src[i] == '-' || IsDigit(src[i])) {
			end0, v0, exact0, ok0 := scanTypedFloat64(src, int(i))
			if ok0 && exact0 && uint(end0) < n && src[end0] == ',' &&
				uint(end0)+1 < n && (src[end0+1] == '-' || IsDigit(src[end0+1])) {
				end1, v1, exact1, ok1 := scanTypedFloat64(src, end0+1)
				if ok1 && exact1 && uint(end1) < n && src[end1] == ']' {
					*(*T)(dst) = T(v0)
					*(*T)(unsafe.Add(dst, node.elem.size)) = T(v1)
					cursor.i = end1 + 1
					cursor.depth--
					return nil
				}
			}
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		// Fused fast path: consume the delimiter and a short float without the
		// general element iterator. cursor.i stays untouched until the whole
		// element is accepted, so every partial match falls through cleanly.
		i := uint(cursor.i)
		if i < n && src[i] <= ' ' {
			i = uint(cursor.skipSpaceAt(int(i)))
		}
		if i < n && index < node.length {
			c := src[i]
			if c == ']' {
				cursor.i = int(i) + 1
				cursor.depth--
				if !replace {
					zeroTypedArrayTail(node, dst, index)
				}
				return nil
			}
			if !first {
				if c != ',' {
					goto general
				}
				i++
				if i < n && src[i] <= ' ' {
					i = uint(cursor.skipSpaceAt(int(i)))
				}
			}
			if i < n && (src[i] == '-' || IsDigit(src[i])) {
				if !cursor.floatLong {
					if value, end, ok := shortTypedFloatAt(src, int(i)); ok {
						*(*T)(unsafe.Add(dst, uintptr(index)*node.elem.size)) = T(value)
						cursor.i = end
						continue
					}
					// Short probes fail on every element of uniformly long
					// arrays; stop paying for them until a short element
					// reappears.
					cursor.floatLong = true
				}
				if node.elem.size == 8 {
					end, value, exact, ok := scanTypedFloat64(src, int(i))
					if ok && exact {
						*(*T)(unsafe.Add(dst, uintptr(index)*node.elem.size)) = T(value)
						cursor.i = end
						cursor.floatLong = end-int(i) >= 6
						continue
					}
				}
			}
		}
	general:
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return err
		}
		if !more {
			if !replace {
				zeroTypedArrayTail(node, dst, index)
			}
			return nil
		}
		if index < node.length {
			element := (*T)(unsafe.Add(dst, uintptr(index)*node.elem.size))
			if err := cursor.Float(element); err != nil {
				return prependDecodePathIndex(err, index)
			}
		} else if err := cursor.Skip(); err != nil {
			return err
		}
	}
}

// zeroTypedArrayTail zeroes fixed-array elements past the document's length,
// matching encoding/json's array padding in merge mode.
func zeroTypedArrayTail(node *typedNode, dst unsafe.Pointer, from int) {
	for index := from; index < node.length; index++ {
		resetTyped(node.elem, unsafe.Add(dst, uintptr(index)*node.elem.size))
	}
}
