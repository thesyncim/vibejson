package slopjson

import (
	"bytes"
	"errors"
	"fmt"
	"testing"

	"github.com/thesyncim/slopjson/document"
)

func TestStoreBuilderEquivalentAndMutable(t *testing.T) {
	for _, options := range []StoreOptions{
		{ChunkDocuments: 1},
		{ChunkDocuments: 7, ShapeTapes: true},
		{ChunkDocuments: 64, ShapeTapes: true, Postings: true, ValueDict: true,
			IndexOptions: document.IndexOptions{HashKeys: true}},
	} {
		t.Run(fmt.Sprintf("chunk=%d/shape=%v/postings=%v", options.ChunkDocuments, options.ShapeTapes, options.Postings), func(t *testing.T) {
			builder, err := NewStoreBuilder(options)
			if err != nil {
				t.Fatal(err)
			}
			if err := builder.CreateIndex(StoreIndexDefinition{
				Name: "country", Paths: []string{"/profile/geo/country"},
			}); err != nil {
				t.Fatal(err)
			}
			if err := builder.CreateIndex(StoreIndexDefinition{
				Name: "country_active", Paths: []string{"/profile/geo/country", "/active"},
			}); err != nil {
				t.Fatal(err)
			}
			want := make(map[string]string)
			for i := 0; i < 257; i++ {
				key := fmt.Sprintf("key-%03d", i)
				doc := fmt.Sprintf(`{"id":%d,"profile":{"geo":{"country":"c%d"}},"active":%t}`, i, i%9, i%2 == 0)
				input := []byte(doc)
				if err := builder.Append(key, input); err != nil {
					t.Fatal(err)
				}
				clear(input)
				want[key] = doc
			}
			if builder.Len() != len(want) {
				t.Fatalf("builder Len = %d, want %d", builder.Len(), len(want))
			}
			store, err := builder.Build()
			if err != nil {
				t.Fatal(err)
			}
			if store.Len() != len(want) || store.Generation() != 1 {
				t.Fatalf("built Store Len/Generation = %d/%d", store.Len(), store.Generation())
			}
			checkStoreSnapshot(t, store.Snapshot(), want)

			indexes := store.Snapshot().AppendIndexes(nil)
			if len(indexes) != 2 || indexes[0].Name != "country" || indexes[1].Name != "country_active" ||
				indexes[0].State != StoreIndexReady || indexes[0].CoveredChunks != indexes[0].TotalChunks ||
				indexes[1].State != StoreIndexReady || indexes[1].CoveredChunks != indexes[1].TotalChunks {
				t.Fatalf("bulk index = %+v", indexes)
			}
			keys, err := store.Snapshot().AppendIndexRawKeys(nil, "country", []byte(`"c3"`))
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) == 0 {
				t.Fatal("nested index found no c3 documents")
			}
			compound, err := store.Snapshot().AppendIndexRawKeys(nil, "country_active", []byte(`"c3"`), []byte(`false`))
			if err != nil || len(compound) == 0 {
				t.Fatalf("bulk compound index = (%v,%v)", compound, err)
			}

			before := store.Snapshot()
			if _, err := store.Put("key-003", []byte(`{"id":3,"profile":{"geo":{"country":"new"}}}`)); err != nil {
				t.Fatal(err)
			}
			if !store.Delete("key-004") || !store.SetTTL("key-005", 1) {
				t.Fatal("built Store mutation or TTL failed")
			}
			if raw, ok := before.GetRaw("key-003"); !ok || string(raw.Bytes()) != want["key-003"] {
				t.Fatal("post-build mutation changed retained snapshot")
			}
			oldKeys, err := before.AppendIndexRawKeys(nil, "country", []byte(`"c3"`))
			if err != nil || !containsString(oldKeys, "key-003") {
				t.Fatalf("retained bulk index lost old row: (%v,%v)", oldKeys, err)
			}
			newKeys, err := store.Snapshot().AppendIndexRawKeys(nil, "country", []byte(`"new"`))
			if err != nil || len(newKeys) != 1 || newKeys[0] != "key-003" {
				t.Fatalf("bulk index did not dual-maintain update: (%v,%v)", newKeys, err)
			}
		})
	}
}

const storeBuilderTemplateRows = 32

