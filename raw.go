package simdjson

import (
	"strconv"

	"github.com/thesyncim/simdjson/document"
)

// RawValue is a borrowed exact JSON value. Selectors and iterators return it
// when callers need source bytes or scalar access without building a tree.
// Its bytes alias the input and remain valid only while that input is alive and
// unmodified. Use AppendJSON or Bytes followed by a copy when ownership is
// required. Concurrent reads are safe while the borrowed input remains
// immutable; callers must synchronize any input mutation themselves. The zero
// RawValue is invalid, has no bytes, and makes scalar accessors report false.
type RawValue struct {
	src []byte
}

// Bytes returns the raw JSON bytes. The returned slice aliases the input.
func (r RawValue) Bytes() []byte {
	return r.src
}

// AppendJSON appends the raw JSON value to dst. The returned caller-owned slice
// may reuse dst's backing storage. For independent ownership, dst's backing
// storage must not overlap r's input.
func (r RawValue) AppendJSON(dst []byte) []byte {
	return append(dst, r.src...)
}

// String returns an owned string copy of the raw JSON value.
func (r RawValue) String() string {
	return string(r.src)
}

// Kind returns the top-level kind of the raw JSON value.
func (r RawValue) Kind() Kind {
	if len(r.src) == 0 {
		return Invalid
	}
	switch r.src[0] {
	case 'n':
		return Null
	case 't', 'f':
		return Bool
	case '"':
		return String
	case '[':
		return Array
	case '{':
		return Object
	default:
		if r.src[0] == '-' || isDigit(r.src[0]) {
			return Number
		}
		return Invalid
	}
}

// Valid reports whether r is strict JSON.
func (r RawValue) Valid() bool {
	return Valid(r.src)
}

// IsNull reports whether r is the JSON null value.
func (r RawValue) IsNull() bool {
	return len(r.src) == 4 && r.src[0] == 'n' && r.src[1] == 'u' && r.src[2] == 'l' && r.src[3] == 'l'
}

// Bool returns r as a bool when it is a JSON boolean.
func (r RawValue) Bool() (bool, bool) {
	switch {
	case len(r.src) == 4 && r.src[0] == 't' && r.src[1] == 'r' && r.src[2] == 'u' && r.src[3] == 'e':
		return true, true
	case len(r.src) == 5 && r.src[0] == 'f' && r.src[1] == 'a' && r.src[2] == 'l' && r.src[3] == 's' && r.src[4] == 'e':
		return false, true
	default:
		return false, false
	}
}

// NumberBytes returns r's original JSON number spelling as an input alias.
func (r RawValue) NumberBytes() ([]byte, bool) {
	if !ValidNumber(r.src) {
		return nil, false
	}
	return r.src, true
}

// NumberText returns r's original JSON number spelling as a string aliasing the
// input.
func (r RawValue) NumberText() (string, bool) {
	if !ValidNumber(r.src) {
		return "", false
	}
	return ownedBytesString(r.src), true
}

