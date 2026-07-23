package slopjson

// Compaction walks a document with a validating recursive-descent parser and
// re-emits it with all insignificant whitespace removed. It shares the scanner
// helpers with the main parser but writes each accepted token straight through
// to the destination, so a valid input is compacted in a single pass and an
// invalid one leaves the destination unchanged in length.

// Compact validates src and returns a new owned compact JSON buffer. It neither
// modifies nor retains src and is safe for concurrent calls. On error it
// returns nil and a [SyntaxError].
func Compact(src []byte) ([]byte, error) {
	return AppendCompact(nil, src)
}

// AppendCompact validates src and appends compact JSON to dst, reusing dst's
// capacity when sufficient. The returned slice is caller-owned and may alias
// dst; src is neither modified nor retained. The writable capacity of dst must
// not overlap src. On error it returns dst unchanged in length and a
// [SyntaxError], although unused capacity may contain partial output. Calls are
// safe concurrently when their sources remain immutable and their writable
// destination storage is independent.
func AppendCompact(dst, src []byte) ([]byte, error) {
	return appendCompact(dst, src, defaultMaxDepth)
}

type compactParser struct {
	src      []byte
	dst      []byte
	i        int
	maxDepth int
}

func appendCompact(dst, src []byte, maxDepth int) ([]byte, error) {
	switch len(src) {
	case 4:
		if src[0] == 'n' && src[1] == 'u' && src[2] == 'l' && src[3] == 'l' ||
			src[0] == 't' && src[1] == 'r' && src[2] == 'u' && src[3] == 'e' {
			return append(dst, src...), nil
		}
	case 5:
		if src[0] == 'f' && src[1] == 'a' && src[2] == 'l' && src[3] == 's' && src[4] == 'e' {
			return append(dst, src...), nil
		}
	}
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	startLen := len(dst)
	c := compactParser{src: src, dst: dst, maxDepth: maxDepth}
	c.skipSpace()
	if err := c.value(0); err != nil {
		return dst[:startLen], err
	}
	c.skipSpace()
	if c.i != len(src) {
		return dst[:startLen], syntaxError(src, c.i, "unexpected data after top-level value")
	}
	return c.dst, nil
}

func (c *compactParser) skipSpace() {
	c.i = skipSpace(c.src, c.i)
}

func (c *compactParser) value(depth int) error {
	if depth > c.maxDepth {
		return syntaxError(c.src, c.i, "maximum nesting depth exceeded")
	}
	if c.i >= len(c.src) {
		return syntaxError(c.src, c.i, "expected value")
	}
	switch c.src[c.i] {
	case 'n':
		return c.literal("null")
	case 't':
		return c.literal("true")
	case 'f':
		return c.literal("false")
	case '"':
		return c.string()
	case '[':
		return c.array(depth + 1)
	case '{':
		return c.object(depth + 1)
	default:
		if c.src[c.i] == '-' || isDigit(c.src[c.i]) {
			return c.number()
		}
		return syntaxError(c.src, c.i, "unexpected byte while parsing value")
	}
}

func (c *compactParser) literal(lit string) error {
	if !matchStringAt(c.src, c.i, lit) {
		return syntaxError(c.src, c.i, "invalid literal")
	}
	c.i += len(lit)
	c.dst = append(c.dst, lit...)
	return nil
}

func (c *compactParser) array(depth int) error {
	if depth > c.maxDepth {
		return syntaxError(c.src, c.i, "maximum nesting depth exceeded")
	}
	c.i++
	c.dst = append(c.dst, '[')
	c.skipSpace()
	if c.i < len(c.src) && c.src[c.i] == ']' {
		c.i++
		c.dst = append(c.dst, ']')
		return nil
	}

	for {
		c.skipSpace()
		if err := c.value(depth); err != nil {
			return err
		}
		c.skipSpace()
		if c.i >= len(c.src) {
			return syntaxError(c.src, c.i, "unterminated array")
		}
		switch c.src[c.i] {
		case ',':
			c.i++
			c.dst = append(c.dst, ',')
		case ']':
			c.i++
			c.dst = append(c.dst, ']')
			return nil
		default:
			return syntaxError(c.src, c.i, "expected comma or closing bracket in array")
		}
	}
}

