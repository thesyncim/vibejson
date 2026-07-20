package simdjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unsafe"
)

// decodeRoute is one forced implementation of the same semantic operation.
// Keeping route selection in test code lets the production dispatch remain
// branch-free while preventing heuristics from silently dropping coverage.
type decodeRoute struct {
	name   string
	decode func([]byte) (any, error)
}

func compareDecodeRoutes(t *testing.T, fixtures [][]byte, routes []decodeRoute, exactErrors bool) {
	t.Helper()
	if len(routes) < 2 {
		t.Fatal("route comparison needs at least two implementations")
	}
	for _, fixture := range fixtures {
		src := bytes.Clone(fixture)
		t.Run(string(src), func(t *testing.T) {
			want, wantErr := routes[0].decode(bytes.Clone(src))
			for _, route := range routes[1:] {
				got, gotErr := route.decode(bytes.Clone(src))
				if (gotErr == nil) != (wantErr == nil) {
					t.Fatalf("%s acceptance differs from %s: got %v, want %v", route.name, routes[0].name, gotErr, wantErr)
				}
				if gotErr != nil {
					if exactErrors && (reflect.TypeOf(gotErr) != reflect.TypeOf(wantErr) || gotErr.Error() != wantErr.Error()) {
						t.Fatalf("%s error differs from %s: got %T %q, want %T %q",
							route.name, routes[0].name, gotErr, gotErr, wantErr, wantErr)
					}
					continue
				}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s value differs from %s: got %#v, want %#v", route.name, routes[0].name, got, want)
				}
			}
		})
	}
}

// decodeCursorRoute bypasses Decoder.Decode's size heuristic while reusing the
// production whole-document cursor dispatch.
func decodeCursorRoute[T any](plan Decoder[T], src []byte, dst *T) error {
	return decodeTypedDocument(src, plan.options, plan.root, unsafe.Pointer(dst), nil)
}

type routeRecord struct {
	ID     int64      `json:"id"`
	Active bool       `json:"active"`
	Name   string     `json:"name"`
	Note   string     `json:"note"`
	Scores [3]float64 `json:"scores"`
}

