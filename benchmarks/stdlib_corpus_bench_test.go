package benchmarks

import (
	"bytes"
	stdjson "encoding/json"
	"fmt"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/bytedance/sonic"
	goccyjson "github.com/goccy/go-json"
	jsoniter "github.com/json-iterator/go"
	segmentjson "github.com/segmentio/encoding/json"
	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/document"
	stdlibcorpus "github.com/thesyncim/simdjson/tests/stdlib"
	"github.com/valyala/fastjson"
)

var corpusBytesSink []byte

func BenchmarkStdlibCorpus(b *testing.B) {
	for _, name := range stdlibcorpus.Names {
		src, err := stdlibcorpus.Read(name)
		if err != nil {
			b.Fatal(err)
		}
		label := strings.TrimSuffix(name, ".json.zst")
		b.Run(label, func(b *testing.B) {
			b.Run("valid", func(b *testing.B) {
				benchmarkCorpusValid(b, src)
			})
			b.Run("dynamic-owned", func(b *testing.B) {
				benchmarkCorpusDynamic(b, src)
			})
			b.Run("dom", func(b *testing.B) {
				benchmarkCorpusDOM(b, src)
			})
			b.Run("typed-reused", func(b *testing.B) {
				benchmarkCorpusTypedByName(b, name, src)
			})
			b.Run("encode", func(b *testing.B) {
				benchmarkCorpusEncodeByName(b, name, src)
			})
		})
	}
}

func benchmarkCorpusTypedByName(b *testing.B, name string, src []byte) {
	switch name {
	case "canada_geometry.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.CanadaRoot](b, src)
	case "citm_catalog.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.CITMRoot](b, src)
	case "golang_source.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.GolangRoot](b, src)
	case "string_escaped.json.zst", "string_unicode.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.StringRoot](b, src)
	case "synthea_fhir.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.SyntheaRoot](b, src)
	case "twitter_status.json.zst":
		benchmarkCorpusTyped[stdlibcorpus.TwitterRoot](b, src)
	default:
		b.Fatalf("missing typed corpus model for %s", name)
	}
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

func benchmarkCorpusValid(b *testing.B, src []byte) {
	validators := []struct {
		name string
		fn   func([]byte) bool
	}{
		{"encoding-json", stdjson.Valid},
		{"go-json", goccyjson.Valid},
		{"Segment", segmentjson.Valid},
		{"jsoniter", jsoniter.Valid},
		{"fastjson", func(src []byte) bool { return fastjson.ValidateBytes(src) == nil }},
		{"simdjson", simdjson.Valid},
	}
	if sonic.APIKind != sonic.UseStdJSON {
		validators = append(validators, struct {
			name string
			fn   func([]byte) bool
		}{"Sonic", sonic.Valid})
	}
	for _, validator := range validators {
		if !validator.fn(src) {
			b.Fatalf("%s rejected corpus input", validator.name)
		}
		b.Run(validator.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				boolSink = validator.fn(src)
			}
		})
	}
}

