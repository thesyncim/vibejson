package vibejson

//go:generate go run ./internal/cmd/codegen typed-ops

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unsafe"
)

// DecoderOptions controls decoding into caller-owned Go values.
type DecoderOptions struct {
	// MaxDepth limits nesting; non-positive values use the default.
	MaxDepth int

	// ZeroCopy allows text and numbers to alias src.
	ZeroCopy bool

	// DisallowUnknownFields rejects unknown object keys.
	DisallowUnknownFields bool

	// CaseSensitive disables folded field matching.
	CaseSensitive bool

	// UseNumber decodes dynamic numbers as json.Number.
	UseNumber bool

	// Replace clears absent state and nulls while reusing unique storage.
	Replace bool

	// InlineFields enables the json:",inline" catch-all map.
	InlineFields bool
}

// Decoder is an immutable compiled decoder for one concrete Go type.
type Decoder[T any] struct {
	root       *typedNode
	rootSlice  *typedNode
	options    DecoderOptions
	structural bool
	scratch    *decoderPlanState
}

// CompileDecoder builds an immutable decoder for T and copies opts.
func CompileDecoder[T any](opts DecoderOptions) (Decoder[T], error) {
	opts.MaxDepth = maxDepthOrDefault(opts.MaxDepth)
	if opts.MaxDepth > int(^uint32(0)>>1) {
		opts.MaxDepth = int(^uint32(0) >> 1)
	}
	typ := reflect.TypeFor[T]()
	compiler := newTypedCompiler(typedCompileDecode)
	compiler.inlineFields = opts.InlineFields
	compiler.replaceReferences = opts.Replace
	root, err := compiler.compile(typ, typ.String())
	if err != nil {
		return Decoder[T]{}, err
	}
	prepareTypedResets(root, make(map[*typedNode]bool))
	prepareDecoderReceivers(root)
	mapSlots := prepareDecoderMapScratch(root)
	replaceReferences := typedReplaceReferenceCount(root, make(map[*typedNode]bool))
	wideReplace := opts.Replace && prepareTypedWideSeen(root)
	root.decReplaceDestination = opts.Replace &&
		typedReplaceReferenceMayAliasDestination(root, root, make(map[*typedNode]bool))
	root.decReplaceAliases = opts.Replace && replaceReferences >= 2
	arrayReplaceAliases := opts.Replace && replaceReferences != 0
	root.decNeedsScratch = mapSlots != 0 || root.decHasReceiver || root.decReplaceAliases || wideReplace
	scratch := newDecoderPlanState(mapSlots, root.decNeedsScratch || arrayReplaceAliases)
	structural := typedStructuralCandidate(root, make(map[*typedNode]bool))
	if wideReplace {
		structural = false
	}
	rootSliceType := reflect.TypeFor[[]T]()
	return Decoder[T]{
		root:       root,
		structural: structural,
		scratch:    scratch,
		rootSlice: &typedNode{
			kind:       typedSlice,
			baseKind:   typedSlice,
			op:         typedOpSlice,
			typedShape: typedShape{typ: rootSliceType, name: rootSliceType.String()},
			elem:       root,
			typedDecodeProgram: typedDecodeProgram{
				decHasReceiver:    root.decHasReceiver,
				decReplaceAliases: arrayReplaceAliases,
			},
		},
		options: opts,
	}, nil
}

// typedReplaceReferenceCount returns a count capped at two.
func typedReplaceReferenceCount(node *typedNode, visiting map[*typedNode]bool) int {
	if node == nil {
		return 0
	}
	if visiting[node] {
		return 1
	}
	switch node.kind {
	case typedMapReplace, typedBytesReplace:
		return 1
	case typedSliceReplace:
		visiting[node] = true
		nested := typedReplaceReferenceCount(node.elem, visiting)
		delete(visiting, node)
		if nested != 0 {
			return 2
		}
		return 1
	case typedPointerReplace:
		visiting[node] = true
		nested := typedReplaceReferenceCount(node.elem, visiting)
		delete(visiting, node)
		if nested != 0 {
			return 2
		}
		return 1
	case typedArray:
		return 0
	case typedStruct:
		visiting[node] = true
		count := 0
		if node.inlineMap != nil {
			count = 1
		}
		for i := range node.fields {
			count += typedReplaceReferenceCount(node.fields[i].node, visiting)
			if count >= 2 {
				delete(visiting, node)
				return 2
			}
		}
		delete(visiting, node)
		return count
	default:
		return 0
	}
}

