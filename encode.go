package vibejson

import (
	"cmp"
	"slices"
	"sync"
	"unicode/utf8"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/byteview"
)

// MarshalJSON implements json.Marshaler.
func (v Value) MarshalJSON() ([]byte, error) {
	return v.AppendJSON(nil), nil
}

// AppendJSON appends compact JSON for v to dst. Strings are decoded and
// re-encoded through appendJSONString, so non-canonical escapes in the source
// are normalized exactly as encoding/json would emit them; number spellings are
// preserved verbatim.
func (v Value) AppendJSON(dst []byte) []byte {
	switch v.node.Kind() {
	case document.Null:
		return append(dst, "null"...)
	case document.Bool:
		if b, _ := v.node.Bool(); b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case document.Number:
		s, _ := v.node.NumberBytes()
		return append(dst, s...)
	case document.String:
		return appendJSONNodeString(dst, v.node)
	case document.Array:
		dst = append(dst, '[')
		iter, _ := v.node.ArrayIter()
		for i := 0; ; i++ {
			node, ok := iter.Next()
			if !ok {
				break
			}
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = v.with(node).AppendJSON(dst)
		}
		return append(dst, ']')
	case document.Object:
		dst = append(dst, '{')
		iter, _ := v.node.ObjectIter()
		for i := 0; ; i++ {
			key, val, ok := iter.Next()
			if !ok {
				break
			}
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONNodeString(dst, key)
			dst = append(dst, ':')
			dst = v.with(val).AppendJSON(dst)
		}
		return append(dst, '}')
	default:
		return append(dst, "null"...)
	}
}

// appendJSONNodeString decodes a string node and re-encodes it, normalizing
// escape spelling the same way appendJSONString does for a Go string, but
// without allocating for the common unescaped case.
func appendJSONNodeString(dst []byte, node Node) []byte {
	if b, ok := node.StringBytes(); ok {
		return appendJSONStringBytes(dst, b)
	}
	decoded, _ := node.AppendText(nil)
	return appendJSONStringBytes(dst, decoded)
}

// Indent validates src and returns a new owned formatted JSON buffer using
// prefix and indent. It neither modifies nor retains its inputs and is safe for
// concurrent calls. On error it returns nil and a [SyntaxError].
func Indent(src []byte, prefix, indent string) ([]byte, error) {
	return AppendIndent(nil, src, prefix, indent)
}

// AppendIndent validates src and appends formatted JSON text using prefix and
// indent.
// Like json.Indent, string and number tokens are copied from src verbatim, so
// escape spelling and number literals are preserved exactly; only structural
// text is inserted. prefix and indent are copied verbatim and need not be JSON
// whitespace, so non-whitespace formatting can make the result invalid, just
// as with json.Indent. The returned slice is caller-owned and may reuse dst's
// capacity; no input is retained. The writable capacity of dst must not overlap
// src. On error it returns dst unchanged in length and a [SyntaxError], although
// unused capacity may contain partial output. Calls are safe concurrently when
// their sources remain immutable and their writable destination storage is
// independent.
func AppendIndent(dst, src []byte, prefix, indent string) ([]byte, error) {
	return appendIndentBytes(dst, src, prefix, indent, DefaultMaxDepth)
}

// appendJSONStringBytes appends text as a quoted, canonically escaped JSON
// string. It is the shared core behind appendJSONString and the decoded-node
// path, so a Value re-encodes strings identically whether the caller holds a
// Go string or an already-decoded byte slice.
// Provenance: GO-STRING-001. Scalar escaping is conservatively treated as an
// adaptation of Go encoding/json appendString at commit
// d468ad3648be469ffc4090e4586c29709182d6b6; BSD-3-Clause, see LICENSE-GO.
// Byte-slice integration and SIMD prefix scanning are local changes.
func appendJSONStringBytes(dst, text []byte) []byte {
	const hex = "0123456789abcdef"
	dst = append(dst, '"')
	start := 0
	for i := 0; i < len(text); {
		c := text[i]
		if c >= utf8.RuneSelf {
			r, size := utf8.DecodeRune(text[i:])
			if size != 1 {
				if r == '\u2028' || r == '\u2029' {
					dst = append(dst, text[start:i]...)
					dst = append(dst, '\\', 'u', '2', '0', '2', hex[r&0xf])
					i += size
					start = i
					continue
				}
				i += size
				continue
			}
			dst = append(dst, text[start:i]...)
			dst = append(dst, '\\', 'u', 'f', 'f', 'f', 'd')
			i++
			start = i
			continue
		}
		if c >= 0x20 && c != '"' && c != '\\' {
			i++
			continue
		}

		dst = append(dst, text[start:i]...)
		switch c {
		case '"', '\\':
			dst = append(dst, '\\', c)
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			dst = append(dst, '\\', 'u', '0', '0', hex[c>>4], hex[c&0xF])
		}
		i++
		start = i
	}
	dst = append(dst, text[start:]...)
	return append(dst, '"')
}

// Canonicalize validates src, sorts object members recursively, and returns a
// new owned compact JSON buffer. It is a deterministic vibejson form, not RFC
// 8785: decoded keys sort by UTF-8 byte order, duplicate keys are retained in
// their original relative order, arrays retain their order, string escapes are
// normalized like [Value.AppendJSON], and number spellings are preserved. It
// neither modifies nor retains src and is safe for concurrent calls. Invalid
// JSON returns a [SyntaxError]; any error returns a nil result.
func Canonicalize(src []byte) ([]byte, error) {
	return AppendCanonicalize(nil, src)
}

