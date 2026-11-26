package vibejson

import (
	"math"
	"unsafe"
)

func (e *encodeState) encodeSlice(node *typedNode, src unsafe.Pointer) error {
	header := typedSliceAt(node.typ, src)
	if header.isNil() {
		e.dst = append(e.dst, "null"...)
		return nil
	}
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	if node.elem.encOp == typedOpStruct {
		return e.encodeStructSlice(node, &header)
	}
	e.depth++
	// The element operation is loop invariant, so the hot scalar kinds get
	// dedicated loops. Integer elements also drop the first-element branch:
	// every value is emitted comma-prefixed and the opening bracket then
	// overwrites the leading comma in place.
	switch node.elem.encOp {
	case typedOpInt64:
		start := len(e.dst)
		for index := 0; index < header.len; index++ {
			e.dst = appendCommaCompactInt(e.dst, *(*int64)(header.elementAt(index, node.elem.size)))
		}
		if header.len == 0 {
			e.dst = append(e.dst, '[')
		} else {
			e.dst[start] = '['
		}
	case typedOpUint64:
		start := len(e.dst)
		for index := 0; index < header.len; index++ {
			e.dst = appendCommaCompactUint(e.dst, *(*uint64)(header.elementAt(index, node.elem.size)))
		}
		if header.len == 0 {
			e.dst = append(e.dst, '[')
		} else {
			e.dst[start] = '['
		}
	case typedOpString:
		e.dst = append(e.dst, '[')
		for index := 0; index < header.len; index++ {
			if index > 0 {
				e.dst = append(e.dst, ',')
			}
			e.dst = appendEncodedJSONString(e.dst, *(*string)(header.elementAt(index, node.elem.size)), e.escapeHTML)
		}
	case typedOpFloat64:
		e.dst = append(e.dst, '[')
		for index := 0; index < header.len; index++ {
			if index > 0 {
				e.dst = append(e.dst, ',')
			}
			if err := e.encodeFloat(*(*float64)(header.elementAt(index, node.elem.size)), 64); err != nil {
				e.depth--
				return prependEncodePathIndex(err, index)
			}
		}
	default:
		e.dst = append(e.dst, '[')
		for index := 0; index < header.len; index++ {
			if index > 0 {
				e.dst = append(e.dst, ',')
			}
			element := header.elementAt(index, node.elem.size)
			var err error
			switch node.elem.encOp {
			case typedOpStruct:
				err = e.encodeStruct(node.elem, element)
			case typedOpSlice:
				err = e.encodeSlice(node.elem, element)
			case typedOpArray:
				err = e.encodeArray(node.elem, element)
			case typedOpFloat64:
				err = e.encodeFloat(*(*float64)(element), 64)
			default:
				err = e.encode(node.elem, element)
			}
			if err != nil {
				e.depth--
				return prependEncodePathIndex(err, index)
			}
		}
	}
	e.dst = append(e.dst, ']')
	e.depth--
	return nil
}

