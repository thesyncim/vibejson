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

// DecoderOptions controls decoding directly into caller-owned Go values.
// [CompileDecoder] copies the value; later changes to the caller's options do
// not affect the compiled decoder.
type DecoderOptions struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int

	// ZeroCopy allows unescaped strings, retained object keys, and textual
	// number values such as json.Number to alias src. Callers must not mutate
	// src while any such result is in use. Escaped strings still require
	// independent storage. When false, results do not alias src; a result may
	// instead retain one private copy of the input rather than allocate each
	// string separately.
	ZeroCopy bool

	// DisallowUnknownFields rejects object keys absent from the compiled type.
	DisallowUnknownFields bool

	// CaseSensitive disables the encoding/json-compatible case-insensitive
	// fallback used after exact field-name matching.
	CaseSensitive bool

	// UseNumber decodes JSON numbers bound for dynamic destinations as
	// json.Number instead of float64, like encoding/json's Decoder.UseNumber.
	// It applies wherever a value's shape is chosen at decode time — a
	// top-level *any as well as any-typed fields nested in structs, maps, and
	// slices. Fields with a declared Go type are unaffected: their type
	// already decides the representation.
	UseNumber bool

	// Replace decodes as if into a fresh zero destination, so a reused
	// destination yields the same result as a new one: state the document
	// does not mention is reset to its zero value. Absent struct fields become
	// zero (nil slices, nil maps, nil pointers), null clears, and a present
	// map is replaced rather than merged into. The default instead matches
	// encoding/json, which merges into existing values and treats null as a
	// no-op for scalars, strings, structs, and arrays. Replace is the right
	// mode for destinations reused across decodes. Existing slice and map
	// storage is reused when unique; overlapping slices and shared maps are
	// detached so later fields cannot overwrite earlier decoded results.
	Replace bool

	// InlineFields activates the ",inline" struct-tag extension: a
	// map[string]T field tagged `json:",inline"` becomes the catch-all for
	// object members that match no declared field, decoded into the map. The
	// option is opt-in so the tag is inert by default; a struct that does not
	// use it compiles to the identical plan and pays nothing. See [Encoder]
	// for the matching re-emission at encode time.
	InlineFields bool
}

// Decoder is an immutable compiled decoder for one concrete Go type. Use
// [Unmarshal] for occasional default-option calls; use a Decoder when decoding
// the type repeatedly, when options are required, or when a caller-owned
// destination should be reused. The same Decoder may be used concurrently when
// each call has a separately synchronized destination; concurrent mutation of
// the same destination remains the caller's responsibility. Mutable parser and
// scratch state is isolated per call.
type Decoder[T any] struct {
	root       *typedNode
	rootSlice  *typedNode
	options    DecoderOptions
	structural bool
	scratch    *decoderPlanState
}

