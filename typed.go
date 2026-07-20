package simdjson

//go:generate go run typed_ops_gen.go

import (
	"encoding/binary"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unsafe"
)

// DecoderOptions controls decoding directly into caller-owned Go values.
type DecoderOptions struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int

	// ZeroCopy aliases unescaped strings into src. Callers must not mutate src
	// while decoded strings are in use. When false, decoded strings are
	// independent of src; the decoder may retain one private copy of the
	// input instead of allocating each string separately.
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
	// mode for destinations reused across decodes.
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
// Unmarshal for occasional default-option calls; use a Decoder when decoding
// the type repeatedly, when options are required, or when a caller-owned
// destination should be reused. A Decoder may be used concurrently because
// Decode keeps mutable parser state local to the call.
type Decoder[T any] struct {
	root       *typedNode
	rootSlice  *typedNode
	options    DecoderOptions
	structural bool
	scratch    *decoderPlanState
}

// CompileDecoder builds a decoder for T. Scalar and field dispatch use the
// compiled plan; runtime reflection is confined to dynamic storage and type
// boundaries such as arbitrary slices, maps, interfaces, and pointers.
func CompileDecoder[T any](opts DecoderOptions) (Decoder[T], error) {
	typ := reflect.TypeFor[T]()
	compiler := newTypedCompiler(typedCompileDecode)
	compiler.inlineFields = opts.InlineFields
	root, err := compiler.compile(typ, typ.String())
	if err != nil {
		return Decoder[T]{}, err
	}
	prepareTypedResets(root, make(map[*typedNode]bool))
	prepareDecoderReceivers(root)
	mapSlots := prepareDecoderMapScratch(root)
	structural := typedStructuralCandidate(root, make(map[*typedNode]bool))
	rootSliceType := reflect.TypeFor[[]T]()
	return Decoder[T]{
		root:       root,
		structural: structural,
		scratch:    newDecoderPlanState(mapSlots, root.decHasReceiver),
		rootSlice: &typedNode{
			kind:           typedSlice,
			baseKind:       typedSlice,
			op:             typedOpSlice,
			typedShape:     typedShape{typ: rootSliceType, name: rootSliceType.String()},
			elem:           root,
			decHasReceiver: root.decHasReceiver,
		},
		options: opts,
	}, nil
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

// Decode decodes one JSON value into dst. By default it merges like
// encoding/json; DecoderOptions.Replace resets state absent from the document.
// Slice capacities already reachable through dst are retained where possible.
//
// Decode keeps ordinary compiled destinations stack eligible. Native hooks
// receive and return cursor state by value and use ordinary addressable
// receivers; standard unmarshal methods retain their detached receiver rule.
func (plan Decoder[T]) Decode(src []byte, dst *T) error {
	if plan.root == nil {
		return fmt.Errorf("simdjson: zero Decoder")
	}
	if dst == nil {
		return fmt.Errorf("simdjson: typed Decode destination is nil")
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
		if existing := *out; existing == nil || !anyDecodeMerges(existing) {
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
	if plan.scratch != nil {
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
	defer plan.release(state)
	return decodeTypedDocument(src, options, root, dst, state)
}

//go:noinline
func (plan Decoder[T]) decodeStructural(src []byte, dst *T) error {
	state := acquireDecoderState(src)
	defer releaseDecoderState(state)
	return decodeTypedDocument(src, plan.options, plan.root, unsafe.Pointer(dst), state)
}

// DecodePrefix decodes one JSON value from the front of src into dst and
// returns the number of bytes consumed, leaving any following data
// unexamined. It is the building block for reading concatenated values;
// the streaming Reader uses it to decode without a separate boundary scan.
// Decoding semantics match Decode. Every destination decodes mid-stream
// here, including a top-level *any: the whole-document builder Decode uses
// assumes the value spans all of src, which a prefix cannot.
func (plan Decoder[T]) DecodePrefix(src []byte, dst *T) (int, error) {
	if plan.root == nil {
		return 0, fmt.Errorf("simdjson: zero Decoder")
	}
	if dst == nil {
		return 0, fmt.Errorf("simdjson: typed Decode destination is nil")
	}
	cursor := newDecoderCursor(src, plan.options)
	if plan.scratch != nil {
		cursor.state = plan.scratch.take()
		defer cursor.releasePlanState(plan.scratch)
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
// The returned slice is always the authoritative result.
func (plan Decoder[T]) DecodeArray(src []byte, dst []T) ([]T, error) {
	if plan.rootSlice == nil {
		return dst[:0], fmt.Errorf("simdjson: zero Decoder")
	}
	cursor := newDecoderCursor(src, plan.options)
	if plan.scratch != nil {
		cursor.state = plan.scratch.take()
		defer cursor.releasePlanState(plan.scratch)
	}
	cursor.skipSpace()
	var err error
	dst, err = decodeCompiledRootSlice(&cursor, plan.rootSlice, dst)
	if err != nil {
		return dst, err
	}
	if err := cursor.Finish(); err != nil {
		return dst, err
	}
	return dst, nil
}

// UnsupportedTypeError reports a type that cannot use the interface-free
// typed path.
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
	return fmt.Sprintf("simdjson: typed decoder does not support %s at %s: %s", e.Type, e.Path, e.Reason)
}

// DecodeError reports valid JSON that cannot be stored in the requested
// Go type.
type DecodeError struct {
	// Offset is the byte position of the offending value within the input.
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
		return fmt.Sprintf("simdjson: cannot decode JSON at byte %d into %s at %s: %s", e.Offset, typeName, e.Path, e.Reason)
	}
	return fmt.Sprintf("simdjson: cannot decode JSON at byte %d into %s: %s", e.Offset, typeName, e.Reason)
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
