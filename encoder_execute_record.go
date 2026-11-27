package vibejson

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"unsafe"
)

func (e *encodeState) encodeStruct(node *typedNode, src unsafe.Pointer) error {
	program := node.encodeProgram
	// encFusedExtra preserves the exact depth limit across fused static
	// levels that no longer recurse.
	if e.depth+program.encFusedExtra >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	e.depth++
	e.dst = append(e.dst, '{')
	if node.encSimple {
		return e.encodeSimpleStructPairs(node, src)
	}
	first := true
	for i := range program.encFields {
		encField := &program.encFields[i]
		fieldBase := src
		if encField.hop >= 0 {
			fieldBase = resolveResetHops(src, node.fieldHops[encField.hop])
			if fieldBase == nil {
				// A nil embedded pointer omits its promoted fields entirely.
				continue
			}
		}
		fieldSrc := unsafe.Add(fieldBase, encField.offset)
		if encField.omit != 0 && ((encField.omit&typedOmitEmpty != 0 && typedValueIsEmpty(encField.node, fieldSrc)) ||
			(encField.omit&typedOmitZero != 0 && typedValueIsZero(encField.node, fieldSrc, typedZeroMethod(encField.omit>>typedOmitZeroMethodShift)))) {
			continue
		}
		name := encField.encName
		if first {
			name = name[1:]
			first = false
		}
		e.dst = append(e.dst, name...)
		var err error
		switch encField.encOp {
		// BEGIN GENERATED TYPED ENCODER FIELD DISPATCH
		case typedOpBool:
			if *(*bool)(fieldSrc) {
				e.dst = append(e.dst, "true"...)
			} else {
				e.dst = append(e.dst, "false"...)
			}
		case typedOpString:
			e.dst = appendEncodedJSONString(e.dst, *(*string)(fieldSrc), e.escapeHTML)
		case typedOpNumber:
			err = e.encodeNumberLiteral(*(*string)(fieldSrc))
		case typedOpInt8:
			e.dst = appendCompactInt(e.dst, int64(*(*int8)(fieldSrc)))
		case typedOpInt16:
			e.dst = appendCompactInt(e.dst, int64(*(*int16)(fieldSrc)))
		case typedOpInt32:
			e.dst = appendCompactInt(e.dst, int64(*(*int32)(fieldSrc)))
		case typedOpInt64:
			e.dst = appendCompactInt(e.dst, *(*int64)(fieldSrc))
		case typedOpUint8:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint8)(fieldSrc)))
		case typedOpUint16:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint16)(fieldSrc)))
		case typedOpUint32:
			e.dst = appendCompactUint(e.dst, uint64(*(*uint32)(fieldSrc)))
		case typedOpUint64:
			e.dst = appendCompactUint(e.dst, *(*uint64)(fieldSrc))
		case typedOpFloat32:
			err = e.encodeFloat(float64(*(*float32)(fieldSrc)), 32)
		case typedOpFloat64:
			err = e.encodeFloat(*(*float64)(fieldSrc), 64)
		case typedOpStruct:
			err = e.encodeStruct(encField.node, fieldSrc)
		case typedOpSlice:
			err = e.encodeSlice(encField.node, fieldSrc)
		case typedOpArray:
			err = e.encodeArray(encField.node, fieldSrc)
		case typedOpMap:
			err = e.encodeMap(encField.node, fieldSrc)
		case typedOpAny:
			err = e.encodeAnyValue(*(*any)(fieldSrc), false)
		case typedOpQuoted:
			err = e.encodeQuoted(encField.node, fieldSrc)
		case typedOpMarshaler:
			err = e.encodeKind(encField.node, fieldSrc, encField.node.encKind)
		// END GENERATED TYPED ENCODER FIELD DISPATCH
		default:
			err = e.encodeKind(encField.node, fieldSrc, encField.node.encKind)
		}
		if err != nil {
			e.depth--
			return prependEncodePathField(err, program.encPaths[i])
		}
	}
	if node.inlineMap != nil {
		if err := e.encodeInlineMembers(node.inlineMap, src, &first); err != nil {
			e.depth--
			return err
		}
	}
	e.dst = append(e.dst, '}')
	e.depth--
	return nil
}

