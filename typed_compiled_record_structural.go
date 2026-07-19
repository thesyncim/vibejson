package simdjson

import "unsafe"

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
				resetTyped(node, dst)
			}
			return nil
		}
		if err := cursor.BeginObject(node.name); err != nil {
			return err
		}
	}
	if !cursor.structuralFirstValueGapOK() {
		return cursor.err(cursor.i, "unexpected colon after object opener")
	}
	if cursor.flags&decoderReplace != 0 && (node.inlineMap != nil || (node.allSet == 0 && len(node.fields) > 0)) {
		resetTyped(node, dst)
	}
	if node.structuralFast {
		if cursor.flags&decoderReplace == 0 {
			switch node.decShape {
			case typedDecShapeInt64String:
				return cursor.decodeCompiledStructStructuralInt64String(node, dst)
			case typedDecShapeSliceStruct:
				return cursor.decodeCompiledStructStructuralSliceStruct(node, dst)
			case typedDecShapeRecord:
				return cursor.decodeCompiledStructStructuralRecord(node, dst)
			case typedDecShapeRecordFloat64x3:
				return cursor.decodeCompiledStructStructuralRecordFloat64x3(node, dst)
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
				resetMissingTypedFields(node, dst, seen)
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
					cursor.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
					tape.index = token + 1
					*(*string)(fieldDst) = unsafe.String(unsafe.SliceData(cursor.src[i+1:end]), end-i-1)
					cursor.i = end + 1
					break
				}
			}
			fieldErr = cursor.stringStructural((*string)(fieldDst))
		case typedOpInt64:
			i := cursor.i
			base := unsafe.Pointer(unsafe.SliceData(cursor.src))
			end := i
			value := uint64(0)
			for end < len(cursor.src) && end-i < 9 && isDigit(fastByteAt(base, end)) {
				value = value*10 + uint64(fastByteAt(base, end)-'0')
				end++
			}
			if end != i && (fastByteAt(base, i) != '0' || end == i+1) &&
				(end == len(cursor.src) || !isDigit(fastByteAt(base, end))) {
				*(*int64)(fieldDst) = int64(value)
				cursor.i = end
			} else if useStableNumericMethods {
				fieldErr = cursor.Int64((*int64)(fieldDst))
			} else {
				fieldErr = cursor.Int((*int64)(fieldDst))
			}
		case typedOpFloat64:
			if useStableNumericMethods {
				fieldErr = cursor.Float64((*float64)(fieldDst))
			} else {
				fieldErr = cursor.Float((*float64)(fieldDst))
			}
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

func (cursor *decoderCursor) decodeCompiledStructStructuralSliceStruct(node *typedNode, dst unsafe.Pointer) error {
	first := &node.fields[0]
	if !cursor.matchFirstObjectFieldStructuralShape(first) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	if err := cursor.decodeCompiledSliceStructural(first.node, unsafe.Add(dst, first.offset)); err != nil {
		return prependDecodePathField(err, first.name)
	}
	second := &node.fields[1]
	if !cursor.matchNextObjectFieldStructuralShape(second) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 1, false, 0)
	}
	if err := cursor.decodeCompiledStructStructural(second.node, unsafe.Add(dst, second.offset)); err != nil {
		return prependDecodePathField(err, second.name)
	}
	if cursor.finishObjectStructural(false) {
		return nil
	}
	return cursor.decodeCompiledStructStructuralSlow(node, dst, 2, false, 0)
}

func (cursor *decoderCursor) decodeCompiledStructStructuralInt64String(node *typedNode, dst unsafe.Pointer) error {
	first := &node.fields[0]
	if !cursor.matchFirstObjectFieldStructuralShape(first) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	firstDst := unsafe.Add(dst, first.offset)
	i := cursor.i
	base := unsafe.Pointer(unsafe.SliceData(cursor.src))
	end := i
	value := uint64(0)
	for end < len(cursor.src) && end-i < 9 && isDigit(fastByteAt(base, end)) {
		value = value*10 + uint64(fastByteAt(base, end)-'0')
		end++
	}
	var err error
	if end != i && (fastByteAt(base, i) != '0' || end == i+1) &&
		(end == len(cursor.src) || !isDigit(fastByteAt(base, end))) {
		*(*int64)(firstDst) = int64(value)
		cursor.i = end
	} else if useStableNumericMethods {
		err = cursor.Int64((*int64)(firstDst))
	} else {
		err = cursor.Int((*int64)(firstDst))
	}
	if err != nil {
		return prependDecodePathField(retagCompiledError(err, first.node.typ), first.name)
	}

	second := &node.fields[1]
	if !cursor.matchNextObjectFieldStructuralShape(second) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 1, false, 0)
	}
	secondDst := unsafe.Add(dst, second.offset)
	i = cursor.i
	tape := &cursor.state.structural
	token := tape.index
	positions := tape.positions
	fast := false
	if i < len(cursor.src) && cursor.src[i] == '"' && uint(token+1) < uint(len(positions)) &&
		int(positions[token]) == i {
		end := int(positions[token+1])
		if (!tape.nonASCII && !tape.escaped || cursor.structuralStringLocallyDirect(i+1, end)) &&
			end < len(cursor.src) && cursor.src[end] == '"' &&
			cursor.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
			tape.index = token + 1
			*(*string)(secondDst) = unsafe.String(unsafe.SliceData(cursor.src[i+1:end]), end-i-1)
			cursor.i = end + 1
			fast = true
		}
	}
	if !fast {
		if err := cursor.stringStructural((*string)(secondDst)); err != nil {
			return prependDecodePathField(retagCompiledError(err, second.node.typ), second.name)
		}
	}
	if cursor.finishObjectStructural(false) {
		return nil
	}
	return cursor.decodeCompiledStructStructuralSlow(node, dst, 2, false, 0)
}