func (e *encodeState) encodeStructSlice(node *typedNode, header *typedSliceState) error {
	e.depth++
	elem := node.elem
	if elem.encSimple && header.len > 0 {
		program := elem.encodeProgram
		// The depth test and the simple-struct dispatch are the same for
		// every element; run them once and drive the pair encoder
		// directly. An empty slice keeps succeeding at the depth limit,
		// exactly as the per-element check behaved, and encFusedExtra
		// accounts for static levels fused into the element's pairs.
		if e.depth+program.encFusedExtra >= DefaultMaxDepth {
			e.depth--
			return &EncodeError{Reason: "maximum nesting depth exceeded"}
		}
		// Every element opens with ",{" in one append and the bracket then
		// overwrites the leading comma, removing the first-element branch.
		start := len(e.dst)
		fields := program.encFields
		packedNames := program.encNameData
		closeBytes := program.encClose
		escapeHTML := e.escapeHTML
		elemSize := elem.size
		e.depth++
		dst := e.dst
		for index := 0; index < header.len; index++ {
			element := header.elementAt(index, elemSize)
			dst = append(dst, ',', '{')
			i := 0
			for ; i+1 < len(fields); i += 2 {
				first := &fields[i]
				second := &fields[i+1]
				dst = appendSimpleFieldName(dst, first, packedNames)
				firstSrc := unsafe.Add(element, first.offset)
				secondSrc := unsafe.Add(element, second.offset)
				errorIndex := i
				var err error
				switch first.pairOp {
				case typedEncPairStringString:
					dst = appendEncodedJSONString(dst, *(*string)(firstSrc), escapeHTML)
					dst = appendSimpleFieldName(dst, second, packedNames)
					dst = appendEncodedJSONString(dst, *(*string)(secondSrc), escapeHTML)
				case typedEncPairInt64Bool:
					dst = appendCompactInt(dst, *(*int64)(firstSrc))
					dst = appendSimpleFieldName(dst, second, packedNames)
					if *(*bool)(secondSrc) {
						dst = append(dst, "true"...)
					} else {
						dst = append(dst, "false"...)
					}
				case typedEncPairInt64Int64:
					dst = appendCompactInt(dst, *(*int64)(firstSrc))
					dst = appendSimpleFieldName(dst, second, packedNames)
					dst = appendCompactInt(dst, *(*int64)(secondSrc))
				case typedEncPairStringInt64:
					dst = appendEncodedJSONString(dst, *(*string)(firstSrc), escapeHTML)
					dst = appendSimpleFieldName(dst, second, packedNames)
					dst = appendCompactInt(dst, *(*int64)(secondSrc))
				case typedEncPairInt64String:
					dst = appendCompactInt(dst, *(*int64)(firstSrc))
					dst = appendSimpleFieldName(dst, second, packedNames)
					dst = appendEncodedJSONString(dst, *(*string)(secondSrc), escapeHTML)
				case typedEncPairSliceInt64:
					e.dst = dst
					err = e.encodeSlice(first.node, firstSrc)
					dst = e.dst
					if err == nil {
						dst = appendSimpleFieldName(dst, second, packedNames)
						dst = appendCompactInt(dst, *(*int64)(secondSrc))
					}
				case typedEncPairMapMap:
					e.dst = dst
					err = e.encodeMap(first.node, firstSrc)
					if err == nil {
						e.dst = appendSimpleFieldName(e.dst, second, packedNames)
						errorIndex = i + 1
						err = e.encodeMap(second.node, secondSrc)
					}
					dst = e.dst
				default:
					e.dst = dst
					err = e.encodeStructFieldValue(first, firstSrc)
					if err == nil {
						e.dst = appendSimpleFieldName(e.dst, second, packedNames)
						errorIndex = i + 1
						err = e.encodeStructFieldValue(second, secondSrc)
					}
					dst = e.dst
				}
				if err != nil {
					e.dst = dst
					e.depth -= 2
					return prependEncodePathIndex(prependEncodePathField(err, program.encPaths[errorIndex]), index)
				}
			}
			if i < len(fields) {
				field := &fields[i]
				dst = appendSimpleFieldName(dst, field, packedNames)
				switch field.encOp {
				case typedOpString:
					dst = appendEncodedJSONString(dst, *(*string)(unsafe.Add(element, field.offset)), escapeHTML)
				case typedOpInt64:
					dst = appendCompactInt(dst, *(*int64)(unsafe.Add(element, field.offset)))
				case typedOpBool:
					if *(*bool)(unsafe.Add(element, field.offset)) {
						dst = append(dst, "true"...)
					} else {
						dst = append(dst, "false"...)
					}
				case typedOpArray:
					if field.node.elem.encOp == typedOpFloat64 && e.depth < DefaultMaxDepth {
						var err error
						dst, err = e.appendFloat64Array(dst, field.node, unsafe.Add(element, field.offset))
						if err != nil {
							e.dst = dst
							e.depth -= 2
							return prependEncodePathIndex(prependEncodePathField(err, program.encPaths[i]), index)
						}
					} else {
						e.dst = dst
						if err := e.encodeArray(field.node, unsafe.Add(element, field.offset)); err != nil {
							e.depth -= 2
							return prependEncodePathIndex(prependEncodePathField(err, program.encPaths[i]), index)
						}
						dst = e.dst
					}
				default:
					e.dst = dst
					if err := e.encodeStructFieldValue(field, unsafe.Add(element, field.offset)); err != nil {
						e.depth -= 2
						return prependEncodePathIndex(prependEncodePathField(err, program.encPaths[i]), index)
					}
					dst = e.dst
				}
			}
			if len(closeBytes) == 1 {
				dst = append(dst, '}')
			} else {
				dst = append(dst, closeBytes...)
			}
		}
		dst[start] = '['
		e.dst = append(dst, ']')
		e.depth -= 2
		return nil
	}
	e.dst = append(e.dst, '[')
	for index := 0; index < header.len; index++ {
		if index > 0 {
			e.dst = append(e.dst, ',')
		}
		element := header.elementAt(index, elem.size)
		if err := e.encodeStruct(elem, element); err != nil {
			e.depth--
			return prependEncodePathIndex(err, index)
		}
	}
	e.dst = append(e.dst, ']')
	e.depth--
	return nil
}

