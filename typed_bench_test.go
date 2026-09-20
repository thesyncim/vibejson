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

func benchmarkDecodeFresh[T any](b *testing.B, decoder Decoder[T], src []byte) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst T
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDecodeReused[T any](b *testing.B, decoder Decoder[T], src []byte, dst *T) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkDecodePrefixReused[T any](b *testing.B, decoder Decoder[T], src []byte, dst *T) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		consumed, err := decoder.DecodePrefix(src, dst)
		if err != nil {
			b.Fatal(err)
		}
		if consumed != len(src) {
			b.Fatalf("consumed %d of %d bytes", consumed, len(src))
		}
	}
}

func benchmarkUnmarshalFresh[T any](b *testing.B, src []byte) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		var dst T
		if err := Unmarshal(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkUnmarshalReused[T any](b *testing.B, src []byte, dst *T) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := Unmarshal(src, dst); err != nil {
			b.Fatal(err)
		}
	}
}

func benchmarkValid(b *testing.B, src []byte) {
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
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
	benchmarkDecodeFresh(b, decoder, benchSmallJSON)
}

func BenchmarkDecodeMapReused(b *testing.B) {
	decoder, err := CompileDecoder[map[string]int](DecoderOptions{CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	src := []byte(`{"alpha":1,"bravo":2,"charlie":3,"delta":4,"echo":5,"foxtrot":6,"golf":7,"hotel":8}`)
	dst := make(map[string]int, 8)
	benchmarkDecodeReused(b, decoder, src, &dst)
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
	benchmarkDecodeFresh(b, decoder, src)
}

func BenchmarkDecodeLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	benchmarkDecodeFresh(b, decoder, src)
}

func BenchmarkDecodeLargeReused(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	benchmarkDecodeReused(b, decoder, src, &dst)
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
	benchmarkDecodeReused(b, decoder, src, &dst)
}

func BenchmarkUnmarshalLargeReused(b *testing.B) {
	src := benchRecordsJSON(1024)
	dst := benchDocument{Items: make([]benchRecord, 0, 1024)}
	benchmarkUnmarshalReused(b, src, &dst)
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
	benchmarkDecodeFresh(b, decoder, src)
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
	benchmarkDecodeReused(b, decoder, src, &dst)
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
	benchmarkDecodePrefixReused(b, decoder, src, &dst)
}

func BenchmarkDecodeLargeOwned(b *testing.B) {
	src := benchRecordsJSON(1024)
	decoder, err := CompileDecoder[benchDocument](DecoderOptions{CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	benchmarkDecodeFresh(b, decoder, src)
}

func BenchmarkUnmarshalAnyLarge(b *testing.B) {
	src := benchRecordsJSON(1024)
	benchmarkUnmarshalFresh[any](b, src)
}

func BenchmarkDecodeAnyLarge(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	src := benchRecordsJSON(1024)
	benchmarkDecodeFresh(b, decoder, src)
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
			benchmarkDecodeReused(b, decoder, src, &dst)
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
	benchmarkValid(b, src)
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
	benchmarkDecodeFresh(b, decoder, src)
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
	benchmarkValid(b, src)
}

func BenchmarkUnmarshalAnyMedium(b *testing.B) {
	src := benchRecordsJSON(32)
	benchmarkUnmarshalFresh[any](b, src)
}

func BenchmarkDecodeAnyMedium(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	src := benchRecordsJSON(32)
	benchmarkDecodeFresh(b, decoder, src)
}

func BenchmarkUnmarshalAnySmall(b *testing.B) {
	benchmarkUnmarshalFresh[any](b, benchSmallJSON)
}

func BenchmarkDecodeAnySmall(b *testing.B) {
	decoder, err := CompileDecoder[any](DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	benchmarkDecodeFresh(b, decoder, benchSmallJSON)
}

func BenchmarkUnmarshalSmall(b *testing.B) {
	var warm benchSmall
	if err := Unmarshal(benchSmallJSON, &warm); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	benchmarkUnmarshalFresh[benchSmall](b, benchSmallJSON)
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

func BenchmarkNumericSliceStorage(b *testing.B) {
	type signed int64
	type unsigned uint64
	type floating float64
	b.Run("int64", func(b *testing.B) { benchmarkNumericSliceStorage[int64](b, intArrayJSON(256)) })
	b.Run("uint64", func(b *testing.B) {
		benchmarkNumericSliceStorage[uint64](b, bytes.ReplaceAll(intArrayJSON(256), []byte("-"), nil))
	})
	b.Run("float64", func(b *testing.B) { benchmarkNumericSliceStorage[float64](b, floatArrayJSON(256)) })
	b.Run("named-int64", func(b *testing.B) { benchmarkNumericSliceStorage[signed](b, intArrayJSON(256)) })
	b.Run("named-uint64", func(b *testing.B) {
		benchmarkNumericSliceStorage[unsigned](b, bytes.ReplaceAll(intArrayJSON(256), []byte("-"), nil))
	})
	b.Run("named-float64", func(b *testing.B) { benchmarkNumericSliceStorage[floating](b, floatArrayJSON(256)) })
}

func benchmarkNumericSliceStorage[T any](b *testing.B, src []byte) {
	b.Run("Decode", func(b *testing.B) {
		decoder, err := CompileDecoder[[]T](DecoderOptions{})
		if err != nil {
			b.Fatal(err)
		}
		var dst []T
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			if err := decoder.Decode(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("DecodeArray", func(b *testing.B) {
		decoder, err := CompileDecoder[T](DecoderOptions{})
		if err != nil {
			b.Fatal(err)
		}
		dst, err := decoder.DecodeArray(src, nil)
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			dst, err = decoder.DecodeArray(src, dst)
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("Fresh", func(b *testing.B) {
		decoder, err := CompileDecoder[[]T](DecoderOptions{})
		if err != nil {
			b.Fatal(err)
		}
		b.ReportAllocs()
		b.SetBytes(int64(len(src)))
		for b.Loop() {
			var dst []T
			if err := decoder.Decode(src, &dst); err != nil {
				b.Fatal(err)
			}
		}
	})
}

func BenchmarkContainerDecode(b *testing.B) {
	b.Run("int64-array", func(b *testing.B) {
		benchmarkContainerDecode[[32]int64](b, []byte(`[`+strings.Repeat(`123,`, 31)+`123]`))
	})
	b.Run("uint32-array", func(b *testing.B) {
		benchmarkContainerDecode[[32]uint32](b, []byte(`[`+strings.Repeat(`123,`, 31)+`123]`))
	})
	b.Run("bool-array", func(b *testing.B) {
		benchmarkContainerDecode[[32]bool](b, []byte(`[`+strings.Repeat(`true,`, 31)+`true]`))
	})
	b.Run("string-array", func(b *testing.B) {
		benchmarkContainerDecode[[32]string](b, []byte(`[`+strings.Repeat(`"hello",`, 31)+`"hello"]`))
	})
	const record = `{"id":1,"ok":true,"name":"sample"}`
	records := []byte(`[` + strings.Repeat(record+`,`, 31) + record + `]`)
	b.Run("record-array", func(b *testing.B) { benchmarkContainerDecode[[32]benchSmall](b, records) })
	b.Run("record-slice", func(b *testing.B) { benchmarkContainerDecode[[]benchSmall](b, records) })
	b.Run("pointer-array", func(b *testing.B) { benchmarkContainerDecode[[32]*benchSmall](b, records) })
	b.Run("pointer-slice", func(b *testing.B) { benchmarkContainerDecode[[]*benchSmall](b, records) })
	b.Run("pointer-map", func(b *testing.B) {
		benchmarkContainerDecode[map[string]*benchSmall](b, []byte(`{"a":`+record+`,"b":`+record+`}`))
	})
}

func benchmarkContainerDecode[T any](b *testing.B, src []byte) {
	decoder, err := CompileDecoder[T](DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	var dst T
	if err := decoder.Decode(src, &dst); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for b.Loop() {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDynamicCodec(b *testing.B) {
	type record struct {
		ID    int            `json:"id"`
		Extra map[string]int `json:",inline"`
	}
	for _, inline := range []bool{false, true} {
		name := "default"
		if inline {
			name = "inline"
		}
		b.Run(name, func(b *testing.B) {
			for _, item := range []struct {
				name  string
				value any
			}{
				{"scalar", int64(42)},
				{"record", record{ID: 1, Extra: map[string]int{"x": 2}}},
				{"map", map[string]int{"a": 1, "b": 2}},
			} {
				b.Run("encode-"+item.name, func(b *testing.B) {
					encoder, err := CompileEncoder[any](EncoderOptions{InlineFields: inline})
					if err != nil {
						b.Fatal(err)
					}
					value := item.value
					dst, err := encoder.AppendJSON(nil, &value)
					if err != nil {
						b.Fatal(err)
					}
					b.ReportAllocs()
					for b.Loop() {
						dst, err = encoder.AppendJSON(dst[:0], &value)
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
			b.Run("decode-pointer", func(b *testing.B) {
				decoder, err := CompileDecoder[any](DecoderOptions{InlineFields: inline})
				if err != nil {
					b.Fatal(err)
				}
				var value any = &record{Extra: make(map[string]int)}
				src := []byte(`{"id":1,"x":2}`)
				if err := decoder.Decode(src, &value); err != nil {
					b.Fatal(err)
				}
				b.ReportAllocs()
				for b.Loop() {
					if err := decoder.Decode(src, &value); err != nil {
						b.Fatal(err)
					}
				}
			})
		})
	}
}
