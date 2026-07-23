package simdjson

import (
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
	"github.com/thesyncim/simdjson/internal/byteview"
)

// Node is a lightweight value handle obtained from an Index or Value. Node
// accessors read directly from indexed source and do not allocate unless they
// must unescape or materialize data. An Index-derived Node borrows its source
// and entry storage. A Value-derived Node keeps the Value's owned backing
// arrays alive independently of the originating Value. Concurrent reads are
// safe while the borrowed source and index remain alive and unmodified; callers
// must synchronize any mutation of Index-backed storage themselves. The zero
// Node has kind Invalid. Accessors returning a boolean report false for an
// invalid Node, a wrong JSON kind, an absent child, or an out-of-range number.
type Node struct {
	src   *byte
	entry *IndexEntry
}

// nodeFromStorage constructs a root cursor only when both backing stores are
// present. The typed interior pointers keep the arrays visible to the garbage
// collector even after the originating slices go out of scope.
func nodeFromStorage(src []byte, entries []IndexEntry) Node {
	if len(src) == 0 || len(entries) == 0 {
		return Node{}
	}
	return Node{src: &src[0], entry: &entries[0]}
}

func (v Node) valid() bool {
	return v.entry != nil
}

// Kind returns the JSON kind of v.
func (v Node) Kind() document.Kind {
	if !v.valid() {
		return document.Invalid
	}
	return v.entry.Kind()
}

// Raw returns v's exact source range as a value borrowing the same document.
// An invalid Node returns a zero RawValue.
func (v Node) Raw() RawValue {
	if !v.valid() {
		return RawValue{}
	}
	e := v.entry
	return RawValue{src: byteview.SliceRange(v.src, e.start, e.end)}
}

// IsNull reports whether v is null.
func (v Node) IsNull() bool {
	return v.Kind() == document.Null
}

// Bool returns v as a boolean.
func (v Node) Bool() (bool, bool) {
	if v.Kind() != document.Bool {
		return false, false
	}
	return byteview.ByteAt(v.src, uintptr(v.entry.start)) == 't', true
}

// NumberBytes returns the original number spelling without revalidating it.
func (v Node) NumberBytes() ([]byte, bool) {
	if v.Kind() != document.Number {
		return nil, false
	}
	e := v.entry
	return byteview.SliceRange(v.src, e.start, e.end), true
}

// NumberText returns an allocation-free string alias of the source number.
func (v Node) NumberText() (string, bool) {
	b, ok := v.NumberBytes()
	if !ok {
		return "", false
	}
	return ownedBytesString(b), true
}

// IsInteger reports whether v is a number with an integer spelling: an
// optional minus sign followed by digits, with no fraction or exponent.
// It does not imply that the value fits in a particular integer type.
func (v Node) IsInteger() bool {
	// tapeFlagInt is exclusive to Number entries; strings use only the
	// escaped/key bits and every other kind has zero flags. Testing the flag
	// therefore preserves the kind check while avoiding a second packed-kind
	// decode after callers have already dispatched on Kind.
	return v.valid() && v.entry.flags()&tapeFlagInt != 0
}

// Int64 parses an integer value.
func (v Node) Int64() (int64, bool) {
	if v.Kind() != document.Number {
		return 0, false
	}
	e := v.entry
	if e.flags()&tapeFlagInt != 0 {
		return tapeInt64(v.src, e.start, e.end)
	}
	// A number without the integer flag carries a fraction or exponent, and
	// JSON forbids the leading-plus and leading-zero forms that would let
	// strconv.ParseInt accept a non-integer spelling, so the verdict is always
	// rejection — reached here without an allocating parse, exactly as
	// [Node.Uint64] already short-circuits its non-integer inputs.
	return 0, false
}

// Uint64 parses an unsigned integer value. Fractional, exponent, negative,
// and out-of-range spellings report false.
func (v Node) Uint64() (uint64, bool) {
	if v.Kind() != document.Number {
		return 0, false
	}
	e := v.entry
	base := tapeSourceBase(v.src)
	if e.flags()&tapeFlagInt == 0 || byteview.ByteAt(v.src, uintptr(e.start)) == '-' {
		return 0, false
	}
	return tapeUint64(base, int(e.start), int(e.end))
}

