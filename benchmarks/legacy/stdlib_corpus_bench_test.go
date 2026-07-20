package legacy

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	"github.com/klauspost/compress/zstd"
)

var legacyCorpusNames = []string{
	"canada_geometry.json.zst",
	"citm_catalog.json.zst",
	"golang_source.json.zst",
	"string_escaped.json.zst",
	"string_unicode.json.zst",
	"synthea_fhir.json.zst",
	"twitter_status.json.zst",
}

var legacyCorpusBytesSink []byte

func BenchmarkStdlibCorpusNativeSonic(b *testing.B) {
	assertNativeSonic(b)
	decoder, err := zstd.NewReader(nil)
	if err != nil {
		b.Fatal(err)
	}
	defer decoder.Close()
	for _, name := range legacyCorpusNames {
		compressed, err := os.ReadFile(filepath.Join("..", "..", "tests", "stdlib", "testdata", name))
		if err != nil {
			b.Fatal(err)
		}
		src, err := decoder.DecodeAll(compressed, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.Run(strings.TrimSuffix(name, ".json.zst"), func(b *testing.B) {
			benchmarkLegacyValid(b, src)
			benchmarkLegacyDynamic(b, src)
			benchmarkLegacyTypedByName(b, name, src)
			benchmarkLegacyEncodeByName(b, name, src)
		})
	}
}

func benchmarkLegacyValid(b *testing.B, src []byte) {
	for _, tc := range []struct {
		name string
		fn   func([]byte) bool
	}{
		{"valid/encoding-json", stdjson.Valid},
		{"valid/Sonic-native", sonic.Valid},
	} {
		if !tc.fn(src) {
			b.Fatalf("%s rejected corpus input", tc.name)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				boolSink = tc.fn(src)
			}
		})
	}
}

func benchmarkLegacyDynamic(b *testing.B, src []byte) {
	var want, got any
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	if err := sonic.ConfigStd.Unmarshal(src, &got); err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatal("Sonic dynamic result differs from encoding/json")
	}
	for _, tc := range []struct {
		name string
		fn   func([]byte, *any) error
	}{
		{"dynamic-owned/encoding-json", func(src []byte, dst *any) error { return stdjson.Unmarshal(src, dst) }},
		{"dynamic-owned/Sonic-native", func(src []byte, dst *any) error { return sonic.ConfigStd.Unmarshal(src, dst) }},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				var dst any
				if err := tc.fn(src, &dst); err != nil {
					b.Fatal(err)
				}
				anySink = dst
			}
		})
	}
}

func benchmarkLegacyTypedByName(b *testing.B, name string, src []byte) {
	switch name {
	case "canada_geometry.json.zst":
		benchmarkLegacyTyped[canadaRoot](b, src)
	case "citm_catalog.json.zst":
		benchmarkLegacyTyped[citmRoot](b, src)
	case "golang_source.json.zst":
		benchmarkLegacyTyped[golangRoot](b, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkLegacyTyped[stringRoot](b, src)
	case "synthea_fhir.json.zst":
		benchmarkLegacyTyped[syntheaRoot](b, src)
	case "twitter_status.json.zst":
		benchmarkLegacyTyped[twitterRoot](b, src)
	}
}

func benchmarkLegacyTyped[T any](b *testing.B, src []byte) {
	var want, got T
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	if err := sonic.ConfigStd.Unmarshal(src, &got); err != nil {
		b.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		b.Fatal("Sonic typed result differs from encoding/json")
	}
	b.Run("typed-reused/encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		var dst T
		for b.Loop() {
			if err := stdjson.Unmarshal(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
		runtime.KeepAlive(dst)
	})
	for _, tc := range []struct {
		name string
		api  sonic.API
	}{
		{"typed-reused/Sonic-native-owned", sonic.ConfigStd},
		{"typed-reused/Sonic-native-zero-copy", sonic.ConfigFastest},
	} {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			var dst T
			for b.Loop() {
				if err := tc.api.Unmarshal(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
			runtime.KeepAlive(dst)
		})
	}
}

func benchmarkLegacyEncodeByName(b *testing.B, name string, src []byte) {
	switch name {
	case "canada_geometry.json.zst":
		benchmarkLegacyEncode[canadaRoot](b, src)
	case "citm_catalog.json.zst":
		benchmarkLegacyEncode[citmRoot](b, src)
	case "golang_source.json.zst":
		benchmarkLegacyEncode[golangRoot](b, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkLegacyEncode[stringRoot](b, src)
	case "synthea_fhir.json.zst":
		benchmarkLegacyEncode[syntheaRoot](b, src)
	case "twitter_status.json.zst":
		benchmarkLegacyEncode[twitterRoot](b, src)
	}
}

func benchmarkLegacyEncode[T any](b *testing.B, src []byte) {
	var value T
	if err := stdjson.Unmarshal(src, &value); err != nil {
		b.Fatal(err)
	}
	want, err := stdjson.Marshal(&value)
	if err != nil {
		b.Fatal(err)
	}
	got, err := sonic.ConfigStd.Marshal(&value)
	if err != nil {
		b.Fatal(err)
	}
	if err := equivalentLegacyJSON(want, got); err != nil {
		b.Fatal(err)
	}
	b.Run("encode/encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(want)))
		for b.Loop() {
			out, err := stdjson.Marshal(&value)
			if err != nil {
				b.Fatal(err)
			}
			legacyCorpusBytesSink = out
		}
	})
	b.Run("encode/Sonic-native", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(want)))
		for b.Loop() {
			out, err := sonic.ConfigStd.Marshal(&value)
			if err != nil {
				b.Fatal(err)
			}
			legacyCorpusBytesSink = out
		}
	})
}

func equivalentLegacyJSON(want, got []byte) error {
	if bytes.Equal(want, got) {
		return nil
	}
	var wantValue, gotValue any
	if err := stdjson.Unmarshal(want, &wantValue); err != nil {
		return err
	}
	if err := stdjson.Unmarshal(got, &gotValue); err != nil {
		return err
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		return fmt.Errorf("encoded value differs from encoding/json")
	}
	return nil
}
