package slopjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

type ownedContractDocument struct {
	S string            `json:"s"`
	N json.Number       `json:"n"`
	A any               `json:"a"`
	M map[string]string `json:"m"`
	Q int               `json:"q,string"`
	B []byte            `json:"b"`
}

func checkOwnershipDecoderModes[T any](t *testing.T, src []byte, check func(zeroCopy bool, got, want T)) {
	t.Helper()
	var want T
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	for _, zeroCopy := range []bool{false, true} {
		dec, err := CompileDecoder[T](DecoderOptions{ZeroCopy: zeroCopy})
		if err != nil {
			t.Fatal(err)
		}
		var got T
		if err := dec.Decode(src, &got); err != nil {
			t.Fatalf("ZeroCopy=%v: %v", zeroCopy, err)
		}
		check(zeroCopy, got, want)
	}
}

// TestOwnedModeNeverAliasesCallerSrc proves owned-mode decodes never
// alias caller src. Each case decodes every retaining kind with default
// (owned) options, scribbles the source buffer, and checks the result byte
// for byte against encoding/json. The field order varies per case so each
// retaining kind gets to be the first ownSource trigger.
func TestOwnedModeNeverAliasesCallerSrc(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"string first", `{"s":"hello world","n":123.456,"a":{"k":"va` + jsonUnicodeEscape("00e9") + `l","arr":[1,"two",null]},"m":{"k1":"v1","k` + jsonUnicodeEscape("0032") + `":"v2"},"q":"42","b":"aGVsbG8="}`},
		{"number first", `{"n":987654321012345678,"s":"later","a":"plain","m":{"x":"y"},"q":"7","b":"QQ=="}`},
		{"any first", `{"a":[1.5,"str",{"deep":"va` + jsonUnicodeEscape("0041") + `l"}],"s":"tail","n":1e10,"m":{"only":"one"},"q":"-3","b":""}`},
		{"map first", `{"m":{"first":"map","se` + jsonUnicodeEscape("0063") + `ond":"pair"},"a":true,"s":"x","n":0.5,"q":"0","b":"AA=="}`},
		{"quoted first", `{"q":"1234","m":{},"a":null,"s":"es` + jsonUnicodeEscape("0063") + `aped","n":2,"b":"aGk="}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := []byte(tc.body)
			var want ownedContractDocument
			if err := json.Unmarshal(src, &want); err != nil {
				t.Fatalf("reference: %v", err)
			}
			var got ownedContractDocument
			if err := Unmarshal(src, &got); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			// Scribble the entire caller buffer.
			for i := range src {
				src[i] = 'Z'
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("after src scribble:\ngot  %#v\nwant %#v", got, want)
			}
		})
	}
}

// Top-level retaining shapes: any, map, slice-of-string.
func TestOwnedTopLevelShapes(t *testing.T) {
	assertOwnedTopLevelShape[any](t, "any",
		[]byte(`{"k":"clean","e":"a`+jsonUnicodeEscape("0042")+`c","nested":[1,"two"]}`))
	assertOwnedTopLevelShape[map[string]string](t, "map",
		[]byte(`{"alpha":"one","beta":"t`+jsonUnicodeEscape("0077")+`o"}`))
	assertOwnedTopLevelShape[[]string](t, "slice",
		[]byte(`["plain","es`+jsonUnicodeEscape("0063")+`aped","third"]`))
}

func assertOwnedTopLevelShape[T any](t *testing.T, name string, src []byte) {
	t.Helper()
	var want, got T
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	if err := Unmarshal(src, &got); err != nil {
		t.Fatal(err)
	}
	for i := range src {
		src[i] = 0xEE
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: got %#v want %#v", name, got, want)
	}
}

// TestAnyArenaBlockSwitch proves dynamic (any) values that materialize
// many escaped strings survive arena block switches inside the dynamic parse,
// and that later escaped typed strings append after — not over — them.
func TestAnyArenaBlockSwitch(t *testing.T) {
	// One escaped string long enough to matter, repeated enough times inside
	// the any value to cross several 2 KiB arena blocks (stringArenaSeed).
	esc := strings.Repeat(jsonUnicodeEscape("00e9")+"unit", 30) // ~150 decoded bytes each
	var b strings.Builder
	b.WriteString(`{"pre":"p` + jsonUnicodeEscape("0050") + `p","dyn":{`)
	const dynKeys = 64
	for i := 0; i < dynKeys; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"k%02d":"%s"`, i, esc)
	}
	b.WriteString(`},"post":[`)
	const postStrings = 64
	for i := 0; i < postStrings; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `"t%02d%s"`, i, esc)
	}
	b.WriteString(`]}`)
	src := []byte(b.String())

	type doc struct {
		Pre  string   `json:"pre"`
		Dyn  any      `json:"dyn"`
		Post []string `json:"post"`
	}
	checkOwnershipDecoderModes(t, src, func(zeroCopy bool, got, want doc) {
		if !reflect.DeepEqual(got, want) {
			// Locate the first mismatch precisely for the report.
			gm := got.Dyn.(map[string]any)
			wm := want.Dyn.(map[string]any)
			for k, wv := range wm {
				if gm[k] != wv {
					t.Fatalf("ZeroCopy=%v: dyn[%q] = %.60q, want %.60q", zeroCopy, k, gm[k], wv)
				}
			}
			for i := range want.Post {
				if got.Post[i] != want.Post[i] {
					t.Fatalf("ZeroCopy=%v: post[%d] = %.60q, want %.60q", zeroCopy, i, got.Post[i], want.Post[i])
				}
			}
			t.Fatalf("ZeroCopy=%v: mismatch (pre=%q)", zeroCopy, got.Pre)
		}
	})
}

