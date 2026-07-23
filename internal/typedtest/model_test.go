package typedtest

import (
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/thesyncim/slopjson"
)

func mustCompile[T any](t *testing.T, opts slopjson.DecoderOptions) slopjson.Decoder[T] {
	t.Helper()
	decoder, err := slopjson.CompileDecoder[T](opts)
	if err != nil {
		t.Fatal(err)
	}
	return decoder
}

func TestCompiledDecoderMatchesStdlib(t *testing.T) {
	sources := [][]byte{
		[]byte(`{"items":[{"id":1,"active":true,"name":"one","message":"plain","scores":[1,2.5,-3e4],"number":1234567890123456}],"meta":{"count":1,"source":"test"},"optional":{"count":2,"source":"pointer"},"fixed":[3,4],"unknown":[1,{"x":2}]}`),
		[]byte(`{"ITEMS":[],"META":{"COUNT":0,"SOURCE":"folded"},"optional":null,"fixed":[1,2,3]}`),
		[]byte(`null`),
	}
	decoder := mustCompile[Document](t, slopjson.DecoderOptions{ZeroCopy: true})
	for _, src := range sources {
		var want Document
		stdDecoder := json.NewDecoder(strings.NewReader(string(src)))
		stdDecoder.UseNumber()
		if err := stdDecoder.Decode(&want); err != nil {
			t.Fatal(err)
		}
		var got Document
		if err := decoder.Decode(src, &got); err != nil {
			t.Fatalf("compiled decode %s: %v", src, err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("compiled decode %s = %#v, want %#v", src, got, want)
		}
	}
}

func TestCompiledDecoderReuseAndOptions(t *testing.T) {
	src := []byte(`{"items":[{"id":7,"active":true,"name":"aliased","message":"m","scores":[1,2,3],"number":7}],"meta":{"count":1,"source":"source"},"optional":null,"fixed":[8,9]}`)
	decoder := mustCompile[Document](t, slopjson.DecoderOptions{ZeroCopy: true})
	dst := Document{Items: make([]Record, 4, 16), Optional: &Meta{Count: 99}}
	base := &dst.Items[0]
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	if len(dst.Items) != 1 || &dst.Items[0] != base {
		t.Fatalf("destination storage was not reused: len=%d cap=%d", len(dst.Items), cap(dst.Items))
	}
	src[strings.Index(string(src), "aliased")] = 'A'
	if dst.Items[0].Name != "Aliased" {
		t.Fatalf("zero-copy name = %q", dst.Items[0].Name)
	}

	strict := mustCompile[Document](t, slopjson.DecoderOptions{DisallowUnknownFields: true})
	if err := strict.Decode([]byte(`{"unknown":1}`), &dst); err == nil {
		t.Fatal("strict compiled decoder accepted unknown field")
	}
	caseSensitive := mustCompile[Document](t, slopjson.DecoderOptions{CaseSensitive: true, Replace: true})
	if err := caseSensitive.Decode([]byte(`{"ITEMS":[]}`), &dst); err != nil {
		t.Fatal(err)
	}
	if dst.Items != nil && len(dst.Items) != 0 {
		t.Fatalf("case-sensitive unknown field changed items: %#v", dst.Items)
	}
}

func TestCompiledDecoderOwnedStringsDoNotAliasInput(t *testing.T) {
	src := []byte(`{"items":[{"name":"owned","message":"plain","number":123}],"meta":{"source":"origin"}}`)
	decoder := mustCompile[Document](t, slopjson.DecoderOptions{CaseSensitive: true})
	var dst Document
	if err := decoder.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	for i := range src {
		if src[i] != '{' && src[i] != '}' && src[i] != '[' && src[i] != ']' {
			src[i] = 'x'
		}
	}
	if dst.Items[0].Name != "owned" || dst.Items[0].Message != "plain" || dst.Items[0].Number != "123" || dst.Meta.Source != "origin" {
		t.Fatalf("owned result changed after source mutation: %#v", dst)
	}
}

func TestCompiledDecoderSlowPathsMatchStdlib(t *testing.T) {
	src := []byte(" \n { \"it\\u0065ms\" : [ { \"id\" : 1, \"active\" : true, \"name\" : \"caf\xc3\xa9\", \"message\" : \"line\\n\\u263a\", \"scores\" : [ 1, 2.5, -3e4 ], \"number\" : 7 } ], \"meta\" : { \"count\" : 1, \"source\" : \"slow\" }, \"optional\" : null, \"fixed\" : [1, 2], \"unknown-long-key\" : { \"nested\" : [true, null, {\"x\":1}] } } \t")
	var want Document
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	var got Document
	decoder := mustCompile[Document](t, slopjson.DecoderOptions{ZeroCopy: true})
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("slow-path decode = %#v, want %#v", got, want)
	}
}

