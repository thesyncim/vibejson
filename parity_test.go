package simdjson

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func mustCompileTestDecoder[T any](tb testing.TB, opts DecoderOptions) Decoder[T] {
	tb.Helper()
	decoder, err := CompileDecoder[T](opts)
	if err != nil {
		tb.Fatal(err)
	}
	return decoder
}

func requireNoTestError(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
}

// assertEncodesLikeStdlib is the encode-side oracle: simdjson.Marshal must
// agree with encoding/json.Marshal on both the acceptance decision and, when
// both accept, the exact byte output. It is the extracted form of the
// four-line clone repeated across the encoder tests, so a single place decides
// what "matches stdlib" means and prints got/want on failure.
func assertEncodesLikeStdlib[T any](t *testing.T, value *T) {
	t.Helper()
	want, wantErr := json.Marshal(value)
	got, gotErr := Marshal(value)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%#v: encode acceptance differs: simdjson=%v stdlib=%v", value, gotErr, wantErr)
	}
	if gotErr == nil && !bytes.Equal(got, want) {
		t.Fatalf("%#v:\nsimdjson %s\nstdlib   %s", value, got, want)
	}
}

// assertDecodesLikeStdlib is the decode-side oracle: simdjson.Unmarshal into a
// fresh T must agree with encoding/json.Unmarshal on the acceptance decision
// and, when both accept, on the decoded value by reflect.DeepEqual. It is the
// extracted form of the decode-and-compare clone repeated across the decoder
// tests, and prints got/want on failure.
func assertDecodesLikeStdlib[T any](t *testing.T, src []byte) {
	t.Helper()
	var got, want T
	gotErr := Unmarshal(src, &got)
	wantErr := json.Unmarshal(src, &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: decode acceptance differs: simdjson=%v stdlib=%v", src, gotErr, wantErr)
	}
	if gotErr == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\nsimdjson %#v\nstdlib   %#v", src, got, want)
	}
}

// assertCompiledDecodesLikeStdlib applies the decode oracle through an
// already compiled decoder and caller-provided, possibly prefilled values.
func assertCompiledDecodesLikeStdlib[T any](t *testing.T, decoder Decoder[T], src []byte, got, want *T) bool {
	t.Helper()
	gotErr := decoder.Decode(src, got)
	wantErr := json.Unmarshal(src, want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: decode acceptance differs: simdjson=%v stdlib=%v", src, gotErr, wantErr)
	}
	if gotErr != nil {
		return false
	}
	if !reflect.DeepEqual(*got, *want) {
		t.Fatalf("%s:\nsimdjson %#v\nstdlib   %#v", src, *got, *want)
	}
	return true
}
