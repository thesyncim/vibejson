package vibejson

import "unsafe"

// decodeCompiledStructWide is the Replace-only body for records that need an
// operation-local presence set: records wider than one word, and uncommon
// records with pre-decode ignored-field resets. CompileDecoder retains the
// scratch, so a warm decode tracks every field without an allocation. Keeping
// this body separate leaves the ordinary field loop and epilogue unchanged.
func (cursor *decoderCursor) decodeCompiledStructWide(node *typedNode, dst unsafe.Pointer) error {
	seen, scratch := cursor.takeWideSeen(len(node.fields))
	defer cursor.releaseWideSeen(scratch)

	position, first := 0, true
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
			cursor.resetMissingTypedFieldsWide(node, dst, seen)
			cursor.resetMissingInlineMap(node, dst, inlineDec != nil)
			releaseInlineMapScratch(inlineDec)
			return nil
		}
		first = false
		if matched {
			position++
		} else {
			field = node.findFieldSlow(key, !cursor.CaseSensitive())
			if field == nil {
				if node.inlineMap != nil {
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
			position = int(field.pos) + 1
		}

		fieldPosition := int(field.pos)
		seen[fieldPosition>>6] |= uint64(1) << (fieldPosition & 63)
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
		fieldErr := cursor.decodeCompiledStructWideField(field, fieldNode, fieldDst)
		if fieldErr != nil {
			if field.op > typedOpInvalid && field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
	}
}

func (cursor *decoderCursor) decodeCompiledStructWideField(field *typedField, fieldNode *typedNode, fieldDst unsafe.Pointer) error {
	switch field.op {
	case typedOpBool:
		return cursor.Bool((*bool)(fieldDst))
	case typedOpString:
		return cursor.String((*string)(fieldDst))
	case typedOpNumber:
		return cursor.Number((*string)(fieldDst))
	case typedOpInt8:
		if useStableNumericMethods {
			return cursor.Int8((*int8)(fieldDst))
		}
		return cursor.Int((*int8)(fieldDst))
	case typedOpInt16:
		if useStableNumericMethods {
			return cursor.Int16((*int16)(fieldDst))
		}
		return cursor.Int((*int16)(fieldDst))
	case typedOpInt32:
		if useStableNumericMethods {
			return cursor.Int32((*int32)(fieldDst))
		}
		return cursor.Int((*int32)(fieldDst))
	case typedOpInt64:
		if useStableNumericMethods {
			return cursor.Int64((*int64)(fieldDst))
		}
		return cursor.Int((*int64)(fieldDst))
	case typedOpUint8:
		if useStableNumericMethods {
			return cursor.Uint8((*uint8)(fieldDst))
		}
		return cursor.Uint((*uint8)(fieldDst))
	case typedOpUint16:
		if useStableNumericMethods {
			return cursor.Uint16((*uint16)(fieldDst))
		}
		return cursor.Uint((*uint16)(fieldDst))
	case typedOpUint32:
		if useStableNumericMethods {
			return cursor.Uint32((*uint32)(fieldDst))
		}
		return cursor.Uint((*uint32)(fieldDst))
	case typedOpUint64:
		if useStableNumericMethods {
			return cursor.Uint64((*uint64)(fieldDst))
		}
		return cursor.Uint((*uint64)(fieldDst))
	case typedOpFloat32:
		if useStableNumericMethods {
			return cursor.Float32((*float32)(fieldDst))
		}
		return cursor.Float((*float32)(fieldDst))
	case typedOpFloat64:
		if useStableNumericMethods {
			return cursor.Float64((*float64)(fieldDst))
		}
		return cursor.Float((*float64)(fieldDst))
	case typedOpStruct:
		return cursor.decodeCompiledStruct(fieldNode, fieldDst)
	case typedOpSlice:
		return cursor.decodeCompiledSlice(fieldNode, fieldDst)
	case typedOpArray:
		return cursor.decodeCompiledArray(fieldNode, fieldDst)
	case typedOpPointer:
		return cursor.decodeCompiledPointer(fieldNode, fieldDst)
	case typedOpMap:
		return cursor.decodeCompiledMap(fieldNode, fieldDst)
	case typedOpAny:
		return cursor.decodeCompiledAny(fieldDst)
	case typedOpBytes:
		return cursor.decodeCompiledBytes(fieldNode, fieldDst)
	case typedOpQuoted:
		return cursor.decodeQuotedField(fieldNode, fieldDst)
	case typedOpUnmarshaler:
		switch fieldNode.kind {
		case typedUnmarshalerJSON:
			return cursor.decodeViaUnmarshaler(fieldNode, fieldDst)
		case typedUnmarshalerSimd:
			return cursor.decodeViaSimdHook(fieldNode, fieldDst)
		default:
			return cursor.decodeViaTextUnmarshaler(fieldNode, fieldDst)
		}
	case typedOpIface:
		return cursor.decodeCompiledIface(fieldNode, fieldDst)
	default:
		return cursor.decodeCompiled(fieldNode, fieldDst)
	}
}
