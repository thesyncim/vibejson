package simdjson

import (
	"unsafe"
)

func (e *encodeState) encodeSlice(node *typedNode, src unsafe.Pointer) error {
	header := typedSliceAt(node.typ, src)
	if header.value.IsNil() {
		e.dst = append(e.dst, "null"...)
		return nil
	}
	if encoderDetectCycles {
		key := encoderCycleKey{typ: node.typ, ptr: header.data, length: header.len, kind: encoderCycleSlice}
		if err := e.enterReference(key); err != nil {
			return err
		}
		defer e.leaveReference(key)
	}
	if encoderHasDepthLimit && e.depth >= defaultMaxDepth {
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
		if encoderHasDepthLimit && e.depth+int(program.encFusedExtra) >= defaultMaxDepth {
			e.depth--
			return &EncodeError{Reason: "maximum nesting depth exceeded"}
		}
		// Every element opens with ",{" in one append and the bracket then
		// overwrites the leading comma, removing the first-element branch.
		start := len(e.dst)
		for index := 0; index < header.len; index++ {
			element := header.elementAt(index, elem.size)
			e.depth++
			e.dst = append(e.dst, ',', '{')
			if err := e.encodeSimpleStructPairs(elem, element); err != nil {
				e.depth--
				return prependEncodePathIndex(err, index)
			}
		}
		e.dst[start] = '['
		e.dst = append(e.dst, ']')
		e.depth--
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
	if encoderHasDepthLimit && e.depth >= defaultMaxDepth {
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

func (e *encodeState) encodeFloat64Array(node *typedNode, src unsafe.Pointer) error {
	e.depth++
	e.dst = append(e.dst, '[')
	if node.length > 0 {
		if err := e.encodeFloat(*(*float64)(src), 64); err != nil {
			e.depth--
			return prependEncodePathIndex(err, 0)
		}
		for index := 1; index < node.length; index++ {
			e.dst = append(e.dst, ',')
			element := unsafe.Add(src, uintptr(index)*8)
			if err := e.encodeFloat(*(*float64)(element), 64); err != nil {
				e.depth--
				return prependEncodePathIndex(err, index)
			}
		}
	}
	e.dst = append(e.dst, ']')
	e.depth--
	return nil
}