// encodeNonAddressableStruct is the cold struct walk for a map value or
// interface payload that reaches pointer-receiver marshalers. Direct struct
// fields and array elements remain non-addressable; pointers, slices, maps,
// and interfaces restore or independently determine addressability and stay on
// their ordinary encoders. The hot encodeStruct loop therefore pays nothing
// for this compatibility rule.
func (e *encodeState) encodeNonAddressableStruct(node *typedNode, src unsafe.Pointer) error {
	program := node.encodeProgram
	if e.depth+program.encFusedExtra >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	e.depth++
	e.dst = append(e.dst, '{')

	if node.encSimple {
		for i := range program.encFields {
			field := &program.encFields[i]
			e.dst = append(e.dst, field.encName...)
			fieldSrc := unsafe.Add(src, field.offset)
			var err error
			if field.node.encHasPtrMarshaler {
				err = e.encodeNonAddressableMarshaler(field.node, fieldSrc)
			} else {
				err = e.encodeStructFieldValue(field, fieldSrc)
			}
			if err != nil {
				e.depth--
				return prependEncodePathField(err, program.encPaths[i])
			}
		}
		e.dst = append(e.dst, program.encClose...)
		e.depth--
		return nil
	}

	first := true
	for i := range program.encFields {
		field := &program.encFields[i]
		fieldBase := src
		if field.hop >= 0 {
			fieldBase = resolveResetHops(src, node.fieldHops[field.hop])
			if fieldBase == nil {
				continue
			}
		}
		fieldSrc := unsafe.Add(fieldBase, field.offset)
		if field.omit != 0 && ((field.omit&typedOmitEmpty != 0 && typedValueIsEmpty(field.node, fieldSrc)) ||
			(field.omit&typedOmitZero != 0 && typedValueIsZero(field.node, fieldSrc, typedZeroMethod(field.omit>>typedOmitZeroMethodShift)))) {
			continue
		}
		name := field.encName
		if first {
			name = name[1:]
			first = false
		}
		e.dst = append(e.dst, name...)
		var err error
		if field.hop < 0 && field.node.encHasPtrMarshaler {
			err = e.encodeNonAddressableMarshaler(field.node, fieldSrc)
		} else {
			err = e.encodeStructFieldValue(field, fieldSrc)
		}
		if err != nil {
			e.depth--
			return prependEncodePathField(err, program.encPaths[i])
		}
	}
	if node.inlineMap != nil {
		if err := e.encodeInlineMembers(node.inlineMap, src, &first); err != nil {
			e.depth--
			return err
		}
	}
	e.dst = append(e.dst, '}')
	e.depth--
	return nil
}

