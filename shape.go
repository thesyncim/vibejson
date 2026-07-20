package simdjson

import (
	"github.com/thesyncim/simdjson/document"
	"github.com/thesyncim/simdjson/internal/byteview"
)

// Shape-compiled field access.
//
// Real document corpora are shape-homogeneous: millions of documents share one
// object layout. A flat object's layout — every value exactly one entry — is
// fully determined by its ordered key sequence, so once that sequence is seen
// its field positions are known for every later object with the same sequence:
// member m's key sits at entry offset 2m+1 from the header and its value at
// 2m+2, the fixed stride Node.Get's flat scan walks. A ShapeCache fingerprints
// the key sequence, compiles "field name -> member ordinal" once per distinct
// sequence, and every subsequent same-shape object resolves any field through
// one fold over words already in cache plus one fixed-offset read — no key
// scans, no probing, no per-document table builds.
//
// The intended engine loop over a homogeneous batch:
//
//	shape, ok := cache.Resolve(doc)      // fingerprint fold, amortized O(members)
//	ref, ok := shape.Field("user_id")    // once per shape, cached by the engine
//	v, ok := ref.In(doc)                 // O(1): one offset read plus one key verify
//
// Compare ObjectProbe, which answers many different keys against one object:
// a shape answers the same few keys against many same-layout objects.

// shapeMaxFields caps compiled shape width. The fingerprint fold is O(width)
// per document, so extremely wide objects would pay more resolving than a
// probe build costs; they also are not the homogeneous-corpus workload shapes
// serve. Wider objects simply report false from Resolve and callers fall back
// to Get or a probe.
const shapeMaxFields = 4096

// shapeSeed opens the fingerprint fold. It differs from keyHashSeed so a
// shape fingerprint and a key hash never inhabit one identifier space, and it
// covers the flat-layout discriminant: this fingerprint family is defined
// only over flat objects, which Resolve gates structurally before folding.
const (
	shapeSeed = 0xA0761D6478BD642F
	shapeMul2 = 0xC4CEB9FE1A85EC53
)

// shapeFinish avalanches the fold state into a table-ready fingerprint,
// the standard 64-bit finalizer (xor-shift, multiply) over both hash words.
func shapeFinish(h uint64) uint64 {
	h ^= h >> 33
	h *= keyHashMul
	h ^= h >> 29
	h *= shapeMul2
	h ^= h >> 32
	return h
}

// shapeField is one compiled member: the key's raw spelling (content between
// the quotes, escapes included) and its decoded name, both arena-backed. For
// an unescaped key the two alias the same bytes. The value's entry offset is
// implicit: member m's value is at 2m+2 from the object header.
type shapeField struct {
	raw     string // raw content; FieldRef.In verifies documents against this
	decoded string // decoded name; Shape.Field matches against this
}

// shapeRecord is one compiled shape. Records are immutable after compile and
// live in arena chunks that never move, so a Shape's identity (its record
// pointer) is stable for the cache's lifetime and two Shapes are equal
// exactly when they came from the same compiled sequence.
type shapeRecord struct {
	fingerprint uint64
	// info and next are the expected object-header words, precomputed so
	// FieldRef.In verifies kind, member count, and flat layout in two
	// compares: info is the header's packed word with flags masked off
	// (enrichment marks headers there), next is the flat span 2*count+1.
	info   uint32
	next   uint32
	fields []shapeField
	// table maps decoded field name to member ordinal: open addressing over
	// ordinal+1 with zero marking empty, at most half full. Among members
	// sharing one decoded name the table holds the last ordinal, so Field
	// obeys Get's last-duplicate rule. Nil for the empty shape.
	table []uint32
	mask  uint32
}

