package vibejson

type typedDecShape uint8

const (
	typedDecShapeNone typedDecShape = iota
	typedDecShapeRecord
	typedDecShapeMask = 0x0f

	// The upper shape bits carry uncommon Replace-only record properties.
	// Keeping them here preserves the decode program's compact hot layout:
	// standalone booleans would move allSet and the field slices.
	typedDecFlagWideSeen     typedDecShape = 1 << 6
	typedDecFlagResetIgnored typedDecShape = 1 << 7

	// Narrow records reserve one high presence bit for the Replace-only inline
	// map epilogue. Inline records with more than 62 JSON fields use the
	// scalable presence executor instead.
	typedSeenInlineMap uint64 = 1 << 62
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
	// decReplaceAliases marks a Replace root graph with two or more reusable
	// reference slots, or a repeated DecodeArray element graph with any
	// reusable reference. Its operation-local tracker is managed by typed decode.
	decReplaceAliases bool
	// decReplaceDestination marks a Replace graph whose pointer pointee or
	// slice-backing layout can overlap non-reference storage inside the reused
	// destination.
	decReplaceDestination bool
	// decNeedsScratch separates ordinary Decode's hot state requirement from
	// a plan cache that may exist solely for DecodeArray alias tracking.
	decNeedsScratch bool
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

func (p *typedDecodeProgram) decShapeKind() typedDecShape {
	return p.decShape & typedDecShapeMask
}

func (p *typedDecodeProgram) hasDecWideSeen() bool {
	return p.decShape&typedDecFlagWideSeen != 0
}

func (p *typedDecodeProgram) setDecWideSeen() {
	p.decShape |= typedDecFlagWideSeen
}

func (p *typedDecodeProgram) hasDecResetIgnored() bool {
	return p.decShape&typedDecFlagResetIgnored != 0
}

func (p *typedDecodeProgram) setDecResetIgnored() {
	p.decShape |= typedDecFlagResetIgnored
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
