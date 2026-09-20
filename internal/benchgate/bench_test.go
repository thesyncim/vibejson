package benchgate

import (
	"bytes"
	"strconv"
	"strings"
	"testing"

	vibejson "github.com/thesyncim/vibejson"
)

type small struct {
	ID   int    `json:"id"`
	OK   bool   `json:"ok"`
	Name string `json:"name"`
}

type record struct {
	ID      int        `json:"id"`
	Active  bool       `json:"active"`
	Name    string     `json:"name"`
	Message string     `json:"message"`
	Scores  [3]float64 `json:"scores"`
}

type meta struct {
	Count  int    `json:"count"`
	Source string `json:"source"`
}

type document struct {
	Items []record `json:"items"`
	Meta  meta     `json:"meta"`
}

var (
	benchSmallJSON = []byte(`{"id":1,"ok":true,"name":"sim"}`)
	indexSink      int
)

func benchmarkJSON() []byte {
	return []byte(`{
		"items": [
			{"id": 1, "active": true, "message": "this is a long ASCII string that should make the SIMD scanner skip most bytes quickly"},
			{"id": 2, "active": false, "message": "another long string with escaped data\nand normal text around it"},
			{"id": 3, "active": true, "message": "the parser keeps object order and number spelling intact"}
		],
		"meta": {"source": "benchmark", "version": 1}
	}`)
}

func longStringJSON() []byte {
	return []byte(`{"s":"` + strings.Repeat("a", 4096) + `"}`)
}

func longUnicodeStringJSON() []byte {
	return []byte(`{"s":"` + strings.Repeat("こんにちは世界", 256) + `"}`)
}

func recordsJSON(count int) []byte {
	var out strings.Builder
	out.Grow(count * 128)
	out.WriteString(`{"items":[`)
	for i := range count {
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

func fixed16Uint64ArrayJSON(count int) []byte {
	var out strings.Builder
	out.Grow(count*17 + 2)
	out.WriteByte('[')
	for i := range count {
		if i != 0 {
			out.WriteByte(',')
		}
		out.WriteString(strconv.FormatUint(1_000_000_000_000_000+uint64(i), 10))
	}
	out.WriteByte(']')
	return []byte(out.String())
}

func BenchmarkValidLongString(b *testing.B) {
	src := longStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if !vibejson.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidLongUnicodeString(b *testing.B) {
	src := longUnicodeStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if !vibejson.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkBuildIndex(b *testing.B) {
	src := benchmarkJSON()
	count, err := vibejson.RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]vibejson.IndexEntry, count)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		index, err := vibejson.BuildIndex(src, storage)
		if err != nil {
			b.Fatal(err)
		}
		indexSink = index.Len()
	}
}

func BenchmarkDecodeSmall(b *testing.B) {
	decoder, err := vibejson.CompileDecoder[small](vibejson.DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(benchSmallJSON)))
	b.ReportAllocs()
	for range b.N {
		var dst small
		if err := decoder.Decode(benchSmallJSON, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkDecodeUint64Array16(b *testing.B) {
	const count = 1024
	src := fixed16Uint64ArrayJSON(count)
	b.Run("DecodeArray", func(b *testing.B) {
		decoder, err := vibejson.CompileDecoder[uint64](vibejson.DecoderOptions{Replace: true})
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
	})
	b.Run("Decode", func(b *testing.B) {
		decoder, err := vibejson.CompileDecoder[[]uint64](vibejson.DecoderOptions{Replace: true})
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
	})
}

func BenchmarkDecodeLargeReused(b *testing.B) {
	src := recordsJSON(1024)
	decoder, err := vibejson.CompileDecoder[document](vibejson.DecoderOptions{ZeroCopy: true, CaseSensitive: true})
	if err != nil {
		b.Fatal(err)
	}
	dst := document{Items: make([]record, 0, 1024)}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if err := decoder.Decode(src, &dst); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkValidLarge(b *testing.B) {
	src := recordsJSON(1024)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for range b.N {
		if !vibejson.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkEncodeLarge(b *testing.B) {
	src := recordsJSON(1024)
	decoder, err := vibejson.CompileDecoder[document](vibejson.DecoderOptions{})
	if err != nil {
		b.Fatal(err)
	}
	var doc document
	if err := decoder.Decode(src, &doc); err != nil {
		b.Fatal(err)
	}
	encoder, err := vibejson.CompileEncoder[document](vibejson.EncoderOptions{})
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
		out, err = encoder.AppendJSON(out[:0], &doc)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestCanonicalize(t *testing.T) {
	got, err := vibejson.Canonicalize([]byte(`{"b":1,"a":2}`))
	if err != nil || string(got) != `{"a":2,"b":1}` {
		t.Fatalf("Canonicalize = %q, %v", got, err)
	}
}

func TestDepthLimitAgreement(t *testing.T) {
	valid := []byte(`{"a":[1,2]}`)
	if !vibejson.Valid(valid) {
		t.Fatal("Valid rejected a shallow document")
	}
	if _, err := vibejson.Canonicalize(valid); err != nil {
		t.Fatal(err)
	}
	if vibejson.Valid([]byte(`{"a":[1,2]`)) {
		t.Fatal("Valid accepted an incomplete document")
	}
}

func TestStringDecodingVsStdlib(t *testing.T) {
	var got struct {
		Text string `json:"text"`
	}
	if err := vibejson.Unmarshal([]byte(`{"text":"a\u0062"}`), &got); err != nil {
		t.Fatal(err)
	}
	if got.Text != "ab" {
		t.Fatalf("decoded text = %q", got.Text)
	}
}

func TestValidLongStringFallback(t *testing.T) {
	if !vibejson.Valid(longStringJSON()) || vibejson.Valid([]byte(`{"s":"unterminated}`)) {
		t.Fatal("long-string validation boundary failed")
	}
}

func TestAppendDecodedJSONStringMalformedInputIsLossless(t *testing.T) {
	prefix := []byte("prefix:")
	got := vibejson.AppendDecodedJSONString(append([]byte(nil), prefix...), []byte(`\u0`))
	if !bytes.Equal(got, append(prefix, []byte(`\u0`)...)) {
		t.Fatalf("malformed escape changed destination: %q", got)
	}
}
