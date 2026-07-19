package simdjson

import (
	"encoding"
	"encoding/base64"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"unsafe"
)

func (cursor *decoderCursor) decodeCompiledPointer(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	if null {
		*(*unsafe.Pointer)(dst) = nil
		return nil
	}
	pointer := *(*unsafe.Pointer)(dst)
	if pointer == nil {
		pointer = allocateTypedPointer(node, dst)
	}
	switch node.elem.kind {
	case typedStruct:
		return cursor.decodeCompiledStruct(node.elem, pointer)
	case typedSlice:
		return cursor.decodeCompiledSlice(node.elem, pointer)
	case typedArray:
		return cursor.decodeCompiledArray(node.elem, pointer)
	case typedPointer:
		return cursor.decodeCompiledPointer(node.elem, pointer)
	case typedMap:
		return cursor.decodeCompiledMap(node.elem, pointer)
	default:
		return cursor.decodeCompiled(node.elem, pointer)
	}
}

// takeInlineDecoder returns one key and element box for a run of unknown
// members. Eligible compiled plans reuse cleared boxes across operations;
// observable or recursively active boxes retain the one-call fallback.
func (cursor *decoderCursor) takeInlineDecoder(inline *typedInlineMap) *decoderMapScratch {
	if inline.decMapScratch != 0 && cursor.state != nil && cursor.state.operation != nil {
		index := int(inline.decMapScratch - 1)
		if index < len(cursor.state.operation.maps) {
			scratch := &cursor.state.operation.maps[index]
			if !scratch.inUse {
				if !scratch.element.IsValid() {
					element := reflect.New(inline.elem.typ)
					scratch.element = element.Elem()
					scratch.key = reflect.New(inline.mapType.Key()).Elem()
				}
				scratch.entries = 0
				scratch.inUse = true
				return scratch
			}
		}
	}
	element := reflect.New(inline.elem.typ)
	return &decoderMapScratch{
		key:     reflect.New(inline.mapType.Key()).Elem(),
		element: element.Elem(),
		inUse:   true,
	}
}

// decodeInlineEntry decodes one member into the catch-all, allocating the map on
// first use. The member name becomes the key, using an already-owned source
// backing or an independent clone; the value follows the cursor's ownership
// rules like any other decode. SetMapIndex copies both key and value into the
// map, so reusing the boxes across members is safe.
func (d *decoderMapScratch) decodeInlineEntry(cursor *decoderCursor, inline *typedInlineMap, structPtr unsafe.Pointer, key string) error {
	mapValue := reflect.NewAt(inline.mapType, unsafe.Add(structPtr, inline.offset)).Elem()
	if mapValue.IsNil() {
		mapValue.Set(reflect.MakeMap(inline.mapType))
	}
	d.element.SetZero()
	batchedReceivers := d.entries > 0 && inline.elem.decHasReceiver && cursor.beginReceiverBatch()
	d.entries++
	elementPtr := d.element.Addr().UnsafePointer()
	var err error
	switch inline.elem.kind {
	case typedStruct:
		err = cursor.decodeCompiledStruct(inline.elem, elementPtr)
	case typedSlice:
		err = cursor.decodeCompiledSlice(inline.elem, elementPtr)
	case typedArray:
		err = cursor.decodeCompiledArray(inline.elem, elementPtr)
	case typedPointer:
		err = cursor.decodeCompiledPointer(inline.elem, elementPtr)
	case typedMap:
		err = cursor.decodeCompiledMap(inline.elem, elementPtr)
	default:
		err = cursor.decodeCompiled(inline.elem, elementPtr)
	}
	cursor.endReceiverBatch(batchedReceivers)
	if err != nil {
		return err
	}
	if cursor.flags&(decoderZeroCopy|decoderSourceOwned) == 0 {
		key = strings.Clone(key)
	}
	d.key.SetString(key)
	mapValue.SetMapIndex(d.key, d.element)
	return nil
}

