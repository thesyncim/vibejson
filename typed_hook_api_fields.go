package simdjson

import (
	"strings"
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

const maxFieldFoldVariants = 64

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
			panic("simdjson: duplicate FieldSet member name: " + name)
		}
		set.fields[i] = MakeField(name)
		set.byName[name] = i
	}
	// An ASCII-folded lookup is safe only when no other declared name is in
	// the same Unicode EqualFold class. Disable the packed field's ASCII fold
	// bits on a collision: an expected-order match must not shadow another
	// field's exact name.
	for i, name := range names {
		for j, other := range names {
			if i != j && strings.EqualFold(name, other) {
				set.fields[i].f.keyFold = 0
				break
			}
		}
	}
	// Preserve the original one-entry-per-name ASCII index. Each key resolves to
	// the first declaration in its full Unicode fold class; an exact name still
	// wins through byName above it.
	for i, name := range names {
		fold := foldFieldKey(name)
		if fold == "" {
			continue
		}
		first := i
		for j := 0; j < i; j++ {
			if strings.EqualFold(names[j], fold) {
				first = j
				break
			}
		}
		if _, exists := set.byNameFold[fold]; !exists {
			set.byNameFold[fold] = first
		}
	}

	// Add the remaining non-ASCII variants to the same index. Expansion is
	// bounded across the set; on overflow a hidden-capacity marker selects the
	// ordered EqualFold fallback for map misses.
	added := 0
	fallback := false
	for i, name := range names {
		variants, ok := fieldFoldVariants(name)
		if !ok {
			fallback = true
			break
		}
		for _, fold := range variants {
			if _, exists := set.byNameFold[fold]; exists {
				continue
			}
			if added == maxFieldFoldVariants {
				fallback = true
				break
			}
			set.byNameFold[fold] = i
			added++
		}
		if fallback {
			break
		}
	}
	if fallback {
		visible := len(set.fields)
		fields := make([]Field, visible, visible+1)
		copy(fields, set.fields)
		set.fields = fields
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
	fold := foldFieldKey(key)
	if i, ok := s.byNameFold[fold]; ok {
		return i, true
	}
	if len(s.fields) == 0 {
		return -1, false
	}
	if cap(s.fields) == len(s.fields) {
		return -1, false
	}
	// Only bounded-expansion overflow reaches this path.
	for i := range s.fields {
		if strings.EqualFold(s.fields[i].f.name, key) {
			return i, true
		}
	}
	return -1, false
}

// foldFieldKey lower-cases the ASCII letters of a key for the case-insensitive
// index. A key with no ASCII letters returns itself. Unicode simple-fold
// variants are expanded when the FieldSet is built.
func foldFieldKey(key string) string {
	needs := false
	for i := 0; i < len(key); i++ {
		if c := key[i]; 'A' <= c && c <= 'Z' {
			needs = true
			break
		}
	}
	if !needs {
		return key
	}
	b := make([]byte, len(key))
	for i := 0; i < len(key); i++ {
		c := key[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		b[i] = c
	}
	return string(b)
}

func fieldFoldVariants(name string) ([]string, bool) {
	if !utf8.ValidString(name) {
		return nil, false
	}
	variants := []string{""}
	for _, r := range name {
		class := simpleFoldClass(r)
		if len(variants) > maxFieldFoldVariants/len(class) {
			return nil, false
		}
		next := make([]string, 0, len(variants)*len(class))
		for _, prefix := range variants {
			for _, folded := range class {
				next = append(next, prefix+string(folded))
			}
		}
		variants = next
	}
	return variants, true
}

func simpleFoldClass(r rune) []rune {
	class := make([]rune, 0, 3)
	for current := r; ; current = unicode.SimpleFold(current) {
		folded := current
		if 'A' <= folded && folded <= 'Z' {
			folded += 'a' - 'A'
		}
		seen := false
		for _, existing := range class {
			if existing == folded {
				seen = true
				break
			}
		}
		if !seen {
			class = append(class, folded)
		}
		if unicode.SimpleFold(current) == r {
			return class
		}
	}
}