func prepareTypedWideSeen(root *typedNode) bool {
	seen := make(map[*typedNode]bool)
	anyWide := false
	var visit func(*typedNode)
	visit = func(node *typedNode) {
		if node == nil || seen[node] {
			return
		}
		seen[node] = true
		visit(node.elem)
		for i := range node.fields {
			visit(node.fields[i].node)
		}
		if node.inlineMap != nil {
			visit(node.inlineMap.elem)
		}
		if node.kind != typedStruct {
			return
		}
		if node.hasDecResetIgnored() {
			node.setDecWideSeen()
			node.allSet = 0
			anyWide = true
			return
		}
		specialEpilogue := node.inlineMap != nil
		if len(node.fields) <= 64 &&
			!(specialEpilogue && len(node.fields) > 62) {
			return
		}
		wideSeen := specialEpilogue
		referenceWalk := make(map[*typedNode]bool)
		for i := range node.fields {
			field := &node.fields[i]
			if field.hop >= 0 || typedReplaceReferenceCount(field.node, referenceWalk) != 0 {
				wideSeen = true
				break
			}
		}
		if wideSeen {
			node.setDecWideSeen()
			node.allSet = 0
			anyWide = true
		}
	}
	visit(root)
	return anyWide
}

func typedReplaceReferenceMayAliasDestination(root, node *typedNode, visiting map[*typedNode]bool) bool {
	if node == nil || visiting[node] {
		return false
	}
	switch node.kind {
	case typedPointerReplace:
		pointee := node.typ.Elem()
		if pointee.Size() != 0 &&
			(typedStaticContains(root.typ, pointee, make(map[reflect.Type]bool)) ||
				typedStaticContains(pointee, root.typ, make(map[reflect.Type]bool))) {
			return true
		}
	case typedSliceReplace, typedBytesReplace:
		elem := node.typ.Elem()
		if elem.Size() != 0 &&
			(root.typ == elem ||
				typedStaticArrayContainsElement(root.typ, elem, make(map[reflect.Type]bool))) {
			return true
		}
	}
	visiting[node] = true
	defer delete(visiting, node)
	switch node.kind {
	case typedStruct:
		for i := range node.fields {
			if typedReplaceReferenceMayAliasDestination(root, node.fields[i].node, visiting) {
				return true
			}
		}
		if node.inlineMap != nil {
			return typedReplaceReferenceMayAliasDestination(root, node.inlineMap.elem, visiting)
		}
	case typedPointerReplace, typedSliceReplace, typedArray, typedMapReplace, typedBytesReplace:
		return typedReplaceReferenceMayAliasDestination(root, node.elem, visiting)
	}
	return false
}

func typedStaticArrayContainsElement(container, target reflect.Type, visiting map[reflect.Type]bool) bool {
	if visiting[container] {
		return false
	}
	visiting[container] = true
	defer delete(visiting, container)
	switch container.Kind() {
	case reflect.Struct:
		for i := 0; i < container.NumField(); i++ {
			if typedStaticArrayContainsElement(container.Field(i).Type, target, visiting) {
				return true
			}
		}
	case reflect.Array:
		if container.Len() == 0 {
			return false
		}
		if container.Elem() == target {
			return true
		}
		return typedStaticArrayContainsElement(container.Elem(), target, visiting)
	}
	return false
}

func typedStaticContains(container, target reflect.Type, visiting map[reflect.Type]bool) bool {
	if container == target {
		return true
	}
	if visiting[container] {
		return false
	}
	visiting[container] = true
	defer delete(visiting, container)
	switch container.Kind() {
	case reflect.Struct:
		for i := 0; i < container.NumField(); i++ {
			if typedStaticContains(container.Field(i).Type, target, visiting) {
				return true
			}
		}
	case reflect.Array:
		return typedStaticContains(container.Elem(), target, visiting)
	}
	return false
}

