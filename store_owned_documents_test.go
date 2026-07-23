package slopjson

import (
	"bytes"
	"fmt"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStoreBuilderSelectsOwnedNarrowTapeWidths(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		kind uint8
	}{
		{name: "8-bit coordinates", doc: `{"a":1,"b":"x"}`, kind: storeOwnedDocNarrow8},
		{
			name: "9-bit start",
			doc:  fmt.Sprintf(`{"a":"%s","b":"%s"}`, strings.Repeat("a", 128), strings.Repeat("b", 128)),
			kind: storeOwnedDocNarrow9,
		},
		{
			name: "16-bit start",
			doc: fmt.Sprintf(`{"a":"%s","b":"%s","c":"%s","d":"%s"}`,
				strings.Repeat("a", 128), strings.Repeat("b", 128),
				strings.Repeat("c", 128), strings.Repeat("d", 128)),
			kind: storeOwnedDocNarrowLength8,
		},
		{
			name: "wide value",
			doc:  fmt.Sprintf(`{"a":"%s"}`, strings.Repeat("x", 300)),
			kind: storeOwnedDocNarrow,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := buildOwnedLayoutStore(t, tc.doc, 2)
			state := store.state.Load()
			if state.mappedDocs.compactRefs == nil {
				t.Fatal("small owned publication did not select compact row refs")
			}
			for row := uint64(0); row < 2; row++ {
				ref := state.mappedDocs.refAt(row)
				if ref.kind != tc.kind {
					t.Fatalf("row %d tape kind = %d, want %d", row, ref.kind, tc.kind)
				}
			}
			assertOwnedStoreRoundTrip(t, store, "k1", tc.doc, "/a")
		})
	}
}

func TestStoreBuilderSelectsOwnedTemplateSpanWidths(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		kind uint8
	}{
		{
			name: "8-bit coordinates",
			doc:  `{"a":{"x":"short"},"b":{"y":1}}`,
			kind: storeOwnedDocTemplate8,
		},
		{
			name: "16-bit start",
			doc: fmt.Sprintf(`{"a":{"x":"%s"},"b":{"y":"%s"},"c":{"z":"%s"}}`,
				strings.Repeat("a", 110), strings.Repeat("b", 110), strings.Repeat("c", 30)),
			kind: storeOwnedDocTemplateLength8,
		},
		{
			name: "wide child",
			doc:  fmt.Sprintf(`{"a":{"x":"%s"}}`, strings.Repeat("x", 300)),
			kind: storeOwnedDocTemplate,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := buildOwnedLayoutStore(t, tc.doc, 2)
			state := store.state.Load()
			if len(state.mappedDocs.templates) != 1 {
				t.Fatalf("template count = %d, want 1", len(state.mappedDocs.templates))
			}
			for row := uint64(0); row < 2; row++ {
				ref := state.mappedDocs.refAt(row)
				if ref.kind != tc.kind || ref.shapeID != 0 {
					t.Fatalf("row %d ref = %+v, want kind %d template 0", row, ref, tc.kind)
				}
			}
			assertOwnedStoreRoundTrip(t, store, "k1", tc.doc, "/a/x")
		})
	}
}

