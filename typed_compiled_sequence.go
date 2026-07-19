package simdjson

import (
	"bytes"
	"unsafe"
)

const scalarSliceReserveMinBytes = 4096

func (cursor *decoderCursor) decodeCompiledSlice(node *typedNode, dst unsafe.Pointer) error {
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '[' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			setTypedSliceZero(node, dst)
			return nil
		}
		if err := cursor.BeginArray(node.name); err != nil {
			return err
		}
	}
	// Homogeneous scalar slices ([]int64, []float64, ...) run a fused loop that
	// hoists the element-kind dispatch out of the per-element path and parses
	// each number straight into the backing array, replacing the generic
	// double-switch below. The element kind is loop-invariant, so the choice is
	// made once here; each branch matches the exact 64-bit width so a named
	// alias type is written through storage of the right size.
	switch elem := node.elem; elem.kind {
	case typedInt:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledInt64Slice(cursor, node, dst)
		}
	case typedUint:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledUint64Slice(cursor, node, dst)
		}
	case typedFloat:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledFloat64Slice(cursor, node, dst)
		}
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	if node.decHasReceiver {
		return cursor.decodeCompiledSliceReceivers(node, dst, &header)
	}
	for index, first := 0, true; ; index, first = index+1, false {
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return err
		}
		if !more {
			if index == 0 {
				// An empty array yields a fresh empty slice, dropping any
				// reused backing, exactly like encoding/json's MakeSlice(T,0,0).
				// Keeping the old array would leak stale elements when the same
				// destination later decodes a longer array into the reused cap.
				setTypedEmptySlice(node, dst)
			}
			return nil
		}
		if index == header.cap {
			capacity := nextTypedSliceCapacity(header.cap, index+1)
			if header.cap == 0 && node.elem.kind == typedStruct && cursor.depth <= 3 {
				if estimate := (len(cursor.src) - cursor.i) / 128; estimate > capacity {
					capacity = estimate
					if capacity > 1024 {
						capacity = 1024
					}
				}
			}
			header.grow(capacity)
		}
		header.setLen(index + 1)
		element := unsafe.Add(header.data, uintptr(index)*node.elem.size)
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
		if elementErr != nil {
			return prependDecodePathIndex(elementErr, index)
		}
	}
}

// decodeCompiledSliceReceivers is the uncommon generic slice loop for element
// graphs containing standard unmarshal methods. Keeping it separate leaves the
// ordinary compiled slice loop byte-for-byte free of receiver-arena branches.
func (cursor *decoderCursor) decodeCompiledSliceReceivers(node *typedNode, dst unsafe.Pointer, header *typedSliceState) error {
	for index, first := 0, true; ; index, first = index+1, false {
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return err
		}
		if !more {
			if index == 0 {
				setTypedEmptySlice(node, dst)
			}
			return nil
		}
		if index == header.cap {
			capacity := nextTypedSliceCapacity(header.cap, index+1)
			if header.cap == 0 && node.elem.kind == typedStruct && cursor.depth <= 3 {
				if estimate := (len(cursor.src) - cursor.i) / 128; estimate > capacity {
					capacity = estimate
					if capacity > 1024 {
						capacity = 1024
					}
				}
			}
			header.grow(capacity)
		}
		header.setLen(index + 1)
		element := unsafe.Add(header.data, uintptr(index)*node.elem.size)
		batchedReceivers := index > 0 && cursor.beginReceiverBatch()
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
		cursor.endReceiverBatch(batchedReceivers)
		if elementErr != nil {
			return prependDecodePathIndex(elementErr, index)
		}
	}
}