func benchmarkCorpusDynamic(b *testing.B, src []byte) {
	var want any
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	decoders := []struct {
		name string
		fn   func([]byte, *any) error
	}{
		{"encoding-json", func(src []byte, dst *any) error { return stdjson.Unmarshal(src, dst) }},
		{"go-json", func(src []byte, dst *any) error { return goccyjson.Unmarshal(src, dst) }},
		{"Segment", func(src []byte, dst *any) error { return segmentjson.Unmarshal(src, dst) }},
		{"jsoniter", func(src []byte, dst *any) error { return jsoniter.Unmarshal(src, dst) }},
	}
	if sonic.APIKind != sonic.UseStdJSON {
		decoders = append(decoders, struct {
			name string
			fn   func([]byte, *any) error
		}{"Sonic", func(src []byte, dst *any) error { return sonic.Unmarshal(src, dst) }})
	}
	for _, decoder := range decoders {
		var got any
		if err := decoder.fn(src, &got); err != nil {
			b.Fatalf("%s: %v", decoder.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s dynamic result differs from encoding/json", decoder.name)
		}
		b.Run(decoder.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				var dst any
				if err := decoder.fn(src, &dst); err != nil {
					b.Fatal(err)
				}
				anySink = dst
			}
		})
	}
	for _, tc := range []struct {
		name string
		opts simdjson.DecoderOptions
	}{
		{"simdjson-owned", simdjson.DecoderOptions{}},
		{"simdjson-zero-copy", simdjson.DecoderOptions{ZeroCopy: true}},
	} {
		decoder, err := simdjson.CompileDecoder[any](tc.opts)
		if err != nil {
			b.Fatal(err)
		}
		var got any
		if err := decoder.Decode(src, &got); err != nil {
			b.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s dynamic result differs from encoding/json", tc.name)
		}
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			for b.Loop() {
				var value any
				if err := decoder.Decode(src, &value); err != nil {
					b.Fatal(err)
				}
				anySink = value
			}
		})
	}
}

// benchmarkCorpusDOM measures a full document walk built on top of a parse.
// The two columns are different shapes on purpose: simdjson.Parse builds only
// the structural index and decodes each scalar on demand as the Value walk
// reaches it, whereas encoding/json materializes a complete owned any tree
// first and the walk reads the finished nodes. The comparison is therefore
// "one parse plus a total traversal" for each library, not two identical data
// structures.
func benchmarkCorpusDOM(b *testing.B, src []byte) {
	// Confirm both walks reach every scalar before timing.
	root, err := simdjson.Parse(src)
	if err != nil {
		b.Fatal(err)
	}
	var stdTree any
	if err := stdjson.Unmarshal(src, &stdTree); err != nil {
		b.Fatal(err)
	}
	if walkValue(root) < 0 || walkAny(stdTree) < 0 {
		b.Fatal("unreachable")
	}

	b.Run("encoding-json", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			var tree any
			if err := stdjson.Unmarshal(src, &tree); err != nil {
				b.Fatal(err)
			}
			intSink = walkAny(tree)
		}
	})
	b.Run("simdjson", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			value, err := simdjson.Parse(src)
			if err != nil {
				b.Fatal(err)
			}
			intSink = walkValue(value)
		}
	})
}

// walkValue traverses an on-demand simdjson Value, forcing every scalar to
// decode so the walk exercises the same work a real consumer would. It returns
// a running node count so nothing is optimized away.
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
		s, _ := v.Text()
		return len(s)
	case document.Number:
		f, _ := v.Float64()
		if f == 0 {
			return 1
		}
		return 1
	case document.Bool:
		if bv, _ := v.Bool(); bv {
			return 1
		}
		return 0
	default:
		return 0
	}
}

// walkAny traverses a materialized encoding/json any tree with the same shape
// of work as walkValue, so the two DOM columns visit equivalent structure.
func walkAny(v any) int {
	switch t := v.(type) {
	case map[string]any:
		count := 1
		for key, child := range t {
			count += len(key)
			count += walkAny(child)
		}
		return count
	case []any:
		count := 1
		for i := range t {
			count += walkAny(t[i])
		}
		return count
	case string:
		return len(t)
	case float64:
		if t == 0 {
			return 1
		}
		return 1
	case bool:
		if t {
			return 1
		}
		return 0
	default:
		return 0
	}
}

