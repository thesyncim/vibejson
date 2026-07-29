package vibejson

import (
	"reflect"

	"github.com/thesyncim/vibejson/x/jsonfields"
)

type typedSelectedFieldTrie struct {
	selected bool
	hop      int16
	children map[int]*typedSelectedFieldTrie
}

func newTypedSelectedFieldTrie(fields []jsonfields.Field) *typedSelectedFieldTrie {
	root := &typedSelectedFieldTrie{hop: -1}
	for i := range fields {
		node := root
		for _, index := range fields[i].Index {
			if node.children == nil {
				node.children = make(map[int]*typedSelectedFieldTrie)
			}
			child := node.children[index]
			if child == nil {
				child = &typedSelectedFieldTrie{hop: -1}
				node.children[index] = child
			}
			node = child
		}
		node.selected = true
	}
	return root
}

func (t *typedSelectedFieldTrie) setHop(index []int, hop int16) {
	node := t
	for _, field := range index {
		node = node.children[field]
	}
	node.hop = hop
}

func (t *typedSelectedFieldTrie) descendantHop() int16 {
	if t.hop >= 0 {
		return t.hop
	}
	for _, child := range t.children {
		if hop := child.descendantHop(); hop >= 0 {
			return hop
		}
	}
	return -1
}

func appendTypedIgnoredFieldResets(
	ops []typedResetOp,
	node *typedNode,
	typ reflect.Type,
	selected *typedSelectedFieldTrie,
	offset uintptr,
	hop uint32,
	depth uint16,
) []typedResetOp {
	for index := 0; index < typ.NumField(); index++ {
		field := typ.Field(index)
		child := selected.children[index]
		if child == nil {
			if field.Type.Size() != 0 {
				ops = append(ops, typedResetOp{
					offset: offset + field.Offset,
					typ:    field.Type,
					hop:    hop,
					depth:  depth,
					kind:   typedResetReflectZero,
				})
			}
			continue
		}
		if child.selected {
			continue
		}
		fieldType := field.Type
		if fieldType.Kind() == reflect.Pointer {
			if !field.IsExported() {
				// Replace behaves like a fresh destination. An unexported
				// embedded pointer therefore starts nil, preserving the
				// standard error when JSON would need to allocate through it.
				ops = append(ops, typedResetOp{
					offset: offset + field.Offset,
					typ:    field.Type,
					hop:    hop,
					depth:  depth,
					kind:   typedResetReflectZero,
				})
				continue
			}
			fieldType = fieldType.Elem()
			fieldHop := child.descendantHop()
			if fieldHop < 0 {
				continue
			}
			ops = appendTypedIgnoredFieldResets(
				ops, node, fieldType, child, 0,
				uint32(fieldHop)+1, depth+1,
			)
			continue
		}
		ops = appendTypedIgnoredFieldResets(
			ops, node, fieldType, child,
			offset+field.Offset, hop, depth,
		)
	}
	return ops
}

func addTypedHopResets(node *typedNode, path int16, hops []typedFieldHop, seen uint64) {
	for depth := 1; depth <= len(hops); depth++ {
		found := false
		for i := range node.hopResets {
			reset := &node.hopResets[i]
			if int(reset.depth) != depth ||
				!typedHopPrefixEqual(node.fieldHops[reset.path], hops, depth) {
				continue
			}
			reset.seen |= seen
			found = true
			break
		}
		if !found {
			node.hopResets = append(node.hopResets, typedHopReset{
				path:  path,
				depth: uint16(depth),
				seen:  seen,
			})
		}
	}
}

func typedHopPrefixEqual(first, second []typedFieldHop, depth int) bool {
	if len(first) < depth || len(second) < depth {
		return false
	}
	for i := 0; i < depth; i++ {
		if first[i].offset != second[i].offset ||
			first[i].pointee != second[i].pointee ||
			first[i].unexported != second[i].unexported {
			return false
		}
	}
	return true
}
