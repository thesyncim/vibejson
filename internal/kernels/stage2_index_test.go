package kernels

import (
	"encoding/binary"
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

func TestStage2IndexRejectsMemberCountOverflowWithoutKindBleed(t *testing.T) {
	src := []byte(`[0,1]`)
	positions := [...]uint32{0, 1, 2, 3, 4}
	var slab [Stage2IndexSlabLen]uint64
	var entries [3][16]byte
	var state Stage2IndexState
	Stage2IndexReset(&state)

	// Enter the array and finish its first scalar, then place the resumable
	// machine one comma below the tape limit. The next comma makes the
	// following value unrepresentable without allocating a 27th count bit.
	Stage2IndexPositionsFused(
		&src[0], len(src), positions[:2], &slab,
		&entries[0][0], len(entries), &state,
	)
	if state.Bad != 0 || state.Depth != 1 {
		t.Fatalf("prefix state = bad %#x depth %d", state.Bad, state.Depth)
	}
	state.Count = Stage2IndexMaxCount - 1
	Stage2IndexPositionsFused(
		&src[0], len(src), positions[2:], &slab,
		&entries[0][0], len(entries), &state,
	)
	if state.Bad&Stage2IndexCount == 0 {
		t.Fatalf("overflow state bad = %#x, missing count flag", state.Bad)
	}
	info := binary.LittleEndian.Uint32(entries[0][12:16])
	if kind := info & (7 << 26); kind != Stage2IndexInfoArray {
		t.Fatalf("overflow changed array kind bits to %#x (info %#x)", kind, info)
	}
	if count := info & uint32(Stage2IndexMaxCount); count != 0 {
		t.Fatalf("overflow count = %d, want masked zero", count)
	}
}
