package slopjson

import (
	"encoding/json"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
)

type inlineRaw struct {
	ID    int                        `json:"id"`
	Name  string                     `json:"name"`
	Extra map[string]json.RawMessage `json:",inline"`
}

type inlineAny struct {
	ID    int            `json:"id"`
	Extra map[string]any `json:",inline"`
}

// inlineOnly routes every member through the lossless catch-all.
type inlineOnly struct {
	Extra map[string]json.RawMessage `json:",inline"`
}

func mustInlineDecoder[T any](t testing.TB, opts DecoderOptions) Decoder[T] {
	t.Helper()
	decoder, err := CompileDecoder[T](opts)
	if err != nil {
		t.Fatal(err)
	}
	return decoder
}

func mustInlineEncoder[T any](t testing.TB, opts EncoderOptions) Encoder[T] {
	t.Helper()
	encoder, err := CompileEncoder[T](opts)
	if err != nil {
		t.Fatal(err)
	}
	return encoder
}

func mustInlineDecode[T any](t testing.TB, decoder Decoder[T], src []byte, dst *T) {
	t.Helper()
	if err := decoder.Decode(src, dst); err != nil {
		t.Fatal(err)
	}
}

func mustInlineAppend[T any](t testing.TB, encoder Encoder[T], value *T) []byte {
	t.Helper()
	out, err := encoder.AppendJSON(nil, value)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// Unknown members decode into the catch-all and re-emit sorted after declared fields.
func TestInlineCatchAllRoundTrip(t *testing.T) {
	src := []byte(`{"id":1,"name":"x","c":"hi","a":true,"b":[1,2]}`)

	dec := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
	var v inlineRaw
	mustInlineDecode(t, dec, src, &v)
	if v.ID != 1 || v.Name != "x" {
		t.Fatalf("declared fields = %d, %q", v.ID, v.Name)
	}
	want := map[string]json.RawMessage{
		"a": json.RawMessage("true"),
		"b": json.RawMessage("[1,2]"),
		"c": json.RawMessage(`"hi"`),
	}
	if !reflect.DeepEqual(map[string]json.RawMessage(v.Extra), want) {
		t.Fatalf("catch-all = %v, want %v", v.Extra, want)
	}

	enc := mustInlineEncoder[inlineRaw](t, EncoderOptions{InlineFields: true})
	out := mustInlineAppend(t, enc, &v)
	const wantOut = `{"id":1,"name":"x","a":true,"b":[1,2],"c":"hi"}`
	if string(out) != wantOut {
		t.Fatalf("encode = %s, want %s", out, wantOut)
	}
}

func TestInlineCatchAllKeyOwnership(t *testing.T) {
	t.Run("shared owned source", func(t *testing.T) {
		decoder := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
		src := []byte(`{"id":1,"name":"owned","plain":true,"escaped\u002dkey":2}`)
		var got inlineRaw
		mustInlineDecode(t, decoder, src, &got)
		for i := range src {
			src[i] = 'x'
		}
		runtime.GC()
		if got.Name != "owned" || string(got.Extra["plain"]) != "true" || string(got.Extra["escaped-key"]) != "2" {
			t.Fatalf("owned catch-all changed after source mutation: %#v", got)
		}
	})

	t.Run("key clone before source ownership", func(t *testing.T) {
		decoder := mustInlineDecoder[inlineOnly](t, DecoderOptions{InlineFields: true})
		src := []byte(`{"plain":true}`)
		var got inlineOnly
		mustInlineDecode(t, decoder, src, &got)
		for i := range src {
			src[i] = 'x'
		}
		if string(got.Extra["plain"]) != "true" {
			t.Fatalf("catch-all key or value aliases caller source: %#v", got.Extra)
		}
	})
}

func TestInlineDecoderScratchCleared(t *testing.T) {
	decoder := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
	if decoder.root.inlineMap.decMapScratch == 0 || decoder.scratch == nil {
		t.Fatal("eligible inline map did not receive decoder scratch")
	}
	var got inlineRaw
	mustInlineDecode(t, decoder, []byte(`{"id":1,"name":"x","extra":[1,2,3]}`), &got)
	state := decoder.scratch.take()
	defer decoder.scratch.release(state)
	slot := int(decoder.root.inlineMap.decMapScratch - 1)
	scratch := &state.operation.maps[slot]
	if scratch.inUse || scratch.entries != 0 {
		t.Fatalf("released inline scratch remains active: inUse=%v entries=%d", scratch.inUse, scratch.entries)
	}
	if !scratch.key.IsValid() || !scratch.key.IsZero() {
		t.Fatal("released inline key box retained a value")
	}
	if !scratch.element.IsValid() || !scratch.element.IsZero() {
		t.Fatal("released inline element box retained a value")
	}
}

func TestInlineDecoderScratchAllocs(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector adds bookkeeping allocations")
	}
	decoder := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
	src := []byte(`{"id":1,"name":"x","alpha":true,"beta":[1,2,3],"gamma":"hello","delta":42}`)
	var warm inlineRaw
	mustInlineDecode(t, decoder, src, &warm)
	allocs := testing.AllocsPerRun(1000, func() {
		var got inlineRaw
		if err := decoder.Decode(src, &got); err != nil {
			panic(err)
		}
	})
	if allocs > 10 {
		t.Fatalf("four inline entries allocated %.1f times per decode, want <=10", allocs)
	}
}

