package simdjson

import (
	"encoding"
	"encoding/json"
	"reflect"
	"strings"
	"time"
	"unicode"
)

type typedCompileMode uint8

const (
	typedCompileDecode typedCompileMode = iota
	typedCompileEncode
)

// typedCompiler still constructs the shared pre-split node graph, but mode
// identifies the public plan that owns the graph. Keeping the direction at the
// compiler boundary lets decode-only and encode-only construction move out in
// independently benchmarked steps without changing executor layout first.
type typedCompiler struct {
	nodes           map[reflect.Type]*typedNode
	mode            typedCompileMode
	encScratchTypes []reflect.Type
	encBackingSlots int
	encHasMap       bool
	escapeHTML      bool
	// dynamic marks plans compiled for interface values at encode time.
	// Their nodes run against whatever static plan is executing, so they
	// must never carry indexes into that plan's scratch slots.
	dynamic bool
	// inlineFields activates the ",inline" catch-all extension. When false the
	// tag is inert and a ",inline" map compiles as an ordinary named field, so
	// the feature is opt-in and free for every type that does not request it.
	inlineFields bool
}

func newTypedCompiler(mode typedCompileMode) typedCompiler {
	return typedCompiler{
		nodes: make(map[reflect.Type]*typedNode),
		mode:  mode,
	}
}

func (c *typedCompiler) compilesEncode() bool {
	return c.mode == typedCompileEncode
}

func (c *typedCompiler) compilesDecode() bool {
	return c.mode == typedCompileDecode
}

// compileInlineMap records a struct's ",inline" catch-all. The field must be a
// map with a string key and no pointer indirection to reach it, matching
// encoding/json/v2; its presence moves the struct off the packed encode path
// so the trailing member splice has somewhere to run.
func (c *typedCompiler) compileInlineMap(node *typedNode, structType reflect.Type, resolved resolvedField, path string) error {
	mapType := resolved.typ
	if mapType.Kind() != reflect.Map || mapType.Key().Kind() != reflect.String {
		return &UnsupportedTypeError{Type: mapType, Path: path, Reason: `",inline" requires a map with a string key`}
	}
	if node.inlineMap != nil {
		return &UnsupportedTypeError{Type: structType, Path: path, Reason: `a struct may declare only one ",inline" field`}
	}
	offset, hops, err := c.fieldHops(structType, resolved.index, path+"."+resolved.name)
	if err != nil {
		return err
	}
	if hops != nil {
		return &UnsupportedTypeError{Type: mapType, Path: path, Reason: `",inline" field must not sit behind an embedded pointer`}
	}
	elem, err := c.compile(mapType.Elem(), path+"[inline]")
	if err != nil {
		return err
	}
	inline := &typedInlineMap{offset: offset, mapType: mapType, elem: elem}
	if !c.compilesEncode() {
		node.inlineMap = inline
		return nil
	}
	inline.encKey = -1
	inline.encBacking = noEncoderBackingSlot
	inline.encScratchLimit = encoderMapScratchLimit(mapType.Elem())
	// Reuse the same pooled scratch as encodeMap: one map iterator and entry
	// slice per encode, plus a reserved key box and a pooled value backing, so
	// a populated catch-all encodes without per-member allocation.
	c.encHasMap = true
	if !c.dynamic {
		inline.encKey = int32(len(c.encScratchTypes))
		c.encScratchTypes = append(c.encScratchTypes, mapType.Key())
		inline.encBacking = encoderBackingSlot(c.encBackingSlots)
		c.encBackingSlots++
	}
	node.inlineMap = inline
	node.encSimple = false
	return nil
}

var jsonNumberReflectType = reflect.TypeFor[json.Number]()
var timeReflectType = reflect.TypeFor[time.Time]()

