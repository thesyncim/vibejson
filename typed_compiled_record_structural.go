package vibejson

import (
	"encoding/binary"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
)

// decodeCompiledStructStructural is the On-Demand sibling of
// decodeCompiledStruct. Keeping a separate loop means compact inputs retain
// the original packed-key loop without a per-field mode branch.
func (cursor *decoderCursor) decodeCompiledStructStructural(node *typedNode, dst unsafe.Pointer) error {
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
			if cursor.flags&decoderReplace != 0 {
				cursor.clearTypedReplaceReferences(node, dst)
				resetTyped(node, dst)
			}
			return nil
		}
		if err := cursor.BeginObject(node.name); err != nil {
			return err
		}
	}
	tape := &cursor.state.structural
	if !(tape.index+1 < len(tape.positions) && int(tape.positions[tape.index+1]) == cursor.i) &&
		!cursor.structuralFirstValueGapOK() {
		return cursor.err(cursor.i, "unexpected colon after object opener")
	}
	if cursor.flags&decoderReplace != 0 {
		if len(node.hopResets) != 0 {
			cursor.prepareReplaceHops(node, dst)
		}
		if node.allSet == 0 && len(node.fields) > 0 {
			resetTyped(node, dst)
		}
	}
	if node.structuralFast {
		if cursor.flags&decoderReplace == 0 {
			switch node.decShapeKind() {
			case typedDecShapeRecord:
				return cursor.decodeCompiledStructStructuralRecord(node, dst)
			}
		}
		return cursor.decodeCompiledStructStructuralExpected(node, dst)
	}
	return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
}