func typedStructuralCandidate(node *typedNode, visiting map[*typedNode]bool) bool {
	if node == nil || visiting[node] {
		return false
	}
	switch node.kind {
	case typedStruct:
		if !node.structuralFast {
			return false
		}
		visiting[node] = true
		for i := range node.fields {
			field := &node.fields[i]
			switch field.op {
			case typedOpStruct, typedOpSlice, typedOpArray:
				if !typedStructuralCandidate(field.node, visiting) {
					delete(visiting, node)
					return false
				}
			}
		}
		delete(visiting, node)
		return true
	case typedSlice, typedArray:
		visiting[node] = true
		eligible := typedStructuralCandidate(node.elem, visiting)
		delete(visiting, node)
		return eligible
	case typedBool, typedString, typedInt, typedUint, typedFloat:
		return true
	default:
		return false
	}
}

// Decode decodes one JSON value into dst and rejects trailing data. It may
// partially modify dst when decoding fails.
func (plan Decoder[T]) Decode(src []byte, dst *T) error {
	if plan.root == nil {
		return fmt.Errorf("vibejson: zero Decoder")
	}
	if dst == nil {
		return fmt.Errorf("vibejson: typed Decode destination is nil")
	}
	if plan.root.kind == typedAny {
		out := (*any)(unsafe.Pointer(dst))
		if existing := *out; plan.options.Replace || existing == nil || !anyDecodeMerges(existing) {
			value, err := unmarshalAny(src, plan.options)
			if err != nil {
				return err
			}
			*out = value
			return nil
		}
	}
	if plan.structural && decoderStructuralWorthwhile(src) &&
		(decoderPreferStructuralRecords || plan.root.kind != typedStruct) {
		return plan.decodeStructural(src, dst)
	}
	if plan.scratch != nil && plan.root.decNeedsScratch {
		return decodeTypedDocumentScratch(src, plan.options, plan.root, unsafe.Pointer(dst), plan.scratch)
	}
	if plan.root.kind == typedUnmarshalerSimd && !plan.options.Replace {
		return decodeRootHook(src, plan.options, any(dst).(UnmarshalerSimd))
	}
	return decodeTypedDocument(src, plan.options, plan.root, unsafe.Pointer(dst), nil)
}

func decodeTypedDocument(src []byte, options DecoderOptions, root *typedNode, dst unsafe.Pointer, state *decoderState) error {
	if state == nil && root.decReplaceDestination {
		return decodeTypedDocumentReplace(src, options, root, dst)
	}
	cursor := newDecoderCursor(src, options)
	structural := state != nil && state.structuralActive && !state.structural.bad
	if state != nil {
		cursor.state = state
		if !structural {
			state.structuralActive = false
		}
	}
	if root.decReplaceDestination {
		cursor.setReplaceDestination(dst, root.size)
	}
	cursor.skipSpace()
	var err error
	switch root.kind {
	case typedStruct:
		if structural {
			err = cursor.decodeCompiledStructStructural(root, dst)
		} else {
			err = cursor.decodeCompiledStruct(root, dst)
		}
	case typedSlice:
		if structural {
			err = cursor.decodeCompiledSliceStructural(root, dst)
		} else {
			err = cursor.decodeCompiledSlice(root, dst)
		}
	case typedArray:
		if structural {
			err = cursor.decodeCompiledArrayStructural(root, dst)
		} else {
			err = cursor.decodeCompiledArray(root, dst)
		}
	case typedPointer:
		err = cursor.decodeCompiledPointer(root, dst)
	case typedMap:
		err = cursor.decodeCompiledMap(root, dst)
	default:
		err = cursor.decodeCompiled(root, dst)
	}
	if err != nil {
		return err
	}
	return cursor.Finish()
}

//go:noinline
func decodeTypedDocumentReplace(src []byte, options DecoderOptions, root *typedNode, dst unsafe.Pointer) error {
	var state decoderState
	return decodeTypedDocument(src, options, root, dst, &state)
}

func decodeTypedDocumentScratch(src []byte, options DecoderOptions, root *typedNode, dst unsafe.Pointer, plan *decoderPlanState) error {
	state := plan.take()
	prepareTypedReplaceState(state, root.decReplaceAliases)
	defer releaseTypedPlanState(plan, state)
	return decodeTypedDocument(src, options, root, dst, state)
}

func prepareTypedReplaceState(state *decoderState, aliases bool) {
	if !aliases {
		return
	}
	if state.operation == nil {
		state.operation = new(decoderOperationState)
	}
	if state.operation.replace == nil {
		state.operation.replace = new(decoderReplaceState)
	}
}