func (cursor *decoderCursor) decodeCompiledSliceStructural(node *typedNode, dst unsafe.Pointer) error {
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '[' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			setTypedSliceZero(node, dst)
			return nil
		}
		if err := cursor.BeginArray(node.name); err != nil {
			return err
		}
	}
	if !cursor.structuralFirstValueGapOK() {
		return cursor.err(cursor.i, "unexpected colon after array opener")
	}
	switch elem := node.elem; elem.kind {
	case typedInt:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledInt64Slice(cursor, node, dst)
		}
	case typedUint:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledUint64Slice(cursor, node, dst)
		}
	case typedFloat:
		if elem.bits == 64 && elem.size == 8 {
			return decodeCompiledFloat64Slice(cursor, node, dst)
		}
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	for index, first := 0, true; ; index, first = index+1, false {
		var more bool
		var err error
		switch cursor.nextArrayElementExact(first) {
		case structuralArrayValue:
			more = true
		case structuralArrayEnd:
			more = false
		default:
			var handled bool
			more, handled, err = cursor.nextArrayElementStructural(first)
			if !handled {
				more, err = cursor.NextArrayElement(first)
			}
		}
		if err != nil {
			return err
		}
		if !more {
			if index == 0 {
				setTypedEmptySlice(node, dst)
			}
			return nil
		}
		if index == header.cap {
			capacity := nextTypedSliceCapacity(header.cap, index+1)
			if header.cap == 0 && node.elem.kind == typedStruct && cursor.depth <= 3 {
				if estimate := (len(cursor.src) - cursor.i) / 128; estimate > capacity {
					capacity = estimate
					if capacity > 1024 {
						capacity = 1024
					}
				}
			}
			header.grow(capacity)
		}
		header.setLen(index + 1)
		element := unsafe.Add(header.data, uintptr(index)*node.elem.size)
		var elementErr error
		switch node.elem.kind {
		case typedStruct:
			elementErr = cursor.decodeCompiledStructStructural(node.elem, element)
		case typedSlice:
			elementErr = cursor.decodeCompiledSliceStructural(node.elem, element)
		case typedArray:
			elementErr = cursor.decodeCompiledArrayStructural(node.elem, element)
		case typedPointer:
			elementErr = cursor.decodeCompiledPointer(node.elem, element)
		case typedMap:
			elementErr = cursor.decodeCompiledMap(node.elem, element)
		default:
			elementErr = cursor.decodeCompiled(node.elem, element)
		}
		if elementErr != nil {
			return prependDecodePathIndex(elementErr, index)
		}
		cursor.syncStructuralValue()
	}
}

// The fused scalar-slice decoders below (int64 / uint64 / float64) each replace
// the generic loop in decodeCompiledSlice for a homogeneous slice of that 64-bit
// scalar. Exact built-in slices stay concrete for the whole loop; defined
// slice or element types use the reflective dynamic-slice boundary. Grow,
// empty-array, and error semantics are identical to the generic path. The gain
// is structural: the loop-invariant element kind is resolved once, so every
// element parses straight through the number method without the
// generic double-switch, and the common "value then comma" delimiter step is
// consumed inline so a full array of numbers never re-enters NextArrayElement.
//
// The delimiter-and-grow prologue is intentionally duplicated across the three
// rather than factored behind a helper that returns the element pointer: such a
// helper can make the destination escape. Keeping the element address local
// also makes the unsafe precondition visible at each use.

