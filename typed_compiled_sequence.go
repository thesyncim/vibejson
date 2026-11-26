package vibejson

import (
	"bytes"
	"encoding/binary"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
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
			if node.decBuiltinSlice {
				return decodeCompiledBuiltinFloat64SliceStructural(cursor, node, (*[]float64)(dst))
			}
			return decodeCompiledFloat64Slice(cursor, node, dst)
		}
	}
	header := typedSliceAt(node.typ, dst)
	header.setLen(0)
	if elem := node.elem; elem.kind == typedStruct && elem.structuralFast &&
		cursor.flags&decoderReplace == 0 && elem.decShapeKind() == typedDecShapeRecord {
		return cursor.decodeCompiledRecordSliceStructural(node, elem, dst, &header)
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

func (cursor *decoderCursor) decodeCompiledRecordSliceStructural(node, elem *typedNode, dst unsafe.Pointer, header *typedSliceState) error {
	tape := &cursor.state.structural
	positions := tape.positions
	src := cursor.src
	n := uint(len(src))
	elemSize := elem.size
	cleanStrings := !tape.nonASCII && !tape.escaped
	zeroCopy := cursor.flags&decoderZeroCopy != 0
	f0Field := &elem.fields[0]
	f1Field := &elem.fields[1]
	f2Field := &elem.fields[2]
	f3Field := &elem.fields[3]
	f4Field := &elem.fields[4]
	inlineFast := f4Field.node.length == 3 && f4Field.node.elem.op == typedOpFloat64 && cursor.depth+1 < cursor.maxDepth
	token := tape.index
	i := uint(cursor.i)
	for index := 0; ; index++ {
		var openPos uint
		var recToken int
		if index == 0 {
			if token+1 < len(positions) {
				next := uint(positions[token+1])
				if next == i && next < n {
					c := src[next]
					if c == '{' && cursor.depth < cursor.maxDepth && token+2 < len(positions) && uint(positions[token+2]) == next+1 {
						openPos = next
						recToken = token + 1
						goto decodeRecord
					}
					if c == ']' {
						tape.index = token + 1
						cursor.i = int(next) + 1
						cursor.depth--
						setTypedEmptySlice(node, dst)
						return nil
					}
				}
			}
		} else {
			if token+1 < len(positions) {
				delim := uint(positions[token+1])
				if delim == i && delim < n {
					c := src[delim]
					if c == ',' && token+2 < len(positions) {
						op := uint(positions[token+2])
						if op == delim+1 && op < n && src[op] == '{' &&
							cursor.depth < cursor.maxDepth && token+3 < len(positions) && uint(positions[token+3]) == op+1 {
							openPos = op
							recToken = token + 2
							goto decodeRecord
						}
					} else if c == ']' {
						tape.index = token + 1
						cursor.i = int(delim) + 1
						cursor.depth--
						header.setLen(index)
						return nil
					}
				}
			}
		}
		// Fallback for whitespace/indented/null/error transitions.
		{
			tape.index = token
			cursor.i = int(i)
			var more bool
			var err error
			first := index == 0
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
				header.setLen(index)
				return err
			}
			if !more {
				if index == 0 {
					setTypedEmptySlice(node, dst)
				} else {
					header.setLen(index)
				}
				return nil
			}
			if index == header.cap {
				capacity := nextTypedSliceCapacity(header.cap, index+1)
				if header.cap == 0 && cursor.depth <= 3 {
					if estimate := (len(src) - cursor.i) / 128; estimate > capacity {
						capacity = estimate
						if capacity > 1024 {
							capacity = 1024
						}
					}
				}
				header.grow(capacity)
			}
			header.setLen(index + 1)
			if err := cursor.decodeCompiledStructStructural(elem, header.elementAt(index, elemSize)); err != nil {
				return prependDecodePathIndex(err, index)
			}
			cursor.syncStructuralValue()
			token = tape.index
			i = uint(cursor.i)
			continue
		}

	decodeRecord:
		if index == header.cap {
			capacity := nextTypedSliceCapacity(header.cap, index+1)
			if header.cap == 0 && cursor.depth <= 3 {
				if estimate := (len(src) - int(openPos+1)) / 128; estimate > capacity {
					capacity = estimate
					if capacity > 1024 {
						capacity = 1024
					}
				}
			}
			header.grow(capacity)
		}
		elemPtr := header.elementAt(index, elemSize)
		if inlineFast && recToken+28 < len(positions) {
			pos := positions[recToken:]
			// Field 0: int64
			_ = pos[4]
			if !matchRecordFieldKey(src, uint(pos[1]), uint(pos[3]), f0Field) {
				goto slowRecord
			}
			ri := uint(pos[3])
			rEnd := uint(pos[4])
			width := rEnd - ri
			var intVal uint64
			if width-1 < 4 && ri+4 <= n {
				word := binary.LittleEndian.Uint32(src[ri:])
				d0 := uint32(byte(word)) - '0'
				d1 := uint32(byte(word>>8)) - '0'
				d2 := uint32(byte(word>>16)) - '0'
				d3 := uint32(byte(word>>24)) - '0'
				switch width {
				case 1:
					if d0 > 9 {
						goto slowRecord
					}
					intVal = uint64(d0)
				case 2:
					if d0 == 0 || d0 > 9 || d1 > 9 {
						goto slowRecord
					}
					intVal = uint64(d0*10 + d1)
				case 3:
					if d0 == 0 || d0 > 9 || d1 > 9 || d2 > 9 {
						goto slowRecord
					}
					intVal = uint64(d0*100 + d1*10 + d2)
				case 4:
					if d0 == 0 || d0 > 9 || d1 > 9 || d2 > 9 || d3 > 9 {
						goto slowRecord
					}
					intVal = uint64(d0*1000 + d1*100 + d2*10 + d3)
				}
			} else {
				goto slowRecord
			}
			pos = pos[3:]

			// Field 1: bool
			_ = pos[4]
			if rEnd >= n || src[rEnd] != ',' || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), f1Field) {
				goto slowRecord
			}
			ri = uint(pos[4])
			var boolVal bool
			if ri+4 <= n && src[ri] == 't' && binary.LittleEndian.Uint32(src[ri:]) == wordTrueLE {
				boolVal = true
				ri += 4
			} else if ri+5 <= n && src[ri] == 'f' && binary.LittleEndian.Uint32(src[ri+1:]) == wordAlseLE {
				boolVal = false
				ri += 5
			} else {
				goto slowRecord
			}
			pos = pos[4:]

			// Field 2: string
			_ = pos[5]
			if ri >= n || uint(pos[1]) != ri || src[ri] != ',' || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), f2Field) {
				goto slowRecord
			}
			s2Start := uint(pos[4])
			s2End := uint(pos[5])
			if s2End >= n || s2Start >= s2End || src[s2Start] != '"' || src[s2End] != '"' ||
				(!cleanStrings && !cursor.structuralStringLocallyDirect(int(s2Start)+1, int(s2End))) {
				goto slowRecord
			}
			ri = s2End + 1
			pos = pos[5:]

			// Field 3: string
			_ = pos[5]
			if ri >= n || uint(pos[1]) != ri || src[ri] != ',' || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), f3Field) {
				goto slowRecord
			}
			s3Start := uint(pos[4])
			s3End := uint(pos[5])
			if s3End >= n || s3Start >= s3End || src[s3Start] != '"' || src[s3End] != '"' ||
				(!cleanStrings && !cursor.structuralStringLocallyDirect(int(s3Start)+1, int(s3End))) {
				goto slowRecord
			}
			ri = s3End + 1
			pos = pos[5:]

			// Field 4: [3]float64
			_ = pos[11]
			if ri >= n || uint(pos[1]) != ri || src[ri] != ',' {
				goto slowRecord
			}
			arrOpen := uint(pos[4])
			if !matchRecordFieldKey(src, uint(pos[2]), arrOpen, f4Field) {
				goto slowRecord
			}
			f0 := uint(pos[5])
			c1 := uint(pos[6])
			f1 := uint(pos[7])
			c2 := uint(pos[8])
			f2 := uint(pos[9])
			arrClose := uint(pos[10])
			objClose := uint(pos[11])
			if objClose >= n || src[arrOpen] != '[' || f0 != arrOpen+1 ||
				c1 >= arrClose || c2 >= arrClose || arrClose+1 != objClose ||
				src[c1] != ',' || src[c2] != ',' || src[arrClose] != ']' || src[objClose] != '}' {
				goto slowRecord
			}
			var v0, v1, v2 float64
			var ok0, ok1, ok2 bool
			switch c1 - f0 {
			case 1:
				if d := src[f0] - '0'; d <= 9 {
					v0, ok0 = float64(d), true
				}
			case 3:
				if d0, d2 := src[f0]-'0', src[f0+2]-'0'; src[f0+1] == '.' && d0 <= 9 && d2 <= 9 {
					v0, ok0 = float64(d0)+oneDigitFractions[d2], true
				}
			case 4:
				if d1, d3 := src[f0+1]-'0', src[f0+3]-'0'; src[f0] == '-' && d1 <= 9 && d3 <= 9 && src[f0+2]|0x20 == 'e' {
					v0, ok0 = -float64(d1)*anyPow10[d3], true
				}
			}
			if !ok0 {
				v0, ok0 = shortStructuralFloatAt(src, f0, c1)
			}
			switch c2 - f1 {
			case 1:
				if d := src[f1] - '0'; d <= 9 {
					v1, ok1 = float64(d), true
				}
			case 3:
				if d0, d2 := src[f1]-'0', src[f1+2]-'0'; src[f1+1] == '.' && d0 <= 9 && d2 <= 9 {
					v1, ok1 = float64(d0)+oneDigitFractions[d2], true
				}
			case 4:
				if d1, d3 := src[f1+1]-'0', src[f1+3]-'0'; src[f1] == '-' && d1 <= 9 && d3 <= 9 && src[f1+2]|0x20 == 'e' {
					v1, ok1 = -float64(d1)*anyPow10[d3], true
				}
			}
			if !ok1 {
				v1, ok1 = shortStructuralFloatAt(src, f1, c2)
			}
			switch arrClose - f2 {
			case 1:
				if d := src[f2] - '0'; d <= 9 {
					v2, ok2 = float64(d), true
				}
			case 3:
				if d0, d2 := src[f2]-'0', src[f2+2]-'0'; src[f2+1] == '.' && d0 <= 9 && d2 <= 9 {
					v2, ok2 = float64(d0)+oneDigitFractions[d2], true
				}
			case 4:
				if d1, d3 := src[f2+1]-'0', src[f2+3]-'0'; src[f2] == '-' && d1 <= 9 && d3 <= 9 && src[f2+2]|0x20 == 'e' {
					v2, ok2 = -float64(d1)*anyPow10[d3], true
				}
			}
			if !ok2 {
				v2, ok2 = shortStructuralFloatAt(src, f2, arrClose)
			}
			if !ok0 || !ok1 || !ok2 {
				goto slowRecord
			}

			// Commit all 5 fields cleanly (no partial mutations on fallback).
			*(*int64)(unsafe.Add(elemPtr, f0Field.offset)) = int64(intVal)
			*(*bool)(unsafe.Add(elemPtr, f1Field.offset)) = boolVal
			if zeroCopy {
				*(*string)(unsafe.Add(elemPtr, f2Field.offset)) = byteview.String(src[s2Start+1 : s2End])
				*(*string)(unsafe.Add(elemPtr, f3Field.offset)) = byteview.String(src[s3Start+1 : s3End])
			} else {
				n2 := int(s2End - s2Start - 1)
				n3 := int(s3End - s3Start - 1)
				need := n2 + n3
				if need == 0 {
					*(*string)(unsafe.Add(elemPtr, f2Field.offset)) = ""
					*(*string)(unsafe.Add(elemPtr, f3Field.offset)) = ""
				} else {
					if cap(cursor.strings)-len(cursor.strings) < need {
						cursor.ensureStringArenaCapacity(need)
					}
					off := len(cursor.strings)
					if n2 == 0 {
						*(*string)(unsafe.Add(elemPtr, f2Field.offset)) = ""
					} else {
						cursor.strings = append(cursor.strings, src[s2Start+1:s2End]...)
						*(*string)(unsafe.Add(elemPtr, f2Field.offset)) = OwnedBytesString(cursor.strings[off : off+n2])
						off += n2
					}
					if n3 == 0 {
						*(*string)(unsafe.Add(elemPtr, f3Field.offset)) = ""
					} else {
						cursor.strings = append(cursor.strings, src[s3Start+1:s3End]...)
						*(*string)(unsafe.Add(elemPtr, f3Field.offset)) = OwnedBytesString(cursor.strings[off : off+n3])
					}
				}
			}
			values := (*[3]float64)(unsafe.Add(elemPtr, f4Field.offset))
			values[0] = v0
			values[1] = v1
			values[2] = v2
			token = recToken + 28
			i = objClose + 1
			continue
		}

	slowRecord:
		tape.index = recToken
		cursor.i = int(openPos) + 1
		cursor.depth++
		if err := cursor.decodeCompiledStructStructuralRecord(elem, elemPtr); err != nil {
			header.setLen(index + 1)
			return prependDecodePathIndex(err, index)
		}
		token = tape.index
		i = uint(cursor.i)
	}
}