func releaseTypedPlanState(plan *decoderPlanState, state *decoderState) {
	if operation := state.operation; operation != nil && operation.replace != nil {
		replace := operation.replace
		clear(replace.refs[:])
		overflowCount := replace.count - len(replace.refs)
		if overflowCount > 0 {
			clear(replace.overflow[:overflowCount])
		}
		clear(replace.scopes[:])
		scopeOverflowCount := replace.scopeCount - len(replace.scopes)
		if scopeOverflowCount > 0 {
			clear(replace.scopeOverflow[:scopeOverflowCount])
		}
		replace.count = 0
		replace.scopeCount = 0
		replace.currentScope = 0
	}
	plan.release(state)
}

//go:noinline
func (plan Decoder[T]) decodeStructural(src []byte, dst *T) error {
	state := acquireDecoderState(src)
	defer releaseDecoderState(state)
	return decodeTypedDocument(src, plan.options, plan.root, unsafe.Pointer(dst), state)
}

// DecodePrefix decodes one JSON value from the front of src and returns bytes
// consumed, including leading but excluding trailing whitespace.
func (plan Decoder[T]) DecodePrefix(src []byte, dst *T) (int, error) {
	if plan.root == nil {
		return 0, fmt.Errorf("vibejson: zero Decoder")
	}
	if dst == nil {
		return 0, fmt.Errorf("vibejson: typed Decode destination is nil")
	}
	if plan.scratch != nil && plan.root.decNeedsScratch {
		state := plan.scratch.take()
		prepareTypedReplaceState(state, plan.root.decReplaceAliases)
		defer releaseTypedPlanState(plan.scratch, state)
		return plan.decodePrefixState(src, dst, state)
	}
	if plan.root.decReplaceDestination {
		return plan.decodePrefixReplace(src, dst)
	}
	return plan.decodePrefixState(src, dst, nil)
}

//go:noinline
func (plan Decoder[T]) decodePrefixReplace(src []byte, dst *T) (int, error) {
	var state decoderState
	return plan.decodePrefixState(src, dst, &state)
}

func (plan Decoder[T]) decodePrefixState(src []byte, dst *T, state *decoderState) (int, error) {
	cursor := newDecoderCursor(src, plan.options)
	cursor.state = state
	if plan.root.decReplaceDestination {
		cursor.setReplaceDestination(unsafe.Pointer(dst), plan.root.size)
	}
	cursor.skipSpace()
	var err error
	switch plan.root.kind {
	case typedStruct:
		err = cursor.decodeCompiledStruct(plan.root, unsafe.Pointer(dst))
	case typedSlice:
		err = cursor.decodeCompiledSlice(plan.root, unsafe.Pointer(dst))
	case typedArray:
		err = cursor.decodeCompiledArray(plan.root, unsafe.Pointer(dst))
	case typedPointer:
		err = cursor.decodeCompiledPointer(plan.root, unsafe.Pointer(dst))
	case typedMap:
		err = cursor.decodeCompiledMap(plan.root, unsafe.Pointer(dst))
	default:
		err = cursor.decodeCompiled(plan.root, unsafe.Pointer(dst))
	}
	if err != nil {
		return cursor.i, err
	}
	return cursor.i, nil
}

// DecodeArray decodes a top-level JSON array into dst, reusing its capacity.
func (plan Decoder[T]) DecodeArray(src []byte, dst []T) ([]T, error) {
	if plan.rootSlice == nil {
		return dst[:0], fmt.Errorf("vibejson: zero Decoder")
	}
	scratch := plan.scratch
	if scratch != nil {
		cursor := newDecoderCursor(src, plan.options)
		cursor.state = scratch.take()
		prepareTypedReplaceState(cursor.state, plan.rootSlice.decReplaceAliases)
		defer releaseTypedPlanState(scratch, cursor.state)
		return plan.decodeArrayCursor(src, dst, &cursor)
	}
	if plan.root.decReplaceDestination && cap(dst) != 0 {
		return plan.decodeArrayReplace(src, dst)
	}
	cursor := newDecoderCursor(src, plan.options)
	return plan.decodeArrayCursor(src, dst, &cursor)
}

//go:noinline
func (plan Decoder[T]) decodeArrayReplace(src []byte, dst []T) ([]T, error) {
	var state decoderState
	cursor := newDecoderCursor(src, plan.options)
	cursor.state = &state
	return plan.decodeArrayCursor(src, dst, &cursor)
}

