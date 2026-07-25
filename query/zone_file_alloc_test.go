package query

import (
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// The durable chunk-summary probe is on the query path, so it is held to the
// same contract as everything else there: a warm execution allocates nothing
// that scales with the collection, and probing the summaries allocates nothing
// at all.
//
// The distinction matters because the probe walks a radix tree with a
// callback, and a closure that escapes would put its captured mask buffer,
// counter, and decoded summary on the heap once per directory page rather than
// once per process. That is exactly the shape of regression a plain
// correctness test cannot see.

// zoneAllocCorpus is a clustered page-backed corpus with no declared index, so
// every predicate below reaches the chunk-summary tier rather than an index.
func zoneAllocCorpus(tb testing.TB, documents int) *durable.Snapshot {
	tb.Helper()
	file, err := os.CreateTemp(tb.TempDir(), "query-zone-alloc-*")
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = file.Close() })
	source := &store.Collection{}
	for i := range documents {
		doc := fmt.Appendf(nil, `{"id":%d,"bucket":%d,"score":%d}`, i, i%128, i*3)
		if _, err := source.Put(fmt.Sprintf("key-%07d", i), doc); err != nil {
			tb.Fatal(err)
		}
	}
	options := durable.Options{Collection: store.Options{ChunkDocuments: 64}}
	if _, err := durable.CreateFrom(source, file, options); err != nil {
		tb.Fatal(err)
	}
	collection, err := durable.Open(file, options)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = collection.Close() })
	snapshot, err := collection.Snapshot()
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = snapshot.Close() })
	return snapshot
}

// Given a durable snapshot carrying chunk summaries, when the summaries are
// probed repeatedly with a reused mask buffer, then the probe allocates
// nothing.
func TestFileZoneProbeAllocFree(t *testing.T) {
	snapshot := zoneAllocCorpus(t, 20000)
	code, ok := store.ZoneCodeNumber([]byte("1000"))
	if !ok {
		t.Fatal("literal has no zone code")
	}
	probe := store.ZoneProbe{Path: store.ZonePathHash("id"), Lo: code, Hi: code, Op: store.ZoneOpEq}
	masks := make([]store.Mask, 0, 4096)
	// Warm: the first probe fills the page cache with the directory pages.
	if out, pruned, bounded := snapshot.AppendZoneMasks(masks[:0], probe); !bounded || pruned == 0 || len(out) == 0 {
		t.Fatalf("probe did not prune: %d masks, %d pruned, bounded %v", len(out), pruned, bounded)
	}
	allocs := testing.AllocsPerRun(50, func() {
		if _, _, bounded := snapshot.AppendZoneMasks(masks[:0], probe); !bounded {
			t.Fatal("probe stopped pruning")
		}
	})
	if allocs != 0 {
		t.Fatalf("AppendZoneMasks allocated %.1f times per probe", allocs)
	}
}

// Given a warm Exec, when a pruned durable query runs repeatedly, then its
// allocation count does not scale with the collection and matches what the
// same query costs with pruning disabled to within the fixed per-execution
// overhead.
func TestFileZonePrunedQueryAllocStaysFlat(t *testing.T) {
	measure := func(documents int) uint64 {
		snapshot := zoneAllocCorpus(t, documents)
		q := Select(Count()).Where(Cmp("id", Lt, 100))
		e := Exec{Options: ExecOptions{Workers: 4}}
		for range 4 {
			if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
				t.Fatal(err)
			}
		}
		if e.Workspace.zonePruned == 0 {
			t.Fatal("query did not prune: the allocation bound is vacuous")
		}
		var before, after runtime.MemStats
		const runs = 20
		runtime.ReadMemStats(&before)
		for range runs {
			if err := q.RunInto(&e, FromFile(snapshot)); err != nil {
				t.Fatal(err)
			}
		}
		runtime.ReadMemStats(&after)
		return (after.Mallocs - before.Mallocs) / runs
	}
	small := measure(20000)
	large := measure(100000)
	// Five times the documents must not cost more allocations. A tolerance of
	// one absorbs the integer division above, not a per-batch residual.
	if large > small+1 {
		t.Fatalf("allocations scale with the corpus: %d at 20k documents, %d at 100k", small, large)
	}
	t.Logf("pruned durable query allocations: %d per execution at 20k, %d at 100k", small, large)
}
