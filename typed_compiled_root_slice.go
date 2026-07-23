package slopjson

import "unsafe"

// decodeCompiledRootSlice keeps DecodeArray's statically known []T concrete.
// Dynamic nested slice types cross through reflect in decodeCompiledSlice,
// while this boundary can use ordinary Go slice operations without exposing a
// slice header or making DecodeArray's local slice variable escape.
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

	switch elem := node.elem; elem.kind {
	case typedInt:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledRootInt64Slice(cursor, elem, dst)
		}
	case typedUint:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledRootUint64Slice(cursor, elem, dst)
		}
	case typedFloat:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledRootFloat64Slice(cursor, elem, dst)
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

func decodeCompiledRootInt64Slice[T any](cursor *decoderCursor, elem *typedNode, dst []T) ([]T, error) {
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]T, 0, capacity)
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
					dst = make([]T, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]T, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		element := (*int64)(unsafe.Pointer(&dst[index]))
		if useStableNumericMethods {
			if err := cursor.Int64(element); err != nil {
				return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
			}
		} else if err := cursor.Int(element); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}

func decodeCompiledRootUint64Slice[T any](cursor *decoderCursor, elem *typedNode, dst []T) ([]T, error) {
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]T, 0, capacity)
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
					dst = make([]T, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]T, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		element := (*uint64)(unsafe.Pointer(&dst[index]))
		if useStableNumericMethods {
			if err := cursor.Uint64(element); err != nil {
				return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
			}
		} else if err := cursor.Uint(element); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}

func decodeCompiledRootFloat64Slice[T any](cursor *decoderCursor, elem *typedNode, dst []T) ([]T, error) {
	dst = dst[:0]
	if cap(dst) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			dst = make([]T, 0, capacity)
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
					dst = make([]T, 0)
				}
				return dst, nil
			}
		}
		if index == cap(dst) {
			next := make([]T, index, nextTypedSliceCapacity(cap(dst), index+1))
			copy(next, dst)
			dst = next
		}
		dst = dst[:index+1]
		element := (*float64)(unsafe.Pointer(&dst[index]))
		if useStableNumericMethods {
			if err := cursor.Float64(element); err != nil {
				return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
			}
		} else if err := cursor.Float(element); err != nil {
			return dst, prependDecodePathIndex(retagCompiledError(err, elem.typ), index)
		}
	}
}
