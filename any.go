package vibejson

// The one-pass builder behind dynamic decoding: Unmarshal into a *any and
// Decoder[any].Decode land in unmarshalAny, which turns a whole document
// into the standard Go JSON shapes without an intermediate tree.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math"
	"math/bits"
	"strconv"

	"github.com/thesyncim/vibejson/x/byteview"
	"github.com/thesyncim/vibejson/x/scanner"
)

// unmarshalAny is the whole-document engine behind dynamic decoding: one
// pass over src builds the standard Go JSON shapes — map[string]any, []any,
// string, float64 (json.Number under opts.UseNumber), bool, and nil — with
// no intermediate tree. [Unmarshal] and [Decoder.Decode] route top-level
// empty-interface destinations here whenever the existing value does not
// require encoding/json's decode-into-pointer merge; nested any fields
// decode mid-stream through decodeCompiledAny instead.
//
// The whole-document contract is what pays: because the value must span all
// of src, the parser can size the [4]any array arena from the source length.
// Only MaxDepth, ZeroCopy, and UseNumber apply from opts; the remaining
// DecoderOptions concern typed destinations and have nothing to act on in the
// dynamic shapes.
func unmarshalAny(src []byte, opts DecoderOptions) (any, error) {
	if len(src) > 256 {
		return unmarshalAnyLarge(src, opts)
	}
	p := parser{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth), zeroCopy: opts.ZeroCopy}
	if !opts.ZeroCopy {
		if capacity := ownedAnyStringCapacity(src, opts.UseNumber); capacity != 0 {
			p.strings = make([]byte, 0, capacity)
		}
	}
	p.skipSpace()
	v, err := p.parseAnyValue(0, opts.UseNumber)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.i != len(src) {
		return nil, p.err(p.i, "unexpected data after top-level value")
	}
	return v, nil
}

//go:noinline
func unmarshalAnyLarge(src []byte, opts DecoderOptions) (any, error) {
	p := parser{src: src, maxDepth: maxDepthOrDefault(opts.MaxDepth), zeroCopy: opts.ZeroCopy}
	var keyCache [64]string
	var scalarCache anyScalarCache
	p.scalarCache = &scalarCache
	if !opts.ZeroCopy {
		p.keyCache = &keyCache
		// Skip a full-document pre-scan on large inputs; seed a bounded
		// pointer-free arena and let p.arenaBlock() grow on demand.
		capHint := len(src) / 4
		if capHint > 16<<10 {
			capHint = 16 << 10
		}
		p.strings = make([]byte, 0, capHint+stringArenaHeadroom)
	}
	p.skipSpace()
	v, err := p.parseAnyValue(0, opts.UseNumber)
	if err != nil {
		return nil, err
	}
	p.skipSpace()
	if p.i != len(src) {
		return nil, p.err(p.i, "unexpected data after top-level value")
	}
	return v, nil
}