// Interleave typed escaped strings and any values so the arena alternates
// between cursor-side and parser-side appends across block switches.
func TestInterleavedTypedAndDynamicEscapes(t *testing.T) {
	type pair struct {
		S string `json:"s"`
		A any    `json:"a"`
	}
	esc := strings.Repeat(jsonUnicodeEscape("0041")+"b", 100) // 200 decoded bytes
	var b strings.Builder
	b.WriteByte('[')
	const n = 48
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, `{"s":"s%02d%s","a":"a%02d%s"}`, i, esc, i, esc)
	}
	b.WriteByte(']')
	src := []byte(b.String())

	checkOwnershipDecoderModes(t, src, func(zeroCopy bool, got, want []pair) {
		for i := range want {
			if got[i].S != want[i].S {
				t.Fatalf("ZeroCopy=%v: [%d].S = %.60q, want %.60q", zeroCopy, i, got[i].S, want[i].S)
			}
			if got[i].A != want[i].A {
				t.Fatalf("ZeroCopy=%v: [%d].A = %.60q, want %.60q", zeroCopy, i, got[i].A, want[i].A)
			}
		}
	})
}

// TestEscapedMapKeysArena proves escaped map keys retained by the result
// map survive later escaped strings — keys and values alike — appending to the
// arena.
func TestEscapedMapKeysArena(t *testing.T) {
	var b strings.Builder
	b.WriteByte('{')
	const n = 80
	for i := 0; i < n; i++ {
		if i > 0 {
			b.WriteByte(',')
		}
		// Escaped key and escaped value, each unique.
		fmt.Fprintf(&b, `"key%02d%s":"val%02d%s"`,
			i, strings.Repeat(jsonUnicodeEscape("00e9"), 20),
			i, strings.Repeat(jsonUnicodeEscape("00fc"), 20))
	}
	b.WriteByte('}')
	src := []byte(b.String())

	checkOwnershipDecoderModes(t, src, func(zeroCopy bool, got, want map[string]string) {
		if len(got) != len(want) {
			t.Fatalf("ZeroCopy=%v: %d entries, want %d", zeroCopy, len(got), len(want))
		}
		for k, wv := range want {
			gv, ok := got[k]
			if !ok {
				t.Fatalf("ZeroCopy=%v: missing key %.40q", zeroCopy, k)
			}
			if gv != wv {
				t.Fatalf("ZeroCopy=%v: got[%.40q] = %.40q, want %.40q", zeroCopy, k, gv, wv)
			}
		}
	})

	// map[string]any: keys through typedKey, values through the dynamic parse.
	var wantAny map[string]any
	if err := json.Unmarshal(src, &wantAny); err != nil {
		t.Fatal(err)
	}
	var gotAny map[string]any
	if err := Unmarshal(src, &gotAny); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotAny, wantAny) {
		t.Fatalf("map[string]any mismatch:\ngot  %#v\nwant %#v", gotAny, wantAny)
	}
}

// TestQuotedFieldTransientArena proves that quoted (",string") fields
// whose inner content is escaped — which use a transient arena region — do not
// leave the decoded result aliasing that region once later escaped strings
// reuse it.
func TestQuotedFieldTransientArena(t *testing.T) {
	type doc struct {
		Born string      `json:"born"` // escaped: creates the arena
		Q    string      `json:"q,string"`
		N    json.Number `json:"n,string"`
		I    int         `json:"i,string"`
		Tail string      `json:"tail"` // escaped: appends over transient bytes
	}
	// Q's inner JSON string is itself escaped twice over (outer layer consumed
	// by stringToken, inner layer by the sub-decode).
	src := []byte(`{"born":"b` + jsonUnicodeEscape("0042") + `b",` +
		`"q":"\"inner` + `\\` + `u0041value\"",` +
		`"n":"12` + jsonUnicodeEscape("0033") + `.5",` +
		`"i":"4` + jsonUnicodeEscape("0032") + `",` +
		`"tail":"t` + strings.Repeat(jsonUnicodeEscape("0054"), 40) + `t"}`)
	checkOwnershipDecoderModes(t, src, func(zeroCopy bool, got, want doc) {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ZeroCopy=%v:\ngot  %#v\nwant %#v", zeroCopy, got, want)
		}
	})
}

