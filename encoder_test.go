package slopjson

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// stdlibCompactJSON is plain encoding/json.Marshal: the default byte format
// the compiled encoder targets, HTML escaping included.
func stdlibCompactJSON(t *testing.T, v any) ([]byte, error) {
	t.Helper()
	return json.Marshal(v)
}

type encodeOmitEmpty struct {
	Bool    bool        `json:"bool,omitempty"`
	Int     int         `json:"int,omitempty"`
	Uint    uint8       `json:"uint,omitempty"`
	Float   float64     `json:"float,omitempty"`
	Text    string      `json:"text,omitempty"`
	Number  json.Number `json:"number,omitempty"`
	Slice   []int       `json:"slice,omitempty"`
	Pointer *int        `json:"pointer,omitempty"`
	Keep    int         `json:"keep"`
}

type encodeEdge struct {
	//lint:ignore SA5008 malformed tag is intentional encoding/json parity input
	Dash    int     `json:"-,"`
	Renamed float32 `json:"float 32"`
	Escaped string  `json:"escaped"`
}

func TestEncoderMatchesStdlib(t *testing.T) {
	one := 1
	values := []any{
		&typedTestRecord{ID: 42, OK: true, Name: "plain", Scores: [3]float64{1, 2.5, -3e4}, Number: json.Number("12.5e3")},
		&typedTestRecord{Name: "esc \" \\ \n \r \t \b \f \x01 <&>     héllo"},
		&typedTestRecord{Name: string([]byte{'b', 'a', 'd', 0xFF, 0xFE, 'x'})},
		&typedTestDocument{},
		&typedTestDocument{Items: []typedTestRecord{}, Count: 7},
		&typedTestDocument{Items: []typedTestRecord{{ID: 1}, {ID: 2, Number: json.Number("-0.5")}}, Next: &typedTestRecord{ID: 3}},
		&encodeOmitEmpty{},
		&encodeOmitEmpty{Bool: true, Int: -1, Uint: 2, Float: 0.5, Text: "x", Number: json.Number("9"), Slice: []int{0}, Pointer: &one},
		&encodeOmitEmpty{Float: math.Copysign(0, -1)}, // negative zero is empty for omitempty
		&encodeEdge{Dash: 1, Renamed: 2.5, Escaped: "ok"},
		&typedEdgeValue{ID: 5, Long: "long", Values: []int{1, 2, 3}, Fixed: [3]typedEdgeInt{7, 8, 9}},
	}
	for _, value := range values {
		want, wantErr := stdlibCompactJSON(t, value)
		got, gotErr := marshalAnyForTest(t, value)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("%#v: acceptance differs: slopjson=%v stdlib=%v", value, gotErr, wantErr)
		}
		if gotErr != nil {
			continue
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("%#v:\nslopjson %s\nstdlib   %s", value, got, want)
		}
	}
}

// marshalAnyForTest dispatches the concrete pointer types used by the
// differential tests through the generic Marshal entry point.
func marshalAnyForTest(t *testing.T, value any) ([]byte, error) {
	t.Helper()
	switch v := value.(type) {
	case *typedTestRecord:
		return Marshal(v)
	case *typedTestDocument:
		return Marshal(v)
	case *encodeOmitEmpty:
		return Marshal(v)
	case *encodeEdge:
		return Marshal(v)
	case *typedEdgeValue:
		return Marshal(v)
	default:
		t.Fatalf("unsupported test type %T", value)
		return nil, nil
	}
}

func TestEncoderFloatFormats(t *testing.T) {
	floats := []float64{
		0, math.Copysign(0, -1), 1, -1, 0.5, 1e-6, 9.9e-7, 1e20, 1e21, 1.5e22,
		-2.75e-9, 123456789.123456789, math.MaxFloat64, math.SmallestNonzeroFloat64,
		3.14159265358979, 1e6, 2e8,
	}
	type wrapper struct {
		F64 float64 `json:"f64"`
		F32 float32 `json:"f32"`
	}
	for _, f := range floats {
		value := wrapper{F64: f, F32: float32(f)}
		assertEncodesLikeStdlib(t, &value)
	}
}