func TestInlineCatchAllAny(t *testing.T) {
	dec := mustInlineDecoder[inlineAny](t, DecoderOptions{InlineFields: true})
	var v inlineAny
	mustInlineDecode(t, dec, []byte(`{"id":2,"flag":true,"nums":[1,2.5]}`), &v)
	if v.ID != 2 {
		t.Fatalf("id = %d", v.ID)
	}
	if v.Extra["flag"] != true || !reflect.DeepEqual(v.Extra["nums"], []any{1.0, 2.5}) {
		t.Fatalf("catch-all = %#v", v.Extra)
	}
}

func TestInlineEmptyCatchAll(t *testing.T) {
	dec := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
	var v inlineRaw
	mustInlineDecode(t, dec, []byte(`{"id":7,"name":"y"}`), &v)
	if v.Extra != nil {
		t.Fatalf("catch-all allocated with no unknown members: %v", v.Extra)
	}
	enc := mustInlineEncoder[inlineRaw](t, EncoderOptions{InlineFields: true})
	out := mustInlineAppend(t, enc, &v)
	if string(out) != `{"id":7,"name":"y"}` {
		t.Fatalf("encode = %s", out)
	}
}

// A catch-all consumes members that DisallowUnknownFields would reject.
func TestInlineCatchAllWinsOverDisallow(t *testing.T) {
	dec := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true, DisallowUnknownFields: true})
	var v inlineRaw
	if err := dec.Decode([]byte(`{"id":1,"surprise":9}`), &v); err != nil {
		t.Fatalf("catch-all did not consume the unknown member: %v", err)
	}
	if string(v.Extra["surprise"]) != "9" {
		t.Fatalf("catch-all = %v", v.Extra)
	}
}

// Catch-all members follow declared fields in sorted key order.
func TestInlineOrderingIsDeterministic(t *testing.T) {
	enc := mustInlineEncoder[inlineRaw](t, EncoderOptions{InlineFields: true})
	v := inlineRaw{ID: 1, Extra: map[string]json.RawMessage{"z": json.RawMessage("1"), "a": json.RawMessage("2")}}
	const want = `{"id":1,"name":"","a":2,"z":1}`
	for range 32 {
		out, err := enc.AppendJSON(nil, &v)
		if err != nil {
			t.Fatal(err)
		}
		if string(out) != want {
			t.Fatalf("encode = %s, want %s", out, want)
		}
	}
}

