package query

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// durableScanCorpus builds a page-backed snapshot of n documents carrying no
// persistent index at all. That is the shape the general batched executor
// exists for: every pushdown lane declines, so the query admits and parses the
// whole corpus in worker batches, which is the path whose per-document cost
// this file pins down.
func durableScanCorpus(tb testing.TB, n int) *durable.Snapshot {
	tb.Helper()
	file, err := os.CreateTemp(tb.TempDir(), "query-file-scan-*")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = file.Close() })
	source := &store.Collection{}
	for i := range n {
		doc := fmt.Appendf(nil,
			`{"id":%d,"bucket":%d,"score":%d,"label":"row-%06d","active":%t,`+
				`"note":"lorem ipsum dolor sit amet consectetur adipiscing elit sed do %d"}`,
			i, i%128, i*3, i, i%3 != 0, i)
		if _, err := source.Put(fmt.Sprintf("key-%07d", i), doc); err != nil {
			tb.Fatal(err)
		}
	}
	options := durable.Options{Collection: store.Options{ChunkDocuments: 64}}
	if _, err := durable.CreateFrom(source, file, options); err != nil {
		tb.Fatal(err)
	}
	fs, err := durable.Open(file, options)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = fs.Close() })
	snapshot, err := fs.Snapshot()
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = snapshot.Close() })
	return snapshot
}

// scanAllocsPerRun reports the heap allocations and bytes one warm execution of
// q over snapshot costs. It reads MemStats rather than using
// testing.AllocsPerRun because the batched executor allocates on worker and
// scanner goroutines, which AllocsPerRun's single-goroutine accounting does not
// see — the very allocations this file is about.
func scanAllocsPerRun(tb testing.TB, q *Query, snapshot *durable.Snapshot, runs int) (allocs, bytes uint64) {
	tb.Helper()
	e := Exec{Options: ExecOptions{Workers: 4}}
	// Four warm-up passes: the first fills the retained buffers, and the ring
	// slots only all see their high-water batch once the corpus has been
	// walked at least once with every slot in play.
	for range 4 {
		if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
			tb.Fatal(err)
		}
	}
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	for range runs {
		if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
			tb.Fatal(err)
		}
	}
	runtime.ReadMemStats(&after)
	return (after.Mallocs - before.Mallocs) / uint64(runs),
		(after.TotalAlloc - before.TotalAlloc) / uint64(runs)
}

