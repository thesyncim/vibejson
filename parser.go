package vibejson

import (
	"encoding/binary"
	"sync"
	"unicode/utf16"

	"github.com/thesyncim/vibejson/document"
)

// DefaultMaxDepth is the nesting limit used when an option's MaxDepth is not
// positive.
const DefaultMaxDepth = 10000

func maxDepthOrDefault(maxDepth int) int {
	if maxDepth <= 0 {
		return DefaultMaxDepth
	}
	return maxDepth
}

const escapedUnicodePrefixLE = uint16('\\') | uint16('u')<<8

// Options configures parser limits.
type Options struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int

	// ZeroCopy reuses src storage for unescaped strings and numbers.
	// Callers must not mutate src while returned values are used. When false,
	// retained text is independent of src.
	ZeroCopy bool
}

// Parse validates src and returns an ordered, owning Value for lazy navigation.
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

// ParseOptions parses src using opts and returns its root Value.
func ParseOptions(src []byte, opts Options) (Value, error) {
	maxDepth := maxDepthOrDefault(opts.MaxDepth)

	pooled := parseTapePool.Get().(*[]IndexEntry)
	storage := (*pooled)[:cap(*pooled)]

	estimate := len(src)/8 + 8
	var entries []IndexEntry
	grown := false
	for {
		if cap(storage) < estimate {
			storage = make([]IndexEntry, 0, estimate)
			grown = true
		}
		index, err := BuildIndexOptions(src, storage[:cap(storage)], document.IndexOptions{MaxDepth: maxDepth})
		if err == document.ErrIndexFull {
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
		entries = index.Entries
		break
	}

	if len(entries) == 0 {
		if !grown {
			*pooled = storage[:0]
		}
		parseTapePool.Put(pooled)
		return Value{}, syntaxError(src, 0, "expected value")
	}

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

type parser struct {
	src      []byte
	i        int
	maxDepth int
	zeroCopy bool
	strings  []byte
	anyArena *anyValueArena
}

func (p *parser) err(off int, msg string) error {
	return syntaxError(p.src, off, msg)
}

func (p *parser) skipSpace() {
	p.i = SkipSpace(p.src, p.i)
}

func (p *parser) arenaBlock() []byte {
	if p.strings == nil {
		capacity := stringArenaSeed
		if capacity > len(p.src) {
			capacity = len(p.src) + 1
		}
		p.strings = make([]byte, 0, capacity)
	} else if cap(p.strings) >= stringArenaHeadroom && cap(p.strings)-len(p.strings) < stringArenaHeadroom {
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
			var err error
			if out, err = p.appendUnicodeEscapeRun(out); err != nil {
				return "", err
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
			return OwnedBytesString(out[outStart:]), nil
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

func (p *parser) appendUnicodeEscapeRun(out []byte) ([]byte, error) {
	for p.i+8 <= len(p.src) {
		w := binary.LittleEndian.Uint64(p.src[p.i:])
		if uint16(w) != escapedUnicodePrefixLE {
			break
		}
		a := hexNibbleTable[byte(w>>16)]
		b := hexNibbleTable[byte(w>>24)]
		c := hexNibbleTable[byte(w>>32)]
		d := hexNibbleTable[byte(w>>40)]
		if a|b|c|d >= 0x10 {
			return out, p.err(p.i, "invalid unicode escape")
		}
		u := uint32(a)<<12 | uint32(b)<<8 | uint32(c)<<4 | uint32(d)
		if u-0xD800 < 0x800 {
			break
		}
		switch {
		case u < 0x80:
			out = append(out, byte(u))
		case u < 0x800:
			out = append(out, 0xc0|byte(u>>6), 0x80|byte(u)&0x3f)
		default:
			out = append(out, 0xe0|byte(u>>12), 0x80|byte(u>>6)&0x3f, 0x80|byte(u)&0x3f)
		}
		p.i += 6
	}
	for p.i+6 <= len(p.src) && p.src[p.i] == '\\' && p.src[p.i+1] == 'u' {
		escapeStart := p.i
		u, ok := hex4(p.src, p.i+2)
		if !ok {
			return out, p.err(escapeStart, "invalid unicode escape")
		}
		p.i += 6
		r := rune(u)
		switch {
		case 0xD800 <= r && r <= 0xDBFF:
			if p.i+6 > len(p.src) || p.src[p.i] != '\\' || p.src[p.i+1] != 'u' {
				return out, p.err(escapeStart, "missing low surrogate")
			}
			lo, ok := hex4(p.src, p.i+2)
			if !ok || lo < 0xDC00 || lo > 0xDFFF {
				return out, p.err(escapeStart, "invalid low surrogate")
			}
			p.i += 6
			r = utf16.DecodeRune(r, rune(lo))
		case 0xDC00 <= r && r <= 0xDFFF:
			return out, p.err(escapeStart, "unexpected low surrogate")
		}
		out = appendEscapedRune(out, r)
	}
	return out, nil
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

// IsDigit reports whether c is an ASCII decimal digit.
func IsDigit(c byte) bool {
	return '0' <= c && c <= '9'
}

func isOneNine(c byte) bool {
	return '1' <= c && c <= '9'
}