func TestEncoderFloatFormatsDifferential(t *testing.T) {
	type wrapper struct {
		F float64 `json:"f"`
	}
	encoder := mustCompileTestEncoder[wrapper](t, EncoderOptions{})
	state := uint64(0x9e3779b97f4a7c15)
	for i := 0; i < 100000; i++ {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		var value float64
		if i&1 == 0 {
			scaled := int64(state%1999999999999999) - 999999999999999
			value = float64(scaled) / 1e6
		} else {
			value = math.Float64frombits(state)
		}
		if math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		input := wrapper{F: value}
		got, gotErr := encoder.AppendJSON(nil, &input)
		want, wantErr := json.Marshal(&input)
		if (gotErr == nil) != (wantErr == nil) || !bytes.Equal(got, want) {
			t.Fatalf("float %.17g (%#x): slopjson=%s err=%v, stdlib=%s err=%v", value, math.Float64bits(value), got, gotErr, want, wantErr)
		}
	}
}

func BenchmarkAppendCompactUint(b *testing.B) {
	for _, value := range []uint64{7, 42, 9999, 12345678, 123456789, 1234567890, 1234567890123, math.MaxUint64} {
		b.Run(fmt.Sprintf("%d/compact", value), func(b *testing.B) {
			var dst []byte
			for range b.N {
				dst = appendCompactUint(dst[:0], value)
			}
		})
		b.Run(fmt.Sprintf("%d/strconv", value), func(b *testing.B) {
			var dst []byte
			for range b.N {
				dst = strconv.AppendUint(dst[:0], value, 10)
			}
		})
	}
}

func TestAppendCompactUintMatchesStrconv(t *testing.T) {
	values := []uint64{0, 1, 9, 10, 99, 100, math.MaxUint64}
	for _, power := range pow10Uint64 {
		if power > 0 {
			values = append(values, power-1)
		}
		values = append(values, power)
		if power < math.MaxUint64 {
			values = append(values, power+1)
		}
	}
	state := uint64(0x9e3779b97f4a7c15)
	for range 100000 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		values = append(values, state)
	}
	for _, value := range values {
		want := strconv.AppendUint(nil, value, 10)
		for _, capacity := range []int{0, 2, 10, 20} {
			got := appendCompactUint(make([]byte, 0, capacity), value)
			if !bytes.Equal(got, want) {
				t.Fatalf("appendCompactUint(%d, cap=%d) = %q, want %q", value, capacity, got, want)
			}
		}
	}
}

func TestStoreCompactDigitPairBounds(t *testing.T) {
	for pair := range uint64(100) {
		buf := [4]byte{0xa5, 0, 0, 0x5a}
		storeCompactDigitPair((*[2]byte)(buf[1:3]), pair)
		want := encodeDigitPairs[pair*2 : pair*2+2]
		if got := string(buf[1:3]); got != want {
			t.Fatalf("pair %d = %q, want %q", pair, got, want)
		}
		if buf[0] != 0xa5 || buf[3] != 0x5a {
			t.Fatalf("pair %d overwrote sentinels: %#v", pair, buf)
		}
	}
}

func TestEncoderErrors(t *testing.T) {
	type inner struct {
		F float64 `json:"f"`
	}
	type outer struct {
		Items []inner `json:"items"`
	}
	badFloat := outer{Items: []inner{{F: 1}, {F: math.NaN()}}}
	_, err := Marshal(&badFloat)
	var encodeErr *EncodeError
	if !errors.As(err, &encodeErr) {
		t.Fatalf("NaN error = %v, want *EncodeError", err)
	}
	if encodeErr.Path != "items[1].f" {
		t.Fatalf("NaN path = %q, want items[1].f", encodeErr.Path)
	}

	type badNumber struct {
		N json.Number `json:"n"`
	}
	if _, err := Marshal(&badNumber{N: json.Number("1e")}); err == nil {
		t.Fatal("invalid json.Number accepted")
	}
	got, err := Marshal(&badNumber{})
	if err != nil || string(got) != `{"n":0}` {
		t.Fatalf("empty json.Number = %s, %v; want {\"n\":0}", got, err)
	}

	type unsupported struct {
		C chan int `json:"c"`
	}
	if _, err := Marshal(&unsupported{}); err == nil {
		t.Fatal("chan field accepted")
	}
}

func TestEncoderAppendJSONReusesBufferAllocs(t *testing.T) {
	encoder := mustCompileTestEncoder[typedTestRecord](t, EncoderOptions{})
	value := typedTestRecord{ID: 9, OK: true, Name: "reuse", Scores: [3]float64{1, 2, 3}, Number: json.Number("4")}
	buffer := make([]byte, 0, 256)
	allocs := testing.AllocsPerRun(1000, func() {
		out, err := encoder.AppendJSON(buffer[:0], &value)
		if err != nil {
			panic(err)
		}
		buffer = out[:0]
	})
	if allocs != 0 {
		t.Fatalf("AppendJSON allocations = %v, want 0", allocs)
	}
}

