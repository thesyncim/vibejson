package vibejson

import (
	"encoding"
	"encoding/json"
	"fmt"
	"testing"
)

type benchOrdinaryInterfaceValue struct{ Number int }

func (benchOrdinaryInterfaceValue) String() string { return "ordinary" }

func benchmarkMarshalerInterface[T any](b *testing.B, value T) {
	encoder, err := CompileEncoder[T](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	dst, err := encoder.AppendJSON(nil, &value)
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		dst, err = encoder.AppendJSON(dst[:0], &value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeMarshalerInterface(b *testing.B) {
	b.Run("JSON/addressable", func(b *testing.B) {
		benchmarkMarshalerInterface(b, nilInterfaceJSONHook{})
	})
	b.Run("JSON/non-nil", func(b *testing.B) {
		benchmarkMarshalerInterface[json.Marshaler](b, &nilInterfaceJSONHook{})
	})
	b.Run("text/addressable", func(b *testing.B) {
		benchmarkMarshalerInterface(b, nilInterfaceTextHook{})
	})
	b.Run("text/non-nil", func(b *testing.B) {
		benchmarkMarshalerInterface[encoding.TextMarshaler](b, &nilInterfaceTextHook{})
	})
	b.Run("ordinary/value", func(b *testing.B) {
		benchmarkMarshalerInterface[fmt.Stringer](b, benchOrdinaryInterfaceValue{Number: 7})
	})
	b.Run("ordinary/pointer", func(b *testing.B) {
		benchmarkMarshalerInterface[fmt.Stringer](b, &benchOrdinaryInterfaceValue{Number: 7})
	})
	b.Run("ordinary/nil-pointer", func(b *testing.B) {
		benchmarkMarshalerInterface[fmt.Stringer](b, (*benchOrdinaryInterfaceValue)(nil))
	})
}
