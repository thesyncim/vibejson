package simdjson

import (
	"math"
	"strconv"
	"unsafe"
)

const maxJSONMantissaDigits = 19

// Provenance: GO-NUMSCAN-001.
// jsonNumber, scanJSONNumber, addDigit, add16Digits, and exactFloat64 adapt
// Go's strconv readFloat and atof64exact at commit
// d468ad3648be469ffc4090e4586c29709182d6b6,
// src/internal/strconv/atof.go. Copyright The Go Authors; BSD-3-Clause, see
// LICENSE-GO. Local changes enforce JSON grammar, scan unsafe byte spans,
// batch 16 digits, and integrate exact scaling and Eisel-Lemire fallbacks.
//
// jsonNumber accumulates a scanned number the way strconv's readFloat does:
// a 19-digit mantissa window with truncation tracking, total and mantissa
// digit counts, and the decimal point position, so the exact-conversion
// envelope test below matches strconv's.
type jsonNumber struct {
	mantissa uint64
	exponent int
	negative bool

	truncated bool
	nd        int // total significant digits scanned
	ndMant    int // digits accumulated into mantissa
	dp        int // decimal point position relative to the digits
}

// parseFloat64 parses one strict JSON number with optional surrounding JSON
// whitespace. Successful calls do not allocate.
func parseFloat64(src []byte) (float64, error) {
	start := skipSpace(src, 0)
	if start == len(src) {
		return 0, (&parser{src: src}).err(start, "expected number")
	}
	base := unsafe.Pointer(unsafe.SliceData(src))
	end, number, ok := scanJSONNumber(base, len(src), start)
	if !ok {
		_, msg := scanNumber(src, start)
		return 0, (&parser{src: src}).err(start, msg)
	}
	if trailing := skipSpace(src, end); trailing != len(src) {
		return 0, (&parser{src: src}).err(trailing, "unexpected trailing data")
	}
	if value, exact := number.exactFloat64(); exact {
		return value, nil
	}
	if !number.truncated {
		if value, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return value, nil
		}
	}
	text := unsafe.String((*byte)(unsafe.Add(base, start)), end-start)
	value, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return 0, (&parser{src: src}).err(start, "number out of range")
	}
	return value, nil
}

// tapeFloat64 rounds the JSON number in [start, end) to the nearest float64
// through the same shape-specialized scanner and conversion ladder as the
// typed decoder: the exact-multiply envelope first, then Eisel-Lemire, and
// strconv only for truncated or tie-ambiguous spellings both defer on. A lazy
// read therefore yields the identical bits the streaming decode would, while
// common geographic coordinates avoid a second digit-by-digit scan.
//
// ok is false exactly when strconv.ParseFloat would report an error — an
// out-of-range magnitude that overflows to an infinity — so the lazy readers
// keep reporting those as failures. The exact and Eisel-Lemire paths never
// produce an out-of-range value, so they always succeed.
//
// The digits were validated before this is reached. The parsed-end check is a
// defensive assertion of that index invariant; base+start..base+end must lie
// within one live document.
func tapeFloat64(base unsafe.Pointer, start, end int) (float64, bool) {
	if value, ok := tapeFixedDecimalFloat64(base, start, end); ok {
		return value, true
	}
	parsedEnd, value, exact, number, haveNumber, _ := scanTypedFloat64Number(base, end, start)
	if parsedEnd != end {
		return 0, false
	}
	if exact {
		return value, true
	}
	if !haveNumber {
		_, number, _ = scanJSONNumber(base, end, start)
		haveNumber = !number.truncated
	}
	if haveNumber {
		if value, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return value, true
		}
	}
	text := unsafe.String((*byte)(unsafe.Add(base, start)), end-start)
	value, err := strconv.ParseFloat(text, 64)
	return value, err == nil
}

