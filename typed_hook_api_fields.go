package vibejson

import (
	"unicode"
	"unicode/utf8"
)

// --- Field / FieldSet: packed-key matchers ---------------------------------

// Field is a precompiled member-name matcher: the packed first-word compare the
// interpreter uses for expected-order matching. Build one per member at init
// time with [MakeField] and reuse it for the life of the program; it is
// immutable and safe to share across goroutines.
type Field struct {
	f typedField
}

// Name reports the member name the Field matches.
func (f Field) Name() string { return f.f.name }

// FieldSet groups a struct's Fields so an arbitrary-order body can resolve a
// key from NextField to a member index with one lookup, the same case-folding
// the compiled decoder applies. Build it once with [MakeFieldSet] and treat it
// as immutable. Concurrent reads are safe while callers do not replace the
// values reached through [FieldSet.Field].
type FieldSet struct {
	fields     []Field
	byName     map[string]int
	byNameFold map[string]int
}

// MakeField packs name for the one-word member match. Names of seven bytes or
// fewer pack the closing quote and the following colon into the same word, so a
// single masked compare matches the name, its terminator, and the separator at
// once. Names longer than 255 bytes fall back to the cursor's general key path.
func MakeField(name string) Field {
	var f typedField
	f.name = name
	if len(name) <= 7 {
		for byteIndex := range len(name) {
			ch := name[byteIndex]
			f.key |= uint64(ch) << (byteIndex * 8)
			if lower := ch | 0x20; 'a' <= lower && lower <= 'z' {
				f.keyFold |= 0x20 << (byteIndex * 8)
			}
		}
		f.key |= uint64('"') << (len(name) * 8)
		if len(name) <= 6 {
			f.key |= uint64(':') << ((len(name) + 1) * 8)
			f.keyMask = ^uint64(0) >> ((6 - len(name)) * 8)
		} else {
			f.keyMask = ^uint64(0)
		}
		f.keyLen = uint8(len(name))
	} else if len(name) <= 255 {
		for byteIndex := range 8 {
			ch := name[byteIndex]
			f.key |= uint64(ch) << (byteIndex * 8)
			if lower := ch | 0x20; 'a' <= lower && lower <= 'z' {
				f.keyFold |= 0x20 << (byteIndex * 8)
			}
		}
		f.keyMask = ^uint64(0)
		f.keyLen = uint8(len(name))
	}
	return Field{f: f}
}

// MakeFieldSet builds a FieldSet over the given member names, indexed by
// declaration order. The returned set both exposes each name's packed [Field]
// (via Field) for the expected-order fast path and resolves an arbitrary key to
// its index (via Lookup) for the general path.
func MakeFieldSet(names ...string) FieldSet {
	set := FieldSet{
		fields:     make([]Field, len(names)),
		byName:     make(map[string]int, len(names)),
		byNameFold: make(map[string]int, len(names)*2),
	}
	for i, name := range names {
		if _, duplicate := set.byName[name]; duplicate {
			panic("vibejson: duplicate FieldSet member name: " + name)
		}
		set.fields[i] = MakeField(name)
		set.byName[name] = i
		fold := foldFieldKey(name)
		if first, exists := set.byNameFold[fold]; exists {
			// Packed ASCII folding must not shadow an exact match for
			// another member in the same Unicode fold class.
			set.fields[first].f.keyFold = 0
			set.fields[i].f.keyFold = 0
		} else {
			set.byNameFold[fold] = i
		}
	}
	return set
}

// Len reports the number of members in the set.
func (s FieldSet) Len() int { return len(s.fields) }

// Field returns the packed matcher for member index i, for the expected-order
// fast path: c.Field(first, set.Field(i)). The returned pointer aliases set
// storage and must be treated as read-only; replacing its value invalidates the
// set's concurrent-read guarantee.
func (s FieldSet) Field(i int) *Field { return &s.fields[i] }

// Lookup resolves a key from NextField to a member index and true, or -1 and
// false for an unknown member. It matches exactly when caseSensitive is true
// and otherwise falls back to a case-folded match, mirroring the compiled
// decoder's own field matching. Pass c.CaseSensitive() so a body honours
// DecoderOptions.CaseSensitive.
func (s FieldSet) Lookup(key string, caseSensitive bool) (int, bool) {
	if i, ok := s.byName[key]; ok {
		return i, true
	}
	if caseSensitive {
		return -1, false
	}
	prefix := fieldFoldPrefix(key)
	if prefix == len(key) {
		if i, ok := s.byNameFold[key]; ok {
			return i, true
		}
		return -1, false
	}
	var scratch [64]byte
	fold := appendFoldedFieldKey(append(scratch[:0], key[:prefix]...), key[prefix:])
	if i, ok := s.byNameFold[string(fold)]; ok {
		return i, true
	}
	return -1, false
}

// foldFieldKey gives every Unicode simple-fold class a single spelling.
// Lowercase ASCII keys retain their original storage. Malformed UTF-8 is
// decoded as RuneError, matching strings.EqualFold's rune comparisons.
func foldFieldKey(key string) string {
	prefix := fieldFoldPrefix(key)
	if prefix == len(key) {
		return key
	}
	var scratch [64]byte
	return string(appendFoldedFieldKey(append(scratch[:0], key[:prefix]...), key[prefix:]))
}

func fieldFoldPrefix(key string) int {
	for i := 0; i < len(key); i++ {
		if c := key[i]; ('A' <= c && c <= 'Z') || c >= utf8.RuneSelf {
			return i
		}
	}
	return len(key)
}

func appendFoldedFieldKey(dst []byte, key string) []byte {
	for i := 0; i < len(key); {
		c := key[i]
		if c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			dst = append(dst, c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(key[i:])
		folded := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < folded {
				folded = next
			}
		}
		if 'A' <= folded && folded <= 'Z' {
			folded += 'a' - 'A'
		}
		dst = utf8.AppendRune(dst, folded)
		i += size
	}
	return dst
}
