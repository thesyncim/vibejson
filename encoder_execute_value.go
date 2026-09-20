package vibejson

import (
	"encoding"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
)

// dynamicEncodeNodes caches plans for concrete types seen in interfaces.
var dynamicEncodeNodes sync.Map
var dynamicEncodeInlineNodes sync.Map

type dynamicEncodeEntry struct {
	node      *typedNode
	err       error
	retainBox bool
	pool      sync.Pool
}

type dynamicEncodeBox struct {
	value       reflect.Value
	mapKey      reflect.Value
	mapEntries  []mapEncodeEntry
	mapKeyArena []byte
	mapIter     *reflect.MapIter
	mapBacking  reflect.Value
}

type dynamicEncodeKey struct {
	typ        reflect.Type
	escapeHTML bool
}

// Both modes share construction and retention rules but use separate caches.
func dynamicEncodeBoxFor(typ reflect.Type, escapeHTML bool, cache *sync.Map) (*dynamicEncodeEntry, error) {
	key := dynamicEncodeKey{typ: typ, escapeHTML: escapeHTML}
	if entry, ok := cache.Load(key); ok {
		cached := entry.(*dynamicEncodeEntry)
		return cached, cached.err
	}
	return dynamicEncodeBoxForSlow(typ, escapeHTML, cache, key)
}

// dynamicEncodeBoxForSlow compiles a cache miss and initializes its box.
//
//go:noinline
func dynamicEncodeBoxForSlow(typ reflect.Type, escapeHTML bool, cache *sync.Map, key dynamicEncodeKey) (*dynamicEncodeEntry, error) {
	compiler := newTypedCompiler(typedCompileEncode)
	compiler.escapeHTML = escapeHTML
	compiler.dynamic = true
	compiler.inlineFields = cache == &dynamicEncodeInlineNodes
	node, err := compiler.compile(typ, typ.String())
	if err == nil {
		computeEncPtrMarshaler(node, make(map[*typedNode]bool))
	}
	candidate := &dynamicEncodeEntry{node: node, err: err}
	if err == nil {
		candidate.retainBox = typ.Size() <= encoderValueBackingRetentionBytes
		candidate.pool.New = func() any {
			box := &dynamicEncodeBox{value: reflect.New(typ)}
			if typ.Kind() == reflect.Map {
				box.mapKey = reflect.New(typ.Key()).Elem()
			}
			return box
		}
	}
	entry, _ := cache.LoadOrStore(key, candidate)
	cached := entry.(*dynamicEncodeEntry)
	return cached, cached.err
}

// encodeAny encodes the concrete value stored in an interface.
func (e *encodeState) encodeAny(src unsafe.Pointer) error {
	value := *(*any)(src)
	switch concrete := value.(type) {
	case nil:
		e.dst = append(e.dst, "null"...)
		return nil
	case bool:
		if concrete {
			e.dst = append(e.dst, "true"...)
		} else {
			e.dst = append(e.dst, "false"...)
		}
		return nil
	case string:
		e.dst = appendEncodedJSONString(e.dst, concrete, e.escapeHTML)
		return nil
	case float64:
		return e.encodeFloat(concrete, 64)
	case int:
		e.dst = appendCompactInt(e.dst, int64(concrete))
		return nil
	case int64:
		e.dst = appendCompactInt(e.dst, concrete)
		return nil
	}
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	return e.encodeDynamicValue(reflect.ValueOf(value), &dynamicEncodeNodes)
}

// encodeAnyInline is the opt-in dynamic-interface specialization.
func (e *encodeState) encodeAnyInline(src unsafe.Pointer) error {
	value := *(*any)(src)
	switch concrete := value.(type) {
	case nil:
		e.dst = append(e.dst, "null"...)
		return nil
	case bool:
		if concrete {
			e.dst = append(e.dst, "true"...)
		} else {
			e.dst = append(e.dst, "false"...)
		}
		return nil
	case string:
		e.dst = appendEncodedJSONString(e.dst, concrete, e.escapeHTML)
		return nil
	case float64:
		return e.encodeFloat(concrete, 64)
	case int:
		e.dst = appendCompactInt(e.dst, int64(concrete))
		return nil
	case int64:
		e.dst = appendCompactInt(e.dst, concrete)
		return nil
	}
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	return e.encodeDynamicValue(reflect.ValueOf(value), &dynamicEncodeInlineNodes)
}

