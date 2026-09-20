package vibejson

import (
	"encoding/json"

	"github.com/thesyncim/vibejson/document"
)

// Member is one ordered object entry.
type Member struct {
	// Key is the decoded member name.
	Key string
	// Value is the member value.
	Value Value
}

// valueRoot keeps source and index storage alive for derived Values.
type valueRoot struct {
	src     []byte
	entries []IndexEntry
}

// Value is an immutable handle into a document returned by [Parse]. With
// [Options.ZeroCopy], derived text aliases the unchanged source slice.
type Value struct {
	node Node
	root *valueRoot
}

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

// Text returns v as a decoded string.
func (v Value) Text() (string, bool) {
	if v.node.Kind() != document.String {
		return "", false
	}
	if b, ok := v.node.StringBytes(); ok {
		return OwnedBytesString(b), true
	}
	out, _ := v.node.AppendText(nil)
	return OwnedBytesString(out), true
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

// IsInteger reports whether v has an integer spelling.
func (v Value) IsInteger() bool {
	return v.node.IsInteger()
}

// Array returns the elements as a newly allocated slice.
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

// Object returns ordered members as a newly allocated slice.
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

func nodeKeyString(key Node) string {
	if b, ok := key.StringBytes(); ok {
		return OwnedBytesString(b)
	}
	out, _ := key.AppendText(nil)
	return OwnedBytesString(out)
}

// Get returns the last object member with key.
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

// Any converts v to standard Go JSON shapes.
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

// Node returns a lightweight cursor over the same document.
func (v Value) Node() Node { return v.node }

// String returns v as compact JSON.
func (v Value) String() string {
	b, _ := v.MarshalJSON()
	return string(b)
}

func newRootValue(src []byte, entries []IndexEntry) Value {
	root := &valueRoot{src: src, entries: entries}
	node := NodeFromEntries(src, entries)
	return Value{node: node, root: root}
}
