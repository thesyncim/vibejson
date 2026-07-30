package vibejson

import (
	"encoding"
	"encoding/base64"
	"errors"
	"reflect"
	"runtime"
	"strconv"
	"sync"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
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

// decodeCompiledPointerReplace is compiled only into Replace plans. It reuses
// a unique pointee, but detaches when another destination already claimed the
// same address so a later field cannot overwrite an earlier decoded member.
// Keeping this as a cold kind leaves default pointer dispatch unchanged.
func (cursor *decoderCursor) decodeCompiledPointerReplace(node *typedNode, dst unsafe.Pointer) error {
	null := false
	if !cursor.notNullFast() {
		var err error
		null, err = cursor.TryNull()
		if err != nil {
			return err
		}
	}
	tracked := cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil
	var previousScope uint32
	var scoped bool
	if tracked {
		previousScope, scoped = cursor.beginReplaceScope(dst)
	}
	if null {
		*(*unsafe.Pointer)(dst) = nil
		if tracked {
			cursor.refreshReplaceReference(node, dst)
		}
		cursor.endReplaceScope(previousScope, scoped)
		return nil
	}
	var pointer unsafe.Pointer
	if tracked {
		pointer = cursor.reuseTrackedReplacePointer(node, dst)
	} else {
		pointer = *(*unsafe.Pointer)(dst)
		aliasesDestination := cursor.flags&decoderReplaceWideDestination != 0
		replaceDestination, replaceSpan := cursor.replaceDestinationRange()
		if pointer != nil && !aliasesDestination && replaceDestination != nil {
			pointerStart := uintptr(pointer)
			destinationStart := uintptr(replaceDestination)
			if pointerStart >= destinationStart {
				aliasesDestination = pointerStart-destinationStart < uintptr(replaceSpan)
			} else {
				aliasesDestination = destinationStart-pointerStart < node.elem.size
			}
		}
		if pointer == nil || aliasesDestination {
			pointer = allocateTypedPointer(node, dst)
		}
	}
	var err error
	switch node.elem.kind {
	case typedStruct:
		err = cursor.decodeCompiledStruct(node.elem, pointer)
	case typedSlice:
		err = cursor.decodeCompiledSlice(node.elem, pointer)
	case typedArray:
		err = cursor.decodeCompiledArray(node.elem, pointer)
	case typedPointer:
		err = cursor.decodeCompiledPointer(node.elem, pointer)
	case typedMap:
		err = cursor.decodeCompiledMap(node.elem, pointer)
	default:
		err = cursor.decodeCompiled(node.elem, pointer)
	}
	if err == nil && tracked {
		cursor.refreshReplaceReference(node, dst)
	}
	cursor.endReplaceScope(previousScope, scoped)
	return err
}

func (cursor *decoderCursor) reuseDestinationPointer(node *typedNode, dst unsafe.Pointer) unsafe.Pointer {
	pointer := *(*unsafe.Pointer)(dst)
	if pointer != nil && !cursor.pointerAliasesDestination(pointer, node.elem.size) {
		return pointer
	}
	return allocateTypedPointer(node, dst)
}

func (cursor *decoderCursor) reuseReplacePointer(node *typedNode, dst unsafe.Pointer) unsafe.Pointer {
	if cursor.state == nil || cursor.state.operation == nil ||
		cursor.state.operation.replace == nil {
		return cursor.reuseDestinationPointer(node, dst)
	}
	return cursor.reuseTrackedReplacePointer(node, dst)
}

func (cursor *decoderCursor) reuseTrackedReplacePointer(node *typedNode, dst unsafe.Pointer) unsafe.Pointer {
	pointer := *(*unsafe.Pointer)(dst)
	if pointer != nil {
		reference, _ := cursor.currentReplaceReference(node, dst)
		if cursor.replaceReferenceSeen(reference) {
			pointer = nil
		}
	}
	if pointer == nil {
		return allocateTypedPointer(node, dst)
	}
	return pointer
}

// detachReplaceAlias preserves capacity for the first slice or map that owns a
// backing store, but detaches any later destination whose storage aliases it.
// Without this, overlapping slice windows and shared maps let a later field
// overwrite a value already decoded into an earlier field.
func (cursor *decoderCursor) detachReplaceAlias(node *typedNode, dst unsafe.Pointer) {
	tracked := cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil
	replaceDestination, _ := cursor.replaceDestinationRange()
	if !tracked && replaceDestination == nil {
		return
	}
	reference, live := cursor.currentReplaceReference(node, dst)
	if !live {
		return
	}
	if cursor.replaceReferenceSeen(reference) {
		reflect.NewAt(node.typ, dst).Elem().SetZero()
	}
}

// decodeCompiledReferenceReplace keeps Replace-only scope maintenance out of
// the shared inline-kind dispatcher. The extra switch is confined to the cold
// Replace plan while ordinary compiled decoders retain a compact hot path.
func (cursor *decoderCursor) decodeCompiledReferenceReplace(node *typedNode, dst unsafe.Pointer) error {
	previousScope, scoped := cursor.beginReplaceScope(dst)
	cursor.detachReplaceAlias(node, dst)
	var err error
	switch node.kind {
	case typedSliceReplace:
		err = cursor.decodeCompiledSlice(node, dst)
	case typedMapReplace:
		err = cursor.decodeCompiledMap(node, dst)
	case typedBytesReplace:
		err = cursor.decodeCompiledBytes(node, dst)
	default:
		err = &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "invalid Replace reference operation"}
	}
	if err == nil {
		cursor.refreshReplaceReference(node, dst)
	}
	cursor.endReplaceScope(previousScope, scoped)
	return err
}