// A ShapeCache compiles and caches object shapes for [ShapeCache.Resolve]. It
// owns its storage: key spellings are copied into append-only arena chunks
// that never move, so compiled shapes stay valid as the cache grows and
// outlive the documents they were compiled from. Compilation is gated behind
// a repeat sighting, so a layout that never recurs costs the cache eight
// bytes, not a compiled shape; the cache never evicts and callers bound its
// memory by its lifetime. The zero ShapeCache is empty and ready to use. A
// ShapeCache is not safe for concurrent use; engines shard one per worker.
type ShapeCache struct {
	// table is the open-addressed fingerprint index: zero marks an empty
	// slot, a value with shapePendingBit clear holds a compiled shape id+1,
	// and one with the bit set holds a pending fingerprint id+1 — a layout
	// sighted once and not yet worth compiling. Compiling a pending layout
	// rewrites its slot in place, so occupancy never changes on promotion
	// and pending entries whose slots were rewritten are dropped by the
	// next growth pass.
	table   []uint32
	shapes  []*shapeRecord // compiled id -> shape record
	pending []uint64       // pending id -> fingerprint awaiting a second sighting
	used    int            // occupied table slots, for the growth threshold
	chunk   []byte         // current arena chunk, appended to only within capacity
	scratch []byte         // decoded spelling of an escaped key, reused per compile
	// Compiled-shape storage is arena-chunked like the key arena: records,
	// field slices, and name-table slices are carved from chunks that are
	// never moved or extended beyond capacity, so a compile costs amortized
	// chunk allocations rather than three of its own and one cache's shapes
	// pack densely for the read path.
	recChunk   []shapeRecord
	fieldChunk []shapeField
	slotChunk  []uint32
}

// The arena grows geometrically between the interner's chunk bounds; the
// table starts small and doubles at three-quarters load. The record, field,
// and slot chunks are fixed-size, with oversized requests getting a chunk of
// their own.
const (
	shapeMinTable   = 16
	shapePendingBit = 1 << 31
	shapeRecChunk   = 64
	shapeFieldChunk = 512
	shapeSlotChunk  = 1024
)

// A Shape is a compiled flat-object layout borrowed from a [ShapeCache]. It
// remains valid for the cache's lifetime. Shapes are comparable: two are
// equal exactly when they refer to the same compiled key sequence. The zero
// Shape has no fields. A Shape is immutable and safe for concurrent use once
// the cache stops resolving.
type Shape struct {
	rec *shapeRecord
}

// A FieldRef is one field of a [Shape], resolved once with [Shape.Field] and
// then applied to any number of same-shape documents with [FieldRef.In]. It
// borrows the shape's cache and follows its lifetime. The zero FieldRef
// resolves nothing. A FieldRef is immutable and safe to share across
// goroutines under the cache's concurrency rule.
type FieldRef struct {
	rec *shapeRecord
	ord uint32
}

// Resolve returns the compiled shape of v's object layout. It reports false
// for a non-object, an object with non-flat layout (any member value with
// children, whose span breaks the fixed member offsets; empty containers are
// single entries and stay flat), or an object wider than the shape limit;
// those take nothing from the cache and callers fall back to Get or a
// probe. Resolve also reports
// false on the first sighting of a new layout: compilation costs a full key
// readback plus arena copies, so it is gated behind a repeat sighting and
// only recurring layouts — the workload shapes serve — ever pay it. The
// second sighting compiles, and every later one costs one fingerprint fold
// over the key entries plus a table probe. A heterogeneous corpus where no
// layout recurs therefore stays near the fold cost per document, below one
// ObjectProbe build. Objects with per-key hash enrichment
// (document.IndexOptions.HashKeys) fold stored hashes straight off the
// tape; unenriched objects hash each key's content inline, which resolves
// identically but roughly four times slower per member.
//
// Fingerprint trust: the fingerprint is a 64-bit order-sensitive fold over
// the member count and each key's (32-bit content hash, raw length) pair.
// Distinct sequences of equal count collide only if every differing position
// collides on that pair — one chance in 2^32 per differing key pair for
// non-adversarial data, and 2^-64 for the outer fold — so with S distinct
// layouts in a workload the expected number of colliding pairs is bounded
// by S^2/2^33; at ten thousand layouts that is under 10^-5. The fingerprint
// alone is therefore trusted to route documents to shapes, but it is not
// trusted for reads: [FieldRef.In] verifies the document's key bytes at the
// target ordinal before returning a value, so even an engineered collision
// (the key hash is not cryptographic) can never cause a wrong-field read —
// a colliding document fails In and the caller's Get fallback answers
// correctly. See In for the one residual, duplicate-key deviation this
// leaves.
func (c *ShapeCache) Resolve(v Node) (Shape, bool) {
	if !v.valid() {
		return Shape{}, false
	}
	e := v.entry
	if e.Kind() != document.Object {
		return Shape{}, false
	}
	count := e.Count()
	if count > shapeMaxFields || e.next != 2*count+1 {
		// A non-flat object has no fixed member offsets; a wider one is not
		// worth folding per document. Both gates also keep every entry offset
		// used below inside the object's span: a flat object spans exactly
		// 2*count+1 entries, and the compile and fold touch offsets at most
		// 2*(count-1)+2 = 2*count.
		return Shape{}, false
	}
	fp := c.fingerprint(v, int(count))
	if len(c.table) == 0 {
		c.grow()
	}
	mask := uint32(len(c.table) - 1)
	for slot := uint32(fp) & mask; ; slot = (slot + 1) & mask {
		stored := c.table[slot]
		if stored == 0 {
			// First sighting: remember the fingerprint and decline.
			c.insertPending(fp)
			return Shape{}, false
		}
		if stored&shapePendingBit == 0 {
			if rec := c.shapes[stored-1]; rec.fingerprint == fp {
				return Shape{rec: rec}, true
			}
			continue
		}
		if c.pending[stored&^uint32(shapePendingBit)-1] == fp {
			// Second sighting: compile and promote the slot in place.
			rec := c.compile(v, int(count), fp)
			c.table[slot] = uint32(len(c.shapes))
			return Shape{rec: rec}, true
		}
	}
}

