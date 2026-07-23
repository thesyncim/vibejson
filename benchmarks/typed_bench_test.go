package benchmarks

import (
	"testing"

	"github.com/thesyncim/slopjson"
)

var (
	typedZeroCopyOptions = slopjson.DecoderOptions{ZeroCopy: true, CaseSensitive: true}
	typedOwnedOptions    = slopjson.DecoderOptions{CaseSensitive: true}
	typedSmallSink       TypedSmall
	typedDocumentSink    TypedDocument
	typedSmallDecoder    = mustTypedDecoder[TypedSmall](typedZeroCopyOptions)
	typedDocDecoder      = mustTypedDecoder[TypedDocument](typedZeroCopyOptions)
	typedSmallOwned      = mustTypedDecoder[TypedSmall](typedOwnedOptions)
	typedDocOwned        = mustTypedDecoder[TypedDocument](typedOwnedOptions)
	typedDocEncoder      = mustTypedEncoder[TypedDocument]()
	encodeOutSink        []byte
)

func mustTypedDecoder[T any](opts slopjson.DecoderOptions) slopjson.Decoder[T] {
	decoder, err := slopjson.CompileDecoder[T](opts)
	if err != nil {
		panic(err)
	}
	return decoder
}

func mustTypedEncoder[T any]() slopjson.Encoder[T] {
	encoder, err := slopjson.CompileEncoder[T](slopjson.EncoderOptions{})
	if err != nil {
		panic(err)
	}
	return encoder
}

func BenchmarkParseTyped(b *testing.B) {
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			if fixture.name == "small" {
				benchmarkTypedSmall(b, fixture.data)
				return
			}
			benchmarkTypedDocument(b, fixture.data)
		})
	}
}

func benchmarkTypedSmall(b *testing.B, src []byte) {
	for _, bench := range []struct {
		name    string
		decoder slopjson.Decoder[TypedSmall]
	}{
		{name: "compiled-zero-copy", decoder: typedSmallDecoder},
		{name: "compiled-owned", decoder: typedSmallOwned},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				var dst TypedSmall
				if err := bench.decoder.Decode(src, &dst); err != nil {
					b.Fatal(err)
				}
				typedSmallSink = dst
			}
		})
	}
}

func benchmarkTypedDocument(b *testing.B, src []byte) {
	for _, bench := range []struct {
		name    string
		decoder slopjson.Decoder[TypedDocument]
	}{
		{name: "compiled-zero-copy", decoder: typedDocDecoder},
		{name: "compiled-owned", decoder: typedDocOwned},
	} {
		b.Run(bench.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				var dst TypedDocument
				if err := bench.decoder.Decode(src, &dst); err != nil {
					b.Fatal(err)
				}
				typedDocumentSink = dst
			}
		})
		b.Run(bench.name+"-reused", func(b *testing.B) {
			dst := TypedDocument{Items: make([]TypedRecord, 0, 1024)}
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := bench.decoder.Decode(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
			typedDocumentSink = dst
		})
	}
}

func BenchmarkParseTypedLargeIndentedReused(b *testing.B) {
	src, err := slopjson.Indent(recordsJSON(1024), "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	benchmarkTypedDocument(b, src)
}

func BenchmarkEncodeTyped(b *testing.B) {
	src := recordsJSON(1024)
	var doc TypedDocument
	if err := typedDocOwned.Decode(src, &doc); err != nil {
		b.Fatal(err)
	}
	warm, err := typedDocEncoder.AppendJSON(nil, &doc)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("marshal", func(b *testing.B) {
		b.SetBytes(int64(len(warm)))
		b.ReportAllocs()
		for b.Loop() {
			out, err := slopjson.Marshal(&doc)
			if err != nil {
				b.Fatal(err)
			}
			encodeOutSink = out
		}
	})
	b.Run("compiled-reused", func(b *testing.B) {
		out := make([]byte, 0, len(warm))
		b.SetBytes(int64(len(warm)))
		b.ReportAllocs()
		b.ResetTimer()
		for b.Loop() {
			out, err = typedDocEncoder.AppendJSON(out[:0], &doc)
			if err != nil {
				b.Fatal(err)
			}
		}
		encodeOutSink = out
	})
}
