package simdjson

// The one-pass builder behind dynamic decoding: Unmarshal into a *any and
// Decoder[any].Decode land in unmarshalAny, which turns a whole document
// into the standard Go JSON shapes without an intermediate tree.

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"math/bits"
	"strconv"
	"unsafe"
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
	var arena *anyValueArena
	if len(src) > 64 {
		arena = &anyValueArena{sourceSize: len(src)}
	}
	p := parser{src: src, maxDepth: opts.MaxDepth, zeroCopy: opts.ZeroCopy, anyArena: arena}
	if p.maxDepth <= 0 {
		p.maxDepth = defaultMaxDepth
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
		source := numberSourceOf(p.src)
		if digitStart <= len(p.src)-16 && isDigit(source.byteAt(digitStart+15)) && all16Digits(source.pointerAt(digitStart)) {
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
	source := numberSourceOf(p.src)
	digitStart := p.i
	if p.src[digitStart] == '-' {
		digitStart++
	}
	if end := digitStart + 16; end <= len(p.src) && all16Digits(source.pointerAt(digitStart)) &&
		(end == len(p.src) || isAnyArrayNumberEnd(source.byteAt(end))) {
		if closing := bytes.IndexByte(p.src[end:], ']'); closing >= 0 {
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
		if p.src[digitStart] == '-' {
			negative = true
			digitStart++
		}
		// A sixteen-digit run starting with '0' always has a leading zero and
		// must fall through to the checked scanner for rejection.
		if !useNumber && digitStart <= len(p.src)-16 && source.byteAt(digitStart) != '0' &&
			all16Digits(source.pointerAt(digitStart)) {
			end := digitStart + 16
			if end == len(p.src) || isAnyArrayNumberEnd(source.byteAt(end)) {
				integer := parse16Digits(source.pointerAt(digitStart))
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
	object := p.makeAnyMap()
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
			mask := stringSpecialMask(binary.LittleEndian.Uint64(p.src[keyStart:]))
			keyEnd := keyStart + bits.TrailingZeros64(mask)/8
			if mask != 0 && p.src[keyEnd] == '"' {
				key = p.string(keyStart, keyEnd)
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
			var text string
			if stringEnd < len(p.src) && p.src[stringEnd] == '"' {
				text = p.string(stringStart, stringEnd)
				p.i = stringEnd + 1
			} else {
				text, err = p.parseString()
				if err != nil {
					return nil, err
				}
			}
			value = p.boxAnyString(text, false)
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
	source := numberSourceOf(p.src)
	base := source.pointerAt(0)
	if useNumber {
		end, ok := scanNumberFast(base, len(p.src), start)
		if !ok {
			_, msg := scanNumber(p.src, start)
			return nil, p.err(start, msg)
		}
		p.i = end
		return p.boxAnyString(p.string(start, end), true), nil
	}
	end, integer, negative, isInteger, ok := scanAnyNumberFast(base, len(p.src), start)
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
	if n, exact := exactJSONFloat64(base, start, end); exact {
		return p.boxAnyFloat64(n), nil
	}
	if _, number, ok := scanJSONNumber(base, end, start); ok && !number.truncated {
		if n, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return p.boxAnyFloat64(n), nil
		}
	}
	text := source.stringRange(start, end)
	n, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, p.err(start, "number out of range")
	}
	return p.boxAnyFloat64(n), nil
}

func scanAnyNumberFast(base unsafe.Pointer, n, i int) (end int, integer uint64, negative, isInteger, ok bool) {
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
		if i >= n {
			return i, 0, negative, false, false
		}
	}
	isInteger = true
	if fastByteAt(base, i) == '0' {
		i++
	} else if isOneNine(fastByteAt(base, i)) {
		for ; i < n && isDigit(fastByteAt(base, i)); i++ {
			digit := uint64(fastByteAt(base, i) - '0')
			if isInteger && integer <= (^uint64(0)-digit)/10 {
				integer = integer*10 + digit
			} else {
				isInteger = false
			}
		}
	} else {
		return i, 0, negative, false, false
	}
	if i < n && fastByteAt(base, i) == '.' {
		isInteger = false
		i++
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, 0, negative, false, false
		}
		for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
		}
	}
	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		isInteger = false
		i++
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			i++
		}
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, 0, negative, false, false
		}
		for i++; i < n && isDigit(fastByteAt(base, i)); i++ {
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

// anyValueArena uses ordinary Go backing arrays to amortize storage for small
// nested arrays. Scalar interfaces are always constructed by ordinary Go
// conversions so the compiler and runtime own their layout and lifetime.
type anyValueArena struct {
	sourceSize int

	arrays   [][4]any
	arrayPos int
}

func boxedAnyBool(v bool) any {
	return v
}

// The boxAny helpers funnel every scalar the one-pass parser produces through
// ordinary conversions. Keeping the construction sites centralized makes the
// allocation contract easy to audit without assuming interface layout.

func (p *parser) boxAnyFloat64(v float64) any {
	return v
}

func (p *parser) boxAnyString(v string, number bool) any {
	if number {
		return json.Number(v)
	}
	return v
}

func (p *parser) boxAnySlice(v []any) any {
	return v
}

func (p *parser) makeAnyArrayValues(depth int) []any {
	if p.anyArena == nil {
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

func (p *parser) makeAnyMap() map[string]any {
	return make(map[string]any, 8)
}

func (a *anyValueArena) nextArray() *[4]any {
	if a.arrayPos == len(a.arrays) {
		chunkSize := a.sourceSize/128 + 8
		if chunkSize < 8 {
			chunkSize = 8
		} else if chunkSize > 2048 {
			chunkSize = 2048
		}
		a.arrays = make([][4]any, chunkSize)
		a.arrayPos = 0
	}
	slot := &a.arrays[a.arrayPos]
	a.arrayPos++
	return slot
}
