package slopjson

import "unsafe"

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