func (e *encodeState) encodeArray(node *typedNode, src unsafe.Pointer) error {
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	if node.elem.encOp == typedOpFloat64 {
		return e.encodeFloat64Array(node, src)
	}
	e.depth++
	e.dst = append(e.dst, '[')
	for index := 0; index < node.length; index++ {
		if index > 0 {
			e.dst = append(e.dst, ',')
		}
		element := unsafe.Add(src, uintptr(index)*node.elem.size)
		if err := e.encode(node.elem, element); err != nil {
			e.depth--
			return prependEncodePathIndex(err, index)
		}
	}
	e.dst = append(e.dst, ']')
	e.depth--
	return nil
}

// encodeNonAddressableArray preserves the non-addressability of an array
// reached through a map value or interface while recursively applying the same
// rule to nested arrays and structs. A slice or pointer element never reaches
// this function because those operations restore addressability.
func (e *encodeState) encodeNonAddressableArray(node *typedNode, src unsafe.Pointer) error {
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	e.depth++
	e.dst = append(e.dst, '[')
	for index := 0; index < node.length; index++ {
		if index > 0 {
			e.dst = append(e.dst, ',')
		}
		element := unsafe.Add(src, uintptr(index)*node.elem.size)
		var err error
		if node.elem.encHasPtrMarshaler {
			err = e.encodeNonAddressableMarshaler(node.elem, element)
		} else {
			err = e.encode(node.elem, element)
		}
		if err != nil {
			e.depth--
			return prependEncodePathIndex(err, index)
		}
	}
	e.dst = append(e.dst, ']')
	e.depth--
	return nil
}

func (e *encodeState) encodeFloat64Array(node *typedNode, src unsafe.Pointer) error {
	dst, err := e.appendFloat64Array(e.dst, node, src)
	e.dst = dst
	return err
}

func (e *encodeState) appendFloat64Array(dst []byte, node *typedNode, src unsafe.Pointer) ([]byte, error) {
	dst = append(dst, '[')
	for index := 0; index < node.length; index++ {
		if index > 0 {
			dst = append(dst, ',')
		}
		v := *(*float64)(unsafe.Add(src, uintptr(index)*8))
		if math.Float64bits(v)<<1 < 0xffe0000000000000 {
			if positive := math.Abs(v); positive < 1e15 {
				if math.Trunc(v) == v {
					if v == 0 && math.Signbit(v) {
						dst = append(dst, '-', '0')
					} else {
						dst = appendCompactInt(dst, int64(v))
					}
					continue
				}
				if positive < 1e9 {
					if scaled := v * 10; math.Trunc(scaled) == scaled && scaled/10 == v {
						if scaled < 0 {
							dst = append(dst, '-')
							scaled = -scaled
						}
						digits := uint64(scaled)
						if digits < 100 {
							dst = append(dst, byte('0'+digits/10), '.', byte('0'+digits%10))
						} else {
							dst = appendCompactUint(dst, digits/10)
							dst = append(dst, '.', byte('0'+digits%10))
						}
						continue
					}
				}
			}
		}
		e.dst = dst
		e.depth++
		if err := e.encodeFloat(v, 64); err != nil {
			e.depth--
			return e.dst, prependEncodePathIndex(err, index)
		}
		e.depth--
		dst = e.dst
	}
	return append(dst, ']'), nil
}
