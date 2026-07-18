package simdjson

// Method hooks are the opt-in custom tier for typed decode and encode: the
// easyjson MarshalEasyJSON/UnmarshalEasyJSON analog, refined for this package's
// kernels. A type opts in by implementing [UnmarshalerSimd] or [MarshalerSimd]
// with signatures that avoid raw-value reparsing, output re-validation and
// compaction, and intermediate buffers. The compiled plan detects the
// interfaces at compile time. Cursor state crosses decode hooks by value;
// receiver dispatch follows ordinary Go ownership in both directions.
//
// # Lifetime contract
//
// [DecodeCursor] and [TrustedAppender] are passed and returned by value. A hook
// owns its input value during the call and returns the advanced value. Retaining
// a copy keeps referenced input or output storage alive, but a stale copy is
// disconnected from the enclosing operation and cannot advance it.
//
// # Safety
//
// Decode and encode hooks use normal Go receiver ownership: addressable values
// expose their GC-visible *T, while non-addressable value receivers get a
// runtime-owned value copy. Cursor state contains ordinary Go pointers and is
// transferred by value; no pointer into a decoder stack frame is exposed.
// Interface values are constructed only by reflection and the Go runtime.

import (
	"reflect"
	"strings"
	"unicode"
	"unicode/utf8"
	"unsafe"
)

// UnmarshalerSimd is an opt-in custom decode hook. Use it for a type that needs
// custom semantics or for generated decoding code; ordinary structs should use
// the compiled Decoder. The method reads through the decoder's kernels instead
// of reparsing raw bytes. Dispatch creates no receiver or cursor heap shadow;
// a fresh stack-local receiver may still undergo the ordinary Go escape needed
// when arbitrary user code can retain *T.
//
// The method must consume exactly one JSON value and leave the cursor
// positioned immediately after it, exactly as the compiled decoder would.
// Returning an error aborts the enclosing decode. The returned cursor must be
// the input cursor after consuming exactly one value, including on error.
type UnmarshalerSimd interface {
	UnmarshalSimdJSON(c DecodeCursor) (DecodeCursor, error)
}

// MarshalerSimd is the opt-in encode hook. A type implements it to append its
// own compact JSON through the TrustedAppender's zero-cost helpers and return the
// advanced TrustedAppender. It is the simdjson-native counterpart of json.Marshaler.
//
// The by-value builder shape lets the output buffer remain in registers across
// the whole body. Bodies thread the TrustedAppender through and return it
// (w = w.Int(...), or chained). The output is trusted to be valid compact JSON
// for the value and is spliced into the surrounding document verbatim: there
// is no re-validation, compaction, or escape pass, which is the whole point of
// the hook. Emitting malformed JSON corrupts the surrounding document, so a
// generator must emit correct syntax.
// Tests and debug builds can enable the simdjson_validate_hooks build tag to
// validate exactly the span emitted by every invocation; normal builds compile
// that validation away.
//
// The TrustedAppender must not be retained past the call; see the lifetime
// contract in this file's package comment.
type MarshalerSimd interface {
	MarshalSimdJSON(w TrustedAppender) TrustedAppender
}

var (
	unmarshalerSimdReflectType = reflect.TypeFor[UnmarshalerSimd]()
	marshalerSimdReflectType   = reflect.TypeFor[MarshalerSimd]()
)

// DecodeCursor is the public face of the typed decoder inside an
// UnmarshalSimdJSON body: a handle over the same interface-free parser the
// compiled interpreter drives, exposing the scalar kernels, the packed-key
// field matcher, and the array iterator. Generated code parses with exactly
// the machinery the compiled path uses, so a hook pays no interpretation
// overhead.
//
// A DecodeCursor is obtained as the argument to UnmarshalSimdJSON and returned
// after consuming one value. It owns a copy of the parser state; copying it is
// safe, but only the returned value advances the enclosing decode.
type DecodeCursor struct {
	d decoderCursor
}

