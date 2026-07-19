package simdjson

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

// inlineOnly declares nothing but the catch-all, so every object member flows
// through it. It anchors the lossless round-trip fuzz below.
type inlineOnly struct {
	Extra map[string]json.RawMessage `json:",inline"`
}

// TestInlineCatchAllRoundTrip covers the core contract: unknown root members
// decode into the ",inline" map and re-emit at the object's own level, after
// the declared fields, in sorted order.
func TestInlineCatchAllRoundTrip(t *testing.T) {
	src := []byte(`{"id":1,"name":"x","c":"hi","a":true,"b":[1,2]}`)

	dec, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	var v inlineRaw
	if err := dec.Decode(src, &v); err != nil {
		t.Fatal(err)
	}
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

	enc, err := CompileEncoder[inlineRaw](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	// Declared fields in struct order, then the catch-all members sorted.
	const wantOut = `{"id":1,"name":"x","a":true,"b":[1,2],"c":"hi"}`
	if string(out) != wantOut {
		t.Fatalf("encode = %s, want %s", out, wantOut)
	}
}

func TestInlineCatchAllKeyOwnership(t *testing.T) {
	t.Run("shared owned source", func(t *testing.T) {
		decoder, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
		if err != nil {
			t.Fatal(err)
		}
		src := []byte(`{"id":1,"name":"owned","plain":true,"escaped\u002dkey":2}`)
		var got inlineRaw
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
		for i := range src {
			src[i] = 'x'
		}
		runtime.GC()
		if got.Name != "owned" || string(got.Extra["plain"]) != "true" || string(got.Extra["escaped-key"]) != "2" {
			t.Fatalf("owned catch-all changed after source mutation: %#v", got)
		}
	})

	t.Run("key clone before source ownership", func(t *testing.T) {
		decoder, err := CompileDecoder[inlineOnly](DecoderOptions{InlineFields: true})
		if err != nil {
			t.Fatal(err)
		}
		src := []byte(`{"plain":true}`)
		var got inlineOnly
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatal(err)
		}
		for i := range src {
			src[i] = 'x'
		}
		if string(got.Extra["plain"]) != "true" {
			t.Fatalf("catch-all key or value aliases caller source: %#v", got.Extra)
		}
	})
}

