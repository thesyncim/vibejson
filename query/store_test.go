package query

import (
	"fmt"
	"math/bits"
	"testing"

	"github.com/thesyncim/slopjson"
)

func TestRunSnapshotDeclaredIndexesDifferential(t *testing.T) {
	docs := []string{
		`{"id":1,"tenant":"acme","status":"active","score":10,"nested":{"country":"PT"},"items":[{"sku":"A"}]}`,
		`{"id":2,"tenant":"acme","status":"idle","score":20,"nested":{"country":"US"},"items":[{"sku":"B"}]}`,
		`{"id":3,"tenant":"other","status":"active","score":30,"nested":{"country":"PT"},"items":[{"sku":"B"}]}`,
		`{"id":4,"tenant":"acme","status":"active","score":40,"nested":{"country":"PT"},"items":[{"sku":"A"}]}`,
		`{"id":5,"tenant":"other","status":"idle","score":50,"items":[]}`,
	}
	set := &slopjson.DocSet{ShapeTapes: true}
	store := slopjson.NewStore(slopjson.StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	for i, doc := range docs {
		if _, err := set.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	queries := []*Query{
		Select(Path("id"), Path("nested.country")).Where(And(Cmp("tenant", Eq, "acme"), Cmp("status", Eq, "active"))),
		Select(Path("id")).Where(Cmp("nested.country", Eq, "PT")),
		Select(Path("id")).Where(Cmp("/items/0/sku", Eq, "B")),
		Select(Path("id")).Where(Contains("", `{"status":"active"}`)),
		Select(Path("id")).Where(Contains("nested", `{"country":"PT"}`)),
		Select(Path("id")).Where(Or(Cmp("tenant", Eq, "other"), Cmp("status", Eq, "idle"))).OrderBy("id", Asc),
		Select(Path("id")).Where(Not(Cmp("status", Eq, "idle"))).OrderBy("id", Asc),
		Select(Count(), Sum("score")).Where(And(Cmp("tenant", Eq, "acme"), Cmp("score", Ge, 20))),
		Select(Path("tenant"), Sum("score")).Where(Cmp("nested.country", Eq, "PT")).GroupBy("tenant").OrderBy("tenant", Asc),
	}

	// Compiled plans precede DDL. While Building they take the dense exact path.
	if _, err := store.CreateIndex(slopjson.StoreIndexDefinition{Name: "tenant_status", Paths: []string{"/tenant", "/status"}}); err != nil {
		t.Fatal(err)
	}
	assertSnapshotQueriesEqual(t, queries, set, store.Snapshot(), "building")
	if info, err := store.BackfillIndex("tenant_status", 0); err != nil || info.State != slopjson.StoreIndexReady {
		t.Fatalf("BackfillIndex(tenant_status) = (%+v,%v)", info, err)
	}
	assertSnapshotQueriesEqual(t, queries, set, store.Snapshot(), "compound-ready")

	for _, def := range []slopjson.StoreIndexDefinition{
		{Name: "tenant", Paths: []string{"/tenant"}},
		{Name: "status", Paths: []string{"/status"}},
		{Name: "country", Paths: []string{"/nested/country"}},
		{Name: "sku", Paths: []string{"/items/0/sku"}},
	} {
		if _, err := store.CreateIndex(def); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tenant", "status", "country", "sku"} {
		info, err := store.BackfillIndex(name, 0)
		if err != nil || info.State != slopjson.StoreIndexReady {
			t.Fatalf("BackfillIndex(%s) = (%+v,%v)", name, info, err)
		}
	}
	assertSnapshotQueriesEqual(t, queries, set, store.Snapshot(), "ready")
	plan, err := queries[0].compiled()
	if err != nil {
		t.Fatal(err)
	}
	var workspace Workspace
	if _, err := plan.storeCandidateMasks(store.Snapshot(), &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.storeIndexProbes != 1 {
		t.Fatalf("compound plan issued %d index probes, want one widest probe", workspace.storeIndexProbes)
	}
}

func assertSnapshotQueriesEqual(t *testing.T, queries []*Query, set *slopjson.DocSet, snapshot slopjson.Snapshot, phase string) {
	t.Helper()
	for i, q := range queries {
		want, err := q.Run(set)
		if err != nil {
			t.Fatalf("%s query %d DocSet: %v", phase, i, err)
		}
		got, err := q.RunSnapshot(snapshot)
		if err != nil {
			t.Fatalf("%s query %d Snapshot: %v", phase, i, err)
		}
		if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
			t.Fatalf("%s query %d mismatch:\n got: %s\nwant: %s", phase, i, gotKey, wantKey)
		}
	}
}

func TestRunSnapshotIntoSteadyAllocs(t *testing.T) {
	store := slopjson.NewStore(slopjson.StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 128; i++ {
		doc := fmt.Sprintf(`{"id":%d,"bucket":%d,"nested":{"country":"PT"}}`, i, i&7)
		if _, err := store.Put(fmt.Sprintf("k%03d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	for _, def := range []slopjson.StoreIndexDefinition{
		{Name: "bucket", Paths: []string{"/bucket"}},
		{Name: "bucket_country", Paths: []string{"/bucket", "/nested/country"}},
	} {
		info, err := store.CreateIndex(def)
		if err != nil {
			t.Fatal(err)
		}
		for info.State != slopjson.StoreIndexReady {
			info, err = store.BackfillIndex(def.Name, 0)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	q := Select(Count(), Sum("id")).Where(And(Cmp("bucket", Eq, 3), Cmp("nested.country", Eq, "PT")))
	var result Result
	var workspace Workspace
	snapshot := store.Snapshot()
	if err := q.RunSnapshotInto(&result, snapshot, &workspace); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := q.RunSnapshotInto(&result, snapshot, &workspace); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("RunSnapshotInto allocated %.2f times, want 0", allocs)
	}
}

func TestRunSnapshotCategoricalGroupFastPath(t *testing.T) {
	docs := []string{
		`{"g":"a"}`, `{}`, `{"g":null}`, `{"g":""}`,
		`{"g":"a"}`, `{"g":"b"}`, `{"g":null}`,
	}
	set := &slopjson.DocSet{ShapeTapes: true}
	builder, err := slopjson.NewStoreBuilder(slopjson.StoreOptions{ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, doc := range docs {
		if _, err := set.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
		if err := builder.Append(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	q := Select(Path("g"), Count()).GroupBy("g").OrderBy("g", Asc)
	want, err := q.Run(set)
	if err != nil {
		t.Fatal(err)
	}
	var got Result
	var workspace Workspace
	snapshot := store.Snapshot()
	if err := q.RunSnapshotInto(&got, snapshot, &workspace); err != nil {
		t.Fatal(err)
	}
	if gotKey, wantKey := resultKey(got), resultKey(want); gotKey != wantKey {
		t.Fatalf("categorical grouping mismatch:\n got: %s\nwant: %s", gotKey, wantKey)
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := q.RunSnapshotInto(&got, snapshot, &workspace); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("categorical RunSnapshotInto allocated %.2f times, want 0", allocs)
	}

	// Escaped strings take the general lane and retain decoded-value grouping.
	escaped := slopjson.NewStore(slopjson.StoreOptions{ShapeTapes: true})
	for i, doc := range []string{`{"g":"ab"}`, `{"g":"a\u0062"}`} {
		if _, err := escaped.Put(fmt.Sprintf("e%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	escapedResult, err := q.RunSnapshot(escaped.Snapshot())
	if err != nil || escapedResult.RowCount != 1 {
		t.Fatalf("escaped grouping = (rows=%d, err=%v), want one decoded group", escapedResult.RowCount, err)
	}
}

func TestRunSnapshotSingleMemberContainmentUsesExactIndex(t *testing.T) {
	docs := []string{
		`{"a":1}`, `{"a":2}`, `{"a":[1]}`, `{"b":1}`,
		`{"a":1,"extra":true}`, `[]`, `null`,
	}
	store := slopjson.NewStore(slopjson.StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	for i, doc := range docs {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.CreateIndex(slopjson.StoreIndexDefinition{Name: "a", Paths: []string{"/a"}})
	if err != nil {
		t.Fatal(err)
	}
	for info.State != slopjson.StoreIndexReady {
		info, err = store.BackfillIndex(info.Name, 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	q := Select(Count()).Where(Contains("", `{"a":1}`))
	plan, err := q.compiled()
	if err != nil {
		t.Fatal(err)
	}
	var workspace Workspace
	snapshot := store.Snapshot()
	masks, err := plan.storeCandidateMasks(snapshot, &workspace)
	if err != nil {
		t.Fatal(err)
	}
	candidates := 0
	for _, mask := range masks {
		candidates += bits.OnesCount64(mask.Bits)
	}
	if candidates != 2 {
		t.Fatalf("derived containment candidates = %d, want 2 exact scalar matches", candidates)
	}
	result, err := q.RunSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if col, _ := result.Column("count(*)"); !countIs(col.Cells[0], 2) {
		t.Fatalf("indexed containment result = %s, want 2", col.Cells[0])
	}
	notResult, err := Select(Count()).Where(Not(Contains("", `{"a":1}`))).RunSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if col, _ := notResult.Column("count(*)"); !countIs(col.Cells[0], int64(len(docs)-2)) {
		t.Fatalf("negated indexed containment result = %s, want %d", col.Cells[0], len(docs)-2)
	}
}
