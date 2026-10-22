package vibejson

import (
	"encoding/json"
	"reflect"
	"testing"
)

type ReplaceAliasEmbedded struct {
	X int `json:"x"`
}

type replaceEmbeddedAliasDocument struct {
	*ReplaceAliasEmbedded
	Other *ReplaceAliasEmbedded `json:"other"`
	Tail  int                   `json:"tail"`
}

func newReplaceEmbeddedAliasDocument() replaceEmbeddedAliasDocument {
	shared := &ReplaceAliasEmbedded{X: 7}
	return replaceEmbeddedAliasDocument{ReplaceAliasEmbedded: shared, Other: shared, Tail: 9}
}

func TestReplaceEmbeddedPointerAliases(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceEmbeddedAliasDocument](t, DecoderOptions{Replace: true})
	for _, src := range []string{
		`{"x":1,"other":{"x":2}}`,
		`{"other":{"x":2},"x":1}`,
		`{"other":{"x":2}}`,
		`{"x":1}`,
		`{"x":1,"other":{"x":2},"x":3}`,
		`{"x":1,"other":{"x":2},"other":{"x":3}}`,
		`{"other":null,"x":1}`,
		`{}`,
	} {
		t.Run(src, func(t *testing.T) {
			var want replaceEmbeddedAliasDocument
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			for _, api := range []string{"Decode", "DecodePrefix", "DecodeArray"} {
				t.Run(api, func(t *testing.T) {
					got := newReplaceEmbeddedAliasDocument()
					switch api {
					case "Decode":
						requireNoTestError(t, decoder.Decode([]byte(src), &got))
					case "DecodePrefix":
						n, err := decoder.DecodePrefix([]byte(src+" trailing"), &got)
						requireNoTestError(t, err)
						if n != len(src) {
							t.Fatalf("consumed %d bytes, want %d", n, len(src))
						}
					case "DecodeArray":
						storage := []replaceEmbeddedAliasDocument{got}
						values, err := decoder.DecodeArray([]byte("["+src+"]"), storage[:0])
						requireNoTestError(t, err)
						if len(values) != 1 {
							t.Fatalf("decoded %d values", len(values))
						}
						got = values[0]
					}
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("Replace result differs from fresh decode:\n got %#v, promoted %#v, other %#v\nwant %#v, promoted %#v, other %#v", got, got.ReplaceAliasEmbedded, got.Other, want, want.ReplaceAliasEmbedded, want.Other)
					}
					if got.ReplaceAliasEmbedded != nil && got.ReplaceAliasEmbedded == got.Other {
						t.Fatal("Replace retained shared embedded pointee")
					}
				})
			}
		})
	}
}

func TestReplaceEmbeddedPointerAliasesAfterError(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceEmbeddedAliasDocument](t, DecoderOptions{Replace: true})
	got := newReplaceEmbeddedAliasDocument()
	if err := decoder.Decode([]byte(`{"x":1,"other":{"x":2},"tail":false}`), &got); err == nil {
		t.Fatal("expected a type error")
	}
	if got.ReplaceAliasEmbedded == nil || got.X != 1 || got.Other == nil || got.Other.X != 2 {
		t.Fatalf("completed members were overwritten before an error: embedded=%#v other=%#v", got.ReplaceAliasEmbedded, got.Other)
	}
	got = newReplaceEmbeddedAliasDocument()
	requireNoTestError(t, decoder.Decode([]byte(`{"other":{"x":4},"x":3}`), &got))
	if got.X != 3 || got.Other.X != 4 {
		t.Fatalf("reuse after an error retained aliases: embedded=%#v other=%#v", got.ReplaceAliasEmbedded, got.Other)
	}
}

func TestReplaceEmbeddedPointerAliasIntoDestination(t *testing.T) {
	type document struct {
		*ReplaceAliasEmbedded
		Direct ReplaceAliasEmbedded `json:"direct"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{`{"x":1,"direct":{"x":2}}`, `{"direct":{"x":2},"x":1}`, `{"direct":{"x":2}}`} {
		t.Run(src, func(t *testing.T) {
			got := document{Direct: ReplaceAliasEmbedded{X: 7}}
			got.ReplaceAliasEmbedded = &got.Direct
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("Replace retained embedded pointer into destination: embedded=%#v direct=%#v, want %#v", got.ReplaceAliasEmbedded, got.Direct, want)
			}
		})
	}
}

func TestMergePreservesEmbeddedPointerAliases(t *testing.T) {
	decoder := mustCompileTestDecoder[replaceEmbeddedAliasDocument](t, DecoderOptions{})
	for _, src := range []string{`{"x":1,"other":{"x":2}}`, `{"other":{"x":2},"x":1}`, `{"other":{"x":2}}`} {
		got, want := newReplaceEmbeddedAliasDocument(), newReplaceEmbeddedAliasDocument()
		requireNoTestError(t, decoder.Decode([]byte(src), &got))
		requireNoTestError(t, json.Unmarshal([]byte(src), &want))
		if !reflect.DeepEqual(got, want) || got.ReplaceAliasEmbedded != got.Other {
			t.Fatalf("merge changed shared embedded pointer behavior for %s", src)
		}
	}
}

type ReplaceAliasMiddle struct {
	*ReplaceAliasEmbedded
	Branch int `json:"branch"`
}

func TestReplaceNestedEmbeddedPointerAliases(t *testing.T) {
	type document struct {
		*ReplaceAliasMiddle
		Other *ReplaceAliasEmbedded `json:"other"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{
		`{"x":1,"other":{"x":2}}`,
		`{"other":{"x":2},"x":1}`,
		`{"branch":3,"other":{"x":2}}`,
		`{"other":{"x":2}}`,
	} {
		t.Run(src, func(t *testing.T) {
			shared := &ReplaceAliasEmbedded{X: 7}
			middle := &ReplaceAliasMiddle{ReplaceAliasEmbedded: shared, Branch: 8}
			got := document{ReplaceAliasMiddle: middle, Other: shared}
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("nested embedded alias differs from fresh decode: got %#v, other %#v; want %#v", got.ReplaceAliasMiddle, got.Other, want.ReplaceAliasMiddle)
			}
			if got.ReplaceAliasMiddle != nil && got.ReplaceAliasMiddle != middle {
				t.Fatal("discarded unique outer embedded pointer")
			}
		})
	}
}

