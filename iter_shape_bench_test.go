package simdjson

// Head-to-head measurement of the two iterator shapes on corpus-shaped
// indexes: the mutating Next family against the value-threaded
// Advance/Current family, over identical documents and identical per-element
// work. Run both prefixes with -count and benchstat the extracted columns.

import (
	"strconv"
	"testing"
)

// iterShapeItems is an API-response-shaped array: many small objects, so the
// general ArrayIter hops multi-entry subtrees.
func iterShapeItems() []byte {
	dst := []byte{'['}
	for i := 0; i < 256; i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `{"id":`...)
		dst = strconv.AppendInt(dst, int64(100000+i), 10)
		dst = append(dst, `,"sku":"SKU-`...)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, `","price":`...)
		dst = strconv.AppendInt(dst, int64(1+i%97), 10)
		dst = append(dst, '.')
		dst = strconv.AppendInt(dst, int64(i%100), 10)
		dst = append(dst, `,"active":`...)
		if i%3 == 0 {
			dst = append(dst, `true`...)
		} else {
			dst = append(dst, `false`...)
		}
		dst = append(dst, '}')
	}
	return append(dst, ']')
}

// iterShapeRecord is one record-shaped object whose values include nested
// containers, so the general ObjectIter hops multi-entry subtrees.
func iterShapeRecord() []byte {
	dst := []byte{'{'}
	for i := 0; i < 64; i++ {
		if i > 0 {
			dst = append(dst, ',')
		}
		dst = append(dst, `"field`...)
		dst = strconv.AppendInt(dst, int64(i), 10)
		dst = append(dst, `":`...)
		switch i % 4 {
		case 0:
			dst = strconv.AppendInt(dst, int64(i*37), 10)
		case 1:
			dst = append(dst, `"value-`...)
			dst = strconv.AppendInt(dst, int64(i), 10)
			dst = append(dst, '"')
		case 2:
			dst = append(dst, `[1,2,3]`...)
		default:
			dst = append(dst, `{"a":1,"b":2}`...)
		}
	}
	return append(dst, '}')
}

func iterShapeIndex(b *testing.B, src []byte) Node {
	b.Helper()
	storage := make([]IndexEntry, len(src))
	tape, err := BuildIndex(src, storage)
	if err != nil {
		b.Fatal(err)
	}
	return tape.Root()
}

func BenchmarkIterShapeNext(b *testing.B) {
	b.Run("Array", func(b *testing.B) {
		root := iterShapeIndex(b, iterShapeItems())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			iter, ok := root.ArrayIter()
			if !ok {
				b.Fatal("not an array")
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
	})
	b.Run("Object", func(b *testing.B) {
		root := iterShapeIndex(b, iterShapeRecord())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			iter, ok := root.ObjectIter()
			if !ok {
				b.Fatal("not an object")
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
	})
}

func BenchmarkIterShapeAdvance(b *testing.B) {
	b.Run("Array", func(b *testing.B) {
		root := iterShapeIndex(b, iterShapeItems())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			iter, ok := root.ArrayIter()
			if !ok {
				b.Fatal("not an array")
			}
			total := 0
			for ; iter.Valid(); iter = iter.Advance() {
				total += int(iter.CurrentKind())
			}
			indexBenchmarkSink = total
		}
	})
	b.Run("Object", func(b *testing.B) {
		root := iterShapeIndex(b, iterShapeRecord())
		b.ReportAllocs()
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			iter, ok := root.ObjectIter()
			if !ok {
				b.Fatal("not an object")
			}
			total := 0
			for ; iter.Valid(); iter = iter.Advance() {
				key, value := iter.Current()
				total += int(key.Kind()) + int(value.Kind())
			}
			indexBenchmarkSink = total
		}
	})
}
