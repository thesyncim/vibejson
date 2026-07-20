package simdjson

type typedDecShape uint8

const (
	typedDecShapeNone typedDecShape = iota
	typedDecShapeRecord
	typedDecShapeRecordFloat64x3
)

// typedDecodeProgram is the immutable, direction-specific field lookup
// program embedded in each typed plan node. Value embedding keeps the fields
// at their established offsets without a pointer chase in decode loops.
type typedDecodeProgram struct {
	fields         []typedField
	decShape       typedDecShape
	fieldTable     []int16
	fieldTableMask uint32
}

func compileTypedDecShape(fields []typedField) typedDecShape {
	switch len(fields) {
	case 5:
		if fields[0].op == typedOpInt64 && fields[1].op == typedOpBool &&
			fields[2].op == typedOpString && fields[3].op == typedOpString &&
			fields[4].op == typedOpArray {
			array := fields[4].node
			if array.length == 3 && array.elem != nil && array.elem.kind == typedFloat && array.elem.bits == 64 {
				return typedDecShapeRecordFloat64x3
			}
			return typedDecShapeRecord
		}
	}
	return typedDecShapeNone
}
