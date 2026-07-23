package slopjson

import "testing"

type staticMarshaler struct{ V int }

func (m staticMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"static"`), nil }

type dynamicMarshaler struct{ S string }

func (m dynamicMarshaler) MarshalJSON() ([]byte, error) { return []byte(`"dynamic"`), nil }

func TestDynamicMarshalerForeignScratch(t *testing.T) {
	type doc struct {
		A staticMarshaler `json:"a"`
		B any             `json:"b"`
	}
	v := doc{A: staticMarshaler{V: 1}, B: dynamicMarshaler{S: "x"}}
	out, err := Marshal(&v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"a":"static","b":"dynamic"}` {
		t.Fatalf("got %s", out)
	}
}

type recursiveMap map[string]recursiveMap

func TestEncodeMapScratchReuse(t *testing.T) {
	type doc struct {
		M recursiveMap   `json:"m"`
		N map[string]int `json:"n"`
		O map[int]string `json:"o"`
		P any            `json:"p"`
	}
	v := doc{
		M: recursiveMap{"a": recursiveMap{"b": recursiveMap{"c": nil}}, "z": nil},
		N: map[string]int{"x": 1, "y": 2},
		O: map[int]string{-5: "neg", 10: "ten", 3: "three"},
		P: map[string]bool{"k": true},
	}
	enc, err := CompileEncoder[doc](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"m":{"a":{"b":{"c":null}},"z":null},"n":{"x":1,"y":2},"o":{"-5":"neg","10":"ten","3":"three"},"p":{"k":true}}`
	buf := make([]byte, 0, 256)
	for round := 0; round < 50; round++ {
		out, err := enc.AppendJSON(buf[:0], &v)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Fatalf("round %d:\ngot  %s\nwant %s", round, out, want)
		}
	}
}

// TestMapEncodeLocalSourceAllocationBound guards the cost of keeping a local
// document visible to escape analysis across reflective map iteration. One
// small operation-lifetime allocation is the deliberate safety ceiling; map
// size and pooled scratch reuse must not add per-entry allocations.
func TestMapEncodeLocalSourceAllocationBound(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector instruments allocation and disables pool reuse")
	}
	type document struct {
		M map[string]*uint64 `json:"m"`
		S string             `json:"s"`
	}
	a, b := uint64(1), uint64(2)
	shared := map[string]*uint64{"a": &a, "b": &b, "nil": nil}
	enc, err := CompileEncoder[document](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	buffer := make([]byte, 0, 96)
	value := document{M: shared, S: "warm"}
	if buffer, err = enc.AppendJSON(buffer[:0], &value); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(200, func() {
		local := document{M: shared, S: "stack"}
		buffer, err = enc.AppendJSON(buffer[:0], &local)
		if err != nil {
			panic(err)
		}
	})
	if allocs > 1 {
		t.Fatalf("encoding a local map document allocated %.1f times per run, want <=1", allocs)
	}
}
