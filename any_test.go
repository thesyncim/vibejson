package slopjson

// Contract tests for dynamic decoding through the public surface: Unmarshal
// into *any and CompileDecoder[any]. The helpers below are the one spelling
// the rest of the test suite uses for dynamic decodes, so every differential
// suite exercises exactly what callers reach.

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// unmarshalAnyForTest decodes src into a fresh any through Unmarshal.
func unmarshalAnyForTest(src []byte) (any, error) {
	var v any
	err := Unmarshal(src, &v)
	return v, err
}

// decodeAnyForTest decodes src into a fresh any through a compiled decoder.
func decodeAnyForTest(src []byte, opts DecoderOptions) (any, error) {
	decoder, err := CompileDecoder[any](opts)
	if err != nil {
		return nil, err
	}
	var v any
	err = decoder.Decode(src, &v)
	return v, err
}

func decodeAnyZeroCopyForTest(src []byte) (any, error) {
	return decodeAnyForTest(src, DecoderOptions{ZeroCopy: true})
}

func decodeAnyUseNumberForTest(src []byte) (any, error) {
	return decodeAnyForTest(src, DecoderOptions{UseNumber: true})
}

// TestUnmarshalAnyMergeSemantics pins the destination contract against
// encoding/json for every prefill class: a nil interface and non-pointer
// values are replaced wholesale, an interface holding a non-nil pointer is
// decoded into (merged), a nil pointer is replaced, and null clears the
// interface in every case.
func TestUnmarshalAnyMergeSemantics(t *testing.T) {
	type inner struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	prefills := []struct {
		name string
		make func() any
	}{
		{"nil", func() any { return nil }},
		{"map", func() any { return map[string]any{"stale": true} }},
		{"slice", func() any { return []any{"stale"} }},
		{"string", func() any { return "stale" }},
		{"float", func() any { return 1.5 }},
		{"nil-pointer", func() any { return (*inner)(nil) }},
		{"pointer", func() any { return &inner{A: 7, B: "kept"} }},
		{"pointer-to-map", func() any { return &map[string]any{"kept": 1.0} }},
	}
	inputs := []string{
		`{"a":1}`,
		`{"b":"fresh"}`,
		`[1,2,3]`,
		`"text"`,
		`42`,
		`null`,
		` null `,
	}
	for _, prefill := range prefills {
		for _, src := range inputs {
			var want any = prefill.make()
			wantErr := json.Unmarshal([]byte(src), &want)

			var got any = prefill.make()
			gotErr := Unmarshal([]byte(src), &got)

			if (gotErr == nil) != (wantErr == nil) {
				t.Fatalf("%s <- %s: error = %v, encoding/json error = %v", prefill.name, src, gotErr, wantErr)
			}
			if gotErr == nil && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s <- %s: result = %#v, encoding/json = %#v", prefill.name, src, got, want)
			}
		}
	}
}

