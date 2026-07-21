package simdjson

// typedEncodeProgram is the immutable, direction-specific field emission
// program referenced only by struct nodes in encode plans.
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

// typedEncodeNodeStorage keeps a struct's node and encode program in one heap
// object. Decode plans and encode nodes of every other kind allocate only the
// smaller typedNode, while struct encoding retains one allocation and locality
// between the node and its field program.
type typedEncodeNodeStorage struct {
	node    typedNode
	program typedEncodeProgram
}

func newTypedEncodeNode() *typedNode {
	storage := new(typedEncodeNodeStorage)
	storage.node.encodeProgram = &storage.program
	return &storage.node
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
	program := node.encodeProgram
	fusable := false
	for i := range program.encFields {
		field := &program.encFields[i]
		if fuseEligible(field) {
			fusable = true
			break
		}
	}
	program.encClose = []byte("}")
	if !fusable {
		return
	}
	fused := make([]typedEncField, 0, len(program.encFields)+8)
	fusedPaths := make([]string, 0, len(program.encFields)+8)
	pending := ""
	var extra uint8
	for i := range program.encFields {
		field := program.encFields[i]
		if !fuseEligible(&field) {
			field.encName = pending + field.encName
			pending = ""
			fused = append(fused, field)
			fusedPaths = append(fusedPaths, program.encPaths[i])
			continue
		}
		child := field.node
		childProgram := child.encodeProgram
		if depth := childProgram.encFusedExtra + 1; depth > extra {
			extra = depth
		}
		if len(childProgram.encFields) == 0 {
			pending = pending + field.encName + "{" + string(childProgram.encClose)
			continue
		}
		for j := range childProgram.encFields {
			spliced := childProgram.encFields[j]
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
			fusedPaths = append(fusedPaths, program.encPaths[i]+"."+childProgram.encPaths[j])
		}
		pending = string(childProgram.encClose)
	}
	program.encFields = fused
	program.encPaths = fusedPaths
	program.encClose = append([]byte(pending), '}')
	program.encFusedExtra = extra
}

type typedEncPairOp uint8

const (
	typedEncPairFallback typedEncPairOp = iota
	typedEncPairStringString
	typedEncPairSliceInt64
	typedEncPairMapMap
	typedEncPairInt64Bool
	typedEncPairInt64Int64
	typedEncPairStringInt64
	typedEncPairInt64String
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
	case first == typedOpSlice && second == typedOpInt64:
		return typedEncPairSliceInt64
	case first == typedOpMap && second == typedOpMap:
		return typedEncPairMapMap
	case first == typedOpInt64 && second == typedOpBool:
		return typedEncPairInt64Bool
	case first == typedOpInt64 && second == typedOpInt64:
		return typedEncPairInt64Int64
	case first == typedOpString && second == typedOpInt64:
		return typedEncPairStringInt64
	case first == typedOpInt64 && second == typedOpString:
		return typedEncPairInt64String
	default:
		return typedEncPairFallback
	}
}