// decodeCompiledMap decodes a JSON object into a map with string keys. Like
// encoding/json it allocates a map only when dst holds nil and otherwise
// merges into the existing entries. Entries decode through one reusable
// element that is zeroed between entries, so nested slice capacity is never
// shared between values.
func (cursor *decoderCursor) decodeCompiledMap(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	if null {
		reflect.NewAt(node.typ, dst).Elem().SetZero()
		return nil
	}
	if err := cursor.BeginObject(node.name); err != nil {
		return err
	}
	// Map keys are retained by the result, so switch owned decodes to the
	// private input copy before the first key string is sliced.
	cursor.ownSource()
	mapValue := reflect.NewAt(node.typ, dst).Elem()
	if cursor.flags&decoderReplace != 0 && !mapValue.IsNil() {
		// Replace decodes as if into a fresh destination, so a reused map drops
		// keys the document does not set instead of merging into them.
		mapValue.Clear()
	}
	if mapValue.IsNil() {
		mapValue.Set(reflect.MakeMap(node.typ))
	}
	keyType := node.typ.Key()
	scratch := cursor.takeMapScratch(node)
	var elementValue reflect.Value
	var elementPtr unsafe.Pointer
	var keyValue reflect.Value
	if scratch != nil {
		elementValue = scratch.element
		elementPtr = scratch.element.Addr().UnsafePointer()
		keyValue = scratch.key
	} else {
		element := reflect.New(node.elem.typ)
		elementValue = element.Elem()
		elementPtr = element.UnsafePointer()
		keyValue = reflect.New(keyType).Elem()
	}
	// One reusable key box serves every entry: SetMapIndex copies the key into
	// the map, so the box is reset per entry instead of allocating one each
	// time. The text unmarshaler, when present, is bound to the box once.
	var keyUnmarshaler encoding.TextUnmarshaler
	if node.mapKeyTextDecode {
		keyUnmarshaler = keyValue.Addr().Interface().(encoding.TextUnmarshaler)
	}
	for first := true; ; first = false {
		key, ok, err := cursor.NextObjectField(first)
		if err != nil {
			releaseMapScratch(scratch)
			return err
		}
		if !ok {
			releaseMapScratch(scratch)
			return nil
		}
		elementValue.SetZero()
		batchedReceivers := !first && node.decHasReceiver && cursor.beginReceiverBatch()
		var entryErr error
		switch node.elem.kind {
		case typedStruct:
			entryErr = cursor.decodeCompiledStruct(node.elem, elementPtr)
		case typedSlice:
			entryErr = cursor.decodeCompiledSlice(node.elem, elementPtr)
		case typedArray:
			entryErr = cursor.decodeCompiledArray(node.elem, elementPtr)
		case typedPointer:
			entryErr = cursor.decodeCompiledPointer(node.elem, elementPtr)
		case typedMap:
			entryErr = cursor.decodeCompiledMap(node.elem, elementPtr)
		default:
			entryErr = cursor.decodeCompiled(node.elem, elementPtr)
		}
		cursor.endReceiverBatch(batchedReceivers)
		if entryErr != nil {
			releaseMapScratch(scratch)
			return prependDecodePathField(entryErr, key)
		}
		if keyErr := setMapKeyValue(keyValue, keyUnmarshaler, node, keyType, key); keyErr != nil {
			releaseMapScratch(scratch)
			return prependDecodePathField(&DecodeError{Offset: cursor.i, Type: keyType, Reason: keyErr.Error()}, key)
		}
		mapValue.SetMapIndex(keyValue, elementValue)
	}
}

// setMapKeyValue decodes a member name into the reused key box in place,
// following encoding/json: text unmarshalers first, then string kinds, then
// base-10 integers with range checks. The box is copied into the map by
// SetMapIndex, so it may be reused across entries; the text path zeroes it
// first to match a freshly allocated key.
func setMapKeyValue(keyValue reflect.Value, unmarshaler encoding.TextUnmarshaler, node *typedNode, keyType reflect.Type, key string) error {
	if node.mapKeyTextDecode {
		keyValue.SetZero()
		return unmarshaler.UnmarshalText([]byte(key))
	}
	switch node.mapKeyKind {
	case mapKeyString:
		keyValue.SetString(key)
		return nil
	case mapKeyInt:
		parsed, err := strconv.ParseInt(key, 10, 64)
		if err != nil || keyValue.OverflowInt(parsed) {
			return errors.New("cannot parse map key as " + keyType.String())
		}
		keyValue.SetInt(parsed)
		return nil
	case mapKeyUint:
		parsed, err := strconv.ParseUint(key, 10, 64)
		if err != nil || keyValue.OverflowUint(parsed) {
			return errors.New("cannot parse map key as " + keyType.String())
		}
		keyValue.SetUint(parsed)
		return nil
	default:
		return errors.New("map key type " + keyType.String() + " cannot be decoded")
	}
}

