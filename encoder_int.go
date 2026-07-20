package simdjson

import (
	"math/bits"
	"strconv"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

const encodeDigitPairs = "" +
	"00010203040506070809" +
	"10111213141516171819" +
	"20212223242526272829" +
	"30313233343536373839" +
	"40414243444546474849" +
	"50515253545556575859" +
	"60616263646566676869" +
	"70717273747576777879" +
	"80818283848586878889" +
	"90919293949596979899"

func storeCompactDigitPair(dst *[2]byte, pair uint64) {
	// Callers prove pair < 100. The destination type proves the two-byte write.
	src := unsafe.Add(unsafe.Pointer(unsafe.StringData(encodeDigitPairs)), pair*2)
	*dst = *(*[2]byte)(src)
}

// appendCompactUint formats v with two digits per store. It beats the
// general strconv path on the short integers that dominate JSON documents.
func appendCompactUint(dst []byte, v uint64) []byte {
	if v < 10 {
		return append(dst, byte('0'+v))
	}
	if v < 100 {
		return append(dst, encodeDigitPairs[v*2], encodeDigitPairs[v*2+1])
	}
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
	source := byteSourceOf(dst)
	if digits == 8 {
		simdkernels.Store8Digits((*[8]byte)(source.pointerAt(start)), v)
		return dst
	}
	switch {
	case v >= 1e8:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(source.pointerAt(i)), pair)
		fallthrough
	case v >= 1e6:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(source.pointerAt(i)), pair)
		fallthrough
	case v >= 1e4:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(source.pointerAt(i)), pair)
		fallthrough
	case v >= 100:
		pair := v % 100
		v /= 100
		i -= 2
		storeCompactDigitPair((*[2]byte)(source.pointerAt(i)), pair)
	}
	if v >= 10 {
		i -= 2
		storeCompactDigitPair((*[2]byte)(source.pointerAt(i)), v)
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
	source := byteSourceOf(dst)
	storeCompactDigitPair((*[2]byte)(source.pointerAt(start)), hi)
	simdkernels.Store8Digits((*[8]byte)(source.pointerAt(start+2)), lo)
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
	if v < 0 {
		dst = append(dst, '-')
		return appendCompactUint(dst, uint64(-v))
	}
	return appendCompactUint(dst, uint64(v))
}

// appendCommaCompactUint emits ",digits" with one capacity check, so slice
// loops pay no separate separator append per element.
func appendCommaCompactUint(dst []byte, v uint64) []byte {
	if v < 10 {
		return append(dst, ',', byte('0'+v))
	}
	if v < 100 {
		return append(dst, ',', encodeDigitPairs[v*2], encodeDigitPairs[v*2+1])
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