func decodeCompiledInt64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	if node.decBuiltinSlice {
		return decodeCompiledBuiltinInt64Slice(cursor, node, (*[]int64)(dst))
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	if header.cap == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			header.grow(capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if index == 0 {
					setTypedEmptySlice(node, dst)
				}
				return nil
			}
		}
		if index == header.cap {
			header.grow(nextTypedSliceCapacity(header.cap, index+1))
		}
		header.setLen(index + 1)
		element := (*int64)(unsafe.Add(header.data, uintptr(index)*node.elem.size))
		if useStableNumericMethods {
			if err := cursor.Int64(element); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Int(element); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

func decodeCompiledUint64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	if node.decBuiltinSlice {
		return decodeCompiledBuiltinUint64Slice(cursor, node, (*[]uint64)(dst))
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	if header.cap == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			header.grow(capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if index == 0 {
					setTypedEmptySlice(node, dst)
				}
				return nil
			}
		}
		if index == header.cap {
			header.grow(nextTypedSliceCapacity(header.cap, index+1))
		}
		header.setLen(index + 1)
		element := (*uint64)(unsafe.Add(header.data, uintptr(index)*node.elem.size))
		if useStableNumericMethods {
			if err := cursor.Uint64(element); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Uint(element); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

func decodeCompiledFloat64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	if node.decBuiltinSlice {
		return decodeCompiledBuiltinFloat64Slice(cursor, node, (*[]float64)(dst))
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	if header.cap == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			header.grow(capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				return err
			}
			if !more {
				if index == 0 {
					setTypedEmptySlice(node, dst)
				}
				return nil
			}
		}
		if index == header.cap {
			header.grow(nextTypedSliceCapacity(header.cap, index+1))
		}
		header.setLen(index + 1)
		element := (*float64)(unsafe.Add(header.data, uintptr(index)*node.elem.size))
		if useStableNumericMethods {
			if err := cursor.Float64(element); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Float(element); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

func decodeCompiledBuiltinInt64Slice(cursor *decoderCursor, node *typedNode, target *[]int64) error {
	values := (*target)[:0]
	if cap(values) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			values = make([]int64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				*target = values
				return err
			}
			if !more {
				if index == 0 {
					values = make([]int64, 0)
				}
				*target = values
				return nil
			}
		}
		if index == cap(values) {
			next := make([]int64, index, nextTypedSliceCapacity(cap(values), index+1))
			copy(next, values)
			values = next
		}
		values = values[:index+1]
		*target = values
		if useStableNumericMethods {
			if err := cursor.Int64(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Int(&values[index]); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

func decodeCompiledBuiltinUint64Slice(cursor *decoderCursor, node *typedNode, target *[]uint64) error {
	values := (*target)[:0]
	if cap(values) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			values = make([]uint64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				*target = values
				return err
			}
			if !more {
				if index == 0 {
					values = make([]uint64, 0)
				}
				*target = values
				return nil
			}
		}
		if index == cap(values) {
			next := make([]uint64, index, nextTypedSliceCapacity(cap(values), index+1))
			copy(next, values)
			values = next
		}
		values = values[:index+1]
		*target = values
		if useStableNumericMethods {
			if err := cursor.Uint64(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Uint(&values[index]); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

func decodeCompiledBuiltinFloat64Slice(cursor *decoderCursor, node *typedNode, target *[]float64) error {
	values := (*target)[:0]
	if cap(values) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			values = make([]float64, 0, capacity)
		}
	}
	for index, first := 0, true; ; index, first = index+1, false {
		if !scalarSliceAdvance(cursor, first) {
			more, err := cursor.NextArrayElement(first)
			if err != nil {
				*target = values
				return err
			}
			if !more {
				if index == 0 {
					values = make([]float64, 0)
				}
				*target = values
				return nil
			}
		}
		if index == cap(values) {
			next := make([]float64, index, nextTypedSliceCapacity(cap(values), index+1))
			copy(next, values)
			values = next
		}
		values = values[:index+1]
		*target = values
		if useStableNumericMethods {
			if err := cursor.Float64(&values[index]); err != nil {
				return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
			}
		} else if err := cursor.Float(&values[index]); err != nil {
			return prependDecodePathIndex(retagCompiledError(err, node.elem.typ), index)
		}
	}
}

// initialScalarSliceCapacity counts the delimiters of a large top-level
// scalar array before its first allocation. Numeric and null elements cannot
// contain a comma or quote, so the count is exact for every valid input
// accepted by the fused loops. The input-derived cap keeps malformed inputs
// within the space required by a valid array of one-byte scalars. Small and
// nested slices retain ordinary growth to avoid adding a prescan to common
// record-shaped documents.
func initialScalarSliceCapacity(cursor *decoderCursor) int {
	if cursor.depth != 1 || len(cursor.src)-cursor.i < scalarSliceReserveMinBytes {
		return 0
	}
	end := len(cursor.src)
	for end > cursor.i && isJSONWhitespace(cursor.src[end-1]) {
		end--
	}
	if end <= cursor.i || cursor.src[end-1] != ']' {
		return 0
	}
	values := cursor.src[cursor.i : end-1]
	start := skipSpace(values, 0)
	if start == len(values) {
		return 0
	}
	values = values[start:]
	if bytes.IndexByte(values, '"') >= 0 {
		return 0
	}
	capacity := bytes.Count(values, []byte{','}) + 1
	if limit := (len(values) + 1) / 2; capacity > limit {
		capacity = limit
	}
	return capacity
}

// scalarSliceAdvance consumes the inline delimiter between two scalar elements:
// after the first element a comma advances to the next value. It reports whether
// it recognized and consumed a fast-path delimiter; the first element, a closing
// bracket, whitespace, and every malformed case return false so the caller
// re-enters NextArrayElement, which owns the slow scan and every error message.
// It takes no pointer into the destination, so it adds nothing to the decode's
// escape profile.
func scalarSliceAdvance(cursor *decoderCursor, first bool) bool {
	if first {
		return false
	}
	i := cursor.i
	if i >= len(cursor.src) || cursor.src[i] != ',' {
		return false
	}
	cursor.i = i + 1
	if cursor.i < len(cursor.src) && cursor.src[cursor.i] <= ' ' {
		cursor.skipSpace()
	}
	return true
}

func (cursor *decoderCursor) decodeCompiledArray(node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '[' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
		if replace {
			resetTyped(node, dst)
		}
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			// encoding/json treats null on an array as a no-op.
			if replace {
				resetTyped(node, dst)
			}
			return nil
		}
		if replace {
			resetTyped(node, dst)
		}
		if err := cursor.BeginArray(node.name); err != nil {
			return err
		}
	}
	var err error
	if node.elem.kind == typedFloat {
		if node.elem.bits == 32 {
			err = decodeCompiledFloatArray[float32](cursor, node, dst)
		} else {
			err = decodeCompiledFloatArray[float64](cursor, node, dst)
		}
		if err != nil {
			return retagCompiledError(err, node.elem.typ)
		}
		return nil
	}
	if node.decHasReceiver {
		return cursor.decodeCompiledArrayReceivers(node, dst, replace)
	}
	for index, first := 0, true; ; index, first = index+1, false {
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
			element := unsafe.Add(dst, uintptr(index)*node.elem.size)
			var elementErr error
			switch node.elem.kind {
			case typedBool:
				elementErr = cursor.Bool((*bool)(element))
			case typedString:
				elementErr = cursor.String((*string)(element))
			case typedNumber:
				elementErr = cursor.Number((*string)(element))
			case typedInt:
				if useStableNumericMethods {
					switch node.elem.bits {
					case 8:
						elementErr = cursor.Int8((*int8)(element))
					case 16:
						elementErr = cursor.Int16((*int16)(element))
					case 32:
						elementErr = cursor.Int32((*int32)(element))
					case 64:
						elementErr = cursor.Int64((*int64)(element))
					}
					break
				}
				switch node.elem.bits {
				case 8:
					elementErr = cursor.Int((*int8)(element))
				case 16:
					elementErr = cursor.Int((*int16)(element))
				case 32:
					elementErr = cursor.Int((*int32)(element))
				case 64:
					elementErr = cursor.Int((*int64)(element))
				}
			case typedUint:
				if useStableNumericMethods {
					switch node.elem.bits {
					case 8:
						elementErr = cursor.Uint8((*uint8)(element))
					case 16:
						elementErr = cursor.Uint16((*uint16)(element))
					case 32:
						elementErr = cursor.Uint32((*uint32)(element))
					case 64:
						elementErr = cursor.Uint64((*uint64)(element))
					}
					break
				}
				switch node.elem.bits {
				case 8:
					elementErr = cursor.Uint((*uint8)(element))
				case 16:
					elementErr = cursor.Uint((*uint16)(element))
				case 32:
					elementErr = cursor.Uint((*uint32)(element))
				case 64:
					elementErr = cursor.Uint((*uint64)(element))
				}
			case typedFloat:
				if useStableNumericMethods && node.elem.bits == 32 {
					elementErr = cursor.Float32((*float32)(element))
				} else if useStableNumericMethods {
					elementErr = cursor.Float64((*float64)(element))
				} else if node.elem.bits == 32 {
					elementErr = cursor.Float((*float32)(element))
				} else {
					elementErr = cursor.Float((*float64)(element))
				}
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
			case typedAny:
				elementErr = cursor.decodeCompiledAny(element)
			case typedBytes:
				elementErr = cursor.decodeCompiledBytes(node.elem, element)
			case typedUnmarshalerJSON:
				elementErr = cursor.decodeViaUnmarshaler(node.elem, element)
			case typedUnmarshalerText:
				elementErr = cursor.decodeViaTextUnmarshaler(node.elem, element)
			case typedUnmarshalerSimd:
				elementErr = cursor.decodeViaSimdHook(node.elem, element)
			case typedIface:
				elementErr = cursor.decodeCompiledIface(node.elem, element)
			default:
				elementErr = &DecodeError{Offset: cursor.i, Type: node.elem.typ, Reason: "invalid compiled operation"}
			}
			if elementErr != nil {
				if node.elem.kind <= typedFloat {
					elementErr = retagCompiledError(elementErr, node.elem.typ)
				}
				return prependDecodePathIndex(elementErr, index)
			}
		} else {
			if err := cursor.Skip(); err != nil {
				return err
			}
		}
	}
}

// decodeCompiledArrayReceivers is the uncommon fixed-array loop for element
// graphs containing standard unmarshal methods. Each element after the first
// may draw detached receivers from a typed, GC-scanned array; the ordinary
// array loop carries no receiver-batching branch.
func (cursor *decoderCursor) decodeCompiledArrayReceivers(node *typedNode, dst unsafe.Pointer, replace bool) error {
	for index, first := 0, true; ; index, first = index+1, false {
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
		if index >= node.length {
			if err := cursor.Skip(); err != nil {
				return err
			}
			continue
		}

		element := unsafe.Add(dst, uintptr(index)*node.elem.size)
		batchedReceivers := index > 0 && cursor.beginReceiverBatch()
		elementErr := cursor.decodeCompiled(node.elem, element)
		cursor.endReceiverBatch(batchedReceivers)
		if elementErr != nil {
			return prependDecodePathIndex(elementErr, index)
		}
	}
}

// decodeCompiledArrayStructural keeps the structural path branch-free at the
// operation level. Its scalar loops consume exact token positions, while
// nested containers remain on the same forward structural cursor.
func (cursor *decoderCursor) decodeCompiledArrayStructural(node *typedNode, dst unsafe.Pointer) error {
	replace := cursor.flags&decoderReplace != 0
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '[' && cursor.depth < cursor.maxDepth {
		cursor.depth++
		cursor.i = i + 1
	} else {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			if replace {
				resetTyped(node, dst)
			}
			return nil
		}
		if replace {
			resetTyped(node, dst)
		}
		if err := cursor.BeginArray(node.name); err != nil {
			return err
		}
	}
	if replace {
		resetTyped(node, dst)
	}
	if !cursor.structuralFirstValueGapOK() {
		return cursor.err(cursor.i, "unexpected colon after array opener")
	}
	var err error
	if node.elem.kind == typedFloat {
		if node.elem.bits == 32 {
			err = decodeCompiledFloatArrayStructural[float32](cursor, node, dst)
		} else if node.length == 3 {
			err = decodeCompiledFloat64Array3Structural(cursor, node, dst)
		} else {
			err = decodeCompiledFloat64ArrayStructural(cursor, node, dst)
		}
		if err != nil {
			return retagCompiledError(err, node.elem.typ)
		}
		return nil
	}
	for index, first := 0, true; ; index, first = index+1, false {
		var more bool
		var err error
		switch cursor.nextArrayElementExact(first) {
		case structuralArrayValue:
			more = true
		case structuralArrayEnd:
			more = false
		default:
			var handled bool
			more, handled, err = cursor.nextArrayElementStructural(first)
			if !handled {
				more, err = cursor.NextArrayElement(first)
			}
		}
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
			element := unsafe.Add(dst, uintptr(index)*node.elem.size)
			var elementErr error
			switch node.elem.kind {
			case typedBool:
				elementErr = cursor.Bool((*bool)(element))
			case typedString:
				elementErr = cursor.stringStructural((*string)(element))
			case typedNumber:
				elementErr = cursor.Number((*string)(element))
			case typedInt:
				if useStableNumericMethods {
					switch node.elem.bits {
					case 8:
						elementErr = cursor.Int8((*int8)(element))
					case 16:
						elementErr = cursor.Int16((*int16)(element))
					case 32:
						elementErr = cursor.Int32((*int32)(element))
					case 64:
						elementErr = cursor.Int64((*int64)(element))
					}
					break
				}
				switch node.elem.bits {
				case 8:
					elementErr = cursor.Int((*int8)(element))
				case 16:
					elementErr = cursor.Int((*int16)(element))
				case 32:
					elementErr = cursor.Int((*int32)(element))
				case 64:
					elementErr = cursor.Int((*int64)(element))
				}
			case typedUint:
				if useStableNumericMethods {
					switch node.elem.bits {
					case 8:
						elementErr = cursor.Uint8((*uint8)(element))
					case 16:
						elementErr = cursor.Uint16((*uint16)(element))
					case 32:
						elementErr = cursor.Uint32((*uint32)(element))
					case 64:
						elementErr = cursor.Uint64((*uint64)(element))
					}
					break
				}
				switch node.elem.bits {
				case 8:
					elementErr = cursor.Uint((*uint8)(element))
				case 16:
					elementErr = cursor.Uint((*uint16)(element))
				case 32:
					elementErr = cursor.Uint((*uint32)(element))
				case 64:
					elementErr = cursor.Uint((*uint64)(element))
				}
			case typedStruct:
				elementErr = cursor.decodeCompiledStructStructural(node.elem, element)
			case typedSlice:
				elementErr = cursor.decodeCompiledSliceStructural(node.elem, element)
			case typedArray:
				elementErr = cursor.decodeCompiledArrayStructural(node.elem, element)
			case typedPointer:
				elementErr = cursor.decodeCompiledPointer(node.elem, element)
			case typedMap:
				elementErr = cursor.decodeCompiledMap(node.elem, element)
			case typedAny:
				elementErr = cursor.decodeCompiledAny(element)
			case typedBytes:
				elementErr = cursor.decodeCompiledBytes(node.elem, element)
			case typedUnmarshalerJSON:
				elementErr = cursor.decodeViaUnmarshaler(node.elem, element)
			case typedUnmarshalerText:
				elementErr = cursor.decodeViaTextUnmarshaler(node.elem, element)
			case typedUnmarshalerSimd:
				elementErr = cursor.decodeViaSimdHook(node.elem, element)
			case typedIface:
				elementErr = cursor.decodeCompiledIface(node.elem, element)
			default:
				elementErr = &DecodeError{Offset: cursor.i, Type: node.elem.typ, Reason: "invalid compiled operation"}
			}
			if elementErr != nil {
				if node.elem.kind <= typedFloat {
					elementErr = retagCompiledError(elementErr, node.elem.typ)
				}
				return prependDecodePathIndex(elementErr, index)
			}
		} else {
			if err := cursor.Skip(); err != nil {
				return err
			}
		}
		cursor.syncStructuralValue()
	}
}

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
	base := unsafe.Pointer(unsafe.SliceData(src))
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
	base := unsafe.Pointer(unsafe.SliceData(src))
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
	base := unsafe.Pointer(unsafe.SliceData(src))
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
	base := unsafe.Pointer(unsafe.SliceData(src))
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
