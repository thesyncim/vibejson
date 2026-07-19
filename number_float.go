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

// scanTypedFloat64 parses one JSON float64, fast-pathing the values that
// convert with a single exact multiply. It stays separate from
// scanTypedFloat64Number (rather than projecting it) so the hot typed-array
// path returns four registers instead of also carrying the recovered number.
func scanTypedFloat64(base unsafe.Pointer, n, start int) (end int, value float64, exact, ok bool) {
	i := start
	if fastByteAt(base, i) == '-' {
		i++
	}
	if i <= n-18 && fastByteAt(base, i) == '0' && fastByteAt(base, i+1) == '.' &&
		all16Digits(unsafe.Add(base, i+2)) {
		return scanTypedLeadingZeroFloat64(base, n, start)
	}
	if i <= n-11 && isOneNine(fastByteAt(base, i)) && isDigit(fastByteAt(base, i+1)) {
		if fastByteAt(base, i+2) == '.' && all8Digits(unsafe.Add(base, i+3)) {
			return scanTypedTwoDigitFloat64(base, n, start)
		}
		if i <= n-12 && isDigit(fastByteAt(base, i+2)) && fastByteAt(base, i+3) == '.' &&
			all8Digits(unsafe.Add(base, i+4)) {
			return scanTypedThreeDigitFloat64(base, n, start)
		}
	}
	if start <= n-8 {
		word := loadUint64LE(unsafe.Add(base, start))
		if byteEqMask(word, ',')|byteEqMask(word, ']')|byteEqMask(word, '}') != 0 {
			end, number, ok := scanJSONNumber(base, n, start)
			if !ok {
				return end, 0, false, false
			}
			value, exact = number.exactFloat64()
			return end, value, exact, true
		}
	} else {
		end, number, ok := scanJSONNumber(base, n, start)
		if !ok {
			return end, 0, false, false
		}
		value, exact = number.exactFloat64()
		return end, value, exact, true
	}
	return scanTypedSimpleFloat64(base, n, start)
}

// scanTypedFloat64Number mirrors scanTypedFloat64 but, on the inexact path,
// also returns the jsonNumber it recovered so the caller can round with
// Eisel-Lemire without re-scanning the digits. haveNumber is true only when the
// returned number carries a complete, untruncated mantissa (digits <= 18);
// otherwise the caller must fall back to the full scanJSONNumber. The exact
// fast paths leave number zero and haveNumber false: an exact result needs no
// number at all.
func scanTypedFloat64Number(base unsafe.Pointer, n, start int) (end int, value float64, exact bool, number jsonNumber, haveNumber, ok bool) {
	i := start
	if fastByteAt(base, i) == '-' {
		i++
	}
	if i <= n-18 && fastByteAt(base, i) == '0' && fastByteAt(base, i+1) == '.' &&
		all16Digits(unsafe.Add(base, i+2)) {
		end, value, exact, ok = scanTypedLeadingZeroFloat64(base, n, start)
		return end, value, exact, number, false, ok
	}
	if i <= n-11 && isOneNine(fastByteAt(base, i)) && isDigit(fastByteAt(base, i+1)) {
		if fastByteAt(base, i+2) == '.' && all8Digits(unsafe.Add(base, i+3)) {
			end, value, exact, ok = scanTypedTwoDigitFloat64(base, n, start)
			return end, value, exact, number, false, ok
		}
		if i <= n-12 && isDigit(fastByteAt(base, i+2)) && fastByteAt(base, i+3) == '.' &&
			all8Digits(unsafe.Add(base, i+4)) {
			end, value, exact, ok = scanTypedThreeDigitFloat64(base, n, start)
			return end, value, exact, number, false, ok
		}
	}
	if start <= n-8 {
		word := loadUint64LE(unsafe.Add(base, start))
		if byteEqMask(word, ',')|byteEqMask(word, ']')|byteEqMask(word, '}') != 0 {
			end, number, ok = scanJSONNumber(base, n, start)
			if !ok {
				return end, 0, false, number, false, false
			}
			value, exact = number.exactFloat64()
			return end, value, exact, number, !number.truncated, true
		}
	} else {
		end, number, ok = scanJSONNumber(base, n, start)
		if !ok {
			return end, 0, false, number, false, false
		}
		value, exact = number.exactFloat64()
		return end, value, exact, number, !number.truncated, true
	}
	return scanTypedSimpleFloat64Number(base, n, start)
}

// scanTypedSimpleFloat64 parses a JSON float that matched none of the
// specialized shapes. It is a thin projection of scanTypedSimpleFloat64Number
// for callers that do not need the recovered mantissa.
func scanTypedSimpleFloat64(base unsafe.Pointer, n, start int) (end int, value float64, exact, ok bool) {
	end, value, exact, _, _, ok = scanTypedSimpleFloat64Number(base, n, start)
	return
}