// Escaped base64: the []byte result decodes out of the transient region and
// must be a private copy.
func TestBytesFromEscapedBase64(t *testing.T) {
	type doc struct {
		Born string `json:"born"`
		B    []byte `json:"b"`
		Tail string `json:"tail"`
	}
	// "aGVsbG8=" with the '8' spelled as an escape lands on the arena path.
	src := []byte(`{"born":"x` + jsonUnicodeEscape("0058") + `x",` +
		`"b":"aGVsbG` + jsonUnicodeEscape("0038") + `=",` +
		`"tail":"` + strings.Repeat(jsonUnicodeEscape("0059"), 40) + `"}`)
	checkOwnershipDecoderModes(t, src, func(zeroCopy bool, got, want doc) {
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("ZeroCopy=%v:\ngot  %#v\nwant %#v", zeroCopy, got, want)
		}
	})
}

type streamRec struct {
	ID   int               `json:"id"`
	Name string            `json:"name"`
	Any  any               `json:"any"`
	M    map[string]string `json:"m"`
}

func buildStreamDocs(t *testing.T, count int) ([]byte, []streamRec) {
	t.Helper()
	var b bytes.Buffer
	var want []streamRec
	for i := 0; i < count; i++ {
		doc := fmt.Sprintf(
			`{"id":%d,"name":"n%02d%sx","any":{"k":"a%02d%s"},"m":{"mk%s":"mv%02d"}}`,
			i, i, strings.Repeat(jsonUnicodeEscape("00e9"), 8),
			i, strings.Repeat(jsonUnicodeEscape("0041"), 8),
			jsonUnicodeEscape("00fc"), i)
		b.WriteString(doc)
		b.WriteByte('\n')
		var w streamRec
		if err := json.Unmarshal([]byte(doc), &w); err != nil {
			t.Fatal(err)
		}
		want = append(want, w)
	}
	return b.Bytes(), want
}