func (cursor *decoderCursor) currentReplaceReference(node *typedNode, dst unsafe.Pointer) (decoderReplaceReference, bool) {
	reference := decoderReplaceReference{owner: dst}
	if cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil {
		reference.scope = cursor.state.operation.replace.currentScope
	}
	if node.baseKind == typedPointer {
		reference.kind = decoderReplacePointer
		reference.ptr = *(*unsafe.Pointer)(dst)
		reference.span = node.elem.size
		return reference, reference.ptr != nil
	}
	value := reflect.NewAt(node.typ, dst).Elem()
	if value.IsNil() {
		switch node.baseKind {
		case typedSlice, typedBytes:
			reference.kind = decoderReplaceSlice
		case typedMap:
			reference.kind = decoderReplaceMap
		}
		return reference, false
	}
	switch node.baseKind {
	case typedSlice, typedBytes:
		reference.kind = decoderReplaceSlice
		if value.Cap() == 0 {
			return reference, false
		}
		start := value.UnsafePointer()
		size := node.typ.Elem().Size()
		if start == nil || size == 0 {
			return reference, false
		}
		span := uintptr(value.Cap()) * size
		reference.ptr, reference.span = start, span
	case typedMap:
		reference.kind = decoderReplaceMap
		start := value.UnsafePointer()
		if start == nil {
			return reference, false
		}
		reference.ptr = start
	default:
		return reference, false
	}
	return reference, true
}

func (cursor *decoderCursor) refreshReplaceReference(node *typedNode, dst unsafe.Pointer) {
	if cursor.state == nil || cursor.state.operation == nil ||
		cursor.state.operation.replace == nil {
		return
	}
	reference, live := cursor.currentReplaceReference(node, dst)
	cursor.storeReplaceReference(reference, live)
}

func (cursor *decoderCursor) storeReplaceReference(reference decoderReplaceReference, live bool) {
	state := cursor.state.operation.replace
	inlineCount := state.count
	if inlineCount > len(state.refs) {
		inlineCount = len(state.refs)
	}
	for i := 0; i < inlineCount; i++ {
		previous := state.refs[i]
		if previous.kind == reference.kind && previous.scope == reference.scope &&
			previous.owner == reference.owner {
			state.refs[i] = reference
			return
		}
	}
	for i := 0; i < state.count-len(state.refs); i++ {
		previous := state.overflow[i]
		if previous.kind == reference.kind && previous.scope == reference.scope &&
			previous.owner == reference.owner {
			state.overflow[i] = reference
			return
		}
	}
	if live {
		appendReplaceReference(state, reference)
	}
}

