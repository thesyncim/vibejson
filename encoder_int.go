package vibejson

import (
	"math/bits"
	"strconv"

	simdkernels "github.com/thesyncim/vibejson/simd"
)

var encodeDigitPairTable = [100][2]byte{
	{'0', '0'}, {'0', '1'}, {'0', '2'}, {'0', '3'}, {'0', '4'}, {'0', '5'}, {'0', '6'}, {'0', '7'}, {'0', '8'}, {'0', '9'},
	{'1', '0'}, {'1', '1'}, {'1', '2'}, {'1', '3'}, {'1', '4'}, {'1', '5'}, {'1', '6'}, {'1', '7'}, {'1', '8'}, {'1', '9'},
	{'2', '0'}, {'2', '1'}, {'2', '2'}, {'2', '3'}, {'2', '4'}, {'2', '5'}, {'2', '6'}, {'2', '7'}, {'2', '8'}, {'2', '9'},
	{'3', '0'}, {'3', '1'}, {'3', '2'}, {'3', '3'}, {'3', '4'}, {'3', '5'}, {'3', '6'}, {'3', '7'}, {'3', '8'}, {'3', '9'},
	{'4', '0'}, {'4', '1'}, {'4', '2'}, {'4', '3'}, {'4', '4'}, {'4', '5'}, {'4', '6'}, {'4', '7'}, {'4', '8'}, {'4', '9'},
	{'5', '0'}, {'5', '1'}, {'5', '2'}, {'5', '3'}, {'5', '4'}, {'5', '5'}, {'5', '6'}, {'5', '7'}, {'5', '8'}, {'5', '9'},
	{'6', '0'}, {'6', '1'}, {'6', '2'}, {'6', '3'}, {'6', '4'}, {'6', '5'}, {'6', '6'}, {'6', '7'}, {'6', '8'}, {'6', '9'},
	{'7', '0'}, {'7', '1'}, {'7', '2'}, {'7', '3'}, {'7', '4'}, {'7', '5'}, {'7', '6'}, {'7', '7'}, {'7', '8'}, {'7', '9'},
	{'8', '0'}, {'8', '1'}, {'8', '2'}, {'8', '3'}, {'8', '4'}, {'8', '5'}, {'8', '6'}, {'8', '7'}, {'8', '8'}, {'8', '9'},
	{'9', '0'}, {'9', '1'}, {'9', '2'}, {'9', '3'}, {'9', '4'}, {'9', '5'}, {'9', '6'}, {'9', '7'}, {'9', '8'}, {'9', '9'},
}

func storeCompactDigitPair(dst *[2]byte, pair uint64) {
	*dst = encodeDigitPairTable[pair]
}

// appendCompactUint formats v with two digits per store for 0..999999 in one
// capacity check, dispatching larger values to appendCompactUintSlow.
func appendCompactUint(dst []byte, v uint64) []byte {
	if v < 10 {
		return append(dst, byte('0'+v))
	}
	if v < 1000000 {
		if cap(dst)-len(dst) < 6 {
			return strconv.AppendUint(dst, v, 10)
		}
		start := len(dst)
		out := dst[start : start+6]
		_ = out[5]
		if v < 100 {
			p := encodeDigitPairTable[v]
			out[0], out[1] = p[0], p[1]
			return dst[:start+2]
		}
		if v < 10000 {
			q := v / 100
			r := v - q*100
			pr := encodeDigitPairTable[r]
			if v < 1000 {
				out[0] = byte('0' + q)
				out[1], out[2] = pr[0], pr[1]
				return dst[:start+3]
			}
			pq := encodeDigitPairTable[q]
			out[0], out[1] = pq[0], pq[1]
			out[2], out[3] = pr[0], pr[1]
			return dst[:start+4]
		}
		q := v / 10000
		rem := v - q*10000
		q2 := rem / 100
		r2 := rem - q2*100
		pq2 := encodeDigitPairTable[q2]
		pr2 := encodeDigitPairTable[r2]
		if v < 100000 {
			out[0] = byte('0' + q)
			out[1], out[2] = pq2[0], pq2[1]
			out[3], out[4] = pr2[0], pr2[1]
			return dst[:start+5]
		}
		pq := encodeDigitPairTable[q]
		out[0], out[1] = pq[0], pq[1]
		out[2], out[3] = pq2[0], pq2[1]
		out[4], out[5] = pr2[0], pr2[1]
		return dst[:start+6]
	}
	return appendCompactUintSlow(dst, v)
}

