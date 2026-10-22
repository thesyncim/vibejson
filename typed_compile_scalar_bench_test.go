package vibejson

import "testing"

func BenchmarkCompileScalarPlan(b *testing.B) {
	b.Run("Decode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := CompileDecoder[int](DecoderOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Encode", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := CompileEncoder[int](EncoderOptions{}); err != nil {
				b.Fatal(err)
			}
		}
	})
}
