package durable

import (
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

// benchReadCorpusSize matches the competitive harness so a number measured
// here can be compared against the cross-engine report without re-deriving it.
const benchReadCorpusSize = 100_000

// benchReadDocument mirrors the competitive corpus shape: a few hundred bytes
// of mixed scalars, large enough that a document micro-page holds only a
// handful and the key directory therefore has real depth.
func benchReadDocument(i int) []byte {
	return fmt.Appendf(nil,
		`{"id":%d,"key":"user-%09d","name":"Person Number %d","email":"person%d@example.com",`+
			`"country":"%s","city":"City %d","score":%d.75,"active":%t,`+
			`"tags":["alpha","beta","gamma","delta"],"note":"%s"}`,
		i, i, i, i, benchReadCountries[i%len(benchReadCountries)], i%997, i%1000, i%3 == 0,
		"a padding note that brings this document into the two hundred byte range xxxxxxxxxxxxxxxx")
}

var benchReadCountries = []string{"PT", "ES", "FR", "DE", "IT", "NL", "SE", "NO", "FI", "DK"}

func benchReadKey(i int) string { return fmt.Sprintf("user-%09d", i) }

// openBenchReadCollection builds the corpus through the bulk path and reopens
// it, so the measured store is the one a caller gets from CreateFrom. The
// resident budget is deliberately larger than the file: this benchmark exists
// to measure the warm, no-I/O read path, and any miss would measure the device
// instead.
func openBenchReadCollection(tb testing.TB, count int) (*Collection, func()) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "read.vibe")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	for i := range count {
		if err := builder.Append(benchReadKey(i), benchReadDocument(i)); err != nil {
			tb.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		tb.Fatal(err)
	}
	options := Options{ResidentBytes: 64 << 20, Backend: BackendPortable}
	if _, err := CreateFrom(built, file, options); err != nil {
		tb.Fatal(err)
	}
	collection, err := Open(file, options)
	if err != nil {
		tb.Fatal(err)
	}
	return collection, func() {
		if closeErr := collection.Close(); closeErr != nil {
			tb.Fatal(closeErr)
		}
		_ = file.Close()
	}
}

// benchReadProbeOrder is a fixed permutation so point reads do not walk the
// store in storage order and get an unrepresentative directory-page hit rate.
func benchReadProbeOrder(count int) []int {
	return rand.New(rand.NewPCG(7, 11)).Perm(count)
}

// BenchmarkFileStorePointRead is the warm point-read path: the whole store is
// resident, so what it measures is directory descent, lease traffic, and
// per-call snapshot setup rather than the device.
//
// The two arms exist because they answer different questions. "collection"
// is Collection.AppendRaw, the convenience form a caller reaches for first,
// which takes and drops a generation lease per call. "snapshot" is the same
// work under one caller-held Snapshot. The gap between them is the price of
// the convenience form, and it is the number this benchmark was written to
// expose.
func BenchmarkFileStorePointRead(b *testing.B) {
	if testing.Short() {
		b.Skip("100k-document corpus is too slow for -short")
	}
	collection, done := openBenchReadCollection(b, benchReadCorpusSize)
	defer done()
	order := benchReadProbeOrder(benchReadCorpusSize)

	// Keys are precomputed so the benchmark measures the store rather than
	// fmt.Sprintf, whose allocation would otherwise dominate the alloc column.
	keys := make([]string, len(order))
	for i := range keys {
		keys[i] = benchReadKey(order[i])
	}

	b.Run("collection", func(b *testing.B) {
		buf := make([]byte, 0, 512)
		i := 0
		base := collection.Stats()
		b.ReportAllocs()
		for b.Loop() {
			out, ok, err := collection.AppendRaw(buf[:0], keys[i])
			if err != nil || !ok {
				b.Fatalf("AppendRaw: ok=%v err=%v", ok, err)
			}
			buf = out
			if i++; i == len(keys) {
				i = 0
			}
		}
		b.StopTimer()
		reportReadResidency(b, collection.Stats(), base)
	})

	b.Run("snapshot", func(b *testing.B) {
		snapshot, err := collection.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		defer snapshot.Close()
		buf := make([]byte, 0, 512)
		i := 0
		base := collection.Stats()
		b.ReportAllocs()
		for b.Loop() {
			out, ok, err := snapshot.AppendRaw(buf[:0], keys[i])
			if err != nil || !ok {
				b.Fatalf("AppendRaw: ok=%v err=%v", ok, err)
			}
			buf = out
			if i++; i == len(keys) {
				i = 0
			}
		}
		b.StopTimer()
		reportReadResidency(b, collection.Stats(), base)
	})
}

// reportReadResidency exposes whether a "warm" read benchmark was actually
// warm. A resident budget larger than the file should give zero misses per
// operation; anything else means the number being reported is a device
// measurement wearing a cache measurement's name.
func reportReadResidency(b *testing.B, stats, base Stats) {
	b.Helper()
	if b.N == 0 {
		return
	}
	b.ReportMetric(float64(stats.CacheMisses-base.CacheMisses)/float64(b.N), "miss/op")
	b.ReportMetric(float64(stats.Evictions-base.Evictions)/float64(b.N), "evict/op")
	b.ReportMetric(float64(stats.PageReads-base.PageReads)/float64(b.N), "reads/op")
}
