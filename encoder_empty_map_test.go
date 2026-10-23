package vibejson

import (
	"fmt"
	"testing"
	"unsafe"
)

func TestEmptyMapEncoderParity(t *testing.T) {
	type namedMap map[string]int
	for _, value := range []namedMap{nil, {}, {"one": 1}} {
		checkEncoderParity(t, "named map", value)
		checkEncoderParity[any](t, "interface map", value)
		checkEncoderParity(t, "nested maps", struct {
			Value namedMap `json:"value"`
			Omit  namedMap `json:"omit,omitempty"`
		}{value, value})
	}
	checkEncoderParity(t, "empty numeric keys", map[int]string{})
}

func TestEmptyMapEncoderDepthBoundary(t *testing.T) {
	encoder := mustCompileTestEncoder[map[string]int](t, EncoderOptions{})
	for _, depth := range []int{DefaultMaxDepth - 1, DefaultMaxDepth} {
		for _, value := range []map[string]int{nil, {}} {
			t.Run(fmt.Sprintf("depth=%d/nil=%v", depth, value == nil), func(t *testing.T) {
				state := encodeState{dst: []byte("prefix:"), depth: depth, escapeHTML: true}
				err := state.encodeMap(encoder.root, unsafe.Pointer(&value))
				wantOK := value == nil || depth < DefaultMaxDepth
				if (err == nil) != wantOK {
					t.Fatalf("encode error = %v, want success %v", err, wantOK)
				}
				want := "prefix:"
				if wantOK {
					if value == nil {
						want += "null"
					} else {
						want += "{}"
					}
				}
				if string(state.dst) != want || state.depth != depth {
					t.Fatalf("result = %s, depth %d; want %s, depth %d", state.dst, state.depth, want, depth)
				}
			})
		}
	}
}

func TestEmptyMapEncoderPreservesWarmScratch(t *testing.T) {
	encoder := mustCompileTestEncoder[map[string]*uint64](t, EncoderOptions{})
	scratch, _ := dedicatedEncoderScratch(&encoder)
	large := make(map[string]*uint64)
	for i := range 64 {
		number := uint64(i)
		large[fmt.Sprint(i)] = &number
	}
	if _, err := encoder.AppendJSON(nil, &large); err != nil {
		t.Fatal(err)
	}
	assertEncoderScratchCleared(t, scratch)
	backing := scratch.valueBacking.Pointer()
	entries := cap(scratch.mapEntries)

	empty := map[string]*uint64{}
	for range 3 {
		out, err := encoder.AppendJSON(nil, &empty)
		if err != nil || string(out) != "{}" {
			t.Fatalf("empty encode = %s, %v", out, err)
		}
		assertEncoderScratchCleared(t, scratch)
		if scratch.valueBacking.Pointer() != backing || cap(scratch.mapEntries) != entries {
			t.Fatal("empty map discarded warm scratch storage")
		}
	}
	one := uint64(1)
	small := map[string]*uint64{"one": &one}
	out, err := encoder.AppendJSON(nil, &small)
	if err != nil || string(out) != `{"one":1}` {
		t.Fatalf("encode after empty maps = %s, %v", out, err)
	}
	assertEncoderScratchCleared(t, scratch)
}