// typedElemHasCustomMethods reports whether values or pointers of typ
// implement any of the JSON or text marshaling interfaces, which takes
// precedence over the byte-slice base64 form.
func typedElemHasCustomMethods(typ reflect.Type) bool {
	ptr := reflect.PointerTo(typ)
	return typ.Implements(jsonMarshalerReflectType) || ptr.Implements(jsonMarshalerReflectType) ||
		typ.Implements(textMarshalerReflectType) || ptr.Implements(textMarshalerReflectType) ||
		typ.Implements(jsonUnmarshalerReflectType) || ptr.Implements(jsonUnmarshalerReflectType) ||
		typ.Implements(textUnmarshalerReflectType) || ptr.Implements(textUnmarshalerReflectType)
}

func (c *typedCompiler) compile(typ reflect.Type, path string) (*typedNode, error) {
	if node := c.nodes[typ]; node != nil {
		return node, nil
	}
	node := &typedNode{
		typedShape: typedShape{typ: typ, name: typ.String(), size: typ.Size()},
		encScratch: -1, encMapKey: -1, encBacking: noEncoderBackingSlot,
	}
	c.nodes[typ] = node

	if err := c.compileStructural(node, typ, path); err != nil {
		// A custom un/marshaler stands in for the broken structural layout;
		// a direction that still needs structure reports failure at runtime.
		node.kind, node.encKind, node.baseKind = typedInvalid, typedInvalid, typedInvalid
		node.op, node.encOp = typedOpInvalid, typedOpInvalid
		node.fields, node.encFields, node.fieldHops, node.hopResets = nil, nil, nil, nil
		node.elem = nil
		if !c.applyInterfaceKinds(node, typ) {
			return nil, err
		}
		if c.compilesDecode() {
			if node.kind == typedInvalid {
				return nil, err
			}
		} else if node.encKind == typedInvalid {
			return nil, err
		}
		c.clearOppositeDirection(node)
		c.nodes[typ] = node
		return node, nil
	}
	node.baseKind = node.kind
	if c.compilesEncode() {
		node.encKind = node.kind
		node.encOp = node.op
		node.encNonAddrKind = node.encKind
	}
	c.applyInterfaceKinds(node, typ)
	c.clearOppositeDirection(node)
	return node, nil
}

func (c *typedCompiler) clearOppositeDirection(node *typedNode) {
	if c.compilesDecode() {
		node.encKind = typedInvalid
		node.encNonAddrKind = typedInvalid
		node.encOp = typedOpInvalid
	} else {
		node.kind = typedInvalid
		node.op = typedOpInvalid
	}
}

