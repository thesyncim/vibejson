package slopjson

// Exact numeric equality over JSON number spellings.
//
// jsonNumberEqual is the number kernel behind containment (contains.go): it
// decides whether two validated JSON number spellings denote the same
// mathematical value, the way PostgreSQL's numeric type compares jsonb
// numbers — 1, 1.0, 1e0, and 0.1e1 are all equal — without ever rounding
// through a binary float. A float64 round trip would collapse distinct
// integers past 2^53 and distinct decimals past seventeen significant
// digits; instead each spelling is decomposed exactly into sign,
// significant digits, and a decimal exponent, all read in place from the
// source bytes:
//
//	value = ± 0.D1 D2 … Dn × 10^(weight+1)
//
// where D1…Dn are the significant digits (leading and trailing zeros
// stripped) and weight is the decimal exponent of D1. Two nonzero values
// are equal exactly when their signs, weights, and significant digit
// sequences agree; every zero spelling (0, -0, 0.00, 0e9) equals every
// other, matching numeric, which has no negative zero.
//
// The one subtlety is the weight. It is the spelled exponent plus a small
// adjustment derived from the digit layout, and JSON places no bound on
// the exponent, so 1e10000000000000000000 is a valid number whose exponent
// exceeds int64. Spellings with exponent literals of eighteen digits or
// fewer — everything that occurs outside adversarial input — compare
// weights in one int64. Wider exponents take an exact cold path that does
// the remaining arithmetic on decimal digit strings, so equality is exact
// at any magnitude. PostgreSQL's numeric rejects values beyond about
// 1e131071; this comparator answers exactly where numeric would error,
// which containment documents as a strict extension.

// decNumber is the exact decimal decomposition of one validated JSON number
// spelling. All offsets index the source slice the spelling was parsed
// from; nothing is copied.
type decNumber struct {
	// zero reports that every digit is zero. A zero value equals any other
	// zero regardless of sign or exponent, and leaves the remaining fields
	// meaningless.
	zero bool
	// neg is the significand sign of a nonzero value.
	neg bool
	// sigFirst and sigLast are the offsets of the first and last
	// significant digit (inclusive). The bytes between them are digits and
	// at most one decimal point at dot.
	sigFirst, sigLast int
	// dot is the offset of the decimal point, or -1.
	dot int
	// weight is the decimal exponent of the leading significant digit when
	// expFits; the spelled exponent plus adj.
	weight int64
	// expFits reports that the exponent literal has at most eighteen
	// digits after stripping leading zeros, so weight is exact. When
	// false, expNeg, expDigits, and adj carry the exact form for the cold
	// comparison path.
	expFits   bool
	expNeg    bool
	expDigits []byte // exponent digits, leading zeros stripped
	adj       int64  // weight = spelled exponent + adj
}

// parseDecNumber decomposes src, which must be exactly one validated JSON
// number spelling, into its exact decimal form.
func parseDecNumber(src []byte) decNumber {
	var d decNumber
	i := 0
	if src[i] == '-' {
		d.neg = true
		i++
	}
	intStart := i
	for i < len(src) && src[i] >= '0' && src[i] <= '9' {
		i++
	}
	intEnd := i
	d.dot = -1
	fracStart, fracEnd := 0, 0
	if i < len(src) && src[i] == '.' {
		d.dot = i
		i++
		fracStart = i
		for i < len(src) && src[i] >= '0' && src[i] <= '9' {
			i++
		}
		fracEnd = i
	}
	d.expFits = true
	var exp int64
	if i < len(src) {
		// The remainder is the exponent: e or E, an optional sign, digits.
		i++
		if src[i] == '+' {
			i++
		} else if src[i] == '-' {
			d.expNeg = true
			i++
		}
		for i < len(src) && src[i] == '0' {
			i++
		}
		d.expDigits = src[i:]
		if len(d.expDigits) <= 18 {
			for _, c := range d.expDigits {
				exp = exp*10 + int64(c-'0')
			}
			if d.expNeg {
				exp = -exp
			}
		} else {
			d.expFits = false
		}
	}

	// The first significant digit fixes the weight adjustment. JSON forbids
	// leading zeros, so an integer part is either the single digit 0 or
	// starts with its first significant digit.
	if src[intStart] != '0' {
		d.sigFirst = intStart
		d.adj = int64(intEnd-intStart) - 1
	} else {
		d.sigFirst = -1
		for j := fracStart; j < fracEnd; j++ {
			if src[j] != '0' {
				d.sigFirst = j
				d.adj = -int64(j-fracStart) - 1
				break
			}
		}
		if d.sigFirst < 0 {
			d.zero = true
			return d
		}
	}
	if d.expFits {
		// |exp| < 10^18 and |adj| < 2^32, so the sum cannot overflow.
		d.weight = exp + d.adj
	}

	// The last significant digit strips trailing zeros from the
	// significand; the weight is unaffected because it is anchored at the
	// leading digit.
	d.sigLast = -1
	for j := fracEnd - 1; j >= fracStart; j-- {
		if src[j] != '0' {
			d.sigLast = j
			break
		}
	}
	if d.sigLast < 0 {
		for j := intEnd - 1; ; j-- {
			if src[j] != '0' {
				d.sigLast = j
				break
			}
		}
	}
	return d
}

