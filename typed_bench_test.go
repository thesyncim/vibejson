package vibejson

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

type benchSmall struct {
	ID   int    `json:"id"`
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

type benchRecord struct {
	ID      int        `json:"id"`
	Active  bool       `json:"active"`
	Name    string     `json:"name"`
	Message string     `json:"message"`
	Scores  [3]float64 `json:"scores"`
}

type benchMeta struct {
	Count  int    `json:"count"`
	Source string `json:"source"`
}

type benchDocument struct {
	Items []benchRecord `json:"items"`
	Meta  benchMeta     `json:"meta"`
}

var benchSmallJSON = []byte(`{"id":1,"ok":true,"name":"sim"}`)

var (
	benchUint64SliceSink  []uint64
	benchFloat64SliceSink []float64
	benchFloat64Sink      float64
	benchNumericSliceSink any
)

var (
	benchCompiledDecoderSink Decoder[benchDocument]
	benchCompiledEncoderSink Encoder[benchDocument]
)

func benchRecordsJSON(count int) []byte {
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

func benchRecordsOneMessageJSON(count int, message string) []byte {
	src := benchRecordsJSON(count)
	clean := []byte(`"message":"plain ascii payload sized to exercise vector scanners"`)
	dirty := []byte(`"message":"` + message + `"`)
	return bytes.Replace(src, clean, dirty, 1)
}

func benchRecordsOneEscapedStringJSON(count int) []byte {
	return benchRecordsOneMessageJSON(count, `plain\nascii payload sized to exercise vector scanners`)
}

func benchRecordsOneNonASCIIStringJSON(count int) []byte {
	return benchRecordsOneMessageJSON(count, `plain βeta payload sized to exercise vector scanners`)
}

func benchRecordsShuffledKeysJSON(count int, distantEscape bool) []byte {
	var out strings.Builder
	out.Grow(count * 128)
	out.WriteString(`{"items":[`)
	for i := range count {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(`{"message":"plain`)
		if distantEscape && i == 0 {
			out.WriteString(`\n`)
		} else {
			out.WriteByte(' ')
		}
		out.WriteString(`ascii payload sized to exercise vector scanners","scores":[1,2.5,-3e4],"id":`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`,"active":true,"name":"record-`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`"}`)
	}
	out.WriteString(`],"meta":{"count":`)
	out.WriteString(strconv.Itoa(count))
	out.WriteString(`,"source":"benchmark"}}`)
	return []byte(out.String())
}

func BenchmarkCompileTypedPlan(b *testing.B) {
	b.Run("Decode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			decoder, err := CompileDecoder[benchDocument](DecoderOptions{})
			if err != nil {
				b.Fatal(err)
			}
			benchCompiledDecoderSink = decoder
		}
	})
	b.Run("Encode", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			encoder, err := CompileEncoder[benchDocument](EncoderOptions{})
			if err != nil {
				b.Fatal(err)
			}
			benchCompiledEncoderSink = encoder
		}
	})
}