// ownedAnyStringCapacity computes a reservation for retained dynamic strings.
// Valid input gets one sizing pass instead of a geometric series of arena
// blocks; parseAnyValue remains authoritative when the scan stops on invalid
// syntax.
func ownedAnyStringCapacity(src []byte, useNumber bool) int {
	total := 0
	keyTotal := 0
	beforeStr := 0
	strStart := 0
	dedupeKeys := len(src) > 256
	const keyBudget = 2048
	// Mirror anyScalarCache's 32 slots with stack-only 64-bit fingerprints so
	// repeated clean string values do not over-reserve dead arena slack.
	var seenVal [32]uint64
	fastStrings := useAnyDelimiterSearch(src)
	for i := 0; i < len(src); {
		escaped := false
		if src[i] != '"' {
			if useNumber && (src[i] == '-' || isDigit(src[i])) {
				start := i
				for i < len(src) {
					switch src[i] {
					case '-', '+', '.', 'e', 'E', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
						i++
					default:
						total += i - start
						goto nextToken
					}
				}
				total += i - start
				continue
			}
			i++
			continue
		}
		i++
		strStart = i
		beforeStr = total
		for i < len(src) {
			if fastStrings && !escaped {
				// Skip long runs, then stay linear after the first escape.
				run := nextAnyStringDelimiter(src[i:])
				if run < 0 {
					total += len(src) - i
					return total
				}
				total += run
				i += run
			}
			switch src[i] {
			case '"':
				strEnd := i
				i++
				if dedupeKeys && !escaped {
					j := i
					for j < len(src) && src[j] <= ' ' {
						j++
					}
					if j < len(src) && src[j] == ':' {
						strLen := total - beforeStr
						if keyTotal >= keyBudget {
							total = beforeStr
						} else {
							keyTotal += strLen
						}
					} else if strLen := strEnd - strStart; strLen > 0 && strLen <= 128 {
						raw := src[strStart:strEnd]
						slot := (uint(raw[0]) ^ uint(raw[strLen-1])*31 ^ uint(strLen)*17) & 31
						fp := uint64(strLen)<<48 | uint64(raw[0])<<40 | uint64(raw[strLen>>1])<<32 | uint64(raw[strLen-1])<<24
						if strLen >= 8 {
							fp ^= binary.LittleEndian.Uint64(raw[:8]) ^ binary.LittleEndian.Uint64(raw[strLen-8:])
						}
						if fp == 0 {
							fp = 1
						}
						if seenVal[slot] == fp {
							total = beforeStr
						} else {
							seenVal[slot] = fp
						}
					}
				}
				goto nextToken
			case '\\':
				escaped = true
				i++
				if i >= len(src) {
					return total
				}
				if src[i] != 'u' {
					total++
					i++
					continue
				}
				if i > len(src)-5 {
					return total
				}
				value, ok := hex4(src, i+1)
				if !ok {
					// Three bytes is a safe UTF-8 bound for one malformed
					// \u escape; the parser will report the actual error.
					total += 3
					i += 5
					continue
				}
				i += 5
				r := rune(value)
				if 0xD800 <= r && r <= 0xDBFF && i <= len(src)-6 &&
					src[i] == '\\' && src[i+1] == 'u' {
					low, lowOK := hex4(src, i+2)
					if lowOK && 0xDC00 <= low && low <= 0xDFFF {
						total += 4
						i += 6
						continue
					}
				}
				switch {
				case r <= 0x7f:
					total++
				case r <= 0x7ff:
					total += 2
				default:
					total += 3
				}
			default:
				total++
				i++
			}
		}
	nextToken:
	}
	return total
}

func (p *parser) parseAnyValue(depth int, useNumber bool) (any, error) {
	if depth > p.maxDepth {
		return nil, p.err(p.i, "maximum nesting depth exceeded")
	}
	if p.i >= len(p.src) {
		return nil, p.err(p.i, "expected value")
	}
	switch p.src[p.i] {
	case 'n':
		if !literalNullAt(p.src, p.i) {
			return nil, p.err(p.i, "invalid literal")
		}
		p.i += 4
		return nil, nil
	case 't':
		if !literalTrueAt(p.src, p.i) {
			return nil, p.err(p.i, "invalid literal")
		}
		p.i += 4
		return boxedAnyBool(true), nil
	case 'f':
		if !literalFalseTailAt(p.src, p.i) {
			return nil, p.err(p.i, "invalid literal")
		}
		p.i += 5
		return boxedAnyBool(false), nil
	case '"':
		s, err := p.parseString()
		if err != nil {
			return nil, err
		}
		return p.boxAnyString(s, false), nil
	case '[':
		return p.parseAnyArray(depth+1, useNumber)
	case '{':
		return p.parseAnyObject(depth+1, useNumber)
	default:
		if p.src[p.i] == '-' || isDigit(p.src[p.i]) {
			return p.parseAnyNumber(useNumber)
		}
		return nil, p.err(p.i, "unexpected byte while parsing value")
	}
}

