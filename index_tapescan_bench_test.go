package slopjson

import (
	"fmt"
	"strings"
	"testing"
	"unsafe"

	"github.com/thesyncim/slopjson/document"
	"github.com/thesyncim/slopjson/internal/byteview"
)

// Three lookup strategies compete over one flat enriched object: the linear
// forward hash scan (the pre-kernel getHashed loop, kept here as the
// baseline), the vectorized tape scan, and the ObjectProbe hash table. The
// benchmarks report the per-query cost of each at increasing widths and the
// probe's build price, which together set the crossover: the scan wins while
// (linear - probe) per-query savings have not yet repaid the build.

// scanLinearRef is the forward scalar flat loop getHashed used before the
// kernel: full scan, hash pre-filter, last duplicate wins.
func scanLinearRef(src *byte, header *IndexEntry, count int, key string, queryHash uint32) *IndexEntry {
	var found *IndexEntry
	for member := 0; member < count; member++ {
		keyEntry := tapeEntryOffset(header, uintptr(2*member)+1)
		flags := keyEntry.flags()
		if flags&tapeFlagEscaped == 0 && keyEntry.next != queryHash {
			continue
		}
		if tapeKeyEqual(byteview.SliceRange(src, keyEntry.start, keyEntry.end), flags, key) {
			found = tapeEntryOffset(keyEntry, 1)
		}
	}
	return found
}

// flatObjectDoc builds a flat object of width members, keys f0000..fNNNN with
// integer values, every value one entry so the object stays flat.
func flatObjectDoc(width int) []byte {
	var sb strings.Builder
	sb.WriteString("{")
	for i := 0; i < width; i++ {
		if i > 0 {
			sb.WriteString(",")
		}
		fmt.Fprintf(&sb, `"f%04d":%d`, i, i)
	}
	sb.WriteString("}")
	return []byte(sb.String())
}

var tapeScanBenchSink *IndexEntry

func BenchmarkTapeScanFlat(b *testing.B) {
	kernels := []struct {
		name string
		scan func(src *byte, header *IndexEntry, count int, key string, queryHash uint32) *IndexEntry
	}{
		{"linear", scanLinearRef},
		{"scan", tapeScanFlatHash},
	}
	for _, width := range []int{8, 32, 128, 512} {
		src := flatObjectDoc(width)
		tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)), document.IndexOptions{HashKeys: true})
		if err != nil {
			b.Fatal(err)
		}
		root := tape.Root()
		probe, ok := BuildObjectProbe(root, make([]ProbeSlot, RequiredProbeSlots(root)))
		if !ok {
			b.Fatal("probe build declined")
		}
		positions := []struct {
			name string
			key  string
			want bool
		}{
			{"first", "f0000", true},
			{"mid", fmt.Sprintf("f%04d", width/2), true},
			{"last", fmt.Sprintf("f%04d", width-1), true},
			{"miss", "zzzz_absent", false},
		}
		for _, pos := range positions {
			for _, kernel := range kernels {
				b.Run(fmt.Sprintf("w=%d/pos=%s/kind=%s", width, pos.name, kernel.name), func(b *testing.B) {
					b.ReportAllocs()
					for i := 0; i < b.N; i++ {
						value := kernel.scan(root.src, root.entry, width, pos.key, hashKeyString(pos.key))
						if (value != nil) != pos.want {
							b.Fatal("unexpected lookup verdict")
						}
						tapeScanBenchSink = value
					}
				})
			}
			b.Run(fmt.Sprintf("w=%d/pos=%s/kind=probe", width, pos.name), func(b *testing.B) {
				b.ReportAllocs()
				for i := 0; i < b.N; i++ {
					value, ok := probe.Get(pos.key)
					if ok != pos.want {
						b.Fatal("unexpected lookup verdict")
					}
					tapeScanBenchSink = value.entry
				}
			})
		}
	}
}

// BenchmarkTapeScanProbeBuild prices BuildObjectProbe at the scan benchmark
// widths; build cost divided by the probe's per-query saving over the scan is
// the query count where the probe overtakes it.
func BenchmarkTapeScanProbeBuild(b *testing.B) {
	for _, width := range []int{8, 32, 128, 512} {
		src := flatObjectDoc(width)
		tape, err := BuildIndexOptions(src, make([]IndexEntry, len(src)), document.IndexOptions{HashKeys: true})
		if err != nil {
			b.Fatal(err)
		}
		root := tape.Root()
		storage := make([]ProbeSlot, RequiredProbeSlots(root))
		b.Run(fmt.Sprintf("w=%d", width), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				probe, ok := BuildObjectProbe(root, storage)
				if !ok {
					b.Fatal("build declined")
				}
				tapeScanBenchSink = probe.header
			}
		})
	}
}

// columnArrayDoc builds an array of elems flat objects of width members each,
// keys c0000.. with integer values.
func columnArrayDoc(elems, width int) []byte {
	var sb strings.Builder
	sb.WriteString("[")
	for e := 0; e < elems; e++ {
		if e > 0 {
			sb.WriteString(",")
		}
		sb.WriteString("{")
		for i := 0; i < width; i++ {
			if i > 0 {
				sb.WriteString(",")
			}
			fmt.Fprintf(&sb, `"c%04d":%d`, i, e*width+i)
		}
		sb.WriteString("}")
	}
	sb.WriteString("]")
	return []byte(sb.String())
}

// appendColumnLoop is the per-element baseline a caller writes today: iterate
// the array and Get each element, rehashing the key per element.
func appendColumnLoop(dst []RawValue, v Node, key string) []RawValue {
	iter, _ := v.ArrayIter()
	for {
		elem, ok := iter.Next()
		if !ok {
			return dst
		}
		if value, found := elem.Get(key); found {
			dst = append(dst, value.Raw())
			continue
		}
		dst = append(dst, RawValue{})
	}
}

func BenchmarkAppendColumn(b *testing.B) {
	for _, elems := range []int{64, 1024, 16384} {
		for _, width := range []int{4, 16, 64} {
			src := columnArrayDoc(elems, width)
			need, err := RequiredIndexEntries(src)
			if err != nil {
				b.Fatal(err)
			}
			tape, err := BuildIndexOptions(src, make([]IndexEntry, need), document.IndexOptions{HashKeys: true})
			if err != nil {
				b.Fatal(err)
			}
			root := tape.Root()
			// The gather streams the array's whole entry span; its byte size
			// is the effective-bandwidth denominator.
			spanBytes := int64(root.entry.next) * int64(unsafe.Sizeof(IndexEntry{}))
			dst := make([]RawValue, 0, elems)
			for _, q := range []struct{ name, key string }{
				{"hit", fmt.Sprintf("c%04d", width/2)},
				{"miss", "zzzz_absent"},
			} {
				b.Run(fmt.Sprintf("elems=%d/w=%d/%s/kind=column", elems, width, q.name), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(spanBytes)
					for i := 0; i < b.N; i++ {
						column, ok := root.AppendColumn(dst[:0], q.key)
						if !ok {
							b.Fatal("AppendColumn declined an array")
						}
						dst = column
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(elems), "ns/elem")
				})
				b.Run(fmt.Sprintf("elems=%d/w=%d/%s/kind=loop", elems, width, q.name), func(b *testing.B) {
					b.ReportAllocs()
					b.SetBytes(spanBytes)
					for i := 0; i < b.N; i++ {
						dst = appendColumnLoop(dst[:0], root, q.key)
					}
					b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N)/float64(elems), "ns/elem")
				})
			}
		}
	}
}
