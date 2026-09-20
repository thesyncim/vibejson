package vibejson

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

func mustCompileTestEncoder[T any](tb testing.TB, opts EncoderOptions) Encoder[T] {
	tb.Helper()
	encoder, err := CompileEncoder[T](opts)
	if err != nil {
		tb.Fatal(err)
	}
	return encoder
}

func requireNoTestError(tb testing.TB, err error) {
	tb.Helper()
	if err != nil {
		tb.Fatal(err)
	}
}

func assertEncodesLikeStdlib[T any](t *testing.T, value *T) {
	t.Helper()
	want, wantErr := json.Marshal(value)
	got, gotErr := Marshal(value)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%#v: encode acceptance differs: vibejson=%v stdlib=%v", value, gotErr, wantErr)
	}
	if gotErr == nil && !bytes.Equal(got, want) {
		t.Fatalf("%#v:\nvibejson %s\nstdlib   %s", value, got, want)
	}
}

func assertDecodesLikeStdlib[T any](t *testing.T, src []byte) {
	t.Helper()
	var got, want T
	gotErr := Unmarshal(src, &got)
	wantErr := json.Unmarshal(src, &want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: decode acceptance differs: vibejson=%v stdlib=%v", src, gotErr, wantErr)
	}
	if gotErr == nil && !reflect.DeepEqual(got, want) {
		t.Fatalf("%s:\nvibejson %#v\nstdlib   %#v", src, got, want)
	}
}

func assertCompiledDecodesLikeStdlib[T any](t *testing.T, decoder Decoder[T], src []byte, got, want *T) bool {
	t.Helper()
	gotErr := decoder.Decode(src, got)
	wantErr := json.Unmarshal(src, want)
	if (gotErr == nil) != (wantErr == nil) {
		t.Fatalf("%s: decode acceptance differs: vibejson=%v stdlib=%v", src, gotErr, wantErr)
	}
	if gotErr != nil {
		return false
	}
	if !reflect.DeepEqual(*got, *want) {
		t.Fatalf("%s:\nvibejson %#v\nstdlib   %#v", src, *got, *want)
	}
	return true
}
