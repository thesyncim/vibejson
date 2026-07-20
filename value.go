package simdjson

import (
	"encoding/json"

	"github.com/thesyncim/simdjson/document"
)

// Kind identifies the JSON type stored in a Value.
//
// Deprecated: use document.Kind. This alias will be removed before v1.
type Kind = document.Kind

const (
	// Invalid is the zero Value or an absent lookup result.
	// Deprecated: use document.Invalid.
	Invalid = document.Invalid
	// Null is JSON null.
	// Deprecated: use document.Null.
	Null = document.Null
	// Bool is a JSON true or false value.
	// Deprecated: use document.Bool.
	Bool = document.Bool
	// Number is a JSON number whose original spelling is preserved.
	// Deprecated: use document.Number.
	Number = document.Number
	// String is a JSON string.
	// Deprecated: use document.String.
	String = document.String
	// Array is a JSON array.
	// Deprecated: use document.Array.
	Array = document.Array
	// Object is a JSON object.
	// Deprecated: use document.Object.
	Object = document.Object
)

// Member is one ordered object entry.
type Member struct {
	// Key is the decoded object member name.
	Key string
	// Value is the member value and shares its owning document.
	Value Value
}

// valueRoot owns the storage a Value tree reads from. Parse copies the source
// (unless ZeroCopy) and the structural index into a root, and every Value
// navigated from that document holds a pointer to it. The root therefore keeps
// both the source bytes and the index alive for as long as any reachable Value
// still refers to them, so a Value stays valid after the caller drops the
// original src slice.
type valueRoot struct {
	src     []byte
	entries []IndexEntry
}

// Value is an immutable, owning handle into a document returned by Parse. Use
// Value when member order matters, navigation is lazy, or results must remain
// usable without managing the source and index storage separately. For a
// caller-backed, zero-copy index use Index and Node instead.
//
// Parse builds only the structural index and reads each value straight from it
// as the caller navigates, so a document read in part is not materialized in
// full. Array and Object materialize slices on request; iterator-style access
// through the underlying indexed handles avoids those slices.
//
// A Value keeps its document alive through root: the lightweight node cursor
// aliases the owned source and index, and root is what holds that storage
// reachable. Navigation (Get, Index, Array, Object, Pointer) yields further
// Values that share the same root without reparsing. Value has no mutable
// state and is safe for concurrent reads; when ParseOptions used ZeroCopy, the
// caller must still keep the original source immutable.
type Value struct {
	node Node
	root *valueRoot
}

// with rebinds a navigated node back into v's owning root so the result keeps
// the document's storage alive.
func (v Value) with(node Node) Value {
	return Value{node: node, root: v.root}
}

// Kind returns the JSON kind of v.
func (v Value) Kind() Kind {
	return v.node.Kind()
}

// Bool returns v as a bool.
func (v Value) Bool() (bool, bool) {
	return v.node.Bool()
}

// Text returns v as a decoded (unescaped) string. Escaped strings decode into
// fresh storage; unescaped strings alias v's owned source.
func (v Value) Text() (string, bool) {
	if v.node.Kind() != String {
		return "", false
	}
	if b, ok := v.node.StringBytes(); ok {
		return ownedBytesString(b), true
	}
	out, _ := v.node.AppendText(nil)
	return ownedBytesString(out), true
}

// NumberText returns the original JSON number spelling.
func (v Value) NumberText() (string, bool) {
	return v.node.NumberText()
}

// Float64 parses a number value as float64.
func (v Value) Float64() (float64, bool) {
	return v.node.Float64()
}

// Int64 parses an integer number value as int64.
func (v Value) Int64() (int64, bool) {
	return v.node.Int64()
}

// Uint64 parses an unsigned integer number value as uint64.
func (v Value) Uint64() (uint64, bool) {
	return v.node.Uint64()
}

// IsInteger reports whether v is a number with an integer spelling. It does
// not imply that the value fits in a particular integer type.
func (v Value) IsInteger() bool {
	return v.node.IsInteger()
}

// Array returns v as an array. The element Values are materialized on demand
// and share v's root.
func (v Value) Array() ([]Value, bool) {
	iter, ok := v.node.ArrayIter()
	if !ok {
		return nil, false
	}
	n, _ := v.node.ArrayLen()
	out := make([]Value, 0, n)
	for {
		node, ok := iter.Next()
		if !ok {
			break
		}
		out = append(out, v.with(node))
	}
	return out, true
}

// Object returns v as ordered object members. The member Values are
// materialized on demand and share v's root.
func (v Value) Object() ([]Member, bool) {
	iter, ok := v.node.ObjectIter()
	if !ok {
		return nil, false
	}
	n, _ := v.node.ObjectLen()
	out := make([]Member, 0, n)
	for {
		key, val, ok := iter.Next()
		if !ok {
			break
		}
		out = append(out, Member{Key: nodeKeyString(key), Value: v.with(val)})
	}
	return out, true
}

// nodeKeyString decodes an object key node into a Go string, matching the
// decoded (unescaped) form the eager tree used for keys.
func nodeKeyString(key Node) string {
	if b, ok := key.StringBytes(); ok {
		return ownedBytesString(b)
	}
	out, _ := key.AppendText(nil)
	return ownedBytesString(out)
}

// Get returns the last object member with key, matching encoding/json's
// last-occurrence semantics for duplicate keys.
func (v Value) Get(key string) (Value, bool) {
	node, ok := v.node.Get(key)
	if !ok {
		return Value{}, false
	}
	return v.with(node), true
}

// Index returns the ith array element.
func (v Value) Index(i int) (Value, bool) {
	node, ok := v.node.Index(i)
	if !ok {
		return Value{}, false
	}
	return v.with(node), true
}

// Any converts v to standard Go JSON shapes. Numbers are json.Number.
func (v Value) Any() any {
	switch v.node.Kind() {
	case Null:
		return nil
	case Bool:
		b, _ := v.node.Bool()
		return b
	case Number:
		s, _ := v.node.NumberText()
		return json.Number(s)
	case String:
		s, _ := v.Text()
		return s
	case Array:
		n, _ := v.node.ArrayLen()
		out := make([]any, 0, n)
		iter, _ := v.node.ArrayIter()
		for {
			node, ok := iter.Next()
			if !ok {
				break
			}
			out = append(out, v.with(node).Any())
		}
		return out
	case Object:
		n, _ := v.node.ObjectLen()
		out := make(map[string]any, n)
		iter, _ := v.node.ObjectIter()
		for {
			key, val, ok := iter.Next()
			if !ok {
				break
			}
			out[nodeKeyString(key)] = v.with(val).Any()
		}
		return out
	default:
		return nil
	}
}

// Node returns the underlying lightweight cursor. Its typed interior pointers
// keep the document's source and index backing arrays alive independently of v.
func (v Value) Node() Node { return v.node }

// String returns compact JSON for v.
func (v Value) String() string {
	b, _ := v.MarshalJSON()
	return string(b)
}

// newRootValue wraps owned source and index storage in a root and returns the
// document's top-level Value.
func newRootValue(src []byte, entries []IndexEntry) Value {
	root := &valueRoot{src: src, entries: entries}
	node := nodeFromStorage(src, entries)
	return Value{node: node, root: root}
}
