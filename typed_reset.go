package slopjson

import (
	"reflect"
	"unsafe"
)

func resetTyped(node *typedNode, dst unsafe.Pointer) {
	if node.ready {
		applyTypedReset(node.reset, dst)
		return
	}
	switch node.baseKind {
	case typedBool:
		*(*bool)(dst) = false
	case typedString, typedNumber:
		*(*string)(dst) = ""
	case typedInt, typedUint:
		switch node.bits {
		case 8:
			*(*uint8)(dst) = 0
		case 16:
			*(*uint16)(dst) = 0
		case 32:
			*(*uint32)(dst) = 0
		case 64:
			*(*uint64)(dst) = 0
		}
	case typedFloat:
		if node.bits == 32 {
			*(*float32)(dst) = 0
		} else {
			*(*float64)(dst) = 0
		}
	case typedStruct:
		for i := range node.fields {
			field := &node.fields[i]
			if field.hop >= 0 {
				continue
			}
			resetTyped(field.node, unsafe.Add(dst, field.offset))
		}
		for _, hopOffset := range node.hopResets {
			*(*unsafe.Pointer)(unsafe.Add(dst, hopOffset)) = nil
		}
		if node.inlineMap != nil {
			// The catch-all is not a declared field, so the loop above never
			// reaches it; clear the map so a reused destination starts
			// empty instead of merging stale unknown members.
			reflect.NewAt(node.inlineMap.mapType, unsafe.Add(dst, node.inlineMap.offset)).Elem().SetZero()
		}
	case typedSlice, typedBytes:
		// Reset to the true zero value (nil), so a reused destination under
		// Replace reports an absent slice as nil rather than an empty non-nil
		// slice; the latter marshals as [] instead of null and fails == nil.
		reflect.NewAt(node.typ, dst).Elem().SetZero()
	case typedArray:
		for i := 0; i < node.length; i++ {
			resetTyped(node.elem, unsafe.Add(dst, uintptr(i)*node.elem.size))
		}
	case typedPointer:
		*(*unsafe.Pointer)(dst) = nil
	case typedMap, typedIface:
		reflect.NewAt(node.typ, dst).Elem().SetZero()
	case typedAny:
		*(*any)(dst) = nil
	}
}

type typedResetKind uint8

const (
	typedResetByte typedResetKind = iota
	typedResetUint16
	typedResetUint32
	typedResetUint64
	typedResetBytes
	typedResetString
	typedResetReflectZero
	typedResetPointer
	typedResetInterface
)

type typedResetOp struct {
	offset uintptr
	size   uintptr
	kind   typedResetKind
	typ    reflect.Type
}

func prepareTypedResets(node *typedNode, seen map[*typedNode]bool) {
	if node == nil || seen[node] {
		return
	}
	seen[node] = true
	if node.kind == typedStruct || node.kind == typedArray {
		node.reset = appendTypedReset(node.reset, node, 0)
		node.ready = true
	}
	prepareTypedResets(node.elem, seen)
	for i := range node.fields {
		prepareTypedResets(node.fields[i].node, seen)
	}
	if node.inlineMap != nil {
		prepareTypedResets(node.inlineMap.elem, seen)
	}
}

func appendTypedReset(ops []typedResetOp, node *typedNode, offset uintptr) []typedResetOp {
	switch node.baseKind {
	case typedBool, typedInt, typedUint, typedFloat:
		return appendTypedClear(ops, offset, node.size)
	case typedString, typedNumber:
		return append(ops, typedResetOp{offset: offset, kind: typedResetString})
	case typedSlice, typedBytes:
		return append(ops, typedResetOp{offset: offset, kind: typedResetReflectZero, typ: node.typ})
	case typedPointer:
		return append(ops, typedResetOp{offset: offset, kind: typedResetPointer})
	case typedMap, typedIface:
		return append(ops, typedResetOp{offset: offset, kind: typedResetReflectZero, typ: node.typ})
	case typedAny:
		return append(ops, typedResetOp{offset: offset, kind: typedResetInterface})
	case typedStruct:
		for i := range node.fields {
			field := &node.fields[i]
			if field.hop >= 0 {
				continue
			}
			ops = appendTypedReset(ops, field.node, offset+field.offset)
		}
		for _, hopOffset := range node.hopResets {
			ops = append(ops, typedResetOp{offset: offset + hopOffset, kind: typedResetPointer})
		}
		if node.inlineMap != nil {
			ops = append(ops, typedResetOp{
				offset: offset + node.inlineMap.offset,
				kind:   typedResetReflectZero,
				typ:    node.inlineMap.mapType,
			})
		}
		return ops
	case typedArray:
		if typedRawClearable(node.elem) {
			return appendTypedClear(ops, offset, node.size)
		}
		for i := 0; i < node.length; i++ {
			ops = appendTypedReset(ops, node.elem, offset+uintptr(i)*node.elem.size)
		}
	}
	return ops
}

func typedRawClearable(node *typedNode) bool {
	switch node.baseKind {
	case typedBool, typedInt, typedUint, typedFloat:
		return true
	case typedArray:
		return typedRawClearable(node.elem)
	default:
		return false
	}
}

func appendTypedClear(ops []typedResetOp, offset, size uintptr) []typedResetOp {
	kind := typedResetBytes
	switch size {
	case 1:
		kind = typedResetByte
	case 2:
		kind = typedResetUint16
	case 4:
		kind = typedResetUint32
	case 8:
		kind = typedResetUint64
	}
	return append(ops, typedResetOp{offset: offset, size: size, kind: kind})
}

func applyTypedReset(ops []typedResetOp, dst unsafe.Pointer) {
	for i := range ops {
		op := &ops[i]
		field := unsafe.Add(dst, op.offset)
		switch op.kind {
		case typedResetByte:
			*(*uint8)(field) = 0
		case typedResetUint16:
			*(*uint16)(field) = 0
		case typedResetUint32:
			*(*uint32)(field) = 0
		case typedResetUint64:
			*(*uint64)(field) = 0
		case typedResetBytes:
			clear(unsafe.Slice((*byte)(field), int(op.size)))
		case typedResetString:
			*(*string)(field) = ""
		case typedResetReflectZero:
			reflect.NewAt(op.typ, field).Elem().SetZero()
		case typedResetPointer:
			*(*unsafe.Pointer)(field) = nil
		case typedResetInterface:
			*(*any)(field) = nil
		}
	}
}