// anyDecodeMerges reports whether decoding into an empty interface that
// already holds existing must merge rather than replace: like encoding/json,
// only a held non-nil pointer is decoded into. Everything else — nil, maps,
// slices, scalars, nil pointers — is replaced wholesale (and null clears),
// so those destinations are free to take the whole-document builder.
func anyDecodeMerges(existing any) bool {
	if existing == nil {
		return false
	}
	value := reflect.ValueOf(existing)
	return value.Kind() == reflect.Pointer && !value.IsNil()
}

// decodeCompiledAny decodes one JSON value into an empty interface using the
// standard dynamic shapes: map[string]any, []any, string, float64 (or
// json.Number under UseNumber), bool, and nil. Like encoding/json, an
// interface already holding a non-nil pointer is decoded into rather than
// replaced; anything else is replaced, and null clears the interface.
func (cursor *decoderCursor) decodeCompiledAny(dst unsafe.Pointer) error {
	if existing := *(*any)(dst); existing != nil {
		null := false
		if !cursor.notNullFast() {
			var err error
			null, err = cursor.TryNull()
			if err != nil {
				return err
			}
		}
		if null {
			*(*any)(dst) = nil
			return nil
		}
		existingValue := reflect.ValueOf(existing)
		if existingValue.Kind() == reflect.Pointer && !existingValue.IsNil() {
			inner, err := dynamicDecodeNode(existingValue.Type().Elem())
			if err != nil {
				return &DecodeError{Offset: cursor.i, Type: existingValue.Type(), Reason: err.Error()}
			}
			return cursor.decodeCompiled(inner, existingValue.UnsafePointer())
		}
	}
	// Dynamic strings are retained by the result, so owned decodes switch to
	// the private input copy first.
	cursor.ownSource()
	p := cursor.slowParser()
	p.skipSpace()
	value, err := p.parseAnyValue(cursor.depth, cursor.flags&decoderUseNumber != 0)
	cursor.i = p.i
	// The dynamic tree retains any escaped strings it materialized in the
	// arena; advancing the arena keeps later escaped strings from
	// overwriting them.
	cursor.adoptStringArena(p.strings)
	if err != nil {
		return err
	}
	*(*any)(dst) = value
	return nil
}

// dynamicDecodeNodes caches one compiled decode plan per concrete type found
// inside an interface value.
var dynamicDecodeNodes sync.Map

type dynamicDecodeEntry struct {
	node *typedNode
	err  error
}

func dynamicDecodeNode(typ reflect.Type) (*typedNode, error) {
	if entry, ok := dynamicDecodeNodes.Load(typ); ok {
		cached := entry.(*dynamicDecodeEntry)
		return cached.node, cached.err
	}
	compiler := newTypedCompiler(typedCompileDecode)
	node, err := compiler.compile(typ, typ.String())
	if err == nil {
		prepareTypedResets(node, make(map[*typedNode]bool))
		prepareDecoderReceivers(node)
	}
	entry, _ := dynamicDecodeNodes.LoadOrStore(typ, &dynamicDecodeEntry{node: node, err: err})
	cached := entry.(*dynamicDecodeEntry)
	return cached.node, cached.err
}

// decodeCompiledIface decodes into a non-empty interface: null clears it,
// and a held non-nil pointer is decoded into like encoding/json; any other
// state cannot be decoded.
func (cursor *decoderCursor) decodeCompiledIface(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	if null {
		reflect.NewAt(node.typ, dst).Elem().SetZero()
		return nil
	}
	value := reflect.NewAt(node.typ, dst).Elem()
	if !value.IsNil() {
		concrete := value.Elem()
		if concrete.Kind() == reflect.Pointer && !concrete.IsNil() {
			inner, innerErr := dynamicDecodeNode(concrete.Type().Elem())
			if innerErr != nil {
				return &DecodeError{Offset: cursor.i, Type: concrete.Type(), Reason: innerErr.Error()}
			}
			return cursor.decodeCompiled(inner, concrete.UnsafePointer())
		}
	}
	return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "cannot decode into a non-empty interface"}
}

