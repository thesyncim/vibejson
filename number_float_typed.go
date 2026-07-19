package simdjson

import "unsafe"

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
