package stdlibcorpus

import (
	"encoding/json"
	"testing"

	"github.com/thesyncim/simdjson"
)

var corpusIndexLen int

func BenchmarkHighLevelCorpus(b *testing.B) {
	for _, name := range Names {
		src, err := Read(name)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(name, func(b *testing.B) {
			b.Run("valid/encoding-json", func(b *testing.B) {
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					if !json.Valid(src) {
						b.Fatal("invalid corpus input")
					}
				}
			})
			b.Run("valid/simdjson", func(b *testing.B) {
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					if !simdjson.Valid(src) {
						b.Fatal("invalid corpus input")
					}
				}
			})
			need, err := simdjson.RequiredIndexEntries(src)
			if err != nil {
				b.Fatal(err)
			}
			indexStorage := make([]simdjson.IndexEntry, need)
			b.Run("index/simdjson-reused", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					index, err := simdjson.BuildIndex(src, indexStorage)
					if err != nil {
						b.Fatal(err)
					}
					corpusIndexLen = index.Len()
				}
			})
			b.Run("decode-any/encoding-json", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					var dst any
					if err := json.Unmarshal(src, &dst); err != nil {
						b.Fatal(err)
					}
				}
			})
			b.Run("decode-any/simdjson", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					var dst any
					if err := simdjson.Unmarshal(src, &dst); err != nil {
						b.Fatal(err)
					}
				}
			})
			zeroCopyAnyDecoder, err := simdjson.CompileDecoder[any](simdjson.DecoderOptions{ZeroCopy: true})
			if err != nil {
				b.Fatal(err)
			}
			b.Run("decode-any/simdjson-zero-copy", func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(src)))
				for b.Loop() {
					var dst any
					if err := zeroCopyAnyDecoder.Decode(src, &dst); err != nil {
						b.Fatal(err)
					}
				}
			})
			benchmarkTypedCorpus(b, name, src)
		})
	}
}

func benchmarkTypedCorpus(b *testing.B, name string, src []byte) {
	b.Helper()
	switch name {
	case "canada_geometry.json.zst":
		benchmarkTyped[canadaRoot](b, src)
	case "citm_catalog.json.zst":
		benchmarkTyped[citmRoot](b, src)
	case "golang_source.json.zst":
		benchmarkTyped[golangRoot](b, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkTyped[stringRoot](b, src)
	case "synthea_fhir.json.zst":
		benchmarkTyped[syntheaRoot](b, src)
	case "twitter_status.json.zst":
		benchmarkTyped[twitterRoot](b, src)
	default:
		b.Fatalf("stdlib corpus has no concrete model: %s", name)
	}
}

func benchmarkTyped[T any](b *testing.B, src []byte) {
	b.Helper()
	// Typed decode rows deliberately reuse one destination. Populate it before
	// b.Loop so its untimed setup makes every measured iteration steady-state.
	b.Run("decode-typed/encoding-json-unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		if err := json.Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := json.Unmarshal(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("decode-typed/simdjson-unmarshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		if err := simdjson.Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := simdjson.Unmarshal(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})

	decoder, err := simdjson.CompileDecoder[T](simdjson.DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("decode-typed/simdjson-compiled", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := decoder.Decode(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})

	zeroCopyDecoder, err := simdjson.CompileDecoder[T](simdjson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("decode-typed/simdjson-compiled-zero-copy", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		if err := zeroCopyDecoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
		for b.Loop() {
			if err := zeroCopyDecoder.Decode(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})

	var value T
	if err := json.Unmarshal(src, &value); err != nil {
		b.Fatal(err)
	}
	b.Run("encode-typed/encoding-json-marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			if _, err := json.Marshal(&value); err != nil {
				b.Fatal(err)
			}
		}
	})
	// Marshal deliberately requires two matching large observations before it
	// trusts a size hint. Capacity preparation, like plan compilation, belongs
	// outside the steady-state timer.
	for range 2 {
		if _, err := simdjson.Marshal(&value); err != nil {
			b.Fatal(err)
		}
	}
	b.Run("encode-typed/simdjson-marshal", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			if _, err := simdjson.Marshal(&value); err != nil {
				b.Fatal(err)
			}
		}
	})

	encoder, err := simdjson.CompileEncoder[T](simdjson.EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("encode-typed/simdjson-compiled-reuse", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		dst := make([]byte, 0, len(src))
		for b.Loop() {
			var err error
			dst, err = encoder.AppendJSON(dst[:0], &value)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
}