func (p *parser) parseAnyArray(depth int, useNumber bool) (any, error) {
	if depth > p.maxDepth {
		return nil, p.err(p.i, "maximum nesting depth exceeded")
	}
	p.i++
	p.skipSpace()
	if p.i < len(p.src) && p.src[p.i] == ']' {
		p.i++
		return boxedEmptyAnyValues, nil
	}
	if p.i < len(p.src) && (p.src[p.i] == '-' || isDigit(p.src[p.i])) {
		digitStart := p.i
		if p.src[digitStart] == '-' {
			digitStart++
		}
		if digitStart <= len(p.src)-16 && isDigit(p.src[digitStart+15]) && all16Digits(p.src[digitStart:digitStart+16]) {
			return p.parseAnyNumberArray(depth, useNumber)
		}
	}
	return p.parseAnyArrayValues(p.makeAnyArrayValues(depth), depth, useNumber)
}

func (p *parser) parseAnyArrayValues(values []any, depth int, useNumber bool) (any, error) {
	for {
		p.skipSpace()
		if p.i >= len(p.src) {
			return nil, p.err(p.i, "expected value")
		}
		var v any
		var err error
		switch p.src[p.i] {
		case 'n':
			if !literalNullAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 4
		case 't':
			if !literalTrueAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 4
			v = boxedAnyBool(true)
		case 'f':
			if !literalFalseTailAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 5
			v = boxedAnyBool(false)
		case '"':
			text, err := p.parseString()
			if err != nil {
				return nil, err
			}
			v = p.boxAnyString(text, false)
		case '[':
			v, err = p.parseAnyArray(depth+1, useNumber)
			if err != nil {
				return nil, err
			}
		case '{':
			v, err = p.parseAnyObject(depth+1, useNumber)
			if err != nil {
				return nil, err
			}
		default:
			if p.src[p.i] != '-' && !isDigit(p.src[p.i]) {
				return nil, p.err(p.i, "unexpected byte while parsing value")
			}
			if !useNumber && p.src[p.i] != '-' {
				start := p.i
				end := start + 1
				n := uint64(p.src[start] - '0')
				if p.src[start] != '0' {
					limit := start + 15
					if limit > len(p.src) {
						limit = len(p.src)
					}
					for end < limit && isDigit(p.src[end]) {
						n = n*10 + uint64(p.src[end]-'0')
						end++
					}
				}
				if (end == len(p.src) || isAnyNumberEnd(p.src[end])) &&
					(p.src[start] != '0' || end == start+1) {
					p.i = end
					v = p.boxAnyFloat64(float64(n))
					break
				}
			}
			if !useNumber {
				start := p.i
				negative := false
				if p.src[start] == '-' {
					negative = true
					start++
				}
				if start+2 < len(p.src) && isDigit(p.src[start]) {
					end := start + 3
					if p.src[start+1] == '.' && isDigit(p.src[start+2]) &&
						(end == len(p.src) || isAnyNumberEnd(p.src[end])) {
						n := float64(10*int(p.src[start]-'0')+int(p.src[start+2]-'0')) / 10
						if negative {
							n = -n
						}
						p.i = end
						v = p.boxAnyFloat64(n)
						break
					}
					if p.src[start+1] == 'e' || p.src[start+1] == 'E' {
						exponent := int(p.src[start+2] - '0')
						if isDigit(p.src[start+2]) && exponent <= 15 &&
							(end == len(p.src) || isAnyNumberEnd(p.src[end])) {
							n := float64(p.src[start]-'0') * anyPow10[exponent]
							if negative {
								n = -n
							}
							p.i = end
							v = p.boxAnyFloat64(n)
							break
						}
					}
				}
			}
			v, err = p.parseAnyNumber(useNumber)
			if err != nil {
				return nil, err
			}
		}
		values = append(values, v)
		p.skipSpace()
		if p.i >= len(p.src) {
			return nil, p.err(p.i, "unterminated array")
		}
		switch p.src[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return p.boxAnySlice(values), nil
		default:
			return nil, p.err(p.i, "expected comma or closing bracket in array")
		}
	}
}

