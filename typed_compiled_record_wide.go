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
		fieldErr := cursor.decodeCompiled(fieldNode, fieldDst)
		if fieldErr != nil {
			if field.op > typedOpInvalid && field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
	}
}
