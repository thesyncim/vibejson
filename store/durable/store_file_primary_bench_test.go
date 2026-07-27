package durable

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

func openBenchPrimaryReadCollection(
	tb testing.TB, count int,
) (*Collection, func()) {
	tb.Helper()
	path := filepath.Join(tb.TempDir(), "primary-read.vibe")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		tb.Fatal(err)
	}
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		tb.Fatal(err)
	}
	for at := range count {
		if err := builder.Append(
			benchReadKey(at), benchReadDocument(at),
		); err != nil {
			tb.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		tb.Fatal(err)
	}
	options := Options{
		ResidentBytes: 64 << 20, Backend: BackendPortable,
	}
	if _, err := CreateFromPrimary(built, file, options); err != nil {
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

// BenchmarkFilePrimaryPointRead mirrors BenchmarkFileStorePointRead over the
// same 100k documents and fixed random permutation. It reports the collection
// convenience form and the caller-pinned Snapshot separately, with allocation
// and residency metrics kept visible. On an Apple M4 Max (darwin/arm64), the
// phase-H frame-hint validation run measured 488-494 ns/op for Collection and
// 467-471 ns/op for Snapshot, both at 0 B/op and 0 allocs/op. These honest
// end-to-end results still do not meet the 300 ns promotion gate.
func BenchmarkFilePrimaryPointRead(b *testing.B) {
	if testing.Short() {
		b.Skip("100k-document corpus is too slow for -short")
	}
	collection, done := openBenchPrimaryReadCollection(
		b, benchReadCorpusSize,
	)
	defer done()
	order := benchReadProbeOrder(benchReadCorpusSize)
	keys := make([]string, len(order))
	for at := range keys {
		keys[at] = benchReadKey(order[at])
	}
	warm := make([]byte, 0, 512)
	for _, key := range keys {
		out, ok, err := collection.AppendRaw(warm[:0], key)
		if err != nil || !ok {
			b.Fatalf("warm AppendRaw: ok=%v err=%v", ok, err)
		}
		warm = out
	}

	b.Run("collection", func(b *testing.B) {
		buffer := make([]byte, 0, 512)
		at := 0
		base := collection.Stats()
		b.ReportAllocs()
		for b.Loop() {
			out, ok, err := collection.AppendRaw(
				buffer[:0], keys[at],
			)
			if err != nil || !ok {
				b.Fatalf("AppendRaw: ok=%v err=%v", ok, err)
			}
			buffer = out
			if at++; at == len(keys) {
				at = 0
			}
		}
		b.StopTimer()
		reportReadResidency(b, collection.Stats(), base)
		b.ReportMetric(
			float64(collection.primaryRouter.ResidentBytes())/
				float64(benchReadCorpusSize),
			"router-B/doc",
		)
		b.ReportMetric(
			float64(collection.primaryRouter.BuildDuration().Nanoseconds()),
			"router-build-ns",
		)
	})

	b.Run("snapshot", func(b *testing.B) {
		snapshot, err := collection.Snapshot()
		if err != nil {
			b.Fatal(err)
		}
		defer snapshot.Close()
		buffer := make([]byte, 0, 512)
		at := 0
		base := collection.Stats()
		b.ReportAllocs()
		for b.Loop() {
			out, ok, err := snapshot.AppendRaw(
				buffer[:0], keys[at],
			)
			if err != nil || !ok {
				b.Fatalf("AppendRaw: ok=%v err=%v", ok, err)
			}
			buffer = out
			if at++; at == len(keys) {
				at = 0
			}
		}
		b.StopTimer()
		reportReadResidency(b, collection.Stats(), base)
		b.ReportMetric(
			float64(collection.primaryRouter.ResidentBytes())/
				float64(benchReadCorpusSize),
			"router-B/doc",
		)
	})
}