func (p *parser) parseAnyNumberArray(depth int, useNumber bool) (any, error) {
	capacity := 4
	src := p.src
	digitStart := p.i
	if src[digitStart] == '-' {
		digitStart++
	}
	if end := digitStart + 16; end <= len(src) && all16Digits(src[digitStart:end]) &&
		(end == len(src) || isAnyArrayNumberEnd(src[end])) {
		if closing := bytes.IndexByte(src[end:], ']'); closing >= 0 {
			width := end - p.i
			capacity = 2 + closing/(width+1)
			if capacity < 4 {
				capacity = 4
			} else if capacity > 1024 {
				capacity = 1024
			}
		}
	}
	values := make([]any, 0, capacity)

	for {
		var value any
		fast := false
		digitStart = p.i
		negative := false
		if src[digitStart] == '-' {
			negative = true
			digitStart++
		}
		// A sixteen-digit run starting with '0' always has a leading zero and
		// must fall through to the checked scanner for rejection.
		if !useNumber && digitStart <= len(src)-16 && src[digitStart] != '0' &&
			all16Digits(src[digitStart:digitStart+16]) {
			end := digitStart + 16
			if end == len(src) || isAnyArrayNumberEnd(src[end]) {
				integer := parse16Digits(src[digitStart:end])
				if integer <= uint64(1)<<53 {
					float := float64(integer)
					if negative {
						float = -float
					}
					p.i = end
					value = p.boxAnyFloat64(float)
					fast = true
				}
			}
		}
		if !fast {
			var err error
			value, err = p.parseAnyNumber(useNumber)
			if err != nil {
				return nil, err
			}
		}
		values = append(values, value)

		p.skipSpace()
		if p.i >= len(p.src) {
			return nil, p.err(p.i, "unterminated array")
		}
		switch p.src[p.i] {
		case ']':
			p.i++
			return p.boxAnySlice(values), nil
		case ',':
			p.i++
		default:
			return nil, p.err(p.i, "expected comma or closing bracket in array")
		}
		p.skipSpace()
		if p.i < len(p.src) && (p.src[p.i] == '-' || isDigit(p.src[p.i])) {
			continue
		}
		return p.parseAnyArrayValues(values, depth, useNumber)
	}
}