func TestEncodeDecodeRoundTrip(t *testing.T) {
	decoder := mustCompileTestDecoder[typedTestDocument](t, DecoderOptions{})
	original := typedTestDocument{
		Items: []typedTestRecord{
			{ID: 1, OK: true, Name: "first   record", Scores: [3]float64{1.5, -2, 3e19}, Number: json.Number("42")},
			{ID: -2, Name: strings.Repeat("wide ascii payload ", 8), Number: json.Number("0")},
		},
		Count: 2,
		Next:  &typedTestRecord{ID: 3, Number: json.Number("-7.25")},
	}
	encoded, err := Marshal(&original)
	requireNoTestError(t, err)
	var decoded typedTestDocument
	if err := decoder.Decode(encoded, &decoded); err != nil {
		t.Fatalf("round trip decode of %s: %v", encoded, err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip mismatch:\noriginal %#v\ndecoded  %#v", original, decoded)
	}
}

func FuzzEncoderMatchesStdlib(f *testing.F) {
	f.Add([]byte(`{"id":1,"ok":true,"name":"x","scores":[1,2.5,-3e4],"number":9}`))
	f.Add([]byte(`{"name":" 😀� <&> \t"}`))
	f.Add([]byte(`{"items":[{"id":1}],"count":2,"next":{"id":3}}`))
	// Former inline-round-trip and transform campaign seeds. Their independent
	// domains are preserved by helpers that run before typed encoder parity.
	for _, seed := range []string{
		`{}`,
		`{"id":1,"name":"x"}`,
		`{"a":true,"b":[1,2],"c":"hi"}`,
		`{"zebra":1,"alpha":2,"mango":3}`,
		`{"nested":{"deep":[{"k":"v"}]},"n":-0.5}`,
		`{"unié":true,"esc\"key":"v"}`,
		`{"a":1,"a":2}`,
		`{"big":123456789012345678,"f":1e30,"z":0.0}`,
		`null`,
		` { "b" : [2,1], "a" : "x\\ny" } `,
		`{"dup":2,"dup":1}`,
		`"\uD834\uDD1E"`,
	} {
		f.Add([]byte(seed))
	}
	decoder := mustCompileTestDecoder[typedTestDocument](f, DecoderOptions{})
	inlineDecoder := mustCompileTestDecoder[inlineOnly](f, DecoderOptions{InlineFields: true})
	inlineEncoder := mustCompileTestEncoder[inlineOnly](f, EncoderOptions{InlineFields: true})
	f.Fuzz(func(t *testing.T, src []byte) {
		checkInlineRoundTrip(t, src, inlineDecoder, inlineEncoder)
		checkTransforms(t, src)
		if len(src) > 1<<14 || !Valid(src) {
			return
		}
		var value typedTestDocument
		if err := decoder.Decode(src, &value); err != nil {
			return
		}
		got, gotErr := Marshal(&value)
		want, wantErr := stdlibCompactJSON(t, &value)
		if (gotErr == nil) != (wantErr == nil) {
			t.Fatalf("acceptance differs: slopjson=%v stdlib=%v", gotErr, wantErr)
		}
		if gotErr == nil && !bytes.Equal(got, want) {
			t.Fatalf("encoding differs:\nslopjson %s\nstdlib   %s", got, want)
		}
	})
}

// TestEncoderRandomFloatsMatchStdlib hammers the float fast paths with a
// deterministic mix of exact decimals, integers, and raw bit patterns.
func TestEncoderRandomFloatsMatchStdlib(t *testing.T) {
	type wrapper struct {
		F64 float64 `json:"f64"`
		F32 float32 `json:"f32"`
	}
	state := uint64(0x9E3779B97F4A7C15)
	next := func() uint64 {
		state ^= state << 13
		state ^= state >> 7
		state ^= state << 17
		return state
	}
	for i := 0; i < 200000; i++ {
		var f float64
		switch i % 4 {
		case 0: // small exact decimals
			f = float64(int64(next()%2_000_000)-1_000_000) / 100
		case 1: // integers across the fast-path boundary
			f = float64(int64(next()%(1<<51)) - 1<<50)
		case 2: // arbitrary bit patterns (skip NaN/Inf)
			f = math.Float64frombits(next())
			if math.IsNaN(f) || math.IsInf(f, 0) {
				continue
			}
		default: // tenths near the scaled boundary
			f = float64(int64(next()%20_000_000_000)-10_000_000_000) / 10
		}
		value := wrapper{F64: f, F32: float32(f)}
		if math.IsInf(float64(value.F32), 0) {
			continue
		}
		want, err := stdlibCompactJSON(t, &value)
		if err != nil {
			continue
		}
		got, err := Marshal(&value)
		if err != nil {
			t.Fatalf("float %g (bits %#x): %v", f, math.Float64bits(f), err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("float %g (bits %#x):\nslopjson %s\nstdlib   %s", f, math.Float64bits(f), got, want)
		}
	}
}

type mapKey string

type mapDocument struct {
	Plain    map[string]int             `json:"plain"`
	Named    map[mapKey]string          `json:"named"`
	Nested   map[string]map[string]int  `json:"nested"`
	Structs  map[string]typedTestRecord `json:"structs"`
	Slices   map[string][]int           `json:"slices"`
	Optional map[string]int             `json:"optional,omitempty"`
}

func TestMapsMatchStdlib(t *testing.T) {
	sources := []string{
		`{"plain":{"b":2,"a":1},"named":{"x":"y"},"nested":{"outer":{"inner":3}},"structs":{"r":{"id":1,"ok":true,"name":"n","scores":[1,2,3],"number":4}},"slices":{"s":[1,2,3]}}`,
		`{"plain":{},"named":null,"nested":{"empty":{}},"structs":{},"slices":{"empty":[]}}`,
		`{"plain":{"esc\"aped":1,"uni ✓":2}}`,
		`{"optional":{}}`,
		`{}`,
	}
	decoder := mustCompileTestDecoder[mapDocument](t, DecoderOptions{})
	for _, src := range sources {
		var got, want mapDocument
		if !assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want) {
			t.Fatalf("valid map fixture rejected: %s", src)
		}
		assertEncodesLikeStdlib(t, &got)
	}
}

// textMapKey is a comparable key rendered through encoding.TextMarshaler, so it
// exercises encodeMap's text-key branch through the reused key box.
type textMapKey int

func (k textMapKey) MarshalText() ([]byte, error) {
	return []byte("k" + strconv.Itoa(int(k))), nil
}

// TestMapKeyKindsMatchStdlib pins the signed, unsigned, and TextMarshaler key
// paths byte for byte against encoding/json. Each renders its name from the
// reused key box that replaced per-entry reflect key boxing.
func TestMapKeyKindsMatchStdlib(t *testing.T) {
	type doc struct {
		Signed   map[int]string     `json:"signed"`
		Unsigned map[uint16]int     `json:"unsigned"`
		Text     map[textMapKey]int `json:"text"`
	}
	v := doc{
		Signed:   map[int]string{3: "c", 1: "a", -2: "neg", 2: "b"},
		Unsigned: map[uint16]int{10: 1, 2: 2, 40000: 3},
		Text:     map[textMapKey]int{5: 1, 1: 2, 3: 3},
	}
	assertEncodesLikeStdlib(t, &v)
}

// TestMapEncodeAllocationFree guards the zero-allocation property of the
// SetIterKey/SetIterValue rewrite: a reused encoder emits a populated map
// without allocating once its pooled scratch has warmed.
func TestMapEncodeAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector instruments allocation and disables pool reuse")
	}
	type doc struct {
		M map[string]int `json:"m"`
	}
	enc := mustCompileTestEncoder[doc](t, EncoderOptions{})
	var err error
	v := doc{M: map[string]int{"a": 1, "b": 2, "c": 3, "d": 4}}
	buf := make([]byte, 0, 64)
	buf, err = enc.AppendJSON(buf[:0], &v) // warm the scratch
	requireNoTestError(t, err)
	allocs := testing.AllocsPerRun(100, func() {
		buf, _ = enc.AppendJSON(buf[:0], &v)
	})
	if allocs != 0 {
		t.Fatalf("populated map encode allocated %.1f times per run, want 0", allocs)
	}
}

