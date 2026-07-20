package simdjson

// FieldCursor is a stateful, forward-resuming lookup over one object's members.
// It is simdjson's find_field_unordered analogue: code that reads several known
// fields in roughly document order pays for each key only from where the last
// match left off instead of rescanning every member. Obtain one from
// Value.Fields or Node.Fields.
//
// Find returns the FIRST member whose key matches, in forward scan order,
// starting at the cursor's current position and wrapping around the end exactly
// once back to the start, stopping when the scan returns to where it began. This
// is deliberately NOT encoding/json's last-occurrence-wins rule: on an object
// with duplicate keys, FieldCursor reports the first match reachable from its
// current position, while Get reports the last member in document order. For
// duplicate-key documents that must follow encoding/json semantics, use Get.
//
// A FieldCursor holds no heap state and every Find is allocation-free. Like
// Node and Value, it aliases the document's source and index and is valid only
// while the object it was taken from stays reachable and unmodified. The zero
// FieldCursor, and any cursor taken from a non-object, resolves nothing. Find
// mutates the cursor position, so one cursor is single-consumer and must not be
// used concurrently; independent copies have independent positions.
type FieldCursor struct {
	src *byte
	// first is the object's first member key entry, or nil for a non-object or
	// empty object. It is the wrap-around target and the scan's fixed origin.
	first *IndexEntry
	// pos is the next key entry a scan will examine. It equals first when the
	// cursor has never matched and after a wrap that consumes the whole object.
	pos *IndexEntry
	// step is the fixed entry stride between adjacent members for a flat object
	// (every value one entry), or 0 when spans must be chased through next.
	step uint32
	// count is the object's member count; index tracks pos's ordinal so a scan
	// can advance member by member and know when it has wrapped a full turn.
	count uint32
	index uint32
}

// Fields returns a forward-resuming field cursor over v's object members. See
// FieldCursor for the first-match-in-scan-order contract, which differs from
// Get's last-occurrence-wins rule. A cursor over a non-object resolves nothing.
func (v Node) Fields() FieldCursor {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return FieldCursor{}
	}
	first := tapeEntryOffset(v.entry, 1)
	// A flat object stores every value in a single entry, so members sit at a
	// fixed two-entry stride and the scan needs no span chase.
	var step uint32
	if v.entry.next == 2*uint32(count)+1 {
		step = 2
	}
	return FieldCursor{
		src:   v.src,
		first: first,
		pos:   first,
		step:  step,
		count: uint32(count),
	}
}

// ValueFieldCursor is the Value-level counterpart of FieldCursor: it resolves
// the same forward-resuming lookup but yields Values that keep the document's
// storage alive through their root, so results survive after the caller drops
// the original source slice. See FieldCursor for the first-match-in-scan-order
// contract, which differs from Get's last-occurrence-wins rule. Find mutates
// the cursor position, so one cursor is single-consumer and must not be used
// concurrently; independent copies have independent positions.
type ValueFieldCursor struct {
	cursor FieldCursor
	root   *valueRoot
}

// Fields returns a forward-resuming field cursor over v's object members,
// yielding Values that share v's root. See FieldCursor for the
// first-match-in-scan-order contract, which differs from Get's
// last-occurrence-wins rule. A cursor over a non-object resolves nothing.
func (v Value) Fields() ValueFieldCursor {
	return ValueFieldCursor{cursor: v.node.Fields(), root: v.root}
}

// Find returns the first member matching key in forward scan order from the
// cursor's current position, wrapping around the object's end exactly once and
// stopping where the scan began, exactly as FieldCursor.Find but yielding a
// Value bound to the source cursor's root. On a hit the cursor advances past the
// matched member; on a miss it resets to the object's first member. This reports
// the FIRST match reachable, not the last occurrence: for encoding/json
// duplicate-key semantics use Value.Get. Find never allocates.
func (c *ValueFieldCursor) Find(key string) (Value, bool) {
	node, ok := c.cursor.Find(key)
	if !ok {
		return Value{}, false
	}
	return Value{node: node, root: c.root}, true
}

// nextKeyEntry returns the key entry one member past keyEntry, using the fixed
// stride for a flat object and chasing the value span otherwise. It never reads
// past the object because callers bound their steps by count.
func (c *FieldCursor) nextKeyEntry(keyEntry *IndexEntry) *IndexEntry {
	if c.step != 0 {
		return tapeEntryOffset(keyEntry, uintptr(c.step))
	}
	valueEntry := tapeEntryOffset(keyEntry, 1)
	return tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
}

// findEntry runs the resumable scan and returns the matching value entry, or
// nil if key is absent. On a hit it advances the cursor to the member after the
// match; on a miss it resets the cursor to the object's start so the next Find
// begins a fresh forward pass. The scan visits each member at most once: it
// starts at pos and wraps once through first, stopping when it returns to pos.
func (c *FieldCursor) findEntry(key string) *IndexEntry {
	if c.first == nil {
		return nil
	}
	keyEntry := c.pos
	index := c.index
	for scanned := uint32(0); scanned < c.count; scanned++ {
		if tapeKeyEqual(tapeSourceBytes(c.src, keyEntry.start, keyEntry.end), keyEntry.flags(), key) {
			// Advance past the match so the next Find resumes here. A match on
			// the object's last member leaves the cursor wrapped to the start.
			valueEntry := tapeEntryOffset(keyEntry, 1)
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
			// Wrap to the object's first member and keep scanning until the
			// pass returns to where it began.
			keyEntry = c.first
			index = 0
		} else {
			keyEntry = c.nextKeyEntry(keyEntry)
		}
	}
	// Not found: leave the cursor at a well-defined origin so the next lookup
	// makes a full forward pass rather than resuming mid-object.
	c.pos = c.first
	c.index = 0
	return nil
}

// Find returns the first member matching key in forward scan order from the
// cursor's current position, wrapping around the object's end exactly once and
// stopping where the scan began. On a hit the cursor advances past the matched
// member so the next Find resumes there; on a miss the cursor resets to the
// object's first member. This reports the FIRST match reachable, not the last
// occurrence: for encoding/json duplicate-key semantics use Node.Get. Keys
// compare without unescaping where safe, so escaped keys still match. Find never
// allocates.
func (c *FieldCursor) Find(key string) (Node, bool) {
	entry := c.findEntry(key)
	if entry == nil {
		return Node{}, false
	}
	return Node{src: c.src, entry: entry}, true
}
