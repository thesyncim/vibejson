package simdjson

import "unsafe"

var oneDigitFractions = [...]float64{0, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9}

// shortStructuralFloatAt classifies the exact compact one-digit forms by
// their tape-proved width. Uncommon whitespace and wider forms return to the
// caller's full transactional decoder.
func shortStructuralFloatAt(base unsafe.Pointer, start, limit int) (float64, bool) {
	width := limit - start
	if width <= 0 {
		return 0, false
	}
	word := loadUint64LE(unsafe.Add(base, start))
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
	if uint(token+6) >= uint(len(positions)) {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	src := cursor.src
	base := sliceBase(src)
	first := int(positions[token+1])
	comma1 := int(positions[token+2])
	second := int(positions[token+3])
	comma2 := int(positions[token+4])
	third := int(positions[token+5])
	closePosition := int(positions[token+6])
	if fastByteAt(base, comma1) != ',' || fastByteAt(base, comma2) != ',' ||
		fastByteAt(base, closePosition) != ']' || first+8 > len(src) ||
		second+8 > len(src) || third+8 > len(src) {
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	values := (*[3]float64)(dst)
	value, ok := shortStructuralFloatAt(base, first, comma1)
	if !ok {
		tape.index = token
		cursor.i = start
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	values[0] = value
	value, ok = shortStructuralFloatAt(base, second, comma2)
	if !ok {
		tape.index = token
		cursor.i = start
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	values[1] = value
	value, ok = shortStructuralFloatAt(base, third, closePosition)
	if !ok {
		tape.index = token
		cursor.i = start
		return decodeCompiledFloat64ArrayStructural(cursor, node, dst)
	}
	values[2] = value
	tape.index = token + 6
	cursor.i = closePosition + 1
	cursor.depth--
	return nil
}
func decodeCompiledFloat64ArrayStructural(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	tape := &cursor.state.structural
	positions := tape.positions
	src := cursor.src
	base := sliceBase(src)
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
			i := position
			negative := fastByteAt(base, i) == '-'
			if negative {
				i++
			}
			if i < len(src) && isDigit(fastByteAt(base, i)) {
				value := float64(fastByteAt(base, i) - '0')
				i++
				short := i >= len(src) || !isDigit(fastByteAt(base, i))
				if short && i < len(src) {
					switch fastByteAt(base, i) {
					case '.':
						i++
						if i >= len(src) || !isDigit(fastByteAt(base, i)) {
							short = false
						} else {
							value += float64(fastByteAt(base, i)-'0') / 10
							i++
							short = i >= len(src) || !isDigit(fastByteAt(base, i))
						}
					case 'e', 'E':
						i++
						exponentNegative := false
						if i < len(src) && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
							exponentNegative = fastByteAt(base, i) == '-'
							i++
						}
						if i >= len(src) || !isDigit(fastByteAt(base, i)) {
							short = false
						} else {
							exponent := int(fastByteAt(base, i) - '0')
							if exponentNegative {
								value /= anyPow10[exponent]
							} else {
								value *= anyPow10[exponent]
							}
							i++
							short = i >= len(src) || !isDigit(fastByteAt(base, i))
						}
					}
				}
				if short && typedNumberEnd(base, len(src), i) {
					if negative {
						value = -value
					}
					*element = value
					cursor.i = i
					continue
				}
			}
			end, value, exact, ok := scanTypedFloat64(base, len(src), position)
			if ok && exact && typedNumberEnd(base, len(src), end) {
				*element = value
				cursor.i = end
				continue
			}
			if useStableNumericMethods {
				if err := cursor.Float64(element); err != nil {
					return prependDecodePathIndex(err, index)
				}
			} else if err := cursor.Float(element); err != nil {
				return prependDecodePathIndex(err, index)
			}
			if !typedNumberEnd(base, len(src), cursor.i) {
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
	base := sliceBase(src)
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
				if value, end, ok := shortTypedFloatAt(base, len(src), position); ok {
					*element = T(value)
					cursor.i = end
					continue
				}
				cursor.floatLong = true
			}
			if unsafe.Sizeof(T(0)) == 8 {
				end, value, exact, ok := scanTypedFloat64(base, len(src), position)
				if ok && exact {
					*element = T(value)
					cursor.i = end
					cursor.floatLong = end-position >= 6
					continue
				}
			}
			if useStableNumericMethods {
				if err := decoderCursorFloat(cursor, element); err != nil {
					return prependDecodePathIndex(err, index)
				}
			} else if err := cursor.Float(element); err != nil {
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
	base := sliceBase(src)
	// Straight-line coordinate-pair path: once elements are known to run
	// long, a compact [f,f] parses without the per-element loop machinery.
	if node.length == 2 && unsafe.Sizeof(T(0)) == 8 && cursor.floatLong {
		if i := cursor.i; i < len(src) && (fastByteAt(base, i) == '-' || isDigit(fastByteAt(base, i))) {
			end0, v0, exact0, ok0 := scanTypedFloat64(base, len(src), i)
			if ok0 && exact0 && end0 < len(src) && fastByteAt(base, end0) == ',' &&
				end0+1 < len(src) && (fastByteAt(base, end0+1) == '-' || isDigit(fastByteAt(base, end0+1))) {
				end1, v1, exact1, ok1 := scanTypedFloat64(base, len(src), end0+1)
				if ok1 && exact1 && end1 < len(src) && fastByteAt(base, end1) == ']' {
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
		i := cursor.i
		if i < len(src) && fastByteAt(base, i) <= ' ' {
			i = cursor.skipSpaceAt(i)
		}
		if i < len(src) && index < node.length {
			c := fastByteAt(base, i)
			if c == ']' {
				cursor.i = i + 1
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
				if i < len(src) && fastByteAt(base, i) <= ' ' {
					i = cursor.skipSpaceAt(i)
				}
			}
			if i < len(src) && (fastByteAt(base, i) == '-' || isDigit(fastByteAt(base, i))) {
				if !cursor.floatLong {
					if value, end, ok := shortTypedFloatAt(base, len(src), i); ok {
						*(*T)(unsafe.Add(dst, uintptr(index)*node.elem.size)) = T(value)
						cursor.i = end
						continue
					}
					// Short probes fail on every element of uniformly long
					// arrays; stop paying for them until a short element
					// reappears.
					cursor.floatLong = true
				}
				if unsafe.Sizeof(T(0)) == 8 {
					end, value, exact, ok := scanTypedFloat64(base, len(src), i)
					if ok && exact {
						*(*T)(unsafe.Add(dst, uintptr(index)*node.elem.size)) = T(value)
						cursor.i = end
						cursor.floatLong = end-i >= 6
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
			if useStableNumericMethods {
				if err := decoderCursorFloat(cursor, element); err != nil {
					return prependDecodePathIndex(err, index)
				}
			} else if err := cursor.Float(element); err != nil {
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