// TestInterfaceContainersEncodeAllocationFree guards the concrete-value boxes
// used for maps and slices reached through any. The boxes and concrete map
// scratch are warmed once, then every later encode must reuse them.
func TestInterfaceContainersEncodeAllocationFree(t *testing.T) {
	if raceEnabled {
		t.Skip("the race detector instruments allocation and disables pool reuse")
	}
	type doc struct {
		Value any `json:"value"`
	}
	value := doc{Value: map[string]any{
		"items": []any{"one", float64(2), true, nil, map[string]any{"nested": "map"}},
		"name":  "example",
	}}
	enc := mustCompileTestEncoder[doc](t, EncoderOptions{})
	want, err := json.Marshal(&value)
	requireNoTestError(t, err)
	buf := make([]byte, 0, len(want))
	buf, err = enc.AppendJSON(buf[:0], &value)
	requireNoTestError(t, err)
	if !bytes.Equal(buf, want) {
		t.Fatalf("interface encode differs:\n got %s\nwant %s", buf, want)
	}
	allocs := testing.AllocsPerRun(200, func() {
		buf, err = enc.AppendJSON(buf[:0], &value)
		if err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("interface container encode allocated %.1f times per run, want 0", allocs)
	}
	runtime.KeepAlive(buf)
}

func TestMapDecodeMergesLikeStdlib(t *testing.T) {
	decoder := mustCompileTestDecoder[map[string]int](t, DecoderOptions{})
	got := map[string]int{"keep": 1, "replace": 2}
	want := map[string]int{"keep": 1, "replace": 2}
	src := []byte(`{"replace":20,"new":30}`)
	requireNoTestError(t, decoder.Decode(src, &got))
	requireNoTestError(t, json.Unmarshal(src, &want))
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("merge mismatch: slopjson %#v, stdlib %#v", got, want)
	}

	// Owned map keys must survive input mutation.
	input := []byte(`{"retained":7}`)
	owned := map[string]int(nil)
	requireNoTestError(t, decoder.Decode(input, &owned))
	for i := range input {
		input[i] = 'x'
	}
	if _, ok := owned["retained"]; !ok {
		t.Fatalf("map key aliases mutated input: %#v", owned)
	}

	// Slice values must not share backing arrays across entries.
	sliceDecoder := mustCompileTestDecoder[map[string][]int](t, DecoderOptions{})
	shared := map[string][]int(nil)
	requireNoTestError(t, sliceDecoder.Decode([]byte(`{"a":[1,2,3],"b":[4,5,6]}`), &shared))
	if !reflect.DeepEqual(shared["a"], []int{1, 2, 3}) || !reflect.DeepEqual(shared["b"], []int{4, 5, 6}) {
		t.Fatalf("map slice values share storage: %#v", shared)
	}
}