// TrustedAppender is the encoder handle passed to MarshalSimdJSON. It is a
// by-value builder over the output buffer; methods must thread it through and
// return the advanced value.
//
// Errors are sticky. The first helper that meets an unencodable value (a NaN
// or an infinity) poisons the builder and every later helper is a no-op; the
// enclosing encode reports the failure after the body returns, so a generated
// body stays straight-line with no per-helper error check. The poison is a
// plain bool rather than an error field, keeping the value small.
type TrustedAppender struct {
	dst        []byte
	escapeHTML bool
	bad        bool
}

// --- TrustedAppender: encode helpers ---------------------------------------

// RawUnchecked appends lit verbatim. The caller vouches that lit is valid JSON
// for the position; it is spliced in with no validation or escaping.
func (w TrustedAppender) RawUnchecked(lit string) TrustedAppender {
	w.dst = append(w.dst, lit...)
	return w
}

// RawBytesUnchecked appends lit verbatim, the []byte form of RawUnchecked.
func (w TrustedAppender) RawBytesUnchecked(lit []byte) TrustedAppender {
	w.dst = append(w.dst, lit...)
	return w
}

// RawByteUnchecked appends one byte verbatim, typically a structural
// delimiter. The caller is responsible for its position and validity.
func (w TrustedAppender) RawByteUnchecked(b byte) TrustedAppender {
	w.dst = append(w.dst, b)
	return w
}

// Null appends the JSON null literal.
func (w TrustedAppender) Null() TrustedAppender {
	w.dst = append(w.dst, "null"...)
	return w
}

// Bool appends true or false.
func (w TrustedAppender) Bool(v bool) TrustedAppender {
	if v {
		w.dst = append(w.dst, "true"...)
	} else {
		w.dst = append(w.dst, "false"...)
	}
	return w
}

// Int appends v in base 10.
func (w TrustedAppender) Int(v int64) TrustedAppender {
	w.dst = appendCompactInt(w.dst, v)
	return w
}

// Uint appends v in base 10.
func (w TrustedAppender) Uint(v uint64) TrustedAppender {
	w.dst = appendCompactUint(w.dst, v)
	return w
}

// String appends s as a JSON string under the encoder's escaping options,
// matching encoding/json: control characters, quotes, and backslashes are
// escaped, invalid UTF-8 becomes the replacement character, and HTML-sensitive
// bytes are escaped unless the encoder disabled HTML escaping.
func (w TrustedAppender) String(s string) TrustedAppender {
	w.dst = appendEncodedJSONString(w.dst, s, w.escapeHTML)
	return w
}

// Float64 appends v in encoding/json's shortest round-trippable form. A NaN or
// an infinity has no JSON form and poisons the TrustedAppender; the enclosing encode
// then reports the value as unsupported.
func (w TrustedAppender) Float64(v float64) TrustedAppender {
	dst, err := appendJSONFloat(w.dst, v, 64)
	if err != nil {
		w.bad = true
		return w
	}
	w.dst = dst
	return w
}

// Float32 appends v in encoding/json's shortest round-trippable form for a
// 32-bit float. A NaN or an infinity poisons the TrustedAppender.
func (w TrustedAppender) Float32(v float32) TrustedAppender {
	dst, err := appendJSONFloat(w.dst, float64(v), 32)
	if err != nil {
		w.bad = true
		return w
	}
	w.dst = dst
	return w
}

// EscapeHTML reports whether the encoder escapes HTML-sensitive bytes, so a
// body that formats its own output through Raw can match the option.
func (w TrustedAppender) EscapeHTML() bool { return w.escapeHTML }

// --- DecodeCursor: object framing ------------------------------------------

// BeginObject consumes the opening brace of an object, applying the decoder's
// depth guard. typeName names the type in the error if the next value is not
// an object.
func (c *DecodeCursor) BeginObject(typeName string) error { return c.d.BeginObject(typeName) }

// BeginArray consumes the opening bracket of an array, applying the depth guard.
func (c *DecodeCursor) BeginArray(typeName string) error { return c.d.BeginArray(typeName) }

// NextField is the general object-member iterator. Pass first=true only for
// the first call after BeginObject; it returns the next member's key and true,
// or "" and false at the closing brace. The returned key aliases the source
// (or the escaped-string arena) under the active decode mode and must not be
// mutated. Use it for arbitrary member order, unknown members, and duplicates;
// a straight-line body pairs it with a [FieldSet] for the packed-key match.
func (c *DecodeCursor) NextField(first bool) (key string, ok bool, err error) {
	return c.d.NextObjectField(first)
}

