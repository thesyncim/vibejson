package simdjson

type typedDecShape uint8

const (
	typedDecShapeNone typedDecShape = iota
	typedDecShapeRecord
)

// typedDecodeProgram is the immutable, direction-specific field lookup
// program embedded in each typed plan node. Value embedding keeps the fields
// at their established offsets without a pointer chase in decode loops.
type typedDecodeProgram struct {
	decShape       typedDecShape
	ready          bool
	structuralFast bool
	// decBuiltinSlice is true only for []int64, []uint64, and []float64.
	// Their fused loops can grow through the concrete Go type; defined slice or
	// element types use the reflective dynamic-slice boundary.
	decBuiltinSlice bool
	// decHasReceiver lets containers skip all batching work when their element
	// graph has no standard JSON or text unmarshaler. The GC-scanned array type
	// is kept only in uncommon per-decode arena metadata, not every plan node.
	decHasReceiver bool
	fieldTableMask uint32
	// decMapScratch is the one-based slot for reusable map key and value boxes.
	// Zero keeps maps with observable boxes on the one-call allocation path.
	decMapScratch uint32
	allSet        uint64
	// Keeping the two slice headers after the scalar metadata places them at
	// offsets 104 and 128 in typedNode. Nodes in the 288-byte allocation class
	// alternate by half a cache line, and both offsets keep these headers within
	// one line in either phase.
	fields     []typedField
	fieldTable []int16
}

func compileTypedDecShape(fields []typedField) typedDecShape {
	switch len(fields) {
	case 5:
		if fields[0].op == typedOpInt64 && fields[1].op == typedOpBool &&
			fields[2].op == typedOpString && fields[3].op == typedOpString &&
			fields[4].op == typedOpArray {
			return typedDecShapeRecord
		}
	}
	return typedDecShapeNone
}
