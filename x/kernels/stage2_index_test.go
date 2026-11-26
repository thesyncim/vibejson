package kernels

import (
	"math/bits"
	"testing"

	"github.com/thesyncim/vibejson/document"
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
	var entries [3]document.IndexEntry
	var state Stage2IndexState
	Stage2IndexReset(&state)

	Stage2IndexPositionsFused(
		src, positions[:2], &slab, entries[:], &state,
	)
	if state.Bad != 0 || state.Depth != 1 {
		t.Fatalf("prefix state = bad %#x depth %d", state.Bad, state.Depth)
	}
	state.Count = Stage2IndexMaxCount - 1
	Stage2IndexPositionsFused(
		src, positions[2:], &slab, entries[:], &state,
	)
	if state.Bad&Stage2IndexCount == 0 {
		t.Fatalf("overflow state bad = %#x, missing count flag", state.Bad)
	}
	info := entries[0].Info
	if kind := info & (7 << 26); kind != Stage2IndexInfoArray {
		t.Fatalf("overflow changed array kind bits to %#x (info %#x)", kind, info)
	}
	if count := info & uint32(Stage2IndexMaxCount); count != 0 {
		t.Fatalf("overflow count = %d, want masked zero", count)
	}
}

func TestStage2IndexNativeEntryLayout(t *testing.T) {
	src := []byte(`[1,"abc",true,false,null,{"key":"value"}]`)
	positions := []uint32{0, 1, 2, 3, 7, 8, 9, 13, 14, 19, 20, 24, 25, 26, 30, 31, 32, 38, 39, 40}
	want := []document.IndexEntry{
		{Start: 0, End: 41, Next: 9, Info: Stage2IndexInfoArray | 6},
		{Start: 1, End: 2, Next: 1, Info: Stage2IndexInfoNumber | Stage2IndexIntFlag},
		{Start: 3, End: 8, Next: 1, Info: Stage2IndexInfoString},
		{Start: 9, End: 13, Next: 1, Info: Stage2IndexInfoBool},
		{Start: 14, End: 19, Next: 1, Info: Stage2IndexInfoBool},
		{Start: 20, End: 24, Next: 1, Info: Stage2IndexInfoNull},
		{Start: 25, End: 40, Next: 3, Info: Stage2IndexInfoObject | 1},
		{Start: 26, End: 31, Next: 1, Info: Stage2IndexInfoString | Stage2IndexKeyFlag},
		{Start: 32, End: 39, Next: 1, Info: Stage2IndexInfoString},
	}
	// Splitting at every position exercises both packed position pairs and
	// single-position resumptions, including keys and string closers.
	for _, objectStringFast := range []uint64{0, 1} {
		for split := 0; split <= len(positions); split++ {
			entries := make([]document.IndexEntry, len(want))
			var slab [Stage2IndexSlabLen]uint64
			var state Stage2IndexState
			Stage2IndexReset(&state)
			state.ObjectStringFast = objectStringFast
			for _, chunk := range [][]uint32{positions[:split], positions[split:]} {
				Stage2IndexPositionsFused(
					src, chunk, &slab, entries, &state,
				)
			}
			if !Stage2IndexFinish(&state) || state.EntryOff != uint64(len(want)*16) {
				t.Fatalf("fast %d split %d: state %+v", objectStringFast, split, state)
			}
			for i := range want {
				if entries[i] != want[i] {
					t.Fatalf("fast %d split %d entry %d = %#v, want %#v", objectStringFast, split, i, entries[i], want[i])
				}
			}
		}
	}
}