func isAnyArrayNumberEnd(c byte) bool {
	switch c {
	case ',', ']', ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func isAnyNumberEnd(c byte) bool {
	switch c {
	case ',', ']', '}', ' ', '\t', '\r', '\n':
		return true
	default:
		return false
	}
}

func (p *parser) parseAnyObject(depth int, useNumber bool) (any, error) {
	if depth > p.maxDepth {
		return nil, p.err(p.i, "maximum nesting depth exceeded")
	}
	p.i++
	p.skipSpace()
	object := p.makeAnyMap(depth)
	if p.i < len(p.src) && p.src[p.i] == '}' {
		p.i++
		return object, nil
	}

	for {
		p.skipSpace()
		if p.i >= len(p.src) || p.src[p.i] != '"' {
			return nil, p.err(p.i, "expected object key string")
		}
		keyStart := p.i + 1
		var key string
		var err error
		if keyStart+8 <= len(p.src) {
			word := binary.LittleEndian.Uint64(p.src[keyStart:])
			mask := stringSpecialMask(word)
			keyEnd := keyStart + bits.TrailingZeros64(mask)/8
			if mask != 0 && p.src[keyEnd] == '"' {
				if p.keyCache != nil {
					raw := p.src[keyStart:keyEnd]
					slot := ((word ^ (word >> 17)) + uint64(len(raw))*31) & 63
					if cached := p.keyCache[slot]; len(cached) == len(raw) && cached == byteview.String(raw) {
						key = cached
					} else {
						key = p.string(keyStart, keyEnd)
						p.keyCache[slot] = key
					}
				} else {
					key = p.string(keyStart, keyEnd)
				}
				p.i = keyEnd + 1
			} else {
				key, err = p.parseString()
				if err != nil {
					return nil, err
				}
			}
		} else {
			key, err = p.parseString()
			if err != nil {
				return nil, err
			}
		}
		p.skipSpace()
		if p.i >= len(p.src) || p.src[p.i] != ':' {
			return nil, p.err(p.i, "expected colon after object key")
		}
		p.i++
		p.skipSpace()
		if p.i >= len(p.src) {
			return nil, p.err(p.i, "expected value")
		}
		var value any
		switch p.src[p.i] {
		case 'n':
			if !literalNullAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 4
		case 't':
			if !literalTrueAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 4
			value = boxedAnyBool(true)
		case 'f':
			if !literalFalseTailAt(p.src, p.i) {
				return nil, p.err(p.i, "invalid literal")
			}
			p.i += 5
			value = boxedAnyBool(false)
		case '"':
			stringStart := p.i + 1
			stringEnd := scanStringSpecial(p.src, stringStart)
			if stringEnd < len(p.src) && p.src[stringEnd] == '"' {
				p.i = stringEnd + 1
				value = p.boxAnyCleanString(stringStart, stringEnd)
			} else {
				var text string
				text, err = p.parseString()
				if err != nil {
					return nil, err
				}
				value = p.boxAnyString(text, false)
			}
		case '[':
			value, err = p.parseAnyArray(depth+1, useNumber)
			if err != nil {
				return nil, err
			}
		case '{':
			value, err = p.parseAnyObject(depth+1, useNumber)
			if err != nil {
				return nil, err
			}
		default:
			if p.src[p.i] != '-' && !isDigit(p.src[p.i]) {
				return nil, p.err(p.i, "unexpected byte while parsing value")
			}
			if !useNumber && p.src[p.i] != '-' {
				start := p.i
				end := start + 1
				n := uint64(p.src[start] - '0')
				if p.src[start] != '0' {
					limit := start + 15
					if limit > len(p.src) {
						limit = len(p.src)
					}
					for end < limit && isDigit(p.src[end]) {
						n = n*10 + uint64(p.src[end]-'0')
						end++
					}
				}
				if (end == len(p.src) || isAnyNumberEnd(p.src[end])) &&
					(p.src[start] != '0' || end == start+1) {
					p.i = end
					value = p.boxAnyFloat64(float64(n))
					break
				}
			}
			value, err = p.parseAnyNumber(useNumber)
			if err != nil {
				return nil, err
			}
		}
		object[key] = value
		p.skipSpace()
		if p.i >= len(p.src) {
			return nil, p.err(p.i, "unterminated object")
		}
		switch p.src[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return object, nil
		default:
			return nil, p.err(p.i, "expected comma or closing brace in object")
		}
	}
}

func (p *parser) parseAnyNumber(useNumber bool) (any, error) {
	start := p.i
	if useNumber {
		end, ok := scanNumberFast(p.src, start)
		if !ok {
			_, msg := scanNumber(p.src, start)
			return nil, p.err(start, msg)
		}
		p.i = end
		return p.boxAnyString(p.string(start, end), true), nil
	}
	end, integer, negative, isInteger, ok := scanAnyNumberFast(p.src, start)
	if !ok {
		_, msg := scanNumber(p.src, start)
		return nil, p.err(start, msg)
	}
	p.i = end
	if isInteger && integer <= uint64(1)<<53 {
		n := float64(integer)
		if negative {
			n = -n
		}
		return p.boxAnyFloat64(n), nil
	}
	if n, exact := exactJSONFloat64(p.src, start, end); exact {
		return p.boxAnyFloat64(n), nil
	}
	if _, number, ok := scanJSONNumber(p.src[:end], start); ok && !number.truncated {
		if n, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return p.boxAnyFloat64(n), nil
		}
	}
	text := byteview.String(p.src[start:end])
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, p.err(start, "number out of range")
	}
	return p.boxAnyFloat64(n), nil
}