// Non-map inline fields fail compilation only when the extension is enabled.
func TestInlineRejectsNonMap(t *testing.T) {
	type bad struct {
		X int `json:",inline"`
	}
	if _, err := CompileEncoder[bad](EncoderOptions{InlineFields: true}); err == nil {
		t.Fatal("expected an error for a non-map inline field")
	}
}

// Without InlineFields, the tag is inert and Extra remains an ordinary field.
func TestInlineOptOutIsInert(t *testing.T) {
	dec := mustInlineDecoder[inlineRaw](t, DecoderOptions{})
	var v inlineRaw
	mustInlineDecode(t, dec, []byte(`{"id":1,"surprise":9}`), &v)
	if v.Extra != nil {
		t.Fatalf("catch-all captured a member with the extension off: %v", v.Extra)
	}

	enc := mustInlineEncoder[inlineRaw](t, EncoderOptions{})
	v.Extra = map[string]json.RawMessage{"k": json.RawMessage("1")}
	out := mustInlineAppend(t, enc, &v)
	if !strings.Contains(string(out), `"Extra":{"k":1}`) {
		t.Fatalf("inert map did not serialize under its field name: %s", out)
	}
}

// Default decoding merges catch-all entries; Replace clears stale entries.
func TestInlineReplaceClearsStale(t *testing.T) {
	merge := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true})
	replace := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true, Replace: true})

	var v inlineRaw
	mustInlineDecode(t, merge, []byte(`{"id":1,"a":1,"b":2}`), &v)
	mustInlineDecode(t, merge, []byte(`{"id":2,"c":3}`), &v)
	if len(v.Extra) != 3 || string(v.Extra["a"]) != "1" || string(v.Extra["c"]) != "3" {
		t.Fatalf("merge did not accumulate: %v", v.Extra)
	}

	mustInlineDecode(t, replace, []byte(`{"id":3,"d":4}`), &v)
	if len(v.Extra) != 1 || string(v.Extra["d"]) != "4" {
		t.Fatalf("replace did not clear stale members: %v", v.Extra)
	}
}

// recNode requires independent catch-all backing at every recursion level.
type recNode struct {
	V   int                `json:"v"`
	Sub map[string]recNode `json:",inline"`
}

// Recursive catch-alls must not share element boxes across levels.
func TestInlineRecursiveType(t *testing.T) {
	dec := mustInlineDecoder[recNode](t, DecoderOptions{InlineFields: true})
	enc := mustInlineEncoder[recNode](t, EncoderOptions{InlineFields: true})
	src := []byte(`{"v":1,"a":{"v":2,"b":{"v":3}},"c":{"v":4}}`)
	var v recNode
	mustInlineDecode(t, dec, src, &v)
	if v.V != 1 || v.Sub["a"].V != 2 || v.Sub["a"].Sub["b"].V != 3 || v.Sub["c"].V != 4 {
		t.Fatalf("recursive decode lost structure: %#v", v)
	}
	out := mustInlineAppend(t, enc, &v)
	if string(out) != `{"v":1,"a":{"v":2,"b":{"v":3}},"c":{"v":4}}` {
		t.Fatalf("recursive encode = %s", out)
	}
}

// Nested catch-alls require pooled backing to re-type between levels.
func TestInlineNestedDifferentTypes(t *testing.T) {
	type inner struct {
		Extra map[string]json.RawMessage `json:",inline"`
	}
	type outer struct {
		ID   int              `json:"id"`
		Kids map[string]inner `json:",inline"`
	}
	enc := mustInlineEncoder[outer](t, EncoderOptions{InlineFields: true})
	dec := mustInlineDecoder[outer](t, DecoderOptions{InlineFields: true})
	src := []byte(`{"id":1,"x":{"a":true,"b":2},"y":{"c":"z"}}`)
	var v outer
	mustInlineDecode(t, dec, src, &v)
	out := mustInlineAppend(t, enc, &v)
	if string(out) != `{"id":1,"x":{"a":true,"b":2},"y":{"c":"z"}}` {
		t.Fatalf("nested encode = %s", out)
	}
}

