package simdjson

import (
	"sort"
	"strings"
	"unicode/utf8"
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
	case Null:
		return append(dst, "null"...)
	case Bool:
		if b, _ := v.node.Bool(); b {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case Number:
		s, _ := v.node.NumberBytes()
		return append(dst, s...)
	case String:
		return appendJSONNodeString(dst, v.node)
	case Array:
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
	case Object:
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
	return appendIndentBytes(dst, src, prefix, indent, defaultMaxDepth)
}

// AppendIndent appends pretty JSON for v to dst.
func (v Value) AppendIndent(dst []byte, prefix, indent string) []byte {
	return appendIndentValue(dst, v, prefix, indent, 0)
}

func appendIndentValue(dst []byte, v Value, prefix, indent string, depth int) []byte {
	switch v.node.Kind() {
	case Array:
		if n, _ := v.node.ArrayLen(); n == 0 {
			return append(dst, "[]"...)
		}
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
			dst = append(dst, '\n')
			dst = append(dst, prefix...)
			dst = append(dst, strings.Repeat(indent, depth+1)...)
			dst = appendIndentValue(dst, v.with(node), prefix, indent, depth+1)
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		dst = append(dst, strings.Repeat(indent, depth)...)
		return append(dst, ']')
	case Object:
		if n, _ := v.node.ObjectLen(); n == 0 {
			return append(dst, "{}"...)
		}
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
			dst = append(dst, '\n')
			dst = append(dst, prefix...)
			dst = append(dst, strings.Repeat(indent, depth+1)...)
			dst = appendJSONNodeString(dst, key)
			dst = append(dst, ": "...)
			dst = appendIndentValue(dst, v.with(val), prefix, indent, depth+1)
		}
		dst = append(dst, '\n')
		dst = append(dst, prefix...)
		dst = append(dst, strings.Repeat(indent, depth)...)
		return append(dst, '}')
	default:
		return v.AppendJSON(dst)
	}
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
			_, size := utf8.DecodeRune(text[i:])
			if size != 1 {
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
// new owned compact JSON buffer. It is a deterministic simdjson form, not RFC
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
func AppendCanonicalize(dst, src []byte) ([]byte, error) {
	v, err := ParseOptions(src, Options{ZeroCopy: true})
	if err != nil {
		return dst, err
	}
	return appendCanonical(dst, v), nil
}

// appendCanonical appends compact JSON for v with every object's members sorted
// by decoded key. Arrays keep their order. Strings and numbers are normalized
// exactly as AppendJSON does, so the canonical form is stable regardless of the
// source's escape spelling or member order.
func appendCanonical(dst []byte, v Value) []byte {
	switch v.node.Kind() {
	case Array:
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
			dst = appendCanonical(dst, v.with(node))
		}
		return append(dst, ']')
	case Object:
		members, _ := v.Object()
		sort.SliceStable(members, func(i, j int) bool {
			return members[i].Key < members[j].Key
		})
		dst = append(dst, '{')
		for i := range members {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, members[i].Key)
			dst = append(dst, ':')
			dst = appendCanonical(dst, members[i].Value)
		}
		return append(dst, '}')
	default:
		return v.AppendJSON(dst)
	}
}