// encodeInlineMembers splices a ",inline" catch-all map's entries into the
// enclosing object after its declared fields, sharing the comma bookkeeping
// through first. An empty or nil map emits nothing, so a struct that declares
// a catch-all pays only a length check when it holds no unknown members.
//
// A populated catch-all is allocation-free after warmup: it borrows the pooled
// entry slice and map iterator, copies each member name into a reserved key box
// with SetIterKey, and copies each value into its own slot of a pooled backing
// slice with SetIterValue. Independent slots (rather than one shared box) keep
// the encode correct even when a value recurses into the same catch-all type.
// String keys alias the live map's own storage, so names need no arena.
func (e *encodeState) encodeInlineMembers(inline *typedInlineMap, structPtr unsafe.Pointer, first *bool) error {
	mapPtr := unsafe.Add(structPtr, inline.offset)
	switch inline.mapFastKind {
	case mapFastStringInt:
		return e.encodeInlineMapStringInt(*(*map[string]int)(mapPtr), first)
	case mapFastStringInt64:
		return e.encodeInlineMapStringInt64(*(*map[string]int64)(mapPtr), first)
	case mapFastStringString:
		return e.encodeInlineMapStringString(*(*map[string]string)(mapPtr), first)
	case mapFastStringFloat64:
		return e.encodeInlineMapStringFloat64(*(*map[string]float64)(mapPtr), first)
	case mapFastStringBool:
		return e.encodeInlineMapStringBool(*(*map[string]bool)(mapPtr), first)
	case mapFastStringAny:
		return e.encodeInlineMapStringAny(*(*map[string]any)(mapPtr), first)
	case mapFastStringRawMessage:
		return e.encodeInlineMapStringRawMessage(*(*map[string]json.RawMessage)(mapPtr), first)
	}
	mapValue := reflect.NewAt(inline.mapType, mapPtr).Elem()
	if mapValue.IsNil() || mapValue.Len() == 0 {
		return nil
	}
	elem := inline.elem
	mapLen := mapValue.Len()
	oneShot := mapLen > inline.encScratchLimit && e.scratch != nil
	var savedScratch *encoderScratch
	if oneShot {
		savedScratch = e.scratch
		e.scratch = nil
	}

	var entries []mapEncodeEntry
	var keyArena []byte
	var iterator *reflect.MapIter
	scratch := e.scratch
	if scratch != nil {
		entries = scratch.mapEntries[:0]
		keyArena = scratch.mapKeyArena[:0]
		iterator = scratch.mapIter
		scratch.mapEntries = nil
		scratch.mapKeyArena = nil
		scratch.mapIter = nil
	}
	if cap(entries) < mapLen {
		entries = make([]mapEncodeEntry, 0, mapLen)
	}

	var keyBox reflect.Value
	if scratch != nil && inline.encKey >= 0 {
		keyBox = scratch.marshalers[inline.encKey].value
	} else {
		keyBox = reflect.New(inline.mapType.Key()).Elem()
	}
	backing := e.takeValueBacking(inline.encBacking, elem.typ, mapLen)

	// mapValue remains visible to escape analysis and the GC for the full
	// iteration. The pooled iterator is unbound before release, so it cannot
	// retain this operation's source afterward.
	if iterator == nil {
		iterator = mapValue.MapRange()
	} else {
		iterator.Reset(mapValue)
	}

	for slot := 0; iterator.Next(); slot++ {
		valueSlot := backing.Index(slot)
		valueSlot.SetIterValue(iterator)
		keyBox.SetIterKey(iterator)
		entries = append(entries, mapEncodeEntry{name: keyBox.String(), value: valueSlot})
	}
	if len(entries) > 1 {
		slices.SortFunc(entries, func(a, b mapEncodeEntry) int { return strings.Compare(a.name, b.name) })
	}

	elemHasMarshaler := elem.encHasPtrMarshaler
	var retErr error
	for i := range entries {
		if *first {
			*first = false
		} else {
			e.dst = append(e.dst, ',')
		}
		e.dst = appendEncodedJSONString(e.dst, entries[i].name, e.escapeHTML)
		e.dst = append(e.dst, ':')
		valuePtr := entries[i].value.Addr().UnsafePointer()
		var err error
		if elemHasMarshaler {
			err = e.encodeNonAddressableMarshaler(elem, valuePtr)
		} else {
			err = e.encodeKind(elem, valuePtr, elem.encNonAddrKind)
		}
		if err != nil {
			retErr = prependEncodePathField(err, entries[i].name)
			break
		}
	}
	e.releaseValueBackingPrefix(inline.encBacking, backing, elem.typ, len(entries))
	e.releaseMapScratch(entries, keyArena, iterator, nil, false)
	if oneShot {
		e.scratch = savedScratch
	}
	return retErr
}

func (e *encodeState) encodeInlineMapStringInt(m map[string]int, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[int]
	var entries []stringMapPair[int]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[int], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[int]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	dst := e.dst
	escapeHTML := e.escapeHTML
	for i := range entries {
		if *first {
			*first = false
		} else {
			dst = append(dst, ',')
		}
		dst = appendEncodedJSONString(dst, entries[i].k, escapeHTML)
		v := entries[i].v
		if uint(v) < 10 {
			dst = append(dst, ':', byte('0'+v))
		} else {
			dst = append(dst, ':')
			dst = appendCompactInt(dst, int64(v))
		}
	}
	e.dst = dst
	return nil
}

