package vibejson

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thesyncim/vibejson/document"
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
			hv, ok = h.Get(OwnedBytesString(content))
		} else {
			hv, ok = objectGetEscapedKey(h, key)
		}
		if !ok {
			return false
		}
		if !nodeContains(hv, value) {
			var effective Node
			if clean {
				effective, _ = n.Get(OwnedBytesString(content))
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
		return JSONNumberEqual(av, bv)
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
		return BytesEqualString(ac, OwnedBytesString(bc))
	case aClean:
		return tapeKeyEqual(b.Raw().Bytes(), b.Entry.Flags(), OwnedBytesString(ac))
	case bClean:
		return tapeKeyEqual(a.Raw().Bytes(), a.Entry.Flags(), OwnedBytesString(bc))
	default:
		return RawJSONStringEqual(a.Raw().Bytes(), a.Entry.Flags(), b.Raw().Bytes(), b.Entry.Flags())
	}
}

// RawJSONStringEqual compares decoded content without materializing escaped
// strings.
func RawJSONStringEqual(a []byte, aFlags uint8, b []byte, bFlags uint8) bool {
	aEscaped := aFlags&TapeFlagEscaped != 0
	bEscaped := bFlags&TapeFlagEscaped != 0
	switch {
	case !aEscaped && !bEscaped:
		return BytesEqualString(a[1:len(a)-1], OwnedBytesString(b[1:len(b)-1]))
	case !aEscaped:
		return tapeKeyEqual(b, bFlags, OwnedBytesString(a[1:len(a)-1]))
	case !bEscaped:
		return tapeKeyEqual(a, aFlags, OwnedBytesString(b[1:len(b)-1]))
	}

	ai := JSONStringByteIter{Raw: a[1 : len(a)-1]}
	bi := JSONStringByteIter{Raw: b[1 : len(b)-1]}
	for {
		ab, aok := ai.Next()
		bb, bok := bi.Next()
		if aok != bok || aok && ab != bb {
			return false
		}
		if !aok {
			return true
		}
	}
}

// JSONStringByteIter decodes bytes from a validated string interior.
type JSONStringByteIter struct {
	Raw     []byte
	i       int
	encoded [utf8.UTFMax]byte
	pos     uint8
	n       uint8
}

// Next returns the next decoded UTF-8 byte.
func (it *JSONStringByteIter) Next() (byte, bool) {
	if it.pos < it.n {
		b := it.encoded[it.pos]
		it.pos++
		return b, true
	}
	if it.i == len(it.Raw) {
		return 0, false
	}
	b := it.Raw[it.i]
	if b != '\\' {
		it.i++
		return b, true
	}
	it.i++
	if it.Raw[it.i] != 'u' {
		b = decodedSimpleEscape(it.Raw[it.i])
		it.i++
		return b, true
	}
	u, _ := hex4(it.Raw, it.i+1)
	it.i += 5
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF {
		lo, _ := hex4(it.Raw, it.i+2)
		r = utf16.DecodeRune(r, rune(lo))
		it.i += 6
	}
	it.n = uint8(utf8.EncodeRune(it.encoded[:], r))
	it.pos = 1
	return it.encoded[0], true
}