func (cursor *decoderCursor) replaceReferenceSeen(reference decoderReplaceReference) bool {
	if (reference.kind == decoderReplacePointer ||
		reference.kind == decoderReplaceSlice) &&
		cursor.pointerAliasesDestination(reference.ptr, reference.span) {
		return true
	}
	if cursor.state == nil {
		return false
	}
	if cursor.state.operation == nil || cursor.state.operation.replace == nil {
		return false
	}
	state := cursor.state.operation.replace
	sameInline := -1
	sameOverflow := -1
	inlineCount := state.count
	if inlineCount > len(state.refs) {
		inlineCount = len(state.refs)
	}
	for i := 0; i < inlineCount; i++ {
		previous := state.refs[i]
		if previous.kind == reference.kind && previous.scope == reference.scope &&
			previous.owner == reference.owner {
			sameInline = i
			continue
		}
		if replaceReferencesAlias(previous, reference) {
			return true
		}
	}
	for i := 0; i < state.count-len(state.refs); i++ {
		previous := state.overflow[i]
		if previous.kind == reference.kind && previous.scope == reference.scope &&
			previous.owner == reference.owner {
			sameOverflow = i
			continue
		}
		if replaceReferencesAlias(previous, reference) {
			return true
		}
	}
	if sameInline >= 0 {
		state.refs[sameInline] = reference
		return false
	}
	if sameOverflow >= 0 {
		state.overflow[sameOverflow] = reference
		return false
	}
	appendReplaceReference(state, reference)
	return false
}

func (cursor *decoderCursor) pointerAliasesDestination(pointer unsafe.Pointer, span uintptr) bool {
	if cursor.flags&decoderReplaceWideDestination != 0 {
		return true
	}
	replaceDestination, replaceSpan := cursor.replaceDestinationRange()
	if replaceDestination == nil {
		return false
	}
	// Both ranges belong to live Go objects, so neither can wrap the address
	// space. Express overlap as unsigned distance in each direction: this avoids
	// the four overflow-guarded endpoints used by the general reference tracker.
	distance := uintptr(pointer) - uintptr(replaceDestination)
	return distance < uintptr(replaceSpan) || -distance < span
}

func appendReplaceReference(state *decoderReplaceState, reference decoderReplaceReference) {
	if state.count < len(state.refs) {
		state.refs[state.count] = reference
	} else {
		index := state.count - len(state.refs)
		if index == len(state.overflow) {
			state.overflow = append(state.overflow, reference)
		} else {
			state.overflow[index] = reference
		}
	}
	state.count++
}

func (cursor *decoderCursor) beginReplaceScope(owner unsafe.Pointer) (uint32, bool) {
	if cursor.state == nil || cursor.state.operation == nil ||
		cursor.state.operation.replace == nil {
		return 0, false
	}
	state := cursor.state.operation.replace
	parent := state.currentScope
	for index := 0; index < state.scopeCount; index++ {
		scope := replaceScopeAt(state, index)
		if scope.owner == owner && scope.parent == parent {
			id := uint32(index + 1)
			clearReplaceScope(state, id)
			state.currentScope = id
			return parent, true
		}
	}
	index := -1
	for candidate := 0; candidate < state.scopeCount; candidate++ {
		if replaceScopeAt(state, candidate).owner == nil {
			index = candidate
			break
		}
	}
	if index < 0 {
		index = state.scopeCount
		state.scopeCount++
	}
	setReplaceScopeAt(state, index, decoderReplaceScope{owner: owner, parent: parent})
	state.currentScope = uint32(index + 1)
	return parent, true
}

func (cursor *decoderCursor) endReplaceScope(previous uint32, active bool) {
	if active {
		cursor.state.operation.replace.currentScope = previous
	}
}

