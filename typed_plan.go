package slopjson

import "reflect"

// typedKind classifies a compiled node. Order is load-bearing: dispatch
// ranges over contiguous runs of these values, so new kinds go at the end.
type typedKind uint8

const (
	typedInvalid typedKind = iota
	typedBool
	typedString
	typedNumber
	typedInt
	typedUint
	typedFloat
	typedStruct
	typedSlice
	typedArray
	typedPointer
	typedMap
	typedAny
	typedBytes
	typedUnmarshalerJSON
	typedUnmarshalerText
	typedMarshalerJSON
	typedMarshalerText
	typedIface
	typedTime
	typedUnmarshalerSimd
	typedMarshalerSimd
)

// typedOp selects a struct field's decode operation. Order is load-bearing:
// the scalar run (typedOpBool through typedOpFloat64) is range-checked when
// retagging errors, so new scalar ops belong inside it and containers after.
type typedOp uint8

const (
	// BEGIN GENERATED TYPED OP ENUM
	typedOpInvalid typedOp = iota
	typedOpBool
	typedOpString
	typedOpNumber
	typedOpInt8
	typedOpInt16
	typedOpInt32
	typedOpInt64
	typedOpUint8
	typedOpUint16
	typedOpUint32
	typedOpUint64
	typedOpFloat32
	typedOpFloat64
	typedOpStruct
	typedOpSlice
	typedOpArray
	typedOpPointer
	typedOpMap
	typedOpAny
	typedOpBytes
	typedOpQuoted
	typedOpUnmarshaler
	typedOpMarshaler
	typedOpIface
	// END GENERATED TYPED OP ENUM
)

// encoderBackingSlot is intentionally distinct from the marshaler/key scratch
// indexes stored beside it. Mixing those two plan-local index spaces would be
// memory-safe but would silently borrow the wrong reusable value storage.
type encoderBackingSlot int32

const noEncoderBackingSlot encoderBackingSlot = -1

// typedFieldHop is one embedded-pointer traversal on the way to a flattened
// field: dereference the pointer at offset, allocating pointee on decode.
type typedFieldHop struct {
	offset     uintptr
	pointee    reflect.Type
	unexported bool
}

// typedNode combines the direction-neutral shape with the immutable decode
// program and a reference to an encode program when the node describes a
// struct in an encode plan. Field order is part of the hot layout and is pinned
// by the typed plan layout tests.
type typedNode struct {
	kind           typedKind // decode dispatch
	encKind        typedKind // encode dispatch
	encNonAddrKind typedKind // encode dispatch for map/interface values
	baseKind       typedKind // structural layout, for resets and emptiness
	op             typedOp
	encOp          typedOp
	typedShape
	elem             *typedNode
	mapKeyKind       typedMapKeyKind
	mapKeyTextDecode bool
	mapKeyTextEncode bool
	typedDecodeProgram
	encodeProgram *typedEncodeProgram
	fieldHops     [][]typedFieldHop
	hopResets     []uintptr
	reset         []typedResetOp
	encSimple     bool
	// encHasPtrMarshaler marks types that can reach a pointer-receiver
	// marshaler through struct fields or array elements without crossing a
	// pointer, slice, or map. Only these pay the non-addressable flag when
	// encoded as a map value or interface content.
	encHasPtrMarshaler bool
	// inlineMap is the ",inline" catch-all for a struct: unknown members
	// decode into it and its entries re-emit at the struct's own level. It
	// is nil for every struct without one, so structs pay nothing for the
	// feature they do not use.
	inlineMap       *typedInlineMap
	encScratch      int32
	encMapKey       int32
	encBacking      encoderBackingSlot
	encScratchLimit int
}

// typedInlineMap describes a struct's ",inline" map[string]T catch-all: where
// the map lives in the struct and how one entry value is decoded and encoded.
type typedInlineMap struct {
	offset        uintptr
	mapType       reflect.Type
	elem          *typedNode
	decMapScratch uint32
	// encKey indexes a reserved encoder-scratch box of the key type. Encoding
	// copies each member name into it with SetIterKey, avoiding a reflect
	// allocation per member; values collect into a pooled backing slice so each
	// gets independent storage. It is -1 when no scratch is reserved (dynamic
	// interface plans).
	encKey          int32
	encBacking      encoderBackingSlot
	encScratchLimit int
}

// typedField is one struct member of a compiled decode plan. The key fields
// implement the packed-name fast match: key holds the first eight bytes the
// decoder expects after the opening quote — for names of six bytes or fewer
// that word also packs the closing quote and the colon, so one masked
// eight-byte compare matches name, terminator, and separator at once.
// keyMask selects the significant bytes, and keyFold holds the ASCII case
// bits of the name's letters so the case-insensitive retry is one more mask.
// Longer names compare their first eight bytes here and the rest by memcmp.
type typedField struct {
	name    string
	offset  uintptr
	node    *typedNode
	seen    uint64 // single bit identifying this field in the struct's set
	key     uint64 // expected first source word, packed as described above
	keyMask uint64 // significant bytes of key; zero disables the fast match
	keyFold uint64 // 0x20 at each letter position, for case-folded compares
	pos     int32  // declaration position, resumes expected-order matching
	hop     int16  // index into fieldHops for embedded fields, or -1
	keyLen  uint8  // name length in bytes
	op      typedOp
}
