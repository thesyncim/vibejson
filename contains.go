package slopjson

import (
	"unicode/utf16"
	"unicode/utf8"

	"github.com/thesyncim/slopjson/document"
)

// JSONB-compatible containment.
//
// This file evaluates PostgreSQL's jsonb containment operator (@>) over
// indexed documents: Node.Contains is the primitive, running directly on
// two tapes, and RawContains is the one-shot spelling that indexes its
// operands first. The oracle is PostgreSQL's documented behavior — the
// curated table in testdata/contains_oracle.tsv pins that semantic contract,
// and ADR 0002's phase 4 prunes containment candidates with postings that this
// evaluator then verifies.
//
// The evaluation is one structural recursion with no heap allocation on any
// validated input. Object members resolve through the Get lookup ladder, so an
// enriched haystack (document.IndexOptions.HashKeys) rejects non-matching
// members on one hash-word compare: the needle key is hashed once and
// gates every member of the probed object. Array containment pre-filters
// candidate elements by kind before recursing. Scalars compare through the
// package's exact kernels: strings by decoded content (tapeKeyEqual's
// incremental escape comparison), numbers by exact decimal value
// (jsonNumberEqual), never through float64 rounding.
//
// jsonb collapses duplicate object keys to the last occurrence when a
// document is converted, and the package's Get contract keeps the same
// rule, so both sides agree by construction. The evaluator handles the
// needle's own duplicates without a pre-pass: members are checked in
// order, and a member that fails is re-resolved through Get once to
// decide whether it was shadowed by a later duplicate — free when there
// are no duplicates, exact when there are.

// Contains reports whether needle is contained in v under PostgreSQL's
// documented jsonb containment (@>) semantics:
//
//   - An object contains an object when, for every member of the needle,
//     the haystack has a member with the same key whose value contains the
//     needle member's value, by this definition recursively. Extra
//     haystack members are ignored; the empty object is contained in
//     every object.
//   - An array contains an array when every needle element is contained
//     in some haystack element. Order is ignored and duplicates collapse:
//     one haystack element may satisfy any number of needle elements, so
//     [1] contains [1, 1] and the empty array is contained in every
//     array.
//   - A scalar contains exactly an equal scalar: null equals null,
//     booleans compare by value, strings compare by decoded content (an
//     escape spelling equals its decoded form), and numbers compare by
//     exact numeric value rather than spelling — 1.0 contains 1 and 1e2
//     contains 100 — with exact decimal precision at any magnitude, so
//     integers beyond float64 do not falsely collapse.
//   - Structure must otherwise match: an object never contains an array
//     or scalar needle, an array never contains an object needle, and a
//     scalar never contains a container. The one exception, at the top
//     level only, is PostgreSQL's documented special case: an array v
//     contains a scalar needle when some element of v equals it. The
//     exception does not nest, and it never applies in reverse.
//
// Duplicate keys on either side resolve to an object's last member with
// that key, matching both the package's Get contract and what jsonb's
// document conversion keeps; semantics of duplicate keys before that
// conversion are out of scope. An invalid Node contains nothing and is
// contained in nothing.
//
// The Nodes may come from different documents, or from the same one.
// Contains does not allocate. The cost is one haystack lookup per clean needle
// object member and, for arrays, one scan of the haystack array per needle
// element. An escaped needle key takes an allocation-free object scan because
// its decoded spelling is deliberately not materialized.
func (v Node) Contains(needle Node) bool {
	if v.Kind() == document.Array {
		switch needle.Kind() {
		case document.Null, document.Bool, document.Number, document.String:
			// The top-level exception: a scalar matches an array haystack
			// when some element equals it. Only scalar elements can;
			// deeper structure never participates.
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

// RawContains reports whether needle is contained in haystack under the
// containment contract documented at [Node.Contains]. Both arguments must
// each hold exactly one JSON document; an invalid document returns the
// error a failed [BuildIndex] reports. RawContains indexes both documents
// per call — callers evaluating one needle against many documents, or
// many needles against one document, should build the indexes once and
// use Node.Contains directly.
func RawContains(haystack, needle []byte) (bool, error) {
	h, err := containsIndex(haystack)
	if err != nil {
		return false, err
	}
	n, err := containsIndex(needle)
	if err != nil {
		return false, err
	}
	return h.Root().Contains(n.Root()), nil
}

// containsIndex validates one containment operand and builds its exactly
// sized index.
func containsIndex(src []byte) (Index, error) {
	entries, err := RequiredIndexEntries(src)
	if err != nil {
		return Index{}, err
	}
	return BuildIndex(src, make([]IndexEntry, entries))
}

// nodeContains is the structural recursion below the top level: kinds must
// match exactly, containers recurse, scalars compare by value.
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

// objectContains reports whether every effective member of needle object n
// is matched in haystack object h. Members are checked in document order;
// h.Get supplies the last-duplicate rule on the haystack side and, when h
// is enriched, the hash-gated scan. A failing member is re-resolved
// through n.Get once so that a needle member shadowed by a later
// duplicate — whose value is not the effective one — cannot cause a false
// negative.
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
			// The key is absent from the haystack. Every duplicate of a
			// key resolves the same lookup, so shadowing cannot save it.
			return false
		}
		if !nodeContains(hv, value) {
			var effective Node
			if clean {
				effective, _ = n.Get(ownedBytesString(content))
			} else {
				effective, _ = objectGetEscapedKey(n, key)
			}
			if effective.entry == value.entry {
				return false
			}
			// A later duplicate shadows this member; that occurrence
			// decides when the iteration reaches it.
		}
	}
}