func (cursor *decoderCursor) clearTypedReplaceReferences(node *typedNode, dst unsafe.Pointer) {
	if cursor.state == nil || cursor.state.operation == nil ||
		cursor.state.operation.replace == nil {
		return
	}
	switch node.baseKind {
	case typedPointer, typedSlice, typedBytes, typedMap:
		cursor.clearReplaceOwnerScope(dst)
	case typedStruct:
		for i := range node.fields {
			field := &node.fields[i]
			target := dst
			if field.hop >= 0 {
				target = resolveResetHops(dst, node.fieldHops[field.hop])
				if target == nil {
					continue
				}
			}
			cursor.clearTypedReplaceReferences(field.node, unsafe.Add(target, field.offset))
		}
		if node.inlineMap != nil {
			cursor.clearReplaceOwnerScope(unsafe.Add(dst, node.inlineMap.offset))
		}
	case typedArray:
		for index := 0; index < node.length; index++ {
			cursor.clearTypedReplaceReferences(
				node.elem,
				unsafe.Add(dst, uintptr(index)*node.elem.size),
			)
		}
	}
}

func (cursor *decoderCursor) clearReplaceOwnerScope(owner unsafe.Pointer) {
	state := cursor.state.operation.replace
	for index := 0; index < state.scopeCount; index++ {
		if replaceScopeAt(state, index).owner == owner {
			clearReplaceScope(state, uint32(index+1))
			return
		}
	}
}

func clearReplaceScope(state *decoderReplaceState, target uint32) {
	write := 0
	for read := 0; read < state.count; read++ {
		reference := replaceReferenceAt(state, read)
		if replaceScopeDescendsFrom(state, reference.scope, target) {
			continue
		}
		if write != read {
			setReplaceReferenceAt(state, write, reference)
		}
		write++
	}
	for index := write; index < state.count; index++ {
		setReplaceReferenceAt(state, index, decoderReplaceReference{})
	}
	state.count = write

	for id := uint32(state.scopeCount); id > 0; id-- {
		if id != target && replaceScopeDescendsFrom(state, id, target) {
			setReplaceScopeAt(state, int(id-1), decoderReplaceScope{})
		}
	}
	for state.scopeCount > 0 &&
		replaceScopeAt(state, state.scopeCount-1).owner == nil {
		state.scopeCount--
	}
}

func replaceScopeDescendsFrom(state *decoderReplaceState, scope, ancestor uint32) bool {
	for scope != 0 {
		if scope == ancestor {
			return true
		}
		scope = replaceScopeAt(state, int(scope-1)).parent
	}
	return false
}

func replaceReferenceAt(state *decoderReplaceState, index int) decoderReplaceReference {
	if index < len(state.refs) {
		return state.refs[index]
	}
	return state.overflow[index-len(state.refs)]
}

func setReplaceReferenceAt(state *decoderReplaceState, index int, reference decoderReplaceReference) {
	if index < len(state.refs) {
		state.refs[index] = reference
		return
	}
	state.overflow[index-len(state.refs)] = reference
}

func replaceScopeAt(state *decoderReplaceState, index int) decoderReplaceScope {
	if index < len(state.scopes) {
		return state.scopes[index]
	}
	return state.scopeOverflow[index-len(state.scopes)]
}

func setReplaceScopeAt(state *decoderReplaceState, index int, scope decoderReplaceScope) {
	if index < len(state.scopes) {
		state.scopes[index] = scope
		return
	}
	overflowIndex := index - len(state.scopes)
	if overflowIndex == len(state.scopeOverflow) {
		state.scopeOverflow = append(state.scopeOverflow, scope)
	} else {
		state.scopeOverflow[overflowIndex] = scope
	}
}

func replaceReferencesAlias(previous, reference decoderReplaceReference) bool {
	if previous.kind == decoderReplaceMap || reference.kind == decoderReplaceMap {
		return previous.kind == decoderReplaceMap &&
			reference.kind == decoderReplaceMap &&
			previous.ptr == reference.ptr
	}
	if previous.span == 0 || reference.span == 0 {
		// Zero-sized pointees have no writable range. Keep exact pointer
		// identity tracking for pointer pairs, but they cannot overlap slice
		// storage or differently addressed pointees.
		return previous.kind == decoderReplacePointer &&
			reference.kind == decoderReplacePointer &&
			previous.ptr == reference.ptr
	}
	return replaceMemoryRangesAlias(
		uintptr(previous.ptr), previous.span,
		uintptr(reference.ptr), reference.span,
	)
}

