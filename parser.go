package simdjson

import (
	"sync"
	"unicode/utf16"
)

const defaultMaxDepth = 10000

// Options configures parser limits.
type Options struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int

	// ZeroCopy reuses src storage for unescaped strings and numbers.
	// Callers must not mutate src for as long as the returned Value is used.
	// When false, results are independent of src: decoded strings alias at
	// most one private copy of the input, so retaining any decoded string
	// retains that copy.
	ZeroCopy bool
}

// Parse parses src into an ordered JSON AST.
func Parse(src []byte) (Value, error) {
	return ParseOptions(src, Options{})
}

// parseTapePool recycles tape storage between ParseOptions calls; the tape
// is consumed before the call returns and never escapes.
var parseTapePool = sync.Pool{
	New: func() any {
		storage := make([]IndexEntry, 0, 1024)
		return &storage
	},
}

// ParseOptions parses src using opts and returns the document's root Value. It
// builds only the structural index; each value is read from that index straight
// off the source as the caller navigates, so a document read in part never pays
// to materialize the whole. The returned Value owns its index and (unless
// opts.ZeroCopy) a private copy of src, so it stays valid after the caller
// drops src.
func ParseOptions(src []byte, opts Options) (Value, error) {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}

	// The index needs one entry per structural token. Reuse a pooled estimate
	// buffer for the common case; grow (and keep) a private buffer when the
	// document is larger than the estimate.
	pooled := parseTapePool.Get().(*[]IndexEntry)
	storage := (*pooled)[:cap(*pooled)]

	estimate := len(src)/8 + 8
	var entries []IndexEntry
	grown := false
	for {
		if cap(storage) < estimate {
			// The builder writes entries into storage[:cap] from index 0, so only
			// the capacity matters; allocate with zero length to skip zeroing the
			// whole estimate, which the builder immediately overwrites.
			storage = make([]IndexEntry, 0, estimate)
			grown = true
		}
		index, err := BuildIndexOptions(src, storage[:cap(storage)], IndexOptions{MaxDepth: maxDepth})
		if err == ErrIndexFull {
			estimate = cap(storage) * 2
			continue
		}
		if err != nil {
			if !grown {
				*pooled = storage[:0]
			}
			parseTapePool.Put(pooled)
			return Value{}, err
		}
		entries = index.entries
		break
	}

	if len(entries) == 0 {
		if !grown {
			*pooled = storage[:0]
		}
		parseTapePool.Put(pooled)
		return Value{}, syntaxError(src, 0, "expected value")
	}

	// The Value must own its index storage so it outlives this call. When the
	// pooled buffer was large enough we copy the used entries out and return the
	// buffer to the pool; a grown buffer is already private and belongs to the
	// Value, so we trim it and do not recycle it.
	var owned []IndexEntry
	if grown {
		owned = storage[:len(entries):len(entries)]
	} else {
		owned = make([]IndexEntry, len(entries))
		copy(owned, entries)
		*pooled = storage[:0]
	}
	parseTapePool.Put(pooled)

	body := src
	if !opts.ZeroCopy {
		body = append([]byte(nil), src...)
	}
	return newRootValue(body, owned), nil
}

// parser holds shared low-level scanning state for the dynamic decoding
// paths and the typed decoder's slow paths.
type parser struct {
	src      []byte
	i        int
	maxDepth int
	zeroCopy bool
	ownedSrc []byte
	strings  []byte
	anyArena *anyValueArena
}

func (p *parser) err(off int, msg string) error {
	return syntaxError(p.src, off, msg)
}

func (p *parser) skipSpace() {
	p.i = skipSpace(p.src, p.i)
}

// arenaBlock returns the string arena positioned for a new escaped string,
// starting a fresh block of twice the size when the current one is nearly
// full. Blocks are never copied: strings already handed out keep their block
// alive, so unescaped content is written exactly once regardless of how much
// of the document is escaped.
func (p *parser) arenaBlock() []byte {
	if p.strings == nil {
		capacity := stringArenaSeed
		if capacity > len(p.src) {
			capacity = len(p.src) + 1
		}
		p.strings = make([]byte, 0, capacity)
	} else if cap(p.strings)-len(p.strings) < stringArenaHeadroom {
		p.strings = make([]byte, 0, 2*cap(p.strings))
	}
	return p.strings
}