// BenchmarkFileFilteredCountAlloc is the durable backend's counterpart to the
// zero-allocation contract the two in-memory backends already meet: an
// unindexed filtered COUNT over a corpus far larger than any one batch, run
// repeatedly through a single warm Exec. Everything it reports is
// per-execution steady-state cost, so it is a regression detector first and a
// throughput number second.
//
// The three corpus sizes are what separate a fixed per-execution cost from a
// per-batch one: batches scale with documents, so a residual that scales with
// batches shows up as a number that roughly doubles from 50k to 100k while a
// genuinely fixed cost stays flat. Reading one size alone cannot tell the two
// apart.
func BenchmarkFileFilteredCountAlloc(b *testing.B) {
	for _, documents := range []int{10000, 50000, 100000} {
		b.Run(fmt.Sprintf("docs=%d", documents), func(b *testing.B) {
			snapshot := durableScanCorpus(b, documents)
			q := Select(Count()).Where(Cmp("bucket", Lt, 2))
			e := Exec{Options: ExecOptions{Workers: 4}}
			if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for b.Loop() {
				if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// Given a warm Exec, when a durable filtered count scans a large corpus, then
// its allocation count must not scale with the documents it scanned.
//
// The number this guards is a contract, not a micro-optimization. Every buffer
// a batch needs — the scan buffers, the worker Workspace, the worker's scan
// Segment, the aggregate accumulators, the reorder buffer — is retained by the
// Exec and reused, so a second execution of the same shape has nothing left to
// allocate at all: what remains is a fixed handful of per-execution channels and
// goroutine frames that does not move with the corpus. A change that
// reintroduces per-document or per-batch storage shows up here as an allocation
// count proportional to the corpus, long before it shows up as the scheduler and
// GC saturation it actually causes.
//
// The budget is a constant rather than a fraction of the corpus, and that is the
// point of this revision: the previous documents/50 budget scaled with the very
// thing the test claims does not scale, so it passed comfortably while the
// per-batch store.Segment — 411 of the 430 allocations one warm execution then
// made, and essentially all of its 33.8 MB — was still being minted. Segment
// now has Reset, the executor retains one per worker, and the measured steady
// state is 18 to 21 allocations and a few kilobytes at every corpus size from
// 10k to 100k documents. A budget of 64 leaves ordinary drift room to move while
// sitting an order of magnitude below the per-batch cost it exists to catch, and
// two orders below the 4.2-per-document transient posting index this backend
// carried before that.
func TestFileFilteredCountSteadyAllocs(t *testing.T) {
	const documents = 50000
	const budget = 64
	snapshot := durableScanCorpus(t, documents)
	q := Select(Count()).Where(Cmp("bucket", Lt, 2))
	allocs, bytes := scanAllocsPerRun(t, q, snapshot, 3)
	t.Logf("filtered count over %d documents: %d allocs, %d B per warm execution",
		documents, allocs, bytes)
	if allocs > budget {
		t.Fatalf("filtered count over %d documents allocated %d times (%d B) per warm execution, "+
			"budget %d: something is allocating per document or per batch again",
			documents, allocs, bytes, budget)
	}
}

// Given a batch ring narrow enough to wrap many times, when an ordered
// projection spills and merges, then the durable result must be byte-identical
// to the in-memory one.
//
// This is the ring's safety proof. Batch scratch — scan buffers, the reorder
// slot, the per-batch row headers — is reused by sequence modulo the ring
// length, and the ring is only sound because the credit protocol guarantees a
// sequence has been scanned, reduced, and consumed before its slot comes round
// again. One worker makes the ring three slots wide and a small BatchRows makes
// the corpus wrap it dozens of times, so an off-by-one in that argument
// corrupts a row rather than staying theoretical. The ordering and LIMIT make
// the corruption observable: rows are compared by content, not counted.
func TestRunFileSnapshotBatchRingReuseDifferential(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-ring-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	fs, err := durable.Create(file, durable.Options{
		Collection: store.Options{ChunkDocuments: 8}, Synchronous: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer fs.Close()

	set := &store.Segment{ShapeTapes: true, Postings: true}
	for i := range 600 {
		doc := fmt.Appendf(nil,
			`{"id":%d,"bucket":%d,"score":%d,"label":"lbl-%03d-%c","active":%t,"esc":"a\nb%d"}`,
			i, i%13, (i*7)%600, i, 'a'+i%26, i%3 != 0, i)
		if _, err := set.Append(doc); err != nil {
			t.Fatal(err)
		}
		if _, err := fs.Put(fmt.Sprintf("key-%04d", i), doc); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := fs.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	queries := []*Query{
		Select(Path("id"), Path("label"), Path("esc")).OrderBy("label", Desc).OrderBy("id", Asc),
		Select(Path("id"), Path("esc")).Where(Cmp("active", Eq, true)).OrderBy("score", Asc).Limit(37),
		Select(Path("id")).Where(Cmp("bucket", Eq, 5)),
		Select(Path("label"), Count(), Sum("score")).GroupBy("label").OrderBy("label", Asc),
		Select(Count(), Sum("score"), Min("score"), Max("score")).Where(Cmp("score", Ge, 100)),
	}
	// One worker gives the narrowest ring the executor builds, and eleven rows
	// a batch wraps it more than fifty times over this corpus. The Exec is
	// shared across queries so each one runs against slots another query left
	// filled, which is where stale retained storage would surface.
	e := Exec{Options: ExecOptions{
		Workers: 1, BatchRows: 11, BatchBytes: 4 << 10,
		MemoryBytes: 64 << 10, SpillDirectory: t.TempDir(),
	}}
	for pass := range 2 {
		for i, q := range queries {
			want, err := q.Run(FromSegment(set))
			if err != nil {
				t.Fatalf("pass %d query %d baseline: %v", pass, i, err)
			}
			if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
				t.Fatalf("pass %d query %d file execution: %v", pass, i, err)
			}
			if got, expected := resultKey(e.Result), resultKey(want); got != expected {
				t.Fatalf("pass %d query %d mismatch:\n got: %s\nwant: %s", pass, i, got, expected)
			}
			if e.Stats.Batches < 50 {
				t.Fatalf("pass %d query %d ran %d batches, too few to wrap the ring",
					pass, i, e.Stats.Batches)
			}
		}
	}
}
