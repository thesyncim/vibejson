package query

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/internal/storeio"
)

func TestRunFileSnapshotParallelSpillDifferential(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-store-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := simdjson.CreateFileStore(file, simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{ChunkDocuments: 8}, Synchronous: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	set := &simdjson.DocSet{ShapeTapes: true, Postings: true}
	for i := range 448 {
		label := fmt.Sprintf("group-%03d-%s", i, strings.Repeat(string(rune('a'+i%26)), 1024))
		doc := []byte(fmt.Sprintf(`{"id":%d,"bucket":%d,"score":%d,"label":%q,"active":%t}`,
			i, i%17, i*3, label, i%3 != 0))
		if _, err := set.Append(doc); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(fmt.Sprintf("key-%04d", i), doc); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	queries := []*Query{
		Select(Path("id"), Path("label")).Where(Cmp("active", Eq, true)).OrderBy("label", Desc).Limit(73),
		Select(Count(), Sum("score"), Avg("score"), Min("score"), Max("score")).Where(Cmp("bucket", Ge, 4)),
		Select(Path("bucket"), Count(), Sum("score"), Avg("score")).GroupBy("bucket").OrderBy("bucket", Desc),
		Select(Path("label"), Count(), Sum("score")).GroupBy("label").OrderBy("label", Asc).Limit(91),
		Select(Path("id")).Where(Cmp("id", Ge, 20)).Limit(19),
	}
	spillDir := t.TempDir()
	for i, q := range queries {
		want, err := q.Run(set)
		if err != nil {
			t.Fatalf("query %d baseline: %v", i, err)
		}
		got, stats, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
			Workers: 4, BatchRows: 11, BatchBytes: 4 << 10,
			MemoryBytes: 64 << 10, SpillDirectory: spillDir,
		})
		if err != nil {
			t.Fatalf("query %d file execution: %v", i, err)
		}
		if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
			t.Fatalf("query %d mismatch:\n got: %s\nwant: %s", i, gotKey, wantKey)
		}
		if stats.RowsScanned != uint64(set.Len()) || stats.Batches < 2 || stats.Workers != 4 {
			t.Fatalf("query %d stats = %+v", i, stats)
		}
		if i == 0 || i == 3 {
			if stats.SpillRuns <= maxSpillFanIn || stats.SpilledBytes == 0 {
				t.Fatalf("query %d did not exercise bounded fan-in spill: %+v", i, stats)
			}
		}
	}
	entries, err := os.ReadDir(spillDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("spill directory retained %d files", len(entries))
	}
}

func TestRunFileSnapshotOptions(t *testing.T) {
	q := Select(Count())
	if _, err := q.RunFileSnapshotInto(
		nil, nil, FileExecutionOptions{},
	); err == nil {
		t.Fatal("nil reusable result accepted")
	}
	if _, _, err := q.RunFileSnapshot(nil, FileExecutionOptions{}); err == nil {
		t.Fatal("nil snapshot accepted")
	}
	if _, _, err := q.RunFileSnapshot(nil, FileExecutionOptions{Workers: -1}); err == nil {
		t.Fatal("negative worker count accepted")
	}
	if _, _, err := q.RunFileSnapshot(nil, FileExecutionOptions{MemoryBytes: 1024}); err == nil {
		t.Fatal("undersized memory target accepted")
	}
}