func replaceMemoryRangesAlias(firstStart, firstSpan, secondStart, secondSpan uintptr) bool {
	if firstSpan == 0 || secondSpan == 0 {
		return false
	}
	firstEnd := firstStart + firstSpan
	if firstEnd < firstStart {
		firstEnd = ^uintptr(0)
	}
	secondEnd := secondStart + secondSpan
	if secondEnd < secondStart {
		secondEnd = ^uintptr(0)
	}
	return firstStart < secondEnd && secondStart < firstEnd
}

func (cursor *decoderCursor) setReplaceDestination(ptr unsafe.Pointer, span uintptr) {
	if cursor.state == nil {
		cursor.state = new(decoderState)
	}
	cursor.state.replaceDestination = ptr
	if span > uintptr(^uint32(0)) {
		cursor.state.replaceSpan = ^uint32(0)
		cursor.flags |= decoderReplaceWideDestination
	} else {
		cursor.state.replaceSpan = uint32(span)
	}
}

func (cursor *decoderCursor) replaceDestinationRange() (unsafe.Pointer, uint32) {
	if cursor.state == nil {
		return nil, 0
	}
	return cursor.state.replaceDestination, cursor.state.replaceSpan
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
	key = cursor.ownedText(key)
	mapDst := unsafe.Add(structPtr, inline.offset)
	previousScope, scoped := uint32(0), false
	if cursor.flags&decoderReplace != 0 {
		previousScope, scoped = cursor.beginReplaceScope(mapDst)
	}
	mapValue := reflect.NewAt(inline.mapType, mapDst).Elem()
	if cursor.flags&decoderReplace != 0 && d.entries == 0 {
		cursor.prepareInlineMapReplace(mapValue, mapDst)
	}
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
		cursor.endReplaceScope(previousScope, scoped)
		return err
	}
	d.key.SetString(key)
	mapValue.SetMapIndex(d.key, d.element)
	if cursor.flags&decoderReplace != 0 {
		cursor.refreshInlineMapReference(mapValue, mapDst)
	}
	cursor.endReplaceScope(previousScope, scoped)
	return nil
}

func (cursor *decoderCursor) prepareInlineMapReplace(mapValue reflect.Value, owner unsafe.Pointer) {
	if mapValue.IsNil() {
		return
	}
	if cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil {
		reference := cursor.inlineMapReference(mapValue, owner)
		if cursor.replaceReferenceSeen(reference) {
			mapValue.SetZero()
			return
		}
	}
	mapValue.Clear()
}

func (cursor *decoderCursor) refreshInlineMapReference(mapValue reflect.Value, owner unsafe.Pointer) {
	if cursor.state == nil || cursor.state.operation == nil ||
		cursor.state.operation.replace == nil {
		return
	}
	cursor.storeReplaceReference(cursor.inlineMapReference(mapValue, owner), !mapValue.IsNil())
}

func (cursor *decoderCursor) inlineMapReference(mapValue reflect.Value, owner unsafe.Pointer) decoderReplaceReference {
	reference := decoderReplaceReference{kind: decoderReplaceMap, owner: owner}
	if cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil {
		reference.scope = cursor.state.operation.replace.currentScope
	}
	if !mapValue.IsNil() {
		reference.ptr = mapValue.UnsafePointer()
	}
	return reference
}