// scanTypedSimpleFloat64Number is scanTypedSimpleFloat64 that also surfaces the
// recovered jsonNumber. When the mantissa fits in the 18-digit window it is
// complete and untruncated, so haveNumber is true and the caller may round the
// inexact result with Eisel-Lemire directly. A wider mantissa leaves haveNumber
// false, sending the caller to the full scanJSONNumber for truncation tracking.
func scanTypedSimpleFloat64Number(base unsafe.Pointer, n, start int) (end int, value float64, exact bool, number jsonNumber, haveNumber, ok bool) {
	i := start
	negative := false
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
		if i >= n {
			return i, 0, false, number, false, false
		}
	}

	var mantissa uint64
	digits := 0
	if fastByteAt(base, i) == '0' {
		digits = 1
		i++
		if i < n && isDigit(fastByteAt(base, i)) {
			return i, 0, false, number, false, false
		}
	} else if isOneNine(fastByteAt(base, i)) {
		for i < n && isDigit(fastByteAt(base, i)) {
			digits++
			if digits <= 18 {
				mantissa = mantissa*10 + uint64(fastByteAt(base, i)-'0')
			}
			i++
		}
	} else {
		return i, 0, false, number, false, false
	}

	fractionDigits := 0
	if i < n && fastByteAt(base, i) == '.' {
		i++
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, 0, false, number, false, false
		}
		for i < n && isDigit(fastByteAt(base, i)) {
			digits++
			fractionDigits++
			if digits <= 18 {
				mantissa = mantissa*10 + uint64(fastByteAt(base, i)-'0')
			}
			i++
		}
	}

	exponent := 0
	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		i++
		exponentNegative := false
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			exponentNegative = fastByteAt(base, i) == '-'
			i++
		}
		if i >= n || !isDigit(fastByteAt(base, i)) {
			return i, 0, false, number, false, false
		}
		for i < n && isDigit(fastByteAt(base, i)) {
			if exponent <= 1000 {
				exponent = exponent*10 + int(fastByteAt(base, i)-'0')
			}
			i++
		}
		if exponentNegative {
			exponent = -exponent
		}
	}

	if digits <= 18 {
		decimalExponent := exponent - fractionDigits
		number = jsonNumber{mantissa: mantissa, exponent: decimalExponent, negative: negative}
		haveNumber = true
		if mantissa >= uint64(1)<<52 && decimalExponent >= -22 && decimalExponent < 0 {
			value, exact = scaleJSONFloat64(mantissa, decimalExponent, negative)
		} else {
			value, exact = number.exactFloat64()
		}
	}
	return i, value, exact, number, haveNumber, true
}

// scanTypedTwoDigitFloat64 handles the long DD.dddddddd shape common in
// geographic data. The dispatcher has proved both integer digits, the decimal
// point, and the first eight fractional digits.
func scanTypedTwoDigitFloat64(base unsafe.Pointer, n, start int) (end int, value float64, exact, ok bool) {
	i := start
	negative := false
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
	}
	mantissa := uint64(fastByteAt(base, i)-'0')*10 + uint64(fastByteAt(base, i+1)-'0')
	mantissa = mantissa*1e8 + parse8Digits(unsafe.Add(base, i+3))
	i += 11
	digits := 10
	fractionDigits := 8
	if i+8 <= n {
		word := loadUint64LE(unsafe.Add(base, i))
		invalid := nonDigitMask8(word)
		tailDigits := 0
		switch {
		case invalid&0x0000808080808080 != 0:
		case invalid&0x0080000000000000 != 0:
			tailDigits = 6
			word = word&0x0000ffffffffffff | digitLower&0xffff000000000000
		case invalid&0x8000000000000000 != 0:
			tailDigits = 7
			word = word&0x00ffffffffffffff | digitLower&0xff00000000000000
		default:
			tailDigits = 8
		}
		if tailDigits != 0 {
			mantissa = mantissa*1e8 + parse8DigitsWord(word)
			digits = 18
			fractionDigits = 16
			i += tailDigits
			// The eight-digit tail can end exactly at the buffer or run into
			// further fraction digits; only a proven delimiter may take the
			// fixed-scale exit.
			if i == n {
				// The constants are jsonNegativePow10[6] (1e-16) with its
				// precomputed binary exponent bias.
				value = scaleJSONFloat64Fixed(
					mantissa, 0xe69594bec44de15c, 0xb3d141978676564c, 968, negative,
				)
				return i, value, true, true
			}
			if c := fastByteAt(base, i); c != 'e' && c != 'E' && !isDigit(c) {
				value = scaleJSONFloat64Fixed(
					mantissa, 0xe69594bec44de15c, 0xb3d141978676564c, 968, negative,
				)
				return i, value, true, true
			}
		}
	}
	for i < n && isDigit(fastByteAt(base, i)) {
		digits++
		fractionDigits++
		if digits <= 18 {
			mantissa = mantissa*10 + uint64(fastByteAt(base, i)-'0')
		}
		i++
	}

	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		return scanTypedSimpleFloat64(base, n, start)
	}

	if digits <= 18 {
		decimalExponent := -fractionDigits
		if mantissa >= uint64(1)<<52 && decimalExponent >= -22 && decimalExponent < 0 {
			value, exact = scaleJSONFloat64(mantissa, decimalExponent, negative)
		} else {
			value, exact = (jsonNumber{
				mantissa: mantissa,
				exponent: decimalExponent,
				negative: negative,
			}).exactFloat64()
		}
	}
	return i, value, exact, true
}