// decodeQuotedField decodes a scalar tagged with the string option: the JSON
// value is a string whose contents are one scalar, parsed with encoding/json's
// semantics. A bare null clears pointer fields and resets values only in
// replace mode; anything but a string or null is rejected.
func (cursor *decoderCursor) decodeQuotedField(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	if null {
		if node.baseKind == typedPointer || cursor.flags&decoderReplace != 0 {
			resetTyped(node, dst)
		}
		return nil
	}
	i := cursor.i
	if i >= len(cursor.src) || cursor.src[i] != '"' {
		return &DecodeError{Offset: i, Type: node.typ, Reason: "expected quoted value for string-tagged field"}
	}
	inner, err := cursor.stringToken()
	if err != nil {
		return err
	}
	// The inner scalar may alias a temporary unescape buffer, so decoded
	// strings must never alias it.
	flags := cursor.flags &^ (decoderZeroCopy | decoderSourceOwned)
	sub := decoderCursor{src: inner, maxDepth: cursor.maxDepth, flags: flags}
	scalar := node
	if scalar.kind == typedPointer {
		scalar = scalar.elem
	}
	switch scalar.kind {
	case typedInt, typedUint, typedFloat:
		return decodeQuotedNumber(node, scalar, dst, inner, i)
	}
	if scalar.kind == typedString {
		// The contents must themselves be a JSON string.
		if len(inner) == 0 || inner[0] != '"' {
			return &DecodeError{Offset: i, Type: node.typ, Reason: "string-tagged field does not contain a JSON string"}
		}
	}
	if err := sub.decodeCompiled(node, dst); err != nil {
		if typed, ok := err.(*DecodeError); ok {
			typed.Offset = i
		}
		return err
	}
	if sub.i != len(inner) {
		return &DecodeError{Offset: i, Type: node.typ, Reason: "string-tagged field contains trailing data"}
	}
	return nil
}

// decodeQuotedNumber stores a string-tagged number with encoding/json's
// semantics: the quoted contents are handed to strconv verbatim, which
// accepts spellings strict JSON does not (leading zeros, an explicit plus,
// and strconv's float forms).
func decodeQuotedNumber(node, scalar *typedNode, dst unsafe.Pointer, inner []byte, offset int) error {
	text := unsafe.String(unsafe.SliceData(inner), len(inner))
	if text == "null" {
		// encoding/json treats a quoted null like the bare literal: value
		// fields are left untouched and pointer fields are cleared.
		if node.kind == typedPointer {
			*(*unsafe.Pointer)(dst) = nil
		}
		return nil
	}
	if !acceptStringTaggedNumber(text) {
		return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged number " + strconv.Quote(text)}
	}
	scalarDst := dst
	if node.kind == typedPointer {
		pointer := *(*unsafe.Pointer)(dst)
		if pointer == nil {
			pointer = allocateTypedPointer(node, dst)
		}
		scalarDst = pointer
	}
	switch scalar.kind {
	case typedInt:
		value, err := strconv.ParseInt(text, 10, int(scalar.bits))
		if err != nil {
			return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged integer " + strconv.Quote(text)}
		}
		switch scalar.bits {
		case 8:
			*(*int8)(scalarDst) = int8(value)
		case 16:
			*(*int16)(scalarDst) = int16(value)
		case 32:
			*(*int32)(scalarDst) = int32(value)
		default:
			*(*int64)(scalarDst) = value
		}
	case typedUint:
		value, err := strconv.ParseUint(text, 10, int(scalar.bits))
		if err != nil {
			return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged integer " + strconv.Quote(text)}
		}
		switch scalar.bits {
		case 8:
			*(*uint8)(scalarDst) = uint8(value)
		case 16:
			*(*uint16)(scalarDst) = uint16(value)
		case 32:
			*(*uint32)(scalarDst) = uint32(value)
		default:
			*(*uint64)(scalarDst) = value
		}
	default:
		value, err := strconv.ParseFloat(text, int(scalar.bits))
		if err != nil {
			return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged number " + strconv.Quote(text)}
		}
		if scalar.bits == 32 {
			*(*float32)(scalarDst) = float32(value)
		} else {
			*(*float64)(scalarDst) = value
		}
	}
	return nil
}

