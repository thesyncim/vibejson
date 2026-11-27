package vibejson

import "unsafe"

// decodeCompiledRootSlice keeps DecodeArray's statically known []T concrete.
// Built-in []int64, []uint64, and []float64 take zero-unsafe concrete loops;
// other element types reuse the compiled element dispatcher.
func decodeCompiledRootSlice[T any](cursor *decoderCursor, node *typedNode, dst []T) ([]T, error) {
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '[' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return dst, err
			}
		}
		if null {
			return nil, nil
		}
		if err := cursor.BeginArray(node.name); err != nil {
			return dst, err
		}
	}

	if node.elem.kind == typedInt || node.elem.kind == typedUint || node.elem.kind == typedFloat {
		switch s := any(dst).(type) {
		case []int64:
			res, err := decodeCompiledRootInt64Slice(cursor, node.elem, s)
			return any(res).([]T), err
		case []uint64:
			res, err := decodeCompiledRootUint64Slice(cursor, node.elem, s)
			return any(res).([]T), err
		case []float64:
			res, err := decodeCompiledRootFloat64Slice(cursor, node.elem, s)
			return any(res).([]T), err
		}
	}

	dst = dst[:0]
	for index, first := 0, true; ; index, first = index+1, false {
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return dst, err
		}
		if !more {
			if index == 0 {
				// Match encoding/json: a non-null empty array replaces any
				// retained backing with a fresh non-nil, zero-capacity slice.
				dst = make([]T, 0)
			}
			return dst, nil
		}
		if index == cap(dst) {
			capacity := nextTypedSliceCapacity(cap(dst), index+1)
			if cap(dst) == 0 && node.elem.kind == typedStruct && cursor.depth <= 3 {
				if estimate := (len(cursor.src) - cursor.i) / 128; estimate > capacity {
					capacity = estimate
					if capacity > 1024 {
						capacity = 1024
					}
				}
			}
			next := make([]T, index, capacity)
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		element := unsafe.Pointer(&dst[index])
		batchedReceivers := node.decHasReceiver && index > 0 && cursor.beginReceiverBatch()
		var elementErr error
		switch node.elem.kind {
		case typedStruct:
			elementErr = cursor.decodeCompiledStruct(node.elem, element)
		case typedSlice:
			elementErr = cursor.decodeCompiledSlice(node.elem, element)
		case typedArray:
			elementErr = cursor.decodeCompiledArray(node.elem, element)
		case typedPointer:
			elementErr = cursor.decodeCompiledPointer(node.elem, element)
		case typedMap:
			elementErr = cursor.decodeCompiledMap(node.elem, element)
		default:
			elementErr = cursor.decodeCompiled(node.elem, element)
		}
		if node.decHasReceiver {
			cursor.endReceiverBatch(batchedReceivers)
		}
		if elementErr != nil {
			return dst, prependDecodePathIndex(elementErr, index)
		}
	}
}

func decodeCompiledRootInt64Slice(cursor *decoderCursor, elem *typedNode, dst []int64) ([]int64, error) {
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]int64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return dst, err
			}
			if !more {
				if index == 0 {
					dst = make([]int64, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]int64, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		if err := cursor.Int(&dst[index]); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}

func decodeCompiledRootUint64Slice(cursor *decoderCursor, elem *typedNode, dst []uint64) ([]uint64, error) {
	if count, _, ok := fixed16Uint64ArrayShape(cursor.src, cursor.i); ok {
		return decodeCompiledRootFixed16Slice(cursor, dst, count), nil
	}
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]uint64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return dst, err
			}
			if !more {
				if index == 0 {
					dst = make([]uint64, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]uint64, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		if err := cursor.Uint(&dst[index]); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}

// decodeCompiledRootFixed16Slice decodes a validated fixed-16-digit uint64
// array directly into dst with zero unsafe.
func decodeCompiledRootFixed16Slice(cursor *decoderCursor, dst []uint64, count int) []uint64 {
	if cap(dst) < count {
		capacity := cap(dst)
		if capacity == 0 {
			capacity = count
		} else {
			for capacity < count {
				capacity = nextTypedSliceCapacity(capacity, capacity+1)
			}
		}
		dst = make([]uint64, count, capacity)
	} else {
		dst = dst[:count]
	}
	parseFixed16Uint64Array(cursor.src[cursor.i:cursor.i+count*17], dst)
	cursor.i += count * 17
	cursor.depth--
	return dst
}

func decodeCompiledRootFloat64Slice(cursor *decoderCursor, elem *typedNode, dst []float64) ([]float64, error) {
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]float64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return dst, err
			}
			if !more {
				if index == 0 {
					dst = make([]float64, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]float64, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		if end, value, exact, ok := scanTypedFloat64(cursor.src, cursor.i); ok && exact && typedNumberEnd(cursor.src, end) {
			dst[index] = value
			cursor.i = end
			continue
		}
		if err := cursor.Float(&dst[index]); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}
