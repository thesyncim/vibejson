package jsoncmp

import (
	"math/bits"

	"github.com/thesyncim/vibejson/x/byteview"
)

var pow10Table = [20]uint64{
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

const (
	decFlagZero uint8 = 1 << iota
	decFlagNeg
	decFlagExpFits
	decFlagExpNeg
)

// decNumber is a compact 32-byte exact decimal decomposition used only when
// either operand exceeds 19 significant digits or 17 exponent digits.
type decNumber struct {
	weight   int64 // when expFits: exp + adj; when !expFits: adj
	sigFirst int32
	sigLast  int32
	dot      int32
	expStart int32
	flags    uint8
}

func scanDigits(src []byte, i int) int {
	if len(src)-i >= 8 {
		return scanDigitsSIMD(src, i)
	}
	for i < len(src) && src[i]-'0' <= 9 {
		i++
	}
	return i
}

// parseFastDec parses a JSON number into exact (mant, exp) where value =
// (-1)^neg * mant * 10^exp, provided it has <= 19 significant digits and <= 17
// exponent digits. Returns (ok=false) on malformed input, or (ok=true, exact=false)
// when arbitrary-precision fallback is needed.
func parseFastDec(src []byte, start int) (mant uint64, exp int64, ok, exact bool) {
	n := len(src)
	i := start
	if i >= n {
		return 0, 0, false, false
	}
	sigDigits := 0
	if c := src[i]; c == '0' {
		i++
	} else if c-'0' <= 9 {
		mant = uint64(c - '0')
		sigDigits = 1
		i++
		for i < n {
			d := src[i] - '0'
			if d > 9 {
				break
			}
			sigDigits++
			if sigDigits > 19 {
				return 0, 0, true, false
			}
			mant = mant*10 + uint64(d)
			i++
		}
	} else {
		return 0, 0, false, false
	}

	var fracExp int64
	if i < n && src[i] == '.' {
		i++
		fracStart := i
		for i < n {
			d := src[i] - '0'
			if d > 9 {
				break
			}
			if mant != 0 || d != 0 {
				sigDigits++
				if sigDigits > 19 {
					return 0, 0, true, false
				}
				mant = mant*10 + uint64(d)
			}
			i++
		}
		if i == fracStart {
			return 0, 0, false, false
		}
		fracExp = -int64(i - fracStart)
	}

	var expVal int64
	if i < n {
		if src[i] != 'e' && src[i] != 'E' {
			return 0, 0, false, false
		}
		i++
		if i >= n {
			return 0, 0, false, false
		}
		expNeg := false
		if src[i] == '+' {
			i++
		} else if src[i] == '-' {
			expNeg = true
			i++
		}
		if i >= n || src[i]-'0' > 9 {
			return 0, 0, false, false
		}
		if mant == 0 {
			// Zero with any valid exponent is still zero.
			if scanDigits(src, i) != n {
				return 0, 0, false, false
			}
			return 0, 0, true, true
		}
		for i < n && src[i] == '0' {
			i++
		}
		expStart := i
		for i < n {
			d := src[i] - '0'
			if d > 9 {
				return 0, 0, false, false
			}
			if i-expStart >= 17 {
				return 0, 0, true, false
			}
			expVal = expVal*10 + int64(d)
			i++
		}
		if expNeg {
			expVal = -expVal
		}
	}
	if mant == 0 {
		return 0, 0, true, true
	}
	return mant, expVal + fracExp, true, true
}

func parseDecNumber(src []byte) (decNumber, bool) {
	n := len(src)
	if n == 0 {
		return decNumber{}, false
	}
	var flags uint8 = decFlagExpFits
	i := 0
	if src[0] == '-' {
		flags |= decFlagNeg
		i = 1
		if i >= n {
			return decNumber{}, false
		}
	}
	intStart := i
	i = scanDigits(src, i)
	intEnd := i
	if intEnd == intStart {
		return decNumber{}, false
	}
	dot := int32(-1)
	fracStart, fracEnd := 0, 0
	if i < n && src[i] == '.' {
		dot = int32(i)
		i++
		fracStart = i
		i = scanDigits(src, i)
		fracEnd = i
		if fracEnd == fracStart {
			return decNumber{}, false
		}
	}
	var exp int64
	expStart := n
	if i < n {
		if src[i] != 'e' && src[i] != 'E' {
			return decNumber{}, false
		}
		i++
		if i >= n {
			return decNumber{}, false
		}
		if src[i] == '+' {
			i++
		} else if src[i] == '-' {
			flags |= decFlagExpNeg
			i++
		}
		if i >= n || src[i]-'0' > 9 {
			return decNumber{}, false
		}
		for i < n && src[i] == '0' {
			i++
		}
		expStart = i
		i = scanDigits(src, i)
		if i != n {
			return decNumber{}, false
		}
		if n-expStart <= 18 {
			for j := expStart; j < n; j++ {
				exp = exp*10 + int64(src[j]-'0')
			}
			if flags&decFlagExpNeg != 0 {
				exp = -exp
			}
		} else {
			flags &^= decFlagExpFits
		}
	}

	var sigFirst int
	var adj int64
	if src[intStart] != '0' {
		sigFirst = intStart
		adj = int64(intEnd-intStart) - 1
	} else {
		sigFirst = -1
		for j := fracStart; j < fracEnd; j++ {
			if src[j] != '0' {
				sigFirst = j
				adj = -int64(j-fracStart) - 1
				break
			}
		}
		if sigFirst < 0 {
			return decNumber{flags: flags | decFlagZero}, true
		}
	}

	weight := adj
	if flags&decFlagExpFits != 0 {
		weight = exp + adj
	}

	sigLast := -1
	for j := fracEnd - 1; j >= fracStart; j-- {
		if src[j] != '0' {
			sigLast = j
			break
		}
	}
	if sigLast < 0 {
		for j := intEnd - 1; j >= intStart; j-- {
			if src[j] != '0' {
				sigLast = j
				break
			}
		}
	}
	return decNumber{
		weight:   weight,
		sigFirst: int32(sigFirst),
		sigLast:  int32(sigLast),
		dot:      dot,
		expStart: int32(expStart),
		flags:    flags,
	}, true
}

// NumberEqual reports whether two validated RFC 8259 JSON number tokens
// represent the exact same mathematical rational value (for example -0 == 0,
// 100 == 1e2 == 1.00e+2, and arbitrary-precision decimals beyond float64).
//
// Comparisons allocate nothing for all numbers whose exponent fits in 18
// decimal digits. Empty or malformed inputs return false without panicking.
func NumberEqual(a, b []byte) bool {
	if len(a) != 0 && byteview.String(a) == byteview.String(b) {
		return true
	}
	return numberEqualSlow(a, b)
}

func numberEqualSlow(a, b []byte) bool {
	if len(a) == 0 || len(b) == 0 {
		return false
	}
	aNeg := a[0] == '-'
	ai := 0
	if aNeg {
		if len(a) == 1 {
			return false
		}
		ai = 1
	}
	bNeg := b[0] == '-'
	bi := 0
	if bNeg {
		if len(b) == 1 {
			return false
		}
		bi = 1
	}
	ac, bc := a[ai], b[bi]
	if ac != '0' && bc != '0' {
		if aNeg != bNeg || ac != bc {
			return false
		}
	} else if len(a)-ai == 1 && len(b)-bi == 1 {
		return ac == '0' && bc == '0'
	}

	// Fast-reject distinct plain integers of length >= 8 via SWAR/SIMD without
	// multiplying digits by 10, and skip parseFastDec for >19-digit numbers.
	if ac != '0' && bc != '0' && len(a)-ai >= 8 && len(b)-bi >= 8 {
		if scanDigits(a, ai) == len(a) && scanDigits(b, bi) == len(b) {
			return false
		}
	}

	// Fast path for all numbers fitting in 19 significant digits and 17
	// exponent digits (~99.99% of JSON numbers).
	if len(a)-ai <= 19 && len(b)-bi <= 19 {
		mantA, expA, okA, exactA := parseFastDec(a, ai)
		if !okA {
			return false
		}
		if exactA {
			mantB, expB, okB, exactB := parseFastDec(b, bi)
			if !okB {
				return false
			}
			if exactB {
				if mantA == 0 || mantB == 0 {
					return mantA == mantB
				}
				if aNeg != bNeg {
					return false
				}
				if expA == expB {
					return mantA == mantB
				}
				if expA > expB {
					d := uint64(expA - expB)
					if d > 19 {
						return false
					}
					hi, lo := bits.Mul64(mantA, pow10Table[d])
					return hi == 0 && lo == mantB
				}
				d := uint64(expB - expA)
				if d > 19 {
					return false
				}
				hi, lo := bits.Mul64(mantB, pow10Table[d])
				return hi == 0 && lo == mantA
			}
		}
	}

	// Arbitrary-precision fallback.
	da, aok := parseDecNumber(a)
	if !aok {
		return false
	}
	db, bok := parseDecNumber(b)
	if !bok {
		return false
	}
	aZero := da.flags&decFlagZero != 0
	bZero := db.flags&decFlagZero != 0
	if aZero || bZero {
		return aZero == bZero
	}
	if (da.flags^db.flags)&decFlagNeg != 0 || !decWeightEqual(a, &da, b, &db) {
		return false
	}

	var a1, a2 []byte
	if da.dot > da.sigFirst && da.dot < da.sigLast {
		a1 = a[da.sigFirst:da.dot]
		a2 = a[da.dot+1 : da.sigLast+1]
	} else {
		a1 = a[da.sigFirst : da.sigLast+1]
	}
	var b1, b2 []byte
	if db.dot > db.sigFirst && db.dot < db.sigLast {
		b1 = b[db.sigFirst:db.dot]
		b2 = b[db.dot+1 : db.sigLast+1]
	} else {
		b1 = b[db.sigFirst : db.sigLast+1]
	}
	if len(a1)+len(a2) != len(b1)+len(b2) {
		return false
	}
	switch {
	case len(a1) == len(b1):
		return byteview.String(a1) == byteview.String(b1) &&
			byteview.String(a2) == byteview.String(b2)
	case len(a1) < len(b1):
		k := len(a1)
		rem := len(b1) - k
		return byteview.String(a1) == byteview.String(b1[:k]) &&
			byteview.String(a2[:rem]) == byteview.String(b1[k:]) &&
			byteview.String(a2[rem:]) == byteview.String(b2)
	default:
		k := len(b1)
		rem := len(a1) - k
		return byteview.String(a1[:k]) == byteview.String(b1) &&
			byteview.String(a1[k:]) == byteview.String(b2[:rem]) &&
			byteview.String(a2) == byteview.String(b2[rem:])
	}
}

func decWeightEqual(aSrc []byte, a *decNumber, bSrc []byte, b *decNumber) bool {
	if (a.flags&b.flags)&decFlagExpFits != 0 {
		return a.weight == b.weight
	}
	an, aSmall, aMag, aDigits := decWeightTerm(aSrc, a)
	bn, bSmall, bMag, bDigits := decWeightTerm(bSrc, b)
	if aSmall != bSmall {
		return false
	}
	if aSmall {
		return an == bn && aMag == bMag
	}
	return an == bn && byteview.String(aDigits) == byteview.String(bDigits)
}

const decWeightTermSmallLimit uint64 = 10000000000000000000

func decWeightTerm(src []byte, d *decNumber) (neg, small bool, mag uint64, digits []byte) {
	if d.flags&decFlagExpFits != 0 {
		if d.weight < 0 {
			return true, true, uint64(-d.weight), nil
		}
		return false, true, uint64(d.weight), nil
	}
	expDigits := src[d.expStart:]
	expNeg := d.flags&decFlagExpNeg != 0
	adj := d.weight
	if len(expDigits) <= 19 {
		var m uint64
		for _, c := range expDigits {
			m = m*10 + uint64(c-'0')
		}
		neg, mag = decSignedAdd(expNeg, m, adj)
		if mag < decWeightTermSmallLimit {
			return neg, true, mag, nil
		}
		return neg, false, 0, appendDecimalUint64(nil, mag)
	}
	if expNeg == (adj < 0) {
		digits = decDigitsAddUint64(expDigits, absInt64(adj))
	} else {
		digits = decDigitsSubUint64(expDigits, absInt64(adj))
	}
	if len(digits) <= 19 {
		var m uint64
		for _, c := range digits {
			m = m*10 + uint64(c-'0')
		}
		if m < decWeightTermSmallLimit {
			return expNeg, true, m, nil
		}
	}
	return expNeg, false, 0, digits
}

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

func absInt64(v int64) uint64 {
	if v < 0 {
		return uint64(-v)
	}
	return uint64(v)
}

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