func TestMapErrorsAndPaths(t *testing.T) {
	decoder := mustCompileTestDecoder[map[string]map[string]int](t, DecoderOptions{})
	var dst map[string]map[string]int
	decodeErr := decoder.Decode([]byte(`{"outer":{"inner":"nope"}}`), &dst)
	var typed *DecodeError
	if !errors.As(decodeErr, &typed) {
		t.Fatalf("error = %v, want *DecodeError", decodeErr)
	}
	if typed.Path != "outer.inner" {
		t.Fatalf("path = %q, want outer.inner", typed.Path)
	}

	if _, err := CompileDecoder[map[float64]string](DecoderOptions{}); err == nil {
		t.Fatal("float map keys accepted")
	}

	type withNaN struct {
		M map[string]float64 `json:"m"`
	}
	_, encodeErr := Marshal(&withNaN{M: map[string]float64{"bad": math.NaN()}})
	var enc *EncodeError
	if !errors.As(encodeErr, &enc) {
		t.Fatalf("encode error = %v, want *EncodeError", encodeErr)
	}
	if enc.Path != "m.bad" {
		t.Fatalf("encode path = %q, want m.bad", enc.Path)
	}
}

type anyDocument struct {
	Meta   any            `json:"meta"`
	Blob   map[string]any `json:"blob"`
	Items  []any          `json:"items"`
	Option any            `json:"option,omitempty"`
}

func TestAnyFieldsMatchStdlib(t *testing.T) {
	sources := []string{
		`{"meta":{"a":[1,2.5,{"deep":true}],"b":null},"blob":{"s":"x","n":-3e2},"items":[1,"two",false,null,{"k":"v"}]}`,
		`{"meta":null,"blob":{},"items":[]}`,
		`{"meta":"just a string","blob":{"nested":{"more":{"even":[{}]}}},"items":[[[1]]]}`,
		`{"meta":1e15,"blob":{"big":123456789012345678901234567890}}`,
		`{"option":null}`,
	}
	decoder := mustCompileTestDecoder[anyDocument](t, DecoderOptions{})
	for _, src := range sources {
		var got, want anyDocument
		if !assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want) {
			t.Fatalf("valid any fixture rejected: %s", src)
		}
		assertEncodesLikeStdlib(t, &got)
	}
}