//go:noinline
func compiledStructuralFieldError(err error, field *typedField, retag bool) error {
	if retag {
		err = retagCompiledError(err, field.node.typ)
	}
	return prependDecodePathField(err, field.name)
}

func structuralTapePosition(entries unsafe.Pointer, index int) int {
	return int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
}

func structuralPackedFieldAt(base unsafe.Pointer, openPosition, valuePosition int, expected *typedField) bool {
	keyStart := openPosition + 1
	closePosition := keyStart + int(expected.keyLen)
	gap := valuePosition - closePosition
	return (loadUint64LE(unsafe.Add(base, keyStart))^expected.key)&expected.keyMask == 0 &&
		(gap == 2 || gap == 3 && fastByteAt(base, closePosition+2) <= ' ')
}

// decodeCompiledStructStructuralRecordFloat64x3 treats the dominant compiled
// record as one stage-2 superinstruction. Every raw key, omitted colon, value
// delimiter, and token position is proved before destination or cursor state
// changes, so any uncommon shape falls through transactionally.
func (cursor *decoderCursor) decodeCompiledStructStructuralRecordFloat64x3(node *typedNode, dst unsafe.Pointer) error {
	if cursor.tryCompiledStructStructuralRecordFloat64x3(node, dst) {
		return nil
	}
	return cursor.decodeCompiledStructStructuralRecord(node, dst)
}