// compileStructural fills node with typ's structural layout.
func (c *typedCompiler) compileStructural(node *typedNode, typ reflect.Type, path string) error {
	decode := c.compilesDecode()
	if typ == jsonNumberReflectType {
		node.kind = typedNumber
		node.op = typedOpNumber
		return nil
	}

	switch typ.Kind() {
	case reflect.Bool:
		node.kind = typedBool
		node.op = typedOpBool
	case reflect.String:
		node.kind = typedString
		node.op = typedOpString
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		node.kind = typedInt
		node.bits = typ.Bits()
		switch node.bits {
		case 8:
			node.op = typedOpInt8
		case 16:
			node.op = typedOpInt16
		case 32:
			node.op = typedOpInt32
		case 64:
			node.op = typedOpInt64
		}
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		node.kind = typedUint
		node.bits = typ.Bits()
		switch node.bits {
		case 8:
			node.op = typedOpUint8
		case 16:
			node.op = typedOpUint16
		case 32:
			node.op = typedOpUint32
		case 64:
			node.op = typedOpUint64
		}
	case reflect.Float32, reflect.Float64:
		node.kind = typedFloat
		node.bits = typ.Bits()
		if node.bits == 32 {
			node.op = typedOpFloat32
		} else {
			node.op = typedOpFloat64
		}
	case reflect.Pointer:
		node.kind = typedPointer
		node.op = typedOpPointer
		elem, err := c.compile(typ.Elem(), path+"*")
		if err != nil {
			return err
		}
		node.elem = elem
	case reflect.Slice:
		if elem := typ.Elem(); elem.Kind() == reflect.Uint8 &&
			!typedElemHasCustomMethods(elem) {
			// encoding/json only treats a byte slice as base64 when the
			// element type brings no marshaling methods of its own.
			node.kind = typedBytes
			node.op = typedOpBytes
			break
		}
		node.kind = typedSlice
		node.op = typedOpSlice
		if decode {
			node.decBuiltinSlice = isBuiltinScalarSlice(typ)
		}
		elem, err := c.compile(typ.Elem(), path+"[]")
		if err != nil {
			return err
		}
		node.elem = elem
	case reflect.Array:
		node.kind = typedArray
		node.op = typedOpArray
		node.length = typ.Len()
		elem, err := c.compile(typ.Elem(), path+"[]")
		if err != nil {
			return err
		}
		node.elem = elem
	case reflect.Interface:
		if typ.NumMethod() != 0 {
			node.kind = typedIface
			node.op = typedOpIface
			break
		}
		node.kind = typedAny
		node.op = typedOpAny
	case reflect.Map:
		keyKind, keyOK := classifyMapKey(typ.Key())
		if !keyOK {
			return c.unsupported(typ, path, "unsupported map key type")
		}
		node.mapKeyKind = keyKind
		if decode {
			node.mapKeyTextDecode = reflect.PointerTo(typ.Key()).Implements(textUnmarshalerReflectType)
		} else {
			// encoding/json only consults the value method set for key encoding.
			node.mapKeyTextEncode = typ.Key().Implements(textMarshalerReflectType)
			if mapKeyStringKindFirst && typ.Key().Kind() == reflect.String {
				node.mapKeyTextEncode = false
			}
		}
		node.kind = typedMap
		node.op = typedOpMap
		if !decode {
			c.encHasMap = true
			if !c.dynamic {
				// Reserve an addressable box of the key type in the encoder
				// scratch so encodeMap copies each member name into it with
				// SetIterKey instead of letting reflect box a fresh key; values
				// collect into the pooled valueBacking for independent slots.
				node.encMapKey = int32(len(c.encScratchTypes))
				c.encScratchTypes = append(c.encScratchTypes, typ.Key())
				node.encBacking = encoderBackingSlot(c.encBackingSlots)
				c.encBackingSlots++
			}
			node.encScratchLimit = encoderMapScratchLimit(typ.Elem())
		}
		elem, err := c.compile(typ.Elem(), path+"[key]")
		if err != nil {
			return err
		}
		node.elem = elem
	case reflect.Struct:
		node.kind = typedStruct
		node.op = typedOpStruct
		node.encSimple = !decode
		for _, resolved := range resolveStructFields(typ) {
			if resolved.inline && c.inlineFields {
				if err := c.compileInlineMap(node, typ, resolved, path); err != nil {
					return err
				}
				continue
			}
			fieldNode, err := c.compile(resolved.typ, path+"."+resolved.name)
			if err != nil {
				return err
			}
			offset, hops, hopErr := c.fieldHops(typ, resolved.index, path+"."+resolved.name)
			if hopErr != nil {
				return hopErr
			}
			fieldHop := int16(-1)
			if hops != nil {
				fieldHop = int16(len(node.fieldHops))
				node.fieldHops = append(node.fieldHops, hops)
				if decode {
					// The embedded pointer slot is not a leaf field, so replace
					// style resets must clear it explicitly.
					node.hopResets = append(node.hopResets, hops[0].offset)
				} else {
					node.encSimple = false
				}
			}
			if decode {
				fieldIndex := len(node.fields)
				field := typedField{
					name: resolved.name, offset: offset, node: fieldNode,
					op: fieldNode.op, pos: int32(fieldIndex), hop: fieldHop,
				}
				if resolved.quoted {
					quotedNode := fieldNode
					if quotedNode.kind == typedPointer && resolved.typ.Name() == "" {
						quotedNode = quotedNode.elem
					}
					switch quotedNode.baseKind {
					case typedBool, typedString, typedNumber, typedInt, typedUint, typedFloat:
						if field.op != typedOpUnmarshaler {
							field.op = typedOpQuoted
						}
					}
				}
				if fieldIndex < 64 {
					field.seen = uint64(1) << fieldIndex
				}
				name := resolved.name
				if len(name) <= 7 {
					for byteIndex := range len(name) {
						char := name[byteIndex]
						field.key |= uint64(char) << (byteIndex * 8)
						if lower := char | 0x20; 'a' <= lower && lower <= 'z' {
							field.keyFold |= 0x20 << (byteIndex * 8)
						}
					}
					field.key |= uint64('"') << (len(name) * 8)
					if len(name) <= 6 {
						field.key |= uint64(':') << ((len(name) + 1) * 8)
						field.keyMask = ^uint64(0) >> ((6 - len(name)) * 8)
					} else {
						field.keyMask = ^uint64(0)
					}
					field.keyLen = uint8(len(name))
				} else if len(name) <= 255 {
					for byteIndex := range 8 {
						char := name[byteIndex]
						field.key |= uint64(char) << (byteIndex * 8)
						if lower := char | 0x20; 'a' <= lower && lower <= 'z' {
							field.keyFold |= 0x20 << (byteIndex * 8)
						}
					}
					field.keyMask = ^uint64(0)
					field.keyLen = uint8(len(name))
				}
				node.fields = append(node.fields, field)
			} else {
				encField := typedEncField{
					encName: "," + string(appendEncodedJSONString(nil, resolved.name, c.escapeHTML)) + ":",
					node:    fieldNode, offset: offset, hop: fieldHop,
					encOp: fieldNode.encOp, omitEmpty: resolved.omitEmpty,
				}
				if resolved.quoted {
					quotedNode := fieldNode
					if quotedNode.baseKind == typedPointer && resolved.typ.Name() == "" {
						quotedNode = quotedNode.elem
					}
					switch quotedNode.baseKind {
					case typedBool, typedString, typedNumber, typedInt, typedUint, typedFloat:
						if encField.encOp != typedOpMarshaler {
							encField.encOp = typedOpQuoted
						}
					}
				}
				node.encFields = append(node.encFields, encField)
				node.encPaths = append(node.encPaths, resolved.name)
				if resolved.omitEmpty {
					node.encSimple = false
				}
			}
		}
		if decode {
			node.structuralFast = node.inlineMap == nil
			for i := range node.fields {
				field := &node.fields[i]
				if field.hop >= 0 || field.keyMask == 0 || field.keyLen > 7 {
					node.structuralFast = false
					break
				}
				switch field.op {
				// BEGIN GENERATED TYPED STRUCTURAL FIELD ELIGIBILITY
				case typedOpBool, typedOpString, typedOpInt64, typedOpFloat64, typedOpStruct, typedOpSlice, typedOpArray:
				// END GENERATED TYPED STRUCTURAL FIELD ELIGIBILITY
				default:
					node.structuralFast = false
				}
			}
			if node.structuralFast {
				node.decShape = compileTypedDecShape(node.fields)
			}
			if len(node.fields) <= 64 {
				if len(node.fields) == 64 {
					node.allSet = ^uint64(0)
				} else {
					node.allSet = uint64(1)<<len(node.fields) - 1
				}
			}
			// A fold-based fast match must never shadow another field's exact match.
			for i := range node.fields {
				for j := range node.fields {
					if i != j && strings.EqualFold(node.fields[i].name, node.fields[j].name) {
						node.fields[i].keyFold = 0
					}
				}
			}
			if len(node.fields) >= 8 {
				tableSize := 16
				for tableSize < len(node.fields)*2 {
					tableSize *= 2
				}
				node.fieldTable = make([]int16, tableSize)
				node.fieldTableMask = uint32(tableSize - 1)
				for i := range node.fields {
					slot := fieldNameHash(node.fields[i].name) & node.fieldTableMask
					for node.fieldTable[slot] != 0 {
						slot = (slot + 1) & node.fieldTableMask
					}
					node.fieldTable[slot] = int16(i + 1)
				}
			}
		} else {
			if node.encSimple {
				fuseSimpleStructFields(node)
				for i := 0; i+1 < len(node.encFields); i += 2 {
					node.encFields[i].pairOp = classifyTypedEncPair(node.encFields[i].encOp, node.encFields[i+1].encOp)
				}
				if len(node.encFields) != 0 {
					node.encFields[0].encName = node.encFields[0].encName[1:]
				}
				const blockBytes = 16
				// A wide store is safe when successful encoding is guaranteed to
				// overwrite every byte through its end. Keep the last short-name
				// tail out of the packed table so AppendJSON never modifies bytes
				// past its result.
				tailMin := 1 // closing brace
				for i := len(node.encFields) - 1; i >= 0; i-- {
					field := &node.encFields[i]
					valueMin := minimumTypedEncodedBytes(field.node, field.encOp)
					if valueMin >= blockBytes-tailMin {
						tailMin = blockBytes
					} else {
						tailMin += valueMin
					}
					n := len(field.encName)
					if n <= blockBytes && tailMin >= blockBytes-n {
						field.encNameLen = uint8(n)
					}
					if n >= blockBytes-tailMin {
						tailMin = blockBytes
					} else {
						tailMin += n
					}
				}
				for i := range node.encFields {
					field := &node.encFields[i]
					block := len(node.encNameData) / blockBytes
					if field.encNameLen != 0 && block <= int(^uint16(0)) {
						field.encNameBlock = uint16(block)
						start := len(node.encNameData)
						node.encNameData = append(node.encNameData, make([]byte, blockBytes)...)
						copy(node.encNameData[start:], field.encName)
					} else {
						field.encNameLen = 0
					}
				}
			}
		}
	default:
		return c.unsupported(typ, path, "kind "+typ.Kind().String()+" would require interface or reflective value dispatch")
	}
	return nil
}

