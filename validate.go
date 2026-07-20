package simdjson

import (
	"fmt"
)

// Validate returns nil when src is strict JSON. It neither modifies nor
// retains src and is safe for concurrent calls.
func Validate(src []byte) error {
	if len(src) >= validBitmapMinBytes {
		// The bitmap engine can prove validity; only failures re-run the
		// scalar validator, which produces the exact error offset.
		if ok, decided := validBitmap(src); decided && ok {
			return nil
		}
	}
	return ValidateOptions(src, Options{})
}

// ValidateOptions validates src using opts without building an AST. It neither
// modifies nor retains src and is safe for concurrent calls.
func ValidateOptions(src []byte, opts Options) error {
	v := validator{src: src, maxDepth: opts.MaxDepth}
	if v.maxDepth <= 0 {
		v.maxDepth = defaultMaxDepth
	}
	v.skipSpace()
	if err := v.parseValue(0); err != nil {
		return err
	}
	v.skipSpace()
	if v.i != len(src) {
		return v.err(v.i, "unexpected data after top-level value")
	}
	return nil
}

// Valid reports whether src is strict JSON. It neither modifies nor retains
// src and is safe for concurrent calls.
func Valid(src []byte) bool {
	return validFast(src)
}

// ValidateNumber returns nil when src is exactly one JSON number. It neither
// modifies nor retains src and is safe for concurrent calls.
func ValidateNumber(src []byte) error {
	end, msg := scanNumber(src, 0)
	if msg != "" {
		return syntaxError(src, 0, msg)
	}
	if end != len(src) {
		return syntaxError(src, end, "unexpected data after number")
	}
	return nil
}

// ValidNumber reports whether src is exactly one JSON number. It neither
// modifies nor retains src and is safe for concurrent calls.
func ValidNumber(src []byte) bool {
	end, msg := scanNumber(src, 0)
	return msg == "" && end == len(src)
}

// ValidateString returns nil when src is exactly one strict JSON string. It
// neither modifies nor retains src and is safe for concurrent calls.
func ValidateString(src []byte) error {
	if len(src) == 0 || src[0] != '"' {
		return syntaxError(src, 0, "expected string")
	}
	v := validator{src: src, maxDepth: defaultMaxDepth}
	if err := v.parseString(); err != nil {
		return err
	}
	if v.i != len(src) {
		return syntaxError(src, v.i, "unexpected data after string")
	}
	return nil
}

// ValidString reports whether src is exactly one strict JSON string. It neither
// modifies nor retains src and is safe for concurrent calls.
func ValidString(src []byte) bool {
	return ValidateString(src) == nil
}

type validator struct {
	src      []byte
	i        int
	maxDepth int
	layout   *layout
}

type layout struct {
	inline      [64]int
	overflow    []int
	ncontainers int
	values      int
	members     int
}

func countLayout(src []byte, maxDepth int) (layout, error) {
	var l layout
	v := validator{
		src:      src,
		maxDepth: maxDepth,
		layout:   &l,
	}
	if v.maxDepth <= 0 {
		v.maxDepth = defaultMaxDepth
	}
	v.skipSpace()
	if err := v.parseValue(0); err != nil {
		return layout{}, err
	}
	v.skipSpace()
	if v.i != len(src) {
		return layout{}, v.err(v.i, "unexpected data after top-level value")
	}
	return l, nil
}

func (l *layout) addContainer() int {
	idx := l.ncontainers
	l.ncontainers++
	if idx >= len(l.inline) {
		l.overflow = append(l.overflow, 0)
	}
	return idx
}

func (l *layout) setContainer(idx, count int) {
	if idx < len(l.inline) {
		l.inline[idx] = count
		return
	}
	l.overflow[idx-len(l.inline)] = count
}

func (v *validator) err(off int, msg string) error {
	return syntaxError(v.src, off, msg)
}

func (v *validator) skipSpace() {
	v.i = skipSpace(v.src, v.i)
}

func (v *validator) parseValue(depth int) error {
	if depth > v.maxDepth {
		return v.err(v.i, "maximum nesting depth exceeded")
	}
	if v.i >= len(v.src) {
		return v.err(v.i, "expected value")
	}
	switch v.src[v.i] {
	case 'n':
		return v.parseLiteral("null")
	case 't':
		return v.parseLiteral("true")
	case 'f':
		return v.parseLiteral("false")
	case '"':
		return v.parseString()
	case '[':
		return v.parseArray(depth + 1)
	case '{':
		return v.parseObject(depth + 1)
	default:
		if v.src[v.i] == '-' || isDigit(v.src[v.i]) {
			return v.parseNumber()
		}
		return v.err(v.i, fmt.Sprintf("unexpected byte %q while parsing value", v.src[v.i]))
	}
}

func (v *validator) parseLiteral(lit string) error {
	if !matchStringAt(v.src, v.i, lit) {
		return v.err(v.i, "invalid literal")
	}
	v.i += len(lit)
	return nil
}