func TestInlineDecoderScratchCleared(t *testing.T) {
	decoder, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	if decoder.root.inlineMap.decMapScratch == 0 || decoder.scratch == nil {
		t.Fatal("eligible inline map did not receive decoder scratch")
	}
	var got inlineRaw
	if err := decoder.Decode([]byte(`{"id":1,"name":"x","extra":[1,2,3]}`), &got); err != nil {
		t.Fatal(err)
	}
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
	decoder, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":1,"name":"x","alpha":true,"beta":[1,2,3],"gamma":"hello","delta":42}`)
	var warm inlineRaw
	if err := decoder.Decode(src, &warm); err != nil {
		t.Fatal(err)
	}
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

// TestInlineCatchAllAny checks a map[string]any catch-all decodes the dynamic
// shapes and re-emits them.
func TestInlineCatchAllAny(t *testing.T) {
	dec, err := CompileDecoder[inlineAny](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	var v inlineAny
	if err := dec.Decode([]byte(`{"id":2,"flag":true,"nums":[1,2.5]}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.ID != 2 {
		t.Fatalf("id = %d", v.ID)
	}
	if v.Extra["flag"] != true || !reflect.DeepEqual(v.Extra["nums"], []any{1.0, 2.5}) {
		t.Fatalf("catch-all = %#v", v.Extra)
	}
}

// TestInlineEmptyCatchAll checks that no unknown members leaves the map nil and
// emits nothing extra.
func TestInlineEmptyCatchAll(t *testing.T) {
	dec, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	var v inlineRaw
	if err := dec.Decode([]byte(`{"id":7,"name":"y"}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Extra != nil {
		t.Fatalf("catch-all allocated with no unknown members: %v", v.Extra)
	}
	enc, err := CompileEncoder[inlineRaw](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":7,"name":"y"}` {
		t.Fatalf("encode = %s", out)
	}
}

// TestInlineCatchAllWinsOverDisallow verifies the catch-all consumes members
// that DisallowUnknownFields would otherwise reject.
func TestInlineCatchAllWinsOverDisallow(t *testing.T) {
	dec, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true, DisallowUnknownFields: true})
	if err != nil {
		t.Fatal(err)
	}
	var v inlineRaw
	if err := dec.Decode([]byte(`{"id":1,"surprise":9}`), &v); err != nil {
		t.Fatalf("catch-all did not consume the unknown member: %v", err)
	}
	if string(v.Extra["surprise"]) != "9" {
		t.Fatalf("catch-all = %v", v.Extra)
	}
}

// TestInlineOrderingIsDeterministic pins the only retained ordering contract:
// catch-all members follow declared fields in sorted key order, independent of
// the map's randomized iteration order.
func TestInlineOrderingIsDeterministic(t *testing.T) {
	enc, err := CompileEncoder[inlineRaw](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
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

// TestInlineRejectsNonMap rejects a non-map ",inline" field at compile time,
// but only once the extension is opted in.
func TestInlineRejectsNonMap(t *testing.T) {
	type bad struct {
		X int `json:",inline"`
	}
	if _, err := CompileEncoder[bad](EncoderOptions{InlineFields: true}); err == nil {
		t.Fatal("expected an error for a non-map inline field")
	}
}

// TestInlineOptOutIsInert is the opt-in proof: without InlineFields the tag is
// an ordinary field named by its Go name. Unknown members are not captured and
// the map serializes under its field name, exactly as it would with no tag.
func TestInlineOptOutIsInert(t *testing.T) {
	dec, err := CompileDecoder[inlineRaw](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var v inlineRaw
	if err := dec.Decode([]byte(`{"id":1,"surprise":9}`), &v); err != nil {
		t.Fatal(err)
	}
	if v.Extra != nil {
		t.Fatalf("catch-all captured a member with the extension off: %v", v.Extra)
	}

	enc, err := CompileEncoder[inlineRaw](EncoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	v.Extra = map[string]json.RawMessage{"k": json.RawMessage("1")}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), `"Extra":{"k":1}`) {
		t.Fatalf("inert map did not serialize under its field name: %s", out)
	}
}

// TestInlineReplaceClearsStale pins the reuse semantics: the default merge
// keeps unknown members from a prior decode, while Replace clears them so the
// map reflects only the current document.
func TestInlineReplaceClearsStale(t *testing.T) {
	merge, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	replace, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true, Replace: true})
	if err != nil {
		t.Fatal(err)
	}

	var v inlineRaw
	if err := merge.Decode([]byte(`{"id":1,"a":1,"b":2}`), &v); err != nil {
		t.Fatal(err)
	}
	// Merge into the same destination: the new unknown joins the survivors.
	if err := merge.Decode([]byte(`{"id":2,"c":3}`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Extra) != 3 || string(v.Extra["a"]) != "1" || string(v.Extra["c"]) != "3" {
		t.Fatalf("merge did not accumulate: %v", v.Extra)
	}

	// Replace into the same destination: only the current unknown remains.
	if err := replace.Decode([]byte(`{"id":3,"d":4}`), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Extra) != 1 || string(v.Extra["d"]) != "4" {
		t.Fatalf("replace did not clear stale members: %v", v.Extra)
	}
}

// recNode is a catch-all whose value type is itself, exercising the encoder's
// promise that each member gets independent backing storage: an outer member's
// slot must survive while encoding it recurses into the same catch-all.
type recNode struct {
	V   int                `json:"v"`
	Sub map[string]recNode `json:",inline"`
}

// TestInlineRecursiveType round-trips a self-referential catch-all. If the
// encoder shared one element box across recursion levels, an outer value would
// be clobbered while its own sub-map encoded.
func TestInlineRecursiveType(t *testing.T) {
	dec, err := CompileDecoder[recNode](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	enc, err := CompileEncoder[recNode](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"v":1,"a":{"v":2,"b":{"v":3}},"c":{"v":4}}`)
	var v recNode
	if err := dec.Decode(src, &v); err != nil {
		t.Fatal(err)
	}
	if v.V != 1 || v.Sub["a"].V != 2 || v.Sub["a"].Sub["b"].V != 3 || v.Sub["c"].V != 4 {
		t.Fatalf("recursive decode lost structure: %#v", v)
	}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"v":1,"a":{"v":2,"b":{"v":3}},"c":{"v":4}}` {
		t.Fatalf("recursive encode = %s", out)
	}
}

// TestInlineNestedDifferentTypes nests catch-alls of different value types so
// the pooled backing must re-type between the outer and inner encode.
func TestInlineNestedDifferentTypes(t *testing.T) {
	type inner struct {
		Extra map[string]json.RawMessage `json:",inline"`
	}
	type outer struct {
		ID   int              `json:"id"`
		Kids map[string]inner `json:",inline"`
	}
	enc, err := CompileEncoder[outer](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	dec, err := CompileDecoder[outer](DecoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
	src := []byte(`{"id":1,"x":{"a":true,"b":2},"y":{"c":"z"}}`)
	var v outer
	if err := dec.Decode(src, &v); err != nil {
		t.Fatal(err)
	}
	out, err := enc.AppendJSON(nil, &v)
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"id":1,"x":{"a":true,"b":2},"y":{"c":"z"}}` {
		t.Fatalf("nested encode = %s", out)
	}
}

// TestInlineConcurrentEncode hammers one encoder from many goroutines: the
// pooled backing must stay private to each AppendJSON call.
func TestInlineConcurrentEncode(t *testing.T) {
	enc, err := CompileEncoder[inlineRaw](EncoderOptions{InlineFields: true})
	if err != nil {
		t.Fatal(err)
	}
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
	decoder, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true, ZeroCopy: true, Replace: true})
	if err != nil {
		t.Fatal(err)
	}
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

// BenchmarkInlineEncode measures a populated catch-all with a reused encoder:
// the pooled scratch makes it allocation-free after warmup.
func BenchmarkInlineEncode(b *testing.B) {
	enc, err := CompileEncoder[inlineRaw](EncoderOptions{InlineFields: true})
	if err != nil {
		b.Fatal(err)
	}
	v := inlineRaw{ID: 1, Name: "x", Extra: map[string]json.RawMessage{
		"alpha": json.RawMessage("true"),
		"beta":  json.RawMessage("[1,2,3]"),
		"gamma": json.RawMessage(`"hello"`),
		"delta": json.RawMessage("42"),
	}}
	buf := make([]byte, 0, 128)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		buf, err = enc.AppendJSON(buf[:0], &v)
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInlineDecode measures decoding unknown members into the catch-all:
// one reusable element serves every member, so cost does not scale with the
// number of unknowns beyond the map's own storage.
func BenchmarkInlineDecode(b *testing.B) {
	dec, err := CompileDecoder[inlineRaw](DecoderOptions{InlineFields: true})
	if err != nil {
		b.Fatal(err)
	}
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

// FuzzInlineRoundTrip is the powerful invariant: a struct whose only field is
// the catch-all captures every member of any object it decodes, so re-encoding
// must reproduce the same object. Decode compatibility itself is covered by the
// validation corpus; here we assume a decodable input and prove no member,
// value, or key is lost or invented on the way back out.
func FuzzInlineRoundTrip(f *testing.F) {
	for _, seed := range []string{
		`{}`,
		`{"id":1,"name":"x"}`,
		`{"a":true,"b":[1,2],"c":"hi"}`,
		`{"zebra":1,"alpha":2,"mango":3}`,
		`{"nested":{"deep":[{"k":"v"}]},"n":-0.5}`,
		`{"unié":true,"esc\"key":"v"}`,
		`{"a":1,"a":2}`,
		`{"big":123456789012345678,"f":1e30,"z":0.0}`,
	} {
		f.Add([]byte(seed))
	}

	dec, err := CompileDecoder[inlineOnly](DecoderOptions{InlineFields: true})
	if err != nil {
		f.Fatal(err)
	}
	enc, err := CompileEncoder[inlineOnly](EncoderOptions{InlineFields: true})
	if err != nil {
		f.Fatal(err)
	}

	f.Fuzz(func(t *testing.T, src []byte) {
		var want map[string]any
		if err := json.Unmarshal(src, &want); err != nil || want == nil {
			return // not a JSON object (or a top-level null): nothing to prove
		}
		var v inlineOnly
		if err := dec.Decode(src, &v); err != nil {
			return // decode compatibility is the corpus's job, not this test's
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
	})
}