// tapeUint64 parses a validated, non-negative integer in [start, end).
func tapeUint64(base unsafe.Pointer, start, end int) (uint64, bool) {
	if value, ok := parseTapeDigitsUint64(base, start, end); ok {
		return value, true
	}
	// parseTapeDigitsUint64 deliberately stops at nineteen digits because
	// that is enough for signed reads. Uint64 has one additional valid digit;
	// accumulate that rare width with an explicit overflow guard.
	if end-start != 20 {
		return 0, false
	}
	value := uint64(0)
	for i := start; i < end; i++ {
		digit := uint64(fastByteAt(base, i) - '0')
		if value > (^uint64(0)-digit)/10 {
			return 0, false
		}
		value = value*10 + digit
	}
	return value, true
}

// tapeInt64 parses a number the tape classified as a plain integer: an
// optional minus sign, then digits. Values outside int64 report false, the
// same verdict strconv.ParseInt reaches on them.
func tapeInt64(src *byte, start, end uint32) (int64, bool) {
	base := tapeSourceBase(src)
	i := int(start)
	negative := fastByteAt(base, i) == '-'
	if negative {
		i++
	}
	value, ok := parseTapeDigitsUint64(base, i, int(end))
	if !ok {
		return 0, false
	}
	if negative {
		if value > 1<<63 {
			return 0, false
		}
		return -int64(value), true
	}
	if value > 1<<63-1 {
		return 0, false
	}
	return int64(value), true
}

// Float64 parses a number value as float64.
func (v Node) Float64() (float64, bool) {
	if v.Kind() != document.Number {
		return 0, false
	}
	e := v.entry
	if e.flags()&tapeFlagInt != 0 {
		// A plain integer needs no fraction or exponent handling: parse the
		// digits and let the conversion round once, exactly as ParseFloat
		// rounds decimal input. Twenty digits or more fall through.
		base := tapeSourceBase(v.src)
		i := int(e.start)
		negative := fastByteAt(base, i) == '-'
		if negative {
			i++
		}
		if value, ok := parseTapeDigitsUint64(base, i, int(e.end)); ok {
			f := float64(value)
			if negative {
				f = -f
			}
			return f, true
		}
	}
	// A real float — fraction, exponent, or an integer too wide for the fast
	// path — rounds through the same kernels the streaming decoder uses,
	// reaching strconv only for the spellings they defer on. ok is false only
	// on an out-of-range magnitude, exactly as strconv.ParseFloat reports.
	return tapeFloat64(tapeSourceBase(v.src), int(e.start), int(e.end))
}

// StringBytes returns an unescaped string as a source alias. Escaped strings
// return false; use AppendText for those.
func (v Node) StringBytes() ([]byte, bool) {
	if v.Kind() != document.String {
		return nil, false
	}
	e := v.entry
	if e.flags()&tapeFlagEscaped != 0 {
		return nil, false
	}
	return byteview.SliceRange(v.src, e.start+1, e.end-1), true
}

// AppendText appends v's decoded string to dst. The returned caller-owned slice
// may reuse dst's backing storage. For a non-string it returns dst unchanged and
// false.
func (v Node) AppendText(dst []byte) ([]byte, bool) {
	if v.Kind() != document.String {
		return dst, false
	}
	e := v.entry
	raw := byteview.SliceRange(v.src, e.start+1, e.end-1)
	if e.flags()&tapeFlagEscaped == 0 {
		return append(dst, raw...), true
	}
	return appendDecodedJSONString(dst, raw), true
}

// ArrayLen returns the number of array elements.
func (v Node) ArrayLen() (int, bool) {
	if v.Kind() != document.Array {
		return 0, false
	}
	return int(v.entry.Count()), true
}

// ObjectLen returns the number of object members.
func (v Node) ObjectLen() (int, bool) {
	if v.Kind() != document.Object {
		return 0, false
	}
	return int(v.entry.Count()), true
}