func (cursor *decoderCursor) decodeCompiledStructStructuralExpected(node *typedNode, dst unsafe.Pointer) error {
	var seen uint64
	first := true
	for position := range node.fields {
		field := &node.fields[position]
		status := uint8(structuralFieldSlow)
		if first {
			status = cursor.matchFirstObjectFieldStructuralExpected(field)
		} else {
			status = cursor.matchNextObjectFieldStructuralExpected(field)
		}
		switch status {
		case structuralFieldMatched:
		case structuralFieldEnd:
			if cursor.flags&decoderReplace != 0 {
				cursor.resetMissingTypedFields(node, dst, seen)
			}
			return nil
		default:
			return cursor.decodeCompiledStructStructuralSlow(node, dst, position, first, seen)
		}
		first = false
		seen |= field.seen
		fieldNode := field.node
		fieldDst := unsafe.Add(dst, field.offset)
		var fieldErr error
		switch field.op {
		case typedOpBool:
			i := cursor.i
			if i < len(cursor.src) && cursor.src[i] == 't' && literalTrueAt(cursor.src, i) {
				*(*bool)(fieldDst) = true
				cursor.i = i + 4
			} else if i < len(cursor.src) && cursor.src[i] == 'f' && literalFalseTailAt(cursor.src, i) {
				*(*bool)(fieldDst) = false
				cursor.i = i + 5
			} else {
				fieldErr = cursor.Bool((*bool)(fieldDst))
			}
		case typedOpString:
			i := cursor.i
			tape := &cursor.state.structural
			token := tape.index
			positions := tape.positions
			if i < len(cursor.src) && cursor.src[i] == '"' && uint(token+1) < uint(len(positions)) &&
				int(positions[token]) == i {
				entry := positions[token+1]
				end := int(entry)
				if (!tape.nonASCII && !tape.escaped || cursor.structuralStringLocallyDirect(i+1, end)) &&
					end < len(cursor.src) && cursor.src[end] == '"' &&
					cursor.flags&decoderZeroCopy != 0 {
					tape.index = token + 1
					*(*string)(fieldDst) = byteview.String(cursor.src[i+1 : end])
					cursor.i = end + 1
					break
				}
			}
			fieldErr = cursor.stringStructural((*string)(fieldDst))
		case typedOpInt64:
			src := cursor.src
			i := uint(cursor.i)
			n := uint(len(src))
			end := i
			value := uint64(0)
			for end < n && end-i < 9 && IsDigit(src[end]) {
				value = value*10 + uint64(src[end]-'0')
				end++
			}
			if end != i && (src[i] != '0' || end == i+1) &&
				(end == n || !IsDigit(src[end])) {
				*(*int64)(fieldDst) = int64(value)
				cursor.i = int(end)
			} else {
				fieldErr = cursor.Int((*int64)(fieldDst))
			}
		case typedOpFloat64:
			fieldErr = cursor.Float((*float64)(fieldDst))
		case typedOpStruct:
			fieldErr = cursor.decodeCompiledStructStructural(fieldNode, fieldDst)
		case typedOpSlice:
			fieldErr = cursor.decodeCompiledSliceStructural(fieldNode, fieldDst)
		case typedOpArray:
			fieldErr = cursor.decodeCompiledArrayStructural(fieldNode, fieldDst)
		}
		if fieldErr != nil {
			if field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
	}
	if cursor.finishObjectStructural(first) {
		return nil
	}
	return cursor.decodeCompiledStructStructuralSlow(node, dst, len(node.fields), first, seen)
}

//go:noinline
func compiledStructuralFieldError(err error, field *typedField, retag bool) error {
	if retag {
		err = retagCompiledError(err, field.node.typ)
	}
	return prependDecodePathField(err, field.name)
}

// matchRecordFieldKey checks one expected key (keyLen <= 7) at openPos using a
// single window check so Go inlines it (cost <= 80) and BCE proves every load.
func matchRecordFieldKey(src []byte, openPos, valPos uint, field *typedField) bool {
	if openPos+11 > uint(len(src)) {
		return false
	}
	w := src[openPos:]
	klen := uint(field.keyLen)
	gap := valPos - openPos - klen
	return w[0] == '"' &&
		(binary.LittleEndian.Uint64(w[1:])^field.key)&field.keyMask == 0 &&
		(klen != 7 || w[9] == ':') &&
		(gap == 3 || gap == 4 && w[3+klen] <= ' ')
}

func (cursor *decoderCursor) decodeCompiledStructStructuralRecord(node *typedNode, dst unsafe.Pointer) error {
	tape := &cursor.state.structural
	positions := tape.positions
	token := tape.index
	if uint(token) >= uint(len(positions)) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	pos := positions[token:]
	src := cursor.src
	n := len(src)
	cleanStrings := !tape.nonASCII && !tape.escaped
	zeroCopy := cursor.flags&decoderZeroCopy != 0

	// Field 0: int64
	first := &node.fields[0]
	if len(pos) < 4 {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	_ = pos[3]
	if !tape.gapClean(src, cursor.i, int(pos[1])) || !matchRecordFieldKey(src, uint(pos[1]), uint(pos[3]), first) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	i := int(pos[3])
	token += 3
	pos = pos[3:]
	firstDst := unsafe.Add(dst, first.offset)
	end := i
	value := uint64(0)
	fastInt := false
	if len(pos) > 1 {
		end = int(pos[1])
		width := end - i
		if uint(width-1) < 4 && uint(i) < uint(n) && len(src)-i >= 4 {
			word := binary.LittleEndian.Uint32(src[i:])
			d0 := uint32(byte(word)) - '0'
			d1 := uint32(byte(word>>8)) - '0'
			d2 := uint32(byte(word>>16)) - '0'
			d3 := uint32(byte(word>>24)) - '0'
			switch width {
			case 1:
				fastInt = d0 <= 9
				value = uint64(d0)
			case 2:
				fastInt = d0 != 0 && d0 <= 9 && d1 <= 9
				value = uint64(d0*10 + d1)
			case 3:
				fastInt = d0 != 0 && d0 <= 9 && d1 <= 9 && d2 <= 9
				value = uint64(d0*100 + d1*10 + d2)
			case 4:
				fastInt = d0 != 0 && d0 <= 9 && d1 <= 9 && d2 <= 9 && d3 <= 9
				value = uint64(d0*1000 + d1*100 + d2*10 + d3)
			}
		}
	}
	if !fastInt {
		end = i
		value = 0
		for end < n && end-i < 9 && IsDigit(src[end]) {
			value = value*10 + uint64(src[end]-'0')
			end++
		}
		fastInt = end != i && (src[i] != '0' || end == i+1) &&
			(end == n || !IsDigit(src[end]))
	}
	if fastInt {
		*(*int64)(firstDst) = int64(value)
		i = end
	} else {
		tape.index = token
		cursor.i = i
		if err := cursor.Int((*int64)(firstDst)); err != nil {
			return compiledStructuralFieldError(err, first, true)
		}
		i = cursor.i
		token = tape.index
		pos = positions[token:]
	}

	// Field 1: bool
	second := &node.fields[1]
	if len(pos) < 5 || uint(i) >= uint(n) || int(pos[1]) != i || src[i] != ',' {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 1, false, 0)
	}
	_ = pos[4]
	if !tape.gapClean(src, i+1, int(pos[2])) || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), second) {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 1, false, 0)
	}
	i = int(pos[4])
	token += 4
	pos = pos[4:]
	secondDst := unsafe.Add(dst, second.offset)
	if len(src)-i >= 4 && src[i] == 't' && binary.LittleEndian.Uint32(src[i:]) == wordTrueLE {
		*(*bool)(secondDst) = true
		i += 4
	} else if len(src)-i >= 5 && src[i] == 'f' && binary.LittleEndian.Uint32(src[i+1:]) == wordAlseLE {
		*(*bool)(secondDst) = false
		i += 5
	} else {
		tape.index = token
		cursor.i = i
		if err := cursor.Bool((*bool)(secondDst)); err != nil {
			return compiledStructuralFieldError(err, second, true)
		}
		i = cursor.i
		token = tape.index
		pos = positions[token:]
	}

	// Field 2: string
	third := &node.fields[2]
	if len(pos) < 5 || uint(i) >= uint(n) || int(pos[1]) != i || src[i] != ',' {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 2, false, 0)
	}
	_ = pos[4]
	if !tape.gapClean(src, i+1, int(pos[2])) || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), third) {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 2, false, 0)
	}
	i = int(pos[4])
	token += 4
	pos = pos[4:]
	thirdDst := unsafe.Add(dst, third.offset)
	if i < n && src[i] == '"' && len(pos) > 1 {
		strEnd := int(pos[1])
		if (cleanStrings || cursor.structuralStringLocallyDirect(i+1, strEnd)) &&
			uint(strEnd) < uint(n) && src[strEnd] == '"' {
			token++
			pos = pos[1:]
			if zeroCopy {
				*(*string)(thirdDst) = byteview.String(src[i+1 : strEnd])
			} else if i+1 == strEnd {
				*(*string)(thirdDst) = ""
			} else {
				need := strEnd - (i + 1)
				if cap(cursor.strings)-len(cursor.strings) < need {
					cursor.ensureStringArenaCapacity(need)
				}
				off := len(cursor.strings)
				cursor.strings = append(cursor.strings, src[i+1:strEnd]...)
				*(*string)(thirdDst) = OwnedBytesString(cursor.strings[off:])
			}
			i = strEnd + 1
			goto field3
		}
	}
	tape.index = token
	cursor.i = i
	if err := cursor.stringStructural((*string)(thirdDst)); err != nil {
		return compiledStructuralFieldError(err, third, true)
	}
	i = cursor.i
	token = tape.index
	pos = positions[token:]

