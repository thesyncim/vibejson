//go:build goexperiment.jsonv2

package benchmarks

import (
	stdjson "encoding/json"
	jsonv2 "encoding/json/v2"
	"reflect"
	"strings"
	"testing"

	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
)

// BenchmarkStdlibCorpusJSONV2 measures encoding/json/v2 over the same corpus
// and contracts as the main corpus benchmark, one row per operation, so v2
// numbers slot directly next to the published tables.
func BenchmarkStdlibCorpusJSONV2(b *testing.B) {
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			b.Fatal(err)
		}
		label := strings.TrimSuffix(name, ".json.zst")
		b.Run(label, func(b *testing.B) {
			benchmarkJSONV2Dynamic(b, src)
			switch name {
			case "canada_geometry.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.CanadaRoot](b, src)
			case "citm_catalog.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.CITMRoot](b, src)
			case "golang_source.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.GolangRoot](b, src)
			case "string_escaped.json.zst", "string_unicode.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.StringRoot](b, src)
			case "synthea_fhir.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.SyntheaRoot](b, src)
			case "twitter_status.json.zst":
				benchmarkJSONV2Typed[stdlibcorpus.TwitterRoot](b, src)
			default:
				b.Fatalf("stdlib corpus has no concrete model: %s", name)
			}
		})
	}
}

func benchmarkJSONV2Dynamic(b *testing.B, src []byte) {
	var want, got any
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	if err := jsonv2.Unmarshal(src, &got); err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatal("jsonv2 dynamic result differs from encoding/json")
	}
	for _, decoder := range []struct {
		name string
		fn   func([]byte, any) error
	}{
		{name: "encoding-json", fn: stdjson.Unmarshal},
		{name: "jsonv2", fn: func(src []byte, dst any) error { return jsonv2.Unmarshal(src, dst) }},
	} {
		b.Run("dynamic-owned/"+decoder.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				var dst any
				if err := decoder.fn(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchmarkJSONV2Typed[T any](b *testing.B, src []byte) {
	var want, got T
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	if err := jsonv2.Unmarshal(src, &got); err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatal("jsonv2 typed result differs from encoding/json")
	}
	b.Run("typed-reused/jsonv2", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		for b.Loop() {
			if err := jsonv2.Unmarshal(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("typed-reused/encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		for b.Loop() {
			if err := stdjson.Unmarshal(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("encode/jsonv2", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			out, err := jsonv2.Marshal(&want)
			if err != nil {
				b.Fatal(err)
			}
			corpusBytesSink = out
		}
	})
	b.Run("encode/encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			out, err := stdjson.Marshal(&want)
			if err != nil {
				b.Fatal(err)
			}
			corpusBytesSink = out
		}
	})
}