func (v *validator) parseArray(depth int) error {
	if depth > v.maxDepth {
		return v.err(v.i, "maximum nesting depth exceeded")
	}
	v.i++
	countIndex := -1
	if v.layout != nil {
		countIndex = v.layout.addContainer()
	}
	v.skipSpace()
	if v.i < len(v.src) && v.src[v.i] == ']' {
		v.i++
		return nil
	}

	count := 0
	for {
		v.skipSpace()
		if err := v.parseValue(depth); err != nil {
			return err
		}
		count++
		if v.layout != nil {
			v.layout.values++
		}
		v.skipSpace()
		if v.i >= len(v.src) {
			return v.err(v.i, "unterminated array")
		}
		switch v.src[v.i] {
		case ',':
			v.i++
		case ']':
			v.i++
			if countIndex >= 0 {
				v.layout.setContainer(countIndex, count)
			}
			return nil
		default:
			return v.err(v.i, "expected comma or closing bracket in array")
		}
	}
}

func (v *validator) parseObject(depth int) error {
	if depth > v.maxDepth {
		return v.err(v.i, "maximum nesting depth exceeded")
	}
	v.i++
	countIndex := -1
	if v.layout != nil {
		countIndex = v.layout.addContainer()
	}
	v.skipSpace()
	if v.i < len(v.src) && v.src[v.i] == '}' {
		v.i++
		return nil
	}

	count := 0
	for {
		v.skipSpace()
		if v.i >= len(v.src) || v.src[v.i] != '"' {
			return v.err(v.i, "expected object key string")
		}
		if err := v.parseString(); err != nil {
			return err
		}
		v.skipSpace()
		if v.i >= len(v.src) || v.src[v.i] != ':' {
			return v.err(v.i, "expected colon after object key")
		}
		v.i++
		v.skipSpace()
		if err := v.parseValue(depth); err != nil {
			return err
		}
		count++
		if v.layout != nil {
			v.layout.members++
		}
		v.skipSpace()
		if v.i >= len(v.src) {
			return v.err(v.i, "unterminated object")
		}
		switch v.src[v.i] {
		case ',':
			v.i++
		case '}':
			v.i++
			if countIndex >= 0 {
				v.layout.setContainer(countIndex, count)
			}
			return nil
		default:
			return v.err(v.i, "expected comma or closing brace in object")
		}
	}
}

func (v *validator) parseNumber() error {
	start := v.i
	end, msg := scanNumber(v.src, v.i)
	if msg != "" {
		return v.err(start, msg)
	}
	v.i = end
	return nil
}

func scanNumber(src []byte, i int) (int, string) {
	if i >= len(src) {
		return i, "invalid number"
	}
	if src[i] == '-' {
		i++
		if i >= len(src) {
			return i, "invalid number"
		}
	}
	if src[i] == '0' {
		i++
	} else if isOneNine(src[i]) {
		i++
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	} else {
		return i, "invalid number"
	}
	if i < len(src) && src[i] == '.' {
		i++
		if i >= len(src) || !isDigit(src[i]) {
			return i, "invalid number fraction"
		}
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	if i < len(src) && (src[i] == 'e' || src[i] == 'E') {
		i++
		if i < len(src) && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if i >= len(src) || !isDigit(src[i]) {
			return i, "invalid number exponent"
		}
		for i < len(src) && isDigit(src[i]) {
			i++
		}
	}
	return i, ""
}

func (v *validator) parseString() error {
	v.i++
	for {
		j := scanStringSpecial(v.src, v.i)
		if j >= len(v.src) {
			return v.err(len(v.src), "unterminated string")
		}
		v.i = j
		c := v.src[v.i]
		switch {
		case c == '"':
			v.i++
			return nil
		case c == '\\':
			v.i++
			if v.i >= len(v.src) {
				return v.err(v.i, "unterminated escape sequence")
			}
			if err := v.validateEscape(); err != nil {
				return err
			}
		case c < 0x20:
			return v.err(v.i, "unescaped control byte in string")
		default:
			next, bad := scanStringUnicodeRun(v.src, v.i)
			if bad >= 0 {
				return v.err(bad, "invalid UTF-8 in string")
			}
			v.i = next
		}
	}
}

func (v *validator) validateEscape() error {
	switch v.src[v.i] {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		v.i++
		return nil
	case 'u':
		return v.validateUnicodeEscape()
	default:
		return v.err(v.i-1, "invalid escape sequence")
	}
}

func (v *validator) validateUnicodeEscape() error {
	start := v.i - 1
	v.i++
	u, ok := hex4(v.src, v.i)
	if !ok {
		return v.err(start, "invalid unicode escape")
	}
	v.i += 4
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF {
		if v.i+6 > len(v.src) || v.src[v.i] != '\\' || v.src[v.i+1] != 'u' {
			return v.err(start, "missing low surrogate")
		}
		v.i += 2
		lo, ok := hex4(v.src, v.i)
		if !ok {
			return v.err(start, "invalid low surrogate")
		}
		v.i += 4
		lor := rune(lo)
		if lor < 0xDC00 || lor > 0xDFFF {
			return v.err(start, "invalid low surrogate")
		}
		return nil
	}
	if 0xDC00 <= r && r <= 0xDFFF {
		return v.err(start, "unexpected low surrogate")
	}
	return nil
}