field3:
	// Field 3: string
	fourth := &node.fields[3]
	if len(pos) < 5 || uint(i) >= uint(n) || int(pos[1]) != i || src[i] != ',' {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 3, false, 0)
	}
	_ = pos[4]
	if !tape.gapClean(src, i+1, int(pos[2])) || !matchRecordFieldKey(src, uint(pos[2]), uint(pos[4]), fourth) {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 3, false, 0)
	}
	i = int(pos[4])
	token += 4
	pos = pos[4:]
	fourthDst := unsafe.Add(dst, fourth.offset)
	if i < n && src[i] == '"' && len(pos) > 1 {
		strEnd := int(pos[1])
		if (cleanStrings || cursor.structuralStringLocallyDirect(i+1, strEnd)) &&
			uint(strEnd) < uint(n) && src[strEnd] == '"' {
			token++
			pos = pos[1:]
			if zeroCopy {
				*(*string)(fourthDst) = byteview.String(src[i+1 : strEnd])
			} else if i+1 == strEnd {
				*(*string)(fourthDst) = ""
			} else {
				need := strEnd - (i + 1)
				if cap(cursor.strings)-len(cursor.strings) < need {
					cursor.ensureStringArenaCapacity(need)
				}
				off := len(cursor.strings)
				cursor.strings = append(cursor.strings, src[i+1:strEnd]...)
				*(*string)(fourthDst) = OwnedBytesString(cursor.strings[off:])
			}
			i = strEnd + 1
			goto field4
		}
	}
	tape.index = token
	cursor.i = i
	if err := cursor.stringStructural((*string)(fourthDst)); err != nil {
		return compiledStructuralFieldError(err, fourth, true)
	}
	i = cursor.i
	token = tape.index
	pos = positions[token:]