func (plan Decoder[T]) decodeArrayCursor(src []byte, dst []T, cursor *decoderCursor) ([]T, error) {
	if plan.root.decReplaceDestination && cap(dst) != 0 {
		backing := dst[:cap(dst)]
		cursor.setReplaceDestination(
			unsafe.Pointer(unsafe.SliceData(backing)),
			uintptr(cap(backing))*plan.root.size,
		)
	}
	cursor.skipSpace()
	dst, err := decodeCompiledRootSlice(cursor, plan.rootSlice, dst)
	if err != nil {
		return dst, err
	}
	if err := cursor.Finish(); err != nil {
		return dst, err
	}
	return dst, nil
}

// UnsupportedTypeError reports a static Go type rejected while compiling an
// [Encoder] or [Decoder] plan.
type UnsupportedTypeError struct {
	// Type is the unsupported Go type.
	Type reflect.Type
	// Path identifies its position in the plan.
	Path string
	// Reason describes the unsupported property.
	Reason string
}

// Error formats the unsupported type, plan path, and reason.
func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf("vibejson: typed decoder does not support %s at %s: %s", e.Type, e.Path, e.Reason)
}

// DecodeError reports valid JSON that cannot be stored in the requested Go
// type. The decoder does not attach the input slice to the error.
type DecodeError struct {
	// Offset is the byte offset of the invalid value.
	Offset int
	// Path identifies the destination field or index.
	Path string
	// Type is the destination type when available.
	Type reflect.Type
	// TypeName identifies the destination when Type is unavailable.
	TypeName string
	// Reason describes why the value cannot be assigned.
	Reason string
}

// Error formats the decode failure with its byte offset, destination, optional
// value path, and reason.
func (e *DecodeError) Error() string {
	typeName := e.TypeName
	if e.Type != nil {
		typeName = e.Type.String()
	}
	if e.Path != "" {
		return fmt.Sprintf("vibejson: cannot decode JSON at byte %d into %s at %s: %s", e.Offset, typeName, e.Path, e.Reason)
	}
	return fmt.Sprintf("vibejson: cannot decode JSON at byte %d into %s: %s", e.Offset, typeName, e.Reason)
}

func prependDecodePathField(err error, name string) error {
	if e, ok := err.(*DecodeError); ok {
		switch {
		case e.Path == "":
			e.Path = name
		case e.Path[0] == '[':
			e.Path = name + e.Path
		default:
			e.Path = name + "." + e.Path
		}
	}
	return err
}

func prependDecodePathIndex(err error, index int) error {
	if e, ok := err.(*DecodeError); ok {
		segment := "[" + strconv.Itoa(index) + "]"
		if e.Path == "" || e.Path[0] == '[' {
			e.Path = segment + e.Path
		} else {
			e.Path = segment + "." + e.Path
		}
	}
	return err
}

func (node *typedNode) findFieldSlow(key string, fold bool) *typedField {
	if node.fieldTable != nil {
		slot := fieldNameHash(key) & node.fieldTableMask
		for {
			entry := node.fieldTable[slot]
			if entry == 0 {
				break
			}
			field := &node.fields[entry-1]
			if field.name == key {
				return field
			}
			slot = (slot + 1) & node.fieldTableMask
		}
	} else {
		for i := range node.fields {
			if node.fields[i].name == key {
				return &node.fields[i]
			}
		}
	}
	if fold {
		return node.findFieldFold(key)
	}
	return nil
}

//go:noinline
func (node *typedNode) findFieldFold(key string) *typedField {
	for i := range node.fields {
		if strings.EqualFold(node.fields[i].name, key) {
			return &node.fields[i]
		}
	}
	return nil
}

func fieldNameHash(name string) uint32 {
	h := uint64(len(name)) * 0x9e3779b97f4a7c15
	for len(name) >= 8 {
		h ^= binary.LittleEndian.Uint64([]byte(name))
		h *= 0xbf58476d1ce4e5b9
		name = name[8:]
	}
	var tail uint64
	for i := range len(name) {
		tail |= uint64(name[i]) << (8 * i)
	}
	h ^= tail
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	return uint32(h ^ h>>32)
}

func nextTypedSliceCapacity(current, required int) int {
	capacity := current * 2
	if capacity < 4 {
		capacity = 4
	}
	if capacity < required {
		capacity = required
	}
	return capacity
}
