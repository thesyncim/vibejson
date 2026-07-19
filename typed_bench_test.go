package simdjson

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

var benchUint64SliceSink []uint64

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

func benchRecordsOneEscapedStringJSON(count int) []byte {
	src := benchRecordsJSON(count)
	clean := []byte(`"message":"plain ascii payload sized to exercise vector scanners"`)
	dirty := []byte(`"message":"plain\nascii payload sized to exercise vector scanners"`)
	return bytes.Replace(src, clean, dirty, 1)
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
	var source strings.Builder
	source.Grow(count*17 + 2)
	source.WriteByte('[')
	for i := range count {
		if i != 0 {
			source.WriteByte(',')
		}
		source.WriteString(strconv.FormatUint(1_000_000_000_000_000+uint64(i), 10))
	}
	source.WriteByte(']')
	src := []byte(source.String())

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
	src := benchRecordsOneEscapedStringJSON(1024)
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
	// Same schema as benchRecordsJSON, but record members are rotated so the
	// JSON order never matches the struct field order.
	var out strings.Builder
	count := 1024
	out.Grow(count * 128)
	out.WriteString(`{"items":[`)
	for i := 0; i < count; i++ {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(`{"message":"plain ascii payload sized to exercise vector scanners","scores":[1,2.5,-3e4],"id":`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`,"active":true,"name":"record-`)
		out.WriteString(strconv.Itoa(i))
		out.WriteString(`"}`)
	}
	out.WriteString(`],"meta":{"count":`)
	out.WriteString(strconv.Itoa(count))
	out.WriteString(`,"source":"benchmark"}}`)
	src := []byte(out.String())

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