// CompileDecoder builds an immutable decoder for T and copies opts. It allocates
// reusable type-plan and scratch metadata once; scalar and field dispatch then
// use that plan. Runtime reflection is confined to dynamic storage and type
// boundaries such as arbitrary slices, maps, interfaces, and pointers. A static
// type that cannot be decoded is reported as an [UnsupportedTypeError].
func CompileDecoder[T any](opts DecoderOptions) (Decoder[T], error) {
	if opts.MaxDepth <= 0 {
		opts.MaxDepth = DefaultMaxDepth
	} else if opts.MaxDepth > int(^uint32(0)>>1) {
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
		// Wide Replace records use a retained scalable seen set in the raw
		// executor. Keeping the whole graph on one route also avoids structural
		// tape synchronization at a nested wide boundary.
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

// typedReplaceReferenceCount returns a count capped at two: only that threshold
// matters for a single Decode, because one reusable reference has nothing else
// of the same storage class to alias. Pointer-to-destination aliases are tracked
// separately. DecodeArray treats any nonzero count as repeated and enables the
// same tracker across elements.
func typedReplaceReferenceCount(node *typedNode, visiting map[*typedNode]bool) int {
	if node == nil {
		return 0
	}
	if visiting[node] {
		// A recursive route not cut by a fresh pointer can expose the same
		// reference shape in more than one reused slice element.
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
		// Reset arrays cannot retain aliases from dst.
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

// typedStaticArrayContainsElement reports whether container owns a non-empty
// fixed array whose storage can be sliced as []target without crossing an
// indirection. Slice backing may alias such an array even when it is the only
// reusable reference in the decode graph.
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

// typedStaticContains reports whether a value of container physically contains
// an addressable value of target without crossing an indirection. It mirrors
// the layouts that safe Go pointers can target: structs and arrays, including
// the container itself. Pointer, slice, map, string, and interface payloads
// live elsewhere and are covered by the Replace reference tracker instead.
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

// Decode decodes exactly one JSON value into dst and rejects non-space trailing
// data. By default it merges like encoding/json;
// [DecoderOptions.Replace] resets state absent from the document. Slice
// capacities already reachable through dst are retained where possible;
// Replace detaches stale aliases when two destination slots share storage.
//
// Decode does not modify src. Without [DecoderOptions.ZeroCopy], results do not
// alias src and may retain one private copy of its contents. With ZeroCopy,
// aliased results remain valid only while src is unchanged. Custom unmarshal
// methods receive input bytes under their standard copy-if-retained contract.
//
// A syntax failure is reported as a [SyntaxError], and valid JSON incompatible
// with the destination is reported as a [DecodeError]. On any error dst may be
// partially modified; Decode does not roll changes back.
//
// Decode keeps ordinary compiled destinations stack eligible. Native hooks
// receive and return cursor state by value and use ordinary addressable
// receivers. Standard UnmarshalJSON and UnmarshalText methods run on detached
// receivers that are copied back before Decode returns, including on error.
func (plan Decoder[T]) Decode(src []byte, dst *T) error {
	if plan.root == nil {
		return fmt.Errorf("vibejson: zero Decoder")
	}
	if dst == nil {
		return fmt.Errorf("vibejson: typed Decode destination is nil")
	}
	if plan.root.kind == typedAny {
		// A top-level empty interface is a whole-document dynamic decode, so
		// it takes the dedicated one-pass builder — unless the value already
		// held requires encoding/json's decode-into-pointer merge, which only
		// the cursor path implements. Every empty interface shares the eface
		// layout, so the store through *any is exact for defined types too.
		// The nil test stays at the call site: anyDecodeMerges is beyond the
		// inlining budget, and a fresh destination should not pay a call.
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
	if plan.structural && decoderStructuralWorthwhile(src) {
		return plan.decodeStructural(src, dst)
	}
	if plan.scratch != nil && plan.root.decNeedsScratch {
		return decodeTypedDocumentScratch(src, plan.options, plan.root, unsafe.Pointer(dst), plan.scratch)
	}
	return decodeTypedDocument(src, plan.options, plan.root, unsafe.Pointer(dst), nil)
}

// decodeTypedDocument is the single whole-document cursor contract. A nil or
// operation-only state selects the raw cursor; an eligible structural state
// selects the forward executor unless stage 1 declined the input. Both engines
// share root dispatch, error propagation, and exact-document finalization here.
func decodeTypedDocument(src []byte, options DecoderOptions, root *typedNode, dst unsafe.Pointer, state *decoderState) error {
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

// decodeTypedDocumentScratch checks out isolated operation state for plans
// with reusable map boxes or detached standard-method receivers.
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

// DecodePrefix decodes one JSON value from the front of src into dst and
// returns the number of bytes consumed. The count includes leading whitespace,
// ends immediately after the value, and excludes trailing whitespace. Following
// data is left unexamined and need not form another JSON value. It is the
// building block for reading concatenated values; the streaming Reader uses it
// to decode without a separate boundary scan.
//
// Destination merge, ownership, ZeroCopy, and partial-mutation semantics match
// [Decoder.Decode]. On error, n is only the parser position and must not be used
// as a successfully decoded boundary. Every destination decodes mid-stream
// here, including a top-level *any: the whole-document builder used by Decode
// assumes the value spans all of src, which a prefix cannot.
func (plan Decoder[T]) DecodePrefix(src []byte, dst *T) (int, error) {
	if plan.root == nil {
		return 0, fmt.Errorf("vibejson: zero Decoder")
	}
	if dst == nil {
		return 0, fmt.Errorf("vibejson: typed Decode destination is nil")
	}
	cursor := newDecoderCursor(src, plan.options)
	if plan.scratch != nil && plan.root.decNeedsScratch {
		cursor.state = plan.scratch.take()
		prepareTypedReplaceState(cursor.state, plan.root.decReplaceAliases)
		defer releaseTypedPlanState(plan.scratch, cursor.state)
	}
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

// DecodeArray decodes a top-level JSON array into dst. Once an array starts, dst
// is logically reset to length zero and its capacity is reused where possible.
// JSON null returns a nil slice; a non-null empty array returns a non-nil empty
// slice, matching encoding/json.
//
// The returned slice is authoritative and must be used even on error: it may
// have grown and may include the partially decoded current element. Existing
// backing storage can therefore be modified even when decoding fails. Element
// ownership and ZeroCopy semantics match [Decoder.Decode].
func (plan Decoder[T]) DecodeArray(src []byte, dst []T) ([]T, error) {
	if plan.rootSlice == nil {
		return dst[:0], fmt.Errorf("vibejson: zero Decoder")
	}
	cursor := newDecoderCursor(src, plan.options)
	scratch := plan.scratch
	if scratch != nil {
		cursor.state = scratch.take()
		prepareTypedReplaceState(cursor.state, plan.rootSlice.decReplaceAliases)
		defer releaseTypedPlanState(scratch, cursor.state)
	}
	if plan.root.decReplaceDestination && cap(dst) != 0 {
		backing := dst[:cap(dst)]
		cursor.setReplaceDestination(
			unsafe.Pointer(unsafe.SliceData(backing)),
			uintptr(cap(backing))*plan.root.size,
		)
	}
	cursor.skipSpace()
	dst, err := decodeCompiledRootSlice(&cursor, plan.rootSlice, dst)
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
	// Path identifies the type position within the compiled plan.
	Path string
	// Reason describes the unsupported type property.
	Reason string
}

// Error formats the unsupported type, plan path, and reason.
func (e *UnsupportedTypeError) Error() string {
	return fmt.Sprintf("vibejson: typed decoder does not support %s at %s: %s", e.Type, e.Path, e.Reason)
}

// DecodeError reports valid JSON that cannot be stored in the requested Go
// type. The decoder does not attach the input slice to the error.
type DecodeError struct {
	// Offset is the zero-based byte offset of the offending value in the input.
	Offset int

	// Path locates the offending value using JSON member names and array
	// indexes, for example "items[3].scores[1]". It is empty when the
	// top-level value itself failed. Building the path costs nothing until
	// an error actually unwinds.
	Path string

	// Type is the destination type when it is available directly.
	Type reflect.Type
	// TypeName identifies the destination when no reflect.Type is available.
	TypeName string
	// Reason describes why the JSON value cannot be assigned.
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

// prependDecodePathField and prependDecodePathIndex annotate decode errors
// while they unwind the compiled decode stack, so only failing decodes pay
// for path construction.
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

// findFieldSlow resolves a key that missed the packed fast match: the hash
// table when one was built, otherwise a linear scan with optional ASCII
// case folding.
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

// fieldNameHash is a local lightweight mixer for small power-of-two field
// tables. It uses the SplitMix golden-gamma constant, but it is not a SplitMix
// round: the published multiplication/mix stages are deliberately absent.
func fieldNameHash(name string) uint32 {
	var head uint64
	if len(name) >= 8 {
		head = binary.LittleEndian.Uint64([]byte(name))
	} else {
		for i := range len(name) {
			head |= uint64(name[i]) << (i * 8)
		}
	}
	head ^= uint64(len(name)) * 0x9e3779b97f4a7c15
	head ^= head >> 33
	return uint32(head ^ head>>32)
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
