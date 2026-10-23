package vibejson

import (
	"math"
	"strconv"
	"strings"
	"testing"
)

func TestInlineEncoderScratchAfterLargeMap(t *testing.T) {
	encoder := mustInlineEncoder[inlineScratchPointers](t, EncoderOptions{InlineFields: true})
	scratch, _ := dedicatedEncoderScratch(&encoder)
	large := inlineScratchPointerMap(8192)
	if _, err := encoder.AppendJSON(nil, &large); err != nil {
		t.Fatal(err)
	}
	assertEncoderScratchCleared(t, scratch)
	if scratch.valueBacking.Len() != len(large.Extra) {
		t.Fatal("large inline map did not populate retained value backing")
	}

	small := inlineScratchPointerMap(1)
	for range 3 {
		out, err := encoder.AppendJSON(nil, &small)
		if err != nil || string(out) != `{"field_0":0}` {
			t.Fatalf("small encode = %s, %v", out, err)
		}
		assertEncoderScratchCleared(t, scratch)
		if scratch.valueBacking.Len() != len(large.Extra) {
			t.Fatal("small inline map did not reuse retained value backing")
		}
	}
}

func TestInlineEncoderScratchClearedAfterValueError(t *testing.T) {
	type inlineFloats struct {
		Extra map[string]*float64 `json:",inline"`
	}
	encoder := mustInlineEncoder[inlineFloats](t, EncoderOptions{InlineFields: true})
	scratch, _ := dedicatedEncoderScratch(&encoder)
	finite := 1.0
	warm := inlineFloats{Extra: make(map[string]*float64)}
	for i := range 32 {
		warm.Extra[strconv.Itoa(i)] = &finite
	}
	if _, err := encoder.AppendJSON(nil, &warm); err != nil {
		t.Fatal(err)
	}
	assertEncoderScratchCleared(t, scratch)

	nan := math.NaN()
	bad := inlineFloats{Extra: map[string]*float64{"a": &nan, "z": &finite}}
	if _, err := encoder.AppendJSON(nil, &bad); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Fatalf("encode with NaN error = %v", err)
	}
	// Both values were copied before the first sorted member failed. Cleanup
	// must include the unencoded tail, as well as restoring iterator/key state.
	assertEncoderScratchCleared(t, scratch)
	good := inlineFloats{Extra: map[string]*float64{"ok": &finite}}
	out, err := encoder.AppendJSON(nil, &good)
	if err != nil || string(out) != `{"ok":1}` {
		t.Fatalf("encode after error = %s, %v", out, err)
	}
	assertEncoderScratchCleared(t, scratch)
}

func TestInlineEncoderRecursiveScratchClearedAfterLargeMap(t *testing.T) {
	encoder := mustInlineEncoder[recNode](t, EncoderOptions{InlineFields: true})
	scratch, _ := dedicatedEncoderScratch(&encoder)
	warm := recNode{Sub: make(map[string]recNode)}
	for i := range 32 {
		warm.Sub[strconv.Itoa(i)] = recNode{V: i}
	}
	if _, err := encoder.AppendJSON(nil, &warm); err != nil {
		t.Fatal(err)
	}
	outerBacking := scratch.valueBacking
	assertEncoderScratchCleared(t, scratch)

	value := recNode{V: 1, Sub: map[string]recNode{
		"a": {V: 2, Sub: map[string]recNode{"b": {V: 3}}},
		"z": {V: 4},
	}}
	out, err := encoder.AppendJSON(nil, &value)
	if err != nil || string(out) != `{"v":1,"a":{"v":2,"b":{"v":3}},"z":{"v":4}}` {
		t.Fatalf("recursive encode = %s, %v", out, err)
	}
	assertEncoderScratchCleared(t, scratch)
	// The recursive child may return its own backing into the occupied slot.
	// Also inspect the discarded outer backing, whose values held child maps.
	assertBackingCleared(t, outerBacking, -3)
}
