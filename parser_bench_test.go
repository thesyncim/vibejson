package slopjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/thesyncim/slopjson/document"
)

// flatNumberArray1024 is the 1024-element flat number array shared by the
// index-iteration benchmarks: "[0,0,...,0]" with 1024 numbers.
func flatNumberArray1024() []byte {
	return []byte("[" + strings.Repeat("0,", 1023) + "0]")
}

// flatObject1024 is the 1024-member flat object shared by the object-cursor
// benchmarks: {"a":0,"a":0,...} with 1024 members.
func flatObject1024() []byte {
	return []byte("{" + strings.Repeat(`"a":0,`, 1023) + `"a":0}`)
}

func BenchmarkValid(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValid(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidNumber(b *testing.B) {
	src := []byte(`-12.34e+56`)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !validNumber(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValidNumber(b *testing.B) {
	src := []byte(`-12.34e+56`)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidString(b *testing.B) {
	src := []byte(`"plain ascii string"`)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !validString(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValidString(b *testing.B) {
	src := []byte(`"plain ascii string"`)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidSmall(b *testing.B) {
	src := smallJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValidSmall(b *testing.B) {
	src := smallJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidLongString(b *testing.B) {
	src := longStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValidLongString(b *testing.B) {
	src := longStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkValidLongUnicodeString(b *testing.B) {
	src := longUnicodeStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkStdlibValidLongUnicodeString(b *testing.B) {
	src := longUnicodeStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !json.Valid(src) {
			b.Fatal("invalid")
		}
	}
}

func BenchmarkParse(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := Parse(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStdlibUnmarshal(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var v any
		if err := json.Unmarshal(src, &v); err != nil {
			b.Fatal(err)
		}
		benchmarkSink = v
	}
}

func BenchmarkParseOptionsZeroCopy(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseOptionsZeroCopyForTest(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkBuildIndex(b *testing.B) {
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tape, err := BuildIndex(src, storage)
		if err != nil {
			b.Fatal(err)
		}
		indexBenchmarkSink = tape.Len()
	}
}

// BenchmarkBuildIndexHashKeys measures the isolated cost of the opt-in
// key-hash enrichment pass against BenchmarkBuildIndex's default build.
func BenchmarkBuildIndexHashKeys(b *testing.B) {
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	opts := document.IndexOptions{HashKeys: true}
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tape, err := BuildIndexOptions(src, storage, opts)
		if err != nil {
			b.Fatal(err)
		}
		indexBenchmarkSink = tape.Len()
	}
}

// BenchmarkBuildIndexStringPolicy keeps short-string routing honest at the
// document-size boundary and on the decline paths that resume full validation.
func BenchmarkBuildIndexStringPolicy(b *testing.B) {
	denseClean := func(totalBytes int) []byte {
		prefix := `[` + strings.Repeat(`"abcdefghijklm",`, 63) + `"`
		suffix := `"]`
		return []byte(prefix + strings.Repeat("x", totalBytes-len(prefix)-len(suffix)) + suffix)
	}
	longClean := func(totalBytes int) []byte {
		return []byte(`"` + strings.Repeat("a", totalBytes-2) + `"`)
	}
	repeatedSpecial := func(special string) []byte {
		return []byte(`[` + strings.Repeat(`"abcdefghij`+special+`",`, 67) + `"abcdefghijklm` + special + `"]`)
	}
	lateEscape := func(totalBytes int) []byte {
		return []byte(`"` + strings.Repeat("a", totalBytes-4) + `\n"`)
	}

	cases := []struct {
		name  string
		bytes int
		src   []byte
	}{
		{name: "DenseClean/1023B", bytes: 1023, src: denseClean(1023)},
		{name: "DenseClean/1024B", bytes: 1024, src: denseClean(1024)},
		{name: "DenseClean/1025B", bytes: 1025, src: denseClean(1025)},
		{name: "LongClean/1024B", bytes: 1024, src: longClean(1024)},
		{name: "RepeatedEscaped/1024B", bytes: 1024, src: repeatedSpecial(`\n`)},
		{name: "RepeatedNonASCII/1024B", bytes: 1024, src: repeatedSpecial("é")},
		{name: "LateEscape/1024B", bytes: 1024, src: lateEscape(1024)},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			if len(tc.src) != tc.bytes {
				b.Fatalf("fixture is %d bytes, want %d", len(tc.src), tc.bytes)
			}
			count, err := RequiredIndexEntries(tc.src)
			if err != nil {
				b.Fatal(err)
			}
			storage := make([]IndexEntry, count)
			if _, err := BuildIndex(tc.src, storage); err != nil {
				b.Fatal(err)
			}
			b.SetBytes(int64(len(tc.src)))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				tape, err := BuildIndex(tc.src, storage)
				if err != nil {
					b.Fatal(err)
				}
				indexBenchmarkSink = tape.Len()
			}
		})
	}
}

func BenchmarkBuildIndexPointerCompiled(b *testing.B) {
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	pointer := MustCompilePointer("/items/2/message")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		tape, err := BuildIndex(src, storage)
		if err != nil {
			b.Fatal(err)
		}
		value, ok, err := tape.PointerCompiled(pointer)
		if err != nil || !ok {
			b.Fatal("pointer missing")
		}
		indexBenchmarkSink = len(value.Raw().Bytes())
	}
}

func BenchmarkBuildIndexPointerCompiledLookupOnly(b *testing.B) {
	benchmarkPointerCompiledLookupOnly(b, false)
}

// BenchmarkBuildIndexPointerCompiledLookupOnlyHashKeys is the enriched
// counterpart: pointer resolution walks object headers via Get, so it only
// benefits from precomputed key hashes when the index is built with them.
func BenchmarkBuildIndexPointerCompiledLookupOnlyHashKeys(b *testing.B) {
	benchmarkPointerCompiledLookupOnly(b, true)
}

func benchmarkPointerCompiledLookupOnly(b *testing.B, hashKeys bool) {
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	tape, err := BuildIndexOptions(src, storage, document.IndexOptions{HashKeys: hashKeys})
	if err != nil {
		b.Fatal(err)
	}
	pointer := MustCompilePointer("/items/2/message")
	value, ok, err := tape.PointerCompiled(pointer)
	if err != nil || !ok {
		b.Fatal("pointer missing")
	}
	indexBenchmarkSink = len(value.Raw().Bytes())

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok, err := tape.PointerCompiled(pointer)
		if err != nil || !ok {
			b.Fatal("pointer missing")
		}
		indexBenchmarkSink = len(value.Raw().Bytes())
	}
}

func BenchmarkIndexArrayIter4(b *testing.B) {
	src := rawArrayJSON()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		total := 0
		for {
			value, ok := iter.Next()
			if !ok {
				break
			}
			total += int(value.Kind())
		}
		indexBenchmarkSink = total
	}
}

func BenchmarkIndexArrayIter1024(b *testing.B) {
	src := flatNumberArray1024()
	storage := make([]IndexEntry, 1025)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		total := 0
		for {
			value, ok := iter.Next()
			if !ok {
				break
			}
			total += int(value.Kind())
		}
		indexBenchmarkSink = total
	}
}

func BenchmarkIndexArrayIterKind1024(b *testing.B) {
	src := flatNumberArray1024()
	storage := make([]IndexEntry, 1025)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		total := 0
		for {
			kind, ok := iter.NextKind()
			if !ok {
				break
			}
			total += int(kind)
		}
		indexBenchmarkSink = total
	}
}

func BenchmarkIndexArrayIterRaw1024(b *testing.B) {
	src := flatNumberArray1024()
	storage := make([]IndexEntry, 1025)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		total := 0
		for {
			raw, ok := iter.NextRaw()
			if !ok {
				break
			}
			total += len(raw.Bytes())
		}
		indexBenchmarkSink = total
	}
}

func BenchmarkIndexObjectIter(b *testing.B) {
	src := rawObjectJSON()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ObjectIter()
		if !ok {
			b.Fatal("root is not object")
		}
		total := 0
		for {
			key, value, ok := iter.Next()
			if !ok {
				break
			}
			total += int(key.Kind()) + int(value.Kind())
		}
		indexBenchmarkSink = total
	}
}

func BenchmarkIndexObjectIter1024(b *testing.B) {
	src := flatObject1024()
	storage := make([]IndexEntry, 2049)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ObjectIter()
		if !ok {
			b.Fatal("root is not an object")
		}
		total := 0
		for {
			key, value, ok := iter.Next()
			if !ok {
				break
			}
			total += int(key.Kind()) + int(value.Kind())
		}
		indexBenchmarkSink = total
	}
}

// BenchmarkIndexArrayIndexMid resolves a middle element of a large flat
// array by position, the shape numeric JSON Pointer walks take.
func BenchmarkIndexArrayIndexMid(b *testing.B) {
	src := intArrayJSON(8192)
	storage := make([]IndexEntry, 8193)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := root.Index(4096)
		if !ok {
			b.Fatal("element missing")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkIndexObjectGet1024 resolves the last of 1024 duplicate keys, the
// full-scan worst case for object lookup.
func BenchmarkIndexObjectGet1024(b *testing.B) {
	src := flatObject1024()
	storage := make([]IndexEntry, 2049)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := root.Get("a")
		if !ok {
			b.Fatal("key missing")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// wideObject32 is the 32-member wide-lookup fixture: distinct eight-byte keys
// with short string values, the shape the key-hash pre-filter targets.
func wideObject32() []byte {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < 32; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"field_` + string([]byte{'a' + byte(i/10), '0' + byte(i%10)}) + `":"value"`)
	}
	sb.WriteString("}")
	return []byte(sb.String())
}

// wideObject32Mixed is the mixed-length counterpart of wideObject32: 32
// distinct keys whose lengths cycle from four to eighteen bytes, the common
// shape in which most members differ from a query in length alone.
func wideObject32Mixed() []byte {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < 32; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		sb.WriteString(`"` + "key_with_padding"[:2+i%15] + string([]byte{'a' + byte(i/10), '0' + byte(i%10)}) + `":"value"`)
	}
	sb.WriteString("}")
	return []byte(sb.String())
}

// smallObject8 is the eight-member lookup fixture: a realistic small record
// with mixed-length keys and two container values, so the span-chased
// (non-flat) scan is measured alongside the flat one.
func smallObject8() []byte {
	return []byte(`{"id":184467,"name":"Aurelia Waters","email":"aurelia@example.com",` +
		`"created_at":"2026-07-19T08:30:00Z","active":true,"score":98.6,` +
		`"roles":["admin","editor"],"address":{"city":"Lisbon","zip":"1100"}}`)
}

func benchmarkIndexGet(b *testing.B, src []byte, key string, want, hashKeys bool) {
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndexOptions(src, storage, document.IndexOptions{HashKeys: hashKeys})
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := root.Get(key)
		if ok != want {
			b.Fatal("unexpected lookup verdict")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkIndexGetWide32Hit resolves a key near the end of a 32-member
// enriched object, the deep-scan hit case the hash gate accelerates.
func BenchmarkIndexGetWide32Hit(b *testing.B) {
	benchmarkIndexGet(b, wideObject32(), "field_d0", true, true)
}

// BenchmarkIndexGetWide32Miss scans all 32 members of an enriched object for
// an absent key, the full-miss case where the gate skips every byte compare.
func BenchmarkIndexGetWide32Miss(b *testing.B) {
	benchmarkIndexGet(b, wideObject32(), "field_zz", false, true)
}

// BenchmarkIndexGetWide32HitPlain is the unenriched hit baseline: the default
// build path must leave this lookup at baseline speed. Every key and both
// queries are eight bytes, so a length pre-filter can reject nothing here;
// this row bounds that filter's overhead.
func BenchmarkIndexGetWide32HitPlain(b *testing.B) {
	benchmarkIndexGet(b, wideObject32(), "field_d0", true, false)
}

// BenchmarkIndexGetWide32MissPlain is the unenriched full-miss baseline,
// again with every member the query's length.
func BenchmarkIndexGetWide32MissPlain(b *testing.B) {
	benchmarkIndexGet(b, wideObject32(), "field_zz", false, false)
}

// BenchmarkIndexGetWide32MixedHitPlain resolves the last member of an
// unenriched 32-member object with mixed-length keys, the default-path shape
// where most members differ from the query in length alone.
func BenchmarkIndexGetWide32MixedHitPlain(b *testing.B) {
	benchmarkIndexGet(b, wideObject32Mixed(), "keyd1", true, false)
}

// BenchmarkIndexGetWide32MixedMissPlain scans the same mixed-length object
// for an absent key whose length only two members share.
func BenchmarkIndexGetWide32MixedMissPlain(b *testing.B) {
	benchmarkIndexGet(b, wideObject32Mixed(), "no_such_key", false, false)
}

// BenchmarkIndexGetSmall8HitPlain resolves a late member of an unenriched
// eight-member record, the small-object shape default lookups see most.
func BenchmarkIndexGetSmall8HitPlain(b *testing.B) {
	benchmarkIndexGet(b, smallObject8(), "score", true, false)
}

// BenchmarkIndexGetSmall8MissPlain scans the eight-member record for an
// absent key.
func BenchmarkIndexGetSmall8MissPlain(b *testing.B) {
	benchmarkIndexGet(b, smallObject8(), "missing", false, false)
}

// wideObjectN is the parameterized wide-lookup fixture: n distinct ten-byte
// keys with short string values, the wideObject32 shape at probe-scale widths.
func wideObjectN(n int) []byte {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < n; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"field_%04d":"value"`, i)
	}
	sb.WriteString("}")
	return []byte(sb.String())
}

// buildWideEnriched builds the enriched index for one wide fixture and fails
// the benchmark on any build error.
func buildWideEnriched(b *testing.B, src []byte) Index {
	b.Helper()
	tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)), document.IndexOptions{HashKeys: true})
	if err != nil {
		b.Fatal(err)
	}
	return tape
}

func benchmarkIndexGetWide(b *testing.B, n int, key string, want bool) {
	root := buildWideEnriched(b, wideObjectN(n)).Root()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := root.Get(key)
		if ok != want {
			b.Fatal("unexpected lookup verdict")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

func benchmarkIndexGetCompiledWide32(b *testing.B, key string, want bool) {
	src := wideObject32()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndexOptions(src, storage, document.IndexOptions{HashKeys: true})
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	compiled := CompileKey(key)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := root.GetCompiled(compiled)
		if ok != want {
			b.Fatal("unexpected lookup verdict")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkIndexGetWide128Hit resolves a key near the end of a 128-member
// enriched object, the deep linear scan an ObjectProbe replaces.
func BenchmarkIndexGetWide128Hit(b *testing.B) {
	benchmarkIndexGetWide(b, 128, "field_0126", true)
}

// BenchmarkIndexGetWide128Miss scans all 128 members for an absent key.
func BenchmarkIndexGetWide128Miss(b *testing.B) {
	benchmarkIndexGetWide(b, 128, "field_9999", false)
}

// BenchmarkIndexGetWide512Hit resolves a key near the end of a 512-member
// enriched object.
func BenchmarkIndexGetWide512Hit(b *testing.B) {
	benchmarkIndexGetWide(b, 512, "field_0510", true)
}

// BenchmarkIndexGetWide512Miss scans all 512 members for an absent key.
func BenchmarkIndexGetWide512Miss(b *testing.B) {
	benchmarkIndexGetWide(b, 512, "field_9999", false)
}

func benchmarkObjectProbeGet(b *testing.B, src []byte, key string, want bool) {
	root := buildWideEnriched(b, src).Root()
	probe, ok := BuildObjectProbe(root, make([]ProbeSlot, RequiredProbeSlots(root)))
	if !ok {
		b.Fatal("build declined")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok := probe.Get(key)
		if ok != want {
			b.Fatal("unexpected lookup verdict")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkObjectProbeGet32Hit is the probe counterpart of
// BenchmarkIndexGetWide32Hit on the identical fixture and key.
func BenchmarkObjectProbeGet32Hit(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObject32(), "field_d0", true)
}

// BenchmarkObjectProbeGet32Miss is the probe counterpart of
// BenchmarkIndexGetWide32Miss.
func BenchmarkObjectProbeGet32Miss(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObject32(), "field_zz", false)
}

// BenchmarkObjectProbeGet128Hit is the probe counterpart of
// BenchmarkIndexGetWide128Hit on the identical fixture and key.
func BenchmarkObjectProbeGet128Hit(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObjectN(128), "field_0126", true)
}

// BenchmarkObjectProbeGet128Miss is the probe counterpart of
// BenchmarkIndexGetWide128Miss.
func BenchmarkObjectProbeGet128Miss(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObjectN(128), "field_9999", false)
}

// BenchmarkObjectProbeGet512Hit is the probe counterpart of
// BenchmarkIndexGetWide512Hit on the identical fixture and key.
func BenchmarkObjectProbeGet512Hit(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObjectN(512), "field_0510", true)
}

// BenchmarkObjectProbeGet512Miss is the probe counterpart of
// BenchmarkIndexGetWide512Miss.
func BenchmarkObjectProbeGet512Miss(b *testing.B) {
	benchmarkObjectProbeGet(b, wideObjectN(512), "field_9999", false)
}

func benchmarkBuildObjectProbe(b *testing.B, n int) {
	root := buildWideEnriched(b, wideObjectN(n)).Root()
	storage := make([]ProbeSlot, RequiredProbeSlots(root))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		probe, ok := BuildObjectProbe(root, storage)
		if !ok {
			b.Fatal("build declined")
		}
		indexBenchmarkSink = len(probe.table)
	}
}

// BenchmarkBuildObjectProbe32 prices building a probe over 32 members with
// exact caller storage on an enriched index; amortized against the per-query
// saving over the linear scan, it sets the probe's break-even query count.
func BenchmarkBuildObjectProbe32(b *testing.B) {
	benchmarkBuildObjectProbe(b, 32)
}

// BenchmarkBuildObjectProbe512 is the 512-member build cost.
func BenchmarkBuildObjectProbe512(b *testing.B) {
	benchmarkBuildObjectProbe(b, 512)
}

// BenchmarkIndexGetCompiledWide32Hit is BenchmarkIndexGetWide32Hit with the
// query hash precomputed; the delta against it is the saved rehash.
func BenchmarkIndexGetCompiledWide32Hit(b *testing.B) {
	benchmarkIndexGetCompiledWide32(b, "field_d0", true)
}

// BenchmarkIndexGetCompiledWide32Miss is BenchmarkIndexGetWide32Miss with the
// query hash precomputed.
func BenchmarkIndexGetCompiledWide32Miss(b *testing.B) {
	benchmarkIndexGetCompiledWide32(b, "field_zz", false)
}

func smallObject4() []byte {
	return []byte(`{"alpha":1,"beta":2,"gamma":3,"delta":4}`)
}

func benchmarkIndexGetSmall4(b *testing.B, compiled bool) {
	src := smallObject4()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	key := CompileKey("delta")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var value Node
		var ok bool
		if compiled {
			value, ok = root.GetCompiled(key)
		} else {
			value, ok = root.Get("delta")
		}
		if !ok {
			b.Fatal("key missing")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkIndexGetSmall4 resolves the last key of a four-member unenriched
// object, the shape where per-lookup dispatch overhead would show first.
func BenchmarkIndexGetSmall4(b *testing.B) {
	benchmarkIndexGetSmall4(b, false)
}

// BenchmarkIndexGetCompiledSmall4 is BenchmarkIndexGetSmall4 through a
// compiled key; on an unenriched object the two must stay at parity.
func BenchmarkIndexGetCompiledSmall4(b *testing.B) {
	benchmarkIndexGetSmall4(b, true)
}

func benchmarkIndexPointerCompiledWide32(b *testing.B, hashKeys bool) {
	src := wideObject32()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndexOptions(src, storage, document.IndexOptions{HashKeys: hashKeys})
	if err != nil {
		b.Fatal(err)
	}
	pointer := MustCompilePointer("/field_d0")
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		value, ok, err := tape.PointerCompiled(pointer)
		if err != nil || !ok {
			b.Fatal("pointer missing")
		}
		indexBenchmarkSink = int(value.Kind())
	}
}

// BenchmarkIndexPointerCompiledWide32 resolves one compiled pointer deep into
// an unenriched 32-member object, the repeated-lookup shape a query engine
// applies across documents.
func BenchmarkIndexPointerCompiledWide32(b *testing.B) {
	benchmarkIndexPointerCompiledWide32(b, false)
}

// BenchmarkIndexPointerCompiledWide32HashKeys is the enriched counterpart,
// where the pointer token's precomputed hash skips the per-document rehash.
func BenchmarkIndexPointerCompiledWide32HashKeys(b *testing.B) {
	benchmarkIndexPointerCompiledWide32(b, true)
}

var indexBenchmarkSinkInt64 int64

var indexBenchmarkSinkFloat64 float64

// BenchmarkIndexArrayInt64Sum reads every element of a mixed-width integer
// array through Node.Int64, the integer-heavy lazy read path.
func BenchmarkIndexArrayInt64Sum(b *testing.B) {
	src := intArrayJSON(8192)
	storage := make([]IndexEntry, 8193)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		var total int64
		for {
			value, ok := iter.Next()
			if !ok {
				break
			}
			n, ok := value.Int64()
			if !ok {
				b.Fatal("element is not an integer")
			}
			total += n
		}
		indexBenchmarkSinkInt64 = total
	}
}

// BenchmarkIndexArrayFloat64Sum reads the same integer array through
// Node.Float64, the path lazy full traversals take for numbers.
func BenchmarkIndexArrayFloat64Sum(b *testing.B) {
	src := intArrayJSON(8192)
	storage := make([]IndexEntry, 8193)
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	root := tape.Root()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		iter, ok := root.ArrayIter()
		if !ok {
			b.Fatal("root is not array")
		}
		var total float64
		for {
			value, ok := iter.Next()
			if !ok {
				break
			}
			f, ok := value.Float64()
			if !ok {
				b.Fatal("element is not a number")
			}
			total += f
		}
		indexBenchmarkSinkFloat64 = total
	}
}

// sumNodeFull walks a pre-parsed tape summing every number through
// Node.Float64, forcing the real-float scalar read path on every element.
func sumNodeFull(n Node) float64 {
	switch n.Kind() {
	case document.Number:
		f, _ := n.Float64()
		return f
	case document.Array:
		iter, _ := n.ArrayIter()
		var s float64
		for {
			el, ok := iter.Next()
			if !ok {
				break
			}
			s += sumNodeFull(el)
		}
		return s
	case document.Object:
		iter, _ := n.ObjectIter()
		var s float64
		for {
			_, val, ok := iter.Next()
			if !ok {
				break
			}
			s += sumNodeFull(val)
		}
		return s
	default:
		return 0
	}
}

// BenchmarkIndexFloatWalk reads every number of a real-float corpus through
// Node.Float64 over a pre-parsed tape, isolating the lazy scalar read (Parse is
// excluded). FloatArray is a flat mixed-magnitude array; CoordRings is the
// nested GeoJSON coordinate shape. Both route through the fraction/exponent
// kernel path this change added.
func BenchmarkIndexFloatWalk(b *testing.B) {
	for _, c := range []struct {
		name string
		data []byte
	}{
		{"FloatArray", floatArrayJSON(8192)},
		{"CoordRings", coordRingsJSON(4096)},
	} {
		storage := make([]IndexEntry, len(c.data))
		tape, err := BuildIndex(c.data, storage)
		if err != nil {
			b.Fatal(err)
		}
		root := tape.Root()
		b.Run(c.name, func(b *testing.B) {
			b.SetBytes(int64(len(c.data)))
			b.ReportAllocs()
			var s float64
			for i := 0; i < b.N; i++ {
				s += sumNodeFull(root)
			}
			indexBenchmarkSinkFloat64 = s
		})
	}
}

func BenchmarkParseOptionsZeroCopySmall(b *testing.B) {
	src := smallJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := parseOptionsZeroCopyForTest(src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkStdlibUnmarshalSmall(b *testing.B) {
	src := smallJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		var v any
		if err := json.Unmarshal(src, &v); err != nil {
			b.Fatal(err)
		}
		benchmarkSink = v
	}
}

func BenchmarkPointer(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		root, err := Parse(src)
		if err != nil {
			b.Fatal(err)
		}
		v, ok, err := root.Pointer("/items/2/message")
		if err != nil || !ok || v.Kind() != document.String {
			b.Fatal(v, ok, err)
		}
	}
}

func BenchmarkPointerZeroCopy(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		root, err := ParseOptions(src, Options{ZeroCopy: true})
		if err != nil {
			b.Fatal(err)
		}
		v, ok, err := root.Pointer("/items/2/message")
		if err != nil || !ok || v.Kind() != document.String {
			b.Fatal(v, ok, err)
		}
	}
}