func (e *encodeState) encodeInlineMapStringInt64(m map[string]int64, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[int64]
	var entries []stringMapPair[int64]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[int64], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[int64]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	dst := e.dst
	escapeHTML := e.escapeHTML
	for i := range entries {
		if *first {
			*first = false
		} else {
			dst = append(dst, ',')
		}
		dst = appendEncodedJSONString(dst, entries[i].k, escapeHTML)
		dst = append(dst, ':')
		dst = appendCompactInt(dst, entries[i].v)
	}
	e.dst = dst
	return nil
}

func (e *encodeState) encodeInlineMapStringString(m map[string]string, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[string]
	var entries []stringMapPair[string]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[string], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[string]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	dst := e.dst
	escapeHTML := e.escapeHTML
	for i := range entries {
		if *first {
			*first = false
		} else {
			dst = append(dst, ',')
		}
		dst = appendEncodedJSONString(dst, entries[i].k, escapeHTML)
		dst = append(dst, ':')
		dst = appendEncodedJSONString(dst, entries[i].v, escapeHTML)
	}
	e.dst = dst
	return nil
}

func (e *encodeState) encodeInlineMapStringFloat64(m map[string]float64, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[float64]
	var entries []stringMapPair[float64]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[float64], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[float64]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	for i := range entries {
		if *first {
			*first = false
		} else {
			e.dst = append(e.dst, ',')
		}
		e.dst = appendEncodedJSONString(e.dst, entries[i].k, e.escapeHTML)
		e.dst = append(e.dst, ':')
		if err := e.encodeFloat(entries[i].v, 64); err != nil {
			return prependEncodePathField(err, entries[i].k)
		}
	}
	return nil
}

func (e *encodeState) encodeInlineMapStringBool(m map[string]bool, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[bool]
	var entries []stringMapPair[bool]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[bool], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[bool]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	dst := e.dst
	escapeHTML := e.escapeHTML
	for i := range entries {
		if *first {
			*first = false
		} else {
			dst = append(dst, ',')
		}
		dst = appendEncodedJSONString(dst, entries[i].k, escapeHTML)
		if entries[i].v {
			dst = append(dst, ":true"...)
		} else {
			dst = append(dst, ":false"...)
		}
	}
	e.dst = dst
	return nil
}

func (e *encodeState) encodeInlineMapStringAny(m map[string]any, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[any]
	var entries []stringMapPair[any]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[any], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[any]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	for i := range entries {
		if *first {
			*first = false
		} else {
			e.dst = append(e.dst, ',')
		}
		e.dst = appendEncodedJSONString(e.dst, entries[i].k, e.escapeHTML)
		e.dst = append(e.dst, ':')
		if err := e.encodeAnyValue(entries[i].v, true); err != nil {
			return prependEncodePathField(err, entries[i].k)
		}
	}
	return nil
}

func (e *encodeState) encodeInlineMapStringRawMessage(m map[string]json.RawMessage, first *bool) error {
	n := len(m)
	if n == 0 {
		return nil
	}
	var stack [16]stringMapPair[json.RawMessage]
	var entries []stringMapPair[json.RawMessage]
	if n <= len(stack) {
		entries = stack[:0]
	} else {
		entries = make([]stringMapPair[json.RawMessage], 0, n)
	}
	for k, v := range m {
		entries = append(entries, stringMapPair[json.RawMessage]{k: k, v: v})
	}
	if len(entries) > 1 {
		sortStringMapPairs(entries)
	}
	for i := range entries {
		if *first {
			*first = false
		} else {
			e.dst = append(e.dst, ',')
		}
		e.dst = appendEncodedJSONString(e.dst, entries[i].k, e.escapeHTML)
		e.dst = append(e.dst, ':')
		if err := e.encodeRawMessage(entries[i].v); err != nil {
			return prependEncodePathField(err, entries[i].k)
		}
	}
	return nil
}