// TestStreamDecodeNextSplitValues drives the streaming Reader over values
// split across arbitrarily small reads, exercising the retry, compaction, and
// growth paths. Each decoded value is checked inside its validity window.
func TestStreamDecodeNextSplitValues(t *testing.T) {
	data, want := buildStreamDocs(t, 25)
	for _, zeroCopy := range []bool{false, true} {
		dec, err := CompileDecoder[streamRec](DecoderOptions{ZeroCopy: zeroCopy, Replace: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range []int{1, 2, 3, 7, 64, len(data)} {
			r := newSizedReader(&chunkReader{data: data, chunk: chunk}, 512)
			var got streamRec
			i := 0
			for DecodeNext(r, dec, &got) {
				if i >= len(want) {
					t.Fatalf("zeroCopy=%v chunk=%d: extra value %d", zeroCopy, chunk, i)
				}
				// Check within the validity window.
				if !reflect.DeepEqual(got, want[i]) {
					t.Fatalf("zeroCopy=%v chunk=%d: value %d\ngot  %#v\nwant %#v", zeroCopy, chunk, i, got, want[i])
				}
				i++
			}
			if err := r.Err(); err != nil {
				t.Fatalf("zeroCopy=%v chunk=%d: %v", zeroCopy, chunk, err)
			}
			if i != len(want) {
				t.Fatalf("zeroCopy=%v chunk=%d: %d values, want %d", zeroCopy, chunk, i, len(want))
			}
		}
	}
}

// Owned decodes must survive subsequent Next/DecodeNext calls that rewrite
// the rolling buffer.
func TestStreamOwnedRetentionAcrossNext(t *testing.T) {
	data, want := buildStreamDocs(t, 25)
	dec, err := CompileDecoder[streamRec](DecoderOptions{Replace: true})
	if err != nil {
		t.Fatal(err)
	}
	r := newSizedReader(&chunkReader{data: data, chunk: 3}, 512)
	var retained []streamRec
	var cur streamRec
	for DecodeNext(r, dec, &cur) {
		retained = append(retained, cur) // shallow copy: strings/maps/any alias cur's decode
		cur = streamRec{}                // do not let the next decode merge into retained maps
	}
	if err := r.Err(); err != nil {
		t.Fatal(err)
	}
	if len(retained) != len(want) {
		t.Fatalf("%d values, want %d", len(retained), len(want))
	}
	for i := range want {
		if !reflect.DeepEqual(retained[i], want[i]) {
			t.Fatalf("retained value %d corrupted:\ngot  %#v\nwant %#v", i, retained[i], want[i])
		}
	}
}

// Bytes across buffer growth and compaction, checked inside the window.
func TestReaderBytesGrowAndCompact(t *testing.T) {
	var docs [][]byte
	var stream bytes.Buffer
	for i := 0; i < 12; i++ {
		doc := []byte(fmt.Sprintf(`{"i":%d,"pad":%q}`, i, strings.Repeat("p", 300+37*i)))
		docs = append(docs, doc)
		stream.Write(doc)
		stream.WriteByte('\n')
	}
	for _, chunk := range []int{1, 5, 511, stream.Len()} {
		r := newSizedReader(&chunkReader{data: stream.Bytes(), chunk: chunk}, 512)
		i := 0
		for r.Next() {
			if !bytes.Equal(r.Bytes(), docs[i]) {
				t.Fatalf("chunk=%d: value %d Bytes mismatch:\ngot  %s\nwant %s", chunk, i, r.Bytes(), docs[i])
			}
			i++
		}
		if err := r.Err(); err != nil {
			t.Fatal(err)
		}
		if i != len(docs) {
			t.Fatalf("chunk=%d: %d values, want %d", chunk, i, len(docs))
		}
	}
}

// TestUnmarshalAnySlabIsolation exercises the dynamic decoder's slab
// arena. Arrays
// with 1..10 elements drive slab slot handoff, append growth past the
// 4-element slot capacity, and slab replacement; the whole tree is compared
// against encoding/json.
func TestUnmarshalAnySlabIsolation(t *testing.T) {
	rng := rand.New(rand.NewSource(7))
	var build func(depth int) string
	build = func(depth int) string {
		if depth >= 4 {
			return fmt.Sprintf("%d", rng.Intn(1000))
		}
		n := 1 + rng.Intn(10)
		parts := make([]string, n)
		for i := range parts {
			switch rng.Intn(4) {
			case 0:
				parts[i] = build(depth + 1)
			case 1:
				parts[i] = fmt.Sprintf(`"s%d"`, rng.Intn(1000))
			case 2:
				parts[i] = fmt.Sprintf("%d.%d", rng.Intn(100), rng.Intn(100))
			default:
				parts[i] = "null"
			}
		}
		return "[" + strings.Join(parts, ",") + "]"
	}
	for round := 0; round < 50; round++ {
		src := []byte(build(0))
		if len(src) <= 64 {
			src = []byte("[" + string(src) + "," + string(src) + `,"padpadpadpadpadpadpadpadpadpadpadpadpadpadpad"]`)
		}
		var want, got any
		if err := json.Unmarshal(src, &want); err != nil {
			t.Fatal(err)
		}
		got, err := unmarshalAnyForTest(src)
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		// Dynamic decoding boxes numbers as float64, exactly like encoding/json,
		// so the trees compare directly.
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("round %d mismatch:\nsrc  %s\ngot  %#v\nwant %#v", round, src, got, want)
		}
	}
}

// TestParseValueRetentionAcrossPoolReuse proves Parse AST retention:
// values from an earlier ParseOptions call stay intact while the tape pool is
// reused by later parses and after the original source is scribbled (owned
// mode).
func TestParseValueRetentionAcrossPoolReuse(t *testing.T) {
	src := []byte(`{"a":"alpha","e":"b` + jsonUnicodeEscape("00e9") + `ta","n":42.5,"arr":[1,"two",{"deep":"d"}]}`)
	var wantJSON map[string]any
	if err := json.Unmarshal(src, &wantJSON); err != nil {
		t.Fatal(err)
	}
	v, err := Parse(src)
	if err != nil {
		t.Fatal(err)
	}
	// Scribble the source (owned mode must not alias it).
	for i := range src {
		src[i] = '!'
	}
	// Churn the tape pool with other documents.
	for i := 0; i < 64; i++ {
		other := []byte(fmt.Sprintf(`[{"x":"%d","y":[%d,%d,"%s"]},"filler%d"]`, i, i, i*2, strings.Repeat("f", i), i))
		if _, err := Parse(other); err != nil {
			t.Fatal(err)
		}
	}
	got := v.Any()
	// Value.Any yields json.Number; convert want accordingly via round trip.
	gotJSON, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotBack map[string]any
	if err := json.Unmarshal(gotJSON, &gotBack); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotBack, wantJSON) {
		t.Fatalf("retained Value corrupted:\ngot  %#v\nwant %#v", gotBack, wantJSON)
	}
}