// Int64 parses r as an int64 JSON number.
func (r RawValue) Int64() (int64, bool) {
	if len(r.src) == 0 {
		return 0, false
	}
	base := rawNumberBase(r.src)
	// One pass validates the number and reports the same plain-integer
	// classification the tape records: an optional minus and digits, no
	// fraction or exponent. Anything else is not an int64 and rejects the way
	// strconv.ParseInt does. A whole-slice match is the RawValue invariant that
	// there is exactly one value with no trailing bytes.
	end, integer, ok := scanNumberFastTagged(base, len(r.src), 0)
	if !ok || end != len(r.src) || !integer {
		return 0, false
	}
	i := 0
	negative := fastByteAt(base, i) == '-'
	if negative {
		i++
	}
	// Twenty digits or more can exceed int64 without overflow analysis; hand
	// those to strconv for the value verdict.
	value, ok := parseTapeDigitsUint64(base, i, end)
	if !ok {
		n, err := strconv.ParseInt(ownedBytesString(r.src), 10, 64)
		return n, err == nil
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

// Uint64 parses r as a uint64 JSON number.
func (r RawValue) Uint64() (uint64, bool) {
	if len(r.src) == 0 {
		return 0, false
	}
	base := rawNumberBase(r.src)
	end, integer, ok := scanNumberFastTagged(base, len(r.src), 0)
	if !ok || end != len(r.src) || !integer || fastByteAt(base, 0) == '-' {
		return 0, false
	}
	return tapeUint64(base, 0, end)
}

// Float64 parses r as a float64 JSON number.
func (r RawValue) Float64() (float64, bool) {
	if len(r.src) == 0 {
		return 0, false
	}
	base := rawNumberBase(r.src)
	// Validate that the slice is exactly one JSON number, then round through
	// the same kernels Node.Float64 uses, reaching strconv only for the
	// truncated or tie-ambiguous spellings they defer on.
	end, _, ok := scanNumberFastTagged(base, len(r.src), 0)
	if !ok || end != len(r.src) {
		return 0, false
	}
	return tapeFloat64(base, 0, len(r.src))
}

// Text returns r as an unquoted JSON string. The boolean reports whether r
// is a string at all — it stays true when a malformed string returns an
// error — so callers can distinguish wrong kind from bad content.
//
// Unescaped strings return a string aliasing the input. Escaped strings
// allocate only for the unescaped output.
func (r RawValue) Text() (string, bool, error) {
	if len(r.src) == 0 || r.src[0] != '"' {
		return "", false, nil
	}
	s := rawSeeker{src: r.src, maxDepth: defaultMaxDepth}
	start, end, escaped, err := s.parseStringRaw()
	if err != nil {
		return "", true, err
	}
	if s.i != len(r.src) {
		return "", true, syntaxError(r.src, s.i, "unexpected data after string")
	}
	if !escaped {
		return ownedBytesString(r.src[start:end]), true, nil
	}
	p := parser{src: r.src, maxDepth: defaultMaxDepth, zeroCopy: true}
	text, err := p.parseString()
	if err != nil {
		return "", true, err
	}
	if p.i != len(r.src) {
		return "", true, syntaxError(r.src, p.i, "unexpected data after string")
	}
	return text, true, nil
}

// Pointer validates all of r and returns the JSON Pointer target within it.
// An absent target returns a zero RawValue, false, and nil.
func (r RawValue) Pointer(pointer string) (RawValue, bool, error) {
	return GetRaw(r.src, pointer)
}

// ScanFirstPointer returns a raw JSON Pointer target within r and stops after
// validating the target. It does not validate bytes after the match, and each
// pointer token resolves to the first matching object member. An absent target
// returns a zero RawValue, false, and nil.
func (r RawValue) ScanFirstPointer(pointer string) (RawValue, bool, error) {
	return ScanFirstRaw(r.src, pointer)
}

// PointerCompiled is [RawValue.Pointer] with a precompiled pointer.
func (r RawValue) PointerCompiled(pointer CompiledPointer) (RawValue, bool, error) {
	return pointer.GetRaw(r.src)
}

// ScanFirstPointerCompiled is [RawValue.ScanFirstPointer] with a precompiled
// pointer.
func (r RawValue) ScanFirstPointerCompiled(pointer CompiledPointer) (RawValue, bool, error) {
	return pointer.ScanFirstRaw(r.src)
}

// GetRaw returns the JSON Pointer target as a RawValue aliasing src. On a nil
// error it has validated all of src as one JSON document. Duplicate object keys
// resolve to the last occurrence, like encoding/json. An absent target returns
// a zero RawValue, false, and nil. Invalid pointer syntax or an array-index token
// invalid for the traversed array returns a [document.PointerError]; invalid
// JSON returns a [SyntaxError]. On error the value is zero and ok is false.
func GetRaw(src []byte, pointer string) (RawValue, bool, error) {
	return GetRawOptions(src, pointer, Options{})
}

// ScanFirstRaw returns the JSON Pointer target as a raw source slice and stops as
// soon as that target has been validated. It validates the traversed path and
// skipped siblings before the target, but unlike GetRaw it does not validate
// the remainder of the document after a match, and each pointer token
// resolves to the first matching member — an early-exit scanner never sees a
// later duplicate. Use GetRaw for encoding/json's last-occurrence semantics.
// The returned RawValue aliases src. An absent target returns a zero RawValue,
// false, and nil; syntax errors encountered before a result return false with
// the error.
func ScanFirstRaw(src []byte, pointer string) (RawValue, bool, error) {
	return ScanFirstRawOptions(src, pointer, Options{})
}

// ScanFirstRawOptions is [ScanFirstRaw] with parser options.
func ScanFirstRawOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	if err := validatePointerSyntax(pointer); err != nil {
		return RawValue{}, false, err
	}
	s := rawSeeker{src: src, maxDepth: opts.MaxDepth, stopAfterFound: true}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	s.skipSpace()
	if pointer == "" {
		return s.captureValue(0)
	}
	return s.findValue(0, 1, pointer)
}

// GetRaw validates src and returns p's target with the same borrowing,
// duplicate-key, absence, and error semantics as the package-level [GetRaw].
func (p CompiledPointer) GetRaw(src []byte) (RawValue, bool, error) {
	return p.GetRawOptions(src, Options{})
}

// GetRawOptions is [CompiledPointer.GetRaw] with parser options.
func (p CompiledPointer) GetRawOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := rawSeeker{src: src, maxDepth: opts.MaxDepth}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	s.skipSpace()
	raw, ok, err := s.findCompiledValue(0, 0, p)
	if err != nil {
		return RawValue{}, false, err
	}
	s.skipSpace()
	if s.i != len(src) {
		return RawValue{}, false, syntaxError(src, s.i, "unexpected data after top-level value")
	}
	return raw, ok, nil
}

