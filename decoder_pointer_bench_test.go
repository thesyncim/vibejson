package vibejson

import (
	"encoding/json"
	"testing"
)

type pointerBenchScalar int

// Finite pointer chains expose repeated whole-chain safety scans.
type pointerBenchChain32 = ********************************int
type pointerBenchChain128 = ************************************************************************************************pointerBenchChain32

type pointerBenchRecord struct {
	N    int  `json:"n"`
	Flag bool `json:"flag"`
}

type pointerBenchMarker interface{ pointerBenchMarker() }

func (*pointerBenchScalar) pointerBenchMarker() {}
func (*pointerBenchRecord) pointerBenchMarker() {}

func benchmarkDecodePopulatedPointer[T any](b *testing.B, opts DecoderOptions, src []byte, dst *T) {
	decoder := mustCompileTestDecoder[T](b, opts)
	if err := decoder.Decode(src, dst); err != nil {
		b.Fatal(err)
	}
	got, err := json.Marshal(dst)
	if err != nil || string(got) != string(src) {
		b.Fatalf("decoded value = %s, %v; want %s", got, err, src)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := decoder.Decode(src, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodePointerGuard(b *testing.B) {
	for _, replace := range []bool{false, true} {
		mode := "merge"
		if replace {
			mode = "replace"
		}
		b.Run(mode, func(b *testing.B) {
			opts := DecoderOptions{Replace: replace}
			b.Run("typed_scalar", func(b *testing.B) {
				number := 0
				dst := &number
				benchmarkDecodePopulatedPointer(b, opts, []byte(`7`), &dst)
			})
			b.Run("typed_struct", func(b *testing.B) {
				dst := &pointerBenchRecord{}
				benchmarkDecodePopulatedPointer(b, opts, []byte(`{"n":7,"flag":true}`), &dst)
			})
			b.Run("typed_chain32", func(b *testing.B) {
				var dst pointerBenchChain32
				benchmarkDecodePopulatedPointer(b, opts, []byte(`7`), &dst)
			})
			b.Run("typed_chain128", func(b *testing.B) {
				var dst pointerBenchChain128
				benchmarkDecodePopulatedPointer(b, opts, []byte(`7`), &dst)
			})
			b.Run("typed_double_pointer", func(b *testing.B) {
				number := 0
				pointer := &number
				dst := &pointer
				benchmarkDecodePopulatedPointer(b, opts, []byte(`7`), &dst)
			})
		})
	}
	b.Run("populated", func(b *testing.B) {
		b.Run("any_scalar", func(b *testing.B) {
			number := 0
			var dst any = &number
			benchmarkDecodePopulatedPointer(b, DecoderOptions{}, []byte(`7`), &dst)
		})
		b.Run("any_struct", func(b *testing.B) {
			var dst any = &pointerBenchRecord{}
			benchmarkDecodePopulatedPointer(b, DecoderOptions{}, []byte(`{"n":7,"flag":true}`), &dst)
		})
		b.Run("any_pointer_chain", func(b *testing.B) {
			number := 0
			var inner any = &number
			var dst any = &inner
			benchmarkDecodePopulatedPointer(b, DecoderOptions{}, []byte(`7`), &dst)
		})
		b.Run("interface_scalar", func(b *testing.B) {
			number := pointerBenchScalar(0)
			var dst pointerBenchMarker = &number
			benchmarkDecodePopulatedPointer(b, DecoderOptions{}, []byte(`7`), &dst)
		})
		b.Run("interface_struct", func(b *testing.B) {
			var dst pointerBenchMarker = &pointerBenchRecord{}
			benchmarkDecodePopulatedPointer(b, DecoderOptions{}, []byte(`{"n":7,"flag":true}`), &dst)
		})
	})
}
