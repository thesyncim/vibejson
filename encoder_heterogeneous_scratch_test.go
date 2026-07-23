package slopjson

import (
	"bytes"
	"encoding/json"
	"runtime"
	"testing"
)

type heterogeneousScratchDoc struct {
	Ints    map[string]int     `json:"ints"`
	Strings map[string]string  `json:"strings"`
	Ptrs    map[string]*uint64 `json:"ptrs"`
}

func heterogeneousScratchValue() heterogeneousScratchDoc {
	a, c := uint64(1), uint64(3)
	return heterogeneousScratchDoc{
		Ints:    map[string]int{"a": 1, "b": 2, "c": 3},
		Strings: map[string]string{"a": "one", "b": "two", "c": "three"},
		Ptrs:    map[string]*uint64{"a": &a, "b": nil, "c": &c},
	}
}

// TestHeterogeneousMapScratchAllocationFree guards the fixed backing-slot
// layout: every statically compiled map element type owns a scratch slot, so a
// document that alternates map types does not re-type and reallocate one
// shared reflect slice on every encode.
func TestHeterogeneousMapScratchAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector instruments allocation and disables pool reuse")
	}
	v := heterogeneousScratchValue()
	want, err := json.Marshal(&v)
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CompileEncoder[heterogeneousScratchDoc](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := enc.AppendJSON(make([]byte, 0, len(want)), &v)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(dst, want) {
		t.Fatalf("heterogeneous map encode differs:\n got %s\nwant %s", dst, want)
	}
	allocs := testing.AllocsPerRun(200, func() {
		dst, _ = enc.AppendJSON(dst[:0], &v)
	})
	if allocs != 0 {
		t.Fatalf("heterogeneous map encode allocated %.1f times per run, want 0", allocs)
	}
}

func BenchmarkEncodeHeterogeneousMaps(b *testing.B) {
	v := heterogeneousScratchValue()
	enc, err := CompileEncoder[heterogeneousScratchDoc](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	dst, err := enc.AppendJSON(make([]byte, 0, 160), &v)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(dst)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst, err = enc.AppendJSON(dst[:0], &v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// TestDynamicMapScratchIsPlanIndependent exercises dynamic map plans inside a
// static encoder that also owns fixed scratch slots. Dynamic nodes are cached
// globally and must never index into (or infer the layout of) the surrounding
// encoder's slots. Alternating unrelated element types, recursive interface
// maps, stack growth, and GC guards that separation.
func TestDynamicMapScratchIsPlanIndependent(t *testing.T) {
	type document struct {
		Anchor  map[string]int `json:"anchor"`
		Dynamic any            `json:"dynamic"`
	}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	a, b := uint64(10), uint64(20)
	values := []any{
		map[string]string{"a": "one", "b": "two"},
		map[string]*uint64{"a": &a, "b": nil},
		map[string]any{
			"child": map[string]*uint64{"a": &a, "b": &b},
			"label": "nested",
		},
		map[int][]string{2: {"two"}, 1: {"one", "uno"}},
	}
	buffer := make([]byte, 0, 256)
	for round := 0; round < 20; round++ {
		v := document{Anchor: map[string]int{"round": round}, Dynamic: values[round%len(values)]}
		want, wantErr := json.Marshal(&v)
		got, gotErr := enc.AppendJSON(buffer[:0], &v)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("round %d acceptance differs: encoder=%v stdlib=%v", round, gotErr, wantErr)
		}
		if gotErr == nil && !bytes.Equal(got, want) {
			t.Fatalf("round %d differs:\n got %s\nwant %s", round, got, want)
		}
		buffer = got
		if round%4 == 3 {
			stackSink := forceStackMovement(32+round, round)
			runtime.GC()
			runtime.KeepAlive(stackSink)
		}
	}
}