// Int64 and uint64 slices, plus named float64-layout slices, share DecodeArray's
// concrete loops. Built-in []float64 keeps its specialized concrete loop. The
// callers check scalar kind, 64-bit width, and eight-byte size before entering
// an adapter. Defined slice and element types have the same pointer-free element
// layout; ordinary Go slice assignment supplies the backing-pointer write
// barrier. Assign even on error to preserve the decoded prefix and failing
// element.
func decodeCompiledInt64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	target := (*[]int64)(dst)
	values, err := decodeCompiledRootInt64Slice(cursor, node.elem, *target)
	*target = values
	return err
}

func decodeCompiledUint64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	target := (*[]uint64)(dst)
	values, err := decodeCompiledRootUint64Slice(cursor, node.elem, *target)
	*target = values
	return err
}

func decodeCompiledFloat64Slice(cursor *decoderCursor, node *typedNode, dst unsafe.Pointer) error {
	target := (*[]float64)(dst)
	values, err := decodeCompiledRootFloat64Slice(cursor, node.elem, *target)
	*target = values
	return err
}

// decodeCompiledBuiltinFloat64SliceStructural consumes the stage-1 cursor
// stream already built for a large homogeneous float slice. Scalar conversion
// remains authoritative; the structural stream only batches delimiter and
// token-start discovery so the hot loop can call the exact float scanner
// directly instead of re-entering the generic cursor method for every value.
func decodeCompiledBuiltinFloat64SliceStructural(cursor *decoderCursor, node *typedNode, target *[]float64) error {
	tape := &cursor.state.structural
	positions := tape.positions
	src := cursor.src
	token := tape.index
	for token < len(positions) && int(positions[token]) < cursor.i-1 {
		token++
	}
	if token >= len(positions) || src[positions[token]] != '[' {
		values, err := decodeCompiledRootFloat64Slice(cursor, node.elem, *target)
		*target = values
		return err
	}

	values := (*target)[:0]
	if cap(values) == 0 {
		if capacity := initialScalarSliceCapacity(cursor); capacity != 0 {
			values = make([]float64, 0, capacity)
		}
	}
	for index := 0; ; index++ {
		afterComma := false
		token++
		if token >= len(positions) {
			*target = values
			return cursor.err(len(src), "unterminated array")
		}
		position := int(positions[token])
		if index != 0 {
			if src[position] == ']' {
				tape.index = token
				cursor.i = position + 1
				cursor.depth--
				*target = values
				return nil
			}
			if src[position] != ',' {
				*target = values
				return cursor.err(position, "expected comma or closing bracket in array")
			}
			afterComma = true
			comma := position
			token++
			if token >= len(positions) {
				*target = values
				return cursor.err(len(src), "unterminated array")
			}
			position = int(positions[token])
			gap := comma + 1
			for gap < position && IsJSONWhitespace(src[gap]) {
				gap++
			}
			if gap != position {
				position = gap
			}
		}
		if src[position] == ']' && !afterComma {
			tape.index = token
			cursor.i = position + 1
			cursor.depth--
			if index == 0 {
				values = make([]float64, 0)
			}
			*target = values
			return nil
		}

		tape.index = token
		cursor.i = position
		if index == cap(values) {
			next := make([]float64, index, nextTypedSliceCapacity(cap(values), index+1))
			copy(next, values)
			values = next
		}
		values = values[:index+1]
		*target = values
		element := &values[index]

		if !cursor.floatLong {
			if value, end, ok := shortTypedFloatAt(src, position); ok {
				*element = value
				cursor.i = end
				continue
			}
			cursor.floatLong = true
		}
		end, value, exact, ok := scanTypedFloat64(src, position)
		if ok && exact {
			*element = value
			cursor.i = end
			cursor.floatLong = end-position >= 6
			if typedNumberEnd(src, end) {
				continue
			}
			_, err := cursor.nextArrayElementSlow(false)
			return err
		}
		if err := cursor.Float(element); err != nil {
			return prependDecodePathIndex(err, index)
		}
		if !typedNumberEnd(src, cursor.i) {
			_, err := cursor.nextArrayElementSlow(false)
			return err
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
	for end > cursor.i && IsJSONWhitespace(cursor.src[end-1]) {
		end--
	}
	if end <= cursor.i || cursor.src[end-1] != ']' {
		return 0
	}
	values := cursor.src[cursor.i : end-1]
	start := SkipSpace(values, 0)
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
