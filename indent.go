package vibejson

import "slices"

const (
	indentSpaces64 = "                                                                "
	indentTabs64   = "\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t\t"
)

// Indentation walks a document with the same validating recursive-descent
// parser as compaction, but writes structural whitespace between tokens instead
// of removing it. String and number tokens are copied verbatim from the source,
// so the original escape spelling and number literals are preserved exactly,
// matching encoding/json's json.Indent. An invalid input leaves the destination
// unchanged in length.

type indentParser struct {
	src      []byte
	dst      []byte
	i        int
	maxDepth int
	prefix   string
	indent   string
}

func appendIndentBytes(dst, src []byte, prefix, indent string, maxDepth int) ([]byte, error) {
	maxDepth = maxDepthOrDefault(maxDepth)
	startLen := len(dst)
	p := indentParser{src: src, dst: dst, maxDepth: maxDepth, prefix: prefix, indent: indent}
	p.skipSpace()
	if err := p.value(0); err != nil {
		return dst[:startLen], err
	}
	trailing := p.i
	p.skipSpace()
	if p.i != len(src) {
		return dst[:startLen], syntaxError(src, p.i, "unexpected data after top-level value")
	}
	if trailing != p.i {
		p.dst = append(p.dst, src[trailing:p.i]...)
	}
	return p.dst, nil
}

func (p *indentParser) skipSpace() {
	p.i = skipSpace(p.src, p.i)
}

// newline writes the line break that precedes a nested element or a closing
// bracket at the given nesting depth: a newline, the prefix, then depth copies
// of indent.
func (p *indentParser) newline(depth int) {
	indent := p.indent
	if len(p.prefix) == 0 {
		if depth == 0 || indent == "" {
			p.dst = append(p.dst, '\n')
			return
		}
		if depth == 1 {
			p.dst = append(append(p.dst, '\n'), indent...)
			return
		}
	}
	total := 1 + len(p.prefix)
	indentBytes := 0
	if depth > 0 && indent != "" {
		indentBytes = depth * len(indent)
		total += indentBytes
	}
	dst := slices.Grow(p.dst, total)
	base := len(dst)
	dst = dst[:base+total]
	dst[base] = '\n'
	pos := base + 1
	if len(p.prefix) != 0 {
		pos += copy(dst[pos:], p.prefix)
	}
	if indentBytes > 0 {
		switch indent {
		case "  ", "    ", " ":
			for rem := indentBytes; rem > 0; {
				n := len(indentSpaces64)
				if rem < n {
					n = rem
				}
				copy(dst[pos:pos+n], indentSpaces64[:n])
				pos += n
				rem -= n
			}
		case "\t":
			for rem := indentBytes; rem > 0; {
				n := len(indentTabs64)
				if rem < n {
					n = rem
				}
				copy(dst[pos:pos+n], indentTabs64[:n])
				pos += n
				rem -= n
			}
		default:
			n := copy(dst[pos:], indent)
			for n < indentBytes {
				n += copy(dst[pos+n:], dst[pos:pos+n])
			}
		}
	}
	p.dst = dst
}

// value indents the value at the cursor, where depth is its own nesting level.
func (p *indentParser) value(depth int) error {
	if depth > p.maxDepth {
		return syntaxError(p.src, p.i, "maximum nesting depth exceeded")
	}
	if p.i >= len(p.src) {
		return syntaxError(p.src, p.i, "expected value")
	}
	switch p.src[p.i] {
	case 'n':
		return p.literal("null")
	case 't':
		return p.literal("true")
	case 'f':
		return p.literal("false")
	case '"':
		return p.string()
	case '[':
		return p.array(depth)
	case '{':
		return p.object(depth)
	default:
		if p.src[p.i] == '-' || isDigit(p.src[p.i]) {
			return p.number()
		}
		return syntaxError(p.src, p.i, "unexpected byte while parsing value")
	}
}

func (p *indentParser) literal(lit string) error {
	if !matchStringAt(p.src, p.i, lit) {
		return syntaxError(p.src, p.i, "invalid literal")
	}
	p.i += len(lit)
	p.dst = append(p.dst, lit...)
	return nil
}

func (p *indentParser) number() error {
	var err error
	p.dst, p.i, err = appendJSONNumberToken(p.dst, p.src, p.i, p.maxDepth)
	return err
}

func (p *indentParser) string() error {
	var err error
	p.dst, p.i, err = appendJSONStringToken(p.dst, p.src, p.i, p.maxDepth)
	return err
}

func (p *indentParser) array(depth int) error {
	if depth >= p.maxDepth {
		return syntaxError(p.src, p.i, "maximum nesting depth exceeded")
	}
	p.i++
	p.dst = append(p.dst, '[')
	p.skipSpace()
	if p.i < len(p.src) && p.src[p.i] == ']' {
		p.i++
		p.dst = append(p.dst, ']')
		return nil
	}
	for {
		p.newline(depth + 1)
		p.skipSpace()
		if err := p.value(depth + 1); err != nil {
			return err
		}
		p.skipSpace()
		if p.i >= len(p.src) {
			return syntaxError(p.src, p.i, "unterminated array")
		}
		switch p.src[p.i] {
		case ',':
			p.i++
			p.dst = append(p.dst, ',')
		case ']':
			p.i++
			p.newline(depth)
			p.dst = append(p.dst, ']')
			return nil
		default:
			return syntaxError(p.src, p.i, "expected comma or closing bracket in array")
		}
	}
}

func (p *indentParser) object(depth int) error {
	if depth >= p.maxDepth {
		return syntaxError(p.src, p.i, "maximum nesting depth exceeded")
	}
	p.i++
	p.dst = append(p.dst, '{')
	p.skipSpace()
	if p.i < len(p.src) && p.src[p.i] == '}' {
		p.i++
		p.dst = append(p.dst, '}')
		return nil
	}
	for {
		p.newline(depth + 1)
		p.skipSpace()
		if p.i >= len(p.src) || p.src[p.i] != '"' {
			return syntaxError(p.src, p.i, "expected object key string")
		}
		if err := p.string(); err != nil {
			return err
		}
		p.skipSpace()
		if p.i >= len(p.src) || p.src[p.i] != ':' {
			return syntaxError(p.src, p.i, "expected colon after object key")
		}
		p.i++
		p.dst = append(p.dst, ':', ' ')
		p.skipSpace()
		if err := p.value(depth + 1); err != nil {
			return err
		}
		p.skipSpace()
		if p.i >= len(p.src) {
			return syntaxError(p.src, p.i, "unterminated object")
		}
		switch p.src[p.i] {
		case ',':
			p.i++
			p.dst = append(p.dst, ',')
		case '}':
			p.i++
			p.newline(depth)
			p.dst = append(p.dst, '}')
			return nil
		default:
			return syntaxError(p.src, p.i, "expected comma or closing brace in object")
		}
	}
}