// Index returns the ith array element. A wrong kind or out-of-range index
// returns a zero Node and false.
func (v Node) Index(index int) (Node, bool) {
	count, ok := v.ArrayLen()
	if !ok || index < 0 || index >= count {
		return Node{}, false
	}
	if v.entry.next == uint32(count)+1 {
		// Flat array: every element is one entry, so the ith sits at a
		// fixed offset from the header.
		return Node{src: v.src, entry: tapeEntryOffset(v.entry, uintptr(index)+1)}, true
	}
	entry := tapeEntryOffset(v.entry, 1)
	for range index {
		entry = tapeEntryOffset(entry, uintptr(entry.next))
	}
	return Node{src: v.src, entry: entry}, true
}

// The lookup ladder.
//
// Get and every accelerated spelling of it resolve one contract: the value
// of the last member in document order whose key decodes to the query
// (matching GetRaw and encoding/json), with escaped keys matching their
// decoded spelling. The implementations differ only in how cheaply they
// reject non-matching members, and every gate is a pure pre-filter: a gated
// candidate is always byte-verified, so a collision costs a comparison but
// never misleads, and escaped keys bypass the gates entirely because their
// stored words describe the raw spelling (see index_keyhash.go).
//
//	object state         rung                           reject cost per member
//	unenriched           length gate (getPlain)         one span-length compare
//	enriched (HashKeys)  hash gate (getHashedQuery)     one word compare
//	enriched and flat    tape scan (index_tapescan.go)  1/4 branch, four-wide
//
// The two scalar rungs each come in a flat variant (fixed two-entry member
// stride, detected by header.next == 2*count+1) and a span-chasing variant
// for objects whose member values have children. Around the ladder sit the
// specialized structures: a FieldCursor (field_cursor.go) resumes forward
// scans when several fields are read in document order, an ObjectProbe
// (index_probe.go) answers many distinct keys against one wide object in
// constant time, and a ShapeCache (shape.go) compiles recurring object
// layouts so a field becomes one verified fixed-offset read.

// Get returns the last object member with key. A wrong kind or absent key
// returns a zero Node and false.
func (v Node) Get(key string) (Node, bool) {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		// The empty check also keeps the entry arithmetic of the scans inside
		// the tape: an empty object can be its final entry.
		return Node{}, false
	}
	if v.entry.keysHashed() {
		// An enriched object carries a per-key hash in each key entry's next
		// word; the pre-filter skips the byte comparison on a hash miss.
		return v.getHashedQuery(key, hashKeyString(key), count)
	}
	return v.getPlain(key, count)
}

// GetCompiled is [Node.Get] with a precompiled key. On an object enriched with
// per-key hashes (document.IndexOptions.HashKeys) it skips rehashing the
// query, which pays off when the same key is resolved across many documents;
// on any other object it takes Get's path unchanged. See [CompileKey].
func (v Node) GetCompiled(k CompiledKey) (Node, bool) {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return Node{}, false
	}
	if v.entry.keysHashed() {
		return v.getHashedQuery(k.key, k.hash, count)
	}
	return v.getPlain(k.key, count)
}

// getPlain is Get for an unenriched object. An unescaped key's raw span is
// its content plus two quotes, so a span length other than len(key)+2 cannot
// match and skips the byte comparison (tapeKeyEqual does not inline). Escaped
// keys always byte-compare: their decoded length differs from the raw span.
func (v Node) getPlain(key string, count int) (Node, bool) {
	rawLen := uint32(len(key)) + 2
	if v.entry.next == 2*uint32(count)+1 {
		// Flat object: every value is one entry, so the keys sit at fixed
		// offsets from the header and the scan needs no span chase. Later
		// duplicates still win: the scan runs to the end.
		var found *IndexEntry
		for member := 0; member < count; member++ {
			keyEntry := tapeEntryOffset(v.entry, uintptr(2*member)+1)
			flags := keyEntry.flags()
			if flags&tapeFlagEscaped == 0 && keyEntry.end-keyEntry.start != rawLen {
				continue
			}
			if tapeKeyEqual(byteview.SliceRange(v.src, keyEntry.start, keyEntry.end), flags, key) {
				found = tapeEntryOffset(keyEntry, 1)
			}
		}
		if found == nil {
			return Node{}, false
		}
		return Node{src: v.src, entry: found}, true
	}
	keyEntry := tapeEntryOffset(v.entry, 1)
	var found *IndexEntry
	for member := 0; member < count; member++ {
		valueEntry := tapeEntryOffset(keyEntry, 1)
		flags := keyEntry.flags()
		if (flags&tapeFlagEscaped != 0 || keyEntry.end-keyEntry.start == rawLen) &&
			tapeKeyEqual(byteview.SliceRange(v.src, keyEntry.start, keyEntry.end), flags, key) {
			found = valueEntry
		}
		if member+1 < count {
			keyEntry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
		}
	}
	if found == nil {
		return Node{}, false
	}
	return Node{src: v.src, entry: found}, true
}

