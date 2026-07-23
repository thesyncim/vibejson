package slopjson

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
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
	startLen := len(dst)
	p := indentParser{src: src, dst: dst, maxDepth: maxDepth, prefix: prefix, indent: indent}
	p.skipSpace()
	if err := p.value(0); err != nil {
		return dst[:startLen], err
	}
	p.skipSpace()
	if p.i != len(src) {
		return dst[:startLen], syntaxError(src, p.i, "unexpected data after top-level value")
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
	p.dst = append(p.dst, '\n')
	p.dst = append(p.dst, p.prefix...)
	for k := 0; k < depth; k++ {
		p.dst = append(p.dst, p.indent...)
	}
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
