package simdjson

import "unsafe"

func (cursor *decoderCursor) decodeCompiledStruct(node *typedNode, dst unsafe.Pointer) error {
	if i := cursor.i; i < len(cursor.src) && cursor.src[i] == '{' && cursor.depth < cursor.maxDepth {
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
			// encoding/json treats null on a struct as a no-op.
			if cursor.flags&decoderReplace != 0 {
				resetTyped(node, dst)
			}
			return nil
		}
		if err := cursor.BeginObject(node.name); err != nil {
			return err
		}
	}
	var seen uint64
	if cursor.flags&decoderReplace != 0 && (node.inlineMap != nil || (node.allSet == 0 && len(node.fields) > 0)) {
		// The catch-all breaks the allSet shortcut: even when every declared
		// field is overwritten, stale unknown members must be cleared, so an
		// inline map forces the reset.
		resetTyped(node, dst)
	}
	position, first := 0, true
	// inlineDec stays nil until the first unknown member of a struct that
	// declares a catch-all, so structs without one add only this word.
	var inlineDec *decoderMapScratch
	for {
		var field *typedField
		var key string
		var ok, matched bool
		var err error
		if uint(position) < uint(len(node.fields)) {
			field = &node.fields[position]
			if cursor.flags&decoderExpectedSlow == 0 && cursor.matchObjectFieldExpected(first, field) {
				matched, ok = true, true
			} else {
				cursor.flags |= decoderExpectedSlow
				key, matched, ok, err = cursor.nextObjectFieldExpectedSlow(first, field)
			}
		} else {
			key, ok, err = cursor.NextObjectField(first)
		}
		if err != nil {
			return err
		}
		if !ok {
			releaseInlineMapScratch(inlineDec)
			if cursor.flags&decoderReplace != 0 {
				resetMissingTypedFields(node, dst, seen)
			}
			return nil
		}
		first = false
		if matched {
			position++
		} else {
			field = node.findFieldSlow(key, !cursor.CaseSensitive())
			if field == nil {
				if node.inlineMap != nil {
					// The catch-all consumes the member, so it is never
					// "unknown" and DisallowUnknownFields does not apply.
					if inlineDec == nil {
						inlineDec = cursor.takeInlineDecoder(node.inlineMap)
					}
					if err := inlineDec.decodeInlineEntry(cursor, node.inlineMap, dst, key); err != nil {
						return prependDecodePathField(err, key)
					}
					continue
				}
				if err := cursor.Unknown(node.name, key); err != nil {
					return err
				}
				continue
			}
			// Resume expected-key matching after the matched field, so
			// documents whose member order is shifted from the struct order
			// (extra members, rotations) recover the fast path.
			position = int(field.pos) + 1
		}
		seen |= field.seen
		fieldNode := field.node
		fieldBase := dst
		if field.hop >= 0 {
			resolved, hopErr := resolveDecodeHops(dst, node.fieldHops[field.hop], cursor.i)
			if hopErr != nil {
				return prependDecodePathField(hopErr, field.name)
			}
			fieldBase = resolved
		}
		fieldDst := unsafe.Add(fieldBase, field.offset)
		var fieldErr error
		switch field.op {
		// BEGIN GENERATED TYPED CURSOR FIELD DISPATCH
		case typedOpBool:
			fieldErr = cursor.Bool((*bool)(fieldDst))
		case typedOpString:
			fieldErr = cursor.String((*string)(fieldDst))
		case typedOpNumber:
			fieldErr = cursor.Number((*string)(fieldDst))
		case typedOpInt8:
			if useStableNumericMethods {
				fieldErr = cursor.Int8((*int8)(fieldDst))
			} else {
				fieldErr = cursor.Int((*int8)(fieldDst))
			}
		case typedOpInt16:
			if useStableNumericMethods {
				fieldErr = cursor.Int16((*int16)(fieldDst))
			} else {
				fieldErr = cursor.Int((*int16)(fieldDst))
			}
		case typedOpInt32:
			if useStableNumericMethods {
				fieldErr = cursor.Int32((*int32)(fieldDst))
			} else {
				fieldErr = cursor.Int((*int32)(fieldDst))
			}
		case typedOpInt64:
			if useStableNumericMethods {
				fieldErr = cursor.Int64((*int64)(fieldDst))
			} else {
				fieldErr = cursor.Int((*int64)(fieldDst))
			}
		case typedOpUint8:
			if useStableNumericMethods {
				fieldErr = cursor.Uint8((*uint8)(fieldDst))
			} else {
				fieldErr = cursor.Uint((*uint8)(fieldDst))
			}
		case typedOpUint16:
			if useStableNumericMethods {
				fieldErr = cursor.Uint16((*uint16)(fieldDst))
			} else {
				fieldErr = cursor.Uint((*uint16)(fieldDst))
			}
		case typedOpUint32:
			if useStableNumericMethods {
				fieldErr = cursor.Uint32((*uint32)(fieldDst))
			} else {
				fieldErr = cursor.Uint((*uint32)(fieldDst))
			}
		case typedOpUint64:
			if useStableNumericMethods {
				fieldErr = cursor.Uint64((*uint64)(fieldDst))
			} else {
				fieldErr = cursor.Uint((*uint64)(fieldDst))
			}
		case typedOpFloat32:
			if useStableNumericMethods {
				fieldErr = cursor.Float32((*float32)(fieldDst))
			} else {
				fieldErr = cursor.Float((*float32)(fieldDst))
			}
		case typedOpFloat64:
			if useStableNumericMethods {
				fieldErr = cursor.Float64((*float64)(fieldDst))
			} else {
				fieldErr = cursor.Float((*float64)(fieldDst))
			}
		case typedOpStruct:
			fieldErr = cursor.decodeCompiledStruct(fieldNode, fieldDst)
		case typedOpSlice:
			fieldErr = cursor.decodeCompiledSlice(fieldNode, fieldDst)
		case typedOpArray:
			fieldErr = cursor.decodeCompiledArray(fieldNode, fieldDst)
		case typedOpPointer:
			fieldErr = cursor.decodeCompiledPointer(fieldNode, fieldDst)
		case typedOpMap:
			fieldErr = cursor.decodeCompiledMap(fieldNode, fieldDst)
		case typedOpAny:
			fieldErr = cursor.decodeCompiledAny(fieldDst)
		case typedOpBytes:
			fieldErr = cursor.decodeCompiledBytes(fieldNode, fieldDst)
		case typedOpQuoted:
			fieldErr = cursor.decodeQuotedField(fieldNode, fieldDst)
		case typedOpUnmarshaler:
			switch fieldNode.kind {
			case typedUnmarshalerJSON:
				fieldErr = cursor.decodeViaUnmarshaler(fieldNode, fieldDst)
			case typedUnmarshalerSimd:
				fieldErr = cursor.decodeViaSimdHook(fieldNode, fieldDst)
			default:
				fieldErr = cursor.decodeViaTextUnmarshaler(fieldNode, fieldDst)
			}
		case typedOpIface:
			fieldErr = cursor.decodeCompiledIface(fieldNode, fieldDst)
		// END GENERATED TYPED CURSOR FIELD DISPATCH
		default:
			fieldErr = &DecodeError{Offset: cursor.i, Type: fieldNode.typ, Reason: "invalid compiled operation"}
		}
		if fieldErr != nil {
			if field.op > typedOpInvalid && field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
	}
}