// jsonNumberEqual reports whether a and b, each exactly one validated JSON
// number spelling, denote the same mathematical value. Identical spellings
// short-circuit; otherwise both are decomposed and compared by sign,
// weight, and significant digits. It never allocates outside the huge-
// exponent cold path.
func jsonNumberEqual(a, b []byte) bool {
	if bytesEqualString(a, ownedBytesString(b)) {
		return true
	}
	da := parseDecNumber(a)
	db := parseDecNumber(b)
	if da.zero || db.zero {
		return da.zero == db.zero
	}
	if da.neg != db.neg || !decWeightEqual(&da, &db) {
		return false
	}
	// Lockstep over the significant digits, skipping each side's decimal
	// point. The sequences must agree in content and length.
	i, j := da.sigFirst, db.sigFirst
	for {
		if i == da.dot {
			i++
		}
		if j == db.dot {
			j++
		}
		aMore, bMore := i <= da.sigLast, j <= db.sigLast
		if !aMore || !bMore {
			return aMore == bMore
		}
		if a[i] != b[j] {
			return false
		}
		i++
		j++
	}
}

// decWeightEqual reports whether two nonzero decompositions have the same
// weight. When both exponent literals fit, the weights are int64 and
// exact. Otherwise both sides rebuild their weight as an exact decimal
// term and compare those.
func decWeightEqual(a, b *decNumber) bool {
	if a.expFits && b.expFits {
		return a.weight == b.weight
	}
	an, aSmall, aMag, aDigits := decWeightTerm(a)
	bn, bSmall, bMag, bDigits := decWeightTerm(b)
	if aSmall != bSmall {
		// Canonical forms partition at 10^19: a small term is always below
		// it and a digit-string term always at or above it.
		return false
	}
	if aSmall {
		return an == bn && aMag == bMag
	}
	return an == bn && bytesEqualString(aDigits, ownedBytesString(bDigits))
}

// decWeightTermSmallLimit is the canonical-form boundary for weight terms:
// magnitudes below 10^19 are represented as a uint64, everything else as a
// decimal digit string with no leading zeros.
const decWeightTermSmallLimit uint64 = 10000000000000000000

// decWeightTerm evaluates a decomposition's exact weight — its spelled
// exponent plus its digit-layout adjustment — into canonical form: a sign,
// and either a uint64 magnitude (small true) or a decimal digit string
// (small false). A zero weight is (false, true, 0, nil). This is the cold
// path for exponent literals beyond eighteen digits; it may allocate.
func decWeightTerm(d *decNumber) (neg, small bool, mag uint64, digits []byte) {
	if len(d.expDigits) <= 19 {
		// The exponent magnitude fits a uint64 (10^19-1 < 2^64), and so
		// does the combined magnitude after the adjustment (|adj| < 2^32).
		var m uint64
		for _, c := range d.expDigits {
			m = m*10 + uint64(c-'0')
		}
		neg, mag = decSignedAdd(d.expNeg, m, d.adj)
		if mag < decWeightTermSmallLimit {
			return neg, true, mag, nil
		}
		return neg, false, 0, appendDecimalUint64(nil, mag)
	}
	// A twenty-digit or wider exponent magnitude is at least 10^19, so the
	// small adjustment can neither flip its sign nor reach zero.
	if d.expNeg == (d.adj < 0) {
		digits = decDigitsAddUint64(d.expDigits, absInt64(d.adj))
	} else {
		digits = decDigitsSubUint64(d.expDigits, absInt64(d.adj))
	}
	if len(digits) <= 19 {
		var m uint64
		for _, c := range digits {
			m = m*10 + uint64(c-'0')
		}
		if m < decWeightTermSmallLimit {
			return d.expNeg, true, m, nil
		}
	}
	return d.expNeg, false, 0, digits
}

// decSignedAdd computes ±m + adj exactly as a sign and uint64 magnitude.
// The caller guarantees m < 10^19 and |adj| < 2^32, so the result cannot
// overflow. A zero result normalizes to a positive sign.
func decSignedAdd(neg bool, m uint64, adj int64) (bool, uint64) {
	a := absInt64(adj)
	if neg == (adj < 0) {
		return neg && m+a != 0, m + a
	}
	if m >= a {
		return neg && m != a, m - a
	}
	return !neg, a - m
}

// absInt64 returns |v| as a uint64; the callers' values are far from the
// int64 minimum.
func absInt64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

// appendDecimalUint64 appends v's decimal digits to dst.
func appendDecimalUint64(dst []byte, v uint64) []byte {
	var buf [20]byte
	i := len(buf)
	for {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
		if v == 0 {
			break
		}
	}
	return append(dst, buf[i:]...)
}

// decDigitsAddUint64 returns digits + u as a decimal digit string with no
// leading zeros. digits must itself have no leading zeros.
func decDigitsAddUint64(digits []byte, u uint64) []byte {
	out := make([]byte, len(digits)+1)
	copy(out[1:], digits)
	out[0] = '0'
	for i := len(out) - 1; i >= 0 && u != 0; i-- {
		s := uint64(out[i]-'0') + u%10
		u /= 10
		if s >= 10 {
			s -= 10
			u++
		}
		out[i] = byte('0' + s)
	}
	if out[0] == '0' {
		return out[1:]
	}
	return out
}

// decDigitsSubUint64 returns digits - u as a decimal digit string with no
// leading zeros. The caller guarantees digits ≥ 10^19 > u, so the result
// is positive.
func decDigitsSubUint64(digits []byte, u uint64) []byte {
	out := make([]byte, len(digits))
	copy(out, digits)
	borrow := uint64(0)
	for i := len(out) - 1; i >= 0 && (u != 0 || borrow != 0); i-- {
		sub := u%10 + borrow
		u /= 10
		have := uint64(out[i] - '0')
		if have >= sub {
			out[i] = byte('0' + have - sub)
			borrow = 0
		} else {
			out[i] = byte('0' + have + 10 - sub)
			borrow = 1
		}
	}
	start := 0
	for start < len(out)-1 && out[start] == '0' {
		start++
	}
	return out[start:]
}
