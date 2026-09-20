package vibejson

import (
	"unicode/utf16"
	"unicode/utf8"
	"unsafe"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/byteview"
)

// Node is a lightweight cursor over an Index or Value. It borrows immutable
// source and entry storage; accessors allocate only when materializing data.
type Node struct {
	Src   *byte
	Entry *IndexEntry
}

// NodeFromEntries constructs a root cursor when both backing stores are present.
func NodeFromEntries(src []byte, entries []IndexEntry) Node {
	if len(src) == 0 || len(entries) == 0 {
		return Node{}
	}
	return Node{Src: &src[0], Entry: &entries[0]}
}

// Valid reports whether v refers to an index entry.
func (v Node) Valid() bool {
	return v.Entry != nil
}

// Kind returns the JSON kind of v.
func (v Node) Kind() document.Kind {
	if !v.Valid() {
		return document.Invalid
	}
	return v.Entry.Kind()
}

// Raw returns v's exact source range as a value borrowing the same document.
// An invalid Node returns a zero RawValue.
func (v Node) Raw() RawValue {
	if !v.Valid() {
		return RawValue{}
	}
	e := v.Entry
	return RawValue{Src: byteview.SliceRange(v.Src, e.Start, e.End)}
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
	return byteview.ByteAt(v.Src, uintptr(v.Entry.Start)) == 't', true
}

// NumberBytes returns the original number spelling without revalidating it.
func (v Node) NumberBytes() ([]byte, bool) {
	if v.Kind() != document.Number {
		return nil, false
	}
	e := v.Entry
	return byteview.SliceRange(v.Src, e.Start, e.End), true
}

// NumberText returns an allocation-free string alias of the source number.
func (v Node) NumberText() (string, bool) {
	b, ok := v.NumberBytes()
	if !ok {
		return "", false
	}
	return OwnedBytesString(b), true
}

// IsInteger reports whether v has an integer spelling; it does not report range.
func (v Node) IsInteger() bool {
	// TapeFlagInt is used only by number entries.
	return v.Valid() && v.Entry.Flags()&TapeFlagInt != 0
}

// Int64 parses an integer value.
func (v Node) Int64() (int64, bool) {
	if v.Kind() != document.Number {
		return 0, false
	}
	e := v.Entry
	if e.Flags()&TapeFlagInt != 0 {
		return TapeInt64(v.Src, e.Start, e.End)
	}
	// The tape flag excludes fractions and exponents without parsing.
	return 0, false
}

// Uint64 parses an unsigned integer value. Fractional, exponent, negative,
// and out-of-range spellings report false.
func (v Node) Uint64() (uint64, bool) {
	if v.Kind() != document.Number {
		return 0, false
	}
	e := v.Entry
	base := tapeSourceBase(v.Src)
	if e.Flags()&TapeFlagInt == 0 || byteview.ByteAt(v.Src, uintptr(e.Start)) == '-' {
		return 0, false
	}
	return tapeUint64(base, int(e.Start), int(e.End))
}