func TestStoreBuilderTemplatesComposeWithValueDictionary(t *testing.T) {
	const rows = 16
	const want = `"a-repeated-value-longer-than-the-default-floor"`
	doc := `{"profile":{"label":` + want + `},"active":true}`
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 8, ShapeTapes: true, ValueDict: true})
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; row < rows; row++ {
		if err := builder.Append(fmt.Sprintf("k%d", row), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	if len(store.state.Load().mappedDocs.templates) != 1 {
		t.Fatal("value dictionary disabled structural templates")
	}
	pointer := MustCompilePointer("/profile/label")
	values := make([]RawValue, 0, rows)
	values, err = store.Snapshot().AppendPointer(values, pointer)
	if err != nil || len(values) != rows {
		t.Fatalf("template dictionary pointer = (%d, %v)", len(values), err)
	}
	if got := string(values[rows-1].Bytes()); got != want {
		t.Fatalf("template dictionary pointer = %q, want %q", got, want)
	}
	allocs := testing.AllocsPerRun(100, func() {
		values, err = store.Snapshot().AppendPointer(values[:0], pointer)
	})
	if err != nil || allocs != 0 {
		t.Fatalf("template dictionary pointer allocs/error = %.2f/%v", allocs, err)
	}
	assertOwnedStoreRoundTrip(t, store, "k7", doc, "/profile/label")
}

func TestStoreBuilderCompactRefsRecoverTrimmedRoot(t *testing.T) {
	for _, tc := range []struct {
		name, source, root string
	}{
		{name: "shape", source: " \n {\"a\":1,\"b\":2}\t ", root: `{"a":1,"b":2}`},
		{name: "template", source: "\t {\"nested\":{\"a\":1}} \r\n", root: `{"nested":{"a":1}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := buildOwnedLayoutStore(t, tc.source, 2)
			if store.state.Load().mappedDocs.compactRefs == nil {
				t.Fatal("small publication did not select compact refs")
			}
			index, ok := store.Get("k1")
			if !ok || string(index.Root().Raw().Bytes()) != tc.root {
				t.Fatalf("trimmed root = (%q, %v), want %q", index.Root().Raw().Bytes(), ok, tc.root)
			}
			if raw, ok := store.GetRaw("k1"); !ok || string(raw.Bytes()) != tc.source {
				t.Fatalf("exact source = (%q, %v), want %q", raw.Bytes(), ok, tc.source)
			}
			roots, err := store.Snapshot().AppendPointer(nil, MustCompilePointer(""))
			if err != nil || len(roots) != 2 || string(roots[1].Bytes()) != tc.root {
				t.Fatalf("root column = (%q, %v), want %q", roots[1].Bytes(), err, tc.root)
			}
			var image bytes.Buffer
			if _, err := store.WriteTo(&image); err != nil {
				t.Fatal(err)
			}
			reopened, err := OpenStore(image.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			index, ok = reopened.Get("k1")
			if !ok || string(index.Root().Raw().Bytes()) != tc.root {
				t.Fatalf("reopened root = (%q, %v), want %q", index.Root().Raw().Bytes(), ok, tc.root)
			}
		})
	}
}

func TestStoreReclaimsOwnedDocumentBaseAfterAllChunksDetach(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; row < 4; row++ {
		if err := builder.Append(fmt.Sprintf("k%d", row), []byte(fmt.Sprintf(`{"n":%d}`, row))); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	retained := store.Snapshot()
	if state := store.state.Load(); state.mappedDocChunks != 2 || state.mappedDocs == nil {
		t.Fatalf("initial mapped document state = %d/%p", state.mappedDocChunks, state.mappedDocs)
	}
	if _, err := store.Put("k0", []byte(`{"n":10}`)); err != nil {
		t.Fatal(err)
	}
	if state := store.state.Load(); state.mappedDocChunks != 1 || state.mappedDocs == nil {
		t.Fatalf("first detach state = %d/%p", state.mappedDocChunks, state.mappedDocs)
	}
	if !store.Delete("k2") {
		t.Fatal("Delete(k2) missed")
	}
	if state := store.state.Load(); state.mappedDocChunks != 0 || state.mappedDocs != nil {
		t.Fatalf("last detach retained current owner = %d/%p", state.mappedDocChunks, state.mappedDocs)
	}
	if store.Stats().ExternalDocumentBytes != 0 {
		t.Fatal("current generation still reports detached document storage")
	}
	for i := 0; i < 3; i++ {
		runtime.GC()
		if raw, ok := retained.GetRaw("k0"); !ok || string(raw.Bytes()) != `{"n":0}` {
			t.Fatalf("retained k0 after GC = (%q, %v)", raw.Bytes(), ok)
		}
		if raw, ok := retained.GetRaw("k2"); !ok || string(raw.Bytes()) != `{"n":2}` {
			t.Fatalf("retained k2 after GC = (%q, %v)", raw.Bytes(), ok)
		}
	}
}

func TestStoreMaintenanceDetachesOwnedDocumentBase(t *testing.T) {
	t.Run("expiry", func(t *testing.T) {
		store := buildOwnedDetachFixture(t)
		retained := store.Snapshot()
		deadline := time.Unix(2_000_000_000, 0)
		for row := 0; row < 4; row++ {
			if !store.SetDeadline(fmt.Sprintf("k%d", row), deadline) {
				t.Fatalf("SetDeadline(k%d) missed", row)
			}
		}
		if expired := store.ExpireDue(deadline, 0); expired != 4 {
			t.Fatalf("ExpireDue = %d, want 4", expired)
		}
		assertOwnedDocumentBaseDetached(t, store, retained)
	})

	t.Run("posting backfill", func(t *testing.T) {
		store := buildOwnedDetachFixture(t)
		retained := store.Snapshot()
		info, err := store.AddIndex("postings", StoreIndexPostings)
		if err != nil {
			t.Fatal(err)
		}
		if info, err = store.BackfillIndex(info.Name, 0); err != nil || info.State != StoreIndexReady {
			t.Fatalf("BackfillIndex = (%+v, %v)", info, err)
		}
		assertOwnedDocumentBaseDetached(t, store, retained)
	})
}

func buildOwnedDetachFixture(t *testing.T) *Store {
	t.Helper()
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; row < 4; row++ {
		if err := builder.Append(fmt.Sprintf("k%d", row), []byte(fmt.Sprintf(`{"n":%d}`, row))); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertOwnedDocumentBaseDetached(t *testing.T, store *Store, retained Snapshot) {
	t.Helper()
	state := store.state.Load()
	if state.mappedDocChunks != 0 || state.mappedDocs != nil || store.Stats().ExternalDocumentBytes != 0 {
		t.Fatalf("current owner = %d/%p, external=%d", state.mappedDocChunks, state.mappedDocs, store.Stats().ExternalDocumentBytes)
	}
	runtime.GC()
	if raw, ok := retained.GetRaw("k0"); !ok || string(raw.Bytes()) != `{"n":0}` {
		t.Fatalf("retained k0 after GC = (%q, %v)", raw.Bytes(), ok)
	}
}

func buildOwnedLayoutStore(t *testing.T, doc string, rows int) *Store {
	t.Helper()
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 8, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for row := 0; row < rows; row++ {
		if err := builder.Append(fmt.Sprintf("k%d", row), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func assertOwnedStoreRoundTrip(t *testing.T, store *Store, key, want, pointer string) {
	t.Helper()
	index, ok := store.Get(key)
	if !ok || string(index.Root().Raw().Bytes()) != want {
		t.Fatalf("Get(%q) = (%q, %v)", key, index.Root().Raw().Bytes(), ok)
	}
	node, ok, err := index.Pointer(pointer)
	if err != nil || !ok || len(node.Raw().Bytes()) == 0 {
		t.Fatalf("Pointer(%q) = (%q, %v, %v)", pointer, node.Raw().Bytes(), ok, err)
	}
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := reopened.GetRaw(key); !ok || string(raw.Bytes()) != want {
		t.Fatalf("reopened GetRaw(%q) = (%q, %v)", key, raw.Bytes(), ok)
	}
}