// tapeFixedDecimalFloat64 consumes the long fixed-point shapes common in
// indexed geographic data. The index has already validated every byte and
// supplied the exact end, so this lazy accessor does not need the delimiter
// probes and digit predicates required by the streaming scanner. Restricting
// the shortcut to 13-15 fractional digits keeps the mantissa within 18 digits
// for the two- and three-digit integer parts it accepts.
func tapeFixedDecimalFloat64(base unsafe.Pointer, start, end int) (float64, bool) {
	i := start
	negative := fastByteAt(base, i) == '-'
	if negative {
		i++
	}
	width := end - i
	if width < 16 || width > 19 {
		return 0, false
	}

	integerDigits := 0
	switch {
	case fastByteAt(base, i+2) == '.':
		integerDigits = 2
	case fastByteAt(base, i+3) == '.':
		integerDigits = 3
	default:
		return 0, false
	}
	fractionStart := i + integerDigits + 1
	fractionDigits := end - fractionStart
	if fractionDigits < 13 || fractionDigits > 15 || integerDigits+fractionDigits > 18 {
		return 0, false
	}
	fractionWord := loadUint64LE(unsafe.Add(base, fractionStart))
	if byteEqMask(fractionWord, 'e')|byteEqMask(fractionWord, 'E') != 0 {
		return 0, false
	}

	mantissa := uint64(fastByteAt(base, i) - '0')
	if integerDigits == 2 {
		mantissa = mantissa*10 + uint64(fastByteAt(base, i+1)-'0')
	} else {
		mantissa = (mantissa*10+uint64(fastByteAt(base, i+1)-'0'))*10 +
			uint64(fastByteAt(base, i+2)-'0')
	}
	mantissa = mantissa*1e8 + parse8DigitsWord(fractionWord)
	tailStart := fractionStart + 8
	remaining := fractionDigits - 8
	b0 := fastByteAt(base, tailStart)
	b1 := fastByteAt(base, tailStart+1)
	b2 := fastByteAt(base, tailStart+2)
	b3 := fastByteAt(base, tailStart+3)
	b4 := fastByteAt(base, tailStart+4)
	if b0|0x20 == 'e' || b1|0x20 == 'e' || b2|0x20 == 'e' || b3|0x20 == 'e' || b4|0x20 == 'e' {
		return 0, false
	}
	tail := (((uint64(b0-'0')*10+uint64(b1-'0'))*10+uint64(b2-'0'))*10+uint64(b3-'0'))*10 + uint64(b4-'0')
	if remaining >= 6 {
		b5 := fastByteAt(base, tailStart+5)
		if b5|0x20 == 'e' {
			return 0, false
		}
		tail = tail*10 + uint64(b5-'0')
		if remaining == 7 {
			b6 := fastByteAt(base, tailStart+6)
			if b6|0x20 == 'e' {
				return 0, false
			}
			tail = tail*10 + uint64(b6-'0')
		}
	}
	mantissa = mantissa*pow10Uint64[remaining] + tail

	if mantissa >= uint64(1)<<52 {
		switch fractionDigits {
		case 13:
			return scaleJSONFloat64Fixed(
				mantissa, 0xe12e13424bb40e14, 0xd79a5a0df94f9046, 978, negative,
			), true
		case 14:
			return scaleJSONFloat64Fixed(
				mantissa, 0xb424dc35095cd810, 0xac7b7b3e610c736b, 975, negative,
			), true
		case 15:
			return scaleJSONFloat64Fixed(
				mantissa, 0x901d7cf73ab0acda, 0xf062c8feb409f5ef, 972, negative,
			), true
		}
	}
	value := float64(mantissa) / anyPow10[fractionDigits]
	if negative {
		value = -value
	}
	return value, true
}

func scanJSONNumber(base unsafe.Pointer, n, start int) (int, jsonNumber, bool) {
	i := start
	var number jsonNumber
	if fastByteAt(base, i) == '-' {
		number.negative = true
		i++
		if i >= n {
			return i, number, false
		}
	}

	switch c := fastByteAt(base, i); {
	case c == '0':
		number.addDigit(c)
		i++
	case isOneNine(c):
		if i <= n-16 && all16Digits(unsafe.Add(base, i)) {
			number.add16Digits(parse16Digits(unsafe.Add(base, i)))
			i += 16
		}
		for i < n && isDigit(fastByteAt(base, i)) {
			if number.nd != 0 && i <= n-16 && all16Digits(unsafe.Add(base, i)) {
				number.add16Digits(parse16Digits(unsafe.Add(base, i)))
				i += 16
				continue
			}
			number.addDigit(fastByteAt(base, i))
			i++
		}
	default:
		return i, number, false
	}

	if i < n && fastByteAt(base, i) == '.' {
		number.dp = number.nd
		i++
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, number, false
		}
		for i < n && isDigit(fastByteAt(base, i)) {
			if number.nd != 0 && i <= n-16 && all16Digits(unsafe.Add(base, i)) {
				number.add16Digits(parse16Digits(unsafe.Add(base, i)))
				i += 16
				continue
			}
			number.addDigit(fastByteAt(base, i))
			i++
		}
	} else {
		number.dp = number.nd
	}

	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		i++
		exponentSign := 1
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			if fastByteAt(base, i) == '-' {
				exponentSign = -1
			}
			i++
		}
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, number, false
		}
		exponent := 0
		for i < n && isDigit(fastByteAt(base, i)) {
			if exponent < 10000 {
				exponent = exponent*10 + int(fastByteAt(base, i)-'0')
			}
			i++
		}
		number.dp += exponent * exponentSign
	}
	if number.mantissa != 0 {
		number.exponent = number.dp - number.ndMant
	}
	return i, number, true
}