func (cursor *decoderCursor) tryCompiledStructStructuralRecordFloat64x3(node *typedNode, dst unsafe.Pointer) bool {
	tape := &cursor.state.structural
	positions := tape.positions
	token := tape.index
	if token < 0 || token+28 >= len(positions) || len(node.fields) != 5 ||
		cursor.flags&(decoderZeroCopy|decoderSourceOwned) == 0 {
		return false
	}

	src := cursor.src
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	{
		fields := node.fields
		openPosition := structuralTapePosition(entries, token+1)
		valuePosition := structuralTapePosition(entries, token+3)
		if fastByteAt(base, openPosition) != '"' || !structuralPackedFieldAt(base, openPosition, valuePosition, &fields[0]) ||
			fields[0].keyLen == 7 && fastByteAt(base, openPosition+9) != ':' {
			return false
		}
		openPosition = structuralTapePosition(entries, token+5)
		valuePosition = structuralTapePosition(entries, token+7)
		if fastByteAt(base, openPosition) != '"' || !structuralPackedFieldAt(base, openPosition, valuePosition, &fields[1]) ||
			fields[1].keyLen == 7 && fastByteAt(base, openPosition+9) != ':' {
			return false
		}
		openPosition = structuralTapePosition(entries, token+9)
		valuePosition = structuralTapePosition(entries, token+11)
		if fastByteAt(base, openPosition) != '"' || !structuralPackedFieldAt(base, openPosition, valuePosition, &fields[2]) ||
			fields[2].keyLen == 7 && fastByteAt(base, openPosition+9) != ':' {
			return false
		}
		openPosition = structuralTapePosition(entries, token+14)
		valuePosition = structuralTapePosition(entries, token+16)
		if fastByteAt(base, openPosition) != '"' || !structuralPackedFieldAt(base, openPosition, valuePosition, &fields[3]) ||
			fields[3].keyLen == 7 && fastByteAt(base, openPosition+9) != ':' {
			return false
		}
		openPosition = structuralTapePosition(entries, token+19)
		valuePosition = structuralTapePosition(entries, token+21)
		if fastByteAt(base, openPosition) != '"' || !structuralPackedFieldAt(base, openPosition, valuePosition, &fields[4]) ||
			fields[4].keyLen == 7 && fastByteAt(base, openPosition+9) != ':' {
			return false
		}
	}

	field0Value := structuralTapePosition(entries, token+3)
	field1Value := structuralTapePosition(entries, token+7)
	field2Value := structuralTapePosition(entries, token+11)
	nameEnd := structuralTapePosition(entries, token+12)
	field3Value := structuralTapePosition(entries, token+16)
	messageEnd := structuralTapePosition(entries, token+17)
	if (tape.nonASCII || tape.escaped) &&
		(!cursor.structuralStringLocallyDirect(field2Value+1, nameEnd) ||
			!cursor.structuralStringLocallyDirect(field3Value+1, messageEnd)) {
		return false
	}
	comma0 := structuralTapePosition(entries, token+4)
	comma1 := structuralTapePosition(entries, token+8)
	arrayComma0 := structuralTapePosition(entries, token+23)
	arrayComma1 := structuralTapePosition(entries, token+25)
	arrayEnd := structuralTapePosition(entries, token+27)
	if fastByteAt(base, comma0) != ',' || fastByteAt(base, comma1) != ',' ||
		fastByteAt(base, structuralTapePosition(entries, token+13)) != ',' ||
		fastByteAt(base, structuralTapePosition(entries, token+18)) != ',' ||
		fastByteAt(base, structuralTapePosition(entries, token+11)) != '"' ||
		fastByteAt(base, structuralTapePosition(entries, token+12)) != '"' ||
		fastByteAt(base, structuralTapePosition(entries, token+16)) != '"' ||
		fastByteAt(base, structuralTapePosition(entries, token+17)) != '"' ||
		fastByteAt(base, structuralTapePosition(entries, token+21)) != '[' ||
		fastByteAt(base, arrayComma0) != ',' ||
		fastByteAt(base, arrayComma1) != ',' || fastByteAt(base, arrayEnd) != ']' ||
		fastByteAt(base, structuralTapePosition(entries, token+28)) != '}' {
		return false
	}

	width := comma0 - field0Value
	if uint(width-1) >= 4 || field0Value+4 > n {
		return false
	}
	word := loadUint32LE(unsafe.Add(base, field0Value))
	d0 := uint32(byte(word)) - '0'
	d1 := uint32(byte(word>>8)) - '0'
	d2 := uint32(byte(word>>16)) - '0'
	d3 := uint32(byte(word>>24)) - '0'
	var id uint64
	switch width {
	case 1:
		if d0 > 9 {
			return false
		}
		id = uint64(d0)
	case 2:
		if d0 == 0 || d0 > 9 || d1 > 9 {
			return false
		}
		id = uint64(d0*10 + d1)
	case 3:
		if d0 == 0 || d0 > 9 || d1 > 9 || d2 > 9 {
			return false
		}
		id = uint64(d0*100 + d1*10 + d2)
	case 4:
		if d0 == 0 || d0 > 9 || d1 > 9 || d2 > 9 || d3 > 9 {
			return false
		}
		id = uint64(d0*1000 + d1*100 + d2*10 + d3)
	}

	var active bool
	switch {
	case field1Value+4 == comma1 && literalTrueAt(src, field1Value):
		active = true
	case field1Value+5 == comma1 && field1Value < n && fastByteAt(base, field1Value) == 'f' &&
		literalFalseTailAt(src, field1Value):
	default:
		return false
	}

	float0 := structuralTapePosition(entries, token+22)
	float1 := structuralTapePosition(entries, token+24)
	float2 := structuralTapePosition(entries, token+26)
	if float0+8 > n || float1+8 > n || float2+8 > n {
		return false
	}
	score0, ok := shortStructuralFloatAt(base, float0, arrayComma0)
	if !ok {
		return false
	}
	score1, ok := shortStructuralFloatAt(base, float1, arrayComma1)
	if !ok {
		return false
	}
	score2, ok := shortStructuralFloatAt(base, float2, arrayEnd)
	if !ok {
		return false
	}

	objectEnd := structuralTapePosition(entries, token+28)
	fields := node.fields
	*(*int64)(unsafe.Add(dst, fields[0].offset)) = int64(id)
	*(*bool)(unsafe.Add(dst, fields[1].offset)) = active
	*(*string)(unsafe.Add(dst, fields[2].offset)) = unsafe.String(
		(*byte)(unsafe.Add(base, field2Value+1)), nameEnd-field2Value-1,
	)
	*(*string)(unsafe.Add(dst, fields[3].offset)) = unsafe.String(
		(*byte)(unsafe.Add(base, field3Value+1)), messageEnd-field3Value-1,
	)
	*(*[3]float64)(unsafe.Add(dst, fields[4].offset)) = [3]float64{score0, score1, score2}
	tape.index = token + 28
	cursor.i = objectEnd + 1
	cursor.depth--
	return true
}