// fingerprint folds v's ordered key sequence: the member count seeds the
// state and each key contributes its content hash and raw span length. Value
// bytes never enter the fold, so documents that differ only in values — and
// therefore in key positions, but not key spellings — fingerprint alike.
// On an enriched object each key's hash is already in its entry's next word
// and the fold is pure tape reads; otherwise the content is hashed inline
// with the same function enrichment uses, so enriched and unenriched
// documents of one layout resolve to one shape.
//
// The fold runs as two interleaved lanes over even and odd members: a single
// multiply chain is latency-bound, and the lanes' independent multiplies
// overlap to nearly halve the per-member cost. Order sensitivity survives
// the split — a moved key changes its lane's input sequence — and the final
// combine keeps either lane's difference alive because the odd-constant
// multiply is a bijection. Both hash sources use the identical formula, so
// one layout fingerprints alike however it was built.
func (c *ShapeCache) fingerprint(v Node, count int) uint64 {
	h1 := shapeSeed ^ uint64(count)*keyHashMul
	h2 := shapeMul2 ^ uint64(count)*keyHashSeed
	m := 0
	if v.entry.keysHashed() {
		for ; m+1 < count; m += 2 {
			ke1 := tapeEntryOffset(v.entry, uintptr(2*m)+1)
			ke2 := tapeEntryOffset(v.entry, uintptr(2*m)+3)
			h1 = (h1 ^ (uint64(ke1.next) | uint64(ke1.end-ke1.start)<<32)) * keyHashMul
			h2 = (h2 ^ (uint64(ke2.next) | uint64(ke2.end-ke2.start)<<32)) * keyHashMul
		}
		if m < count {
			ke := tapeEntryOffset(v.entry, uintptr(2*m)+1)
			h1 = (h1 ^ (uint64(ke.next) | uint64(ke.end-ke.start)<<32)) * keyHashMul
		}
		return shapeFinish(h1 ^ h2*shapeMul2)
	}
	for ; m+1 < count; m += 2 {
		ke1 := tapeEntryOffset(v.entry, uintptr(2*m)+1)
		ke2 := tapeEntryOffset(v.entry, uintptr(2*m)+3)
		hash1 := hashKeyContent(byteview.SliceRange(v.src, ke1.start+1, ke1.end-1))
		hash2 := hashKeyContent(byteview.SliceRange(v.src, ke2.start+1, ke2.end-1))
		h1 = (h1 ^ (uint64(hash1) | uint64(ke1.end-ke1.start)<<32)) * keyHashMul
		h2 = (h2 ^ (uint64(hash2) | uint64(ke2.end-ke2.start)<<32)) * keyHashMul
	}
	if m < count {
		ke := tapeEntryOffset(v.entry, uintptr(2*m)+1)
		hash := hashKeyContent(byteview.SliceRange(v.src, ke.start+1, ke.end-1))
		h1 = (h1 ^ (uint64(hash) | uint64(ke.end-ke.start)<<32)) * keyHashMul
	}
	return shapeFinish(h1 ^ h2*shapeMul2)
}

