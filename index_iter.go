package simdjson

// ArrayIter iterates array values without allocating.
//
// Two loop shapes are supported. The Next methods advance through a pointer
// receiver and read naturally in a for loop. The Valid/Current/Advance
// methods thread the iterator by value instead:
//
//	for ; it.Valid(); it = it.Advance() {
//		use(it.Current())
//	}
//
// The by-value shape lets the iterator state remain in registers. Prefer it on
// hot paths; use Next when its conventional pointer-receiver loop is clearer.
// The two shapes visit the same elements.
type ArrayIter struct {
	src       *byte
	entry     *IndexEntry
	remaining uint32
}

// ArrayIter returns an iterator over v's array elements.
func (v Node) ArrayIter() (ArrayIter, bool) {
	count, ok := v.ArrayLen()
	if !ok {
		return ArrayIter{}, false
	}
	entry := (*IndexEntry)(nil)
	if count != 0 {
		entry = tapeEntryOffset(v.entry, 1)
	}
	return ArrayIter{src: v.src, entry: entry, remaining: uint32(count)}, true
}

// Valid reports whether the cursor points at an array element.
func (it ArrayIter) Valid() bool {
	return it.remaining != 0
}

// Current returns the array element at the cursor without advancing it.
func (it ArrayIter) Current() Node {
	if it.remaining == 0 {
		return Node{}
	}
	return Node{src: it.src, entry: it.entry}
}

// CurrentKind returns the kind at the cursor without advancing it.
func (it ArrayIter) CurrentKind() Kind {
	if it.remaining == 0 {
		return Invalid
	}
	return it.entry.Kind()
}

// CurrentRaw returns the exact source slice at the cursor without advancing it.
func (it ArrayIter) CurrentRaw() RawValue {
	if it.remaining == 0 {
		return RawValue{}
	}
	entry := it.entry
	return RawValue{src: tapeSourceBytes(it.src, entry.start, entry.end)}
}

// Advance returns a cursor positioned at the next array element. Assign the
// result back to the cursor to keep iteration state register-resident.
func (it ArrayIter) Advance() ArrayIter {
	if it.remaining == 0 {
		return it
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(entry, uintptr(entry.next))
	} else {
		it.entry = nil
	}
	return it
}

// Next returns the next array element.
func (it *ArrayIter) Next() (Node, bool) {
	if it.remaining == 0 {
		return Node{}, false
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(entry, uintptr(entry.next))
	} else {
		it.entry = nil
	}
	return Node{src: it.src, entry: entry}, true
}

// NextKind advances the iterator and returns only the next value's kind.
func (it *ArrayIter) NextKind() (Kind, bool) {
	if it.remaining == 0 {
		return Invalid, false
	}
	entry := it.entry
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(entry, uintptr(entry.next))
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
		it.entry = tapeEntryOffset(entry, uintptr(entry.next))
	} else {
		it.entry = nil
	}
	return RawValue{src: tapeSourceBytes(it.src, entry.start, entry.end)}, true
}

// ObjectIter iterates ordered object key/value pairs without allocating.
//
// It supports the same two loop shapes as [ArrayIter]. Prefer the by-value
// Valid/Current/Advance form on hot paths.
type ObjectIter struct {
	src       *byte
	entry     *IndexEntry
	remaining uint32
}

// ObjectIter returns an iterator over v's object members.
func (v Node) ObjectIter() (ObjectIter, bool) {
	count, ok := v.ObjectLen()
	if !ok {
		return ObjectIter{}, false
	}
	entry := (*IndexEntry)(nil)
	if count != 0 {
		entry = tapeEntryOffset(v.entry, 1)
	}
	return ObjectIter{src: v.src, entry: entry, remaining: uint32(count)}, true
}

// Valid reports whether the cursor points at an object member.
func (it ObjectIter) Valid() bool {
	return it.remaining != 0
}

// Current returns the key and value at the cursor without advancing it.
func (it ObjectIter) Current() (key, value Node) {
	if it.remaining == 0 {
		return Node{}, Node{}
	}
	keyEntry := it.entry
	return Node{src: it.src, entry: keyEntry}, Node{src: it.src, entry: tapeEntryOffset(keyEntry, 1)}
}

// CurrentRaw returns the exact key and value source slices at the cursor. The
// key includes its JSON quotes and escapes.
func (it ObjectIter) CurrentRaw() (key, value RawValue) {
	if it.remaining == 0 {
		return RawValue{}, RawValue{}
	}
	keyEntry := it.entry
	valueEntry := tapeEntryOffset(keyEntry, 1)
	return RawValue{src: tapeSourceBytes(it.src, keyEntry.start, keyEntry.end)},
		RawValue{src: tapeSourceBytes(it.src, valueEntry.start, valueEntry.end)}
}

// Advance returns a cursor positioned at the next object member. Assign the
// result back to the cursor to keep iteration state register-resident.
func (it ObjectIter) Advance() ObjectIter {
	if it.remaining == 0 {
		return it
	}
	valueEntry := tapeEntryOffset(it.entry, 1)
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
	} else {
		it.entry = nil
	}
	return it
}

// Next returns the next ordered key/value pair. The key is a String Node.
func (it *ObjectIter) Next() (key, value Node, ok bool) {
	if it.remaining == 0 {
		return Node{}, Node{}, false
	}
	keyEntry := it.entry
	valueEntry := tapeEntryOffset(keyEntry, 1)
	key = Node{src: it.src, entry: keyEntry}
	value = Node{src: it.src, entry: valueEntry}
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
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
	valueEntry := tapeEntryOffset(keyEntry, 1)
	key = RawValue{src: tapeSourceBytes(it.src, keyEntry.start, keyEntry.end)}
	value = RawValue{src: tapeSourceBytes(it.src, valueEntry.start, valueEntry.end)}
	it.remaining--
	if it.remaining != 0 {
		it.entry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
	} else {
		it.entry = nil
	}
	return key, value, true
}