func BenchmarkDecodeSmall(b *testing.B) {
	decoder, err := CompileDecoder[benchSmall](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchSmallJSON)))
	b.ReportAllocs()
	for range b.N {
		var dst benchSmall
		if err := decoder.Decode(benchSmallJSON, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeMapReused(b *testing.B) {
	decoder, err := CompileDecoder[map[string]int](DecoderOptions{CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	src := []byte(`{"alpha":1,"bravo":2,"charlie":3,"delta":4,"echo":5,"foxtrot":6,"golf":7,"hotel":8}`)
	dst := make(map[string]int, 8)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeUint64Array16(b *testing.B) {
	const count = 1024
	src := fixed16Uint64ArrayJSON(count)

	b.Run("DecodeArray", func(b *testing.B) {
		decoder, err := CompileDecoder[uint64](DecoderOptions{Replace: true})
		if err != nil {
			b.Fatal(err)
		}
		dst := make([]uint64, 0, count)
		dst, err = decoder.DecodeArray(src, dst)
		if err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dst, err = decoder.DecodeArray(src, dst[:0])
			if err != nil {
				b.Fatal(err)
			}
		}
		benchUint64SliceSink = dst
	})

	b.Run("Decode", func(b *testing.B) {
		decoder, err := CompileDecoder[[]uint64](DecoderOptions{Replace: true})
		if err != nil {
			b.Fatal(err)
		}
		dst := make([]uint64, 0, count)
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
		b.SetBytes(int64(len(src)))
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			if err := decoder.Decode(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
		benchUint64SliceSink = dst
	})
}

func benchNumericTelemetryJSON(count int) []byte {
	var out strings.Builder
	out.Grow(count*10 + 2)
	out.WriteByte('[')
	for i := range count {
		if i != 0 {
			out.WriteByte(',')
		}
		value := float64((i*7919)%200_000-100_000) / 1000
		out.Write(strconv.AppendFloat(nil, value, 'f', 3, 64))
	}
	out.WriteByte(']')
	return []byte(out.String())
}

func benchNumericCoordinatesJSON(count int) []byte {
	coordinates := [...]string{
		"-65.61361699999998", "43.42027300000001",
		"-59.81694799999991", "43.92832899999996",
		"-60.02860999999996", "43.905548000000124",
	}
	var out strings.Builder
	out.Grow(count*20 + 2)
	out.WriteByte('[')
	for i := range count {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(coordinates[i%len(coordinates)])
	}
	out.WriteByte(']')
	return []byte(out.String())
}

func benchmarkNumericFloat64Decode(b *testing.B, src []byte) {
	decoder, err := CompileDecoder[[]float64](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]float64, 0, 1<<15)
	if err := decoder.Decode(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
	benchFloat64SliceSink = dst
	benchFloat64Sink = dst[len(dst)-1]
}

// BenchmarkDecodeNumericFloat64Slice measures reused typed decoding of two
// large homogeneous numeric streams: fixed-precision telemetry samples and
// long geographic coordinates. Both are public end-to-end Decoder workloads.
func BenchmarkDecodeNumericFloat64Slice(b *testing.B) {
	const count = 1 << 15
	b.Run("telemetry", func(b *testing.B) {
		benchmarkNumericFloat64Decode(b, benchNumericTelemetryJSON(count))
	})
	b.Run("coordinates", func(b *testing.B) {
		benchmarkNumericFloat64Decode(b, benchNumericCoordinatesJSON(count))
	})
}

func benchmarkNumericVibeJSON[T any](b *testing.B, src []byte, count int, options DecoderOptions) {
	decoder, err := CompileDecoder[[]T](options)
	if err != nil {
		b.Fatal(err)
	}
	dst := make([]T, 0, count)
	if err := decoder.Decode(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(src)), "input-B/op")
	b.ReportMetric(float64(count), "values/op")
	benchNumericSliceSink = dst
}

func benchmarkNumericStdlib[T any](b *testing.B, src []byte, count int) {
	dst := make([]T, 0, count)
	if err := json.Unmarshal(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for b.Loop() {
		if err := json.Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(len(src)), "input-B/op")
	b.ReportMetric(float64(count), "values/op")
	benchNumericSliceSink = dst
}

// BenchmarkNumericDecodePublication is the reproducible, contract-matched
// source for the published numeric SIMD chart. Each row decodes the same
// complete document into a reused typed slice; input construction, decoder
// compilation, and destination allocation remain outside the timed region.
func BenchmarkNumericDecodePublication(b *testing.B) {
	const (
		identifierCount = 1024
		floatCount      = 1 << 15
	)
	identifiers := fixed16Uint64ArrayJSON(identifierCount)
	telemetry := benchNumericTelemetryJSON(floatCount)
	coordinates := benchNumericCoordinatesJSON(floatCount)

	b.Run("identifiers", func(b *testing.B) {
		b.Run("vibejson", func(b *testing.B) {
			benchmarkNumericVibeJSON[uint64](b, identifiers, identifierCount, DecoderOptions{Replace: true})
		})
		b.Run("encoding-json", func(b *testing.B) {
			benchmarkNumericStdlib[uint64](b, identifiers, identifierCount)
		})
	})
	b.Run("telemetry", func(b *testing.B) {
		b.Run("vibejson", func(b *testing.B) {
			benchmarkNumericVibeJSON[float64](b, telemetry, floatCount, DecoderOptions{})
		})
		b.Run("encoding-json", func(b *testing.B) {
			benchmarkNumericStdlib[float64](b, telemetry, floatCount)
		})
	})
	b.Run("coordinates", func(b *testing.B) {
		b.Run("vibejson", func(b *testing.B) {
			benchmarkNumericVibeJSON[float64](b, coordinates, floatCount, DecoderOptions{})
		})
		b.Run("encoding-json", func(b *testing.B) {
			benchmarkNumericStdlib[float64](b, coordinates, floatCount)
		})
	})
}

func BenchmarkDecodeMedium(b *testing.B) {
	src := benchRecordsJSON(32)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst benchDocument
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst benchDocument
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeReused(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeOneEscapedStringReused(b *testing.B) {
	benchmarkDecodeLargeOneDirtyStringReused(b, benchRecordsOneEscapedStringJSON(1024))
}

func BenchmarkDecodeLargeOneNonASCIIStringReused(b *testing.B) {
	benchmarkDecodeLargeOneDirtyStringReused(b, benchRecordsOneNonASCIIStringJSON(1024))
}

func benchmarkDecodeLargeOneDirtyStringReused(b *testing.B, src []byte) {
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalLargeReused(b *testing.B) {
	src := benchRecordsJSON(1024)
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeIndented(b *testing.B) {
	compact := benchRecordsJSON(1024)
	src, err := Indent(compact, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst benchDocument
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeIndentedReused(b *testing.B) {
	compact := benchRecordsJSON(1024)
	src, err := Indent(compact, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeIndentedRawReused(b *testing.B) {
	compact := benchRecordsJSON(1024)
	src, err := Indent(compact, "", "  ")
	if err != nil {
		b.Fatal(err)
	}
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		consumed, err := decoder.DecodePrefix(src, &dst)
		if err != nil {
			b.Fatal(err)
		}
		if consumed != len(src) {
			b.Fatalf("consumed %d of %d bytes", consumed, len(src))
		}
	}
}

func BenchmarkDecodeLargeOwned(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst benchDocument
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalAnyLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := Unmarshal(src, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeAnyLarge measures the dynamic engine through a compiled
// Decoder[any], without Unmarshal's per-call plan-cache lookup.
func BenchmarkDecodeAnyLarge(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	src := benchRecordsJSON(1024)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := decoder.Decode(src, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkParseLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if _, err := ParseOptions(src, Options{ZeroCopy: true}); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeLargeShuffledKeys(b *testing.B) {
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	for _, workload := range []struct {
		name          string
		distantEscape bool
	}{
		{name: "clean"},
		{name: "distant-escape", distantEscape: true},
	} {
		b.Run(workload.name, func(b *testing.B) {
			src := benchRecordsShuffledKeysJSON(1024, workload.distantEscape)
			dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
			b.SetBytes(int64(len(src)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if err := decoder.Decode(src, &dst); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkBuildIndexLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	needed, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, needed)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := BuildIndex(src, storage); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

type benchUntaggedRecord struct {
	ID      int
	Active  bool
	Name    string
	Message string
	Scores  [3]float64
}

type benchUntaggedDocument struct {
	Items []benchUntaggedRecord
	Meta  benchMeta `json:"meta"`
}

func BenchmarkDecodeLargeUntagged(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchUntaggedDocument](DecoderOptions{ZeroCopy: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst benchUntaggedDocument
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var doc benchDocument
	if err := decoder.Decode(src, &doc); err != nil {
		b.Fatal(err)
	}
	encoder, err := CompileEncoder[benchDocument](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	out, err := encoder.AppendJSON(nil, &doc)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var encodeErr error
		out, encodeErr = encoder.AppendJSON(out[:0], &doc)
		if encodeErr != nil {
			b.Fatal(encodeErr)
		}
	}
}

func BenchmarkEncodeMap(b *testing.B) {
	value := map[string]int{
		"alpha": 1, "bravo": 2, "charlie": 3, "delta": 4,
		"echo": 5, "foxtrot": 6, "golf": 7, "hotel": 8,
	}
	encoder, err := CompileEncoder[map[string]int](EncoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	buffer, err := encoder.AppendJSON(nil, &value)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(buffer)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		buffer, err = encoder.AppendJSON(buffer[:0], &value)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEncodeLargeStdlib(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var doc benchDocument
	if err := decoder.Decode(src, &doc); err != nil {
		b.Fatal(err)
	}
	out, err := json.Marshal(&doc)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := json.Marshal(&doc); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidMedium(b *testing.B) {
	src := benchRecordsJSON(32)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkUnmarshalAnyMedium(b *testing.B) {
	src := benchRecordsJSON(32)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := Unmarshal(src, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeAnyMedium is the compiled-decoder counterpart of
// BenchmarkUnmarshalAnyMedium (see BenchmarkDecodeAnyLarge).
func BenchmarkDecodeAnyMedium(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	src := benchRecordsJSON(32)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := decoder.Decode(src, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalAnySmall(b *testing.B) {
	b.SetBytes(int64(len(benchSmallJSON)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := Unmarshal(benchSmallJSON, &v); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDecodeAnySmall is the compiled-decoder counterpart of
// BenchmarkUnmarshalAnySmall; on a document this small the plan-cache lookup
// is a visible fraction, so the pair separates engine from entry point.
func BenchmarkDecodeAnySmall(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchSmallJSON)))
	b.ReportAllocs()
	for range b.N {
		var v any
		if err := decoder.Decode(benchSmallJSON, &v); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUnmarshalSmall(b *testing.B) {
	var warm benchSmall
	if err := Unmarshal(benchSmallJSON, &warm); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchSmallJSON)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		var dst benchSmall
		if err := Unmarshal(benchSmallJSON, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalSmall(b *testing.B) {
	value := benchSmall{ID: 1, OK: true, Name: "sim"}
	out, err := Marshal(&value)
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(out)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := Marshal(&value); err != nil {
			b.Fatal(err)
		}
	}
}