// encodeDynamicValue encodes a concrete value through its cached plan.
func (e *encodeState) encodeDynamicValue(value reflect.Value, cache *sync.Map) error {
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	entry, err := dynamicEncodeBoxFor(value.Type(), e.escapeHTML, cache)
	if err != nil {
		return &EncodeError{Reason: err.Error()}
	}
	var box *dynamicEncodeBox
	if entry.retainBox {
		box = entry.pool.Get().(*dynamicEncodeBox)
	} else {
		box = entry.pool.New().(*dynamicEncodeBox)
	}
	box.value.Elem().Set(value)
	e.depth++
	var encodeErr error
	if entry.node.baseKind == typedMap {
		encodeErr = e.encodeMapValue(entry.node, box.value.Elem(), box)
	} else {
		encodeErr = e.encodeNonAddressable(entry.node, box.value.UnsafePointer())
	}
	e.depth--
	box.value.Elem().SetZero()
	if box.mapKey.IsValid() {
		box.mapKey.SetZero()
	}
	if entry.retainBox {
		entry.pool.Put(box)
	}
	return encodeErr
}

// encodeNonAddressable encodes a map or interface value without addressability.
func (e *encodeState) encodeNonAddressable(node *typedNode, src unsafe.Pointer) error {
	if node.encHasPtrMarshaler {
		return e.encodeNonAddressableMarshaler(node, src)
	}
	return e.encodeKind(node, src, node.encNonAddrKind)
}

// encodeNonAddressableMarshaler handles the cold struct/array envelope for a
// pointer-receiver marshaler.
//
//go:noinline
func (e *encodeState) encodeNonAddressableMarshaler(node *typedNode, src unsafe.Pointer) error {
	switch node.encNonAddrKind {
	case typedStruct:
		return e.encodeNonAddressableStruct(node, src)
	case typedArray:
		return e.encodeNonAddressableArray(node, src)
	default:
		return e.encodeKind(node, src, node.encNonAddrKind)
	}
}

// encodeMap writes a map as a byte-sorted object.
func (e *encodeState) encodeMap(node *typedNode, src unsafe.Pointer) error {
	mapValue := reflect.NewAt(node.typ, src).Elem()
	return e.encodeMapValue(node, mapValue, nil)
}