func TestAnyEncodeConcreteTypes(t *testing.T) {
	type custom struct {
		N int `json:"n"`
	}
	values := []anyDocument{
		{Meta: int(7), Blob: map[string]any{"i32": int32(-5), "u": uint16(9)}, Items: []any{int8(1), float32(2.5)}},
		{Meta: custom{N: 3}, Items: []any{&custom{N: 4}, map[string]int{"z": 1, "a": 2}}},
		{Meta: []string{"x", "y"}, Blob: map[string]any{"deep": []any{map[string]any{"k": json.Number("5.5")}}}},
		{Meta: [2]int{1, 2}},
	}
	for _, value := range values {
		assertEncodesLikeStdlib(t, &value)
	}

	unsupported := anyDocument{Meta: make(chan int)}
	if _, err := Marshal(&unsupported); err == nil {
		t.Fatal("chan inside any accepted")
	}
}

type bytesDocument struct {
	Data   []byte            `json:"data"`
	Named  namedBlob         `json:"named"`
	Map    map[string][]byte `json:"map"`
	Option []byte            `json:"option,omitempty"`
}

type namedBlob []byte

func TestByteSlicesMatchStdlib(t *testing.T) {
	sources := []string{
		`{"data":"aGVsbG8gd29ybGQ=","named":"AQID","map":{"k":"eA=="}}`,
		`{"data":"","named":null}`,
		`{"data":"aGk="}`,
		`{"data":"!!!invalid!!!"}`,
		`{"data":123}`,
	}
	decoder := mustCompileTestDecoder[bytesDocument](t, DecoderOptions{})
	for _, src := range sources {
		var got, want bytesDocument
		if !assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want) {
			continue
		}
		assertEncodesLikeStdlib(t, &got)
	}

	// Byte-slice capacity is reused across decodes.
	reuse := bytesDocument{Data: make([]byte, 0, 64)}
	base := &reuse.Data[:1][0]
	requireNoTestError(t, decoder.Decode([]byte(`{"data":"aGVsbG8="}`), &reuse))
	if string(reuse.Data) != "hello" {
		t.Fatalf("decoded bytes = %q", reuse.Data)
	}
	if &reuse.Data[0] != base {
		t.Fatal("byte slice capacity was not reused")
	}
}

type quotedDocument struct {
	I   int         `json:"i,string"`
	I8  int8        `json:"i8,string"`
	U   uint32      `json:"u,string"`
	F   float64     `json:"f,string"`
	B   bool        `json:"b,string"`
	S   string      `json:"s,string"`
	N   json.Number `json:"n,string"`
	Ptr *int        `json:"ptr,string"` // stdlib ignores the option here
}

func TestStringTagOptionMatchesStdlib(t *testing.T) {
	one := 1
	// Encode side.
	values := []quotedDocument{
		{I: -42, I8: 7, U: 9, F: 2.5, B: true, S: `quo"ted <&>`, N: json.Number("5.5"), Ptr: &one},
		{},
		{F: 1e21, S: ""},
	}
	for _, value := range values {
		assertEncodesLikeStdlib(t, &value)
	}

	// Decode side, including malformed corners.
	decoder := mustCompileTestDecoder[quotedDocument](t, DecoderOptions{})
	sources := []string{
		`{"i":"-42","i8":"7","u":"9","f":"2.5","b":"true","s":"\"hi\"","n":"5.5"}`,
		`{"i":null,"s":null}`,
		`{"i":42}`,
		`{"i":"nope"}`,
		`{"i":"42 "}`,
		`{"i8":"300"}`,
		`{"b":"maybe"}`,
		`{"s":"unquoted"}`,
		`{"f":"1e999"}`,
		`{"n":"not-a-number"}`,
		`{"i":""}`,
		`{"ptr":"1"}`,
		`{"ptr":null}`,
		`{"ptr":1}`,
	}
	for _, src := range sources {
		var got, want quotedDocument
		assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want)
	}

	// Round trip.
	original := quotedDocument{I: 3, F: -0.25, B: true, S: "wrap", N: json.Number("8")}
	encoded, err := Marshal(&original)
	requireNoTestError(t, err)
	var decoded quotedDocument
	if err := decoder.Decode(encoded, &decoded); err != nil {
		t.Fatalf("round trip decode of %s: %v", encoded, err)
	}
	if !reflect.DeepEqual(decoded, original) {
		t.Fatalf("round trip mismatch: %#v vs %#v", decoded, original)
	}
}

