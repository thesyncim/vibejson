package simdjson

import (
	"encoding"
	"errors"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/thesyncim/simdjson/internal/byteview"
)

// dynamicEncodeNodes caches one compiled encode plan per concrete type seen
// inside an interface value. These plans run inside whichever static plan
// encountered the interface, so they are compiled with dynamic set and must
// never carry indexes into a plan-specific scratch; see typedCompiler.dynamic.
var dynamicEncodeNodes sync.Map

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

func dynamicEncodeNode(typ reflect.Type, escapeHTML bool) (*typedNode, error) {
	key := dynamicEncodeKey{typ: typ, escapeHTML: escapeHTML}
	if entry, ok := dynamicEncodeNodes.Load(key); ok {
		cached := entry.(*dynamicEncodeEntry)
		return cached.node, cached.err
	}
	compiler := newTypedCompiler(typedCompileEncode)
	compiler.escapeHTML = escapeHTML
	compiler.dynamic = true
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
	entry, _ := dynamicEncodeNodes.LoadOrStore(key, candidate)
	cached := entry.(*dynamicEncodeEntry)
	return cached.node, cached.err
}

func dynamicEncodeBoxFor(typ reflect.Type, escapeHTML bool) (*dynamicEncodeEntry, error) {
	key := dynamicEncodeKey{typ: typ, escapeHTML: escapeHTML}
	if entry, ok := dynamicEncodeNodes.Load(key); ok {
		cached := entry.(*dynamicEncodeEntry)
		return cached, cached.err
	}
	if _, err := dynamicEncodeNode(typ, escapeHTML); err != nil {
		return nil, err
	}
	entry, _ := dynamicEncodeNodes.Load(key)
	cached := entry.(*dynamicEncodeEntry)
	return cached, cached.err
}

// encodeAny encodes the concrete value stored in an empty interface,
// compiling a plan for its type on first use.
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
	if encoderHasDepthLimit && e.depth >= defaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	return e.encodeDynamicValue(reflect.ValueOf(value))
}

// encodeDynamicValue encodes a concrete reflect value through a cached plan
// for its type.
func (e *encodeState) encodeDynamicValue(value reflect.Value) error {
	if encoderHasDepthLimit && e.depth >= defaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	entry, err := dynamicEncodeBoxFor(value.Type(), e.escapeHTML)
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

// encodeNonAddressable encodes a value reached without addressability — a map
// value or interface content — where encoding/json cannot take the address to
// call a pointer-receiver marshaler. The nonAddr flag it raises reroutes those
// marshalers to their default encoding at the one dispatch point that cares,
// and pointers and slices below restore addressability by lowering it again.
func (e *encodeState) encodeNonAddressable(node *typedNode, src unsafe.Pointer) error {
	if node.encHasPtrMarshaler {
		return e.encodeNonAddressableMarshaler(node, src)
	}
	return e.encodeKind(node, src, node.encNonAddrKind)
}

// encodeNonAddressableMarshaler is the cold half of encodeNonAddressable, for
// the rare types that reach a pointer-receiver marshaler through fields or
// elements. Splitting it out keeps the common encodeNonAddressable a single
// tail call that inlines into the map and interface loops.
//
//go:noinline
func (e *encodeState) encodeNonAddressableMarshaler(node *typedNode, src unsafe.Pointer) error {
	prev := e.nonAddr
	e.nonAddr = true
	err := e.encodeKind(node, src, node.encKind)
	e.nonAddr = prev
	return err
}

// encodeMap writes a map with string keys as an object with byte-sorted
// members, matching encoding/json. Values are copied into one reusable
// addressable element before encoding.
func (e *encodeState) encodeMap(node *typedNode, src unsafe.Pointer) error {
	mapValue := reflect.NewAt(node.typ, src).Elem()
	return e.encodeMapValue(node, mapValue, nil)
}

// encodeMapValue is the shared map encoder for addressable compiled values and
// non-addressable maps reached through interfaces. A dynamic key box belongs
// to the concrete-type pool entry, so interface maps need no per-call reflect
// allocation while preserving the addressability rules of encoding/json.
func (e *encodeState) encodeMapValue(node *typedNode, mapValue reflect.Value, dynamic *dynamicEncodeBox) error {
	if mapValue.IsNil() {
		e.dst = append(e.dst, "null"...)
		return nil
	}
	if encoderDetectCycles {
		key := encoderCycleKey{typ: node.typ, ptr: mapValue.UnsafePointer(), kind: encoderCycleMap}
		if err := e.enterReference(key); err != nil {
			return err
		}
		defer e.leaveReference(key)
	}
	if encoderHasDepthLimit && e.depth >= defaultMaxDepth {
		return &EncodeError{Reason: "maximum nesting depth exceeded"}
	}
	mapLen := mapValue.Len()
	retainScratch := mapLen <= node.encScratchLimit
	e.depth++
	numericKeys := !node.mapKeyTextEncode && (node.mapKeyKind == mapKeyInt || node.mapKeyKind == mapKeyUint)
	stringKeys := !node.mapKeyTextEncode && node.mapKeyKind == mapKeyString
	// The entry list, numeric-key arena, and iterator come from the per-call
	// scratch so sorted map encoding does not allocate per map. Ownership moves
	// to this call while it runs; a nested map sees them taken and allocates its
	// own. keyArena backs rendered numeric key names, which entries alias, so
	// both recycle together and nothing may retain a name past this call (error
	// paths clone before storing one in an EncodeError).
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

	// Copy each key into a reserved box with SetIterKey and each value into its
	// own backing slot with SetIterValue, so neither MapIter.Key nor
	// MapIter.Value allocates a fresh value per entry. Independent slots keep a
	// value that recurses into the same map type correct.
	var keyBox reflect.Value
	if dynamic != nil {
		keyBox = dynamic.mapKey
	}
	if keyBox.IsValid() {
		// The concrete-type pool owns this box for the duration of the dynamic
		// encode, including recursive maps of the same key type.
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

	// mapValue remains GC-visible while the iterator is bound. releaseMapScratch
	// unbinds pooled iterators before they can outlive this operation.
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

	// The value type is loop invariant, so the non-addressable dispatch is
	// resolved once: ordinary values encode directly, and only marshaler-
	// bearing types raise the flag.
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
			// Clone: numeric key names alias the pooled arena, and the
			// error must outlive this call's ownership of it.
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

// releaseMapScratch returns bounded working state to the scratch and records the
// largest dirty prefix for one typed clear at operation reset.
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
	// A nested call may return its operation-local backing while the outer call
	// still owns the original pooled slice. Keep the first returned backing and
	// let the outer release leave that occupied slot unchanged.
	if scratch == nil || scratch.mapEntries != nil {
		return
	}
	used := len(entries)
	if scratch.mapEntriesUsed > used {
		used = scratch.mapEntriesUsed
		if used > cap(entries) {
			// A smaller nested backing must not inherit the outer backing's dirty
			// prefix. Leave this slot open so the outer release restores it.
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
	case typedAny, typedIface:
		return reflect.NewAt(node.typ, src).Elem().IsNil()
	default:
		return false
	}
}