func scanAnyNumberFast(src []byte, i int) (end int, integer uint64, negative, isInteger, ok bool) {
	n := len(src)
	if src[i] == '-' {
		negative = true
		i++
		if i >= n {
			return i, 0, negative, false, false
		}
	}
	isInteger = true
	if src[i] == '0' {
		i++
	} else if isOneNine(src[i]) {
		for ; i < n && isDigit(src[i]); i++ {
			digit := uint64(src[i] - '0')
			if isInteger && integer <= (^uint64(0)-digit)/10 {
				integer = integer*10 + digit
			} else {
				isInteger = false
			}
		}
	} else {
		return i, 0, negative, false, false
	}
	if i < n && src[i] == '.' {
		isInteger = false
		i++
		if i >= n || !isDigit(src[i]) {
			return i, 0, negative, false, false
		}
		for i++; i < n && isDigit(src[i]); i++ {
		}
	}
	if i < n && (src[i] == 'e' || src[i] == 'E') {
		isInteger = false
		i++
		if i < n && (src[i] == '+' || src[i] == '-') {
			i++
		}
		if i >= n || !isDigit(src[i]) {
			return i, 0, negative, false, false
		}
		for i++; i < n && isDigit(src[i]); i++ {
		}
	}
	return i, integer, negative, isInteger, true
}

var anyPow10 = [...]float64{
	1, 1e1, 1e2, 1e3, 1e4, 1e5, 1e6, 1e7, 1e8, 1e9, 1e10,
	1e11, 1e12, 1e13, 1e14, 1e15, 1e16, 1e17, 1e18, 1e19, 1e20,
	1e21, 1e22,
}

// boxedEmptyAnyValues is the one boxed empty array every empty JSON array
// decodes to. The sharing is unobservable: the slice has no elements to
// mutate, an append copies out of the zero-capacity backing, and interface
// comparison of slice values panics regardless of identity. It saves the
// interface header allocation the conversion would otherwise pay per empty
// array.
var boxedEmptyAnyValues any = []any{}

var boxedEmptyString any = ""

// boxedSmallFloat64 caches ordinary runtime-boxed float64 interface values for
// common small non-negative integers [0, 1024]. Interface values holding
// float64 are immutable and compared by value, so sharing them is unobservable
// and eliminates heap allocations and GC pressure for common numeric fields.
var boxedSmallFloat64 = func() [1025]any {
	var table [1025]any
	for i := range table {
		table[i] = float64(i)
	}
	return table
}()

// anyValueArena uses ordinary Go backing arrays to amortize storage for small
// nested arrays. Scalar interfaces are always constructed by ordinary Go
// conversions so the compiler and runtime own their layout and lifetime.
type anyValueArena struct {
	arrays   [][4]any
	arrayPos int
}

func boxedAnyBool(v bool) any {
	return v
}

// The boxAny helpers funnel every scalar the one-pass parser produces through
// ordinary conversions. Keeping the construction sites centralized makes the
// allocation contract easy to audit without assuming interface layout.

// anyScalarCache is a stack-resident direct-mapped cache of ordinary Go boxed
// immutable scalar values (string and float64) for large dynamic unmarshals.
// Sharing boxed immutable scalars is unobservable and avoids repeated heap
// interface boxing and duplicate string-arena copies.
type anyScalarCache struct {
	strs     [32]string
	strBox   [32]any
	floats   [16]uint64
	floatBox [16]any
}

func (p *parser) boxAnyFloat64(v float64) any {
	if v >= 0 && v <= 1024 {
		if i := int(v); float64(i) == v && (v != 0 || !math.Signbit(v)) {
			return boxedSmallFloat64[i]
		}
	}
	if c := p.scalarCache; c != nil {
		b := math.Float64bits(v)
		slot := (b ^ (b >> 32) ^ (b >> 48)) & 15
		if c.floatBox[slot] != nil && c.floats[slot] == b {
			return c.floatBox[slot]
		}
		boxed := any(v)
		c.floats[slot] = b
		c.floatBox[slot] = boxed
		return boxed
	}
	return v
}