// objectGetEscapedKey resolves an escaped needle key without materializing its
// decoded spelling. It scans to the last equal key, preserving Get's duplicate
// rule, while rawJSONStringEqual incrementally decodes both sides in constant
// space. Clean needle keys stay on Get's hash-accelerated path above.
func objectGetEscapedKey(object, key Node) (Node, bool) {
	it, _ := object.ObjectIter()
	var found Node
	for {
		candidate, value, ok := it.Next()
		if !ok {
			return found, found.entry != nil
		}
		if stringNodesEqual(candidate, key) {
			found = value
		}
	}
}

// arrayContains reports whether every element of needle array n is
// contained in some element of haystack array h. The haystack scan skips
// elements of a different kind before recursing: containment below the
// top level never crosses kinds.
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

// scalarNodesEqual reports whether two Nodes are equal scalars: same kind,
// same value. Containers and invalid Nodes report false; callers dispatch
// containers before value comparison.
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
		return jsonNumberEqual(av, bv)
	case document.String:
		return stringNodesEqual(a, b)
	default:
		return false
	}
}

// stringNodesEqual compares two string Nodes by decoded content. Clean
// spellings compare bytes directly; an escaped side compares through
// tapeKeyEqual's incremental decoder, and only the escaped-versus-escaped
// case materializes one side, through a small stack buffer.
func stringNodesEqual(a, b Node) bool {
	ac, aClean := a.StringBytes()
	bc, bClean := b.StringBytes()
	switch {
	case aClean && bClean:
		return bytesEqualString(ac, ownedBytesString(bc))
	case aClean:
		return tapeKeyEqual(b.Raw().Bytes(), b.entry.flags(), ownedBytesString(ac))
	case bClean:
		return tapeKeyEqual(a.Raw().Bytes(), a.entry.flags(), ownedBytesString(bc))
	default:
		return rawJSONStringEqual(a.Raw().Bytes(), a.entry.flags(), b.Raw().Bytes(), b.entry.flags())
	}
}

// rawJSONStringEqual compares two validated JSON string spellings by decoded
// UTF-8 content. A clean side remains a direct source alias. When both sides
// contain escapes, two tiny incremental decoders meet byte-for-byte instead
// of materializing either spelling; even arbitrarily long escaped strings are
// therefore allocation-free.
func rawJSONStringEqual(a []byte, aFlags uint8, b []byte, bFlags uint8) bool {
	aEscaped := aFlags&tapeFlagEscaped != 0
	bEscaped := bFlags&tapeFlagEscaped != 0
	switch {
	case !aEscaped && !bEscaped:
		return bytesEqualString(a[1:len(a)-1], ownedBytesString(b[1:len(b)-1]))
	case !aEscaped:
		return tapeKeyEqual(b, bFlags, ownedBytesString(a[1:len(a)-1]))
	case !bEscaped:
		return tapeKeyEqual(a, aFlags, ownedBytesString(b[1:len(b)-1]))
	}

	ai := jsonStringByteIter{raw: a[1 : len(a)-1]}
	bi := jsonStringByteIter{raw: b[1 : len(b)-1]}
	for {
		ab, aok := ai.next()
		bb, bok := bi.next()
		if aok != bok || aok && ab != bb {
			return false
		}
		if !aok {
			return true
		}
	}
}

// jsonStringByteIter decodes one byte at a time from the inside of a validated
// JSON string. Unicode escapes can yield up to four UTF-8 bytes, held inline;
// validation guarantees complete escapes and valid surrogate pairing.
type jsonStringByteIter struct {
	raw     []byte
	i       int
	encoded [utf8.UTFMax]byte
	pos     uint8
	n       uint8
}

func (it *jsonStringByteIter) next() (byte, bool) {
	if it.pos < it.n {
		b := it.encoded[it.pos]
		it.pos++
		return b, true
	}
	if it.i == len(it.raw) {
		return 0, false
	}
	b := it.raw[it.i]
	if b != '\\' {
		it.i++
		return b, true
	}
	it.i++
	if it.raw[it.i] != 'u' {
		b = decodedSimpleEscape(it.raw[it.i])
		it.i++
		return b, true
	}
	u, _ := hex4(it.raw, it.i+1)
	it.i += 5
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF {
		lo, _ := hex4(it.raw, it.i+2)
		r = utf16.DecodeRune(r, rune(lo))
		it.i += 6
	}
	it.n = uint8(utf8.EncodeRune(it.encoded[:], r))
	it.pos = 1
	return it.encoded[0], true
}
