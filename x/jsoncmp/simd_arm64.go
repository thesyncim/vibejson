//go:build !go1.28 && goexperiment.simd && arm64

package jsoncmp

import (
	"encoding/binary"
	"math/bits"
	"simd/archsimd"
)

func scanDigitsSIMD(src []byte, i int) int {
	rem := src[i:]
	zero := archsimd.BroadcastUint8x16('0')
	nine := archsimd.BroadcastUint8x16(9)
	for len(rem) >= 16 {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(rem))
		if v.Sub(zero).Greater(nine).ToInt8x16().ToBits().ReduceMax() != 0 {
			break
		}
		rem = rem[16:]
		i += 16
	}
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
	i := 0
	rem := b
	slash := archsimd.BroadcastUint8x16('\\')
	for len(rem) >= 32 {
		v0 := archsimd.LoadUint8x16Array((*[16]uint8)(rem))
		v1 := archsimd.LoadUint8x16Array((*[16]uint8)(rem[16:]))
		m0 := v0.Equal(slash).ToInt8x16().ToBits()
		m1 := v1.Equal(slash).ToInt8x16().ToBits()
		if m0.Or(m1).ReduceMax() != 0 {
			if m0.ReduceMax() == 0 {
				rem = rem[16:]
				i += 16
			}
			break
		}
		rem = rem[32:]
		i += 32
	}
	if len(rem) >= 16 {
		v := archsimd.LoadUint8x16Array((*[16]uint8)(rem))
		if v.Equal(slash).ToInt8x16().ToBits().ReduceMax() == 0 {
			rem = rem[16:]
			i += 16
		}
	}
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
