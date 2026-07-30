package benchmarks

import (
	stdjson "encoding/json"
	"reflect"
	"runtime"
	"strings"
	"testing"

	goccyjson "github.com/goccy/go-json"
	jsoniter "github.com/json-iterator/go"
	segmentjson "github.com/segmentio/encoding/json"
	"github.com/thesyncim/vibejson"
	stdlibcorpus "github.com/thesyncim/vibejson/tests/stdlib"
)

var comparisonBytesSink []byte

// BenchmarkComparisonCorpus measures public, contract-matched operations over
// the seven-file standard-library corpus. Every included implementation is
// checked against encoding/json before its timer starts.
func BenchmarkComparisonCorpus(b *testing.B) {
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(strings.TrimSuffix(name, ".json.zst"), func(b *testing.B) {
			switch name {
			case "canada_geometry.json.zst":
				benchmarkComparison[stdlibcorpus.CanadaRoot](b, src)
			case "citm_catalog.json.zst":
				benchmarkComparison[stdlibcorpus.CITMRoot](b, src)
			case "golang_source.json.zst":
				benchmarkComparison[stdlibcorpus.GolangRoot](b, src)
			case "string_escaped.json.zst", "string_unicode.json.zst":
				benchmarkComparison[stdlibcorpus.StringRoot](b, src)
			case "synthea_fhir.json.zst":
				benchmarkComparison[stdlibcorpus.SyntheaRoot](b, src)
			case "twitter_status.json.zst":
				benchmarkComparison[stdlibcorpus.TwitterRoot](b, src)
			default:
				b.Fatalf("missing typed corpus model for %s", name)
			}
		})
	}
}

func benchmarkComparison[T any](b *testing.B, src []byte) {
	benchmarkComparisonValidation(b, src)
	benchmarkComparisonTyped[T](b, src)
	benchmarkComparisonDynamic(b, src)
	benchmarkComparisonEncode[T](b, src)
}

func benchmarkComparisonValidation(b *testing.B, src []byte) {
	validators := []struct {
		name  string
		valid func([]byte) bool
	}{
		{name: "vibejson", valid: vibejson.Valid},
		{name: "encoding-json", valid: stdjson.Valid},
		{name: "go-json", valid: goccyjson.Valid},
		{name: "segmentio", valid: segmentjson.Valid},
	}
	// jsoniter.Valid accepts trailing non-space bytes after one JSON value, so
	// it is not comparable to the strict whole-document validators in this row.
	invalid := append(append([]byte(nil), src...), 'x')
	for _, validator := range validators {
		if !validator.valid(src) {
			b.Fatalf("%s rejected valid corpus input", validator.name)
		}
		if validator.valid(invalid) {
			b.Fatalf("%s accepted a late-invalid corpus input", validator.name)
		}
		b.Run("validate/"+validator.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				boolSink = validator.valid(src)
			}
		})
	}
}

func benchmarkComparisonTyped[T any](b *testing.B, src []byte) {
	var want T
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	decoders := []struct {
		name   string
		decode func([]byte, *T) error
	}{
		{name: "vibejson", decode: vibejson.Unmarshal[T]},
		{name: "encoding-json", decode: func(src []byte, dst *T) error { return stdjson.Unmarshal(src, dst) }},
		{name: "go-json", decode: func(src []byte, dst *T) error { return goccyjson.Unmarshal(src, dst) }},
		{name: "segmentio", decode: func(src []byte, dst *T) error { return segmentjson.Unmarshal(src, dst) }},
		{name: "jsoniter", decode: func(src []byte, dst *T) error { return jsoniter.Unmarshal(src, dst) }},
	}
	for _, decoder := range decoders {
		var got T
		if err := decoder.decode(src, &got); err != nil {
			b.Fatalf("%s: %v", decoder.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s typed result differs from encoding/json", decoder.name)
		}
		b.Run("decode-typed-owned/"+decoder.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			var dst T
			if err := decoder.decode(src, &dst); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if err := decoder.decode(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
			runtime.KeepAlive(dst)
		})
	}
}

func benchmarkComparisonDynamic(b *testing.B, src []byte) {
	var want any
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	decoders := []struct {
		name   string
		decode func([]byte, *any) error
	}{
		{name: "vibejson", decode: vibejson.Unmarshal[any]},
		{name: "encoding-json", decode: func(src []byte, dst *any) error { return stdjson.Unmarshal(src, dst) }},
		{name: "go-json", decode: func(src []byte, dst *any) error { return goccyjson.Unmarshal(src, dst) }},
		{name: "segmentio", decode: func(src []byte, dst *any) error { return segmentjson.Unmarshal(src, dst) }},
		{name: "jsoniter", decode: func(src []byte, dst *any) error { return jsoniter.Unmarshal(src, dst) }},
	}
	for _, decoder := range decoders {
		var got any
		if err := decoder.decode(src, &got); err != nil {
			b.Fatalf("%s: %v", decoder.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s dynamic result differs from encoding/json", decoder.name)
		}
		b.Run("decode-dynamic-owned/"+decoder.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				var dst any
				if err := decoder.decode(src, &dst); err != nil {
					b.Fatal(err)
				}
				anySink = dst
			}
		})
	}
}

func benchmarkComparisonEncode[T any](b *testing.B, src []byte) {
	var value T
	if err := stdjson.Unmarshal(src, &value); err != nil {
		b.Fatal(err)
	}
	reference, err := stdjson.Marshal(&value)
	if err != nil {
		b.Fatal(err)
	}
	var want any
	if err := stdjson.Unmarshal(reference, &want); err != nil {
		b.Fatal(err)
	}
	encoders := []struct {
		name   string
		encode func(*T) ([]byte, error)
	}{
		{name: "vibejson", encode: vibejson.Marshal[T]},
		{name: "encoding-json", encode: func(value *T) ([]byte, error) { return stdjson.Marshal(value) }},
		{name: "go-json", encode: func(value *T) ([]byte, error) { return goccyjson.Marshal(value) }},
		{name: "segmentio", encode: func(value *T) ([]byte, error) { return segmentjson.Marshal(value) }},
		{name: "jsoniter", encode: func(value *T) ([]byte, error) { return jsoniter.Marshal(value) }},
	}
	for _, encoder := range encoders {
		// Two observations warm type caches and vibejson's conservative output
		// capacity hint before the steady-state timer begins.
		var encoded []byte
		for range 2 {
			var err error
			encoded, err = encoder.encode(&value)
			if err != nil {
				b.Fatalf("%s: %v", encoder.name, err)
			}
		}
		var roundTrip any
		if err := stdjson.Unmarshal(encoded, &roundTrip); err != nil {
			b.Fatalf("%s emitted invalid JSON: %v", encoder.name, err)
		}
		if !reflect.DeepEqual(roundTrip, want) {
			b.Fatalf("%s encoded result differs semantically from encoding/json", encoder.name)
		}
		b.Run("encode-owned/"+encoder.name, func(b *testing.B) {
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			for b.Loop() {
				var err error
				comparisonBytesSink, err = encoder.encode(&value)
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