// decodeCompiledBytes decodes a base64 JSON string into a byte slice,
// reusing destination capacity when possible.
func (cursor *decoderCursor) decodeCompiledBytes(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	value := reflect.NewAt(node.typ, dst).Elem()
	if null {
		value.SetZero()
		return nil
	}
	i := cursor.i
	if i < len(cursor.src) && cursor.src[i] == '[' {
		// encoding/json also decodes a byte slice from an array of
		// integers, one element per byte.
		return cursor.decodeBytesArray(node, dst)
	}
	if i >= len(cursor.src) || cursor.src[i] != '"' {
		return &DecodeError{Offset: i, Type: node.typ, Reason: "expected base64 string"}
	}
	encoded, err := cursor.stringToken()
	if err != nil {
		return err
	}
	decodedLen := base64.StdEncoding.DecodedLen(len(encoded))
	buffer := value.Bytes()
	if cap(buffer) < decodedLen {
		buffer = make([]byte, decodedLen)
	} else {
		buffer = buffer[:decodedLen]
	}
	if buffer == nil {
		buffer = make([]byte, 0)
	}
	n, err := base64.StdEncoding.Decode(buffer, encoded)
	if err != nil {
		return &DecodeError{Offset: i, Type: node.typ, Reason: "invalid base64: " + err.Error()}
	}
	value.SetBytes(buffer[:n])
	return nil
}

// decodeBytesArray decodes the array form of []byte accepted by
// encoding/json: a JSON array of integers, one per byte, reusing destination
// capacity like every other slice decode.
func (cursor *decoderCursor) decodeBytesArray(node *typedNode, dst unsafe.Pointer) error {
	if err := cursor.BeginArray(node.name); err != nil {
		return err
	}
	value := reflect.NewAt(node.typ, dst).Elem()
	buf := value.Bytes()[:0]
	for first := true; ; first = false {
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		var element uint8
		var decodeErr error
		if useStableNumericMethods {
			decodeErr = cursor.Uint8(&element)
		} else {
			decodeErr = cursor.Uint(&element)
		}
		if decodeErr != nil {
			return retagCompiledError(decodeErr, node.typ)
		}
		buf = append(buf, element)
	}
	if buf == nil {
		buf = make([]byte, 0)
	}
	value.SetBytes(buf)
	return nil
}

// resolveDecodeHops walks embedded pointer hops toward a flattened field,
// allocating nil intermediates like encoding/json, which also only rejects
// unexported embedded pointers at the moment an allocation is required.
func resolveDecodeHops(dst unsafe.Pointer, hops []typedFieldHop, offset int) (unsafe.Pointer, error) {
	for i := range hops {
		hop := &hops[i]
		slot := (*unsafe.Pointer)(unsafe.Add(dst, hop.offset))
		pointer := *slot
		if pointer == nil {
			if hop.unexported {
				return nil, &DecodeError{Offset: offset, TypeName: hop.pointee.String(), Reason: "cannot set embedded pointer to unexported struct type"}
			}
			value := reflect.New(hop.pointee)
			pointer = value.UnsafePointer()
			*slot = pointer
			// Only raw pointers were stored; pin the allocation until the
			// slot write is visible to the collector.
			runtime.KeepAlive(value)
		}
		dst = pointer
	}
	return dst, nil
}

// resolveResetHops walks hops without allocating; a nil link means the field
// is already zero.
func resolveResetHops(dst unsafe.Pointer, hops []typedFieldHop) unsafe.Pointer {
	for i := range hops {
		pointer := *(*unsafe.Pointer)(unsafe.Add(dst, hops[i].offset))
		if pointer == nil {
			return nil
		}
		dst = pointer
	}
	return dst
}

func allocateTypedPointer(node *typedNode, dst unsafe.Pointer) unsafe.Pointer {
	value := reflect.New(node.elem.typ)
	pointer := value.UnsafePointer()
	*(*unsafe.Pointer)(dst) = pointer
	runtime.KeepAlive(value)
	return pointer
}

func setTypedEmptySlice(node *typedNode, dst unsafe.Pointer) {
	slice := typedSliceAt(node.typ, dst)
	slice.setEmpty()
}

func setTypedSliceZero(node *typedNode, dst unsafe.Pointer) {
	slice := typedSliceAt(node.typ, dst)
	slice.setZero()
}

func retagCompiledError(err error, typ reflect.Type) error {
	if typed, ok := err.(*DecodeError); ok {
		typed.Type = typ
		typed.TypeName = ""
	}
	return err
}
