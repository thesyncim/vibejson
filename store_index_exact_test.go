package slopjson

import (
	"fmt"
	"slices"
	"testing"
)

func testScalarIndex(t testing.TB, src string) Index {
	t.Helper()
	entries, err := RequiredIndexEntries([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	index, err := BuildIndex([]byte(src), make([]IndexEntry, entries))
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestStoreExactCompoundIndexLifecycle(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	for _, row := range []struct{ key, doc string }{
		{"a", `{"tenant":"acme","status":"active","n":1}`},
		{"b", `{"tenant":"acme","status":"idle","n":1.0}`},
		{"c", `{"tenant":"other","status":"active","n":2}`},
	} {
		key, doc := row.key, row.doc
		if _, err := store.Put(key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	def := StoreIndexDefinition{Name: "tenant_status", Paths: []string{"/tenant", "/status"}}
	info, err := store.CreateIndex(def)
	if err != nil || info.State != StoreIndexBuilding || info.ColumnCount != 2 {
		t.Fatalf("CreateIndex = (%+v,%v)", info, err)
	}
	// Caller mutation cannot alter the compiled definition or published info.
	def.Paths[0] = "/wrong"
	infos := store.Snapshot().AppendIndexes(nil)
	if len(infos) != 1 || infos[0].Columns[0] != "/tenant" || infos[0].Columns[1] != "/status" {
		t.Fatalf("published definition = %+v", infos)
	}

	// Building is an operational state, never a correctness precondition.
	got, err := store.IndexRawKeys("tenant_status", []byte(`"acme"`), []byte(`"active"`))
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("building lookup = (%v,%v)", got, err)
	}
	info, err = store.BackfillIndex("tenant_status", 1)
	if err != nil || info.CoveredChunks != 1 || info.State != StoreIndexBuilding {
		t.Fatalf("partial backfill = (%+v,%v)", info, err)
	}
	info, err = store.BackfillIndex("tenant_status", 0)
	if err != nil || info.State != StoreIndexReady || info.CoveredChunks != info.TotalChunks {
		t.Fatalf("complete backfill = (%+v,%v)", info, err)
	}

	before := store.Snapshot()
	if _, err := store.Put("a", []byte(`{"tenant":"acme","status":"idle","n":1}`)); err != nil {
		t.Fatal(err)
	}
	if !store.Delete("b") {
		t.Fatal("Delete(b) missed")
	}
	if _, err := store.Put("d", []byte(`{"tenant":"acme","status":"active","n":3}`)); err != nil {
		t.Fatal(err)
	}

	active := []Index{testScalarIndex(t, `"acme"`), testScalarIndex(t, `"active"`)}
	got, err = store.Snapshot().AppendIndexKeys(nil, "tenant_status", active...)
	if err != nil || !slices.Equal(got, []string{"d"}) {
		t.Fatalf("current active lookup = (%v,%v)", got, err)
	}
	idle := testScalarIndex(t, `"idle"`)
	got, err = store.Snapshot().AppendIndexKeys(nil, "tenant_status", active[0], idle)
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("current idle lookup = (%v,%v)", got, err)
	}
	got, err = before.AppendIndexKeys(nil, "tenant_status", active...)
	if err != nil || !slices.Equal(got, []string{"a"}) {
		t.Fatalf("retained snapshot lookup = (%v,%v)", got, err)
	}
}

func TestStoreExactIndexNestedFields(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 2, ShapeTapes: true, ValueDict: true})
	docs := []string{
		`{"profile":{"geo":{"country":"PT"},"a/b":{"~tag":"blue"}},"items":[{"sku":"A"}]}`,
		`{"profile":{"geo":{"country":"US"},"a/b":{"~tag":"blue"}},"items":[{"sku":"B"}]}`,
		`{"profile":{"geo":{"country":"PT"},"a/b":{"~tag":"red"}},"items":[{"sku":"B"}]}`,
		`{"profile":{"geo":{}},"items":[]}`,
	}
	for i, doc := range docs {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	for _, def := range []StoreIndexDefinition{
		{Name: "country", Paths: []string{"/profile/geo/country"}},
		{Name: "escaped", Paths: []string{"/profile/a~1b/~0tag"}},
		{Name: "country_sku", Paths: []string{"/profile/geo/country", "/items/0/sku"}},
	} {
		info, err := store.CreateIndex(def)
		if err != nil {
			t.Fatal(err)
		}
		info, err = store.BackfillIndex(def.Name, 0)
		if err != nil || info.State != StoreIndexReady {
			t.Fatalf("BackfillIndex(%s) = (%+v,%v)", def.Name, info, err)
		}
	}

	for _, test := range []struct {
		name   string
		values []string
		want   []string
	}{
		{"country", []string{`"PT"`}, []string{"k0", "k2"}},
		{"escaped", []string{`"blue"`}, []string{"k0", "k1"}},
		{"country_sku", []string{`"PT"`, `"B"`}, []string{"k2"}},
	} {
		values := make([][]byte, len(test.values))
		for i := range test.values {
			values[i] = []byte(test.values[i])
		}
		got, err := store.IndexRawKeys(test.name, values...)
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("%s lookup = (%v,%v), want %v", test.name, got, err, test.want)
		}
	}
}

func TestStoreExactIndexMutationDifferential(t *testing.T) {
	for _, chunkDocuments := range []int{1, 3, 8, 64} {
		t.Run(fmt.Sprintf("chunk=%d", chunkDocuments), func(t *testing.T) {
			store := NewStore(StoreOptions{ChunkDocuments: chunkDocuments, ShapeTapes: true})
			for i := 0; i < 97; i++ {
				doc := fmt.Sprintf(`{"tenant":"t%d","profile":{"bucket":%d},"seq":%d}`, i%7, i%11, i)
				if _, err := store.Put(fmt.Sprintf("k%03d", i), []byte(doc)); err != nil {
					t.Fatal(err)
				}
			}
			info, err := store.CreateIndex(StoreIndexDefinition{
				Name: "tenant_bucket", Paths: []string{"/tenant", "/profile/bucket"},
			})
			if err != nil {
				t.Fatal(err)
			}
			if info, err = store.BackfillIndex(info.Name, 2); err != nil {
				t.Fatal(err)
			}
			checkStoreExactIndexDifferential(t, store.Snapshot(), info.Name)
			if info, err = store.BackfillIndex(info.Name, 0); err != nil || info.State != StoreIndexReady {
				t.Fatalf("complete backfill = (%+v,%v)", info, err)
			}

			retained := store.Snapshot()
			for step := 0; step < 240; step++ {
				i := (step*37 + 13) % 131
				key := fmt.Sprintf("k%03d", i)
				if step%9 == 0 {
					store.Delete(key)
				} else {
					doc := fmt.Sprintf(`{"tenant":"t%d","profile":{"bucket":%d},"seq":%d}`, (i+step)%7, (i*3+step)%11, step)
					if _, err := store.Put(key, []byte(doc)); err != nil {
						t.Fatal(err)
					}
				}
				if step%17 == 0 {
					checkStoreExactIndexDifferential(t, store.Snapshot(), info.Name)
					checkStoreExactIndexDifferential(t, retained, info.Name)
				}
			}
			checkStoreExactIndexDifferential(t, store.Snapshot(), info.Name)
			checkStoreExactIndexDifferential(t, retained, info.Name)
		})
	}
}

func checkStoreExactIndexDifferential(t testing.TB, snapshot Snapshot, name string) {
	t.Helper()
	tenantPath := MustCompilePointer("/tenant")
	bucketPath := MustCompilePointer("/profile/bucket")
	for tenant := 0; tenant < 7; tenant++ {
		for bucket := 0; bucket < 11; bucket++ {
			want := make([]string, 0)
			tenantNeedle := testScalarIndex(t, fmt.Sprintf(`"t%d"`, tenant)).Root().Raw()
			bucketNeedle := testScalarIndex(t, fmt.Sprint(bucket)).Root().Raw()
			snapshot.Range(func(key string, _ RawValue) bool {
				doc, _ := snapshot.Get(key)
				tenantNode, tenantOK, tenantErr := doc.PointerCompiled(tenantPath)
				bucketNode, bucketOK, bucketErr := doc.PointerCompiled(bucketPath)
				if tenantErr != nil || bucketErr != nil {
					t.Fatalf("reference pointer: tenant=%v bucket=%v", tenantErr, bucketErr)
				}
				if tenantOK && bucketOK && storeIndexScalarEqual(tenantNode.Raw(), tenantNeedle) && storeIndexScalarEqual(bucketNode.Raw(), bucketNeedle) {
					want = append(want, key)
				}
				return true
			})
			got, err := snapshot.IndexRawKeys(name, tenantNeedle.Bytes(), bucketNeedle.Bytes())
			if err != nil || !slices.Equal(got, want) {
				t.Fatalf("%s/%d = (%v,%v), want %v", tenantNeedle.Bytes(), bucket, got, err, want)
			}
		}
	}
}

func TestStoreExactIndexScalarSemantics(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 1})
	docs := []string{
		`{"v":1}`,
		`{"v":1.0}`,
		`{"v":"a"}`,
		`{"v":"\u0061"}`,
		`{"v":1e100000}`,
		`{"v":2e100000}`,
		`{"v":null}`,
		`{"v":[1]}`,
		`{}`,
	}
	for i, doc := range docs {
		if _, err := store.Put(string(rune('a'+i)), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.CreateIndex(StoreIndexDefinition{Name: "v", Paths: []string{"/v"}})
	if err != nil {
		t.Fatal(err)
	}
	for info.State != StoreIndexReady {
		info, err = store.BackfillIndex("v", 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		needle string
		want   []string
	}{
		{`1e0`, []string{"a", "b"}},
		{`"a"`, []string{"c", "d"}},
		{`1e100000`, []string{"e"}}, // wide-number bucket collision is rechecked
		{`2e100000`, []string{"f"}},
		{`null`, []string{"g"}},
	} {
		got, err := store.IndexRawKeys("v", []byte(test.needle))
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("lookup %s = (%v,%v), want %v", test.needle, got, err, test.want)
		}
	}
	if _, err := store.IndexRawKeys("v", []byte(`[1]`)); err != ErrStoreIndexScalar {
		t.Fatalf("container lookup error = %v, want %v", err, ErrStoreIndexScalar)
	}
}

func TestStoreExactIndexDefinitionErrors(t *testing.T) {
	store := new(Store)
	for _, def := range []StoreIndexDefinition{
		{},
		{Name: "x"},
		{Name: "x", Paths: []string{"not-a-pointer"}},
		{Name: "x", Paths: []string{"/a", "/b", "/c", "/d", "/e"}},
	} {
		if _, err := store.CreateIndex(def); err == nil {
			t.Fatalf("CreateIndex(%+v) succeeded", def)
		}
	}
	if _, err := store.CreateIndex(StoreIndexDefinition{Name: "x", Paths: []string{"/a"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateIndex(StoreIndexDefinition{Name: "x", Paths: []string{"/b"}}); err != ErrStoreIndexExists {
		t.Fatalf("duplicate error = %v", err)
	}
	if _, err := store.IndexRawKeys("x"); err != ErrStoreIndexArity {
		t.Fatalf("arity error = %v", err)
	}
}

func TestStoreIndexMasksPersistentPromotion(t *testing.T) {
	var masks storeIndexMasks
	for _, id := range []uint32{9, 1, 5, 2} {
		masks = masks.set(id, uint64(1)<<id)
	}
	inline := masks
	masks = masks.set(33, 7)
	if masks.wide.root == nil || inline.wide.root != nil {
		t.Fatal("fifth chunk did not promote without modifying old value")
	}
	var ids []uint32
	masks.each(func(id uint32, _ uint64) bool {
		ids = append(ids, id)
		return true
	})
	if !slices.Equal(ids, []uint32{1, 2, 5, 9, 33}) {
		t.Fatalf("iteration order = %v", ids)
	}
	before := masks
	masks = masks.set(5, 0)
	if before.get(5) == 0 || masks.get(5) != 0 {
		t.Fatal("persistent delete changed old bitmap or retained new bit")
	}
	if masks.wide.root != nil || masks.n != storeIndexInlineMasks {
		t.Fatal("four-word posting did not demote to compact inline storage")
	}

	var vector storeIndexMaskVector
	vector = vector.set(1, 1)
	vector = vector.set(1<<30, 2)
	deep := vector
	vector = vector.set(1<<30, 0)
	if vector.depth != 0 || vector.get(1) != 1 || deep.get(1<<30) != 2 {
		t.Fatal("radix vector did not shrink without changing retained root")
	}
}

func TestStoreIndexPostingBulkBuild(t *testing.T) {
	pending := make(map[uint64][]storeIndexChunkMask, 2048)
	for i := uint64(0); i < 2048; i++ {
		// Hold the first radix digit constant and vary later digits. This is
		// the adversarial ordering for a builder that incorrectly sorts by the
		// ordinary high-to-low integer order.
		hash := i<<5 | 7
		pending[hash] = []storeIndexChunkMask{{chunk: uint32(i & 63), mask: uint64(1) << (i & 63)}}
	}
	root := storeIndexBuildBulk(pending)
	for hash, want := range pending {
		got, ok := storeIndexPostingLookup(root, hash)
		if !ok || got.get(want[0].chunk) != want[0].mask {
			t.Fatalf("lookup %#x missed bulk-built posting", hash)
		}
	}
}

func TestStoreExactIndexSteadyLookupAllocs(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 64; i++ {
		doc := []byte(`{"tenant":"acme","bucket":3}`)
		if _, err := store.Put(string(rune(i+1)), doc); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.CreateIndex(StoreIndexDefinition{Name: "tb", Paths: []string{"/tenant", "/bucket"}})
	if err != nil {
		t.Fatal(err)
	}
	for info.State != StoreIndexReady {
		info, err = store.BackfillIndex("tb", 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	snapshot := store.Snapshot()
	tenant := testScalarIndex(t, `"acme"`)
	bucket := testScalarIndex(t, `3`)
	dst := make([]string, 0, snapshot.Len())
	dst, err = snapshot.AppendIndexKeys(dst[:0], "tb", tenant, bucket)
	if err != nil || len(dst) != snapshot.Len() {
		t.Fatalf("warm lookup = (%d,%v)", len(dst), err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		var runErr error
		dst, runErr = snapshot.AppendIndexKeys(dst[:0], "tb", tenant, bucket)
		if runErr != nil || len(dst) != snapshot.Len() {
			panic("lookup failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("AppendIndexKeys allocated %.2f times, want 0", allocs)
	}
	rawTenant, rawBucket := []byte(`"acme"`), []byte(`3`)
	allocs = testing.AllocsPerRun(100, func() {
		var runErr error
		dst, runErr = snapshot.AppendIndexRawKeys(dst[:0], "tb", rawTenant, rawBucket)
		if runErr != nil || len(dst) != snapshot.Len() {
			panic("raw lookup failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("AppendIndexRawKeys allocated %.2f times, want 0", allocs)
	}
}

func TestStoreExactIndexStats(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 2})
	for i := 0; i < 10; i++ {
		doc := fmt.Sprintf(`{"v":%d}`, i&1)
		if _, err := store.Put(fmt.Sprint(i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.CreateIndex(StoreIndexDefinition{Name: "v", Paths: []string{"/v"}})
	if err != nil {
		t.Fatal(err)
	}
	if info, err = store.BackfillIndex(info.Name, 0); err != nil || info.State != StoreIndexReady {
		t.Fatalf("BackfillIndex = (%+v,%v)", info, err)
	}
	stats, err := store.IndexStats(info.Name)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Fingerprints != 2 || stats.ChunkWords != 10 || stats.CandidateRows != 10 ||
		stats.EstimatedBytes == 0 || stats.PackedBytes == 0 || stats.DirectoryNodes != 0 {
		t.Fatalf("IndexStats = %+v", stats)
	}
	allocs := testing.AllocsPerRun(100, func() {
		var runErr error
		stats, runErr = store.IndexStats(info.Name)
		if runErr != nil || stats.CandidateRows != 10 {
			panic("IndexStats failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("IndexStats allocated %.2f times, want 0", allocs)
	}
	if _, err := store.IndexStats("missing"); err != ErrStoreIndexNotFound {
		t.Fatalf("missing IndexStats error = %v", err)
	}
}