func (p *parser) boxAnyCleanString(start, end int) any {
	if start == end {
		return boxedEmptyString
	}
	raw := p.src[start:end]
	if c := p.scalarCache; c != nil && len(raw) <= 128 {
		slot := (uint(raw[0]) ^ uint(raw[len(raw)-1])*31 ^ uint(len(raw))*17) & 31
		if cached := c.strs[slot]; len(cached) == len(raw) && cached == byteview.String(raw) {
			return c.strBox[slot]
		}
		text := p.string(start, end)
		boxed := any(text)
		c.strs[slot] = text
		c.strBox[slot] = boxed
		return boxed
	}
	return p.boxAnyString(p.string(start, end), false)
}

func (p *parser) boxAnyString(v string, number bool) any {
	if number {
		return json.Number(v)
	}
	if len(v) == 0 {
		return boxedEmptyString
	}
	return v
}

func (p *parser) boxAnySlice(v []any) any {
	return v
}

func (p *parser) makeAnyArrayValues(depth int) []any {
	if p.anyArena.arrays == nil && len(p.src) <= 64 {
		return make([]any, 0, 4)
	}
	if depth <= 2 && p.i < len(p.src) && p.src[p.i] == '{' {
		capacity := (len(p.src) - p.i) / 128
		if capacity > 4 {
			if capacity > 1024 {
				capacity = 1024
			}
			return make([]any, 0, capacity)
		}
	}
	slot := p.anyArena.nextArray()
	return slot[:0]
}

func (p *parser) makeAnyMap(depth int) map[string]any {
	const defaultCapacity = 8
	// Small whole-document objects are cheap to size exactly and otherwise
	// pay several retained bucket-growth allocations. Restrict the lookahead
	// to the root and 64 KiB so deeply nested or very large documents never
	// acquire quadratic scanning cost.
	if depth == 1 && len(p.src)-p.i <= 64<<10 {
		if capacity, ok := rootAnyObjectCapacity(p.src, p.i); ok {
			return make(map[string]any, capacity)
		}
	}
	return make(map[string]any, defaultCapacity)
}

func rootAnyObjectCapacity(src []byte, start int) (int, bool) {
	members := 0
	nesting := 0
	fastStrings := useAnyDelimiterSearch(src[start:])
	for i := start; i < len(src); i++ {
		switch src[i] {
		case '"':
			closed := false
			escaped := false
			for i++; i < len(src); {
				if fastStrings && !escaped {
					if next := nextAnyStringDelimiter(src[i:]); next >= 0 {
						i += next
					} else {
						return 0, false
					}
				}
				if src[i] == '"' {
					closed = true
					break
				}
				if src[i] == '\\' {
					escaped = true
					i += 2
				} else {
					i++
				}
			}
			if !closed {
				return 0, false
			}
		case '{', '[':
			nesting++
		case '}', ']':
			if nesting == 0 {
				return members, src[i] == '}'
			}
			nesting--
		case ':':
			if nesting == 0 {
				members++
			}
		}
	}
	return 0, false
}

// A small sample avoids the delimiter-search setup on escape-heavy text.
// This only selects a scan strategy; both paths count the same bytes.
func useAnyDelimiterSearch(src []byte) bool {
	sample := src[:min(len(src), 1024)]
	return bytes.Count(sample, []byte{'\\'}) <= len(sample)/32
}

func nextAnyStringDelimiter(src []byte) int {
	if idx := scanner.IndexStringSyntax(src, 0); idx < len(src) {
		return idx
	}
	return -1
}

func (a *anyValueArena) nextArray() *[4]any {
	if a.arrayPos == len(a.arrays) {
		// Start small so one short array in a large document does not retain a
		// source-sized guess. Later fixed chunks cap unused tail storage while
		// adding only a handful of allocations even to array-heavy documents.
		chunkSize := 16
		if a.arrays != nil {
			chunkSize = 256
		}
		a.arrays = make([][4]any, chunkSize)
		a.arrayPos = 0
	}
	slot := &a.arrays[a.arrayPos]
	a.arrayPos++
	return slot
}
