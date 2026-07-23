package slopjson

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
	i := 0
	for ; i+1 < len(fields); i += 2 {
		first := &fields[i]
		second := &fields[i+1]
		e.dst = appendSimpleFieldName(e.dst, first, packedNames)
		firstSrc := unsafe.Add(src, first.offset)
		secondSrc := unsafe.Add(src, second.offset)
		errorIndex := i
		var err error
		switch first.pairOp {
		case typedEncPairStringString:
			e.dst = appendEncodedJSONString(e.dst, *(*string)(firstSrc), e.escapeHTML)
			e.dst = appendSimpleFieldName(e.dst, second, packedNames)
			e.dst = appendEncodedJSONString(e.dst, *(*string)(secondSrc), e.escapeHTML)
		case typedEncPairSliceInt64:
			err = e.encodeSlice(first.node, firstSrc)
			if err == nil {
				e.dst = appendSimpleFieldName(e.dst, second, packedNames)
				e.dst = appendCompactInt(e.dst, *(*int64)(secondSrc))
			}
		case typedEncPairMapMap:
			err = e.encodeMap(first.node, firstSrc)
			if err == nil {
				e.dst = appendSimpleFieldName(e.dst, second, packedNames)
				errorIndex = i + 1
				err = e.encodeMap(second.node, secondSrc)
			}
		case typedEncPairInt64Bool:
			e.dst = appendCompactInt(e.dst, *(*int64)(firstSrc))
			e.dst = appendSimpleFieldName(e.dst, second, packedNames)
			if *(*bool)(secondSrc) {
				e.dst = append(e.dst, "true"...)
			} else {
				e.dst = append(e.dst, "false"...)
			}
		case typedEncPairInt64Int64:
			e.dst = appendCompactInt(e.dst, *(*int64)(firstSrc))
			e.dst = appendSimpleFieldName(e.dst, second, packedNames)
			e.dst = appendCompactInt(e.dst, *(*int64)(secondSrc))
		case typedEncPairStringInt64:
			e.dst = appendEncodedJSONString(e.dst, *(*string)(firstSrc), e.escapeHTML)
			e.dst = appendSimpleFieldName(e.dst, second, packedNames)
			e.dst = appendCompactInt(e.dst, *(*int64)(secondSrc))
		case typedEncPairInt64String:
			e.dst = appendCompactInt(e.dst, *(*int64)(firstSrc))
			e.dst = appendSimpleFieldName(e.dst, second, packedNames)
			e.dst = appendEncodedJSONString(e.dst, *(*string)(secondSrc), e.escapeHTML)
		default:
			err = e.encodeStructFieldValue(first, firstSrc)
			if err == nil {
				e.dst = appendSimpleFieldName(e.dst, second, packedNames)
				errorIndex = i + 1
				err = e.encodeStructFieldValue(second, secondSrc)
			}
		}
		if err != nil {
			e.depth--
			return prependEncodePathField(err, program.encPaths[errorIndex])
		}
	}
	if i < len(fields) {
		field := &fields[i]
		e.dst = appendSimpleFieldName(e.dst, field, packedNames)
		// The three scalar shapes that dominate odd-count structs stay in
		// this frame; everything else keeps the general dispatch.
		switch field.encOp {
		case typedOpString:
			e.dst = appendEncodedJSONString(e.dst, *(*string)(unsafe.Add(src, field.offset)), e.escapeHTML)
		case typedOpInt64:
			e.dst = appendCompactInt(e.dst, *(*int64)(unsafe.Add(src, field.offset)))
		case typedOpBool:
			if *(*bool)(unsafe.Add(src, field.offset)) {
				e.dst = append(e.dst, "true"...)
			} else {
				e.dst = append(e.dst, "false"...)
			}
		default:
			if err := e.encodeStructFieldValue(field, unsafe.Add(src, field.offset)); err != nil {
				e.depth--
				return prependEncodePathField(err, program.encPaths[i])
			}
		}
	}
	if len(program.encClose) == 1 {
		// The overwhelmingly common close stays an inlined single-byte
		// append; the slice form costs a memmove call per struct.
		e.dst = append(e.dst, '}')
	} else {
		e.dst = append(e.dst, program.encClose...)
	}
	e.depth--
	return nil
}

func appendSimpleFieldName(dst []byte, field *typedEncField, packed []byte) []byte {
	if field.encNameLen == 0 || cap(dst)-len(dst) < 16 {
		return append(dst, field.encName...)
	}
	start := len(dst)
	dst = dst[:start+16]
	// Base-pointer forms keep the block copy free of bounds checks; the
	// cap guard above proved sixteen writable bytes and encNameBlock
	// indexes blocks the compiler packed. The min is free — encNameLen is
	// at most sixteen by construction — and lets the final reslice prove.
	*(*[16]byte)(unsafe.Add(sliceBase(dst), start)) =
		*(*[16]byte)(unsafe.Add(sliceBase(packed), int(field.encNameBlock)*16))
	return dst[:start+min(int(field.encNameLen), 16)]
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