func (cursor *decoderCursor) decodeCompiledStructStructuralRecord(node *typedNode, dst unsafe.Pointer) error {
	first := &node.fields[0]
	if !cursor.matchFirstObjectFieldStructuralShape(first) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 0, true, 0)
	}
	firstDst := unsafe.Add(dst, first.offset)
	i := cursor.i
	base := unsafe.Pointer(unsafe.SliceData(cursor.src))
	tape := &cursor.state.structural
	positions := tape.positions
	token := tape.index
	end := i
	value := uint64(0)
	fastInt := false
	if uint(token+1) < uint(len(positions)) {
		end = int(positions[token+1])
		width := end - i
		if uint(width-1) < 4 && i+4 <= len(cursor.src) {
			word := loadUint32LE(unsafe.Add(base, i))
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
		for end < len(cursor.src) && end-i < 9 && isDigit(fastByteAt(base, end)) {
			value = value*10 + uint64(fastByteAt(base, end)-'0')
			end++
		}
		fastInt = end != i && (fastByteAt(base, i) != '0' || end == i+1) &&
			(end == len(cursor.src) || !isDigit(fastByteAt(base, end)))
	}
	var err error
	if fastInt {
		*(*int64)(firstDst) = int64(value)
		cursor.i = end
	} else if useStableNumericMethods {
		err = cursor.Int64((*int64)(firstDst))
	} else {
		err = cursor.Int((*int64)(firstDst))
	}
	if err != nil {
		return compiledStructuralFieldError(err, first, true)
	}

	second := &node.fields[1]
	if !cursor.matchNextObjectFieldStructuralShape(second) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 1, false, 0)
	}
	secondDst := unsafe.Add(dst, second.offset)
	i = cursor.i
	if i < len(cursor.src) && cursor.src[i] == 't' && literalTrueAt(cursor.src, i) {
		*(*bool)(secondDst) = true
		cursor.i = i + 4
	} else if i < len(cursor.src) && cursor.src[i] == 'f' && literalFalseTailAt(cursor.src, i) {
		*(*bool)(secondDst) = false
		cursor.i = i + 5
	} else if err = cursor.Bool((*bool)(secondDst)); err != nil {
		return compiledStructuralFieldError(err, second, true)
	}

	third := &node.fields[2]
	if !cursor.matchNextObjectFieldStructuralShape(third) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 2, false, 0)
	}
	thirdDst := unsafe.Add(dst, third.offset)
	i = cursor.i
	token = tape.index
	fast := false
	if i < len(cursor.src) && cursor.src[i] == '"' && uint(token+1) < uint(len(positions)) &&
		int(positions[token]) == i {
		end := int(positions[token+1])
		if (!tape.nonASCII && !tape.escaped || cursor.structuralStringLocallyDirect(i+1, end)) &&
			end < len(cursor.src) && cursor.src[end] == '"' &&
			cursor.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
			tape.index = token + 1
			*(*string)(thirdDst) = unsafe.String(unsafe.SliceData(cursor.src[i+1:end]), end-i-1)
			cursor.i = end + 1
			fast = true
		}
	}
	if !fast {
		if err := cursor.stringStructural((*string)(thirdDst)); err != nil {
			return compiledStructuralFieldError(err, third, true)
		}
	}

	fourth := &node.fields[3]
	if !cursor.matchNextObjectFieldStructuralShape(fourth) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 3, false, 0)
	}
	fourthDst := unsafe.Add(dst, fourth.offset)
	i = cursor.i
	token = tape.index
	fast = false
	if i < len(cursor.src) && cursor.src[i] == '"' && uint(token+1) < uint(len(positions)) &&
		int(positions[token]) == i {
		end := int(positions[token+1])
		if (!tape.nonASCII && !tape.escaped || cursor.structuralStringLocallyDirect(i+1, end)) &&
			end < len(cursor.src) && cursor.src[end] == '"' &&
			cursor.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
			tape.index = token + 1
			*(*string)(fourthDst) = unsafe.String(unsafe.SliceData(cursor.src[i+1:end]), end-i-1)
			cursor.i = end + 1
			fast = true
		}
	}
	if !fast {
		if err := cursor.stringStructural((*string)(fourthDst)); err != nil {
			return compiledStructuralFieldError(err, fourth, true)
		}
	}

	fifth := &node.fields[4]
	if !cursor.matchNextObjectFieldStructuralShape(fifth) {
		return cursor.decodeCompiledStructStructuralSlow(node, dst, 4, false, 0)
	}
	if err := cursor.decodeCompiledArrayStructural(fifth.node, unsafe.Add(dst, fifth.offset)); err != nil {
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
		switch cursor.matchObjectFieldStructural(first, field) {
		case structuralFieldMatched:
			matched, ok = true, true
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
					if inlineDec == nil {
						inlineDec = cursor.takeInlineDecoder(node.inlineMap)
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
			fieldErr = &DecodeError{Offset: cursor.i, Type: fieldNode.typ, Reason: "invalid compiled operation"}
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