func (cursor *decoderCursor) resetMissingInlineMap(node *typedNode, dst unsafe.Pointer, seen bool) {
	if node.inlineMap == nil || seen {
		return
	}
	mapDst := unsafe.Add(dst, node.inlineMap.offset)
	if cursor.state != nil && cursor.state.operation != nil &&
		cursor.state.operation.replace != nil {
		cursor.clearReplaceOwnerScope(mapDst)
	}
	reflect.NewAt(node.inlineMap.mapType, mapDst).Elem().SetZero()
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
		if node.mapKeyKind == mapKeyString {
			key = cursor.ownedText(key)
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
	if existing := *(*any)(dst); cursor.flags&decoderReplace == 0 && existing != nil {
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
	p := cursor.slowParser()
	p.zeroCopy = cursor.flags&decoderZeroCopy != 0
	block := cursor.prepareOwnedParser(&p)
	p.skipSpace()
	value, err := p.parseAnyValue(int(cursor.depth), cursor.flags&decoderUseNumber != 0)
	cursor.i = p.i
	cursor.finishOwnedParser(&p, block)
	if err != nil {
		return err
	}
	*(*any)(dst) = value
	return nil
}

func (cursor *decoderCursor) decodeCompiledAnyInline(dst unsafe.Pointer) error {
	if existing := *(*any)(dst); cursor.flags&decoderReplace == 0 && existing != nil {
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
			inner, err := dynamicDecodeInlineNode(existingValue.Type().Elem())
			if err != nil {
				return &DecodeError{Offset: cursor.i, Type: existingValue.Type(), Reason: err.Error()}
			}
			return cursor.decodeCompiled(inner, existingValue.UnsafePointer())
		}
	}
	p := cursor.slowParser()
	p.zeroCopy = cursor.flags&decoderZeroCopy != 0
	block := cursor.prepareOwnedParser(&p)
	p.skipSpace()
	value, err := p.parseAnyValue(int(cursor.depth), cursor.flags&decoderUseNumber != 0)
	cursor.i = p.i
	cursor.finishOwnedParser(&p, block)
	if err != nil {
		return err
	}
	*(*any)(dst) = value
	return nil
}

// dynamicDecodeNodes caches one compiled decode plan per concrete type found
// inside an interface value.
var dynamicDecodeNodes sync.Map
var dynamicDecodeInlineNodes sync.Map

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

func dynamicDecodeInlineNode(typ reflect.Type) (*typedNode, error) {
	if entry, ok := dynamicDecodeInlineNodes.Load(typ); ok {
		cached := entry.(*dynamicDecodeEntry)
		return cached.node, cached.err
	}
	compiler := newTypedCompiler(typedCompileDecode)
	compiler.inlineFields = true
	node, err := compiler.compile(typ, typ.String())
	if err == nil {
		prepareTypedResets(node, make(map[*typedNode]bool))
		prepareDecoderReceivers(node)
	}
	entry, _ := dynamicDecodeInlineNodes.LoadOrStore(typ, &dynamicDecodeEntry{node: node, err: err})
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
	if cursor.flags&decoderReplace != 0 {
		value.SetZero()
		return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "cannot decode into a non-empty interface"}
	}
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

func (cursor *decoderCursor) decodeCompiledIfaceInline(node *typedNode, dst unsafe.Pointer) error {
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
	if cursor.flags&decoderReplace != 0 {
		value.SetZero()
		return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "cannot decode into a non-empty interface"}
	}
	if !value.IsNil() {
		concrete := value.Elem()
		if concrete.Kind() == reflect.Pointer && !concrete.IsNil() {
			inner, innerErr := dynamicDecodeInlineNode(concrete.Type().Elem())
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
			previousScope, scoped := uint32(0), false
			if node.baseKind == typedPointer && cursor.flags&decoderReplace != 0 {
				previousScope, scoped = cursor.beginReplaceScope(dst)
			}
			resetTyped(node, dst)
			if scoped {
				cursor.refreshReplaceReference(node, dst)
				cursor.endReplaceScope(previousScope, true)
			}
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
	flags := cursor.flags &^ decoderZeroCopy
	sub := decoderCursor{
		src:      inner,
		state:    cursor.state,
		maxDepth: cursor.maxDepth,
		flags:    flags,
	}
	scalar := node
	if scalar.baseKind == typedPointer {
		scalar = scalar.elem
	}
	switch scalar.kind {
	case typedInt, typedUint, typedFloat:
		return cursor.decodeQuotedNumber(node, scalar, dst, inner, i, cursor.flags&decoderReplace != 0)
	}
	if scalar.kind == typedString {
		// The contents must themselves be a JSON string.
		if len(inner) == 0 || inner[0] != '"' {
			return &DecodeError{Offset: i, Type: node.typ, Reason: "string-tagged field does not contain a JSON string"}
		}
	}
	if err := sub.decodeCompiled(node, dst); err != nil {
		switch typed := err.(type) {
		case *DecodeError:
			typed.Offset = i
		case *SyntaxError:
			raw := cursor.src[i+1 : cursor.i-1]
			offset := i + 1 + decodedJSONStringRawOffset(raw, typed.Offset)
			return syntaxError(cursor.src, offset, typed.Message)
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
func (cursor *decoderCursor) decodeQuotedNumber(node, scalar *typedNode, dst unsafe.Pointer, inner []byte, offset int, replace bool) error {
	text := byteview.String(inner)
	if text == "null" {
		// encoding/json treats a quoted null like the bare literal: value
		// fields are left untouched and pointer fields are cleared.
		if node.baseKind == typedPointer {
			previousScope, scoped := uint32(0), false
			if replace {
				previousScope, scoped = cursor.beginReplaceScope(dst)
			}
			*(*unsafe.Pointer)(dst) = nil
			if replace {
				cursor.refreshReplaceReference(node, dst)
			}
			cursor.endReplaceScope(previousScope, scoped)
		} else if replace {
			resetTyped(node, dst)
		}
		return nil
	}
	if !acceptStringTaggedNumber(text) {
		return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged number " + strconv.Quote(text)}
	}
	scalarDst := dst
	previousScope, scoped := uint32(0), false
	if node.baseKind == typedPointer {
		if replace {
			previousScope, scoped = cursor.beginReplaceScope(dst)
		}
		pointer := *(*unsafe.Pointer)(dst)
		if replace {
			pointer = cursor.reuseReplacePointer(node, dst)
		}
		if pointer == nil {
			pointer = allocateTypedPointer(node, dst)
		}
		scalarDst = pointer
	}
	switch scalar.kind {
	case typedInt:
		value, err := strconv.ParseInt(text, 10, int(scalar.bits))
		if err != nil {
			cursor.endReplaceScope(previousScope, scoped)
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
			cursor.endReplaceScope(previousScope, scoped)
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
			cursor.endReplaceScope(previousScope, scoped)
			return &DecodeError{Offset: offset, Type: node.typ, Reason: "cannot parse string-tagged number " + strconv.Quote(text)}
		}
		if scalar.bits == 32 {
			*(*float32)(scalarDst) = float32(value)
		} else {
			*(*float64)(scalarDst) = value
		}
	}
	if replace && node.baseKind == typedPointer {
		cursor.refreshReplaceReference(node, dst)
	}
	cursor.endReplaceScope(previousScope, scoped)
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
		if node.elem != nil {
			return cursor.decodeCompiledSlice(node, dst)
		}
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
	buf := value.Bytes()
	count := 0
	for first := true; ; first = false {
		more, err := cursor.NextArrayElement(first)
		if err != nil {
			return err
		}
		if !more {
			break
		}
		if count == len(buf) {
			buf = append(buf, 0)
		}
		element := &buf[count]
		var decodeErr error
		if useStableNumericMethods {
			decodeErr = cursor.Uint8(element)
		} else {
			decodeErr = cursor.Uint(element)
		}
		if decodeErr != nil {
			return retagCompiledError(decodeErr, node.typ)
		}
		count++
	}
	if count == 0 {
		setTypedEmptySlice(node, dst)
		return nil
	}
	value.SetBytes(buf[:count])
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
	if slice.len == 0 && slice.cap == 0 && !slice.isNil() {
		// The destination already holds the non-nil len=cap=0 sentinel a
		// fresh reflect.MakeSlice would install, so reused destinations
		// decode empty arrays without manufacturing anything. Any other
		// shape takes the fresh slice, preserving the isolation contract
		// (TestTypedDecoderEmptySliceSentinelIsNonNilAndIsolated) and
		// encoding/json's element-releasing replacement semantics.
		return
	}
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