// typedMapKeyKind classifies how map keys convert to and from JSON member
// names, following encoding/json: text unmarshalers win on decode, string
// kinds win on encode, and integer kinds round trip through base 10.
type typedMapKeyKind uint8

const (
	mapKeyString typedMapKeyKind = iota
	mapKeyInt
	mapKeyUint
	mapKeyText
)

func classifyMapKey(keyType reflect.Type) (typedMapKeyKind, bool) {
	implementsText := keyType.Implements(textMarshalerReflectType) ||
		reflect.PointerTo(keyType).Implements(textUnmarshalerReflectType)
	switch keyType.Kind() {
	case reflect.String:
		return mapKeyString, true
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return mapKeyInt, true
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		return mapKeyUint, true
	default:
		if implementsText {
			return mapKeyText, true
		}
		return 0, false
	}
}

var (
	jsonMarshalerReflectType   = reflect.TypeFor[json.Marshaler]()
	jsonUnmarshalerReflectType = reflect.TypeFor[json.Unmarshaler]()
	textMarshalerReflectType   = reflect.TypeFor[encoding.TextMarshaler]()
	textUnmarshalerReflectType = reflect.TypeFor[encoding.TextUnmarshaler]()
)

// applyInterfaceKinds overrides dispatch for types with custom un/marshalers,
// reporting whether any interface applies. json.Number keeps its fast path.
func (c *typedCompiler) applyInterfaceKinds(node *typedNode, typ reflect.Type) bool {
	// Interface values dispatch on their concrete type instead; json.Number
	// keeps its fast path.
	if typ == jsonNumberReflectType || typ.Kind() == reflect.Interface {
		return false
	}
	pointerType := reflect.PointerTo(typ)
	applied := false
	if c.compilesDecode() {
		if pointerType.Implements(unmarshalerSimdReflectType) {
			// Native hooks remain opt-in. Decode uses owned receiver/cursor state;
			// encode uses ordinary GC-visible receiver ownership. There is no
			// layout-sensitive fast mode.
			node.kind = typedUnmarshalerSimd
			node.op = typedOpUnmarshaler
			applied = true
		} else if typ.Implements(jsonUnmarshalerReflectType) || pointerType.Implements(jsonUnmarshalerReflectType) {
			node.kind = typedUnmarshalerJSON
			node.op = typedOpUnmarshaler
			applied = true
		} else if typ.Implements(textUnmarshalerReflectType) || pointerType.Implements(textUnmarshalerReflectType) {
			node.kind = typedUnmarshalerText
			node.op = typedOpUnmarshaler
			applied = true
		}
		return applied
	}
	if typ == timeReflectType {
		node.encKind = typedTime
		node.encNonAddrKind = typedTime
		node.encOp = typedOpMarshaler
		return true
	}
	if pointerType.Implements(marshalerSimdReflectType) {
		node.encKind = typedMarshalerSimd
		node.encOp = typedOpMarshaler
		if typ.Implements(marshalerSimdReflectType) {
			node.encNonAddrKind = typedMarshalerSimd
		}
		return true
	}
	if typ.Implements(jsonMarshalerReflectType) {
		node.encNonAddrKind = typedMarshalerJSON
		applied = true
	} else if typ.Implements(textMarshalerReflectType) {
		node.encNonAddrKind = typedMarshalerText
		applied = true
	}
	if pointerType.Implements(jsonMarshalerReflectType) {
		node.encKind = typedMarshalerJSON
		node.encOp = typedOpMarshaler
		applied = true
	} else if typ.Implements(jsonMarshalerReflectType) {
		node.encKind = typedMarshalerJSON
		node.encOp = typedOpMarshaler
		applied = true
	} else if pointerType.Implements(textMarshalerReflectType) {
		node.encKind = typedMarshalerText
		node.encOp = typedOpMarshaler
		applied = true
	} else if typ.Implements(textMarshalerReflectType) {
		node.encKind = typedMarshalerText
		node.encOp = typedOpMarshaler
		applied = true
	}
	if !c.dynamic &&
		(typ.Implements(jsonMarshalerReflectType) || typ.Implements(textMarshalerReflectType)) {
		node.encScratch = int32(len(c.encScratchTypes))
		c.encScratchTypes = append(c.encScratchTypes, typ)
	}
	return applied
}

