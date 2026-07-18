package simdjson

import (
	"reflect"
	"slices"
	"strings"
	"unsafe"
)

func (e *encodeState) encodeStruct(node *typedNode, src unsafe.Pointer) error {
	// encFusedExtra preserves the exact depth limit across fused static
	// levels that no longer recurse.
	if encoderHasDepthLimit && e.depth+int(node.encFusedExtra) >= defaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	e.depth++
	e.dst = append(e.dst, '{')
	if node.encSimple {
		return e.encodeSimpleStructPairs(node, src)
	}
	first := true
	for i := range node.encFields {
		encField := &node.encFields[i]
		fieldBase := src
		if encField.hop >= 0 {
			fieldBase = resolveResetHops(src, node.fieldHops[encField.hop])
			if fieldBase == nil {
				// A nil embedded pointer omits its promoted fields entirely.
				continue
			}
		}
		fieldSrc := unsafe.Add(fieldBase, encField.offset)
		if encField.omitEmpty && typedValueIsEmpty(encField.node, fieldSrc) {
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
			err = e.encodeAny(fieldSrc)
		case typedOpQuoted:
			err = e.encodeQuoted(encField.node, fieldSrc)
		case typedOpMarshaler:
			err = e.encodeMarshaler(encField.node, fieldSrc)
		// END GENERATED TYPED ENCODER FIELD DISPATCH
		default:
			err = e.encode(encField.node, fieldSrc)
		}
		if err != nil {
			e.depth--
			return prependEncodePathField(err, node.encPaths[i])
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
	mapValue := reflect.NewAt(inline.mapType, unsafe.Add(structPtr, inline.offset)).Elem()
	if mapValue.IsNil() || mapValue.Len() == 0 {
		return nil
	}
	elem := inline.elem
	mapLen := mapValue.Len()
	if mapLen > inline.encScratchLimit && e.scratch != nil {
		return e.encodeInlineMembersOneShot(inline, structPtr, first)
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
	if inline.sorted {
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
	e.releaseValueBacking(inline.encBacking, backing, elem.typ)
	e.releaseMapScratch(entries, keyArena, iterator, nil, false)
	return retErr
}

// encodeInlineMembersOneShot keeps an exceptional map from borrowing or
// replacing the bounded scratch. The recursive call sees nil scratch and runs
// the ordinary implementation with operation-local storage.
func (e *encodeState) encodeInlineMembersOneShot(inline *typedInlineMap, structPtr unsafe.Pointer, first *bool) error {
	scratch := e.scratch
	e.scratch = nil
	err := e.encodeInlineMembers(inline, structPtr, first)
	e.scratch = scratch
	return err
}
