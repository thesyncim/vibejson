package slopjson

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
		element := header.elementAt(index, node.elem.size)
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
		element := header.elementAt(index, node.elem.size)
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
		element := header.elementAt(index, node.elem.size)
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
		element := (*int64)(header.elementAt(index, node.elem.size))
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
		element := (*uint64)(header.elementAt(index, node.elem.size))
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
		element := (*float64)(header.elementAt(index, node.elem.size))
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
		// Indented arrays break after every comma, so the gap is a
		// newline-and-indent run; the indent-stride skipper walks it without
		// falling back to byte steps on four-space tails.
		cursor.i = cursor.skipSpaceAt(cursor.i)
	}
	return true
}
