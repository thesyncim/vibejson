// Package orderedkey encodes typed JSON scalars so ordinary byte comparison is
// semantic query order. It is the shared storage primitive for ordered
// secondary indexes; readable query syntax and transport framing deliberately
// live elsewhere.
package orderedkey

import (
	"encoding/binary"
	"math"
	"unicode/utf16"
	"unicode/utf8"
)

// Direction selects one component's index order.
type Direction uint8

const (
	Ascending Direction = iota
	Descending
)

// Type tags leave intentional gaps. A future ordered scalar family can be
// assigned between existing families without renumbering every persisted key.
const (
	tagNull byte = 0x10

	tagFalse byte = 0x20
	tagTrue  byte = 0x21

	tagNumber         byte = 0x30
	tagNumberNegative byte = 0x10
	tagNumberZero     byte = 0x20
	tagNumberPositive byte = 0x30

	tagString byte = 0x40
)

// AppendNull appends one prefix-free null component.
func AppendNull(dst []byte, direction Direction) ([]byte, bool) {
	if direction > Descending {
		return dst, false
	}
	start := len(dst)
	dst = append(dst, tagNull)
	invertDescending(dst[start:], direction)
	return dst, true
}

// AppendBool appends one prefix-free Boolean component.
func AppendBool(dst []byte, value bool, direction Direction) ([]byte, bool) {
	if direction > Descending {
		return dst, false
	}
	start := len(dst)
	tag := byte(tagFalse)
	if value {
		tag = tagTrue
	}
	dst = append(dst, tag)
	invertDescending(dst[start:], direction)
	return dst, true
}

// AppendNumber canonicalizes one validated JSON number into order-preserving
// bytes. Equivalent spellings such as 1, 1.0, and 1e0 produce identical keys;
// finite JSON decimals never pass through float64.
//
// False reports invalid syntax or an adjusted exponent outside int64. An
// ordered index must put the latter row in its residual bitmap, ensuring range
// queries recheck it rather than silently losing a valid extreme number.
func AppendNumber(dst, number []byte, direction Direction) ([]byte, bool) {
	if direction > Descending {
		return dst, false
	}
	start := len(dst)
	negative, integerDigits, first, last, explicitExponent, ok := numberParts(number)
	if !ok {
		return dst, false
	}
	dst = append(dst, tagNumber)
	if first < 0 {
		dst = append(dst, tagNumberZero)
		invertDescending(dst[start:], direction)
		return dst, true
	}
	delta := int64(integerDigits) - int64(first)
	if explicitExponent > 0 && delta > math.MaxInt64-explicitExponent ||
		explicitExponent < 0 && delta < math.MinInt64-explicitExponent {
		return dst[:start], false
	}
	adjusted := explicitExponent + delta
	sign := byte(tagNumberPositive)
	if negative {
		sign = tagNumberNegative
	}
	dst = append(dst, sign)
	exponentStart := len(dst)
	dst = binary.BigEndian.AppendUint64(dst, uint64(adjusted)^(uint64(1)<<63))
	if negative {
		invert(dst[exponentStart:])
	}

	digitIndex := 0
	for _, b := range number {
		if b < '0' || b > '9' {
			if b == 'e' || b == 'E' {
				break
			}
			continue
		}
		if digitIndex >= first && digitIndex <= last {
			digit := b - '0'
			if negative {
				// Reverse magnitude: digits 10..1 and terminator 11.
				dst = append(dst, 10-digit)
			} else {
				// Forward magnitude: digits 1..10 and terminator 0.
				dst = append(dst, digit+1)
			}
		}
		digitIndex++
	}
	if negative {
		dst = append(dst, 11)
	} else {
		dst = append(dst, 0)
	}
	invertDescending(dst[start:], direction)
	return dst, true
}

// AppendString appends decoded UTF-8 string bytes. Zero-zero terminates the
// component; embedded zero is escaped as zero-0xff. This is prefix-free and
// preserves byte order.
func AppendString(dst, decoded []byte, direction Direction) ([]byte, bool) {
	if direction > Descending || !utf8.Valid(decoded) {
		return dst, false
	}
	start := len(dst)
	dst = append(dst, tagString)
	for _, b := range decoded {
		dst = appendStringByte(dst, b)
	}
	dst = append(dst, 0, 0)
	invertDescending(dst[start:], direction)
	return dst, true
}