// TestUnmarshalAnyPointerMergePreservesIdentity verifies the merge case
// beyond DeepEqual: decoding into an interface holding a non-nil pointer
// writes through that pointer, so a second reference observes the update.
func TestUnmarshalAnyPointerMergePreservesIdentity(t *testing.T) {
	type inner struct {
		A int    `json:"a"`
		B string `json:"b"`
	}
	shared := &inner{A: 7, B: "kept"}
	var dst any = shared
	if err := Unmarshal([]byte(`{"a":1}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst != any(shared) {
		t.Fatalf("merge replaced the held pointer: %#v", dst)
	}
	if shared.A != 1 || shared.B != "kept" {
		t.Fatalf("merged value = %+v, want {A:1 B:kept}", *shared)
	}
}

// TestUnmarshalAnyUseNumber checks DecoderOptions.UseNumber against
// encoding/json's Decoder.UseNumber on both the whole-document builder
// (top-level *any) and the cursor path (an any field inside a struct).
func TestUnmarshalAnyUseNumber(t *testing.T) {
	src := []byte(`{"n":-12.5e2,"big":123456789012345678901234567890,"xs":[1,2.5,1e400],"s":"3"}`)

	stdlib := json.NewDecoder(bytes.NewReader(src))
	stdlib.UseNumber()
	var want any
	if err := stdlib.Decode(&want); err != nil {
		t.Fatal(err)
	}
	got, err := decodeAnyUseNumberForTest(src)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("UseNumber tree = %#v, want %#v", got, want)
	}

	// Nested any fields decode mid-stream on the cursor path and must apply
	// the same option; the typed sibling keeps its declared representation.
	type doc struct {
		Dyn   any     `json:"dyn"`
		Typed float64 `json:"typed"`
	}
	decoder, err := CompileDecoder[doc](DecoderOptions{UseNumber: true})
	if err != nil {
		t.Fatal(err)
	}
	var d doc
	if err := decoder.Decode([]byte(`{"dyn":[1,2.5],"typed":2.5}`), &d); err != nil {
		t.Fatal(err)
	}
	if want := []any{json.Number("1"), json.Number("2.5")}; !reflect.DeepEqual(d.Dyn, want) {
		t.Fatalf("nested any = %#v, want %#v", d.Dyn, want)
	}
	if d.Typed != 2.5 {
		t.Fatalf("typed field = %v, want 2.5", d.Typed)
	}
}

// TestDecodeAnyWholeValueContracts pins the trailing-data split between the
// whole-document and prefix entry points for T=any: Decode consumes exactly
// one document, DecodePrefix stops at the value boundary, and DecodeArray
// streams elements through the cursor path.
func TestDecodeAnyWholeValueContracts(t *testing.T) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}

	var v any
	if err := decoder.Decode([]byte(`{"a":1} {"b":2}`), &v); err == nil {
		t.Fatal("Decode accepted trailing data after the top-level value")
	} else if syntax, ok := err.(*SyntaxError); !ok || syntax.Offset != 8 {
		t.Fatalf("trailing-data error = %#v, want *SyntaxError at offset 8", err)
	}

	v = nil
	n, err := decoder.DecodePrefix([]byte(` {"a":1} {"b":2}`), &v)
	if err != nil {
		t.Fatal(err)
	}
	if n != 8 {
		t.Fatalf("DecodePrefix consumed %d bytes, want 8", n)
	}
	if want := map[string]any{"a": 1.0}; !reflect.DeepEqual(v, want) {
		t.Fatalf("DecodePrefix value = %#v, want %#v", v, want)
	}

	// DecodePrefix keeps the merge contract too.
	type inner struct {
		A int `json:"a"`
	}
	held := &inner{}
	var merged any = held
	if _, err := decoder.DecodePrefix([]byte(`{"a":3}[]`), &merged); err != nil {
		t.Fatal(err)
	}
	if merged != any(held) || held.A != 3 {
		t.Fatalf("DecodePrefix merge = %#v (held %+v), want write-through", merged, *held)
	}

	values, err := decoder.DecodeArray([]byte(`[{"a":1},null,2.5]`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := []any{map[string]any{"a": 1.0}, nil, 2.5}; !reflect.DeepEqual(values, want) {
		t.Fatalf("DecodeArray = %#v, want %#v", values, want)
	}
}

// TestUnmarshalAnyLargeDocumentPaths runs one document per arena regime —
// tiny (no arena), mid-size (array arena, ordinary boxing), and slab-boxed —
// through both the fresh-destination builder and the prefilled cursor path,
// comparing trees against encoding/json.
func TestUnmarshalAnyLargeDocumentPaths(t *testing.T) {
	build := func(rows int) []byte {
		var b strings.Builder
		b.WriteString(`{"rows":[`)
		for i := range rows {
			if i != 0 {
				b.WriteByte(',')
			}
			b.WriteString(`{"id":`)
			b.WriteString(string(rune('0' + i%10)))
			b.WriteString(`,"s":"payload-payload","f":2.5,"xs":[1,2,3],"none":[]}`)
		}
		b.WriteString(`]}`)
		return []byte(b.String())
	}
	for _, src := range [][]byte{
		[]byte(`{"a":[1,"x"]}`),
		build(512),
		build(11000),
	} {
		var want any
		if err := json.Unmarshal(src, &want); err != nil {
			t.Fatal(err)
		}
		fresh, err := unmarshalAnyForTest(src)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(fresh, want) {
			t.Fatalf("fresh decode of %d bytes diverges from encoding/json", len(src))
		}
		var merged any = &map[string]any{}
		if err := Unmarshal(src, &merged); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(*(merged.(*map[string]any)), want.(map[string]any)) {
			t.Fatalf("merged decode of %d bytes diverges from encoding/json", len(src))
		}
	}
}

// TestUnmarshalAnyErrorParity feeds malformed documents to Unmarshal into
// *any and checks the verdict against encoding/json plus our own error-shape
// invariants: dynamic decode errors are *SyntaxError values whose offsets lie
// within the input, identical between the builder and the cursor path.
func TestUnmarshalAnyErrorParity(t *testing.T) {
	// Invalid UTF-8 is excluded: the library strictly rejects it where
	// encoding/json substitutes U+FFFD, a deliberate divergence pinned by
	// the validation parity suites.
	inputs := []string{
		``, ` `, `nul`, `troo`, `+1`, `01`, `1.`, `1e`, `-`, `"unterminated`,
		`"bad\escape"`, `[1,]`, `[1 2]`, `{"a"}`, `{"a":}`, `{"a":1,}`,
		`{"a":1]`, `[}`, `{"a":1} extra`, `[[[`, `1e400`,
	}
	for _, src := range inputs {
		var want any
		wantErr := json.Unmarshal([]byte(src), &want)

		fresh, freshErr := unmarshalAnyForTest([]byte(src))
		if (freshErr == nil) != (wantErr == nil) {
			t.Fatalf("%q: error = %v, encoding/json error = %v", src, freshErr, wantErr)
		}
		if freshErr == nil {
			if !reflect.DeepEqual(fresh, want) {
				t.Fatalf("%q: result = %#v, encoding/json = %#v", src, fresh, want)
			}
			continue
		}
		syntax, ok := freshErr.(*SyntaxError)
		if !ok {
			t.Fatalf("%q: error type = %T, want *SyntaxError", src, freshErr)
		}
		if syntax.Offset < 0 || syntax.Offset > len(src) {
			t.Fatalf("%q: error offset %d outside input", src, syntax.Offset)
		}

		// The prefilled (cursor) path must fail identically.
		var merged any = &map[string]any{}
		mergedErr := Unmarshal([]byte(src), &merged)
		if mergedErr == nil {
			t.Fatalf("%q: prefilled decode accepted what the builder rejected", src)
		}
	}
}
