//go:build !goexperiment.simd || go1.28 || (!arm64 && !amd64)

package jsoncmp

import (
	"encoding/binary"
	"math/bits"
)

func scanDigitsSIMD(src []byte, i int) int {
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

func indexBackslashSIMD(b []byte) int {
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
