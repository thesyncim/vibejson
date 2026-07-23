package slopjson

import (
	"encoding/json"

	"github.com/thesyncim/slopjson/document"
)

// Member is one ordered object entry. Its Value shares the containing
// document's lifetime. An unescaped Key aliases that document's source; an
// escaped Key has independent decoded storage.
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

// Value is an immutable handle into a lazily accessed document returned by
// [Parse] or [ParseOptions]. Navigation yields Values sharing the same document
// lifetime. Parse owns private source and index storage; ParseOptions with
// [Options.ZeroCopy] instead borrows src, which must remain unmodified while any
// derived Value, Node, string, or number spelling is in use. Concurrent reads
// are safe under that rule.
//
// The zero Value has kind Invalid. Accessors returning a boolean report false
// for an invalid Value, a wrong JSON kind, an absent child, or an out-of-range
// number. [Value.Array], [Value.Object], and [Value.Any] materialize caller-owned
// containers; indexed navigation and iterators do not.
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
func (v Value) Kind() document.Kind {
	return v.node.Kind()
}

// Bool returns v as a bool.
func (v Value) Bool() (bool, bool) {
	return v.node.Bool()
}

// Text returns v as a decoded string. Escaped strings have independent storage;
// unescaped strings alias the document source and therefore alias caller input
// when ParseOptions used [Options.ZeroCopy].
func (v Value) Text() (string, bool) {
	if v.node.Kind() != document.String {
		return "", false
	}
	if b, ok := v.node.StringBytes(); ok {
		return ownedBytesString(b), true
	}
	out, _ := v.node.AppendText(nil)
	return ownedBytesString(out), true
}

// NumberText returns the original JSON number spelling as a string aliasing the
// document source. With [Options.ZeroCopy], it therefore aliases caller input.
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

// Array returns a newly allocated slice of element Values sharing v's document.
// A wrong kind returns nil and false; an empty array returns a non-nil empty
// slice and true.
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

// Object returns a newly allocated slice of ordered members sharing v's
// document. A wrong kind returns nil and false; an empty object returns a
// non-nil empty slice and true.
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
// last-occurrence semantics for duplicate keys. A wrong kind or absent key
// returns a zero Value and false.
func (v Value) Get(key string) (Value, bool) {
	node, ok := v.node.Get(key)
	if !ok {
		return Value{}, false
	}
	return v.with(node), true
}

// Index returns the ith array element. A wrong kind or out-of-range index
// returns a zero Value and false.
func (v Value) Index(i int) (Value, bool) {
	node, ok := v.node.Index(i)
	if !ok {
		return Value{}, false
	}
	return v.with(node), true
}

// Any converts v to standard Go JSON shapes. Numbers are json.Number. Array and
// object containers are newly allocated; unescaped strings, number spellings,
// and unescaped object keys preserve v's source ownership, including ZeroCopy
// aliasing. A null or invalid Value returns nil.
func (v Value) Any() any {
	switch v.node.Kind() {
	case document.Null:
		return nil
	case document.Bool:
		b, _ := v.node.Bool()
		return b
	case document.Number:
		s, _ := v.node.NumberText()
		return json.Number(s)
	case document.String:
		s, _ := v.Text()
		return s
	case document.Array:
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
	case document.Object:
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

// Node returns a lightweight cursor over the same document. The returned Node
// remains valid independently of v; a ZeroCopy source must still remain
// unmodified.
func (v Value) Node() Node { return v.node }

// String returns an owned compact JSON string for v. An invalid Value returns
// "null".
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