func TestDisableHTMLEscaping(t *testing.T) {
	type doc struct {
		S string `json:"s"`
	}
	value := doc{S: `<a href="x">&  </a>`}

	var buffer bytes.Buffer
	stdEncoder := json.NewEncoder(&buffer)
	stdEncoder.SetEscapeHTML(false)
	requireNoTestError(t, stdEncoder.Encode(&value))
	want := bytes.TrimSuffix(buffer.Bytes(), []byte("\n"))

	encoder := mustCompileTestEncoder[doc](t, EncoderOptions{DisableHTMLEscaping: true})
	got, err := encoder.AppendJSON(nil, &value)
	requireNoTestError(t, err)
	if !bytes.Equal(got, want) {
		t.Fatalf("no-escape mode:\nslopjson %s\nstdlib   %s", got, want)
	}
}

type embBase struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type embShadow struct {
	embBase
	Name string `json:"name"` // shadows the embedded name
}

type embPointer struct {
	*embBase
	Extra string `json:"extra"`
}

type embTagged struct {
	embBase `json:"base"` // tagged: nested object, not flattened
}

type embConflictA struct{ Same int }
type embConflictB struct{ Same int }
type embConflict struct {
	embConflictA
	embConflictB     // same depth, same name: both dropped
	Z            int `json:"z"`
}

type embInt int

type embNonStruct struct {
	embInt     // named by its type
	V      int `json:"v"`
}

type embUnexported struct {
	hidden
	Top int `json:"top"`
}

type hidden struct {
	Inner string `json:"inner"`
}

type embDeep struct {
	embMid
	Own int `json:"own"`
}

type embMid struct {
	embBase
	Mid int `json:"mid"`
}

func TestEmbeddedFieldsMatchStdlib(t *testing.T) {
	// Each embedding rule needs its own destination type, so a row is a named
	// subtest closure that binds the type to its source. assertRoundTripsLikeStdlib
	// runs the shared decode-then-re-encode comparison for that type.
	cases := []struct {
		name string
		run  func(*testing.T)
	}{
		{"value embedding", func(t *testing.T) { assertRoundTripsLikeStdlib[embMid](t, `{"id":1,"name":"n","mid":2}`) }},
		{"shadowing", func(t *testing.T) { assertRoundTripsLikeStdlib[embShadow](t, `{"id":3,"name":"outer"}`) }},
		{"pointer embedding", func(t *testing.T) { assertRoundTripsLikeStdlib[embPointer](t, `{"id":4,"name":"p","extra":"e"}`) }},
		{"pointer embedding absent", func(t *testing.T) { assertRoundTripsLikeStdlib[embPointer](t, `{"extra":"only"}`) }},
		{"tagged anonymous", func(t *testing.T) { assertRoundTripsLikeStdlib[embTagged](t, `{"base":{"id":5,"name":"tag"}}`) }},
		{"same depth conflict", func(t *testing.T) { assertRoundTripsLikeStdlib[embConflict](t, `{"Same":9,"z":1}`) }},
		{"embedded scalar", func(t *testing.T) { assertRoundTripsLikeStdlib[embNonStruct](t, `{"embInt":7,"v":8}`) }},
		{"unexported embedded struct", func(t *testing.T) { assertRoundTripsLikeStdlib[embUnexported](t, `{"inner":"i","top":9}`) }},
		{"deep nesting", func(t *testing.T) { assertRoundTripsLikeStdlib[embDeep](t, `{"id":1,"name":"d","mid":2,"own":3}`) }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, testCase.run)
	}
}

// assertRoundTripsLikeStdlib decodes src into T with both libraries, requires
// the same acceptance decision and decoded value, and — when both accept —
// re-encodes and requires the same bytes. It proves a destination type both
// decodes and re-encodes identically to encoding/json.
func assertRoundTripsLikeStdlib[T any](t *testing.T, src string) {
	t.Helper()
	var got, want T
	gotErr := Unmarshal([]byte(src), &got)
	wantErr := json.Unmarshal([]byte(src), &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: decode acceptance differs: slopjson=%v stdlib=%v", src, gotErr, wantErr)
	}
	if gotErr != nil {
		return
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s: decoded differs:\nslopjson %#v\nstdlib   %#v", src, got, want)
	}
	assertEncodesLikeStdlib(t, &got)
}

func TestEmbeddedPointerEncodeNil(t *testing.T) {
	value := embPointer{Extra: "only"}
	assertEncodesLikeStdlib(t, &value)
}

