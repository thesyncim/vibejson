package vibejson

import (
	"unsafe"
)

// encodeSimpleStructPairs runs a simple struct's pair program. The caller
// has already appended the opening brace and the first member's name and
// checked depth (including fused levels); this function emits the members
// and the closing braces recorded in encClose, and owns the depth decrement
// on every exit path.
func (e *encodeState) encodeSimpleStructPairs(node *typedNode, src unsafe.Pointer) error {
	program := node.encodeProgram
	fields := program.encFields
	packedNames := program.encNameData
	dst := e.dst
	escapeHTML := e.escapeHTML
	i := 0
	for ; i+1 < len(fields); i += 2 {
		first := &fields[i]
		second := &fields[i+1]
		dst = appendSimpleFieldName(dst, first, packedNames)
		firstSrc := unsafe.Add(src, first.offset)
		secondSrc := unsafe.Add(src, second.offset)
		errorIndex := i
		var err error
		switch first.pairOp {
		case typedEncPairStringString:
			dst = appendEncodedJSONString(dst, *(*string)(firstSrc), escapeHTML)
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
			e.depth--
			return prependEncodePathField(err, program.encPaths[errorIndex])
		}
	}
	if i < len(fields) {
		field := &fields[i]
		dst = appendSimpleFieldName(dst, field, packedNames)
		// The scalar and fixed-array shapes that dominate odd-count structs
		// stay in this frame; everything else keeps the general dispatch.
		switch field.encOp {
		case typedOpString:
			dst = appendEncodedJSONString(dst, *(*string)(unsafe.Add(src, field.offset)), escapeHTML)
		case typedOpInt64:
			dst = appendCompactInt(dst, *(*int64)(unsafe.Add(src, field.offset)))
		case typedOpBool:
			if *(*bool)(unsafe.Add(src, field.offset)) {
				dst = append(dst, "true"...)
			} else {
				dst = append(dst, "false"...)
			}
		case typedOpArray:
			e.dst = dst
			var err error
			if field.node.elem.encOp == typedOpFloat64 && e.depth < DefaultMaxDepth {
				err = e.encodeFloat64Array(field.node, unsafe.Add(src, field.offset))
			} else {
				err = e.encodeArray(field.node, unsafe.Add(src, field.offset))
			}
			dst = e.dst
			if err != nil {
				e.depth--
				return prependEncodePathField(err, program.encPaths[i])
			}
		default:
			e.dst = dst
			if err := e.encodeStructFieldValue(field, unsafe.Add(src, field.offset)); err != nil {
				e.depth--
				return prependEncodePathField(err, program.encPaths[i])
			}
			dst = e.dst
		}
	}
	if len(program.encClose) == 1 {
		// The overwhelmingly common close stays an inlined single-byte
		// append; the slice form costs a memmove call per struct.
		dst = append(dst, '}')
	} else {
		dst = append(dst, program.encClose...)
	}
	e.dst = dst
	e.depth--
	return nil
}

func appendSimpleFieldName(dst []byte, field *typedEncField, packed []byte) []byte {
	if field.encNameLen == 0 || field.encNameLen > 16 || cap(dst)-len(dst) < 16 {
		return append(dst, field.encName...)
	}
	start := len(dst)
	dst = dst[:start+16]
	off := int(field.encNameBlock) * 16
	*(*[16]byte)(dst[start : start+16]) = *(*[16]byte)(packed[off : off+16])
	return dst[:start+int(field.encNameLen)]
}

func (e *encodeState) encodeStructFieldValue(field *typedEncField, src unsafe.Pointer) error {
	switch field.encOp {
	// BEGIN GENERATED TYPED ENCODER VALUE DISPATCH
	case typedOpBool:
		if *(*bool)(src) {
			e.dst = append(e.dst, "true"...)
		} else {
			e.dst = append(e.dst, "false"...)
		}
	case typedOpString:
		e.dst = appendEncodedJSONString(e.dst, *(*string)(src), e.escapeHTML)
	case typedOpNumber:
		return e.encodeNumberLiteral(*(*string)(src))
	case typedOpInt8:
		e.dst = appendCompactInt(e.dst, int64(*(*int8)(src)))
	case typedOpInt16:
		e.dst = appendCompactInt(e.dst, int64(*(*int16)(src)))
	case typedOpInt32:
		e.dst = appendCompactInt(e.dst, int64(*(*int32)(src)))
	case typedOpInt64:
		e.dst = appendCompactInt(e.dst, *(*int64)(src))
	case typedOpUint8:
		e.dst = appendCompactUint(e.dst, uint64(*(*uint8)(src)))
	case typedOpUint16:
		e.dst = appendCompactUint(e.dst, uint64(*(*uint16)(src)))
	case typedOpUint32:
		e.dst = appendCompactUint(e.dst, uint64(*(*uint32)(src)))
	case typedOpUint64:
		e.dst = appendCompactUint(e.dst, *(*uint64)(src))
	case typedOpFloat32:
		return e.encodeFloat(float64(*(*float32)(src)), 32)
	case typedOpFloat64:
		return e.encodeFloat(*(*float64)(src), 64)
	case typedOpStruct:
		return e.encodeStruct(field.node, src)
	case typedOpSlice:
		return e.encodeSlice(field.node, src)
	case typedOpArray:
		return e.encodeArray(field.node, src)
	case typedOpMap:
		return e.encodeMap(field.node, src)
	case typedOpAny:
		return e.encodeAny(src)
	case typedOpQuoted:
		return e.encodeQuoted(field.node, src)
	case typedOpMarshaler:
		return e.encodeMarshaler(field.node, src)
	// END GENERATED TYPED ENCODER VALUE DISPATCH
	default:
		return e.encode(field.node, src)
	}
	return nil
}