// tapeUint64 parses a validated non-negative integer in [start, end).
func tapeUint64(base unsafe.Pointer, start, end int) (uint64, bool) {
	if value, ok := parseTapeDigitsUint64(base, start, end); ok {
		return value, true
	}
	// The 20-digit case needs an explicit overflow check.
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

// TapeInt64 parses a tape-classified integer and reports overflow.
func TapeInt64(src *byte, start, end uint32) (int64, bool) {
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
	e := v.Entry
	if e.Flags()&TapeFlagInt != 0 {
		// Plain integers can use the exact digit path.
		base := tapeSourceBase(v.Src)
		i := int(e.Start)
		negative := fastByteAt(base, i) == '-'
		if negative {
			i++
		}
		if value, ok := parseTapeDigitsUint64(base, i, int(e.End)); ok {
			f := float64(value)
			if negative {
				f = -f
			}
			return f, true
		}
	}
	// Fractions, exponents, and wide integers use the shared float parser.
	return tapeFloat64(tapeSourceBase(v.Src), int(e.Start), int(e.End))
}

// StringBytes returns an unescaped string as a source alias. Escaped strings
// return false; use AppendText for those.
func (v Node) StringBytes() ([]byte, bool) {
	if v.Kind() != document.String {
		return nil, false
	}
	e := v.Entry
	if e.Flags()&TapeFlagEscaped != 0 {
		return nil, false
	}
	return byteview.SliceRange(v.Src, e.Start+1, e.End-1), true
}

// AppendText appends v's decoded string to dst. The returned caller-owned slice
// may reuse dst's backing storage. For a non-string it returns dst unchanged and
// false.
func (v Node) AppendText(dst []byte) ([]byte, bool) {
	if v.Kind() != document.String {
		return dst, false
	}
	e := v.Entry
	raw := byteview.SliceRange(v.Src, e.Start+1, e.End-1)
	if e.Flags()&TapeFlagEscaped == 0 {
		return append(dst, raw...), true
	}
	return appendDecodedJSONStringTrusted(dst, raw), true
}

// ArrayLen returns the number of array elements.
func (v Node) ArrayLen() (int, bool) {
	if v.Kind() != document.Array {
		return 0, false
	}
	return int(v.Entry.Count()), true
}

// ObjectLen returns the number of object members.
func (v Node) ObjectLen() (int, bool) {
	if v.Kind() != document.Object {
		return 0, false
	}
	return int(v.Entry.Count()), true
}

// Index returns the ith array element, or a zero Node and false.
func (v Node) Index(index int) (Node, bool) {
	count, ok := v.ArrayLen()
	if !ok || index < 0 || index >= count {
		return Node{}, false
	}
	if v.Entry.Next == uint32(count)+1 {
		// Flat arrays use a fixed stride.
		return Node{Src: v.Src, Entry: EntryAt(v.Entry, uintptr(index)+1)}, true
	}
	entry := EntryAt(v.Entry, 1)
	for range index {
		entry = EntryAt(entry, uintptr(entry.Next))
	}
	return Node{Src: v.Src, Entry: entry}, true
}

// Get returns the last object member with key, or a zero Node and false.
func (v Node) Get(key string) (Node, bool) {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return Node{}, false
	}
	if v.Entry.KeysHashed() {
		return v.getHashedQuery(key, HashKey(key), count)
	}
	return v.getPlain(key, count)
}

// GetCompiled is Get with a precomputed key hash.
func (v Node) GetCompiled(k CompiledKey) (Node, bool) {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return Node{}, false
	}
	if v.Entry.KeysHashed() {
		return v.getHashedQuery(k.Key, k.Hash, count)
	}
	return v.getPlain(k.Key, count)
}

// getPlain scans an object without key hashes.
func (v Node) getPlain(key string, count int) (Node, bool) {
	rawLen := uint32(len(key)) + 2
	if v.Entry.Next == 2*uint32(count)+1 {
		var found *IndexEntry
		for member := 0; member < count; member++ {
			keyEntry := EntryAt(v.Entry, uintptr(2*member)+1)
			flags := keyEntry.Flags()
			if flags&TapeFlagEscaped == 0 && keyEntry.End-keyEntry.Start != rawLen {
				continue
			}
			if tapeKeyEqual(byteview.SliceRange(v.Src, keyEntry.Start, keyEntry.End), flags, key) {
				found = EntryAt(keyEntry, 1)
			}
		}
		if found == nil {
			return Node{}, false
		}
		return Node{Src: v.Src, Entry: found}, true
	}
	keyEntry := EntryAt(v.Entry, 1)
	var found *IndexEntry
	for member := 0; member < count; member++ {
		valueEntry := EntryAt(keyEntry, 1)
		flags := keyEntry.Flags()
		if (flags&TapeFlagEscaped != 0 || keyEntry.End-keyEntry.Start == rawLen) &&
			tapeKeyEqual(byteview.SliceRange(v.Src, keyEntry.Start, keyEntry.End), flags, key) {
			found = valueEntry
		}
		if member+1 < count {
			keyEntry = EntryAt(valueEntry, uintptr(valueEntry.Next))
		}
	}
	if found == nil {
		return Node{}, false
	}
	return Node{Src: v.Src, Entry: found}, true
}