func BenchmarkGetRaw(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := GetRaw(src, "/items/2/message")
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRaw(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ScanFirstRaw(src, "/items/2/message")
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawTrusted(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ScanFirstRawTrusted(src, "/items/2/message")
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkGetRawCompiled(b *testing.B) {
	src := benchmarkJSON()
	ptr := MustCompilePointer("/items/2/message")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ptr.GetRaw(src)
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawCompiled(b *testing.B) {
	src := benchmarkJSON()
	ptr := MustCompilePointer("/items/2/message")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ptr.ScanFirstRaw(src)
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawTrustedCompiled(b *testing.B) {
	src := benchmarkJSON()
	ptr := MustCompilePointer("/items/2/message")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ptr.ScanFirstRawTrusted(src)
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkGetRawEarly(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := GetRaw(src, "/items/0/id")
		if err != nil || !ok || raw.Kind() != document.Number {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawEarly(b *testing.B) {
	src := benchmarkJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ScanFirstRaw(src, "/items/0/id")
		if err != nil || !ok || raw.Kind() != document.Number {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawEarlyCompiled(b *testing.B) {
	src := benchmarkJSON()
	ptr := MustCompilePointer("/items/0/id")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ptr.ScanFirstRaw(src)
		if err != nil || !ok || raw.Kind() != document.Number {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkGetRawLongString(b *testing.B) {
	src := longStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := GetRaw(src, "/s")
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawLongString(b *testing.B) {
	src := longStringJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ScanFirstRaw(src, "/s")
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkScanFirstRawLongStringCompiled(b *testing.B) {
	src := longStringJSON()
	ptr := MustCompilePointer("/s")
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		raw, ok, err := ptr.ScanFirstRaw(src)
		if err != nil || !ok || raw.Kind() != document.String {
			b.Fatal(raw, ok, err)
		}
	}
}

func BenchmarkAppendCompact(b *testing.B) {
	src := benchmarkJSON()
	dst := make([]byte, 0, len(src))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out, err := AppendCompact(dst[:0], src)
		if err != nil {
			b.Fatal(err)
		}
		dst = out[:0]
	}
}

func BenchmarkStdlibCompact(b *testing.B) {
	src := benchmarkJSON()
	var dst bytes.Buffer
	dst.Grow(len(src))
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		dst.Reset()
		if err := json.Compact(&dst, src); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkEachArrayRaw(b *testing.B) {
	src := rawArrayJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		if err := EachArray(src, func(_ int, value RawValue) error {
			total += len(value.Bytes())
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	benchmarkSink = total
}

func BenchmarkStdlibArrayRawMessages(b *testing.B) {
	src := rawArrayJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	var total int
	for i := 0; i < b.N; i++ {
		var values []json.RawMessage
		if err := json.Unmarshal(src, &values); err != nil {
			b.Fatal(err)
		}
		for _, value := range values {
			total += len(value)
		}
	}
	benchmarkSink = total
}

func BenchmarkEachObjectRaw(b *testing.B) {
	src := rawObjectJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	total := 0
	for i := 0; i < b.N; i++ {
		if err := EachObject(src, func(key string, value RawValue) error {
			total += len(key) + len(value.Bytes())
			return nil
		}); err != nil {
			b.Fatal(err)
		}
	}
	benchmarkSink = total
}

func BenchmarkStdlibObjectRawMessages(b *testing.B) {
	src := rawObjectJSON()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	var total int
	for i := 0; i < b.N; i++ {
		var values map[string]json.RawMessage
		if err := json.Unmarshal(src, &values); err != nil {
			b.Fatal(err)
		}
		for key, value := range values {
			total += len(key) + len(value)
		}
	}
	benchmarkSink = total
}
