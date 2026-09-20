package vibejson

import (
	"strconv"

	"github.com/thesyncim/vibejson/document"
)

// RawValue is a borrowed exact JSON value. Its bytes alias the input.
type RawValue struct {
	Src []byte
}

// Bytes returns the raw JSON bytes. The returned slice aliases the input.
func (r RawValue) Bytes() []byte {
	return r.Src
}

// AppendJSON appends the raw JSON value to dst.
func (r RawValue) AppendJSON(dst []byte) []byte {
	return append(dst, r.Src...)
}

// String returns an owned string copy of the raw JSON value.
func (r RawValue) String() string {
	return string(r.Src)
}

// Kind returns the top-level kind of the raw JSON value.
func (r RawValue) Kind() document.Kind {
	if len(r.Src) == 0 {
		return document.Invalid
	}
	switch r.Src[0] {
	case 'n':
		return document.Null
	case 't', 'f':
		return document.Bool
	case '"':
		return document.String
	case '[':
		return document.Array
	case '{':
		return document.Object
	default:
		if r.Src[0] == '-' || IsDigit(r.Src[0]) {
			return document.Number
		}
		return document.Invalid
	}
}

// IsNull reports whether r is the JSON null value.
func (r RawValue) IsNull() bool {
	return len(r.Src) == 4 && r.Src[0] == 'n' && r.Src[1] == 'u' && r.Src[2] == 'l' && r.Src[3] == 'l'
}

// Bool returns r as a bool when it is a JSON boolean.
func (r RawValue) Bool() (bool, bool) {
	switch {
	case len(r.Src) == 4 && r.Src[0] == 't' && r.Src[1] == 'r' && r.Src[2] == 'u' && r.Src[3] == 'e':
		return true, true
	case len(r.Src) == 5 && r.Src[0] == 'f' && r.Src[1] == 'a' && r.Src[2] == 'l' && r.Src[3] == 's' && r.Src[4] == 'e':
		return false, true
	default:
		return false, false
	}
}

// NumberBytes returns r's original JSON number spelling as an input alias.
func (r RawValue) NumberBytes() ([]byte, bool) {
	if !validNumber(r.Src) {
		return nil, false
	}
	return r.Src, true
}

// NumberText returns r's original JSON number spelling as an input alias.
func (r RawValue) NumberText() (string, bool) {
	if !validNumber(r.Src) {
		return "", false
	}
	return OwnedBytesString(r.Src), true
}

