package benchmarks

import (
	"strconv"
	"strings"
	"testing"

	"github.com/thesyncim/simdjson"
	simdbackend "github.com/thesyncim/simdjson/simd"
)

type fixture struct {
	name string
	data []byte
}

var fixtures = []fixture{
	{name: "small", data: []byte(`{"id":1,"ok":true,"name":"sim"}`)},
	{name: "medium", data: recordsJSON(32)},
	{name: "large", data: recordsJSON(1024)},
}

var (
	boolSink          bool
	anySink           any
	simdjsonValueSink simdjson.Value
	intSink           int
)

func recordsJSON(count int) []byte {
	var out strings.Builder
	out.Grow(count * 128)
	out.WriteString(`{"items":[`)
	for i := 0; i < count; i++ {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(`{"id":`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`,"active":`)
		if i&1 == 0 {
			out.WriteString("true")
		} else {
			out.WriteString("false")
		}
		out.WriteString(`,"name":"record-`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`","message":"plain ascii payload sized to exercise vector scanners","scores":[1,2.5,-3e4]}`)
	}
	out.WriteString(`],"meta":{"count":`)
	out.WriteString(strconv.Itoa(count))
	out.WriteString(`,"source":"benchmark"}}`)
	return []byte(out.String())
}

func TestFixturesValid(t *testing.T) {
	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			if !simdjson.Valid(fixture.data) {
				t.Fatal("fixture rejected")
			}
		})
	}
}

func TestFixtureSizes(t *testing.T) {
	want := map[string]int{"small": 31, "medium": 4240, "large": 136586}
	for _, fixture := range fixtures {
		if got := len(fixture.data); got != want[fixture.name] {
			t.Fatalf("%s fixture size = %d, want %d", fixture.name, got, want[fixture.name])
		}
	}
}

func BenchmarkValid(b *testing.B) {
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			b.Run(simdbackend.Current().StringBackend, func(b *testing.B) {
				b.SetBytes(int64(len(fixture.data)))
				b.ReportAllocs()
				for b.Loop() {
					boolSink = simdjson.Valid(fixture.data)
				}
			})
		})
	}
}

func BenchmarkValidLateInvalid(b *testing.B) {
	for _, fixture := range fixtures {
		invalid := append(append([]byte(nil), fixture.data...), 'x')
		b.Run(fixture.name, func(b *testing.B) {
			b.SetBytes(int64(len(invalid)))
			b.ReportAllocs()
			for b.Loop() {
				boolSink = simdjson.Valid(invalid)
			}
		})
	}
}

func BenchmarkParseAny(b *testing.B) {
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			benchmarkParseAny(b, fixture.data)
		})
	}
}

func BenchmarkParseAnyNumbers16(b *testing.B) {
	benchmarkParseAny(b, numbers16JSON(1024))
}

func benchmarkParseAny(b *testing.B, src []byte) {
	b.Run("parse-value-any", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for b.Loop() {
			value, err := simdjson.ParseOptions(src, simdjson.Options{ZeroCopy: true})
			if err != nil {
				b.Fatal(err)
			}
			anySink = value.Any()
		}
	})
	b.Run("unmarshal-owned", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for b.Loop() {
			var value any
			if err := simdjson.Unmarshal(src, &value); err != nil {
				b.Fatal(err)
			}
			anySink = value
		}
	})
	decoder, err := simdjson.CompileDecoder[any](simdjson.DecoderOptions{ZeroCopy: true})
	if err != nil {
		b.Fatal(err)
	}
	b.Run("compiled-zero-copy", func(b *testing.B) {
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		for b.Loop() {
			var value any
			if err := decoder.Decode(src, &value); err != nil {
				b.Fatal(err)
			}
			anySink = value
		}
	})
}

func numbers16JSON(count int) []byte {
	var out strings.Builder
	out.Grow(count*17 + 2)
	out.WriteByte('[')
	for i := 0; i < count; i++ {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.FormatInt(1_000_000_000_000_000+int64(i), 10))
	}
	out.WriteByte(']')
	return []byte(out.String())
}

func BenchmarkParseNative(b *testing.B) {
	for _, fixture := range fixtures {
		b.Run(fixture.name, func(b *testing.B) {
			b.Run("value", func(b *testing.B) {
				b.SetBytes(int64(len(fixture.data)))
				b.ReportAllocs()
				for b.Loop() {
					value, err := simdjson.ParseOptions(fixture.data, simdjson.Options{ZeroCopy: true})
					if err != nil {
						b.Fatal(err)
					}
					simdjsonValueSink = value
				}
			})
			b.Run("index-reused", func(b *testing.B) {
				count, err := simdjson.RequiredIndexEntries(fixture.data)
				if err != nil {
					b.Fatal(err)
				}
				storage := make([]simdjson.IndexEntry, count)
				b.SetBytes(int64(len(fixture.data)))
				b.ReportAllocs()
				b.ResetTimer()
				for b.Loop() {
					index, err := simdjson.BuildIndex(fixture.data, storage)
					if err != nil {
						b.Fatal(err)
					}
					intSink = index.Len()
				}
			})
		})
	}
}
