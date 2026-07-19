package simdjson

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"
	"time"
)

// trustSinkInner gives the kitchen sink a nested pointer struct so decode
// paths recurse through pointer indirection.
type trustSinkInner struct {
	S string  `json:"s"`
	A any     `json:"a"`
	F float64 `json:"f"`
}

// trustSink routes one document through every retention-relevant decode
// path: plain and escaped strings, dynamic values, maps, slices, raw
// passthrough, base64, string-tagged scalars, unmarshalers, and numbers.
type trustSink struct {
	S  string            `json:"s"`
	A  any               `json:"a"`
	M  map[string]any    `json:"m"`
	MS map[string]string `json:"ms"`
	L  []any             `json:"l"`
	LS []string          `json:"ls"`
	Q  int64             `json:"q,string"`
	R  json.RawMessage   `json:"r"`
	B  []byte            `json:"b"`
	P  *trustSinkInner   `json:"p"`
	T  time.Time         `json:"t"`
	N  float64           `json:"n"`
	NN json.Number       `json:"nn"`
}

var trustDecoders = func() map[string]Decoder[trustSink] {
	decoders := make(map[string]Decoder[trustSink], 2)
	for name, opts := range map[string]DecoderOptions{
		"owned":     {},
		"zero-copy": {ZeroCopy: true},
	} {
		dec, err := CompileDecoder[trustSink](opts)
		if err != nil {
			panic(err)
		}
		decoders[name] = dec
	}
	return decoders
}()

// trustSinkDoc builds a document that exercises every trustSink field with
// escaped content interleaved so the string arena sees retained writers on
// both sides of every dynamic value.
func trustSinkDoc() []byte {
	e := jsonUnicodeEscape
	return []byte(`{` +
		`"s":"lead` + e("0041") + `tail",` +
		`"a":{"k":"any` + e("00e9") + `value","list":["x` + e("0042") + `y",1.5,true]},` +
		`"m":{"mk":"m` + e("004D") + `v","plain":"clean"},` +
		`"ms":{"a` + e("0043") + `b":"c` + e("0044") + `d"},` +
		`"l":["e` + e("0045") + `f",{"g":"h` + e("0046") + `i"}],` +
		`"ls":["j` + e("0047") + `k","plain"],` +
		`"q":"-42",` +
		`"r":{"raw":  [1, "t` + e("0048") + `u"]},` +
		`"b":"aGVsbG8=",` +
		`"p":{"s":"n` + e("0049") + `o","a":"p` + e("004A") + `q","f":2.5},` +
		`"t":"2026-07-14T12:34:56Z",` +
		`"n":-0.125,` +
		`"nn":1234567890123456789012345` +
		`}`)
}