// ScanFirstRaw returns p's target as a raw source slice and stops as soon as
// that target has been validated. Like the package-level ScanFirstRaw, each
// pointer token resolves to the first matching member; borrowing, absence, and
// error semantics also match [ScanFirstRaw].
func (p CompiledPointer) ScanFirstRaw(src []byte) (RawValue, bool, error) {
	return p.ScanFirstRawOptions(src, Options{})
}

// ScanFirstRawOptions is [CompiledPointer.ScanFirstRaw] with parser options.
func (p CompiledPointer) ScanFirstRawOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := rawSeeker{src: src, maxDepth: opts.MaxDepth, stopAfterFound: true}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	s.skipSpace()
	return s.findCompiledValue(0, 0, p)
}

// GetRawOptions is [GetRaw] with parser options.
func GetRawOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	if err := validatePointerSyntax(pointer); err != nil {
		return RawValue{}, false, err
	}
	s := rawSeeker{src: src, maxDepth: opts.MaxDepth}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	s.skipSpace()
	var (
		raw RawValue
		ok  bool
		err error
	)
	if pointer == "" {
		raw, ok, err = s.captureValue(0)
	} else {
		raw, ok, err = s.findValue(0, 1, pointer)
	}
	if err != nil {
		return RawValue{}, false, err
	}
	s.skipSpace()
	if s.i != len(src) {
		return RawValue{}, false, syntaxError(src, s.i, "unexpected data after top-level value")
	}
	return raw, ok, nil
}

type rawSeeker struct {
	src            []byte
	i              int
	maxDepth       int
	stopAfterFound bool
	done           bool
}

func (s *rawSeeker) skipSpace() {
	s.i = skipSpace(s.src, s.i)
}

func (s *rawSeeker) captureValue(depth int) (RawValue, bool, error) {
	start := s.i
	if err := s.skipValue(depth); err != nil {
		return RawValue{}, false, err
	}
	if s.stopAfterFound {
		s.done = true
	}
	return RawValue{src: s.src[start:s.i]}, true, nil
}

func (s *rawSeeker) findValue(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if tokenStart > len(pointer) {
		return s.captureValue(depth)
	}
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	if s.i >= len(s.src) {
		return RawValue{}, false, syntaxError(s.src, s.i, "expected value")
	}
	switch s.src[s.i] {
	case '{':
		return s.findObject(depth+1, tokenStart, pointer)
	case '[':
		return s.findArray(depth+1, tokenStart, pointer)
	default:
		if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		return RawValue{}, false, nil
	}
}