//go:noinline
func appendCompactUintSlow(dst []byte, v uint64) []byte {
	if v >= 1e10 {
		return appendCompactUintLarge(dst, v)
	}
	if v >= 1e9 {
		return appendCompactUint10(dst, v)
	}
	if v >= 1e8 {
		return appendCompactUint9(dst, v)
	}
	// Provenance unresolved: repository history says this digit-count
	// approximation was borrowed, but did not record its source. Do not infer
	// one; see docs/provenance.md.
	// (bits.Len64(v)*1233)>>12 approximates floor(log10(v)) via
	// log10(2) ~= 1233/4096; callers' range guards absorb the boundary
	// cases.
	digits := int((bits.Len64(v)*1233)>>12) + 1
	if v < pow10Uint64[digits-1] {
		digits--
	}
	if cap(dst)-len(dst) < digits {
		return strconv.AppendUint(dst, v, 10)
	}
	start := len(dst)
	dst = dst[:start+digits]
	i := len(dst)
	if digits == 8 {
		simdkernels.Store8Digits((*[8]byte)(dst[start:start+8]), v)
		return dst
	}
	switch {
	case v >= 1e8:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(dst[i:i+2]), pair)
		fallthrough
	case v >= 1e6:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(dst[i:i+2]), pair)
		fallthrough
	case v >= 1e4:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(dst[i:i+2]), pair)
		fallthrough
	case v >= 100:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(dst[i:i+2]), pair)
	}
	if v >= 10 {
		i -= 2
		storeCompactDigitPair((*[2]byte)(dst[i:i+2]), v)
	} else {
		i--
		dst[i] = byte('0' + v)
	}
	return dst
}

// appendCompactUint9 stays out of line so this rare width does not further
// enlarge the common formatter or wrappers that inline around its call.
//
//go:noinline
func appendCompactUint9(dst []byte, v uint64) []byte {
	if cap(dst)-len(dst) < 9 {
		return strconv.AppendUint(dst, v, 10)
	}
	hi := v / 1e8
	lo := v - hi*1e8
	start := len(dst)
	dst = dst[:start+9]
	dst[len(dst)-9] = byte('0' + hi)
	simdkernels.Store8Digits((*[8]byte)(dst[len(dst)-8:]), lo)
	return dst
}

// appendCompactUint10 stays out of line so this rare width does not further
// enlarge the common formatter or wrappers that inline around its call.
//
//go:noinline
func appendCompactUint10(dst []byte, v uint64) []byte {
	if cap(dst)-len(dst) < 10 {
		return strconv.AppendUint(dst, v, 10)
	}
	hi := v / 1e8
	lo := v - hi*1e8
	start := len(dst)
	dst = dst[:start+10]
	storeCompactDigitPair((*[2]byte)(dst[start:start+2]), hi)
	simdkernels.Store8Digits((*[8]byte)(dst[start+2:start+10]), lo)
	return dst
}

//go:noinline
func appendCompactUintLarge(dst []byte, v uint64) []byte {
	digits := int((bits.Len64(v)*1233)>>12) + 1
	if v < pow10Uint64[digits-1] {
		digits--
	}
	if cap(dst)-len(dst) < digits {
		return strconv.AppendUint(dst, v, 10)
	}
	if v < 1e16 {
		var block [16]byte
		simdkernels.Store16Digits(&block, v)
		return append(dst, block[16-digits:]...)
	}
	hi := v / 1e16
	lo := v - hi*1e16
	dst = appendCompactUint(dst, hi)
	start := len(dst)
	dst = dst[:start+16]
	simdkernels.Store16Digits((*[16]byte)(dst[start:]), lo)
	return dst
}

func appendCompactInt(dst []byte, v int64) []byte {
	u := uint64(v)
	if v < 0 {
		dst = append(dst, '-')
		u = uint64(-v)
	}
	return appendCompactUint(dst, u)
}

// appendCommaCompactUint emits ",digits" with one capacity check, so slice
// loops pay no separate separator append per element.
func appendCommaCompactUint(dst []byte, v uint64) []byte {
	if v < 10 {
		return append(dst, ',', byte('0'+v))
	}
	if v < 100 {
		p := encodeDigitPairTable[v]
		return append(dst, ',', p[0], p[1])
	}
	// The digit-count formula requires v >= 100, proved above.
	digits := int((bits.Len64(v)*1233)>>12) + 1
	if v < pow10Uint64[digits-1] {
		digits--
	}
	if v >= 1e16 || cap(dst)-len(dst) < digits+1 {
		dst = append(dst, ',')
		return appendCompactUint(dst, v)
	}
	var block [16]byte
	simdkernels.Store16Digits(&block, v)
	start := len(dst)
	dst = dst[:start+digits+1]
	dst[start] = ','
	copy(dst[start+1:], block[16-digits:])
	return dst
}

func appendCommaCompactInt(dst []byte, v int64) []byte {
	if v < 0 {
		dst = append(dst, ',', '-')
		return appendCompactUint(dst, uint64(-v))
	}
	return appendCommaCompactUint(dst, uint64(v))
}
