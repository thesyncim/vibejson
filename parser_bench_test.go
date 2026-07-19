package simdjson

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
		if !ValidNumber(src) {
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
		if !ValidString(src) {
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
	src := benchmarkJSON()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		b.Fatal(err)
	}
	storage := make([]IndexEntry, count)
	tape, err := BuildIndex(src, storage)
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

func BenchmarkIndexArrayCursorKind1024(b *testing.B) {
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
			b.Fatal("root is not an array")
		}
		total := 0
		for iter.Valid() {
			total += int(iter.CurrentKind())
			iter = iter.Advance()
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

func BenchmarkIndexObjectCursor1024(b *testing.B) {
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
		for iter.Valid() {
			key, value := iter.Current()
			total += int(key.Kind()) + int(value.Kind())
			iter = iter.Advance()
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
	case Number:
		f, _ := n.Float64()
		return f
	case Array:
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
	case Object:
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
		if err != nil || !ok || v.Kind() != String {
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
		if err != nil || !ok || v.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != Number {
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
		if err != nil || !ok || raw.Kind() != Number {
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
		if err != nil || !ok || raw.Kind() != Number {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
		if err != nil || !ok || raw.Kind() != String {
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