func TestRunFileSnapshotPersistentFloat64CoveringAggregates(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-cover-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := simdjson.CreateFileStore(file, simdjson.FileStoreOptions{
		Store:          simdjson.StoreOptions{ChunkDocuments: 4},
		Float64Columns: []string{"/score", "/nested/value"},
		Synchronous:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	set := &simdjson.DocSet{ShapeTapes: true}
	for row, document := range []string{
		`{"score":1.5,"nested":{"value":10}}`,
		`{"score":2,"nested":{"value":"text"}}`,
		`{"score":"text","nested":{"value":-3}}`,
		`{"other":7}`,
		`{"score":-1,"nested":{"value":8}}`,
	} {
		raw := []byte(document)
		if _, err := set.Append(raw); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(fmt.Sprintf("k%d", row), raw); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	run := func(q *Query) (Result, FileExecutionStats) {
		t.Helper()
		want, err := q.Run(set)
		if err != nil {
			t.Fatal(err)
		}
		got, stats, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
			Workers: 3, BatchRows: 2, MemoryBytes: 64 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
			t.Fatalf("covering aggregate mismatch:\n got: %s\nwant: %s", gotKey, wantKey)
		}
		return got, stats
	}

	_, stats := run(Select(
		Count(), Sum("score"), Avg("score"), Min("score"), Max("score"),
		Sum("nested.value"), Max("nested.value"),
	))
	if stats.CoveringColumns != 2 || stats.RowsTotal != uint64(set.Len()) ||
		stats.RowsScanned != 0 || stats.Batches != 0 || stats.BufferedBytes != 0 {
		t.Fatalf("covering aggregate stats = %+v", stats)
	}
	_, stats = run(Select(Sum("score"), Max("score")))
	if stats.CoveringColumns != 1 || stats.RowsScanned != 0 {
		t.Fatalf("duplicate covering aggregate stats = %+v", stats)
	}
	result, stats := run(Select(Sum("score")).Limit(0))
	if result.RowCount != 0 || stats.CoveringColumns != 0 || stats.RowsScanned != 0 {
		t.Fatalf("LIMIT 0 covering result = rows %d stats %+v", result.RowCount, stats)
	}

	// COUNT(path) includes present non-numeric values, and an unconfigured
	// numeric path has no cover. Both shapes must remain on the JSON executor.
	_, stats = run(Select(Count("score")))
	if stats.CoveringColumns != 0 || stats.RowsScanned != uint64(set.Len()) {
		t.Fatalf("COUNT(path) incorrectly used numeric cover: %+v", stats)
	}
	_, stats = run(Select(Sum("other")))
	if stats.CoveringColumns != 0 || stats.RowsScanned != uint64(set.Len()) {
		t.Fatalf("unconfigured SUM incorrectly used cover: %+v", stats)
	}
	_, stats = run(Select(Sum("score")).Where(Cmp("other", Ge, 0)))
	if stats.CoveringColumns != 0 || stats.RowsScanned != uint64(set.Len()) {
		t.Fatalf("filtered SUM incorrectly used unfiltered cover: %+v", stats)
	}
}

func TestRunFileSnapshotIndexNativeScalarGroups(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-groups-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := simdjson.CreateFileStore(file, simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{ChunkDocuments: 4},
		Indexes: []simdjson.StoreIndexDefinition{{
			Name: "kind", Paths: []string{"/kind"},
		}},
		Synchronous: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	documents := []string{
		`{"kind":"a"}`,
		`{"kind":"b"}`,
		`{"kind":"\u0061"}`,
		`{"missing":true}`,
		`{"kind":null}`,
		`{"kind":{"nested":true}}`,
		`{"kind":1}`,
		`{"kind":1.0}`,
	}
	set := &simdjson.DocSet{ShapeTapes: true}
	for row, document := range documents {
		raw := []byte(document)
		if _, err := set.Append(raw); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(fmt.Sprintf("k%d", row), raw); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	q := Select(Path("kind"), Count()).GroupBy("kind").OrderBy("kind", Asc)
	want, err := q.Run(set)
	if err != nil {
		t.Fatal(err)
	}
	var workspace FileExecutionWorkspace
	got, stats, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
		Workers: 3, MemoryBytes: 64 << 10, Workspace: &workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
		t.Fatalf("index-native groups mismatch:\n got: %s\nwant: %s", gotKey, wantKey)
	}
	if stats.RowsTotal != 8 || stats.RowsScanned != 2 ||
		stats.IndexLookups != 1 || stats.IndexPostingPages == 0 ||
		stats.IndexCertificateRows != 6 || stats.IndexRecheckRows != 2 ||
		stats.IndexGroupedRows != 6 || stats.IndexGroups != 5 ||
		stats.Batches != 0 || stats.BufferedBytes != 0 {
		t.Fatalf("index-native group stats = %+v", stats)
	}

	// Result strings and raw projections are execution-owned, not aliases of
	// the reusable index certificate arena.
	beforeReuse := resultKey(got)
	if _, _, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
		Workers: 1, MemoryBytes: 64 << 10, Workspace: &workspace,
	}); err != nil {
		t.Fatal(err)
	}
	if afterReuse := resultKey(got); afterReuse != beforeReuse {
		t.Fatalf("result changed after workspace reuse:\n got: %s\nwant: %s", afterReuse, beforeReuse)
	}
}

func TestRunFileSnapshotIndexCatalogScalarGroups(t *testing.T) {
	builder, err := simdjson.NewStoreBuilder(simdjson.StoreOptions{
		ChunkDocuments: 4, ShapeTapes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	set := &simdjson.DocSet{ShapeTapes: true}
	documents := []string{
		`{"profile":{"kind":"a"}}`,
		`{"profile":{"kind":"\u0061"}}`,
		`{"missing":true}`,
		`{"profile":{"kind":null}}`,
		`{"profile":{"kind":1}}`,
		`{"profile":{"kind":1.0}}`,
	}
	for row, document := range documents {
		raw := []byte(document)
		if err := builder.Append(fmt.Sprintf("k%d", row), raw); err != nil {
			t.Fatal(err)
		}
		if _, err := set.Append(raw); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "query-file-group-catalog-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{ChunkDocuments: 4, ShapeTapes: true},
		Indexes: []simdjson.StoreIndexDefinition{{
			Name: "kind", Paths: []string{"/profile/kind"},
		}},
		PageSize: 4096, MaxPageSize: 64 << 10, Synchronous: true,
	}
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := simdjson.OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	q := Select(Path("profile.kind"), Count()).
		GroupBy("profile.kind").
		OrderBy("profile.kind", Asc)
	want, err := q.Run(set)
	if err != nil {
		t.Fatal(err)
	}
	var workspace FileExecutionWorkspace
	got, stats, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
		Workers: 4, MemoryBytes: 64 << 10, Workspace: &workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
		t.Fatalf("index catalog groups mismatch:\n got: %s\nwant: %s", gotKey, wantKey)
	}
	if stats.RowsTotal != 6 || stats.RowsScanned != 0 ||
		stats.IndexLookups != 1 || stats.IndexPostingPages != 0 ||
		stats.IndexCertificateRows != 6 || stats.IndexRecheckRows != 0 ||
		stats.IndexGroupedRows != 6 || stats.IndexGroups != 3 ||
		stats.Batches != 0 || stats.BufferedBytes != 0 {
		t.Fatalf("index catalog group stats = %+v", stats)
	}
	var reusable Result
	execution := FileExecutionOptions{
		Workers: 4, MemoryBytes: 64 << 10, Workspace: &workspace,
	}
	if _, err := q.RunFileSnapshotInto(
		&reusable, snapshot, execution,
	); err != nil {
		t.Fatal(err)
	}
	var reuseStats FileExecutionStats
	allocs := testing.AllocsPerRun(100, func() {
		reuseStats, err = q.RunFileSnapshotInto(
			&reusable, snapshot, execution,
		)
		if err != nil || reusable.RowCount != 3 {
			panic("reusable index catalog groups")
		}
	})
	if allocs != 0 {
		t.Fatalf(
			"reusable index catalog result allocations = %.2f, want zero",
			allocs,
		)
	}
	if gotKey, wantKey := resultKey(reusable), resultKey(want); gotKey != wantKey {
		t.Fatalf(
			"reusable index catalog groups mismatch:\n got: %s\nwant: %s",
			gotKey, wantKey,
		)
	}
	if reuseStats.IndexPostingPages != 0 ||
		reuseStats.IndexGroupedRows != 6 {
		t.Fatalf("reusable index catalog stats = %+v", reuseStats)
	}
	reusable.Release()
	if reusable.RowCount != 0 || reusable.Columns != nil {
		t.Fatalf("released result = %+v", reusable)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}

	// A non-first scalar mutation transactionally rewrites the bounded
	// catalog, so the O(groups) query lane survives ordinary churn.
	mutated := []byte(`{"profile":{"kind":"b"}}`)
	if created, err := store.Put("k1", mutated); err != nil || created {
		t.Fatalf("mutate covered group = (%v,%v)", created, err)
	}
	mutatedSet := &simdjson.DocSet{ShapeTapes: true}
	for row, document := range documents {
		raw := []byte(document)
		if row == 1 {
			raw = mutated
		}
		if _, err := mutatedSet.Append(raw); err != nil {
			t.Fatal(err)
		}
	}
	current, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer current.Close()
	want, err = q.Run(mutatedSet)
	if err != nil {
		t.Fatal(err)
	}
	got, stats, err = q.RunFileSnapshot(current, FileExecutionOptions{
		Workers: 4, MemoryBytes: 64 << 10, Workspace: &workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
		t.Fatalf(
			"incremental catalog groups mismatch:\n got: %s\nwant: %s",
			gotKey, wantKey,
		)
	}
	if stats.RowsTotal != 6 || stats.RowsScanned != 0 ||
		stats.IndexLookups != 1 || stats.IndexPostingPages != 0 ||
		stats.IndexCertificateRows != 6 || stats.IndexRecheckRows != 0 ||
		stats.IndexGroupedRows != 6 || stats.IndexGroups != 4 {
		t.Fatalf("incremental catalog group stats = %+v", stats)
	}
}

func TestRunFileSnapshotSegmentedIndexCatalogScalarGroups(t *testing.T) {
	const documents = 256
	builder, err := simdjson.NewStoreBuilder(simdjson.StoreOptions{
		ChunkDocuments: 8, ShapeTapes: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	for row := range documents {
		if err := builder.Append(
			fmt.Sprintf("k%03d", row),
			fmt.Appendf(nil, `{"kind":"value-%03d"}`, row),
		); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(
		t.TempDir(), "query-file-index-segmented-groups-*",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{
			ChunkDocuments: 8, ShapeTapes: true,
		},
		Indexes: []simdjson.StoreIndexDefinition{{
			Name: "kind", Paths: []string{"/kind"},
		}},
		PageSize: 4096, MaxPageSize: 4096,
		MaxKeyBytes: 32, InlineValueBytes: 128,
		MaxDocumentBytes: 1024, Synchronous: true,
	}
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := simdjson.OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	q := Select(Path("kind"), Count()).
		GroupBy("kind").
		OrderBy("kind", Asc)
	execution := FileExecutionOptions{
		Workers: 1, MemoryBytes: 64 << 10,
		Workspace: &FileExecutionWorkspace{},
	}
	var result Result
	stats, err := q.RunFileSnapshotInto(
		&result, snapshot, execution,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.RowCount != documents || stats.RowsScanned != 0 ||
		stats.IndexPostingPages != 0 ||
		stats.IndexGroupedRows != documents ||
		stats.IndexGroups != documents {
		t.Fatalf(
			"segmented query = rows %d stats %+v",
			result.RowCount, stats,
		)
	}
	allocs := testing.AllocsPerRun(100, func() {
		stats, err = q.RunFileSnapshotInto(
			&result, snapshot, execution,
		)
		if err != nil || result.RowCount != documents {
			panic("segmented file query")
		}
	})
	if allocs != 0 {
		t.Fatalf(
			"segmented query warm allocations = %.2f, want zero",
			allocs,
		)
	}
	first, ok := result.Columns[0].Cells[0].Text()
	if !ok || first != "value-000" {
		t.Fatalf("segmented first group = (%q,%v)", first, ok)
	}
	last, ok := result.Columns[0].Cells[documents-1].Text()
	if !ok || last != "value-255" {
		t.Fatalf("segmented last group = (%q,%v)", last, ok)
	}
}

func TestRunFileSnapshotPersistentCompoundIndexPushdown(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-index-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{ChunkDocuments: 8, ShapeTapes: true},
		Indexes: []simdjson.StoreIndexDefinition{
			{Name: "tenant_country", Paths: []string{"/tenant", "/profile/geo/country"}},
			{Name: "tenant", Paths: []string{"/tenant"}},
			{Name: "country", Paths: []string{"/profile/geo/country"}},
		},
		Synchronous: false,
	}
	store, err := simdjson.CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	set := &simdjson.DocSet{ShapeTapes: true, Postings: true}
	for i := range 512 {
		tenant := "other"
		if i%8 == 0 {
			tenant = "acme"
		}
		country := "US"
		if i%16 == 0 {
			country = "PT"
		}
		padding := ""
		if i%17 == 0 {
			padding = strings.Repeat("x", 900)
		}
		doc := []byte(fmt.Sprintf(
			`{"id":%d,"tenant":%q,"profile":{"geo":{"country":%q}},"padding":%q}`,
			i, tenant, country, padding,
		))
		if _, err := set.Append(doc); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(fmt.Sprintf("key-%04d", i), doc); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = simdjson.OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()

	run := func(q *Query) (Result, FileExecutionStats) {
		t.Helper()
		want, err := q.Run(set)
		if err != nil {
			t.Fatal(err)
		}
		got, stats, err := q.RunFileSnapshot(snapshot, FileExecutionOptions{
			Workers: 3, BatchRows: 7, BatchBytes: 16 << 10, MemoryBytes: 64 << 10,
		})
		if err != nil {
			t.Fatal(err)
		}
		if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
			t.Fatalf("indexed file result mismatch:\n got: %s\nwant: %s", gotKey, wantKey)
		}
		return got, stats
	}

	_, stats := run(
		Select(Path("id"), Path("profile.geo.country")).Where(And(
			Cmp("tenant", Eq, "acme"),
			Cmp("profile.geo.country", Eq, "PT"),
		)),
	)
	if !stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.RowsTotal != 512 || stats.CandidateRows != 32 ||
		stats.RowsScanned != 32 || stats.CandidateChunks != 32 {
		t.Fatalf("compound pushdown stats = %+v", stats)
	}

	_, stats = run(
		Select(Count()).Where(And(
			Cmp("tenant", Eq, "absent"),
			Cmp("profile.geo.country", Eq, "PT"),
		)),
	)
	if !stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.CandidateRows != 0 || stats.RowsScanned != 0 || stats.Batches != 0 {
		t.Fatalf("empty compound pushdown stats = %+v", stats)
	}

	result, stats := run(Select(Count()).Where(Cmp("tenant", Eq, "acme")))
	if result.RowCount != 1 || !countIs(result.Columns[0].Cells[0], 64) ||
		!stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.CandidateRows != 64 || stats.CandidateChunks != 64 ||
		stats.IndexPostingPages == 0 ||
		stats.IndexCertificateRows != 64 || stats.IndexRecheckRows != 0 ||
		stats.RowsScanned != 0 || stats.Batches != 0 {
		t.Fatalf("direct exact count = result %s stats %+v", resultKey(result), stats)
	}

	result, stats = run(Select(Count()).Where(Contains("", `{"tenant":"acme"}`)))
	if result.RowCount != 1 || !countIs(result.Columns[0].Cells[0], 64) ||
		!stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.CandidateRows != 64 || stats.IndexCertificateRows != 64 ||
		stats.IndexRecheckRows != 0 ||
		stats.RowsScanned != 0 || stats.Batches != 0 {
		t.Fatalf("direct root containment count = result %s stats %+v", resultKey(result), stats)
	}

	result, stats = run(Select(Count()).Where(Contains("profile.geo", `{"country":"PT"}`)))
	if result.RowCount != 1 || !countIs(result.Columns[0].Cells[0], 32) ||
		!stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.CandidateRows != 32 || stats.IndexCertificateRows != 32 ||
		stats.IndexRecheckRows != 0 ||
		stats.RowsScanned != 0 || stats.Batches != 0 {
		t.Fatalf("direct nested containment count = result %s stats %+v", resultKey(result), stats)
	}

	result, stats = run(Select(Count()).Where(Contains(
		"", `{"tenant":"acme","profile":{"geo":{"country":"PT"}}}`,
	)))
	if result.RowCount != 1 || !countIs(result.Columns[0].Cells[0], 32) ||
		!stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.CandidateRows != 32 || stats.IndexCertificateRows != 32 ||
		stats.IndexRecheckRows != 0 || stats.RowsScanned != 0 {
		t.Fatalf("compound containment count = result %s stats %+v", resultKey(result), stats)
	}

	result, stats = run(Select(Count()).Where(Contains(
		"", `{"tenant":"absent","tenant":"acme"}`,
	)))
	if result.RowCount != 1 || !countIs(result.Columns[0].Cells[0], 64) ||
		!stats.IndexBounded || stats.IndexLookups != 1 ||
		stats.IndexCertificateRows != 64 || stats.IndexRecheckRows != 0 ||
		stats.RowsScanned != 0 {
		t.Fatalf("last-wins containment count = result %s stats %+v", resultKey(result), stats)
	}

	_, stats = run(Select(Count()).Where(Contains(
		"", `{"profile":{"geo":{}}}`,
	)))
	if stats.IndexBounded || stats.IndexLookups != 0 ||
		stats.IndexCertificateRows != 0 || stats.IndexRecheckRows != 0 ||
		stats.RowsScanned != 512 {
		t.Fatalf("empty-object containment incorrectly flattened: %+v", stats)
	}

	_, stats = run(
		Select(Path("id")).Where(Or(
			Cmp("tenant", Eq, "acme"),
			Cmp("profile.geo.country", Eq, "PT"),
		)),
	)
	if !stats.IndexBounded || stats.IndexLookups != 2 ||
		stats.CandidateRows != 64 || stats.RowsScanned != 64 || stats.CandidateChunks != 64 {
		t.Fatalf("bounded OR pushdown stats = %+v", stats)
	}

	_, stats = run(
		Select(Path("id")).Where(Or(
			Cmp("tenant", Eq, "acme"),
			Cmp("id", Ge, 500),
		)),
	)
	if stats.IndexBounded || stats.IndexLookups != 0 || stats.RowsScanned != 512 {
		t.Fatalf("mixed OR fallback stats = %+v", stats)
	}

	_, stats = run(Select(Count()).Where(Not(Cmp("tenant", Eq, "acme"))))
	if stats.IndexBounded || stats.IndexLookups != 0 || stats.RowsScanned != 512 {
		t.Fatalf("durable NOT fallback stats = %+v", stats)
	}

	_, stats = run(Select(Path("id")).Where(Cmp("id", Ge, 500)))
	if stats.IndexBounded || stats.IndexLookups != 0 ||
		stats.RowsTotal != 512 || stats.RowsScanned != 512 {
		t.Fatalf("unindexed fallback stats = %+v", stats)
	}
}

func TestRunFileSnapshotIndexCorruptionFailsClosed(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "query-file-index-corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := simdjson.FileStoreOptions{
		Store: simdjson.StoreOptions{ChunkDocuments: 8},
		Indexes: []simdjson.StoreIndexDefinition{{
			Name: "status", Paths: []string{"/status"},
		}},
		Synchronous: true,
	}
	store, err := simdjson.CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 16 {
		if _, err := store.Put(
			fmt.Sprintf("key-%02d", i),
			[]byte(fmt.Sprintf(`{"id":%d,"status":"active"}`, i)),
		); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	var rootScratch [4096]byte
	super, root, _, err := storeio.RecoverStateRoot(file, 4096, rootScratch[:])
	if err != nil {
		t.Fatal(err)
	}
	ref := root.IndexDirectory
	var posting storeio.PageRef
	for {
		page := make([]byte, ref.Length)
		if _, err := file.ReadAt(page, int64(ref.Offset)); err != nil {
			t.Fatal(err)
		}
		view, err := storeio.OpenIndexDirectoryPage(
			page, super.FileEnd, root.NextLogicalID, root.IndexCount,
		)
		if err != nil {
			t.Fatal(err)
		}
		if view.Header().Level == 0 {
			entry, ok := view.EntryAt(0)
			if !ok {
				t.Fatal("current index leaf is empty")
			}
			posting = entry.Posting.Page
			break
		}
		child, ok := view.ChildAt(0)
		if !ok {
			t.Fatal("current index branch is empty")
		}
		ref = child.Ref
	}
	corrupt := make([]byte, posting.Length)
	if _, err := file.ReadAt(corrupt, int64(posting.Offset)); err != nil {
		t.Fatal(err)
	}
	certificate := []byte(`"active"`)
	position := bytes.Index(corrupt, certificate)
	if position < 0 {
		t.Fatal("posting certificate is absent")
	}
	copy(corrupt[position:], `xxxxxxxx`)
	if _, err := storeio.SealPage(corrupt); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(corrupt, int64(posting.Offset)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}

	store, err = simdjson.OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	_, stats, err := Select(Count()).Where(Cmp("status", Eq, "active")).RunFileSnapshot(
		snapshot, FileExecutionOptions{Workers: 1},
	)
	if !errors.Is(err, storeio.ErrPostingPageCorrupt) {
		t.Fatalf("corrupt index query error = %v, want %v", err, storeio.ErrPostingPageCorrupt)
	}
	if stats.RowsScanned != 0 {
		t.Fatalf("corrupt index silently scanned %d rows", stats.RowsScanned)
	}
}