// getHashedQuery is Get's gated scan for an enriched object (see
// enrichKeyHashes). It rejects each member whose stored key hash differs from
// queryHash before the byte comparison. Escaped keys skip the pre-filter and
// always byte-compare because their stored hash covers the raw spelling.
// Semantics match getPlain exactly, last duplicate included: the scan runs to
// the end.
func (v Node) getHashedQuery(key string, queryHash uint32, count int) (Node, bool) {
	if v.entry.next == 2*uint32(count)+1 {
		// Flat object: keys sit at a fixed two-entry stride, so the vectorized
		// tape scan tests four members per iteration and verifies candidates
		// backward, where the first byte-equal key is the winning last
		// duplicate (see tapeScanFlatHash).
		if value := tapeScanFlatHash(v.src, v.entry, count, key, queryHash); value != nil {
			return Node{src: v.src, entry: value}, true
		}
		return Node{}, false
	}
	keyEntry := tapeEntryOffset(v.entry, 1)
	var found *IndexEntry
	for member := 0; member < count; member++ {
		valueEntry := tapeEntryOffset(keyEntry, 1)
		flags := keyEntry.flags()
		if (flags&tapeFlagEscaped != 0 || keyEntry.next == queryHash) &&
			tapeKeyEqual(byteview.SliceRange(v.src, keyEntry.start, keyEntry.end), flags, key) {
			found = valueEntry
		}
		if member+1 < count {
			keyEntry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
		}
	}
	if found == nil {
		return Node{}, false
	}
	return Node{src: v.src, entry: found}, true
}

// Pointer resolves an RFC 6901 JSON Pointer relative to v. An absent target or
// invalid Node returns a zero Node, false, and nil. Invalid pointer syntax or an
// invalid array-index token returns a [document.PointerError].
func (v Node) Pointer(pointer string) (Node, bool, error) {
	if pointer == "" {
		return v, v.valid(), nil
	}
	if pointer[0] != '/' {
		return Node{}, false, &document.PointerError{Pointer: pointer, Message: "pointer must be empty or start with slash"}
	}
	cur := v
	for i := 1; i <= len(pointer); {
		j := i
		for j < len(pointer) && pointer[j] != '/' {
			j++
		}
		token, err := unescapePointerToken(pointer[i:j])
		if err != nil {
			return Node{}, false, err
		}
		switch cur.Kind() {
		case document.Object:
			next, ok := cur.Get(token)
			if !ok {
				return Node{}, false, nil
			}
			cur = next
		case document.Array:
			index, ok, err := parsePointerIndex(token)
			if err != nil || !ok {
				return Node{}, ok, err
			}
			next, ok := cur.Index(index)
			if !ok {
				return Node{}, false, nil
			}
			cur = next
		default:
			return Node{}, false, nil
		}
		i = j + 1
	}
	return cur, cur.valid(), nil
}

// PointerCompiled resolves a precompiled JSON Pointer relative to v with the
// same absence and array-index error semantics as [Node.Pointer].
func (v Node) PointerCompiled(pointer CompiledPointer) (Node, bool, error) {
	return v.pointerTokens(pointer.tokens)
}