func benchmarkCorpusTyped[T any](b *testing.B, src []byte) {
	var want T
	if err := stdjson.Unmarshal(src, &want); err != nil {
		b.Fatal(err)
	}
	decoders := []struct {
		name string
		fn   func([]byte, *T) error
	}{
		{"encoding-json", func(src []byte, dst *T) error { return stdjson.Unmarshal(src, dst) }},
		{"go-json", func(src []byte, dst *T) error { return goccyjson.Unmarshal(src, dst) }},
		{"Segment", func(src []byte, dst *T) error { return segmentjson.Unmarshal(src, dst) }},
		{"jsoniter", func(src []byte, dst *T) error { return jsoniter.Unmarshal(src, dst) }},
	}
	if sonic.APIKind != sonic.UseStdJSON {
		decoders = append(decoders, struct {
			name string
			fn   func([]byte, *T) error
		}{"Sonic", func(src []byte, dst *T) error { return sonic.Unmarshal(src, dst) }})
	}
	for _, decoder := range decoders {
		var got T
		if err := decoder.fn(src, &got); err != nil {
			b.Fatalf("%s: %v", decoder.name, err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s typed result differs from encoding/json", decoder.name)
		}
		b.Run(decoder.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			var dst T
			for b.Loop() {
				if err := decoder.fn(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
			runtime.KeepAlive(dst)
		})
	}
	for _, opts := range []struct {
		name string
		opts simdjson.DecoderOptions
	}{
		{"simdjson-owned", simdjson.DecoderOptions{}},
		{"simdjson-zero-copy", simdjson.DecoderOptions{ZeroCopy: true}},
	} {
		decoder, err := simdjson.CompileDecoder[T](opts.opts)
		if err != nil {
			b.Fatal(err)
		}
		var got T
		if err := decoder.Decode(src, &got); err != nil {
			b.Fatal(err)
		}
		if !reflect.DeepEqual(got, want) {
			b.Fatalf("%s typed result differs from encoding/json", opts.name)
		}
		b.Run(opts.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(src)))
			var dst T
			for b.Loop() {
				if err := decoder.Decode(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
			runtime.KeepAlive(dst)
		})
	}
}

func benchmarkCorpusEncode[T any](b *testing.B, src []byte) {
	var value T
	if err := stdjson.Unmarshal(src, &value); err != nil {
		b.Fatal(err)
	}
	want, err := stdjson.Marshal(&value)
	if err != nil {
		b.Fatal(err)
	}
	encoders := []struct {
		name string
		fn   func(*T) ([]byte, error)
	}{
		{"encoding-json", func(src *T) ([]byte, error) { return stdjson.Marshal(src) }},
		{"go-json", func(src *T) ([]byte, error) { return goccyjson.Marshal(src) }},
		{"Segment", func(src *T) ([]byte, error) { return segmentjson.Marshal(src) }},
		{"jsoniter", func(src *T) ([]byte, error) { return jsoniter.Marshal(src) }},
		{"simdjson-owned", func(src *T) ([]byte, error) { return simdjson.Marshal(src) }},
	}
	if sonic.APIKind != sonic.UseStdJSON {
		encoders = append(encoders, struct {
			name string
			fn   func(*T) ([]byte, error)
		}{"Sonic", func(src *T) ([]byte, error) { return sonic.Marshal(src) }})
	}
	for _, encoder := range encoders {
		got, err := encoder.fn(&value)
		if err != nil {
			b.Fatalf("%s: %v", encoder.name, err)
		}
		if err := equivalentJSON(want, got); err != nil {
			b.Fatalf("%s: %v", encoder.name, err)
		}
		// Capacity preparation is outside the timer. In particular, simdjson
		// requires two matching large observations before an exact hint can
		// affect later allocations.
		if _, err := encoder.fn(&value); err != nil {
			b.Fatalf("%s warmup: %v", encoder.name, err)
		}
		b.Run(encoder.name, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(want)))
			for b.Loop() {
				out, err := encoder.fn(&value)
				if err != nil {
					b.Fatal(err)
				}
				corpusBytesSink = out
			}
		})
	}
	compiled, err := simdjson.CompileEncoder[T](simdjson.EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("simdjson-compiled-reuse", func(b *testing.B) {
		b.ReportAllocs()
		b.SetBytes(int64(len(want)))
		out := make([]byte, 0, len(want))
		for b.Loop() {
			out, err = compiled.AppendJSON(out[:0], &value)
			if err != nil {
				b.Fatal(err)
			}
		}
		corpusBytesSink = out
	})
}

func equivalentJSON(want, got []byte) error {
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