// Field matches one expected member name with the packed one-word compare,
// consuming the comma (when first is false), the quoted name, and the colon on
// success, and leaving the cursor on the member value. It reports false without
// advancing when the next member is not name, so a body can fall back to
// NextField. Expected-order bodies chain Field calls; the first miss should
// drop to a NextField loop keyed by a [FieldSet].
func (c *DecodeCursor) Field(first bool, f *Field) bool {
	return c.d.matchObjectFieldExpected(first, &f.f)
}

// CaseSensitive reports whether the decoder was compiled with
// DecoderOptions.CaseSensitive, so a NextField loop can fold key comparisons to
// match the decoder's own field matching.
func (c *DecodeCursor) CaseSensitive() bool { return c.d.CaseSensitive() }

// --- DecodeCursor: array framing -------------------------------------------

// NextElement reports whether another array element follows. Pass first=true
// only for the first call after BeginArray. It consumes the comma between
// elements and the closing bracket at the end.
func (c *DecodeCursor) NextElement(first bool) (bool, error) { return c.d.NextArrayElement(first) }

// --- DecodeCursor: low-level positioning -----------------------------------

// Expect consumes ch when it is the next byte, without skipping whitespace, and
// reports whether it did. It lets a body stay on the packed path for a compact
// document and fall back explicitly when a delimiter is missing.
func (c *DecodeCursor) Expect(ch byte) bool {
	d := &c.d
	if i := d.i; i < len(d.src) && d.src[i] == ch {
		d.i = i + 1
		return true
	}
	return false
}

// ExpectObjectClose consumes a closing brace, updating the depth bookkeeping,
// and reports whether it did. A body uses it to close an object opened with
// BeginObject after matching every member in order.
func (c *DecodeCursor) ExpectObjectClose() bool {
	d := &c.d
	if i := d.i; i < len(d.src) && d.src[i] == '}' {
		d.i = i + 1
		d.depth--
		return true
	}
	return false
}

// Skip validates and consumes exactly one JSON value without materializing it,
// for a member or element a body does not model.
func (c *DecodeCursor) Skip() error { return c.d.Skip() }

// Null consumes a null literal and reports true, or leaves a non-null value in
// place and reports false. A body calls it before a scalar read to distinguish
// an absent value.
func (c *DecodeCursor) Null() (bool, error) { return c.d.TryNull() }

// Raw captures the raw bytes of the next JSON value, validating and consuming
// exactly one value. The returned RawValue aliases the source buffer, so it is
// valid only under the input's lifetime and, in zero-copy mode, only while the
// input is unmodified.
func (c *DecodeCursor) Raw() (RawValue, error) {
	d := &c.d
	start := d.i
	if err := d.Skip(); err != nil {
		return RawValue{}, err
	}
	return RawValue{src: d.src[start:d.i]}, nil
}

// --- DecodeCursor: scalar reads --------------------------------------------

// Bool decodes a JSON boolean into dst.
func (c *DecodeCursor) Bool(dst *bool) error { return c.d.Bool(dst) }

// Int decodes a JSON number into an int.
func (c *DecodeCursor) Int(dst *int) error {
	if useStableNumericMethods {
		return c.d.IntNative(dst)
	}
	return c.d.Int(dst)
}

// Int8 decodes a JSON number into an int8.
func (c *DecodeCursor) Int8(dst *int8) error {
	if useStableNumericMethods {
		return c.d.Int8(dst)
	}
	return c.d.Int(dst)
}

// Int16 decodes a JSON number into an int16.
func (c *DecodeCursor) Int16(dst *int16) error {
	if useStableNumericMethods {
		return c.d.Int16(dst)
	}
	return c.d.Int(dst)
}

// Int32 decodes a JSON number into an int32.
func (c *DecodeCursor) Int32(dst *int32) error {
	if useStableNumericMethods {
		return c.d.Int32(dst)
	}
	return c.d.Int(dst)
}

// Int64 decodes a JSON number into an int64.
func (c *DecodeCursor) Int64(dst *int64) error {
	if useStableNumericMethods {
		return c.d.Int64(dst)
	}
	return c.d.Int(dst)
}

