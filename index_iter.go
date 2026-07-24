package vibejson

import (
	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/internal/byteview"
)

// ArrayIter iterates array values without allocating.
//
// The iterator and returned Nodes or RawValues borrow the originating Node's
// document. An iterator is single-consumer and must not be advanced
// concurrently; independent iterators may advance concurrently while the
// borrowed document remains immutable.
type ArrayIter struct {
	src       *byte
	entry     *IndexEntry
	remaining uint32
}

// ArrayIter returns an iterator over v's array elements. A wrong kind returns a
// zero iterator and false; an empty array returns an exhausted iterator and
// true.
func (v Node) ArrayIter() (ArrayIter, bool) {
	count, ok := v.ArrayLen()
	if !ok {
		return ArrayIter{}, false
	}
	entry := (*IndexEntry)(nil)
	if count != 0 {
		entry = EntryAt(v.Entry, 1)
	}
	return ArrayIter{src: v.Src, entry: entry, remaining: uint32(count)}, true
}

// Next returns the next array element.
func (it *ArrayIter) Next() (Node, bool) {
	if it.remaining == 0 {
		return Node{}, false
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = EntryAt(entry, uintptr(entry.Next))
	} else {
		it.entry = nil
	}
	return Node{Src: it.src, Entry: entry}, true
}

// NextKind advances the iterator and returns only the next value's kind.
func (it *ArrayIter) NextKind() (document.Kind, bool) {
	if it.remaining == 0 {
		return document.Invalid, false
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = EntryAt(entry, uintptr(entry.Next))
	} else {
		it.entry = nil
	}
	return entry.Kind(), true
}

// NextRaw advances the iterator and returns the next exact source slice.
func (it *ArrayIter) NextRaw() (RawValue, bool) {
	if it.remaining == 0 {
		return RawValue{}, false
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = EntryAt(entry, uintptr(entry.Next))
	} else {
		it.entry = nil
	}
	return RawValue{Src: byteview.SliceRange(it.src, entry.Start, entry.End)}, true
}

// ObjectIter iterates ordered object key/value pairs without allocating.
//
// The iterator and returned Nodes or RawValues borrow the originating Node's
// document. An iterator is single-consumer and must not be advanced
// concurrently; independent iterators may advance concurrently while the
// borrowed document remains immutable.
type ObjectIter struct {
	src       *byte
	entry     *IndexEntry
	remaining uint32
}

// ObjectIter returns an iterator over v's object members. A wrong kind returns a
// zero iterator and false; an empty object returns an exhausted iterator and
// true.
func (v Node) ObjectIter() (ObjectIter, bool) {
	count, ok := v.ObjectLen()
	if !ok {
		return ObjectIter{}, false
	}
	entry := (*IndexEntry)(nil)
	if count != 0 {
		entry = EntryAt(v.Entry, 1)
	}
	return ObjectIter{src: v.Src, entry: entry, remaining: uint32(count)}, true
}

// Next returns the next ordered key/value pair. The key is a String Node.
func (it *ObjectIter) Next() (key, value Node, ok bool) {
	if it.remaining == 0 {
		return Node{}, Node{}, false
	}
	keyEntry := it.entry
	valueEntry := EntryAt(keyEntry, 1)
	key = Node{Src: it.src, Entry: keyEntry}
	value = Node{Src: it.src, Entry: valueEntry}
	it.remaining--
	if it.remaining != 0 {
		it.entry = EntryAt(valueEntry, uintptr(valueEntry.Next))
	} else {
		it.entry = nil
	}
	return key, value, true
}

// NextRaw advances the iterator and returns the next exact key and value
// source slices. The key includes its JSON quotes and escapes.
func (it *ObjectIter) NextRaw() (key, value RawValue, ok bool) {
	if it.remaining == 0 {
		return RawValue{}, RawValue{}, false
	}
	keyEntry := it.entry
	valueEntry := EntryAt(keyEntry, 1)
	key = RawValue{Src: byteview.SliceRange(it.src, keyEntry.Start, keyEntry.End)}
	value = RawValue{Src: byteview.SliceRange(it.src, valueEntry.Start, valueEntry.End)}
	it.remaining--
	if it.remaining != 0 {
		it.entry = EntryAt(valueEntry, uintptr(valueEntry.Next))
	} else {
		it.entry = nil
	}
	return key, value, true
}
