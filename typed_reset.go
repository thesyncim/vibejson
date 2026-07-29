package vibejson

import (
	"reflect"
	"unsafe"
)

func resetTyped(node *typedNode, dst unsafe.Pointer) {
	if typedResetWhole(node) {
		// A custom decoder owns the entire value, including state invisible to
		// the structural JSON field plan. Replace must therefore present the
		// true zero value rather than selectively clearing exported fields.
		reflect.NewAt(node.typ, dst).Elem().SetZero()
		return
	}
	if node.ready {
		applyTypedReset(node, node.reset, dst)
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
		for i := range node.hopResets {
			hop := &node.hopResets[i]
			if hop.depth == 1 {
				offset := node.fieldHops[hop.path][0].offset
				*(*unsafe.Pointer)(unsafe.Add(dst, offset)) = nil
			}
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
	case typedMap, typedIface, typedIfaceInline:
		reflect.NewAt(node.typ, dst).Elem().SetZero()
	case typedAny, typedAnyInline:
		*(*any)(dst) = nil
	}
}

func typedResetWhole(node *typedNode) bool {
	switch node.kind {
	case typedUnmarshalerJSON, typedUnmarshalerText, typedUnmarshalerSimd:
		return true
	default:
		return false
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
	typedResetIgnoredStart
	typedResetIgnoredEnd
)

type typedResetOp struct {
	offset uintptr
	size   uintptr
	typ    reflect.Type
	// hop is a one-based fieldHops index. depth selects the pointer prefix to
	// follow before applying offset; zero means offset is relative to dst.
	hop   uint32
	depth uint16
	kind  typedResetKind
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
	if typedResetWhole(node) {
		return append(ops, typedResetOp{offset: offset, kind: typedResetReflectZero, typ: node.typ})
	}
	switch node.baseKind {
	case typedBool, typedInt, typedUint, typedFloat:
		return appendTypedClear(ops, offset, node.size)
	case typedString, typedNumber:
		return append(ops, typedResetOp{offset: offset, kind: typedResetString})
	case typedSlice, typedBytes:
		return append(ops, typedResetOp{offset: offset, kind: typedResetReflectZero, typ: node.typ})
	case typedPointer:
		return append(ops, typedResetOp{offset: offset, kind: typedResetPointer})
	case typedMap, typedIface, typedIfaceInline:
		return append(ops, typedResetOp{offset: offset, kind: typedResetReflectZero, typ: node.typ})
	case typedAny, typedAnyInline:
		return append(ops, typedResetOp{offset: offset, kind: typedResetInterface})
	case typedStruct:
		for i := range node.fields {
			field := &node.fields[i]
			if field.hop >= 0 {
				continue
			}
			ops = appendTypedReset(ops, field.node, offset+field.offset)
		}
		for i := range node.hopResets {
			hop := &node.hopResets[i]
			if hop.depth == 1 {
				hopOffset := node.fieldHops[hop.path][0].offset
				ops = append(ops, typedResetOp{offset: offset + hopOffset, kind: typedResetPointer})
			}
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

func resetTypedIgnored(node *typedNode, dst unsafe.Pointer) {
	ops := node.reset
	if len(ops) == 0 || ops[0].kind != typedResetIgnoredStart {
		return
	}
	for i := 1; i < len(ops) && ops[i].kind != typedResetIgnoredEnd; i++ {
		applyTypedResetOp(node, &ops[i], dst)
	}
}

func applyTypedReset(node *typedNode, ops []typedResetOp, dst unsafe.Pointer) {
	for i := range ops {
		applyTypedResetOp(node, &ops[i], dst)
	}
}

func applyTypedResetOp(node *typedNode, op *typedResetOp, dst unsafe.Pointer) {
	if op.kind == typedResetIgnoredStart || op.kind == typedResetIgnoredEnd {
		return
	}
	target := dst
	if op.hop != 0 {
		hops := node.fieldHops[op.hop-1]
		target = resolveResetHops(dst, hops[:op.depth])
		if target == nil {
			return
		}
	}
	field := unsafe.Add(target, op.offset)
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
