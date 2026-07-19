package simdjson

// typedEncodeProgram is the immutable, direction-specific field emission
// program embedded in each typed plan node. Value embedding keeps the fields
// at their established offsets without a pointer chase in encode loops.
type typedEncodeProgram struct {
	encFields   []typedEncField
	encNameData []byte
	// encClose is what the pair encoder appends after the last member:
	// a single brace normally, more when fused children close here too.
	encClose []byte
	// encPaths names each encode field for error paths, parallel to
	// encFields: fusion splices child members in, so the encode list no
	// longer parallels node.fields. Kept off typedEncField to preserve its
	// cache-line budget.
	encPaths []string
	// encFusedExtra counts the static struct levels fused into this
	// node's pair program, so depth checks preserve the exact limit the
	// unfused recursion enforced.
	encFusedExtra uint8
}

// typedEncField holds the complete encoder-only view of a struct field, so the
// hot encode loop does not touch the larger decoder field record.
type typedEncField struct {
	encName      string
	node         *typedNode
	offset       uintptr
	hop          int16
	encNameBlock uint16
	encOp        typedOp
	pairOp       typedEncPairOp
	encNameLen   uint8
	omitEmpty    bool
}

// fuseEligible reports whether a struct-typed member should splice into
// its parent: any unconditional simple struct qualifies.
func fuseEligible(field *typedEncField) bool {
	return field.encOp == typedOpStruct && field.node.encSimple
}

// fuseSimpleStructFields splices the members of directly nested simple
// structs into the parent's encode field list at compile time: the child's
// opening brace and first member name fuse into one literal, its remaining
// members follow with composed offsets, and its closing brace attaches to
// the next literal or to the parent's own close. The encoder then walks
// one flat pair program with no recursion or depth bookkeeping for the
// fused levels. Only unconditional members qualify: every field of a
// simple node is hop-free and never omitted, and struct values cannot be
// type-recursive without indirection, so nesting is finite.
func fuseSimpleStructFields(node *typedNode) {
	fusable := false
	for i := range node.encFields {
		field := &node.encFields[i]
		if fuseEligible(field) {
			fusable = true
			break
		}
	}
	node.encClose = []byte("}")
	if !fusable {
		return
	}
	fused := make([]typedEncField, 0, len(node.encFields)+8)
	fusedPaths := make([]string, 0, len(node.encFields)+8)
	pending := ""
	var extra uint8
	for i := range node.encFields {
		field := node.encFields[i]
		if !fuseEligible(&field) {
			field.encName = pending + field.encName
			pending = ""
			fused = append(fused, field)
			fusedPaths = append(fusedPaths, node.encPaths[i])
			continue
		}
		child := field.node
		if depth := child.encFusedExtra + 1; depth > extra {
			extra = depth
		}
		if len(child.encFields) == 0 {
			pending = pending + field.encName + "{" + string(child.encClose)
			continue
		}
		for j := range child.encFields {
			spliced := child.encFields[j]
			spliced.offset += field.offset
			spliced.encNameLen = 0
			spliced.encNameBlock = 0
			spliced.pairOp = typedEncPairFallback
			if j == 0 {
				// The child's first literal already lost its comma when the
				// child compiled; the parent-side comma comes from this
				// field's own literal.
				spliced.encName = pending + field.encName + "{" + spliced.encName
				pending = ""
			}
			fused = append(fused, spliced)
			fusedPaths = append(fusedPaths, node.encPaths[i]+"."+child.encPaths[j])
		}
		pending = string(child.encClose)
	}
	node.encFields = fused
	node.encPaths = fusedPaths
	node.encClose = append([]byte(pending), '}')
	node.encFusedExtra = extra
}

type typedEncPairOp uint8

