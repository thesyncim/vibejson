package durable

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

// benchBatchOptions is the shipped default geometry with only the batch bound
// moved, so a batch arm reports what a caller gets by setting one option. The
// resident budget tracks the reservation because a wider batch may hold more
// dirty pages before its single publication.
func benchBatchOptions(documents int) Options {
	return Options{
		Collection:        store.Options{ChunkDocuments: 64},
		ResidentBytes:     512 << 20,
		Backend:           BackendPortable,
		MaxRetiredExtents: 1 << 17,
		MaxBatchDocuments: documents,
	}
}

func benchBatchDocument(i int) []byte {
	return fmt.Appendf(nil, `{"id":%d,"name":"row-%d","score":%d.5,"tag":"benchmark"}`, i, i, i%97)
}

// reportBatchWrite converts one arm's counters into the two numbers the batched
// write path is judged by: wall time and device bytes per document, both
// amortised over the whole run rather than per call, so arms with different
// batch sizes are directly comparable.
func reportBatchWrite(b *testing.B, collection *Collection, base Stats, documents int) {
	b.Helper()
	if documents == 0 {
		return
	}
	stats := collection.Stats()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(documents), "ns/doc")
	b.ReportMetric(float64(stats.DeviceBytes-base.DeviceBytes)/float64(documents), "devB/doc")
	b.ReportMetric(float64(documents)/float64(max(stats.DeviceCommits-base.DeviceCommits, 1)), "docs/fsync")
}

// BenchmarkFileStoreBatchWrite measures the cost of ingesting the same
// documents one Put at a time and in batches of several sizes. The batch=1 arm
// is the control: it proves how much of the gap is the batched machinery itself
// rather than the grouping.
func BenchmarkFileStoreBatchWrite(b *testing.B) {
	for _, size := range []int{1, 10, 100, 1000} {
		b.Run(fmt.Sprintf("update/batch=%d", size), func(b *testing.B) {
			collection, done := openBenchCollection(b, benchBatchOptions(size))
			defer done()
			base := collection.Stats()
			written := 0
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				start := i * size
				if err := collection.Update(func(batch *WriteBatch) error {
					for j := range size {
						if err := batch.Put(
							fmt.Sprintf("key-%09d", start+j), benchBatchDocument(start+j),
						); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				written += size
			}
			b.StopTimer()
			reportBatchWrite(b, collection, base, written)
		})
	}
	b.Run("put", func(b *testing.B) {
		collection, done := openBenchCollection(b, benchBatchOptions(1))
		defer done()
		base := collection.Stats()
		written := 0
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			if _, err := collection.Put(fmt.Sprintf("key-%09d", i), benchBatchDocument(i)); err != nil {
				b.Fatal(err)
			}
			written++
		}
		b.StopTimer()
		reportBatchWrite(b, collection, base, written)
	})
}

// BenchmarkFileStoreBatchWriteIndexed repeats the comparison with one exact
// index configured. Indexed batches are the case a naive grouping cannot serve:
// tuple hashes are uniformly distributed, so every document touches a different
// posting leaf.
func BenchmarkFileStoreBatchWriteIndexed(b *testing.B) {
	indexes := []store.IndexDefinition{{Name: "tag", Paths: []string{"/tag"}}}
	document := func(i int) []byte {
		return fmt.Appendf(nil, `{"id":%d,"name":"row-%d","tag":"t%04d"}`, i, i, i%1000)
	}
	for _, size := range []int{1, 100} {
		b.Run(fmt.Sprintf("update/batch=%d", size), func(b *testing.B) {
			options := benchBatchOptions(size)
			options.Indexes = indexes
			collection, done := openBenchCollection(b, options)
			defer done()
			base := collection.Stats()
			written := 0
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				start := i * size
				if err := collection.Update(func(batch *WriteBatch) error {
					for j := range size {
						if err := batch.Put(fmt.Sprintf("key-%09d", start+j), document(start+j)); err != nil {
							return err
						}
					}
					return nil
				}); err != nil {
					b.Fatal(err)
				}
				written += size
			}
			b.StopTimer()
			reportBatchWrite(b, collection, base, written)
		})
	}
	b.Run("put", func(b *testing.B) {
		options := benchBatchOptions(1)
		options.Indexes = indexes
		collection, done := openBenchCollection(b, options)
		defer done()
		base := collection.Stats()
		written := 0
		b.ResetTimer()
		for i := 0; b.Loop(); i++ {
			if _, err := collection.Put(fmt.Sprintf("key-%09d", i), document(i)); err != nil {
				b.Fatal(err)
			}
			written++
		}
		b.StopTimer()
		reportBatchWrite(b, collection, base, written)
	})
}

// BenchmarkFileStoreCreateFromFloor is the lower bound any online write path is
// measured against: a bulk build writes every page once, sequentially, with no
// copy-on-write directory rewrites at all.
func BenchmarkFileStoreCreateFromFloor(b *testing.B) {
	const documents = 20_000
	source, err := store.New(store.Options{ChunkDocuments: 64})
	if err != nil {
		b.Fatal(err)
	}
	for i := range documents {
		if _, err := source.Put(fmt.Sprintf("key-%09d", i), benchBatchDocument(i)); err != nil {
			b.Fatal(err)
		}
	}
	options := benchBatchOptions(64)
	b.ResetTimer()
	for i := 0; b.Loop(); i++ {
		path := filepath.Join(b.TempDir(), fmt.Sprintf("bulk-%d.vibe", i))
		file, openErr := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
		if openErr != nil {
			b.Fatal(openErr)
		}
		size, buildErr := CreateFrom(source, file, options)
		if buildErr != nil {
			b.Fatal(buildErr)
		}
		b.SetBytes(size)
		if closeErr := file.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
	}
	b.StopTimer()
	b.ReportMetric(float64(b.Elapsed().Nanoseconds())/float64(b.N*documents), "ns/doc")
}