func TestTypedDecodeForcedRouteParity(t *testing.T) {
	owned, err := CompileDecoder[routeRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zeroCopy, err := CompileDecoder[routeRecord](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	if !owned.structural || owned.root.decShape != typedDecShapeRecordFloat64x3 {
		t.Fatalf("fixture lost structural specialization: structural=%v shape=%v", owned.structural, owned.root.decShape)
	}

	genericRoot := *owned.root
	genericRoot.structuralFast = false
	genericRoot.decShape = typedDecShapeNone
	generic := owned
	generic.root = &genericRoot

	routes := []decodeRoute{
		{name: "compiled cursor", decode: func(src []byte) (any, error) {
			var out routeRecord
			err := decodeCursorRoute(owned, src, &out)
			return out, err
		}},
		{name: "structural specialization", decode: func(src []byte) (any, error) {
			var out routeRecord
			err := owned.decodeStructural(src, &out)
			return out, err
		}},
		{name: "generic structural loop", decode: func(src []byte) (any, error) {
			var out routeRecord
			err := generic.decodeStructural(src, &out)
			return out, err
		}},
		{name: "automatic owned", decode: func(src []byte) (any, error) {
			var out routeRecord
			err := owned.Decode(src, &out)
			return out, err
		}},
		{name: "automatic zero-copy", decode: func(src []byte) (any, error) {
			var out routeRecord
			err := zeroCopy.Decode(src, &out)
			return out, err
		}},
	}

	valid := [][]byte{
		[]byte(`{"id":7,"active":true,"name":"alpha","note":"plain","scores":[1.5,-2,3e4]}`),
		[]byte(` { "scores" : [0,1,2], "note":"plain text", "name":"beta", "active":false, "id":-9 } `),
		[]byte(` { "scores" : [0,1,2], "note":"line\ntext", "name":"beta", "active":false, "id":-9 } `),
		[]byte(` { "scores" : [0,1,2], "note":"plain text", "name":"βeta", "active":false, "id":-9 } `),
		[]byte(`{"id":-9,"active":false,"name":"beta","note":"line\ntext","scores":[0,1,2]}`),
		[]byte(`{"id":-9,"active":false,"name":"βeta","note":"plain","scores":[0,1,2]}`),
		[]byte(` { "scores" : [0,1,2], "note":"line\ntext", "name":"βeta", "active":false, "id":-9 } `),
		[]byte(`{"id":1,"id":2,"active":true,"name":"first","name":"last","note":"x","scores":[1,2,3]}`),
		[]byte(`{"id":5,"active":true,"name":"escaped\u0020name","note":"x","scores":[1,2,3],"unknown":{"x":1}}`),
		[]byte(`null`),
	}
	large := []byte("{\n  \"scores\": [0,1,2],\n  \"note\": \"line\\ntext\",\n  \"name\": \"beta\",\n  \"active\": false,\n  \"padding\": \"" +
		strings.Repeat("x", 5000) + "\",\n  \"id\": -9\n}")
	if len(large) < 4096 || !decoderStructuralWorthwhile(large) {
		t.Fatal("large fixture no longer forces automatic structural decoding")
	}
	valid = append(valid, large)
	compareDecodeRoutes(t, valid, append(routes, decodeRoute{name: "encoding/json", decode: func(src []byte) (any, error) {
		var out routeRecord
		err := json.Unmarshal(src, &out)
		return out, err
	}}), false)

	invalid := [][]byte{
		[]byte(`{"id":"bad","active":true,"name":"x","note":"y","scores":[1,2,3]}`),
		[]byte(`{"id":1,"active":true,"name":"x","note":"y","scores":[1,"bad",3]}`),
		[]byte(`{"id":9223372036854775808,"active":true,"name":"x","note":"y","scores":[1,2,3]}`),
		[]byte(`{"id":1,"active":true,"name":"x","note":"y","scores":[1,2`),
	}
	compareDecodeRoutes(t, invalid, routes[:4], true)
}

type routeRecordFloat64x4 struct {
	ID     int64      `json:"id"`
	Active bool       `json:"active"`
	Name   string     `json:"name"`
	Note   string     `json:"note"`
	Scores [4]float64 `json:"scores"`
}

func BenchmarkTypedDecodeStructuralRecordSpecializations(b *testing.B) {
	pad := strings.Repeat("x", 5000)
	b.Run("record", func(b *testing.B) {
		benchmarkTypedDecodeStructuralRecord[routeRecordFloat64x4](b,
			[]byte(`{"id":7,"active":true,"name":"record","note":"`+pad+`","scores":[1,2.5,-3e4,4]}`),
			typedDecShapeRecord,
		)
	})
	b.Run("float64x3", func(b *testing.B) {
		benchmarkTypedDecodeStructuralRecord[routeRecord](b,
			[]byte(`{"id":7,"active":true,"name":"record","note":"`+pad+`","scores":[1,2.5,-3e4]}`),
			typedDecShapeRecordFloat64x3,
		)
	})
}

func benchmarkTypedDecodeStructuralRecord[T any](b *testing.B, src []byte, shape typedDecShape) {
	decoder, err := CompileDecoder[T](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	if !decoder.structural || decoder.root.decShape != shape ||
		!decoderStructuralWorthwhile(src) {
		b.Fatalf("fixture no longer selects structural specialization %d", shape)
	}

	genericRoot := *decoder.root
	genericRoot.structuralFast = false
	genericRoot.decShape = typedDecShapeNone
	generic := decoder
	generic.root = &genericRoot

	for _, bench := range []struct {
		name    string
		decoder Decoder[T]
	}{
		{name: "specialized", decoder: decoder},
		{name: "generic", decoder: generic},
	} {
		b.Run(bench.name, func(b *testing.B) {
			var dst T
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for range b.N {
				if err := bench.decoder.decodeStructural(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// FuzzStructuralRouteParity pads arbitrary inputs into both production-sized
// structural routes. The bitmap validator must match its scalar reference and
// typed structural decoding must match the raw cursor. Trailing JSON whitespace
// keeps mutations focused on the original parser input.
func FuzzStructuralRouteParity(f *testing.F) {
	for _, src := range [][]byte{
		[]byte(`{"a": [1, 2.5e-3, true, false, null, "x\nA"]}`),
		[]byte("[\n  \"" + strings.Repeat("word ", 40) + "\\u2028\",\n  -0.125e+9\n]"),
		bytes.Repeat([]byte(`{"k": "v", "n": [1,2,3]} `), 40),
		[]byte(`{"id":7,"active":true,"name":"alpha","note":"plain","scores":[1.5,-2,3e4]}`),
		[]byte(` { "scores" : [0,1,2], "note":"line\ntext", "name":"beta", "active":false, "id":-9 } `),
		[]byte(`{"id":1,"id":2,"active":true,"name":"first","note":"x","scores":[1,2,3]}`),
		[]byte(`{"id":1,"active":true,"name":"x","note":"y","scores":[1,2`),
		[]byte(`{:}`),
		[]byte(`{:"id":1,"active":true,"name":"x","note":"y","scores":[1,2,3]}`),
		[]byte(`{"id":1,"active":true,"name":"x","note":"y","scores"::[1,2,3]}`),
		[]byte(`{"id":1,"active":true,"name":"x","note":"y","scores":[:1,2,3]}`),
	} {
		f.Add(src)
	}
	decoder, err := CompileDecoder[routeRecord](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<16 {
			t.Skip()
		}
		doc := bitmapRoutedInput(src)
		got, decided := validBitmap(doc)
		if !decided {
			t.Fatal("production-sized sparse sample did not commit")
		}
		want := validateOptions(doc, Options{}) == nil
		if got != want {
			t.Fatalf("validBitmap = %v, scalar validator = %v on embedded %q", got, want, src)
		}

		if len(src) > 1<<13 {
			return
		}
		padded := make([]byte, max(len(src), decoderStructuralMinBytes))
		copy(padded, src)
		for i := len(src); i < len(padded); i++ {
			padded[i] = ' '
		}

		var raw, structural routeRecord
		rawErr := decodeCursorRoute(decoder, padded, &raw)
		structuralErr := decoder.Decode(padded, &structural)
		if (rawErr == nil) != (structuralErr == nil) {
			t.Fatalf("acceptance differs: raw=%v structural=%v\nsrc=%.160q", rawErr, structuralErr, src)
		}
		if rawErr == nil && !reflect.DeepEqual(raw, structural) {
			t.Fatalf("value differs: raw=%+v structural=%+v\nsrc=%.160q", raw, structural, src)
		}
	})
}

func TestTypedDecodeOwnedAndZeroCopyLifetime(t *testing.T) {
	owned, err := CompileDecoder[routeRecord](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	zeroCopy, err := CompileDecoder[routeRecord](DecoderOptions{ZeroCopy: true})
	if err != nil {
		t.Fatal(err)
	}
	ownedSrc := []byte(`{"id":1,"active":true,"name":"alpha","note":"plain","scores":[1,2,3]}`)
	zeroSrc := bytes.Clone(ownedSrc)
	var ownedValue, zeroValue routeRecord
	if err := decodeCursorRoute(owned, ownedSrc, &ownedValue); err != nil {
		t.Fatal(err)
	}
	if err := decodeCursorRoute(zeroCopy, zeroSrc, &zeroValue); err != nil {
		t.Fatal(err)
	}
	copy(ownedSrc[bytes.Index(ownedSrc, []byte("alpha")):], "xxxxx")
	copy(zeroSrc[bytes.Index(zeroSrc, []byte("alpha")):], "xxxxx")
	if ownedValue.Name != "alpha" {
		t.Fatalf("owned string changed after source mutation: %q", ownedValue.Name)
	}
	if zeroValue.Name != "xxxxx" {
		t.Fatalf("zero-copy string did not expose its documented alias: %q", zeroValue.Name)
	}
}

func TestTypedStructuralDecodeIsolatesDirtyString(t *testing.T) {
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name   string
		src    []byte
		marker string
	}{
		{name: "escaped", src: benchRecordsOneEscapedStringJSON(64), marker: "\n"},
		{name: "non-ASCII", src: benchRecordsOneNonASCIIStringJSON(64), marker: "β"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if !decoderStructuralWorthwhile(tc.src) {
				t.Fatal("fixture did not select structural decoding")
			}

			var raw, structural benchDocument
			if err := decodeCursorRoute(decoder, tc.src, &raw); err != nil {
				t.Fatalf("raw cursor decode: %v", err)
			}
			if err := decoder.Decode(tc.src, &structural); err != nil {
				t.Fatalf("structural decode: %v", err)
			}
			if !reflect.DeepEqual(structural, raw) {
				t.Fatal("isolated dirty string changed structural decode semantics")
			}
			if !strings.Contains(structural.Items[0].Message, tc.marker) ||
				strings.Contains(structural.Items[1].Message, tc.marker) {
				t.Fatal("dirty string was not isolated to its record")
			}
		})
	}
}

func TestHookAndCompiledForcedRouteParity(t *testing.T) {
	hookDecoder, err := CompileDecoder[hookPerson](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	compiledDecoder, err := CompileDecoder[hookPersonPlain](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	routes := []decodeRoute{
		{name: "compiled", decode: func(src []byte) (any, error) {
			var out hookPersonPlain
			err := compiledDecoder.Decode(src, &out)
			return out, err
		}},
		{name: "hook", decode: func(src []byte) (any, error) {
			var out hookPerson
			err := hookDecoder.Decode(src, &out)
			return projectHook(out), err
		}},
		{name: "encoding/json", decode: func(src []byte) (any, error) {
			var out hookPersonPlain
			err := json.Unmarshal(src, &out)
			return out, err
		}},
	}
	fixtures := make([][]byte, 0, len(adversarialHookDocs()))
	for _, text := range adversarialHookDocs() {
		fixtures = append(fixtures, []byte(text))
	}
	compareDecodeRoutes(t, fixtures, routes, false)
}
