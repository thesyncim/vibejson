package simdjson

import (
	"encoding/json"
	"errors"
	"reflect"
	"testing"
)

// The fused homogeneous 64-bit scalar-slice decoders (decodeCompiledInt64Slice,
// decodeCompiledUint64Slice, decodeCompiledFloat64Slice) replace the generic
// element loop with an inline delimiter step. These tests pin that path to
// encoding/json across the delimiter, null, whitespace, overflow, and malformed
// edges the inline step must hand back to the general scanner.

// decodeMatchesStdlib decodes src into a fresh T through both this library and
// encoding/json and reports whether acceptance and value agree.
func decodeMatchesStdlib[T any](t *testing.T, src []byte) {
	t.Helper()
	decoder, err := CompileDecoder[T](DecoderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	var got, want T
	gotErr := decoder.Decode(src, &got)
	wantErr := json.Unmarshal(src, &want)
	if !decodeAcceptanceMatches(gotErr, wantErr) {
		t.Fatalf("%T acceptance differs on %.80q: simdjson=%v stdlib=%v", got, src, gotErr, wantErr)
	}
	if gotErr == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("%T value differs on %.80q: simdjson=%#v stdlib=%#v", got, src, got, want)
	}
}

func decodeAcceptanceMatches(gotErr, wantErr error) bool {
	return (gotErr == nil) == (wantErr == nil)
}

func TestScalarSliceAcceptanceClassifier(t *testing.T) {
	rejected := errors.New("rejected")
	for _, tc := range []struct {
		name            string
		gotErr, wantErr error
		want            bool
	}{
		{name: "both accept", want: true},
		{name: "both reject", gotErr: rejected, wantErr: rejected, want: true},
		{name: "simdjson rejects", gotErr: rejected},
		{name: "stdlib rejects", wantErr: rejected},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := decodeAcceptanceMatches(tc.gotErr, tc.wantErr); got != tc.want {
				t.Fatalf("decodeAcceptanceMatches() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestScalarSliceDecodeMatchesStdlib(t *testing.T) {
	cases := []string{
		`[]`, `[ ]`, ` [ ] `, `[1]`, `[1,2,3]`, `[ 1 , 2 , 3 ]`,
		"[1,\n2,\r\n3]", `[1,null,3]`, `[null]`, `[null,null]`,
		`[1,2,]`, `[1,,2]`, `[1 2]`, `[,1]`, `[1,]`, `[`, `]`, `[1`,
		`[9223372036854775807,-9223372036854775808]`,
		`[9223372036854775808]`, `[18446744073709551615]`, `[-1]`,
		`[1.5,2.5,-3e4]`, `[1e400]`, `[1e-400]`, `[0.0,-0.0]`,
		`[1,"x",3]`, `[true]`, `[[1]]`, `[{}]`, `null`, `[1,2,3,4,5,6,7,8,9,10]`,
		`[   ]`, `[1,   2]`, "[1,\t2]",
	}
	for _, s := range cases {
		src := []byte(s)
		t.Run(s, func(t *testing.T) {
			decodeMatchesStdlib[[]int64](t, src)
			decodeMatchesStdlib[[]uint64](t, src)
			decodeMatchesStdlib[[]float64](t, src)
			decodeMatchesStdlib[[]int](t, src)
			decodeMatchesStdlib[[]int32](t, src)
			decodeMatchesStdlib[[]float32](t, src)
			decodeMatchesStdlib[[]uint32](t, src)
		})
	}
}

// FuzzScalarSliceDecodeMatchesStdlib searches for any array spelling where the
// fused 64-bit slice decoders disagree with encoding/json on acceptance or
// value. Non-array inputs are exercised too, so the fast path's hand-back to the
// general scanner is covered on malformed material.
func FuzzScalarSliceDecodeMatchesStdlib(f *testing.F) {
	for _, s := range []string{
		`[1,2,3]`, `[1,null,3]`, `[]`, `[ 1 , 2 ]`, `[1,2,]`, `[1e10,2e-5]`,
		`[9223372036854775807]`, `[18446744073709551615]`, `[1.5,null]`,
	} {
		f.Add([]byte(s))
	}
	int64Dec, _ := CompileDecoder[[]int64](DecoderOptions{})
	uint64Dec, _ := CompileDecoder[[]uint64](DecoderOptions{})
	float64Dec, _ := CompileDecoder[[]float64](DecoderOptions{})
	f.Fuzz(func(t *testing.T, src []byte) {
		if len(src) > 1<<12 {
			return
		}
		compareSliceDecode(t, src, int64Dec, func() any { var v []int64; return &v })
		compareSliceDecode(t, src, uint64Dec, func() any { var v []uint64; return &v })
		compareSliceDecode(t, src, float64Dec, func() any { var v []float64; return &v })
	})
}

// compareSliceDecode decodes src through dec and encoding/json into fresh
// destinations of the same type and fails on any acceptance or value mismatch.
func compareSliceDecode[T any](t *testing.T, src []byte, dec Decoder[T], mkStd func() any) {
	t.Helper()
	var got T
	gotErr := dec.Decode(src, &got)
	want := mkStd()
	wantErr := json.Unmarshal(src, want)
	if !decodeAcceptanceMatches(gotErr, wantErr) {
		t.Fatalf("%T acceptance differs on %.80q: simdjson=%v stdlib=%v", got, src, gotErr, wantErr)
	}
	if gotErr != nil {
		return
	}
	wantVal := reflect.ValueOf(want).Elem().Interface()
	if !reflect.DeepEqual(got, wantVal) {
		t.Fatalf("%T value differs on %.80q: simdjson=%#v stdlib=%#v", got, src, got, wantVal)
	}
}
