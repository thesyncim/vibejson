package vibejson

import "github.com/thesyncim/vibejson/x/byteview"

// FieldCursor is a stateful, forward-resuming lookup over one object's members.
// Find returns the first match at or after the current position and wraps once.
type FieldCursor struct {
	src *byte
	// first is the wrap-around key entry.
	first *IndexEntry
	// pos is the next key entry to examine.
	pos *IndexEntry
	// step is the flat-object member stride, or 0 for linked spans.
	step uint32
	// index tracks pos's ordinal for wrap detection.
	count uint32
	index uint32
	// hashed records whether key hashes are available.
	hashed bool
}

// Fields returns a FieldCursor over v's object members. A non-object or empty
// object returns a cursor that resolves nothing.
func (v Node) Fields() FieldCursor {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return FieldCursor{}
	}
	first := EntryAt(v.Entry, 1)
	// Flat objects use a fixed two-entry stride.
	var step uint32
	if v.Entry.Next == 2*uint32(count)+1 {
		step = 2
	}
	return FieldCursor{
		src:    v.Src,
		first:  first,
		pos:    first,
		step:   step,
		count:  uint32(count),
		hashed: v.Entry.KeysHashed(),
	}
}

// ValueFieldCursor is the Value-level counterpart of FieldCursor.
type ValueFieldCursor struct {
	cursor FieldCursor
	root   *valueRoot
}

// Fields returns a ValueFieldCursor over v's object members. A non-object or
// empty object returns a cursor that resolves nothing.
func (v Value) Fields() ValueFieldCursor {
	return ValueFieldCursor{cursor: v.node.Fields(), root: v.root}
}

// Find applies the [FieldCursor.Find] contract and returns a Value sharing the
// originating document. An absent key returns a zero Value and false.
func (c *ValueFieldCursor) Find(key string) (Value, bool) {
	node, ok := c.cursor.Find(key)
	if !ok {
		return Value{}, false
	}
	return Value{node: node, root: c.root}, true
}

// nextKeyEntry returns the next key entry.
func (c *FieldCursor) nextKeyEntry(keyEntry *IndexEntry) *IndexEntry {
	if c.step != 0 {
		return EntryAt(keyEntry, uintptr(c.step))
	}
	valueEntry := EntryAt(keyEntry, 1)
	return EntryAt(valueEntry, uintptr(valueEntry.Next))
}

// findEntryQuery scans at most one full turn and advances after a hit.
func (c *FieldCursor) findEntryQuery(key string, queryHash uint32) *IndexEntry {
	if c.first == nil {
		return nil
	}
	rawLen := uint32(len(key)) + 2
	keyEntry := c.pos
	index := c.index
	for scanned := uint32(0); scanned < c.count; scanned++ {
		flags := keyEntry.Flags()
		candidate := flags&TapeFlagEscaped != 0
		if !candidate {
			if c.hashed {
				candidate = keyEntry.Next == queryHash
			} else {
				candidate = keyEntry.End-keyEntry.Start == rawLen
			}
		}
		if candidate &&
			tapeKeyEqual(byteview.SliceRange(c.src, keyEntry.Start, keyEntry.End), flags, key) {
			// Resume after the match, wrapping at the last member.
			valueEntry := EntryAt(keyEntry, 1)
			next := index + 1
			if next == c.count {
				c.pos = c.first
				c.index = 0
			} else {
				c.pos = c.nextKeyEntry(keyEntry)
				c.index = next
			}
			return valueEntry
		}
		index++
		if index == c.count {
			keyEntry = c.first
			index = 0
		} else {
			keyEntry = c.nextKeyEntry(keyEntry)
		}
	}
	c.pos = c.first
	c.index = 0
	return nil
}

// Find returns the first member matching key from the current position.
func (c *FieldCursor) Find(key string) (Node, bool) {
	// Hash an enriched lookup once.
	var queryHash uint32
	if c.hashed {
		queryHash = HashKey(key)
	}
	entry := c.findEntryQuery(key, queryHash)
	if entry == nil {
		return Node{}, false
	}
	return Node{Src: c.src, Entry: entry}, true
}

// FindCompiled is Find with a precomputed key hash.
func (c *FieldCursor) FindCompiled(k CompiledKey) (Node, bool) {
	entry := c.findEntryQuery(k.Key, k.Hash)
	if entry == nil {
		return Node{}, false
	}
	return Node{Src: c.src, Entry: entry}, true
}