func (p *parser) parseString() (string, error) {
	p.i++
	start := p.i
	chunkStart := start
	var out []byte
	outStart := -1

	for {
		if p.i+6 <= len(p.src) && p.src[p.i] == '\\' && p.src[p.i+1] == 'u' {
			if outStart < 0 {
				out = p.arenaBlock()
				outStart = len(out)
			}
			out = append(out, p.src[chunkStart:p.i]...)
			for p.i+6 <= len(p.src) && p.src[p.i] == '\\' && p.src[p.i+1] == 'u' {
				escapeStart := p.i
				u, ok := hex4(p.src, p.i+2)
				if !ok {
					return "", p.err(escapeStart, "invalid unicode escape")
				}
				p.i += 6
				r := rune(u)
				switch {
				case 0xD800 <= r && r <= 0xDBFF:
					if p.i+6 > len(p.src) || p.src[p.i] != '\\' || p.src[p.i+1] != 'u' {
						return "", p.err(escapeStart, "missing low surrogate")
					}
					lo, ok := hex4(p.src, p.i+2)
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return "", p.err(escapeStart, "invalid low surrogate")
					}
					p.i += 6
					r = utf16.DecodeRune(r, rune(lo))
				case 0xDC00 <= r && r <= 0xDFFF:
					return "", p.err(escapeStart, "unexpected low surrogate")
				}
				out = appendEscapedRune(out, r)
			}
			chunkStart = p.i
			continue
		}
		j := p.i
		if j >= len(p.src) || p.src[j] != '\\' {
			j = scanStringSpecial(p.src, j)
		}
		if j >= len(p.src) {
			return "", p.err(len(p.src), "unterminated string")
		}
		p.i = j
		c := p.src[p.i]
		switch {
		case c == '"':
			if outStart < 0 {
				s := p.string(start, p.i)
				p.i++
				return s, nil
			}
			out = append(out, p.src[chunkStart:p.i]...)
			p.strings = out
			p.i++
			return ownedBytesString(out[outStart:]), nil
		case c == '\\':
			if outStart < 0 {
				out = p.arenaBlock()
				outStart = len(out)
			}
			out = append(out, p.src[chunkStart:p.i]...)
			p.i++
			if p.i >= len(p.src) {
				return "", p.err(p.i, "unterminated escape sequence")
			}
			if err := p.appendEscape(&out); err != nil {
				return "", err
			}
			chunkStart = p.i
		case c < 0x20:
			return "", p.err(p.i, "unescaped control byte in string")
		default:
			next, bad := scanStringUnicodeRun(p.src, p.i)
			if bad >= 0 {
				return "", p.err(bad, "invalid UTF-8 in string")
			}
			p.i = next
		}
	}
}

func (p *parser) appendEscape(out *[]byte) error {
	switch p.src[p.i] {
	case '"', '\\', '/':
		*out = append(*out, p.src[p.i])
		p.i++
		return nil
	case 'b':
		*out = append(*out, '\b')
		p.i++
		return nil
	case 'f':
		*out = append(*out, '\f')
		p.i++
		return nil
	case 'n':
		*out = append(*out, '\n')
		p.i++
		return nil
	case 'r':
		*out = append(*out, '\r')
		p.i++
		return nil
	case 't':
		*out = append(*out, '\t')
		p.i++
		return nil
	case 'u':
		r, err := p.parseUnicodeEscape()
		if err != nil {
			return err
		}
		*out = appendEscapedRune(*out, r)
		return nil
	default:
		return p.err(p.i-1, "invalid escape sequence")
	}
}

func (p *parser) parseUnicodeEscape() (rune, error) {
	start := p.i - 1
	p.i++
	u, ok := hex4(p.src, p.i)
	if !ok {
		return 0, p.err(start, "invalid unicode escape")
	}
	p.i += 4
	r := rune(u)
	if 0xD800 <= r && r <= 0xDBFF {
		if p.i+6 > len(p.src) || p.src[p.i] != '\\' || p.src[p.i+1] != 'u' {
			return 0, p.err(start, "missing low surrogate")
		}
		p.i += 2
		lo, ok := hex4(p.src, p.i)
		if !ok {
			return 0, p.err(start, "invalid low surrogate")
		}
		p.i += 4
		lor := rune(lo)
		if lor < 0xDC00 || lor > 0xDFFF {
			return 0, p.err(start, "invalid low surrogate")
		}
		return utf16.DecodeRune(r, lor), nil
	}
	if 0xDC00 <= r && r <= 0xDFFF {
		return 0, p.err(start, "unexpected low surrogate")
	}
	return r, nil
}

func hex4(src []byte, i int) (uint16, bool) {
	if i+4 > len(src) {
		return 0, false
	}
	a := hexNibbleTable[src[i]]
	b := hexNibbleTable[src[i+1]]
	c := hexNibbleTable[src[i+2]]
	d := hexNibbleTable[src[i+3]]
	return uint16(a)<<12 | uint16(b)<<8 | uint16(c)<<4 | uint16(d), a|b|c|d < 0x10
}

var hexNibbleTable = func() [256]byte {
	var table [256]byte
	for i := range table {
		table[i] = 0xff
	}
	for c := byte('0'); c <= '9'; c++ {
		table[c] = c - '0'
	}
	for c := byte('a'); c <= 'f'; c++ {
		table[c] = c - 'a' + 10
		table[c-'a'+'A'] = c - 'a' + 10
	}
	return table
}()

func appendEscapedRune(dst []byte, r rune) []byte {
	switch {
	case r <= 0x7f:
		return append(dst, byte(r))
	case r <= 0x7ff:
		return append(dst, 0xc0|byte(r>>6), 0x80|byte(r)&0x3f)
	case r <= 0xffff:
		return append(dst, 0xe0|byte(r>>12), 0x80|byte(r>>6)&0x3f, 0x80|byte(r)&0x3f)
	default:
		return append(dst, 0xf0|byte(r>>18), 0x80|byte(r>>12)&0x3f, 0x80|byte(r>>6)&0x3f, 0x80|byte(r)&0x3f)
	}
}

func isDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

func isOneNine(c byte) bool {
	return '1' <= c && c <= '9'
}