// AppendJSONString decodes and appends one complete validated JSON string
// without a transient text buffer. It also fails closed on malformed input,
// making the storage boundary safe if a future caller violates the validated
// input contract.
func AppendJSONString(dst, raw []byte, direction Direction) ([]byte, bool) {
	if direction > Descending || len(raw) < 2 || raw[0] != '"' ||
		raw[len(raw)-1] != '"' || !utf8.Valid(raw) {
		return dst, false
	}
	start := len(dst)
	dst = append(dst, tagString)
	for i := 1; i < len(raw)-1; {
		b := raw[i]
		if b != '\\' {
			if b < 0x20 || b == '"' {
				return dst[:start], false
			}
			dst = appendStringByte(dst, b)
			i++
			continue
		}
		i++
		if i >= len(raw)-1 {
			return dst[:start], false
		}
		switch raw[i] {
		case '"', '\\', '/':
			dst = appendStringByte(dst, raw[i])
			i++
		case 'b':
			dst = appendStringByte(dst, '\b')
			i++
		case 'f':
			dst = appendStringByte(dst, '\f')
			i++
		case 'n':
			dst = appendStringByte(dst, '\n')
			i++
		case 'r':
			dst = appendStringByte(dst, '\r')
			i++
		case 't':
			dst = appendStringByte(dst, '\t')
			i++
		case 'u':
			high, next, ok := decodeHex4(raw, i+1)
			if !ok {
				return dst[:start], false
			}
			i = next
			r := rune(high)
			if 0xd800 <= high && high <= 0xdbff {
				if i+2 > len(raw)-1 || raw[i] != '\\' || raw[i+1] != 'u' {
					return dst[:start], false
				}
				low, lowNext, lowOK := decodeHex4(raw, i+2)
				if !lowOK || low < 0xdc00 || low > 0xdfff {
					return dst[:start], false
				}
				r = utf16.DecodeRune(r, rune(low))
				i = lowNext
			} else if 0xdc00 <= high && high <= 0xdfff {
				return dst[:start], false
			}
			var encoded [utf8.UTFMax]byte
			n := utf8.EncodeRune(encoded[:], r)
			for _, out := range encoded[:n] {
				dst = appendStringByte(dst, out)
			}
		default:
			return dst[:start], false
		}
	}
	dst = append(dst, 0, 0)
	invertDescending(dst[start:], direction)
	return dst, true
}

// AppendPrefixEnd appends the exclusive upper bound of an equality prefix.
// False means prefix is the all-0xff sentinel and has no finite successor.
func AppendPrefixEnd(dst, prefix []byte) ([]byte, bool) {
	start := len(dst)
	dst = append(dst, prefix...)
	for i := len(dst) - 1; i >= start; i-- {
		if dst[i] != 0xff {
			dst[i]++
			return dst[:i+1], true
		}
	}
	return dst[:start], false
}

func appendStringByte(dst []byte, b byte) []byte {
	if b == 0 {
		return append(dst, 0, 0xff)
	}
	return append(dst, b)
}

func invertDescending(component []byte, direction Direction) {
	if direction == Descending {
		invert(component)
	}
}

func invert(value []byte) {
	for i := range value {
		value[i] = ^value[i]
	}
}

// numberParts returns normalized decimal geometry without materializing a
// digit buffer. first and last address the combined integer+fraction sequence;
// first < 0 denotes zero.
func numberParts(number []byte) (negative bool, integerDigits, first, last int, exponent int64, ok bool) {
	if !validNumber(number) {
		return false, 0, 0, 0, 0, false
	}
	i := 0
	if number[i] == '-' {
		negative = true
		i++
	}
	integerStart := i
	for i < len(number) && number[i] >= '0' && number[i] <= '9' {
		i++
	}
	integerDigits = i - integerStart
	digitIndex := 0
	first, last = -1, -1
	for j := integerStart; j < i; j++ {
		if number[j] != '0' {
			if first < 0 {
				first = digitIndex
			}
			last = digitIndex
		}
		digitIndex++
	}
	if i < len(number) && number[i] == '.' {
		i++
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			if number[i] != '0' {
				if first < 0 {
					first = digitIndex
				}
				last = digitIndex
			}
			digitIndex++
			i++
		}
	}
	if i == len(number) {
		return negative, integerDigits, first, last, 0, true
	}
	i++ // e/E
	exponentNegative := false
	if number[i] == '+' || number[i] == '-' {
		exponentNegative = number[i] == '-'
		i++
	}
	for ; i < len(number); i++ {
		digit := int64(number[i] - '0')
		if exponent > (math.MaxInt64-digit)/10 {
			return false, 0, 0, 0, 0, false
		}
		exponent = exponent*10 + digit
	}
	if exponentNegative {
		exponent = -exponent
	}
	return negative, integerDigits, first, last, exponent, true
}

func validNumber(number []byte) bool {
	if len(number) == 0 {
		return false
	}
	i := 0
	if number[i] == '-' {
		i++
		if i == len(number) {
			return false
		}
	}
	if number[i] == '0' {
		i++
		if i < len(number) && number[i] >= '0' && number[i] <= '9' {
			return false
		}
	} else {
		if number[i] < '1' || number[i] > '9' {
			return false
		}
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
	}
	if i < len(number) && number[i] == '.' {
		i++
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	if i < len(number) && (number[i] == 'e' || number[i] == 'E') {
		i++
		if i < len(number) && (number[i] == '+' || number[i] == '-') {
			i++
		}
		start := i
		for i < len(number) && number[i] >= '0' && number[i] <= '9' {
			i++
		}
		if i == start {
			return false
		}
	}
	return i == len(number)
}

func decodeHex4(raw []byte, start int) (uint16, int, bool) {
	if start < 0 || start+4 > len(raw)-1 {
		return 0, start, false
	}
	var value uint16
	for _, b := range raw[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, start, false
		}
	}
	return value, start + 4, true
}
