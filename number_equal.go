package vibejson

// decNumber is an exact, source-backed decimal decomposition.
type decNumber struct {
	zero              bool
	neg               bool
	sigFirst, sigLast int
	dot               int
	weight            int64
	expFits           bool
	expNeg            bool
	expDigits         []byte
	adj               int64
}

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
		d.weight = exp + d.adj
	}

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

// JSONNumberEqual reports whether two validated JSON number spellings agree.
func JSONNumberEqual(a, b []byte) bool {
	if BytesEqualString(a, OwnedBytesString(b)) {
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

func decWeightEqual(a, b *decNumber) bool {
	if a.expFits && b.expFits {
		return a.weight == b.weight
	}
	an, aSmall, aMag, aDigits := decWeightTerm(a)
	bn, bSmall, bMag, bDigits := decWeightTerm(b)
	if aSmall != bSmall {
		return false
	}
	if aSmall {
		return an == bn && aMag == bMag
	}
	return an == bn && BytesEqualString(aDigits, OwnedBytesString(bDigits))
}

const decWeightTermSmallLimit uint64 = 10000000000000000000

func decWeightTerm(d *decNumber) (neg, small bool, mag uint64, digits []byte) {
	if len(d.expDigits) <= 19 {
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