func (c *compactParser) object(depth int) error {
	if depth > c.maxDepth {
		return syntaxError(c.src, c.i, "maximum nesting depth exceeded")
	}
	c.i++
	c.dst = append(c.dst, '{')
	c.skipSpace()
	if c.i < len(c.src) && c.src[c.i] == '}' {
		c.i++
		c.dst = append(c.dst, '}')
		return nil
	}

	for {
		c.skipSpace()
		if c.i >= len(c.src) || c.src[c.i] != '"' {
			return syntaxError(c.src, c.i, "expected object key string")
		}
		if err := c.string(); err != nil {
			return err
		}
		c.skipSpace()
		if c.i >= len(c.src) || c.src[c.i] != ':' {
			return syntaxError(c.src, c.i, "expected colon after object key")
		}
		c.i++
		c.dst = append(c.dst, ':')
		c.skipSpace()
		if err := c.value(depth); err != nil {
			return err
		}
		c.skipSpace()
		if c.i >= len(c.src) {
			return syntaxError(c.src, c.i, "unterminated object")
		}
		switch c.src[c.i] {
		case ',':
			c.i++
			c.dst = append(c.dst, ',')
		case '}':
			c.i++
			c.dst = append(c.dst, '}')
			return nil
		default:
			return syntaxError(c.src, c.i, "expected comma or closing brace in object")
		}
	}
}

func (c *compactParser) number() error {
	var err error
	c.dst, c.i, err = appendJSONNumberToken(c.dst, c.src, c.i, c.maxDepth)
	return err
}

func (c *compactParser) string() error {
	var err error
	c.dst, c.i, err = appendJSONStringToken(c.dst, c.src, c.i, c.maxDepth)
	return err
}

// appendJSONNumberToken validates the number at src[i] and copies its exact
// source bytes to dst, so no re-formatting alters the literal. It returns the
// updated dst, the index just past the number, and any error. Shared by the
// compact and indent transforms.
func appendJSONNumberToken(dst, src []byte, i, maxDepth int) ([]byte, int, error) {
	start := i
	v := validator{src: src, i: i, maxDepth: maxDepth}
	if err := v.parseNumber(); err != nil {
		return dst, i, err
	}
	return append(dst, src[start:v.i]...), v.i, nil
}

// appendJSONStringToken validates the string at src[i] (src[i] must be '"') and
// copies its exact source bytes to dst, preserving the original escape spelling
// (\uXXXX, \/, surrogate pairs) rather than re-encoding a decoded value. It
// enforces the same strict escape and UTF-8 rules as the parser. Shared by the
// compact and indent transforms.
func appendJSONStringToken(dst, src []byte, i, maxDepth int) ([]byte, int, error) {
	start := i
	i++
	for {
		j := scanStringSpecial(src, i)
		if j >= len(src) {
			return dst, len(src), syntaxError(src, len(src), "unterminated string")
		}
		i = j
		ch := src[i]
		switch {
		case ch == '"':
			i++
			return append(dst, src[start:i]...), i, nil
		case ch == '\\':
			i++
			if i >= len(src) {
				return dst, i, syntaxError(src, i, "unterminated escape sequence")
			}
			v := validator{src: src, i: i, maxDepth: maxDepth}
			if err := v.validateEscape(); err != nil {
				return dst, i, err
			}
			i = v.i
		case ch < 0x20:
			return dst, i, syntaxError(src, i, "unescaped control byte in string")
		default:
			next, bad := scanStringUnicodeRun(src, i)
			if bad >= 0 {
				return dst, bad, syntaxError(src, bad, "invalid UTF-8 in string")
			}
			i = next
		}
	}
}