func (s *rawSeeker) findArray(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	tokenEnd, nextToken := pointerToken(pointer, tokenStart)
	token, err := unescapePointerToken(pointer[tokenStart:tokenEnd])
	if err != nil {
		return RawValue{}, false, err
	}
	index, indexOK, err := parsePointerIndex(token)
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.skipSpace()
	if s.i < len(s.src) && s.src[s.i] == ']' {
		s.i++
		return RawValue{}, false, nil
	}

	var (
		raw RawValue
		ok  bool
	)
	for elem := 0; ; elem++ {
		s.skipSpace()
		if indexOK && elem == index {
			raw, ok, err = s.findValue(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
		} else {
			err = s.skipValue(depth)
		}
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) {
			return RawValue{}, false, syntaxError(s.src, s.i, "unterminated array")
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return raw, ok, nil
		default:
			return RawValue{}, false, syntaxError(s.src, s.i, "expected comma or closing bracket in array")
		}
	}
}

func (s *rawSeeker) findObject(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	tokenEnd, nextToken := pointerToken(pointer, tokenStart)
	token, err := unescapePointerToken(pointer[tokenStart:tokenEnd])
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.skipSpace()
	if s.i < len(s.src) && s.src[s.i] == '}' {
		s.i++
		return RawValue{}, false, nil
	}

	var (
		raw RawValue
		ok  bool
	)
	for {
		s.skipSpace()
		if s.i >= len(s.src) || s.src[s.i] != '"' {
			return RawValue{}, false, syntaxError(s.src, s.i, "expected object key string")
		}
		keyStart, keyEnd, escaped, err := s.parseStringRaw()
		if err != nil {
			return RawValue{}, false, err
		}
		matched, err := s.keyMatches(token, keyStart, keyEnd, escaped)
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) || s.src[s.i] != ':' {
			return RawValue{}, false, syntaxError(s.src, s.i, "expected colon after object key")
		}
		s.i++
		s.skipSpace()
		if matched {
			raw, ok, err = s.findValue(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
		} else {
			err = s.skipValue(depth)
		}
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) {
			return RawValue{}, false, syntaxError(s.src, s.i, "unterminated object")
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return raw, ok, nil
		default:
			return RawValue{}, false, syntaxError(s.src, s.i, "expected comma or closing brace in object")
		}
	}
}

func (s *rawSeeker) findCompiledValue(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if tokenIndex >= len(pointer.tokens) {
		return s.captureValue(depth)
	}
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	if s.i >= len(s.src) {
		return RawValue{}, false, syntaxError(s.src, s.i, "expected value")
	}
	switch s.src[s.i] {
	case '{':
		return s.findCompiledObject(depth+1, tokenIndex, pointer)
	case '[':
		return s.findCompiledArray(depth+1, tokenIndex, pointer)
	default:
		if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		return RawValue{}, false, nil
	}
}

func (s *rawSeeker) findCompiledArray(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	token := pointer.tokens[tokenIndex]
	index, indexOK, err := token.arrayIndex()
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.skipSpace()
	if s.i < len(s.src) && s.src[s.i] == ']' {
		s.i++
		return RawValue{}, false, nil
	}

	var (
		raw RawValue
		ok  bool
	)
	nextToken := tokenIndex + 1
	for elem := 0; ; elem++ {
		s.skipSpace()
		if indexOK && elem == index {
			raw, ok, err = s.findCompiledValue(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
		} else {
			err = s.skipValue(depth)
		}
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) {
			return RawValue{}, false, syntaxError(s.src, s.i, "unterminated array")
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return raw, ok, nil
		default:
			return RawValue{}, false, syntaxError(s.src, s.i, "expected comma or closing bracket in array")
		}
	}
}

