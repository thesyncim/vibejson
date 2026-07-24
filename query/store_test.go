package query

import (
	"fmt"
	"math/bits"
	"slices"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

func TestRunSnapshotDeclaredIndexesDifferential(t *testing.T) {
	docs := []string{
		`{"id":1,"tenant":"acme","status":"active","score":10,"nested":{"country":"PT"},"items":[{"sku":"A"}]}`,
		`{"id":2,"tenant":"acme","status":"idle","score":20,"nested":{"country":"US"},"items":[{"sku":"B"}]}`,
		`{"id":3,"tenant":"other","status":"active","score":30,"nested":{"country":"PT"},"items":[{"sku":"B"}]}`,
		`{"id":4,"tenant":"acme","status":"active","score":40,"nested":{"country":"PT"},"items":[{"sku":"A"}]}`,
		`{"id":5,"tenant":"other","status":"idle","score":50,"items":[]}`,
	}
	set := &store.DocSet{ShapeTapes: true}
	s := store.NewStore(store.StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	for i, doc := range docs {
		if _, err := set.Append([]byte(doc)); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
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
	if _, err := s.CreateIndex(store.StoreIndexDefinition{Name: "tenant_status", Paths: []string{"/tenant", "/status"}}); err != nil {
		t.Fatal(err)
	}
	buildingSnapshot, _ := s.Snapshot()
	assertSnapshotQueriesEqual(t, queries, set, buildingSnapshot, "building")
	if info, err := s.BackfillIndex("tenant_status", 0); err != nil || info.State != store.StoreIndexReady {
		t.Fatalf("BackfillIndex(tenant_status) = (%+v,%v)", info, err)
	}
	compoundReadySnapshot, _ := s.Snapshot()
	assertSnapshotQueriesEqual(t, queries, set, compoundReadySnapshot, "compound-ready")

	for _, def := range []store.StoreIndexDefinition{
		{Name: "tenant", Paths: []string{"/tenant"}},
		{Name: "status", Paths: []string{"/status"}},
		{Name: "country", Paths: []string{"/nested/country"}},
		{Name: "sku", Paths: []string{"/items/0/sku"}},
	} {
		if _, err := s.CreateIndex(def); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"tenant", "status", "country", "sku"} {
		info, err := s.BackfillIndex(name, 0)
		if err != nil || info.State != store.StoreIndexReady {
			t.Fatalf("BackfillIndex(%s) = (%+v,%v)", name, info, err)
		}
	}
	readySnapshot, _ := s.Snapshot()
	assertSnapshotQueriesEqual(t, queries, set, readySnapshot, "ready")
	plan, err := queries[0].compiled()
	if err != nil {
		t.Fatal(err)
	}
	var workspace Workspace
	if _, err := plan.storeCandidateMasks(readySnapshot, &workspace); err != nil {
		t.Fatal(err)
	}
	if workspace.storeIndexProbes != 1 {
		t.Fatalf("compound plan issued %d index probes, want one widest probe", workspace.storeIndexProbes)
	}
}

func assertSnapshotQueriesEqual(t *testing.T, queries []*Query, set *store.DocSet, snapshot store.Snapshot, phase string) {
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
	s := store.NewStore(store.StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 128; i++ {
		doc := fmt.Sprintf(`{"id":%d,"bucket":%d,"nested":{"country":"PT"}}`, i, i&7)
		if _, err := s.Put(fmt.Sprintf("k%03d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	for _, def := range []store.StoreIndexDefinition{
		{Name: "bucket", Paths: []string{"/bucket"}},
		{Name: "bucket_country", Paths: []string{"/bucket", "/nested/country"}},
	} {
		info, err := s.CreateIndex(def)
		if err != nil {
			t.Fatal(err)
		}
		for info.State != store.StoreIndexReady {
			info, err = s.BackfillIndex(def.Name, 0)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	q := Select(Count(), Sum("id")).Where(And(Cmp("bucket", Eq, 3), Cmp("nested.country", Eq, "PT")))
	var result Result
	var workspace Workspace
	snapshot, _ := s.Snapshot()
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

func TestRunSnapshotIndexedCountFastPath(t *testing.T) {
	s := store.NewStore(store.StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 256; i++ {
		value := 2
		if i&3 == 0 {
			value = 1
		}
		doc := fmt.Sprintf(`{"id":%d,"v":%d,"g":%d,"nested":{"bucket":%d}}`, i, value, i%5, i%7)
		if _, err := s.Put(fmt.Sprintf("k%03d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	for _, def := range []store.StoreIndexDefinition{
		{Name: "v", Paths: []string{"/v"}},
		{Name: "g", Paths: []string{"/g"}},
		{Name: "bucket", Paths: []string{"/nested/bucket"}},
	} {
		info, err := s.CreateIndex(def)
		if err != nil {
			t.Fatal(err)
		}
		if info, err = s.BackfillIndex(info.Name, 0); err != nil || info.State != store.StoreIndexReady {
			t.Fatalf("BackfillIndex(%s) = (%+v,%v)", def.Name, info, err)
		}
	}
	snapshot, _ := s.Snapshot()
	countQuery := Select(Count()).Where(Cmp("v", Eq, 1))
	var result Result
	var workspace Workspace
	if err := countQuery.RunSnapshotInto(&result, snapshot, &workspace); err != nil {
		t.Fatal(err)
	}
	count, _ := result.Column("count(*)")
	if !countIs(count.Cells[0], 64) {
		t.Fatalf("collision-rechecked count = %s, want 64", count.Cells[0])
	}
	if len(workspace.raws) != 0 {
		t.Fatal("direct indexed count materialized JSON columns")
	}
	allocs := testing.AllocsPerRun(100, func() {
		if err := countQuery.RunSnapshotInto(&result, snapshot, &workspace); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("direct indexed count allocated %.2f times, want 0", allocs)
	}

	for _, test := range []struct {
		name  string
		query *Query
		want  int64
	}{
		{"not", Select(Count()).Where(Not(Cmp("v", Eq, 1))), 192},
		{"and", Select(Count()).Where(And(Cmp("v", Eq, 1), Cmp("g", Eq, 0))), 13},
		{"or", Select(Count()).Where(Or(Cmp("v", Eq, 1), Cmp("g", Eq, 0))), 103},
		{"nested", Select(Count()).Where(Cmp("nested.bucket", Eq, 3)), 37},
		{"contains", Select(Count()).Where(Contains("", `{"v":1}`)), 64},
		{"not-contains", Select(Count()).Where(Not(Contains("", `{"v":1}`))), 192},
	} {
		var got Result
		var runWorkspace Workspace
		if err := test.query.RunSnapshotInto(&got, snapshot, &runWorkspace); err != nil {
			t.Fatalf("%s: %v", test.name, err)
		}
		column, _ := got.Column("count(*)")
		if !countIs(column.Cells[0], test.want) {
			t.Fatalf("%s count = %s, want %d", test.name, column.Cells[0], test.want)
		}
		if len(runWorkspace.raws) != 0 {
			t.Fatalf("%s materialized JSON columns", test.name)
		}
	}

	// The ordinary projection uses the cheaper candidate-only probe and still
	// evaluates the final predicate before materialization.
	projected, err := Select(Path("id")).
		Where(Cmp("v", Eq, 1)).
		RunSnapshot(snapshot)
	if err != nil || projected.RowCount != 64 {
		t.Fatalf("candidate projection = (rows=%d, err=%v), want 64", projected.RowCount, err)
	}
}

func TestAdvanceStoreMasksUntil(t *testing.T) {
	masks := make([]store.StoreMask, 1024)
	for i := range masks {
		masks[i].Chunk = uint32(i * 4)
		masks[i].Bits = 1
	}
	for _, test := range []struct {
		pos    int
		target uint32
		want   int
	}{
		{0, 1, 1},
		{0, 4, 1},
		{0, 2048, 512},
		{17, 4092, 1023},
		{17, 4093, 1024},
	} {
		if got := advanceStoreMasksUntil(masks, test.pos, test.target); got != test.want {
			t.Fatalf("advance(%d,%d) = %d, want %d", test.pos, test.target, got, test.want)
		}
	}
}

func TestStoreMaskBooleanDifferential(t *testing.T) {
	state := uint64(0x9e3779b97f4a7c15)
	random := func() uint64 {
		state ^= state >> 12
		state ^= state << 25
		state ^= state >> 27
		return state * 0x2545f4914f6cdd1d
	}
	build := func(n int) []store.StoreMask {
		out := make([]store.StoreMask, n)
		var chunk uint32
		for i := range out {
			chunk += 1 + uint32(random()%32)
			out[i] = store.StoreMask{Chunk: chunk, Bits: random() | 1}
		}
		return out
	}
	linearAdvance := func(masks []store.StoreMask, pos int, target uint32) int {
		pos++
		for pos < len(masks) && masks[pos].Chunk < target {
			pos++
		}
		return pos
	}
	linearIntersect := func(a, b []store.StoreMask) []store.StoreMask {
		out := make([]store.StoreMask, 0, min(len(a), len(b)))
		for i, j := 0, 0; i < len(a) && j < len(b); {
			switch {
			case a[i].Chunk < b[j].Chunk:
				i++
			case a[i].Chunk > b[j].Chunk:
				j++
			default:
				if bits := a[i].Bits & b[j].Bits; bits != 0 {
					out = append(out, store.StoreMask{Chunk: a[i].Chunk, Bits: bits})
				}
				i++
				j++
			}
		}
		return out
	}
	linearAndNot := func(a, b []store.StoreMask) []store.StoreMask {
		out := make([]store.StoreMask, 0, len(a))
		j := 0
		for _, left := range a {
			for j < len(b) && b[j].Chunk < left.Chunk {
				j++
			}
			value := left.Bits
			if j < len(b) && b[j].Chunk == left.Chunk {
				value &^= b[j].Bits
			}
			if value != 0 {
				out = append(out, store.StoreMask{Chunk: left.Chunk, Bits: value})
			}
		}
		return out
	}

	for trial := 0; trial < 500; trial++ {
		a := build(int(random() % 512))
		b := build(int(random() % 512))
		for i := range a {
			target := uint32(random() % 20000)
			if got, want := advanceStoreMasksUntil(a, i, target), linearAdvance(a, i, target); got != want {
				t.Fatalf("trial %d advance(%d,%d) = %d, want %d", trial, i, target, got, want)
			}
		}
		if got, want := intersectStoreMasks(nil, a, b), linearIntersect(a, b); !slices.Equal(got, want) {
			t.Fatalf("trial %d intersection mismatch:\n got %v\nwant %v", trial, got, want)
		}
		if got, want := andNotStoreMasks(nil, a, b), linearAndNot(a, b); !slices.Equal(got, want) {
			t.Fatalf("trial %d difference mismatch:\n got %v\nwant %v", trial, got, want)
		}
	}
}

func TestRunSnapshotCategoricalGroupFastPath(t *testing.T) {
	docs := []string{
		`{"g":"a"}`, `{}`, `{"g":null}`, `{"g":""}`,
		`{"g":"a"}`, `{"g":"b"}`, `{"g":null}`,
	}
	set := &store.DocSet{ShapeTapes: true}
	builder, err := store.NewBuilder(store.StoreOptions{ShapeTapes: true})
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
	s, err := builder.Build()
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
	snapshot, _ := s.Snapshot()
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
	escaped := store.NewStore(store.StoreOptions{ShapeTapes: true})
	for i, doc := range []string{`{"g":"ab"}`, `{"g":"ab"}`} {
		if _, err := escaped.Put(fmt.Sprintf("e%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	escapedSnapshot, _ := escaped.Snapshot()
	escapedResult, err := q.RunSnapshot(escapedSnapshot)
	if err != nil || escapedResult.RowCount != 1 {
		t.Fatalf("escaped grouping = (rows=%d, err=%v), want one decoded group", escapedResult.RowCount, err)
	}
}

func TestRunSnapshotSingleMemberContainmentUsesExactIndex(t *testing.T) {
	docs := []string{
		`{"a":1}`, `{"a":2}`, `{"a":[1]}`, `{"b":1}`,
		`{"a":1,"extra":true}`, `[]`, `null`,
	}
	s := store.NewStore(store.StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	for i, doc := range docs {
		if _, err := s.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := s.CreateIndex(store.StoreIndexDefinition{Name: "a", Paths: []string{"/a"}})
	if err != nil {
		t.Fatal(err)
	}
	for info.State != store.StoreIndexReady {
		info, err = s.BackfillIndex(info.Name, 0)
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
	snapshot, _ := s.Snapshot()
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