// FuzzDecodeTrust is the corruption-catching differential: any strictly
// valid document decoded into the kitchen sink must produce exactly
// encoding/json's result in both ownership modes, and invalid documents must
// be rejected. Arena overwrites, aliasing slips, and retention bugs all
// surface as value divergence without needing to anticipate their shape.
func FuzzDecodeTrust(f *testing.F) {
	f.Add(trustSinkDoc())
	f.Add([]byte(`{"b":"p` + jsonUnicodeEscape("0042") + `q","a":"x` + jsonUnicodeEscape("0041") + `y","s":"r` + jsonUnicodeEscape("0043") + `s"}`))
	f.Add([]byte(`{"a":[{"m":{"x":"` + jsonUnicodeEscape("2028") + `"}},"` + jsonUnicodeEscape("D834") + jsonUnicodeEscape("DD1E") + `"]}`))
	f.Add([]byte(`{"r":" not raw ","q":"7"}`))
	f.Add([]byte(`{"unknown":{"deep":["skip",{"me":1}]},"s":"kept"}`))
	// Former FuzzTypedDecoderMatchesStdlib seeds. Keeping them in this campaign
	// ensures mutations continue to exercise typedEdgeValue's exact field,
	// array, pointer, escaped-name, and duplicate-name behavior.
	for _, src := range [][]byte{
		[]byte(`{}`),
		[]byte(`null`),
		[]byte(`{"id":1,"long_field_name":"x","values":[1,2],"fixed":[3,4,5],"next":{"id":2}}`),
		[]byte(`{"\u0065scaped":"x","ID":7,"id":8}`),
	} {
		f.Add(src)
	}
	// Former scalar-slice and merge-semantics campaign seeds. Their oracles
	// now run beside the other typed-decode checks for every compatible input.
	for _, src := range []string{
		`[1,2,3]`, `[1,null,3]`, `[]`, `[ 1 , 2 ]`, `[1,2,]`, `[1e10,2e-5]`,
		`[9223372036854775807]`, `[18446744073709551615]`, `[1.5,null]`,
		`{"items":[{"id":7},{"name":"x"}],"count":null}`,
		`{"next":{"scores":[1]},"items":null}`,
	} {
		f.Add([]byte(src))
	}
	typedEdgeDecoder, err := CompileDecoder[typedEdgeValue](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	int64SliceDecoder, err := CompileDecoder[[]int64](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	uint64SliceDecoder, err := CompileDecoder[[]uint64](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	float64SliceDecoder, err := CompileDecoder[[]float64](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	mergeDecoder, err := CompileDecoder[typedTestDocument](DecoderOptions{})
	if err != nil {
		f.Fatal(err)
	}
	f.Fuzz(func(t *testing.T, src []byte) {
		// Run the typed-edge oracle before any trust-sink early return. Its own
		// original size and validity domain remains independently enforced.
		checkTypedEdgeValueMatchesStdlib(t, typedEdgeDecoder, src)
		checkScalarSliceDecodeMatchesStdlib(
			t, src, int64SliceDecoder, uint64SliceDecoder, float64SliceDecoder,
		)
		checkMergeSemanticsMatchStdlib(t, mergeDecoder, src)
		if len(src) > 1<<15 {
			t.Skip()
		}
		valid := strictJSONValid(src)
		var want trustSink
		wantErr := json.Unmarshal(src, &want)
		for name, dec := range trustDecoders {
			var got trustSink
			err := dec.Decode(src, &got)
			if !valid {
				if err == nil {
					t.Fatalf("%s accepted input that is not strict JSON (length %d)", name, len(src))
				}
				continue
			}
			if (err == nil) != (wantErr == nil) {
				t.Fatalf("%s error = %v, encoding/json error = %v", name, err, wantErr)
			}
			if err == nil && !reflect.DeepEqual(got, want) {
				t.Fatalf("%s decode differs from encoding/json:\n got: %+v\nwant: %+v", name, got, want)
			}
		}
		if !valid || wantErr != nil {
			// The round trip only holds for strictly valid input: stdlib
			// tolerates invalid UTF-8 inside RawMessage where this library's
			// encoder rejects it by design.
			return
		}
		// Close the loop through the encoder: marshaling the decoded values
		// must reproduce encoding/json byte for byte, so encoder aliasing or
		// escaping slips on fuzz-generated shapes surface here.
		wantOut, wantOutErr := json.Marshal(&want)
		gotOut, gotOutErr := Marshal(&want)
		if (gotOutErr == nil) != (wantOutErr == nil) {
			t.Fatalf("Marshal error = %v, encoding/json error = %v", gotOutErr, wantOutErr)
		}
		if gotOutErr == nil && !bytes.Equal(gotOut, wantOut) {
			t.Fatalf("Marshal differs from encoding/json:\n got: %s\nwant: %s", gotOut, wantOut)
		}
	})
}

// TestConcurrentCompiledDecoders shares one compiled decoder per mode across
// goroutines decoding distinct documents concurrently, so the race detector
// sees any shared mutable state and each goroutine verifies its own results
// against encoding/json.
func TestConcurrentCompiledDecoders(t *testing.T) {
	docs := [][]byte{
		trustSinkDoc(),
		[]byte(`{"s":"z` + jsonUnicodeEscape("005A") + `z","a":["w` + jsonUnicodeEscape("0057") + `w"],"n":1e-3}`),
		[]byte(`{"m":{"k1":"a` + jsonUnicodeEscape("00E9") + `b","k2":"plain"},"q":"123","b":"QUJD"}`),
		[]byte(`{"l":[1,"two",{"three":3}],"r":[null, false],"nn":42}`),
	}
	wants := make([]trustSink, len(docs))
	for i, doc := range docs {
		if err := json.Unmarshal(doc, &wants[i]); err != nil {
			t.Fatal(err)
		}
	}
	for name, dec := range trustDecoders {
		t.Run(name, func(t *testing.T) {
			var group errgroupLite
			for g := 0; g < 8; g++ {
				doc := docs[g%len(docs)]
				want := wants[g%len(docs)]
				group.Go(func() error {
					for range 200 {
						var got trustSink
						if err := dec.Decode(doc, &got); err != nil {
							return err
						}
						if !reflect.DeepEqual(got, want) {
							return errConcurrentDivergence
						}
					}
					return nil
				})
			}
			if err := group.Wait(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

var errConcurrentDivergence = errorString("concurrent decode diverged from encoding/json")

type errorString string

func (e errorString) Error() string { return string(e) }

// errgroupLite avoids a dependency on x/sync for one test.
type errgroupLite struct {
	wg   chan error
	jobs int
}

func (g *errgroupLite) Go(fn func() error) {
	if g.wg == nil {
		g.wg = make(chan error, 64)
	}
	g.jobs++
	go func() { g.wg <- fn() }()
}

func (g *errgroupLite) Wait() error {
	var first error
	for range g.jobs {
		if err := <-g.wg; err != nil && first == nil {
			first = err
		}
	}
	return first
}

// TestBytesArrayFormParity covers the encoding/json quirk the trust fuzz
// found: a byte slice also decodes from a JSON array of integers, with
// element range errors, empties, and null behaving exactly like the stdlib.
func TestBytesArrayFormParity(t *testing.T) {
	for _, src := range []string{
		`{"b":[72,105]}`,
		`{"b":[]}`,
		`{"b":[0,255]}`,
		`{"b":[256]}`,
		`{"b":[-1]}`,
		`{"b":[1.5]}`,
		`{"b":["x"]}`,
		`{"b":null}`,
		`{"b":"aGk="}`,
	} {
		var want trustSink
		wantErr := json.Unmarshal([]byte(src), &want)
		for name, dec := range trustDecoders {
			var got trustSink
			err := dec.Decode([]byte(src), &got)
			if (err == nil) != (wantErr == nil) {
				t.Fatalf("%s %s: error = %v, encoding/json error = %v", name, src, err, wantErr)
			}
			if err == nil && !reflect.DeepEqual(got.B, want.B) {
				t.Fatalf("%s %s: B = %#v, want %#v", name, src, got.B, want.B)
			}
		}
	}
}

// TestStringTaggedNumberParity pins the strconv semantics of string-tagged
// numbers against encoding/json: leading zeros, explicit plus signs, float
// spellings, range errors, and the null quirk must all behave identically.
func TestStringTaggedNumberParity(t *testing.T) {
	type tagged struct {
		I int64    `json:"i,string"`
		U uint8    `json:"u,string"`
		F float64  `json:"f,string"`
		P *int32   `json:"p,string"`
		G *float32 `json:"g,string"`
	}
	dec, err := CompileDecoder[tagged](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{
		`{"i":"000"}`, `{"i":"-000"}`, `{"i":"+5"}`, `{"i":"5"}`, `{"i":" 5"}`,
		`{"i":"5.0"}`, `{"i":"1e2"}`, `{"i":""}`, `{"i":"null"}`, `{"i":null}`,
		`{"i":"9223372036854775807"}`, `{"i":"9223372036854775808"}`,
		`{"u":"255"}`, `{"u":"256"}`, `{"u":"-0"}`, `{"u":"+0"}`,
		`{"f":"000.5"}`, `{"f":"+1.5"}`, `{"f":"1e2"}`, `{"f":"NaN"}`,
		`{"f":"Inf"}`, `{"f":"-Inf"}`, `{"f":"0x1p3"}`, `{"f":"1e999"}`,
		`{"f":"1_0"}`, `{"f":".5"}`, `{"f":"5."}`,
		`{"p":"012"}`, `{"p":null}`, `{"g":"08.25"}`,
	} {
		var want tagged
		wantErr := json.Unmarshal([]byte(src), &want)
		var got tagged
		gotErr := dec.Decode([]byte(src), &got)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("%s: error = %v, encoding/json error = %v", src, gotErr, wantErr)
		}
		// DeepEqual rejects NaN == NaN, but the stdlib accepts "NaN"
		// spellings for string-tagged floats; agreeing NaNs compare equal.
		if gotErr == nil && math.IsNaN(got.F) && math.IsNaN(want.F) {
			got.F, want.F = 0, 0
		}
		if gotErr == nil && !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %+v, want %+v", src, got, want)
		}
	}
}

// TestOwnedDecodeSurvivesSourceMutation proves the owned contract: after an
// owned-mode decode, destroying the source buffer must not change one byte
// of the result.
func TestOwnedDecodeSurvivesSourceMutation(t *testing.T) {
	original := trustSinkDoc()

	var want trustSink
	if err := json.Unmarshal(original, &want); err != nil {
		t.Fatal(err)
	}

	src := bytes.Clone(original)
	var got trustSink
	if err := trustDecoders["owned"].Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	for i := range src {
		src[i] = 0xAA
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("owned decode changed after source mutation:\n got: %+v\nwant: %+v", got, want)
	}
}

// TestDecoderCallsAreIndependent proves compiled decoders carry no state
// between calls: an earlier result must not change when the same decoder
// processes different documents afterwards.
func TestDecoderCallsAreIndependent(t *testing.T) {
	first := trustSinkDoc()
	second := []byte(`{"s":"z` + jsonUnicodeEscape("005A") + `z","a":"w` + jsonUnicodeEscape("0057") + `w","m":{"q":"v` + jsonUnicodeEscape("0056") + `v"}}`)

	var want trustSink
	if err := json.Unmarshal(first, &want); err != nil {
		t.Fatal(err)
	}
	for name, dec := range trustDecoders {
		var got trustSink
		if err := dec.Decode(bytes.Clone(first), &got); err != nil {
			t.Fatal(err)
		}
		for range 8 {
			var other trustSink
			if err := dec.Decode(bytes.Clone(second), &other); err != nil {
				t.Fatal(err)
			}
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: first result changed after later decodes:\n got: %+v\nwant: %+v", name, got, want)
		}
	}
}