func buildNestedTemplateStore(t *testing.T) *Store {
	t.Helper()
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.CreateIndex(StoreIndexDefinition{Name: "country", Paths: []string{"/profile/geo/country"}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < storeBuilderTemplateRows; i++ {
		doc := fmt.Sprintf(`{"id":%d,"profile":{"geo":{"country":"c%d"}},"active":%t}`, i, i%4, i&1 == 0)
		if err := builder.Append(fmt.Sprintf("k%02d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func TestStoreBuilderInternsNestedStructuralTemplates(t *testing.T) {
	const rows = storeBuilderTemplateRows
	store := buildNestedTemplateStore(t)
	state := store.state.Load()
	if state.mappedDocs == nil || len(state.mappedDocs.templates) != 1 {
		t.Fatalf("nested template catalog = %+v", state.mappedDocs)
	}
	state.chunks.each(func(id uint32, chunk *storeChunk) bool {
		for row := 0; row < chunk.docs.Len(); row++ {
			ref := state.mappedDocs.refAt(chunk.docs.mappedBase + uint64(row))
			if !storeOwnedDocIsTemplate(ref.kind) || ref.shapeID != 0 || len(chunk.docs.docAt(row).entries) != 0 {
				t.Fatalf("chunk %d row %d ref/index = %+v/%+v", id, row, ref, chunk.docs.docAt(row))
			}
		}
		return true
	})

	pointer := MustCompilePointer("/profile/geo/country")
	values := make([]RawValue, 0, rows)
	values, err := store.Snapshot().AppendPointer(values, pointer)
	if err != nil || len(values) != rows || string(values[7].Bytes()) != `"c3"` {
		t.Fatalf("template pointer = (%d,%q,%v)", len(values), values[7].Bytes(), err)
	}
	keys := make([]string, 0, rows)
	keys, err = store.Snapshot().AppendIndexRawKeys(keys, "country", []byte(`"c3"`))
	if err != nil || len(keys) != rows/4 {
		t.Fatalf("template exact index = (%d,%v)", len(keys), err)
	}
	index, ok := store.Get("k07")
	if !ok {
		t.Fatal("navigable template Get missed")
	}
	node, ok, err := index.PointerCompiled(pointer)
	if err != nil || !ok || string(node.Raw().Bytes()) != `"c3"` {
		t.Fatalf("widened template pointer = (%q,%v,%v)", node.Raw().Bytes(), ok, err)
	}
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	before := store.Snapshot()
	if _, err := store.Put("k07", []byte(`{"id":7,"profile":{"geo":{"country":"new"}},"active":false}`)); err != nil {
		t.Fatal(err)
	}
	if raw, ok := before.GetRaw("k07"); !ok || !bytes.Contains(raw.Bytes(), []byte(`"c3"`)) {
		t.Fatalf("retained template snapshot = (%q,%v)", raw.Bytes(), ok)
	}
	if keys, err = store.Snapshot().AppendIndexRawKeys(keys[:0], "country", []byte(`"new"`)); err != nil || len(keys) != 1 || keys[0] != "k07" {
		t.Fatalf("mutated template index = (%v,%v)", keys, err)
	}

	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := reopened.GetRaw("k07"); !ok || !bytes.Contains(raw.Bytes(), []byte(`"c3"`)) {
		t.Fatalf("reopened template = (%q,%v)", raw.Bytes(), ok)
	}
}

func TestStoreBuilderNestedStructuralTemplateAllocs(t *testing.T) {
	store := buildNestedTemplateStore(t)
	pointer := MustCompilePointer("/profile/geo/country")
	values := make([]RawValue, 0, storeBuilderTemplateRows)
	var err error
	values, err = store.Snapshot().AppendPointer(values, pointer)
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		values, err = store.Snapshot().AppendPointer(values[:0], pointer)
	})
	if err != nil || allocs != 0 {
		t.Fatalf("template pointer allocs/error = %.2f/%v", allocs, err)
	}

	keys := make([]string, 0, storeBuilderTemplateRows)
	keys, err = store.Snapshot().AppendIndexRawKeys(keys, "country", []byte(`"c3"`))
	if err != nil {
		t.Fatal(err)
	}
	allocs = testing.AllocsPerRun(100, func() {
		keys, err = store.Snapshot().AppendIndexRawKeys(keys[:0], "country", []byte(`"c3"`))
	})
	if err != nil || allocs != 0 {
		t.Fatalf("template exact index allocs/error = %.2f/%v", allocs, err)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestStoreBuilderCompactsKeyDirectory(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range []struct{ key, json string }{
		{"", `0`},
		{"alpha", `1`},
		{"a-key-longer-than-the-first-arena-growth", `2`},
	} {
		if err := builder.Append(row.key, []byte(row.json)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	state := store.state.Load()
	if state.keys != nil || state.baseKeys == nil || state.baseKeys.count != 3 ||
		state.baseKeys.sourceBlock == nil {
		t.Fatalf("published key directory retained builder graph: %+v", state)
	}
	if state.baseKeys.dense == nil || state.baseKeys.compact != nil || state.baseKeys.refs != nil ||
		state.baseKeys.denseShift != 1 {
		t.Fatalf("power-of-two chunks did not select dense key refs: %+v", state.baseKeys)
	}
	state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		if chunk.keys != nil || chunk.keyBytes != nil || chunk.mappedKeys != state.baseKeys {
			t.Fatalf("chunk retained heap key storage: %+v", chunk)
		}
		return true
	})
	for key, want := range map[string]string{
		"": `0`, "alpha": `1`, "a-key-longer-than-the-first-arena-growth": `2`,
	} {
		if raw, ok := store.GetRaw(key); !ok || string(raw.Bytes()) != want {
			t.Fatalf("GetRaw(%q) = (%q,%v), want (%q,true)", key, raw.Bytes(), ok, want)
		}
	}
	if state.baseKeys.sourceBlock.OutsideHeap() && store.Stats().ExternalKeyBytes == 0 {
		t.Fatal("owned external key bytes not reported")
	}

	before := store.Snapshot()
	if !store.Delete("alpha") {
		t.Fatal("Delete(alpha) missed compact base")
	}
	if created, err := store.Put("later", []byte(`3`)); err != nil || !created {
		t.Fatalf("Put(later) = (%v,%v)", created, err)
	}
	if raw, ok := before.GetRaw("alpha"); !ok || string(raw.Bytes()) != `1` {
		t.Fatalf("retained compact snapshot = (%q,%v)", raw.Bytes(), ok)
	}
	if _, ok := store.GetRaw("alpha"); ok {
		t.Fatal("deleted compact-base key remained visible")
	}
}

func TestStoreBuilderCompactsNonPowerOfTwoKeyDirectory(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 3})
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; row < 7; row++ {
		if err := builder.Append(fmt.Sprintf("key-%d", row), []byte(fmt.Sprintf(`{"n":%d}`, row))); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	base := store.state.Load().baseKeys
	if base == nil || base.compact == nil || base.dense != nil || base.refs != nil {
		t.Fatalf("non-power-of-two chunks did not select explicit compact refs: %+v", base)
	}
	for row := 0; row < 7; row++ {
		key := fmt.Sprintf("key-%d", row)
		if raw, ok := store.GetRaw(key); !ok || string(raw.Bytes()) != fmt.Sprintf(`{"n":%d}`, row) {
			t.Fatalf("GetRaw(%q) = (%q, %v)", key, raw.Bytes(), ok)
		}
	}
}

func TestStoreBuilderKeyTableCollisionAndAllocs(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 8})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"alpha", "beta", "gamma"} {
		if err := builder.Append(key, []byte(`0`)); err != nil {
			t.Fatal(err)
		}
	}

	// A single artificial fingerprint and probe bucket exercises the exact-key
	// comparison rather than relying on maphash collisions occurring by chance.
	const collision = uint64(0x123456789abcdef0)
	builder.keyTable = storeBuilderKeyTable{}
	builder.keyTable.reserve(builder, 3)
	for row := range uint64(3) {
		builder.keyTable.insert(collision, row)
	}
	for _, key := range []string{"alpha", "beta", "gamma"} {
		if !builder.keyTable.contains(builder, collision, key) {
			t.Fatalf("collision chain missed %q", key)
		}
	}
	if builder.keyTable.contains(builder, collision, "delta") {
		t.Fatal("collision chain accepted a different key")
	}

	allocs := testing.AllocsPerRun(100, func() {
		clear(builder.keyTable.slots)
		for row := range uint64(3) {
			builder.keyTable.insert(collision, row)
		}
		for _, key := range []string{"alpha", "beta", "gamma"} {
			if !builder.keyTable.contains(builder, collision, key) {
				panic("key table lookup invariant")
			}
		}
	})
	if allocs != 0 {
		t.Fatalf("reserved key insert/lookup allocations = %.2f, want 0", allocs)
	}
}

func TestStoreBuilderSharesImmutableShapesAcrossChunks(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if err := builder.Append(string(rune('a'+i)), []byte(`{"a":1,"b":"x"}`)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	var shared *shapeRecord
	store.state.Load().chunks.each(func(id uint32, chunk *storeChunk) bool {
		if len(chunk.docs.mappedShapes) != 1 || len(chunk.docs.shapes.shapes) != 0 {
			t.Fatalf("chunk %d mapped/heap shapes = %d/%d, want 1/0", id, len(chunk.docs.mappedShapes), len(chunk.docs.shapes.shapes))
		}
		rec := chunk.docs.mappedShapes[0]
		if shared == nil {
			shared = rec
		} else if rec != shared {
			t.Fatalf("chunk %d recompiled immutable shape", id)
		}
		for row := 0; row < chunk.docs.Len(); row++ {
			if chunk.docs.shapeTapeRefAt(row).rec != shared {
				t.Fatalf("chunk %d row %d did not use shared shape", id, row)
			}
		}
		return true
	})
}

func TestStoreBuilderReservesOneBoundedSourceArena(t *testing.T) {
	const rows = 64
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: rows, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	document := []byte(`{"a":"0123456789abcdef","b":123456789}`)
	for i := 0; i < rows; i++ {
		if err := builder.Append(string(rune(i+1)), document); err != nil {
			t.Fatal(err)
		}
	}
	chunk := builder.chunks.get(0)
	if chunk == nil || len(chunk.docs.srcChunk) != rows*len(document) {
		t.Fatalf("source arena length = %d, want %d", len(chunk.docs.srcChunk), rows*len(document))
	}
	if cap(chunk.docs.srcChunk) < rows*len(document) || cap(chunk.docs.srcChunk) > rows*len(document)*5/4 {
		t.Fatalf("source arena capacity = %d, want [%d,%d]", cap(chunk.docs.srcChunk), rows*len(document), rows*len(document)*5/4)
	}
}

func TestStoreBuilderErrorsAndEmptyStore(t *testing.T) {
	if _, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 65}); err == nil {
		t.Fatal("invalid chunk bound accepted")
	}
	var nilBuilder *StoreBuilder
	if !errors.Is(nilBuilder.Append("k", []byte(`null`)), ErrStoreBuilderClosed) {
		t.Fatal("nil Append error")
	}
	if _, err := nilBuilder.Build(); !errors.Is(err, ErrStoreBuilderClosed) {
		t.Fatal("nil Build error")
	}

	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 3, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append("bad", []byte(`{"broken"`)); err == nil {
		t.Fatal("invalid JSON accepted")
	}
	if builder.Len() != 0 {
		t.Fatalf("invalid append changed Len to %d", builder.Len())
	}
	if err := builder.Append("bad", []byte(`{"valid":true}`)); err != nil {
		t.Fatal(err)
	}
	if err := builder.Append("bad", []byte(`{"duplicate":true}`)); !errors.Is(err, ErrStoreDuplicateKey) {
		t.Fatalf("duplicate error = %v", err)
	}
	if builder.Len() != 1 {
		t.Fatalf("duplicate changed Len to %d", builder.Len())
	}
	if err := builder.CreateIndex(StoreIndexDefinition{Name: "", Paths: []string{"/valid"}}); err == nil {
		t.Fatal("invalid builder index accepted")
	}
	if err := builder.CreateIndex(StoreIndexDefinition{Name: "valid", Paths: []string{"/valid"}}); err != nil {
		t.Fatal(err)
	}
	if err := builder.CreateIndex(StoreIndexDefinition{Name: "valid", Paths: []string{"/other"}}); !errors.Is(err, ErrStoreIndexExists) {
		t.Fatalf("duplicate builder index error = %v", err)
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append("later", []byte(`null`)); !errors.Is(err, ErrStoreBuilderClosed) {
		t.Fatalf("Append after Build error = %v", err)
	}
	if _, err := builder.Build(); !errors.Is(err, ErrStoreBuilderClosed) {
		t.Fatalf("second Build error = %v", err)
	}
	if err := builder.CreateIndex(StoreIndexDefinition{Name: "later", Paths: []string{"/valid"}}); !errors.Is(err, ErrStoreBuilderClosed) {
		t.Fatalf("CreateIndex after Build error = %v", err)
	}
	if raw, ok := store.GetRaw("bad"); !ok || string(raw.Bytes()) != `{"valid":true}` {
		t.Fatalf("built value = (%q,%v)", raw.Bytes(), ok)
	}
	keys, err := store.Snapshot().AppendIndexRawKeys(nil, "valid", []byte(`true`))
	if err != nil || len(keys) != 1 || keys[0] != "bad" {
		t.Fatalf("built exact index = (%v,%v)", keys, err)
	}

	emptyBuilder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 2})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := emptyBuilder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Len() != 0 || empty.Generation() != 0 {
		t.Fatalf("empty Len/Generation = %d/%d", empty.Len(), empty.Generation())
	}
	if _, err := empty.Put("first", []byte(`1`)); err != nil {
		t.Fatal(err)
	}
}
