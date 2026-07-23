package slopjson

import (
	"encoding/json"
	"reflect"
	"testing"
)

func jsonUnmarshalStd(src []byte, dst any) error { return json.Unmarshal(src, dst) }

// jsonUnicodeEscape builds a \uXXXX escape without a literal backslash-u in
// source, keeping the intent obvious at call sites.
func jsonUnicodeEscape(hex string) string {
	return "\\u" + hex
}

// TestArenaBlockSwitchRetention decodes enough escaped strings to force the
// arena through several block switches and checks every retained value
// against encoding/json, in both ownership modes and through the dynamic
// parser.
func TestArenaBlockSwitchRetention(t *testing.T) {
	var doc []byte
	doc = append(doc, '[')
	for i := range 64 {
		if i > 0 {
			doc = append(doc, ',')
		}
		doc = append(doc, '"')
		for range 40 {
			doc = append(doc, jsonUnicodeEscape("00e9")...)
			doc = append(doc, "plain"...)
		}
		doc = append(doc, '"')
	}
	doc = append(doc, ']')

	var want []string
	if err := jsonUnmarshalStd(doc, &want); err != nil {
		t.Fatal(err)
	}
	for _, opts := range []DecoderOptions{{}, {ZeroCopy: true}} {
		dec, err := CompileDecoder[[]string](opts)
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := dec.Decode(doc, &got); err != nil {
			t.Fatal(err)
		}
		if len(got) != len(want) {
			t.Fatalf("ZeroCopy=%v: %d strings, want %d", opts.ZeroCopy, len(got), len(want))
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("ZeroCopy=%v: string %d = %.40q, want %.40q", opts.ZeroCopy, i, got[i], want[i])
			}
		}
	}

	tree, err := decodeAnyZeroCopyForTest(doc)
	if err != nil {
		t.Fatal(err)
	}
	values := tree.([]any)
	for i := range want {
		if values[i] != any(want[i]) {
			t.Fatalf("dynamic string %d = %.40q, want %.40q", i, values[i], want[i])
		}
	}
}

// TestArenaRetainsAnyStrings covers the arena-overwrite regression: an any
// field whose string content was unescaped into the arena must survive later
// escaped strings appending after it. The B field creates the arena, A
// retains arena bytes inside the dynamic value, and C appends next.
func TestArenaRetainsAnyStrings(t *testing.T) {
	type S struct {
		B string         `json:"b"`
		A any            `json:"a"`
		M map[string]any `json:"m"`
		C string         `json:"c"`
	}
	src := []byte(`{"b":"p` + jsonUnicodeEscape("0042") + `q",` +
		`"a":"x` + jsonUnicodeEscape("0041") + `y",` +
		`"m":{"k":"m` + jsonUnicodeEscape("004D") + `n"},` +
		`"c":"r` + jsonUnicodeEscape("0043") + `s"}`)
	for _, opts := range []DecoderOptions{{}, {ZeroCopy: true}} {
		dec, err := CompileDecoder[S](opts)
		if err != nil {
			t.Fatal(err)
		}
		var s S
		if err := dec.Decode(src, &s); err != nil {
			t.Fatal(err)
		}
		want := S{B: "pBq", A: any("xAy"), M: map[string]any{"k": "mMn"}, C: "rCs"}
		if !reflect.DeepEqual(s, want) {
			t.Fatalf("ZeroCopy=%v: got %+v, want %+v", opts.ZeroCopy, s, want)
		}
	}
}