type ReplaceAliasLeft struct {
	Value int `json:"left"`
}

type ReplaceAliasRight struct {
	Value int `json:"right"`
}

func TestReplaceOverlappingEmbeddedPointers(t *testing.T) {
	type document struct {
		*ReplaceAliasLeft
		*ReplaceAliasRight
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{`{"left":1,"right":2}`, `{"right":2,"left":1}`, `{"right":2}`, `{"left":1}`} {
		t.Run(src, func(t *testing.T) {
			left := &ReplaceAliasLeft{Value: 7}
			got := document{ReplaceAliasLeft: left, ReplaceAliasRight: (*ReplaceAliasRight)(left)}
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("overlapping embedded aliases differ from fresh decode: left=%#v right=%#v", got.ReplaceAliasLeft, got.ReplaceAliasRight)
			}
		})
	}
}

type ReplaceAliasIgnored struct {
	X       int `json:"x"`
	Ignored int `json:"-"`
}

func TestReplaceEmbeddedAliasIgnoredAndWideResets(t *testing.T) {
	type document struct {
		*ReplaceAliasIgnored
		ReplaceWideScalars
		Other *ReplaceAliasIgnored `json:"other"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{`{"x":1,"other":{"x":2}}`, `{"other":{"x":2},"x":1}`, `{"other":{"x":2}}`} {
		t.Run(src, func(t *testing.T) {
			shared := &ReplaceAliasIgnored{X: 7, Ignored: 8}
			got := document{ReplaceAliasIgnored: shared, Other: shared, ReplaceWideScalars: ReplaceWideScalars{F63: 9}}
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("wide/ignored reset differs from fresh decode: embedded=%#v other=%#v f63=%d", got.ReplaceAliasIgnored, got.Other, got.F63)
			}
		})
	}
}

func TestReplaceEmbeddedAliasesAcrossArrayElements(t *testing.T) {
	type document struct{ *ReplaceAliasEmbedded }
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := &ReplaceAliasEmbedded{X: 7}
	storage := []document{{shared}, {shared}}
	got, err := decoder.DecodeArray([]byte(`[{"x":1},{"x":2}]`), storage[:0])
	requireNoTestError(t, err)
	if len(got) != 2 || got[0].X != 1 || got[1].X != 2 || got[0].ReplaceAliasEmbedded == got[1].ReplaceAliasEmbedded {
		t.Fatalf("retained embedded alias across array elements: %#v", got)
	}
}

func TestReplaceReusesPointeeAfterAbsentEmbeddedChain(t *testing.T) {
	type inner struct{ *ReplaceAliasMiddle }
	type document struct {
		First inner                 `json:"first"`
		Later *ReplaceAliasEmbedded `json:"later"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	shared := &ReplaceAliasEmbedded{X: 7}
	got := document{
		First: inner{&ReplaceAliasMiddle{ReplaceAliasEmbedded: shared, Branch: 8}},
		Later: shared,
	}
	requireNoTestError(t, decoder.Decode([]byte(`{"first":{},"later":{"x":1}}`), &got))
	if got.First.ReplaceAliasMiddle != nil || got.Later != shared || got.Later.X != 1 {
		t.Fatalf("absent embedded chain retained a stale storage claim: first=%#v later=%#v", got.First, got.Later)
	}
}

type ReplaceAliasDirect struct {
	X int `json:"y"`
}

func TestReplaceConvertedEmbeddedPointerAliasIntoDestination(t *testing.T) {
	type document struct {
		*ReplaceAliasEmbedded
		Direct ReplaceAliasDirect `json:"direct"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{`{"x":1,"direct":{"y":2}}`, `{"direct":{"y":2},"x":1}`, `{"direct":{"y":2}}`} {
		t.Run(src, func(t *testing.T) {
			got := document{Direct: ReplaceAliasDirect{X: 7}}
			got.ReplaceAliasEmbedded = (*ReplaceAliasEmbedded)(&got.Direct)
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("converted embedded pointer retained destination alias: embedded=%#v direct=%#v", got.ReplaceAliasEmbedded, got.Direct)
			}
		})
	}
}

func TestReplaceConvertedPointerAliasIntoDestination(t *testing.T) {
	type document struct {
		Value  *ReplaceAliasEmbedded `json:"value"`
		Direct ReplaceAliasDirect    `json:"direct"`
	}
	decoder := mustCompileTestDecoder[document](t, DecoderOptions{Replace: true})
	for _, src := range []string{`{"value":{"x":1},"direct":{"y":2}}`, `{"direct":{"y":2},"value":{"x":1}}`} {
		t.Run(src, func(t *testing.T) {
			got := document{Direct: ReplaceAliasDirect{X: 7}}
			got.Value = (*ReplaceAliasEmbedded)(&got.Direct)
			var want document
			requireNoTestError(t, json.Unmarshal([]byte(src), &want))
			requireNoTestError(t, decoder.Decode([]byte(src), &got))
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("converted pointer retained destination alias: value=%#v direct=%#v", got.Value, got.Direct)
			}
		})
	}
}