// Uint decodes a JSON number into a uint.
func (c *DecodeCursor) Uint(dst *uint) error {
	if useStableNumericMethods {
		return c.d.UintNative(dst)
	}
	return c.d.Uint(dst)
}

// Uint8 decodes a JSON number into a uint8.
func (c *DecodeCursor) Uint8(dst *uint8) error {
	if useStableNumericMethods {
		return c.d.Uint8(dst)
	}
	return c.d.Uint(dst)
}

// Uint16 decodes a JSON number into a uint16.
func (c *DecodeCursor) Uint16(dst *uint16) error {
	if useStableNumericMethods {
		return c.d.Uint16(dst)
	}
	return c.d.Uint(dst)
}

// Uint32 decodes a JSON number into a uint32.
func (c *DecodeCursor) Uint32(dst *uint32) error {
	if useStableNumericMethods {
		return c.d.Uint32(dst)
	}
	return c.d.Uint(dst)
}

// Uint64 decodes a JSON number into a uint64.
func (c *DecodeCursor) Uint64(dst *uint64) error {
	if useStableNumericMethods {
		return c.d.Uint64(dst)
	}
	return c.d.Uint(dst)
}

// Float32 decodes a JSON number into a float32.
func (c *DecodeCursor) Float32(dst *float32) error {
	if useStableNumericMethods {
		return c.d.Float32(dst)
	}
	return c.d.Float(dst)
}

// Float64 decodes a JSON number into a float64.
func (c *DecodeCursor) Float64(dst *float64) error {
	if useStableNumericMethods {
		return c.d.Float64(dst)
	}
	return c.d.Float(dst)
}

// String decodes a JSON string into dst, unescaping as needed. In zero-copy
// mode an unescaped string aliases the source.
func (c *DecodeCursor) String(dst *string) error { return c.d.String(dst) }

// NumberText decodes a JSON number as its literal text, for a json.Number-style
// field that preserves the exact digits.
func (c *DecodeCursor) NumberText(dst *string) error { return c.d.Number(dst) }

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
// the compiled decoder applies. Build it once with [MakeFieldSet].
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
// fast path: c.Field(first, set.Field(i)).
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

// --- plan dispatch ---------------------------------------------------------

// decodeViaSimdHook uses the ordinary addressable destination receiver. The
// runtime-built interface keeps the destination storage visible to the GC;
// dispatchDecodeHook transfers cursor state by value.
func (cursor *decoderCursor) decodeViaSimdHook(node *typedNode, dst unsafe.Pointer) error {
	boxed := pointerInterfaceAt(node.typ, dst)
	hook, ok := boxed.(UnmarshalerSimd)
	if !ok {
		return &DecodeError{Offset: cursor.i, Type: node.typ, Reason: "invalid compiled operation"}
	}
	return dispatchDecodeHook(hook, cursor)
}

// encodeViaSimdHook uses ordinary Go receiver ownership. Addressable values
// expose their real *T through a runtime-built interface, so retaining the
// receiver safely retains (and aliases) the caller's value without allocating
// a detached shadow. Non-addressable map/interface values can reach this point
// only when T itself implements the hook; they receive the normal value copy a
// Go value-receiver call specifies. Compact output is trusted and spliced
// without the validation and re-escape passes of json.Marshaler.
func (e *encodeState) encodeViaSimdHook(node *typedNode, src unsafe.Pointer) error {
	var boxed any
	if e.nonAddr {
		boxed = valueInterfaceAt(node.typ, src)
	} else {
		boxed = pointerInterfaceAt(node.typ, src)
	}
	hook, ok := boxed.(MarshalerSimd)
	if !ok {
		return &EncodeError{Reason: "invalid compiled operation"}
	}
	start := len(e.dst)
	w := dispatchEncodeHook(hook, TrustedAppender{dst: e.dst, escapeHTML: e.escapeHTML})
	e.dst = w.dst
	if w.bad {
		return &EncodeError{Reason: "MarshalSimdJSON: unsupported value"}
	}
	if validateSimdHookOutput && !Valid(e.dst[start:]) {
		return &EncodeError{Reason: "MarshalSimdJSON produced invalid JSON"}
	}
	return nil
}