// Int64 parses r as an int64 JSON number.
func (r RawValue) Int64() (int64, bool) {
	if len(r.Src) == 0 {
		return 0, false
	}
	source := numberSourceOf(r.Src)
	base := source.PointerAt(0)
	// Validate and classify the complete number in one pass.
	end, integer, ok := scanNumberFastTagged(base, len(r.Src), 0)
	if !ok || end != len(r.Src) || !integer {
		return 0, false
	}
	i := 0
	negative := fastByteAt(base, i) == '-'
	if negative {
		i++
	}
	// Wide values need strconv's overflow check.
	value, ok := parseTapeDigitsUint64(base, i, end)
	if !ok {
		n, err := strconv.ParseInt(OwnedBytesString(r.Src), 10, 64)
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
	if len(r.Src) == 0 {
		return 0, false
	}
	source := numberSourceOf(r.Src)
	base := source.PointerAt(0)
	end, integer, ok := scanNumberFastTagged(base, len(r.Src), 0)
	if !ok || end != len(r.Src) || !integer || fastByteAt(base, 0) == '-' {
		return 0, false
	}
	return tapeUint64(base, 0, end)
}

// Float64 parses r as a float64 JSON number.
func (r RawValue) Float64() (float64, bool) {
	if len(r.Src) == 0 {
		return 0, false
	}
	source := numberSourceOf(r.Src)
	base := source.PointerAt(0)
	// Validate the complete number, then use the shared float parser.
	end, _, ok := scanNumberFastTagged(base, len(r.Src), 0)
	if !ok || end != len(r.Src) {
		return 0, false
	}
	return tapeFloat64(base, 0, len(r.Src))
}

// Text returns r as an unquoted JSON string.
func (r RawValue) Text() (string, bool, error) {
	if len(r.Src) == 0 || r.Src[0] != '"' {
		return "", false, nil
	}
	s := rawSeeker{src: r.Src, maxDepth: DefaultMaxDepth}
	start, end, escaped, err := s.parseStringRaw()
	if err != nil {
		return "", true, err
	}
	if s.i != len(r.Src) {
		return "", true, syntaxError(r.Src, s.i, "unexpected data after string")
	}
	if !escaped {
		return OwnedBytesString(r.Src[start:end]), true, nil
	}
	p := parser{src: r.Src, maxDepth: DefaultMaxDepth, zeroCopy: true}
	text, err := p.parseString()
	if err != nil {
		return "", true, err
	}
	if p.i != len(r.Src) {
		return "", true, syntaxError(r.Src, p.i, "unexpected data after string")
	}
	return text, true, nil
}

// StringBytes returns an unescaped string's content as an input alias.
func (r RawValue) StringBytes() ([]byte, bool) {
	if len(r.Src) == 0 || r.Src[0] != '"' {
		return nil, false
	}
	s := rawSeeker{src: r.Src, maxDepth: DefaultMaxDepth}
	start, end, escaped, err := s.parseStringRaw()
	if err != nil || s.i != len(r.Src) || escaped {
		return nil, false
	}
	return r.Src[start:end], true
}

// AppendText appends decoded string content to dst.
func (r RawValue) AppendText(dst []byte) ([]byte, bool, error) {
	if len(r.Src) == 0 || r.Src[0] != '"' {
		return dst, false, nil
	}
	s := rawSeeker{src: r.Src, maxDepth: DefaultMaxDepth}
	start, end, escaped, err := s.parseStringRaw()
	if err != nil {
		return dst, true, err
	}
	if s.i != len(r.Src) {
		return dst, true, syntaxError(r.Src, s.i, "unexpected data after string")
	}
	if !escaped {
		return append(dst, r.Src[start:end]...), true, nil
	}
	return appendDecodedJSONStringTrusted(dst, r.Src[start:end]), true, nil
}

// Pointer validates r and returns its JSON Pointer target.
func (r RawValue) Pointer(pointer string) (RawValue, bool, error) {
	return GetRaw(r.Src, pointer)
}

// ScanFirstPointer returns the first matching target after validating its path.
func (r RawValue) ScanFirstPointer(pointer string) (RawValue, bool, error) {
	return ScanFirstRaw(r.Src, pointer)
}

// PointerCompiled is [RawValue.Pointer] with a precompiled pointer.
func (r RawValue) PointerCompiled(pointer CompiledPointer) (RawValue, bool, error) {
	return pointer.GetRaw(r.Src)
}

// ScanFirstPointerCompiled is [RawValue.ScanFirstPointer] with a precompiled
// pointer.
func (r RawValue) ScanFirstPointerCompiled(pointer CompiledPointer) (RawValue, bool, error) {
	return pointer.ScanFirstRaw(r.Src)
}

// GetRaw validates src and returns its JSON Pointer target. Duplicate keys use
// last-occurrence semantics.
func GetRaw(src []byte, pointer string) (RawValue, bool, error) {
	return GetRawOptions(src, pointer, Options{})
}

// ScanFirstRaw validates the traversed path and returns its first matching
// target, stopping before unrelated trailing input.
func ScanFirstRaw(src []byte, pointer string) (RawValue, bool, error) {
	return ScanFirstRawOptions(src, pointer, Options{})
}

// ScanFirstRawOptions is [ScanFirstRaw] with parser options.
func ScanFirstRawOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	if err := validatePointerSyntax(pointer); err != nil {
		return RawValue{}, false, err
	}
	s := rawSeeker{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth), stopAfterFound: true}
	s.skipSpace()
	if pointer == "" {
		return s.captureValue(0)
	}
	return s.findValue(0, 1, pointer)
}

// GetRaw validates src and returns p's target.
func (p CompiledPointer) GetRaw(src []byte) (RawValue, bool, error) {
	return p.GetRawOptions(src, Options{})
}

// GetRawOptions is [CompiledPointer.GetRaw] with parser options.
func (p CompiledPointer) GetRawOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := rawSeeker{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth)}
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

// ScanFirstRaw validates the traversed path and returns p's first match.
func (p CompiledPointer) ScanFirstRaw(src []byte) (RawValue, bool, error) {
	return p.ScanFirstRawOptions(src, Options{})
}

// ScanFirstRawOptions is [CompiledPointer.ScanFirstRaw] with parser options.
func (p CompiledPointer) ScanFirstRawOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := rawSeeker{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth), stopAfterFound: true}
	s.skipSpace()
	return s.findCompiledValue(0, 0, p)
}

// GetRawOptions is [GetRaw] with parser options.
func GetRawOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	if err := validatePointerSyntax(pointer); err != nil {
		return RawValue{}, false, err
	}
	s := rawSeeker{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth)}
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
	s.i = SkipSpace(s.src, s.i)
}

func (s *rawSeeker) captureValue(depth int) (RawValue, bool, error) {
	start := s.i
	if err := s.skipValue(depth); err != nil {
		return RawValue{}, false, err
	}
	if s.stopAfterFound {
		s.done = true
	}
	return RawValue{Src: s.src[start:s.i]}, true, nil
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
	if tokenIndex >= len(pointer.Tokens) {
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
	token := pointer.Tokens[tokenIndex]
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
	token := pointer.Tokens[tokenIndex].Text

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
		return BytesEqualString(s.src[keyStart:keyEnd], token), nil
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

// BytesEqualString compares without allocating: the conversion inside the
// comparison does not escape, so it compiles to a length check plus memequal
// rather than a byte loop — object-key lookups sit on this.
func BytesEqualString(b []byte, s string) bool {
	return string(b) == s
}