// compile reads every key of a recurring sequence back from the tape, copies
// raw and decoded spellings into the arena, builds the name table with the
// last duplicate winning, and appends the record to the compiled list; the
// caller owns the fingerprint slot it promotes. This full readback is the
// promotion cost ceiling: one pass over the keys, comparable to one
// ObjectProbe build, paid once per recurring layout.
func (c *ShapeCache) compile(v Node, count int, fp uint64) *shapeRecord {
	if len(c.recChunk) == cap(c.recChunk) {
		c.recChunk = make([]shapeRecord, 0, shapeRecChunk)
	}
	c.recChunk = c.recChunk[:len(c.recChunk)+1]
	rec := &c.recChunk[len(c.recChunk)-1]
	*rec = shapeRecord{
		fingerprint: fp,
		info:        packInfo(uint32(count), document.Object, 0),
		next:        2*uint32(count) + 1,
	}
	if count > 0 {
		capacity := probeCapacity(count)
		rec.fields = c.allocFields(count)
		rec.table = c.allocSlots(capacity)
		rec.mask = uint32(capacity - 1)
		hashed := v.entry.keysHashed()
		for m := 0; m < count; m++ {
			ke := tapeEntryOffset(v.entry, uintptr(2*m)+1)
			content := byteview.SliceRange(v.src, ke.start+1, ke.end-1)
			raw := c.internBytes(content)
			decoded := raw
			var hash uint32
			switch {
			case ke.flags()&tapeFlagEscaped != 0:
				c.scratch = appendDecodedJSONString(c.scratch[:0], content)
				decoded = c.internBytes(c.scratch)
				hash = hashKeyString(decoded)
			case hashed:
				// An unescaped key's decoded name is its content, whose hash
				// enrichment already stored in the entry's next word.
				hash = ke.next
			default:
				hash = hashKeyString(decoded)
			}
			rec.fields[m] = shapeField{raw: raw, decoded: decoded}
			// Claim the first free slot in the name's chain, or overwrite the
			// chain's equal earlier duplicate so the later ordinal wins, the
			// Node.Get duplicate rule. The table is at most half full, so a
			// free slot always exists and the loop terminates.
			for slot := hash & rec.mask; ; slot = (slot + 1) & rec.mask {
				stored := rec.table[slot]
				if stored == 0 || rec.fields[stored-1].decoded == decoded {
					rec.table[slot] = uint32(m) + 1
					break
				}
			}
		}
	}
	c.shapes = append(c.shapes, rec)
	return rec
}

// allocFields carves n zeroed contiguous fields from the field arena. Chunk
// elements are claimed exactly once and chunks are never extended beyond
// their capacity, so earlier shapes' field slices stay put and stay theirs.
func (c *ShapeCache) allocFields(n int) []shapeField {
	if len(c.fieldChunk)+n > cap(c.fieldChunk) {
		size := shapeFieldChunk
		if size < n {
			size = n
		}
		c.fieldChunk = make([]shapeField, 0, size)
	}
	start := len(c.fieldChunk)
	c.fieldChunk = c.fieldChunk[:start+n]
	return c.fieldChunk[start : start+n : start+n]
}

// allocSlots carves n zeroed contiguous table slots from the slot arena,
// under the field arena's claiming rules.
func (c *ShapeCache) allocSlots(n int) []uint32 {
	if len(c.slotChunk)+n > cap(c.slotChunk) {
		size := shapeSlotChunk
		if size < n {
			size = n
		}
		c.slotChunk = make([]uint32, 0, size)
	}
	start := len(c.slotChunk)
	c.slotChunk = c.slotChunk[:start+n]
	return c.slotChunk[start : start+n : start+n]
}

// internBytes copies b into the arena and returns a string view of the copy.
// Chunks are never extended beyond their capacity, so earlier views stay put.
func (c *ShapeCache) internBytes(b []byte) string {
	if len(c.chunk)+len(b) > cap(c.chunk) {
		size := 2 * cap(c.chunk)
		if size < internMinChunk {
			size = internMinChunk
		}
		if size > internMaxChunk {
			size = internMaxChunk
		}
		if size < len(b) {
			size = len(b)
		}
		c.chunk = make([]byte, 0, size)
	}
	start := len(c.chunk)
	c.chunk = append(c.chunk, b...)
	return byteview.String(c.chunk[start:len(c.chunk):len(c.chunk)])
}

