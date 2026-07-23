package benchmarks

import (
	"runtime"
	"strings"
	"testing"

	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/document"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

var corpusBytesSink []byte

// BenchmarkCorpus exercises the native APIs over the pinned real-world corpus.
// Setup, model selection, and capacity discovery stay outside timed regions.
func BenchmarkCorpus(b *testing.B) {
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(strings.TrimSuffix(name, ".json.zst"), func(b *testing.B) {
			b.Run("valid", func(b *testing.B) {
				b.SetBytes(int64(len(src)))
				b.ReportAllocs()
				for b.Loop() {
					boolSink = simdjson.Valid(src)
				}
			})
			b.Run("dynamic-owned", func(b *testing.B) {
				benchmarkCorpusDynamic(b, src, simdjson.DecoderOptions{})
			})
			b.Run("dynamic-zero-copy", func(b *testing.B) {
				benchmarkCorpusDynamic(b, src, simdjson.DecoderOptions{ZeroCopy: true})
			})
			b.Run("parse-walk", func(b *testing.B) {
				benchmarkCorpusWalk(b, src)
			})
			b.Run("typed-owned", func(b *testing.B) {
				benchmarkCorpusTypedByName(b, name, src, simdjson.DecoderOptions{})
			})
			b.Run("typed-zero-copy", func(b *testing.B) {
				benchmarkCorpusTypedByName(b, name, src, simdjson.DecoderOptions{ZeroCopy: true})
			})
			b.Run("encode", func(b *testing.B) {
				benchmarkCorpusEncodeByName(b, name, src)
			})
		})
	}
}

func benchmarkCorpusDynamic(b *testing.B, src []byte, opts simdjson.DecoderOptions) {
	decoder, err := simdjson.CompileDecoder[any](opts)
	if err != nil {
		b.Fatal(err)
	}
	var check any
	if err := decoder.Decode(src, &check); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		var value any
		if err := decoder.Decode(src, &value); err != nil {
			b.Fatal(err)
		}
		anySink = value
	}
}

func benchmarkCorpusWalk(b *testing.B, src []byte) {
	root, err := simdjson.Parse(src)
	if err != nil {
		b.Fatal(err)
	}
	if walkValue(root) < 0 {
		b.Fatal("unreachable")
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		value, err := simdjson.Parse(src)
		if err != nil {
			b.Fatal(err)
		}
		intSink = walkValue(value)
	}
}

func walkValue(v simdjson.Value) int {
	switch v.Kind() {
	case document.Object:
		count := 1
		members, _ := v.Object()
		for i := range members {
			count += len(members[i].Key)
			count += walkValue(members[i].Value)
		}
		return count
	case document.Array:
		count := 1
		elems, _ := v.Array()
		for i := range elems {
			count += walkValue(elems[i])
		}
		return count
	case document.String:
		text, _ := v.Text()
		return len(text)
	case document.Number:
		_, _ = v.Float64()
		return 1
	case document.Bool:
		value, _ := v.Bool()
		if value {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func benchmarkCorpusTypedByName(b *testing.B, name string, src []byte, opts simdjson.DecoderOptions) {
	switch name {
	case "canada_geometry.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.CanadaRoot](b, src, opts)
	case "citm_catalog.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.CITMRoot](b, src, opts)
	case "golang_source.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.GolangRoot](b, src, opts)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.StringRoot](b, src, opts)
	case "synthea_fhir.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.SyntheaRoot](b, src, opts)
	case "twitter_status.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.TwitterRoot](b, src, opts)
	default:
		b.Fatalf("missing typed corpus model for %s", name)
	}
}

func benchmarkCorpusTyped[T any](b *testing.B, src []byte, opts simdjson.DecoderOptions) {
	decoder, err := simdjson.CompileDecoder[T](opts)
	if err != nil {
		b.Fatal(err)
	}
	var check T
	if err := decoder.Decode(src, &check); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	var dst T
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
	runtime.KeepAlive(dst)
}

func benchmarkCorpusEncodeByName(b *testing.B, name string, src []byte) {
	switch name {
	case "canada_geometry.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.CanadaRoot](b, src)
	case "citm_catalog.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.CITMRoot](b, src)
	case "golang_source.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.GolangRoot](b, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.StringRoot](b, src)
	case "synthea_fhir.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.SyntheaRoot](b, src)
	case "twitter_status.json.zst":
		benchmarkCorpusEncode[stdlibcorpus.TwitterRoot](b, src)
	default:
		b.Fatalf("missing encoder corpus model for %s", name)
	}
}

func benchmarkCorpusEncode[T any](b *testing.B, src []byte) {
	decoder, err := simdjson.CompileDecoder[T](simdjson.DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var value T
	if err := decoder.Decode(src, &value); err != nil {
		b.Fatal(err)
	}
	encoder, err := simdjson.CompileEncoder[T](simdjson.EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	warm, err := encoder.AppendJSON(nil, &value)
	if err != nil {
		b.Fatal(err)
	}
	out := make([]byte, 0, len(warm))
	b.SetBytes(int64(len(warm)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		out, err = encoder.AppendJSON(out[:0], &value)
		if err != nil {
			b.Fatal(err)
		}
	}
	corpusBytesSink = out
}
