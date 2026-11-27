package vibejson

import (
	"encoding/binary"
	"math"
	"strconv"

	"github.com/thesyncim/vibejson/x/byteview"
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
func tapeFloat64(src []byte, start, end int) (float64, bool) {
	src = src[:end]
	if value, ok := tapeFixedDecimalFloat64(src, start, end); ok {
		return value, true
	}
	parsedEnd, value, exact, number, haveNumber, _ := scanTypedFloat64Number(src, start)
	if parsedEnd != end {
		return 0, false
	}
	if exact {
		return value, true
	}
	if !haveNumber {
		_, number, _ = scanJSONNumber(src, start)
		haveNumber = !number.truncated
	}
	if haveNumber {
		if value, ok := eiselLemire64(number.mantissa, number.exponent, number.negative); ok {
			return value, true
		}
	}
	text := byteview.String(src[start:end])
	value, err := strconv.ParseFloat(text, 64)
	return value, err == nil
}

// tapeFixedDecimalFloat64 consumes the long fixed-point shapes common in
// indexed geographic data. The index has already validated every byte and
// supplied the exact end, so this lazy accessor does not need the delimiter
// probes and digit predicates required by the streaming scanner. Restricting
// the shortcut to 13-15 fractional digits keeps the mantissa within 18 digits
// for the two- and three-digit integer parts it accepts.
func tapeFixedDecimalFloat64(src []byte, start, end int) (float64, bool) {
	rem := src[start:end]
	if len(rem) == 0 {
		return 0, false
	}
	negative := rem[0] == '-'
	if negative {
		rem = rem[1:]
	}
	width := len(rem)
	if width < 16 || width > 19 {
		return 0, false
	}
	_ = rem[15]

	integerDigits := 0
	switch {
	case rem[2] == '.':
		integerDigits = 2
	case rem[3] == '.':
		integerDigits = 3
	default:
		return 0, false
	}
	fractionStart := integerDigits + 1
	fractionDigits := width - fractionStart
	if fractionDigits < 13 || fractionDigits > 15 || integerDigits+fractionDigits > 18 {
		return 0, false
	}
	frac := rem[fractionStart:]
	_ = frac[12]
	fractionWord := binary.LittleEndian.Uint64(frac[0:8])
	if byteEqMask(fractionWord, 'e')|byteEqMask(fractionWord, 'E') != 0 {
		return 0, false
	}

	mantissa := uint64(rem[0] - '0')
	if integerDigits == 2 {
		mantissa = mantissa*10 + uint64(rem[1]-'0')
	} else {
		mantissa = (mantissa*10+uint64(rem[1]-'0'))*10 +
			uint64(rem[2]-'0')
	}
	mantissa = mantissa*1e8 + parse8DigitsWord(fractionWord)
	remaining := fractionDigits - 8
	b0 := frac[8]
	b1 := frac[9]
	b2 := frac[10]
	b3 := frac[11]
	b4 := frac[12]
	if b0|0x20 == 'e' || b1|0x20 == 'e' || b2|0x20 == 'e' || b3|0x20 == 'e' || b4|0x20 == 'e' {
		return 0, false
	}
	tail := (((uint64(b0-'0')*10+uint64(b1-'0'))*10+uint64(b2-'0'))*10+uint64(b3-'0'))*10 + uint64(b4-'0')
	if remaining >= 6 {
		b5 := frac[13]
		if b5|0x20 == 'e' {
			return 0, false
		}
		tail = tail*10 + uint64(b5-'0')
		if remaining == 7 {
			b6 := frac[14]
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

func scanJSONNumber(src []byte, start int) (int, jsonNumber, bool) {
	i := uint(start)
	n := uint(len(src))
	var number jsonNumber
	if i >= n {
		return int(i), number, false
	}
	if src[i] == '-' {
		number.negative = true
		i++
		if i >= n {
			return int(i), number, false
		}
	}

	switch c := src[i]; {
	case c == '0':
		number.addDigit(c)
		i++
	case isOneNine(c):
		if i+16 <= n && all16Digits(src[i:i+16]) {
			number.add16Digits(parse16Digits(src[i : i+16]))
			i += 16
		}
		for i < n && isDigit(src[i]) {
			if number.nd != 0 && i+16 <= n && all16Digits(src[i:i+16]) {
				number.add16Digits(parse16Digits(src[i : i+16]))
				i += 16
				continue
			}
			number.addDigit(src[i])
			i++
		}
	default:
		return int(i), number, false
	}

	if i < n && src[i] == '.' {
		number.dp = number.nd
		i++
		if i >= n || !isDigit(src[i]) {
			return int(i), number, false
		}
		for i < n && isDigit(src[i]) {
			if number.nd != 0 && i+16 <= n && all16Digits(src[i:i+16]) {
				number.add16Digits(parse16Digits(src[i : i+16]))
				i += 16
				continue
			}
			number.addDigit(src[i])
			i++
		}
	} else {
		number.dp = number.nd
	}

	if i < n && (src[i] == 'e' || src[i] == 'E') {
		i++
		exponentSign := 1
		if i < n && (src[i] == '+' || src[i] == '-') {
			if src[i] == '-' {
				exponentSign = -1
			}
			i++
		}
		if i >= n || !isDigit(src[i]) {
			return int(i), number, false
		}
		exponent := 0
		for i < n && isDigit(src[i]) {
			if exponent < 10000 {
				exponent = exponent*10 + int(src[i]-'0')
			}
			i++
		}
		number.dp += exponent * exponentSign
	}
	if number.mantissa != 0 {
		number.exponent = number.dp - number.ndMant
	}
	return int(i), number, true
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

func exactJSONFloat64(src []byte, start, end int) (float64, bool) {
	rem := src[start:end]
	i := uint(0)
	n := uint(len(rem))
	if n == 0 {
		return 0, false
	}
	negative := false
	if rem[0] == '-' {
		negative = true
		i = 1
	}

	var mantissa uint64
	fractionDigits := 0
	if n-i == 16 && all16Digits(rem[i:n]) {
		mantissa = parse16Digits(rem[i:n])
		i = n
	}
	for i < n {
		c := rem[i]
		if c == '.' || c == 'e' || c == 'E' {
			break
		}
		if mantissa > (math.MaxUint64-uint64(c-'0'))/10 {
			return 0, false
		}
		mantissa = mantissa*10 + uint64(c-'0')
		i++
	}
	if i < n && rem[i] == '.' {
		i++
		for i < n {
			c := rem[i]
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
	if i < n {
		i++
		exponentNegative := false
		if i < n && (rem[i] == '+' || rem[i] == '-') {
			exponentNegative = rem[i] == '-'
			i++
		}
		for i < n {
			if exponent > 1000 {
				return 0, false
			}
			exponent = exponent*10 + int(rem[i]-'0')
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
