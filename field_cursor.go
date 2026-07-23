package slopjson

import "github.com/thesyncim/slopjson/internal/byteview"

// FieldCursor is a stateful, forward-resuming lookup over one object's members,
// obtained from [Node.Fields]. It is useful when several known fields are read
// in roughly document order.
//
// [FieldCursor.Find] returns the first matching member at or after the current
// position, wrapping once. A hit advances past that member; a miss resets to the
// first member. This differs from [Node.Get], whose duplicate-key rule is the
// last member in document order.
//
// On the lookup ladder (see the essay at [Node.Get]) a cursor sits between
// repeated Get calls and a built [ObjectProbe]: it exploits document-order
// access to skip already-consumed members without paying any build pass, and
// it applies the same ladder gates — hash gate on an enriched object, length
// gate otherwise — member by member as it scans.
//
// A FieldCursor borrows the Node's source and index. The zero cursor and cursors
// from non-objects or empty objects resolve nothing. Find mutates the position,
// so one cursor is single-consumer and must not be used concurrently;
// independent copies have independent positions. Find does not allocate.
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
	// hashed records once, at construction, whether the object was enriched
	// with per-key hashes (see enrichKeyHashes) so the scan loop consults the
	// pre-filter with a single bool test instead of decoding the header again.
	hashed bool
}

// Fields returns a FieldCursor over v's object members. A non-object or empty
// object returns a cursor that resolves nothing.
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
		src:    v.src,
		first:  first,
		pos:    first,
		step:   step,
		count:  uint32(count),
		hashed: v.entry.keysHashed(),
	}
}

// ValueFieldCursor is the Value-level counterpart of [FieldCursor]. It has the
// same lookup and single-consumer semantics but yields Values sharing the
// originating document's lifetime. Independent copies have independent
// positions.
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

// findEntryQuery runs the resumable scan and returns the matching value
// entry, or nil if key is absent. On a hit it advances the cursor to the
// member after the match; on a miss it resets the cursor to the object's
// start so the next lookup begins a fresh forward pass. The scan visits each
// member at most once: it starts at pos and wraps once through first,
// stopping when it returns to pos.
func (c *FieldCursor) findEntryQuery(key string, queryHash uint32) *IndexEntry {
	if c.first == nil {
		return nil
	}
	// On an enriched object each unescaped member whose stored hash differs
	// from queryHash is rejected before the byte comparison; an unenriched
	// cursor instead rejects each unescaped member whose raw span is not
	// len(key) plus two quotes. Escaped keys always byte-compare — their
	// decoded length differs from the raw span — and neither gate changes
	// which member matches first; they only skip work.
	rawLen := uint32(len(key)) + 2
	keyEntry := c.pos
	index := c.index
	for scanned := uint32(0); scanned < c.count; scanned++ {
		flags := keyEntry.flags()
		candidate := flags&tapeFlagEscaped != 0
		if !candidate {
			if c.hashed {
				candidate = keyEntry.next == queryHash
			} else {
				candidate = keyEntry.end-keyEntry.start == rawLen
			}
		}
		if candidate &&
			tapeKeyEqual(byteview.SliceRange(c.src, keyEntry.start, keyEntry.end), flags, key) {
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

// Find returns the first member matching key from the cursor's current position,
// wrapping once. A hit advances past the member; a miss resets to the first
// member and returns a zero Node and false. Escaped keys match their decoded
// spelling. See [FieldCursor] for duplicate-key and concurrency semantics.
func (c *FieldCursor) Find(key string) (Node, bool) {
	// An enriched cursor hashes the query once here; compiled lookups reuse a
	// hash computed at compile time instead.
	var queryHash uint32
	if c.hashed {
		queryHash = hashKeyString(key)
	}
	entry := c.findEntryQuery(key, queryHash)
	if entry == nil {
		return Node{}, false
	}
	return Node{src: c.src, entry: entry}, true
}

// FindCompiled is [FieldCursor.Find] with a precompiled key. On an object
// enriched with per-key hashes (document.IndexOptions.HashKeys) it skips
// rehashing the query, which pays off when the same key is resolved across
// many documents. See [CompileKey].
func (c *FieldCursor) FindCompiled(k CompiledKey) (Node, bool) {
	entry := c.findEntryQuery(k.key, k.hash)
	if entry == nil {
		return Node{}, false
	}
	return Node{src: c.src, entry: entry}, true
}
