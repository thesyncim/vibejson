package durable

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

// benchWriteOptions is the shipped default geometry with only the knobs a
// benchmark must pin: a portable backend so results do not depend on io_uring
// availability, and a resident budget large enough that eviction noise does not
// hide the write path being measured. Every group-commit knob stays at its
// package default so the benchmark reports what a caller actually gets.
func benchWriteOptions() Options {
	return Options{
		Collection:         store.Options{ChunkDocuments: 16},
		ResidentBytes: 64 << 20,
		Backend:       BackendPortable,
	}
}

func benchDocument(i int) []byte {
	return fmt.Appendf(nil, `{"id":%d,"name":"row-%d","score":%d.5,"tag":"benchmark"}`, i, i, i%97)
}

func openBenchStore(b *testing.B, options Options) (*Collection, func()) {
	b.Helper()
	path := filepath.Join(b.TempDir(), "bench.vibe")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	fileStore, err := Create(file, options)
	if err != nil {
		_ = file.Close()
		b.Fatal(err)
	}
	return fileStore, func() {
		if closeErr := fileStore.Close(); closeErr != nil {
			b.Fatal(closeErr)
		}
		_ = file.Close()
	}
}

// reportWriteAmplification converts the device counters into the two numbers a
// write-path change is judged by: bytes actually handed to the device per
// published generation, and how many generations shared one durability fence.
func reportWriteAmplification(b *testing.B, stats Stats, base Stats, puts int) {
	b.Helper()
	if puts == 0 {
		return
	}
	b.ReportMetric(float64(stats.DeviceBytes-base.DeviceBytes)/float64(puts), "devB/put")
	commits := stats.DeviceCommits - base.DeviceCommits
	if commits != 0 {
		b.ReportMetric(float64(puts)/float64(commits), "puts/fsync")
	}
}

// BenchmarkFileStorePutDeviceBytes measures write amplification per Put at
// several store sizes. A directory whose depth does not track the store's size
// shows up here as a constant, size-independent surcharge on every Put.
func BenchmarkFileStorePutDeviceBytes(b *testing.B) {
	for _, existing := range []int{0, 1_000, 10_000, 100_000} {
		b.Run(fmt.Sprintf("existing=%d", existing), func(b *testing.B) {
			if existing >= 100_000 && testing.Short() {
				b.Skip("100k-document prefill is too slow for -short")
			}
			options := benchWriteOptions()
			fileStore, done := openBenchStore(b, options)
			defer done()
			for i := range existing {
				if _, err := fileStore.Put(fmt.Sprintf("key-%09d", i), benchDocument(i)); err != nil {
					b.Fatal(err)
				}
			}
			base := fileStore.Stats()
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; b.Loop(); i++ {
				if _, err := fileStore.Put(fmt.Sprintf("key-%09d", existing+i), benchDocument(existing+i)); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			reportWriteAmplification(b, fileStore.Stats(), base, b.N)
		})
	}
}

// BenchmarkFileStorePutSynchronous measures the latency a durable caller sees.
// Concurrency is the variable that matters: one serialized writer can never
// group-commit, so its cost is one full double-fence per Put no matter how the
// group-commit knobs are set.
func BenchmarkFileStorePutSynchronous(b *testing.B) {
	for _, writers := range []int{1, 8, 64} {
		b.Run(fmt.Sprintf("writers=%d", writers), func(b *testing.B) {
			options := benchWriteOptions()
			options.Synchronous = true
			fileStore, done := openBenchStore(b, options)
			defer done()
			base := fileStore.Stats()
			b.ResetTimer()
			runFileStoreWriters(b, fileStore, writers)
			b.StopTimer()
			stats := fileStore.Stats()
			reportWriteAmplification(b, stats, base, b.N)
			b.ReportMetric(float64(stats.LargestCommitGroup), "maxgroup")
		})
	}
}