field4:
	// Field 4: array
	fifth := &node.fields[4]
	if len(pos) < 5 || uint(i) >= uint(n) || int(pos[1]) != i || src[i] != ',' {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 4, false, 0)
	}
	_ = pos[4]
	valPos := int(pos[4])
	if !tape.gapClean(src, i+1, int(pos[2])) || !matchRecordFieldKey(src, uint(pos[2]), uint(valPos), fifth) {
		tape.index = token
		cursor.i = i
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 4, false, 0)
	}
	fifthDst := unsafe.Add(dst, fifth.offset)
	if fifth.node.length == 3 && fifth.node.elem.op == typedOpFloat64 &&
		uint(valPos) < uint(n) && src[valPos] == '[' && cursor.depth < cursor.maxDepth && len(pos) >= 12 {
		_ = pos[11]
		f0 := uint(pos[5])
		c1 := uint(pos[6])
		f1 := uint(pos[7])
		c2 := uint(pos[8])
		f2 := uint(pos[9])
		arrClose := uint(pos[10])
		objClose := uint(pos[11])
		if objClose < uint(n) && f0 == uint(valPos)+1 &&
			c1 < arrClose && c2 < arrClose && arrClose < objClose &&
			src[c1] == ',' && src[c2] == ',' && src[arrClose] == ']' &&
			tape.gapClean(src, int(c1)+1, int(f1)) && tape.gapClean(src, int(c2)+1, int(f2)) &&
			arrClose+1 == objClose && src[objClose] == '}' {
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
			if ok0 && ok1 && ok2 {
				values := (*[3]float64)(fifthDst)
				values[0] = v0
				values[1] = v1
				values[2] = v2
				tape.index = token + 11
				cursor.i = int(objClose) + 1
				cursor.depth--
				return nil
			}
		}
	}
	tape.index = token + 4
	cursor.i = valPos
	if fifth.node.length == 3 && fifth.node.elem.op == typedOpFloat64 &&
		valPos < n && src[valPos] == '[' && cursor.depth < cursor.maxDepth &&
		(len(pos) > 5 && int(pos[5]) == valPos+1 || cursor.structuralFirstValueGapOK()) {
		cursor.depth++
		cursor.i = valPos + 1
		if err := decodeCompiledFloat64Array3Structural(cursor, fifth.node, fifthDst); err != nil {
			return compiledStructuralFieldError(err, fifth, false)
		}
	} else if err := cursor.decodeCompiledArrayStructural(fifth.node, fifthDst); err != nil {
		return compiledStructuralFieldError(err, fifth, false)
	}
	if cursor.finishObjectStructural(false) {
		return nil
	}
	return cursor.decodeCompiledStructStructuralSlow(node, dst, 5, false, 0)
}