// pointerTokens resolves a compiled pointer's remaining tokens relative to v
// under PointerCompiled's exact semantics. It is the shared tail: the
// shape-deduplicated batch walk (docset_shape.go) resolves a pointer's first
// token against the stored shape and descends the rest through this loop, so
// both routes share one semantics by construction.
func (v Node) pointerTokens(tokens []compiledPointerToken) (Node, bool, error) {
	cur := v
	for i := range tokens {
		token := tokens[i]
		switch cur.Kind() {
		case document.Object:
			// Get's dispatch, with the token's compile-time hash standing in
			// for the per-call rehash on an enriched object.
			count := int(cur.entry.Count())
			if count == 0 {
				return Node{}, false, nil
			}
			var next Node
			var ok bool
			if cur.entry.keysHashed() {
				next, ok = cur.getHashedQuery(token.text, token.hash, count)
			} else {
				next, ok = cur.getPlain(token.text, count)
			}
			if !ok {
				return Node{}, false, nil
			}
			cur = next
		case document.Array:
			index, ok, err := token.arrayIndex()
			if err != nil || !ok {
				return Node{}, ok, err
			}
			next, ok := cur.Index(index)
			if !ok {
				return Node{}, false, nil
			}
			cur = next
		default:
			return Node{}, false, nil
		}
	}
	return cur, cur.valid(), nil
}

// tapeEntryOffset steps offset entries forward within one tape. Callers
// must stay inside the entries built for this document: entry counts come
// from the tape itself (count, next), so the arithmetic never leaves the
// allocation as long as those fields are trusted and empty containers are
// checked before stepping past their header.
func tapeEntryOffset(entry *IndexEntry, offset uintptr) *IndexEntry {
	return (*IndexEntry)(unsafe.Add(unsafe.Pointer(entry), offset*unsafe.Sizeof(IndexEntry{})))
}

// tapeSourceBase is the typed document-pointer boundary for tape read kernels.
//
// Bounds: src points at byte zero of the live document; callers use only tape
// coordinates produced for that document and preserve their validated bounds.
// Ownership: the returned pointer is borrowed for the synchronous accessor or
// helper call only; Node keeps src typed and therefore visible to the garbage
// collector for that complete use.
// Postconditions: the pointer is not retained, stored, converted to uintptr, or
// used to widen a tape range.
// Callers: Node.Bool, Node.Uint64, Node.Float64, and tapeInt64.
func tapeSourceBase(src *byte) unsafe.Pointer {
	return unsafe.Pointer(src)
}

// tapeKeyEqual reports whether a key's raw span (quotes included) decodes to
// key. Unescaped keys compare directly; escaped keys decode incrementally
// against the query — simple escapes, \uXXXX, and surrogate pairs — without
// materializing the decoded spelling. It is every lookup gate's verifier: the
// one comparison the ladder's pre-filters must always fall through to.
func tapeKeyEqual(raw []byte, flags uint8, key string) bool {
	if flags&tapeFlagEscaped == 0 {
		return bytesEqualString(raw[1:len(raw)-1], key)
	}
	raw = raw[1 : len(raw)-1]
	ki := 0
	for i := 0; i < len(raw); {
		if raw[i] != '\\' {
			if ki >= len(key) || raw[i] != key[ki] {
				return false
			}
			i++
			ki++
			continue
		}
		i++
		if raw[i] != 'u' {
			c := decodedSimpleEscape(raw[i])
			if ki >= len(key) || key[ki] != c {
				return false
			}
			i++
			ki++
			continue
		}
		u, _ := hex4(raw, i+1)
		i += 5
		r := rune(u)
		if 0xD800 <= r && r <= 0xDBFF {
			lo, _ := hex4(raw, i+2)
			r = utf16.DecodeRune(r, rune(lo))
			i += 6
		}
		var encoded [utf8.UTFMax]byte
		n := utf8.EncodeRune(encoded[:], r)
		if ki+n > len(key) || !bytesEqualString(encoded[:n], key[ki:ki+n]) {
			return false
		}
		ki += n
	}
	return ki == len(key)
}