func (n *jsonNumber) addDigit(c byte) {
	if c == '0' && n.nd == 0 {
		n.dp--
		return
	}
	n.nd++
	if n.ndMant < maxJSONMantissaDigits {
		n.mantissa = n.mantissa*10 + uint64(c-'0')
		n.ndMant++
	} else if c != '0' {
		n.truncated = true
	}
}

func (n *jsonNumber) add16Digits(digits uint64) {
	n.nd += 16
	remaining := maxJSONMantissaDigits - n.ndMant
	if remaining >= 16 {
		n.mantissa = n.mantissa*1e16 + digits
		n.ndMant += 16
		return
	}
	if remaining <= 0 {
		n.truncated = n.truncated || digits != 0
		return
	}
	divisor := pow10Uint64[16-remaining]
	n.mantissa = n.mantissa*pow10Uint64[remaining] + digits/divisor
	n.ndMant += remaining
	n.truncated = n.truncated || digits%divisor != 0
}

func (n jsonNumber) exactFloat64() (float64, bool) {
	if n.truncated {
		return 0, false
	}
	mantissa := n.mantissa
	if mantissa == 0 {
		if n.negative {
			return math.Copysign(0, -1), true
		}
		return 0, true
	}
	// This is the same exact-rounding envelope used by strconv: a mantissa
	// below 2^52 combined with a power of ten no larger than 1e22 can be
	// converted with one floating-point multiply or divide. Moving up to 15
	// decimal zeros into the mantissa extends the positive exponent range.
	if mantissa >= uint64(1)<<52 {
		if n.exponent < 0 {
			return scaleJSONFloat64(mantissa, n.exponent, n.negative)
		}
		return 0, false
	}
	value := float64(mantissa)
	if n.negative {
		value = -value
	}
	switch {
	case n.exponent == 0:
		return value, true
	case n.exponent > 0 && n.exponent <= 37:
		exponent := n.exponent
		if exponent > 22 {
			value *= anyPow10[exponent-22]
			exponent = 22
		}
		if value > 1e15 || value < -1e15 {
			return 0, false
		}
		return value * anyPow10[exponent], true
	case n.exponent < 0 && n.exponent >= -22:
		return value / anyPow10[-n.exponent], true
	default:
		return 0, false
	}
}

func exactJSONFloat64(base unsafe.Pointer, start, end int) (float64, bool) {
	i := start
	negative := false
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
	}

	var mantissa uint64
	fractionDigits := 0
	if end-i == 16 && all16Digits(unsafe.Add(base, i)) {
		mantissa = parse16Digits(unsafe.Add(base, i))
		i = end
	}
	for i < end {
		c := fastByteAt(base, i)
		if c == '.' || c == 'e' || c == 'E' {
			break
		}
		if mantissa > (math.MaxUint64-uint64(c-'0'))/10 {
			return 0, false
		}
		mantissa = mantissa*10 + uint64(c-'0')
		i++
	}
	if i < end && fastByteAt(base, i) == '.' {
		i++
		for i < end {
			c := fastByteAt(base, i)
			if c == 'e' || c == 'E' {
				break
			}
			if mantissa > (math.MaxUint64-uint64(c-'0'))/10 {
				return 0, false
			}
			mantissa = mantissa*10 + uint64(c-'0')
			fractionDigits++
			i++
		}
	}

	exponent := 0
	if i < end {
		i++
		exponentNegative := false
		if i < end && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			exponentNegative = fastByteAt(base, i) == '-'
			i++
		}
		for i < end {
			if exponent > 1000 {
				return 0, false
			}
			exponent = exponent*10 + int(fastByteAt(base, i)-'0')
			i++
		}
		if exponentNegative {
			exponent = -exponent
		}
	}
	return (jsonNumber{
		mantissa: mantissa,
		exponent: exponent - fractionDigits,
		negative: negative,
	}).exactFloat64()
}