// Concurrent calls must receive private pooled backing.
func TestInlineConcurrentEncode(t *testing.T) {
	enc := mustInlineEncoder[inlineRaw](t, EncoderOptions{InlineFields: true})
	v := inlineRaw{ID: 9, Name: "n", Extra: map[string]json.RawMessage{
		"alpha": json.RawMessage("1"), "beta": json.RawMessage(`"two"`),
		"gamma": json.RawMessage("[3,3,3]"), "delta": json.RawMessage("true"),
	}}
	const want = `{"id":9,"name":"n","alpha":1,"beta":"two","delta":true,"gamma":[3,3,3]}`
	var wg sync.WaitGroup
	for g := 0; g < 16; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 2000; i++ {
				out, err := enc.AppendJSON(nil, &v)
				if err != nil {
					t.Errorf("encode: %v", err)
					return
				}
				if string(out) != want {
					t.Errorf("concurrent encode = %s", out)
					return
				}
			}
		}()
	}
	wg.Wait()
}

func TestInlineConcurrentDecode(t *testing.T) {
	decoder := mustInlineDecoder[inlineRaw](t, DecoderOptions{InlineFields: true, ZeroCopy: true, Replace: true})
	src := []byte(`{"id":9,"name":"n","alpha":1,"beta":"two","gamma":[3,3,3],"delta":true}`)
	var wait sync.WaitGroup
	for range 16 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			var got inlineRaw
			for range 500 {
				if err := decoder.Decode(src, &got); err != nil {
					t.Errorf("decode: %v", err)
					return
				}
				if got.ID != 9 || got.Name != "n" || string(got.Extra["alpha"]) != "1" ||
					string(got.Extra["beta"]) != `"two"` || string(got.Extra["gamma"]) != "[3,3,3]" ||
					string(got.Extra["delta"]) != "true" {
					t.Errorf("concurrent decode = %#v", got)
					return
				}
			}
		}()
	}
	wait.Wait()
}

// Reused encoder scratch should make a populated catch-all allocation-free.
func BenchmarkInlineEncode(b *testing.B) {
	enc := mustInlineEncoder[inlineRaw](b, EncoderOptions{InlineFields: true})
	v := inlineRaw{ID: 1, Name: "x", Extra: map[string]json.RawMessage{
		"alpha": json.RawMessage("true"),
		"beta":  json.RawMessage("[1,2,3]"),
		"gamma": json.RawMessage(`"hello"`),
		"delta": json.RawMessage("42"),
	}}
	buf := make([]byte, 0, 128)
	var err error
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, err = enc.AppendJSON(buf[:0], &v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// One reusable element should serve every decoded catch-all member.
func BenchmarkInlineDecode(b *testing.B) {
	dec := mustInlineDecoder[inlineRaw](b, DecoderOptions{InlineFields: true})
	src := []byte(`{"id":1,"name":"x","alpha":true,"beta":[1,2,3],"gamma":"hello","delta":42}`)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var v inlineRaw
		if err := dec.Decode(src, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// checkInlineRoundTrip is the catch-all oracle in the encoder fuzz campaign.
func checkInlineRoundTrip(t *testing.T, src []byte, dec Decoder[inlineOnly], enc Encoder[inlineOnly]) {
	t.Helper()
	var want map[string]any
	if err := json.Unmarshal(src, &want); err != nil || want == nil {
		return // not a JSON object (or a top-level null): nothing to prove
	}
	var v inlineOnly
	if err := dec.Decode(src, &v); err != nil {
		return // decode compatibility is the corpus's job, not this oracle's
	}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatalf("encode failed: %v (src=%s)", err, src)
	}
	var got map[string]any
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("re-encoded output is not valid JSON: %v (%s)", err, out)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round trip changed the object:\n src=%s\n out=%s\n want=%#v\n got=%#v", src, out, want, got)
	}
}