// AppendCanonicalize validates src, sorts object members recursively, and
// appends the same form as Canonicalize to dst. The returned slice is
// caller-owned and may reuse dst's capacity; src is not retained. The writable
// capacity of dst must not overlap src. Validation completes before output
// begins, so an error returns dst without writing to it. Canonicalization builds
// temporary navigation storage and, for objects, member-order storage even when
// dst has sufficient capacity. Calls are safe concurrently when their sources
// remain immutable and their writable destination storage is independent.
type canonicalMember struct {
	key  string
	node Node
}

type canonicalScratch struct {
	entries []IndexEntry
	members []canonicalMember
	keyBuf  []byte
}

var canonicalScratchPool = sync.Pool{
	New: func() any {
		return &canonicalScratch{
			entries: make([]IndexEntry, 0, 64),
		}
	},
}

const (
	canonicalMaxPooledEntries = 4096
	canonicalMaxPooledMembers = 256
	canonicalMaxPooledKeyBuf  = 4096
)

func AppendCanonicalize(dst, src []byte) ([]byte, error) {
	scratch := canonicalScratchPool.Get().(*canonicalScratch)
	opts := document.IndexOptions{MaxDepth: DefaultMaxDepth}
	idx, err := BuildIndexOptions(src, scratch.entries[:0], opts)
	if err == document.ErrIndexFull {
		needed, reqErr := RequiredIndexEntries(src)
		if reqErr != nil {
			if cap(scratch.entries) <= canonicalMaxPooledEntries {
				scratch.entries = scratch.entries[:0]
				canonicalScratchPool.Put(scratch)
			}
			return dst, reqErr
		}
		if needed < cap(scratch.entries)*2 {
			needed = cap(scratch.entries) * 2
		}
		scratch.entries = make([]IndexEntry, 0, needed)
		idx, err = BuildIndexOptions(src, scratch.entries[:0], opts)
	}
	if err != nil {
		if cap(scratch.entries) <= canonicalMaxPooledEntries {
			scratch.entries = scratch.entries[:0]
			canonicalScratchPool.Put(scratch)
		}
		return dst, err
	}
	scratch.entries = idx.Entries[:0]
	scratch.members = scratch.members[:0]
	scratch.keyBuf = scratch.keyBuf[:0]
	out := appendCanonicalNode(dst, idx.Root(), scratch)
	if cap(scratch.entries) <= canonicalMaxPooledEntries &&
		cap(scratch.members) <= canonicalMaxPooledMembers &&
		cap(scratch.keyBuf) <= canonicalMaxPooledKeyBuf {
		clear(scratch.members[:cap(scratch.members)])
		scratch.entries = scratch.entries[:0]
		scratch.members = scratch.members[:0]
		scratch.keyBuf = scratch.keyBuf[:0]
		canonicalScratchPool.Put(scratch)
	}
	return out, nil
}

// appendCanonicalNode appends compact JSON for node with every object's members
// sorted by decoded key. Arrays keep their order. Strings and numbers are
// normalized exactly as AppendJSON does, so the canonical form is stable
// regardless of the source's escape spelling or member order.
func appendCanonicalNode(dst []byte, node Node, scratch *canonicalScratch) []byte {
	switch node.Kind() {
	case document.Null:
		return append(dst, "null"...)
	case document.Bool:
		if b, _ := node.Bool(); b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case document.Number:
		s, _ := node.NumberBytes()
		return append(dst, s...)
	case document.String:
		return appendJSONNodeString(dst, node)
	case document.Array:
		dst = append(dst, '[')
		iter, _ := node.ArrayIter()
		for i := 0; ; i++ {
			elem, ok := iter.Next()
			if !ok {
				break
			}
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendCanonicalNode(dst, elem, scratch)
		}
		return append(dst, ']')
	case document.Object:
		count, _ := node.ObjectLen()
		if count == 0 {
			return append(dst, '{', '}')
		}
		iter, _ := node.ObjectIter()
		var stack [16]canonicalMember
		var members []canonicalMember
		memberBase := len(scratch.members)
		keyBase := len(scratch.keyBuf)
		if count <= len(stack) {
			members = stack[:0]
		} else {
			if cap(scratch.members)-memberBase < count {
				next := make([]canonicalMember, memberBase, memberBase+count)
				copy(next, scratch.members)
				scratch.members = next
			}
			members = scratch.members[memberBase:memberBase : memberBase+count]
		}
		for {
			keyNode, valNode, ok := iter.Next()
			if !ok {
				break
			}
			var key string
			if b, ok := keyNode.StringBytes(); ok {
				key = byteview.String(b)
			} else {
				start := len(scratch.keyBuf)
				scratch.keyBuf, _ = keyNode.AppendText(scratch.keyBuf)
				key = byteview.String(scratch.keyBuf[start:])
			}
			members = append(members, canonicalMember{key: key, node: valNode})
		}
		if count > len(stack) {
			scratch.members = scratch.members[:memberBase+len(members)]
		}
		if len(members) <= 16 {
			// Stable insertion sort for small objects.
			for i := 1; i < len(members); i++ {
				cur := members[i]
				j := i - 1
				for j >= 0 && members[j].key > cur.key {
					members[j+1] = members[j]
					j--
				}
				members[j+1] = cur
			}
		} else {
			slices.SortStableFunc(members, func(a, b canonicalMember) int {
				return cmp.Compare(a.key, b.key)
			})
		}
		dst = append(dst, '{')
		for i := range members {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, members[i].key)
			dst = append(dst, ':')
			dst = appendCanonicalNode(dst, members[i].node, scratch)
		}
		if count > len(stack) {
			clear(scratch.members[memberBase : memberBase+len(members)])
			scratch.members = scratch.members[:memberBase]
		}
		scratch.keyBuf = scratch.keyBuf[:keyBase]
		return append(dst, '}')
	default:
		return dst
	}
}