// getHashedQuery scans an object using stored key hashes as a prefilter.
func (v Node) getHashedQuery(key string, queryHash uint32, count int) (Node, bool) {
	if v.Entry.Next == 2*uint32(count)+1 {
		if value := tapeScanFlatHash(v.Src, v.Entry, count, key, queryHash); value != nil {
			return Node{Src: v.Src, Entry: value}, true
		}
		return Node{}, false
	}
	keyEntry := EntryAt(v.Entry, 1)
	var found *IndexEntry
	for member := 0; member < count; member++ {
		valueEntry := EntryAt(keyEntry, 1)
		flags := keyEntry.Flags()
		if (flags&TapeFlagEscaped != 0 || keyEntry.Next == queryHash) &&
			tapeKeyEqual(byteview.SliceRange(v.Src, keyEntry.Start, keyEntry.End), flags, key) {
			found = valueEntry
		}
		if member+1 < count {
			keyEntry = EntryAt(valueEntry, uintptr(valueEntry.Next))
		}
	}
	if found == nil {
		return Node{}, false
	}
	return Node{Src: v.Src, Entry: found}, true
}

// Pointer resolves an RFC 6901 JSON Pointer relative to v. An absent target or
// invalid Node returns a zero Node, false, and nil. Invalid pointer syntax or an
// invalid array-index token returns a [document.PointerError].
func (v Node) Pointer(pointer string) (Node, bool, error) {
	if pointer == "" {
		return v, v.Valid(), nil
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
	return cur, cur.Valid(), nil
}

// PointerCompiled resolves a precompiled JSON Pointer relative to v with the
// same absence and array-index error semantics as [Node.Pointer].
func (v Node) PointerCompiled(pointer CompiledPointer) (Node, bool, error) {
	return v.PointerTokens(pointer.Tokens)
}

// PointerTokens resolves compiled pointer tokens relative to v.
func (v Node) PointerTokens(tokens []CompiledPointerToken) (Node, bool, error) {
	cur := v
	for i := range tokens {
		token := tokens[i]
		switch cur.Kind() {
		case document.Object:
			count := int(cur.Entry.Count())
			if count == 0 {
				return Node{}, false, nil
			}
			var next Node
			var ok bool
			if cur.Entry.KeysHashed() {
				next, ok = cur.getHashedQuery(token.Text, token.Hash, count)
			} else {
				next, ok = cur.getPlain(token.Text, count)
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
	return cur, cur.Valid(), nil
}

// EntryAt steps offset entries forward within one tape.
func EntryAt(entry *IndexEntry, offset uintptr) *IndexEntry {
	return (*IndexEntry)(unsafe.Add(unsafe.Pointer(entry), offset*unsafe.Sizeof(IndexEntry{})))
}

// tapeSourceBase returns the source pointer for bounded tape reads.
func tapeSourceBase(src *byte) unsafe.Pointer {
	return unsafe.Pointer(src)
}

// tapeKeyEqual compares a raw key span with its decoded query.
func tapeKeyEqual(raw []byte, flags uint8, key string) bool {
	if flags&TapeFlagEscaped == 0 {
		return BytesEqualString(raw[1:len(raw)-1], key)
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
		if ki+n > len(key) || !BytesEqualString(encoded[:n], key[ki:ki+n]) {
			return false
		}
		ki += n
	}
	return ki == len(key)
}