type textKey struct{ A, B int }

func (k textKey) MarshalText() ([]byte, error) { return fmt.Appendf(nil, "%d-%d", k.A, k.B), nil }
func (k *textKey) UnmarshalText(text []byte) error {
	_, err := fmt.Sscanf(string(text), "%d-%d", &k.A, &k.B)
	return err
}

type stringTextKey string

func (k stringTextKey) MarshalText() ([]byte, error) { return []byte("SHOULD-NOT-BE-USED"), nil }
func (k *stringTextKey) UnmarshalText(text []byte) error {
	*k = stringTextKey("text:" + string(text))
	return nil
}

type mapKeyDocument struct {
	Ints  map[int]string        `json:"ints"`
	Uints map[uint8]int         `json:"uints"`
	Texts map[textKey]int       `json:"texts"`
	Asym  map[stringTextKey]int `json:"asym"`
	Named map[int32]bool        `json:"named"`
}

func TestNonStringMapKeysMatchStdlib(t *testing.T) {
	// Encode: string kinds beat TextMarshaler; ints format base 10; text keys
	// marshal; everything sorts by rendered name.
	value := mapKeyDocument{
		Ints:  map[int]string{-3: "a", 10: "b", 2: "c"},
		Uints: map[uint8]int{255: 1, 0: 2},
		Texts: map[textKey]int{{A: 1, B: 2}: 3, {A: 0, B: 0}: 4},
		Asym:  map[stringTextKey]int{"raw": 5},
		Named: map[int32]bool{-9: true},
	}
	assertEncodesLikeStdlib(t, &value)

	decoder := mustCompileTestDecoder[mapKeyDocument](t, DecoderOptions{})
	sources := []string{
		`{"ints":{"-3":"a","10":"b"},"uints":{"255":1},"texts":{"7-8":9},"asym":{"k":1},"named":{"-9":true}}`,
		`{"ints":{"not-a-number":"x"}}`,
		`{"uints":{"-1":1}}`,
		`{"uints":{"256":1}}`,
		`{"texts":{"badkey":1}}`,
		`{"ints":{"1.5":"x"}}`,
	}
	for _, src := range sources {
		var got, want mapKeyDocument
		assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want)
	}
}

type speaker interface{ Speak() string }

type dog struct {
	Sound string `json:"sound"`
}

func (d *dog) Speak() string { return d.Sound }

type ifaceDocument struct {
	Animal speaker `json:"animal"`
	Blob   any     `json:"blob"`
	Option speaker `json:"option,omitempty"`
}

func TestNonEmptyInterfacesMatchStdlib(t *testing.T) {
	// Encode: concrete dynamic dispatch, nil as null, omitempty.
	values := []ifaceDocument{
		{Animal: &dog{Sound: "woof"}, Blob: map[string]any{"k": 1.5}},
		{},
	}
	for _, value := range values {
		assertEncodesLikeStdlib(t, &value)
	}

	// Decode: null clears; a held non-nil pointer is decoded into; anything
	// else fails, all matching encoding/json.
	decoder := mustCompileTestDecoder[ifaceDocument](t, DecoderOptions{})
	sources := []string{
		`{"animal":null}`,
		`{"animal":{"sound":"nope"}}`,
	}
	for _, src := range sources {
		got := ifaceDocument{Animal: &dog{Sound: "old"}}
		want := ifaceDocument{Animal: &dog{Sound: "old"}}
		assertCompiledDecodesLikeStdlib(t, decoder, []byte(src), &got, &want)
	}

	// Empty interface holding a pointer is decoded into, keeping identity.
	target := &dog{Sound: "before"}
	holder := struct {
		Blob any `json:"blob"`
	}{Blob: target}
	requireNoTestError(t, Unmarshal([]byte(`{"blob":{"sound":"after"}}`), &holder))
	if target.Sound != "after" {
		t.Fatalf("pointer held by interface not decoded into: %#v", target)
	}
	if holder.Blob != any(target) {
		t.Fatalf("interface identity lost: %#v", holder.Blob)
	}

	// Fresh interface without a pointer errors like stdlib.
	var fresh ifaceDocument
	gotErr := decoder.Decode([]byte(`{"animal":{"sound":"x"}}`), &fresh)
	var want ifaceDocument
	wantErr := json.Unmarshal([]byte(`{"animal":{"sound":"x"}}`), &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("fresh non-empty interface: slopjson=%v stdlib=%v", gotErr, wantErr)
	}
}
