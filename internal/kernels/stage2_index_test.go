package kernels

import (
	"math/bits"
	"testing"
)

func TestStage2NonDigitMaskMatchesBytewise(t *testing.T) {
	const digits = uint64(0x3535353535353535)
	for value := range 256 {
		want := '0' <= byte(value) && byte(value) <= '9'
		for lane := range 8 {
			shift := uint(lane * 8)
			x := digits&^(uint64(0xff)<<shift) | uint64(value)<<shift
			if got := stage2NonDigitMask8(x) == 0; got != want {
				t.Fatalf("lane %d byte %#02x = %v, want %v", lane, value, got, want)
			}
		}
	}

	state := uint64(0x243f6a8885a308d3)
	for range 1_000_000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		want := 8
		for lane := range 8 {
			c := byte(state >> uint(lane*8))
			if want == 8 && (c < '0' || c > '9') {
				want = lane
			}
		}
		got := 8
		if invalid := stage2NonDigitMask8(state); invalid != 0 {
			got = bits.TrailingZeros64(invalid) >> 3
		}
		if got != want {
			t.Fatalf("stage2NonDigitMask8(%#016x) prefix = %d, want %d", state, got, want)
		}
	}
}
