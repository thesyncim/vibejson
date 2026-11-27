package vibejson

import (
	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/jsoncmp"
)

// Contains reports JSONB-compatible containment. Objects use last-key
// semantics, arrays ignore order, and scalars compare by exact value.
func (v Node) Contains(needle Node) bool {
	if v.Kind() == document.Array {
		switch needle.Kind() {
		case document.Null, document.Bool, document.Number, document.String:
			// At the top level an array may contain a scalar element.
			it, _ := v.ArrayIter()
			for {
				element, ok := it.Next()
				if !ok {
					return false
				}
				if scalarNodesEqual(element, needle) {
					return true
				}
			}
		}
	}
	return nodeContains(v, needle)
}

// RawContains indexes both operands and applies Contains.
func RawContains(haystack, needle []byte) (bool, error) {
	h, err := ContainsIndex(haystack)
	if err != nil {
		return false, err
	}
	n, err := ContainsIndex(needle)
	if err != nil {
		return false, err
	}
	return h.Root().Contains(n.Root()), nil
}

// ContainsIndex validates and indexes one containment operand.
func ContainsIndex(src []byte) (Index, error) {
	entries, err := RequiredIndexEntries(src)
	if err != nil {
		return Index{}, err
	}
	return BuildIndex(src, make([]IndexEntry, entries))
}

// nodeContains applies structural containment below the top level.
func nodeContains(h, n Node) bool {
	switch n.Kind() {
	case document.Object:
		if h.Kind() != document.Object {
			return false
		}
		return objectContains(h, n)
	case document.Array:
		if h.Kind() != document.Array {
			return false
		}
		return arrayContains(h, n)
	case document.Invalid:
		return false
	default:
		return scalarNodesEqual(h, n)
	}
}

// objectContains matches every effective needle member in the haystack.
func objectContains(h, n Node) bool {
	it, _ := n.ObjectIter()
	for {
		key, value, ok := it.Next()
		if !ok {
			return true
		}
		content, clean := key.StringBytes()
		var hv Node
		if clean {
			hv, ok = h.Get(ownedBytesString(content))
		} else {
			hv, ok = objectGetEscapedKey(h, key)
		}
		if !ok {
			return false
		}
		if !nodeContains(hv, value) {
			var effective Node
			if clean {
				effective, _ = n.Get(ownedBytesString(content))
			} else {
				effective, _ = objectGetEscapedKey(n, key)
			}
			if effective.Entry == value.Entry {
				return false
			}
		}
	}
}

// objectGetEscapedKey resolves an escaped key without materializing it.
func objectGetEscapedKey(object, key Node) (Node, bool) {
	it, _ := object.ObjectIter()
	var found Node
	for {
		candidate, value, ok := it.Next()
		if !ok {
			return found, found.Entry != nil
		}
		if stringNodesEqual(candidate, key) {
			found = value
		}
	}
}

// arrayContains matches every needle element in the haystack.
func arrayContains(h, n Node) bool {
	nit, _ := n.ArrayIter()
	for {
		element, ok := nit.Next()
		if !ok {
			return true
		}
		kind := element.Kind()
		hit, _ := h.ArrayIter()
		for {
			candidate, ok := hit.Next()
			if !ok {
				return false
			}
			if candidate.Kind() == kind && nodeContains(candidate, element) {
				break
			}
		}
	}
}

// scalarNodesEqual compares equal scalar kinds and values.
func scalarNodesEqual(a, b Node) bool {
	kind := a.Kind()
	if kind != b.Kind() {
		return false
	}
	switch kind {
	case document.Null:
		return true
	case document.Bool:
		av, _ := a.Bool()
		bv, _ := b.Bool()
		return av == bv
	case document.Number:
		av, _ := a.NumberBytes()
		bv, _ := b.NumberBytes()
		return jsoncmp.NumberEqual(av, bv)
	case document.String:
		return stringNodesEqual(a, b)
	default:
		return false
	}
}

// stringNodesEqual compares decoded string content.
func stringNodesEqual(a, b Node) bool {
	ac, aClean := a.StringBytes()
	bc, bClean := b.StringBytes()
	switch {
	case aClean && bClean:
		return bytesEqualString(ac, ownedBytesString(bc))
	case aClean:
		return jsoncmp.RawStringEqualString(b.Raw().Bytes(), b.Entry.Flags(), ownedBytesString(ac))
	case bClean:
		return jsoncmp.RawStringEqualString(a.Raw().Bytes(), a.Entry.Flags(), ownedBytesString(bc))
	default:
		return jsoncmp.RawStringEqual(a.Raw().Bytes(), a.Entry.Flags(), b.Raw().Bytes(), b.Entry.Flags())
	}
}