// insertPending records a first-sighted fingerprint in the table, growing it
// first when the insertion would cross three-quarters load.
func (c *ShapeCache) insertPending(fp uint64) {
	if len(c.pending) >= shapePendingBit-1 {
		// Table slots hold id+1 beside the pending bit; the eight bytes per
		// entry make this unreachable in practice, so the guard only pins
		// the invariant.
		panic("simdjson: ShapeCache exceeds 31-bit identifiers")
	}
	if (c.used+1)*4 >= len(c.table)*3 {
		c.grow()
	}
	c.pending = append(c.pending, fp)
	mask := uint32(len(c.table) - 1)
	slot := uint32(fp) & mask
	for c.table[slot] != 0 {
		slot = (slot + 1) & mask
	}
	c.table[slot] = uint32(len(c.pending)) | shapePendingBit
	c.used++
}

// grow doubles the fingerprint table and reinserts every occupied slot,
// compiled and pending alike. Only slots move; records, pending
// fingerprints, and arena bytes are untouched, and pending entries whose
// slots were promoted are no longer referenced and simply do not carry over.
func (c *ShapeCache) grow() {
	size := 2 * len(c.table)
	if size < shapeMinTable {
		size = shapeMinTable
	}
	table := make([]uint32, size)
	mask := uint32(size - 1)
	for _, stored := range c.table {
		if stored == 0 {
			continue
		}
		var fp uint64
		if stored&shapePendingBit != 0 {
			fp = c.pending[stored&^uint32(shapePendingBit)-1]
		} else {
			fp = c.shapes[stored-1].fingerprint
		}
		slot := uint32(fp) & mask
		for table[slot] != 0 {
			slot = (slot + 1) & mask
		}
		table[slot] = stored
	}
	c.table = table
}

// Len returns the number of fields in the shape, duplicates included.
func (s Shape) Len() int {
	if s.rec == nil {
		return 0
	}
	return len(s.rec.fields)
}

// Field resolves a field name to its position in the shape. Names match by
// decoded spelling, exactly as Node.Get matches queries, and among duplicate
// members the last wins. An absent name or the zero Shape reports false.
// Field costs one small-table probe; engines call it once per shape and
// cache the FieldRef.
func (s Shape) Field(name string) (FieldRef, bool) {
	rec := s.rec
	if rec == nil || rec.table == nil {
		return FieldRef{}, false
	}
	for slot := hashKeyString(name) & rec.mask; ; slot = (slot + 1) & rec.mask {
		stored := rec.table[slot]
		if stored == 0 {
			return FieldRef{}, false
		}
		if rec.fields[stored-1].decoded == name {
			return FieldRef{rec: rec, ord: stored - 1}, true
		}
	}
}

// In returns the field's value in document v, in constant time: two header
// compares, one key verification, one fixed-offset read. It reports false —
// never a wrong value — unless v is a flat object of the shape's exact width
// whose key at this field's position byte-matches the compiled raw spelling.
// The verification makes In self-checking: whatever document is passed, a
// returned value is always the one keyed by this field's spelling at its
// compiled position, so a fingerprint collision or a document that never
// went through Resolve yields false, and the caller falls back to Node.Get.
//
// Contract: apply In to documents the owning cache Resolved to this same
// Shape. That routing plus the verification reduces the residual error
// surface to one construction: a document whose key sequence was engineered
// to collide with this shape's fingerprint, which byte-matches this field's
// spelling at the compiled ordinal, and which repeats that spelling at a
// later member — there In returns the compiled ordinal's value while Get
// would return the later duplicate's. A document without duplicate spellings
// of the queried field can never mis-resolve.
func (r FieldRef) In(v Node) (Node, bool) {
	rec := r.rec
	if rec == nil || !v.valid() {
		return Node{}, false
	}
	e := v.entry
	// Kind and width in one masked compare (flags carry only the enrichment
	// marker on object headers), flat layout in one more; together they bound
	// every entry offset below inside the object's 2*count+1 entry span.
	if e.info&(infoCountMask|infoKindMask) != rec.info || e.next != rec.next {
		return Node{}, false
	}
	ke := tapeEntryOffset(e, uintptr(2*r.ord)+1)
	if !bytesEqualString(byteview.SliceRange(v.src, ke.start+1, ke.end-1), rec.fields[r.ord].raw) {
		return Node{}, false
	}
	return Node{src: v.src, entry: tapeEntryOffset(ke, 1)}, true
}