// encodeMapValue is shared by addressable and interface maps.
func (e *encodeState) encodeMapValue(node *typedNode, mapValue reflect.Value, dynamic *dynamicEncodeBox) error {
	if mapValue.IsNil() {
		e.dst = append(e.dst, "null"...)
		return nil
	}
	if e.depth >= DefaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	mapLen := mapValue.Len()
	retainScratch := mapLen <= node.encScratchLimit
	e.depth++
	numericKeys := !node.mapKeyTextEncode && (node.mapKeyKind == mapKeyInt || node.mapKeyKind == mapKeyUint)
	stringKeys := !node.mapKeyTextEncode && node.mapKeyKind == mapKeyString
	// Reuse per-call entries, numeric-key storage, and iterators when possible.
	var entries []mapEncodeEntry
	var keyArena []byte
	var iterator *reflect.MapIter
	scratch := e.scratch
	useDynamicScratch := dynamic != nil && retainScratch
	if useDynamicScratch {
		entries = dynamic.mapEntries[:0]
		keyArena = dynamic.mapKeyArena[:0]
		iterator = dynamic.mapIter
		dynamic.mapEntries = nil
		dynamic.mapKeyArena = nil
		dynamic.mapIter = nil
	} else if scratch != nil && retainScratch {
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
	if numericKeys && mapLen <= int(^uint(0)>>1)/20 {
		required := mapLen * 20
		if cap(keyArena) < required {
			keyArena = make([]byte, 0, required)
		}
	}

	// Copy keys and values into addressable slots before sorting.
	var keyBox reflect.Value
	if dynamic != nil {
		keyBox = dynamic.mapKey
	}
	if keyBox.IsValid() {
		// The dynamic pool owns this box for the duration of the encode.
	} else if scratch != nil && node.encMapKey >= 0 {
		keyBox = scratch.marshalers[node.encMapKey].value
	} else {
		keyBox = reflect.New(node.typ.Key()).Elem()
	}
	var backing reflect.Value
	if useDynamicScratch {
		backing = dynamic.mapBacking
		dynamic.mapBacking = reflect.Value{}
		if !backing.IsValid() || backing.Len() < mapLen {
			backing = reflect.MakeSlice(reflect.SliceOf(node.elem.typ), mapLen, mapLen)
		}
	} else if retainScratch {
		backing = e.takeValueBacking(node.encBacking, node.elem.typ, mapLen)
	} else {
		backing = reflect.MakeSlice(reflect.SliceOf(node.elem.typ), mapLen, mapLen)
	}

	// Keep mapValue visible while the iterator is bound.
	if iterator == nil {
		iterator = mapValue.MapRange()
	} else {
		iterator.Reset(mapValue)
	}

	switch {
	case numericKeys:
		for slot := 0; iterator.Next(); slot++ {
			keyBox.SetIterKey(iterator)
			start := len(keyArena)
			if node.mapKeyKind == mapKeyInt {
				value := keyBox.Int()
				if value < 0 {
					keyArena = appendCompactInt(keyArena, value)
				} else {
					keyArena = appendCompactUint(keyArena, uint64(value))
				}
			} else {
				keyArena = appendCompactUint(keyArena, keyBox.Uint())
			}
			name := byteview.String(keyArena[start:])
			valueSlot := backing.Index(slot)
			valueSlot.SetIterValue(iterator)
			entries = append(entries, mapEncodeEntry{name: name, value: valueSlot})
		}
	case stringKeys:
		for slot := 0; iterator.Next(); slot++ {
			keyBox.SetIterKey(iterator)
			valueSlot := backing.Index(slot)
			valueSlot.SetIterValue(iterator)
			entries = append(entries, mapEncodeEntry{name: keyBox.String(), value: valueSlot})
		}
	default:
		for slot := 0; iterator.Next(); slot++ {
			keyBox.SetIterKey(iterator)
			name, err := mapKeyName(node, keyBox)
			if err != nil {
				e.releaseMapValueBacking(node, backing, dynamic, useDynamicScratch, len(entries))
				e.releaseMapScratch(entries, keyArena, iterator, dynamic, useDynamicScratch)
				e.depth--
				return &EncodeError{Reason: err.Error()}
			}
			valueSlot := backing.Index(slot)
			valueSlot.SetIterValue(iterator)
			entries = append(entries, mapEncodeEntry{name: name, value: valueSlot})
		}
	}
	slices.SortFunc(entries, func(a, b mapEncodeEntry) int { return strings.Compare(a.name, b.name) })

	// Resolve non-addressable dispatch once for the value type.
	elemHasMarshaler := node.elem.encHasPtrMarshaler
	e.dst = append(e.dst, '{')
	for i := range entries {
		if i > 0 {
			e.dst = append(e.dst, ',')
		}
		e.dst = appendEncodedJSONString(e.dst, entries[i].name, e.escapeHTML)
		e.dst = append(e.dst, ':')
		valuePtr := entries[i].value.Addr().UnsafePointer()
		var err error
		if elemHasMarshaler {
			err = e.encodeNonAddressableMarshaler(node.elem, valuePtr)
		} else {
			err = e.encodeKind(node.elem, valuePtr, node.elem.encNonAddrKind)
		}
		if err != nil {
			// Numeric key names alias the pooled arena; clone error paths.
			name := strings.Clone(entries[i].name)
			e.releaseMapValueBacking(node, backing, dynamic, useDynamicScratch, mapLen)
			e.releaseMapScratch(entries, keyArena, iterator, dynamic, useDynamicScratch)
			e.depth--
			return prependEncodePathField(err, name)
		}
	}
	e.dst = append(e.dst, '}')
	e.releaseMapValueBacking(node, backing, dynamic, useDynamicScratch, mapLen)
	e.releaseMapScratch(entries, keyArena, iterator, dynamic, useDynamicScratch)
	e.depth--
	return nil
}

// releaseMapScratch returns working state to the scratch pool.
func (e *encodeState) releaseMapScratch(entries []mapEncodeEntry, keyArena []byte, iterator *reflect.MapIter, dynamic *dynamicEncodeBox, useDynamic bool) {
	if useDynamic {
		clear(entries)
		dynamic.mapEntries = entries[:0]
		dynamic.mapKeyArena = keyArena[:0]
		iterator.Reset(reflect.Value{})
		dynamic.mapIter = iterator
		return
	}
	scratch := e.scratch
	// Keep the outer backing when a nested call returned a different slice.
	if scratch == nil || scratch.mapEntries != nil {
		return
	}
	used := len(entries)
	if scratch.mapEntriesUsed > used {
		used = scratch.mapEntriesUsed
		if used > cap(entries) {
			// Do not inherit a dirty prefix from a smaller nested backing.
			return
		}
	}
	scratch.mapEntriesUsed = used
	scratch.mapEntries = entries[:0]
	scratch.mapKeyArena = keyArena[:0]
	if scratch.mapIter == nil {
		iterator.Reset(reflect.Value{})
		scratch.mapIter = iterator
	}
}

func (e *encodeState) releaseMapValueBacking(node *typedNode, backing reflect.Value, dynamic *dynamicEncodeBox, useDynamic bool, used int) {
	if useDynamic {
		clearEncoderValueBacking(backing, used)
		dynamic.mapBacking = backing
		return
	}
	if backing.Len() <= node.encScratchLimit {
		e.releaseValueBackingPrefix(node.encBacking, backing, node.elem.typ, used)
	}
}

type mapEncodeEntry struct {
	name  string
	value reflect.Value
}

// mapKeyName renders a map key as its JSON member name. The compiled node
// records the active encoding/json release's TextMarshaler precedence; the
// remaining key kinds use strings and base 10 integers.
func mapKeyName(node *typedNode, key reflect.Value) (string, error) {
	if node.mapKeyTextEncode {
		// encoding/json renders a nil pointer key as the empty name
		// instead of calling its method.
		if key.Kind() == reflect.Pointer && key.IsNil() {
			return "", nil
		}
		marshaler := key.Interface().(encoding.TextMarshaler)
		text, err := marshaler.MarshalText()
		if err != nil {
			return "", err
		}
		return string(text), nil
	}
	switch node.mapKeyKind {
	case mapKeyString:
		return key.String(), nil
	case mapKeyInt:
		return strconv.FormatInt(key.Int(), 10), nil
	case mapKeyUint:
		return strconv.FormatUint(key.Uint(), 10), nil
	default:
		return "", errors.New("map key type " + key.Type().String() + " cannot be encoded")
	}
}

// encodeQuoted writes a scalar tagged with the string option: the value's
// JSON form wrapped in a string. Non-string scalars contain no characters
// that need escaping, so they are wrapped directly; strings are encoded and
// then re-encoded as string contents, like encoding/json.
func (e *encodeState) encodeQuoted(node *typedNode, src unsafe.Pointer) error {
	if node.baseKind == typedPointer {
		pointer := *(*unsafe.Pointer)(src)
		if pointer == nil {
			e.dst = append(e.dst, "null"...)
			return nil
		}
		node = node.elem
		src = pointer
	}
	if node.baseKind == typedString {
		inner := appendEncodedJSONString(nil, *(*string)(src), e.escapeHTML)
		e.dst = appendEncodedJSONString(e.dst, string(inner), false)
		return nil
	}
	e.dst = append(e.dst, '"')
	if err := e.encode(node, src); err != nil {
		return err
	}
	e.dst = append(e.dst, '"')
	return nil
}

// encodeFloat matches encoding/json: shortest representation, with the 'e'
// format only for large or small magnitudes, and a trimmed exponent digit.
func (e *encodeState) encodeFloat(value float64, bits int) error {
	dst, err := appendJSONFloat(e.dst, value, bits)
	if err != nil {
		return err
	}
	e.dst = dst
	return nil
}

// encodeNumberLiteral emits a json.Number after validating its spelling,
// matching encoding/json's handling including the empty-string default.
func (e *encodeState) encodeNumberLiteral(literal string) error {
	if literal == "" {
		literal = "0"
	}
	if !validNumber([]byte(literal)) {
		return &EncodeError{Reason: "invalid number literal " + strconv.Quote(literal)}
	}
	e.dst = append(e.dst, literal...)
	return nil
}

// typedValueIsEmpty reports the omitempty emptiness of the value at src,
// matching encoding/json: false, zero numbers, empty strings, nil pointers,
// and zero-length slices.
func typedValueIsEmpty(node *typedNode, src unsafe.Pointer) bool {
	switch node.baseKind {
	case typedBool:
		return !*(*bool)(src)
	case typedString, typedNumber:
		return len(*(*string)(src)) == 0
	case typedInt:
		switch node.bits {
		case 8:
			return *(*int8)(src) == 0
		case 16:
			return *(*int16)(src) == 0
		case 32:
			return *(*int32)(src) == 0
		default:
			return *(*int64)(src) == 0
		}
	case typedUint:
		switch node.bits {
		case 8:
			return *(*uint8)(src) == 0
		case 16:
			return *(*uint16)(src) == 0
		case 32:
			return *(*uint32)(src) == 0
		default:
			return *(*uint64)(src) == 0
		}
	case typedFloat:
		if node.bits == 32 {
			return *(*float32)(src) == 0
		}
		return *(*float64)(src) == 0
	case typedSlice, typedBytes:
		return reflect.NewAt(node.typ, src).Elem().Len() == 0
	case typedArray:
		return node.length == 0
	case typedPointer:
		return *(*unsafe.Pointer)(src) == nil
	case typedMap:
		return reflect.NewAt(node.typ, src).Elem().Len() == 0
	case typedAny, typedIface, typedAnyInline, typedIfaceInline:
		return reflect.NewAt(node.typ, src).Elem().IsNil()
	default:
		return false
	}
}

func typedValueShouldOmit(node *typedNode, src unsafe.Pointer, omit typedOmit) bool {
	if omit&typedOmitEmpty != 0 && typedValueIsEmpty(node, src) {
		return true
	}
	return omit&typedOmitZero != 0 && typedValueIsZero(node, src, typedZeroMethod(omit>>typedOmitZeroMethodShift))
}

// typedValueIsZero matches encoding/json's omitzero rules. Method selection is
// compiled into typedOmit, keeping reflection-based interface construction off
// the ordinary field path and using it only when omitzero requests IsZero.
func typedValueIsZero(node *typedNode, src unsafe.Pointer, method typedZeroMethod) bool {
	if method != typedZeroDefault {
		value := reflect.NewAt(node.typ, src).Elem()
		switch method {
		case typedZeroInterface:
			return value.IsNil() ||
				(value.Elem().Kind() == reflect.Pointer && value.Elem().IsNil()) ||
				value.Interface().(isZeroer).IsZero()
		case typedZeroPointer:
			return value.IsNil() || value.Interface().(isZeroer).IsZero()
		case typedZeroValue:
			return value.Interface().(isZeroer).IsZero()
		case typedZeroAddress:
			return value.Addr().Interface().(isZeroer).IsZero()
		}
	}
	switch node.baseKind {
	case typedBool:
		return !*(*bool)(src)
	case typedString, typedNumber:
		return len(*(*string)(src)) == 0
	case typedInt:
		switch node.bits {
		case 8:
			return *(*int8)(src) == 0
		case 16:
			return *(*int16)(src) == 0
		case 32:
			return *(*int32)(src) == 0
		default:
			return *(*int64)(src) == 0
		}
	case typedUint:
		switch node.bits {
		case 8:
			return *(*uint8)(src) == 0
		case 16:
			return *(*uint16)(src) == 0
		case 32:
			return *(*uint32)(src) == 0
		default:
			return *(*uint64)(src) == 0
		}
	case typedFloat:
		if node.bits == 32 {
			return *(*float32)(src) == 0
		}
		return *(*float64)(src) == 0
	case typedPointer:
		return *(*unsafe.Pointer)(src) == nil
	case typedSlice, typedBytes, typedMap, typedAny, typedIface, typedAnyInline, typedIfaceInline:
		return reflect.NewAt(node.typ, src).Elem().IsNil()
	default:
		return reflect.NewAt(node.typ, src).Elem().IsZero()
	}
}