// Provenance: GO-FIELDS-001. Adapted from encoding/json isValidTag at Go
// commit d468ad3648be469ffc4090e4586c29709182d6b6,
// src/encoding/json/encode.go; BSD-3-Clause, see LICENSE-GO.
func validTypedTag(name string) bool {
	if name == "" {
		return false
	}
	for _, char := range name {
		if strings.ContainsRune("!#$%&()*+-./:;<=>?@[]^_{|}~ ", char) {
			continue
		}
		if !unicode.IsLetter(char) && !unicode.IsDigit(char) {
			return false
		}
	}
	return true
}

// fieldHops turns a flattened field's index path into a cumulative offset
// plus the embedded pointer dereferences on the way.
func (c *typedCompiler) fieldHops(root reflect.Type, index []int, path string) (uintptr, []typedFieldHop, error) {
	var hops []typedFieldHop
	offset := uintptr(0)
	current := root
	for position, fieldIndex := range index {
		structField := current.Field(fieldIndex)
		offset += structField.Offset
		if position == len(index)-1 {
			break
		}
		next := structField.Type
		if next.Kind() == reflect.Pointer {
			// Like encoding/json, an unexported embedded pointer only fails
			// when decoding must allocate through it.
			hops = append(hops, typedFieldHop{
				offset:     offset,
				pointee:    next.Elem(),
				unexported: !structField.IsExported(),
			})
			offset = 0
			next = next.Elem()
		}
		current = next
	}
	return offset, hops, nil
}

func (c *typedCompiler) unsupported(typ reflect.Type, path, reason string) error {
	delete(c.nodes, typ)
	return &UnsupportedTypeError{Type: typ, Path: path, Reason: reason}
}