func TestCompiledDecoderAllocationContracts(t *testing.T) {
	src := []byte(`{"items":[{"id":7,"active":true,"name":"name","message":"message","scores":[1,2.5,-3e4],"number":7}],"meta":{"count":1,"source":"source"},"fixed":[1,2]}`)
	dst := Document{Items: make([]Record, 0, 4)}
	zeroCopy := mustCompile[Document](t, slopjson.DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err := zeroCopy.Decode(src, &dst); err != nil {
		t.Fatal(err)
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := zeroCopy.Decode(src, &dst); err != nil {
			panic(err)
		}
	}); allocs != 0 {
		t.Fatalf("zero-copy reused allocs = %v, want 0", allocs)
	}

	owned := mustCompile[Document](t, slopjson.DecoderOptions{CaseSensitive: true})
	if allocs := testing.AllocsPerRun(1000, func() {
		if err := owned.Decode(src, &dst); err != nil {
			panic(err)
		}
	}); allocs != 1 {
		t.Fatalf("owned reused allocs = %v, want 1", allocs)
	}
}

func TestCompiledDecoderDecodeArray(t *testing.T) {
	src := []byte(`[{"items":[],"meta":{"count":1,"source":"a"},"optional":null,"fixed":[1,2]},{"items":[],"meta":{"count":2,"source":"b"},"optional":null,"fixed":[3,4]}]`)
	decoder := mustCompile[Document](t, slopjson.DecoderOptions{ZeroCopy: true})
	dst := make([]Document, 0, 4)
	got, err := decoder.DecodeArray(src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Meta.Count != 2 || got[1].Fixed != [2]int{3, 4} {
		t.Fatalf("DecodeArray = %#v", got)
	}
}

func TestCompiledNumericBoundariesMatchStdlib(t *testing.T) {
	intMin := "-9223372036854775808"
	uintMax := "18446744073709551615"
	if strconv.IntSize == 32 {
		intMin = "-2147483648"
		uintMax = "4294967295"
	}
	src := []byte(fmt.Sprintf(`{"i8":-128,"i16":-32768,"i32":-2147483648,"i64":-9223372036854775808,"int":%s,"u8":255,"u16":65535,"u32":4294967295,"u64":18446744073709551615,"uint":%s,"f32":3.4028235e38,"f64":1.7976931348623157e308,"bool":true,"text":"boundary","number":12345678901234567890}`, intMin, uintMax))

	var want Numeric
	if err := json.Unmarshal(src, &want); err != nil {
		t.Fatal(err)
	}
	var got Numeric
	decoder := mustCompile[Numeric](t, slopjson.DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err := decoder.Decode(src, &got); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("numeric boundaries = %#v, want %#v", got, want)
	}
}

func TestCompiledNumericShortFloatsAndNegativeZero(t *testing.T) {
	tests := []string{
		`{"f32":2.5,"f64":-3e4}`,
		`{"f32":1e-9,"f64":3e+4}`,
		`{"f32":-0,"f64":-0.0}`,
	}
	decoder := mustCompile[Numeric](t, slopjson.DecoderOptions{CaseSensitive: true})
	for _, source := range tests {
		var want, got Numeric
		if err := json.Unmarshal([]byte(source), &want); err != nil {
			t.Fatal(err)
		}
		if err := decoder.Decode([]byte(source), &got); err != nil {
			t.Fatalf("Decode(%s): %v", source, err)
		}
		if got.F32 != want.F32 || got.F64 != want.F64 || math.Signbit(float64(got.F32)) != math.Signbit(float64(want.F32)) || math.Signbit(got.F64) != math.Signbit(want.F64) {
			t.Fatalf("Decode(%s) floats = (%v, %v), want (%v, %v)", source, got.F32, got.F64, want.F32, want.F64)
		}
	}
}

func TestCompiledNumericRejectsInvalidValues(t *testing.T) {
	tests := []struct {
		name string
		src  []byte
	}{
		{name: "int8 positive overflow", src: []byte(`{"i8":128}`)},
		{name: "int8 negative overflow", src: []byte(`{"i8":-129}`)},
		{name: "int64 positive overflow", src: []byte(`{"i64":9223372036854775808}`)},
		{name: "int64 negative overflow", src: []byte(`{"i64":-9223372036854775809}`)},
		{name: "uint8 overflow", src: []byte(`{"u8":256}`)},
		{name: "uint negative", src: []byte(`{"u64":-1}`)},
		{name: "uint64 overflow", src: []byte(`{"u64":18446744073709551616}`)},
		{name: "fractional int", src: []byte(`{"i32":1.5}`)},
		{name: "exponent int", src: []byte(`{"i32":1e2}`)},
		{name: "leading zero", src: []byte(`{"i32":01}`)},
		{name: "float32 overflow", src: []byte(`{"f32":3.5e38}`)},
		{name: "float64 overflow", src: []byte(`{"f64":1e309}`)},
		{name: "truncated bool", src: []byte(`{"bool":tru}`)},
		{name: "missing bool", src: []byte(`{"bool":`)},
		{name: "wrong string type", src: []byte(`{"text":1}`)},
		{name: "invalid UTF-8", src: []byte{'{', '"', 't', 'e', 'x', 't', '"', ':', '"', 0xff, '"', '}'}},
	}
	decoder := mustCompile[Numeric](t, slopjson.DecoderOptions{CaseSensitive: true})
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dst Numeric
			if err := decoder.Decode(tt.src, &dst); err == nil {
				t.Fatalf("Decode(%q) succeeded: %#v", tt.src, dst)
			}
		})
	}
}
