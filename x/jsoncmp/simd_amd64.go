//go:build !go1.28 && goexperiment.simd && amd64

package jsoncmp

import (
	"encoding/binary"
	"math/bits"
	"simd/archsimd"
)

var hasAVX2 = archsimd.X86.AVX2()

func scanDigitsSIMD(src []byte, i int) int {
	if len(src)-i >= 32 && hasAVX2 {
		return scanDigitsAVX2(src, i)
	}
	rem := src[i:]
	for len(rem) >= 8 {
		w := binary.LittleEndian.Uint64(rem)
		if ((w & 0x8080808080808080) |
			((w + 0x4646464646464646) & 0x8080808080808080) |
			(^((w | 0x8080808080808080) - 0x3030303030303030) & 0x8080808080808080)) != 0 {
			break
		}
		rem = rem[8:]
		i += 8
	}
	for len(rem) > 0 && rem[0]-'0' <= 9 {
		rem = rem[1:]
		i++
	}
	return i
}

//go:noinline
func scanDigitsAVX2(src []byte, i int) int {
	rem := src[i:]
	zero := archsimd.BroadcastUint8x32('0')
	nine := archsimd.BroadcastUint8x32(9)
	for len(rem) >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(rem))
		if m := v.Sub(zero).Greater(nine).ToBits(); m != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(m)
		}
		rem = rem[32:]
		i += 32
	}
	archsimd.ClearAVXUpperBits()
	for len(rem) >= 8 {
		w := binary.LittleEndian.Uint64(rem)
		if ((w & 0x8080808080808080) |
			((w + 0x4646464646464646) & 0x8080808080808080) |
			(^((w | 0x8080808080808080) - 0x3030303030303030) & 0x8080808080808080)) != 0 {
			break
		}
		rem = rem[8:]
		i += 8
	}
	for len(rem) > 0 && rem[0]-'0' <= 9 {
		rem = rem[1:]
		i++
	}
	return i
}

func indexBackslashSIMD(b []byte) int {
	if len(b) >= 32 && hasAVX2 {
		return indexBackslashAVX2(b)
	}
	i := 0
	rem := b
	for len(rem) >= 8 {
		w := binary.LittleEndian.Uint64(rem)
		x := w ^ 0x5c5c5c5c5c5c5c5c
		if m := (x - 0x0101010101010101) &^ x & 0x8080808080808080; m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		rem = rem[8:]
		i += 8
	}
	for j := 0; j < len(rem); j++ {
		if rem[j] == '\\' {
			return i + j
		}
	}
	return -1
}

//go:noinline
func indexBackslashAVX2(b []byte) int {
	i := 0
	rem := b
	slash := archsimd.BroadcastUint8x32('\\')
	for len(rem) >= 64 {
		v0 := archsimd.LoadUint8x32Array((*[32]uint8)(rem))
		v1 := archsimd.LoadUint8x32Array((*[32]uint8)(rem[32:]))
		m0 := v0.Equal(slash).ToBits()
		m1 := v1.Equal(slash).ToBits()
		if m0|m1 != 0 {
			archsimd.ClearAVXUpperBits()
			if m0 != 0 {
				return i + bits.TrailingZeros32(m0)
			}
			return i + 32 + bits.TrailingZeros32(m1)
		}
		rem = rem[64:]
		i += 64
	}
	if len(rem) >= 32 {
		v := archsimd.LoadUint8x32Array((*[32]uint8)(rem))
		if m := v.Equal(slash).ToBits(); m != 0 {
			archsimd.ClearAVXUpperBits()
			return i + bits.TrailingZeros32(m)
		}
		rem = rem[32:]
		i += 32
	}
	archsimd.ClearAVXUpperBits()
	for len(rem) >= 8 {
		w := binary.LittleEndian.Uint64(rem)
		x := w ^ 0x5c5c5c5c5c5c5c5c
		if m := (x - 0x0101010101010101) &^ x & 0x8080808080808080; m != 0 {
			return i + bits.TrailingZeros64(m)>>3
		}
		rem = rem[8:]
		i += 8
	}
	for j := 0; j < len(rem); j++ {
		if rem[j] == '\\' {
			return i + j
		}
	}
	return -1
}