func (cursor *decoderCursor) decodeCompiledStructStructuralSlow(node *typedNode, dst unsafe.Pointer, position int, first bool, seen uint64) error {
	var inlineDec *decoderMapScratch
	for {
		var field *typedField
		if uint(position) < uint(len(node.fields)) {
			field = &node.fields[position]
		}
		var key string
		var matched, ok bool
		var err error
		stepKey, status := cursor.matchObjectFieldStructural(first, field)
		switch status {
		case structuralFieldMatched:
			matched, ok = true, true
		case structuralFieldKey:
			key, ok = stepKey, true
		case structuralFieldEnd:
			ok = false
		default:
			var handled bool
			key, matched, ok, handled, err = cursor.nextObjectFieldStructural(first, field)
			if !handled {
				key, matched, ok, err = cursor.nextObjectFieldRawStructural(first, field)
			}
		}
		if err != nil {
			return err
		}
		if !ok {
			if cursor.flags&decoderReplace != 0 {
				cursor.resetMissingTypedFields(node, dst, seen)
			}
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
						seen |= typedSeenInlineMap
					}
					if err := inlineDec.decodeInlineEntry(cursor, node.inlineMap, dst, key); err != nil {
						return prependDecodePathField(err, key)
					}
					cursor.syncStructuralValue()
					continue
				}
				if err := cursor.Unknown(node.name, key); err != nil {
					return err
				}
				cursor.syncStructuralValue()
				continue
			}
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
		// BEGIN GENERATED TYPED STRUCTURAL FIELD DISPATCH
		case typedOpBool:
			fieldErr = cursor.Bool((*bool)(fieldDst))
		case typedOpString:
			fieldErr = cursor.stringStructural((*string)(fieldDst))
		case typedOpNumber:
			fieldErr = cursor.Number((*string)(fieldDst))
		case typedOpInt8:
			fieldErr = cursor.Int((*int8)(fieldDst))
		case typedOpInt16:
			fieldErr = cursor.Int((*int16)(fieldDst))
		case typedOpInt32:
			fieldErr = cursor.Int((*int32)(fieldDst))
		case typedOpInt64:
			fieldErr = cursor.Int((*int64)(fieldDst))
		case typedOpUint8:
			fieldErr = cursor.Uint((*uint8)(fieldDst))
		case typedOpUint16:
			fieldErr = cursor.Uint((*uint16)(fieldDst))
		case typedOpUint32:
			fieldErr = cursor.Uint((*uint32)(fieldDst))
		case typedOpUint64:
			fieldErr = cursor.Uint((*uint64)(fieldDst))
		case typedOpFloat32:
			fieldErr = cursor.Float((*float32)(fieldDst))
		case typedOpFloat64:
			fieldErr = cursor.Float((*float64)(fieldDst))
		case typedOpStruct:
			fieldErr = cursor.decodeCompiledStructStructural(fieldNode, fieldDst)
		case typedOpSlice:
			fieldErr = cursor.decodeCompiledSliceStructural(fieldNode, fieldDst)
		case typedOpArray:
			fieldErr = cursor.decodeCompiledArrayStructural(fieldNode, fieldDst)
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
		// END GENERATED TYPED STRUCTURAL FIELD DISPATCH
		default:
			fieldErr = cursor.decodeCompiled(fieldNode, fieldDst)
		}
		if fieldErr != nil {
			if field.op > typedOpInvalid && field.op < typedOpStruct {
				fieldErr = retagCompiledError(fieldErr, fieldNode.typ)
			}
			return prependDecodePathField(fieldErr, field.name)
		}
		cursor.syncStructuralValue()
	}
}