func (s *rawSeeker) findCompiledObject(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	token := pointer.tokens[tokenIndex].text

	s.i++
	s.skipSpace()
	if s.i < len(s.src) && s.src[s.i] == '}' {
		s.i++
		return RawValue{}, false, nil
	}

	var (
		raw RawValue
		ok  bool
	)
	nextToken := tokenIndex + 1
	for {
		s.skipSpace()
		if s.i >= len(s.src) || s.src[s.i] != '"' {
			return RawValue{}, false, syntaxError(s.src, s.i, "expected object key string")
		}
		keyStart, keyEnd, escaped, err := s.parseStringRaw()
		if err != nil {
			return RawValue{}, false, err
		}
		matched, err := s.keyMatches(token, keyStart, keyEnd, escaped)
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) || s.src[s.i] != ':' {
			return RawValue{}, false, syntaxError(s.src, s.i, "expected colon after object key")
		}
		s.i++
		s.skipSpace()
		if matched {
			raw, ok, err = s.findCompiledValue(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
		} else {
			err = s.skipValue(depth)
		}
		if err != nil {
			return RawValue{}, false, err
		}
		s.skipSpace()
		if s.i >= len(s.src) {
			return RawValue{}, false, syntaxError(s.src, s.i, "unterminated object")
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return raw, ok, nil
		default:
			return RawValue{}, false, syntaxError(s.src, s.i, "expected comma or closing brace in object")
		}
	}
}

func (s *rawSeeker) keyMatches(token string, keyStart, keyEnd int, escaped bool) (bool, error) {
	if !escaped {
		return bytesEqualString(s.src[keyStart:keyEnd], token), nil
	}
	p := parser{src: s.src, i: keyStart - 1, maxDepth: s.maxDepth, zeroCopy: true}
	key, err := p.parseString()
	if err != nil {
		return false, err
	}
	return key == token, nil
}

func (s *rawSeeker) parseStringRaw() (start, end int, escaped bool, err error) {
	s.i++
	start = s.i
	for {
		j := scanStringSpecial(s.src, s.i)
		if j >= len(s.src) {
			return 0, 0, false, syntaxError(s.src, len(s.src), "unterminated string")
		}
		s.i = j
		c := s.src[s.i]
		switch {
		case c == '"':
			end = s.i
			s.i++
			return start, end, escaped, nil
		case c == '\\':
			escaped = true
			s.i++
			if s.i >= len(s.src) {
				return 0, 0, false, syntaxError(s.src, s.i, "unterminated escape sequence")
			}
			v := validator{src: s.src, i: s.i, maxDepth: s.maxDepth}
			if err := v.validateEscape(); err != nil {
				return 0, 0, false, err
			}
			s.i = v.i
		case c < 0x20:
			return 0, 0, false, syntaxError(s.src, s.i, "unescaped control byte in string")
		default:
			next, bad := scanStringUnicodeRun(s.src, s.i)
			if bad >= 0 {
				return 0, 0, false, syntaxError(s.src, bad, "invalid UTF-8 in string")
			}
			s.i = next
		}
	}
}

func (s *rawSeeker) skipValue(depth int) error {
	v := validator{src: s.src, i: s.i, maxDepth: s.maxDepth}
	if err := v.parseValue(depth); err != nil {
		return err
	}
	s.i = v.i
	return nil
}

func pointerToken(pointer string, start int) (end, next int) {
	end = start
	for end < len(pointer) && pointer[end] != '/' {
		end++
	}
	if end == len(pointer) {
		return end, len(pointer) + 1
	}
	return end, end + 1
}

func validatePointerSyntax(pointer string) error {
	if pointer == "" {
		return nil
	}
	if pointer[0] != '/' {
		return &document.PointerError{Pointer: pointer, Message: "pointer must be empty or start with slash"}
	}
	for i := 1; i < len(pointer); i++ {
		if pointer[i] != '~' {
			continue
		}
		if i+1 >= len(pointer) || (pointer[i+1] != '0' && pointer[i+1] != '1') {
			msg := "unknown tilde escape"
			if i+1 >= len(pointer) {
				msg = "dangling tilde escape"
			}
			return &document.PointerError{Pointer: pointer, Message: msg}
		}
		i++
	}
	return nil
}

// bytesEqualString compares without allocating: the conversion inside the
// comparison does not escape, so it compiles to a length check plus memequal
// rather than a byte loop — object-key lookups sit on this.
func bytesEqualString(b []byte, s string) bool {
	return string(b) == s
}