const (
	typedEncPairFallback typedEncPairOp = iota
	typedEncPairStringString
	typedEncPairSliceString
	typedEncPairSliceStruct
	typedEncPairSliceSlice
	typedEncPairStructStruct
	typedEncPairMarshalerMarshaler
	typedEncPairStructSlice
	typedEncPairStringSlice
	typedEncPairMarshalerStruct
	typedEncPairMarshalerString
	typedEncPairStructString
	typedEncPairStringStruct
	typedEncPairFloat64Int64
	typedEncPairUint64Uint64
	typedEncPairStringFloat64
	typedEncPairStructInt64
	typedEncPairInt64Int64
	typedEncPairInt64String
	typedEncPairStringInt64
	typedEncPairInt64Slice
	typedEncPairSliceInt64
	typedEncPairSliceAny
	typedEncPairAnySlice
	typedEncPairAnyAny
	typedEncPairAnyInt64
	typedEncPairMapMap
)

func minimumTypedEncodedBytes(node *typedNode, op typedOp) int {
	switch op {
	case typedOpBool:
		return 4
	case typedOpString, typedOpQuoted:
		return 2
	case typedOpStruct, typedOpSlice, typedOpMap:
		return 2
	case typedOpArray:
		if node.length == 0 {
			return 2
		}
		element := minimumTypedEncodedBytes(node.elem, node.elem.encOp)
		if element >= (int(^uint(0)>>1)-1)/node.length {
			return 1
		}
		return 1 + node.length*(element+1)
	default:
		return 1
	}
}

func classifyTypedEncPair(first, second typedOp) typedEncPairOp {
	switch {
	case first == typedOpString && second == typedOpString:
		return typedEncPairStringString
	case first == typedOpSlice && second == typedOpString:
		return typedEncPairSliceString
	case first == typedOpSlice && second == typedOpStruct:
		return typedEncPairSliceStruct
	case first == typedOpSlice && second == typedOpSlice:
		return typedEncPairSliceSlice
	case first == typedOpStruct && second == typedOpStruct:
		return typedEncPairStructStruct
	case first == typedOpMarshaler && second == typedOpMarshaler:
		return typedEncPairMarshalerMarshaler
	case first == typedOpStruct && second == typedOpSlice:
		return typedEncPairStructSlice
	case first == typedOpString && second == typedOpSlice:
		return typedEncPairStringSlice
	case first == typedOpMarshaler && second == typedOpStruct:
		return typedEncPairMarshalerStruct
	case first == typedOpMarshaler && second == typedOpString:
		return typedEncPairMarshalerString
	case first == typedOpStruct && second == typedOpString:
		return typedEncPairStructString
	case first == typedOpString && second == typedOpStruct:
		return typedEncPairStringStruct
	case first == typedOpFloat64 && second == typedOpInt64:
		return typedEncPairFloat64Int64
	case first == typedOpUint64 && second == typedOpUint64:
		return typedEncPairUint64Uint64
	case first == typedOpString && second == typedOpFloat64:
		return typedEncPairStringFloat64
	case first == typedOpStruct && second == typedOpInt64:
		return typedEncPairStructInt64
	case first == typedOpInt64 && second == typedOpInt64:
		return typedEncPairInt64Int64
	case first == typedOpInt64 && second == typedOpString:
		return typedEncPairInt64String
	case first == typedOpString && second == typedOpInt64:
		return typedEncPairStringInt64
	case first == typedOpInt64 && second == typedOpSlice:
		return typedEncPairInt64Slice
	case first == typedOpSlice && second == typedOpInt64:
		return typedEncPairSliceInt64
	case first == typedOpSlice && second == typedOpAny:
		return typedEncPairSliceAny
	case first == typedOpAny && second == typedOpSlice:
		return typedEncPairAnySlice
	case first == typedOpAny && second == typedOpAny:
		return typedEncPairAnyAny
	case first == typedOpAny && second == typedOpInt64:
		return typedEncPairAnyInt64
	case first == typedOpMap && second == typedOpMap:
		return typedEncPairMapMap
	default:
		return typedEncPairFallback
	}
}