// scanTypedThreeDigitFloat64 handles DDD.dddddddddddddd values while keeping
// the mantissa inside the 18-digit exact-scaling envelope.
func scanTypedThreeDigitFloat64(base unsafe.Pointer, n, start int) (end int, value float64, exact, ok bool) {
	i := start
	negative := false
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
	}
	mantissa := uint64(fastByteAt(base, i)-'0')*100 +
		uint64(fastByteAt(base, i+1)-'0')*10 + uint64(fastByteAt(base, i+2)-'0')
	mantissa = mantissa*1e8 + parse8Digits(unsafe.Add(base, i+4))
	i += 12
	digits := 11
	fractionDigits := 8
	if i+8 <= n {
		word := loadUint64LE(unsafe.Add(base, i))
		invalid := nonDigitMask8(word)
		tailDigits := 0
		switch {
		case invalid&0x0000808080808080 != 0:
		case invalid&0x0080000000000000 != 0:
			tailDigits = 6
			word = word&0x0000ffffffffffff | digitLower&0xffff000000000000
		case invalid&0x8000000000000000 != 0:
			tailDigits = 7
			word = word&0x00ffffffffffffff | digitLower&0xff00000000000000
		default:
			digits = 19
			fractionDigits = 16
			i += 8
		}
		if tailDigits != 0 {
			mantissa = mantissa*1e7 + parse8DigitsWord(word)/10
			digits = 18
			fractionDigits = 15
			i += tailDigits
			if c := fastByteAt(base, i); c != 'e' && c != 'E' {
				value = scaleJSONFloat64Fixed(
					mantissa, 0x901d7cf73ab0acda, 0xf062c8feb409f5ef, 972, negative,
				)
				return i, value, true, true
			}
		}
	}
	for i < n && isDigit(fastByteAt(base, i)) {
		digits++
		fractionDigits++
		if digits <= 18 {
			mantissa = mantissa*10 + uint64(fastByteAt(base, i)-'0')
		}
		i++
	}

	if i < n && (fastByteAt(base, i) == 'e' || fastByteAt(base, i) == 'E') {
		return scanTypedSimpleFloat64(base, n, start)
	}
	if digits <= 18 {
		decimalExponent := -fractionDigits
		if mantissa >= uint64(1)<<52 && decimalExponent >= -22 && decimalExponent < 0 {
			value, exact = scaleJSONFloat64(mantissa, decimalExponent, negative)
		} else {
			value, exact = (jsonNumber{
				mantissa: mantissa,
				exponent: decimalExponent,
				negative: negative,
			}).exactFloat64()
		}
	}
	return i, value, exact, true
}

func scanTypedLeadingZeroFloat64(base unsafe.Pointer, n, start int) (end int, value float64, exact, ok bool) {
	i := start
	negative := false
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
	}
	// The dispatcher proved that this is 0. followed by at least 16 digits.
	i += 2
	dp := 0
	for i < n && fastByteAt(base, i) == '0' {
		dp--
		i++
	}
	var mantissa uint64
	ndMant := 0
	truncated := false
	if i <= n-16 && isOneNine(fastByteAt(base, i)) && all16Digits(unsafe.Add(base, i)) {
		mantissa = parse16Digits(unsafe.Add(base, i))
		ndMant = 16
		i += 16
	}
	for i < n && isDigit(fastByteAt(base, i)) {
		c := fastByteAt(base, i)
		if ndMant < maxJSONMantissaDigits {
			mantissa = mantissa*10 + uint64(c-'0')
			ndMant++
		} else if c != '0' {
			truncated = true
		}
		i++
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
			return i, 0, false, false
		}
		exponent := 0
		for i < n && isDigit(fastByteAt(base, i)) {
			if exponent < 10000 {
				exponent = exponent*10 + int(fastByteAt(base, i)-'0')
			}
			i++
		}
		dp += exponent * exponentSign
	}

	number := jsonNumber{
		mantissa:  mantissa,
		exponent:  dp - ndMant,
		negative:  negative,
		truncated: truncated,
	}
	value, exact = number.exactFloat64()
	return i, value, exact, true
}

var pow10Uint64 = [...]uint64{
	1,
	10,
	100,
	1000,
	10000,
	100000,
	1000000,
	10000000,
	100000000,
	1000000000,
	10000000000,
	100000000000,
	1000000000000,
	10000000000000,
	100000000000000,
	1000000000000000,
	10000000000000000,
	100000000000000000,
	1000000000000000000,
	10000000000000000000,
}
