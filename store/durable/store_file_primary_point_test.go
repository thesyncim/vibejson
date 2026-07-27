package durable

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/store"
)

func buildFilePrimaryCorpus(
	t testing.TB, count int,
) (*store.Collection, []string, [][]byte) {
	t.Helper()
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, count)
	values := make([][]byte, count)
	for at := range count {
		keys[at] = fmt.Sprintf("primary-key-%09d", at)
		values[at] = fmt.Appendf(
			nil,
			`{"id":%d,"group":%d,"name":"primary row %d"}`,
			at, at%997, at,
		)
		if err := builder.Append(keys[at], values[at]); err != nil {
			t.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	return built, keys, values
}

func createPrimaryPointFile(
	t testing.TB,
	built *store.Collection,
	options Options,
	name string,
) *os.File {
	t.Helper()
	file, err := os.OpenFile(
		filepath.Join(t.TempDir(), name),
		os.O_RDWR|os.O_CREATE,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	if _, err := CreateFromPrimary(built, file, options); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestFilePrimaryPointReadDifferential100K(t *testing.T) {
	const count = 100_000
	built, keys, values := buildFilePrimaryCorpus(t, count)
	options := Options{
		Backend: BackendPortable, ResidentBytes: 128 << 20,
	}
	legacyFile, err := os.OpenFile(
		filepath.Join(t.TempDir(), "legacy.vibe"),
		os.O_RDWR|os.O_CREATE,
		0o600,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer legacyFile.Close()
	if _, err := CreateFrom(built, legacyFile, options); err != nil {
		t.Fatal(err)
	}
	primaryFile := createPrimaryPointFile(
		t, built, options, "primary.vibe",
	)

	legacy, err := Open(legacyFile, options)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	primary, err := Open(primaryFile, options)
	if err != nil {
		t.Fatal(err)
	}
	defer primary.Close()
	primaryRoot := primary.state.Load().root
	if primaryRoot.PrimaryRoot == (storeio.PageRef{}) ||
		primaryRoot.ChunkDirectory != (storeio.PageRef{}) ||
		primaryRoot.KeyDirectory != (storeio.PageRef{}) ||
		primaryRoot.IndexDirectory != (storeio.PageRef{}) {
		t.Fatalf(
			"primary bulk roots = primary:%+v chunk:%+v key:%+v index:%+v",
			primaryRoot.PrimaryRoot, primaryRoot.ChunkDirectory,
			primaryRoot.KeyDirectory, primaryRoot.IndexDirectory,
		)
	}
	if primary.Len() != count || primary.Stats().VacantChunkSlots != 0 {
		t.Fatalf(
			"primary count/stats = %d/%d, want %d/0",
			primary.Len(), primary.Stats().VacantChunkSlots, count,
		)
	}

	legacySnapshot, err := legacy.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer legacySnapshot.Close()
	primarySnapshot, err := primary.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer primarySnapshot.Close()

	legacyBuffer := make([]byte, 0, 128)
	primaryBuffer := make([]byte, 0, 128)
	legacySnapshotBuffer := make([]byte, 0, 128)
	primarySnapshotBuffer := make([]byte, 0, 128)
	for at, key := range keys {
		legacyValue, legacyOK, legacyErr := legacy.AppendRaw(
			legacyBuffer[:0], key,
		)
		primaryValue, primaryOK, primaryErr := primary.AppendRaw(
			primaryBuffer[:0], key,
		)
		legacySnapshotValue, legacySnapshotOK, legacySnapshotErr :=
			legacySnapshot.AppendRaw(legacySnapshotBuffer[:0], key)
		primarySnapshotValue, primarySnapshotOK, primarySnapshotErr :=
			primarySnapshot.AppendRaw(primarySnapshotBuffer[:0], key)
		if legacyErr != nil || primaryErr != nil ||
			legacySnapshotErr != nil || primarySnapshotErr != nil ||
			!legacyOK || !primaryOK ||
			!legacySnapshotOK || !primarySnapshotOK ||
			!bytes.Equal(legacyValue, values[at]) ||
			!bytes.Equal(primaryValue, legacyValue) ||
			!bytes.Equal(legacySnapshotValue, legacyValue) ||
			!bytes.Equal(primarySnapshotValue, legacyValue) {
			t.Fatalf(
				"point read %d = legacy(%q,%v,%v) primary(%q,%v,%v) "+
					"legacy snapshot(%q,%v,%v) primary snapshot(%q,%v,%v)",
				at,
				legacyValue, legacyOK, legacyErr,
				primaryValue, primaryOK, primaryErr,
				legacySnapshotValue, legacySnapshotOK, legacySnapshotErr,
				primarySnapshotValue, primarySnapshotOK, primarySnapshotErr,
			)
		}
		legacyBuffer = legacyValue
		primaryBuffer = primaryValue
		legacySnapshotBuffer = legacySnapshotValue
		primarySnapshotBuffer = primarySnapshotValue
	}

	for sample := range 10_000 {
		key := fmt.Sprintf("absent-primary-key-%09d", sample*7919)
		if value, ok, err := legacy.AppendRaw(legacyBuffer[:0], key); err != nil || ok || len(value) != 0 {
			t.Fatalf("legacy absent %d = %q,%v,%v", sample, value, ok, err)
		}
		if value, ok, err := primary.AppendRaw(primaryBuffer[:0], key); err != nil || ok || len(value) != 0 {
			t.Fatalf("primary absent %d = %q,%v,%v", sample, value, ok, err)
		}
		if value, ok, err := legacySnapshot.AppendRaw(
			legacySnapshotBuffer[:0], key,
		); err != nil || ok || len(value) != 0 {
			t.Fatalf(
				"legacy snapshot absent %d = %q,%v,%v",
				sample, value, ok, err,
			)
		}
		if value, ok, err := primarySnapshot.AppendRaw(
			primarySnapshotBuffer[:0], key,
		); err != nil || ok || len(value) != 0 {
			t.Fatalf(
				"primary snapshot absent %d = %q,%v,%v",
				sample, value, ok, err,
			)
		}
	}

	if _, err := primary.Put("new", []byte(`{"v":1}`)); !errors.Is(err, ErrPrimaryReadOnly) {
		t.Fatalf("primary Put = %v, want %v", err, ErrPrimaryReadOnly)
	}
	if _, err := primary.Delete(keys[0]); !errors.Is(err, ErrPrimaryReadOnly) {
		t.Fatalf("primary Delete = %v, want %v", err, ErrPrimaryReadOnly)
	}
	if err := primary.Update(func(batch *WriteBatch) error {
		return batch.Put("new", []byte(`{"v":1}`))
	}); !errors.Is(err, ErrPrimaryReadOnly) {
		t.Fatalf("primary Update = %v, want %v", err, ErrPrimaryReadOnly)
	}
}

func TestFilePrimaryResolverAllocatesZero(t *testing.T) {
	built, keys, values := buildFilePrimaryCorpus(t, 2_000)
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
	}
	file := createPrimaryPointFile(t, built, options, "alloc.vibe")
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	state := collection.state.Load()
	buffer := make([]byte, 0, 128)
	target := keys[len(keys)/2]
	if _, ok, err := collection.resolvePrimaryGraph(
		buffer[:0], state, target,
	); err != nil || !ok {
		t.Fatalf("warm primary hit = %v,%v", ok, err)
	}
	if _, ok, err := collection.resolvePrimaryGraph(
		buffer[:0], state, "primary-key-absent",
	); err != nil || ok {
		t.Fatalf("warm primary miss = %v,%v", ok, err)
	}

	var (
		out    []byte
		found  bool
		runErr error
	)
	hitAllocs := testing.AllocsPerRun(1_000, func() {
		out, found, runErr = collection.resolvePrimaryGraph(
			buffer[:0], state, target,
		)
	})
	if runErr != nil || !found || !bytes.Equal(out, values[len(values)/2]) {
		t.Fatalf("primary allocated hit = %q,%v,%v", out, found, runErr)
	}
	if hitAllocs != 0 {
		t.Fatalf("primary hit allocations = %v, want 0", hitAllocs)
	}
	missAllocs := testing.AllocsPerRun(1_000, func() {
		out, found, runErr = collection.resolvePrimaryGraph(
			buffer[:0], state, "primary-key-absent",
		)
	})
	if runErr != nil || found || len(out) != 0 {
		t.Fatalf("primary allocated miss = %q,%v,%v", out, found, runErr)
	}
	if missAllocs != 0 {
		t.Fatalf("primary miss allocations = %v, want 0", missAllocs)
	}
}

type filePrimaryCorruptionRefs struct {
	catalog storeio.PageRef
	tablet  storeio.PageRef
	locator storeio.PageRef
	anchor  storeio.PageRef
	leaf    storeio.PageRef
}

func locateFilePrimaryCorruptionRefs(
	t testing.TB,
	file *os.File,
	root storeio.StateRoot,
	key string,
) filePrimaryCorruptionRefs {
	t.Helper()
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	bounds := storeio.GlobalTabletCatalogBounds{
		StoreID: root.StoreID, SelectedRootGeneration: root.Generation,
		FileEnd: uint64(info.Size()), NextLogicalID: root.NextLogicalID,
	}
	rootPage := readPrimaryOpenTestPage(t, file, root.PrimaryRoot)
	catalog, err := storeio.OpenGlobalTabletCatalogNode(
		rootPage, root.PrimaryRoot, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	keyBytes := []byte(key)
	route := catalog.Route(keyBytes)
	nodePage := readPrimaryOpenTestPage(t, file, route.Ref)
	node, err := storeio.OpenGlobalTabletCatalogNode(
		nodePage, route.Ref, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	if catalog.ChildLevel() == storeio.GlobalTabletCatalogBranch {
		route = node.Route(keyBytes)
		nodePage = readPrimaryOpenTestPage(t, file, route.Ref)
		node, err = storeio.OpenGlobalTabletCatalogNode(
			nodePage, route.Ref, bounds,
		)
		if err != nil {
			t.Fatal(err)
		}
	}
	tabletRoute := node.Route(keyBytes)
	tabletPage := readPrimaryOpenTestPage(t, file, tabletRoute.Ref)
	tablet, err := storeio.OpenGlobalTabletCatalogTabletRoot(
		tabletPage, tabletRoute.Ref, bounds,
	)
	if err != nil {
		t.Fatal(err)
	}
	locator, ok := tablet.LocatorRef()
	if !ok {
		t.Fatal("primary tablet has no locator")
	}
	anchorRoute, ok := tablet.RouteAnchor(keyBytes)
	if !ok {
		t.Fatal("primary tablet has no anchor route")
	}
	anchorPage := readPrimaryOpenTestPage(t, file, anchorRoute.Ref)
	anchor, err := storeio.OpenGlobalTabletCatalogAnchor(
		anchorPage, &tablet, anchorRoute.PageID,
	)
	if err != nil {
		t.Fatal(err)
	}
	leafRoute, ok := anchor.RouteHashed(
		storeio.KeyHashBytes(root.StoreID, keyBytes), keyBytes,
	)
	if !ok {
		t.Fatal("primary anchor has no leaf route")
	}
	return filePrimaryCorruptionRefs{
		catalog: root.PrimaryRoot, tablet: tabletRoute.Ref,
		locator: locator, anchor: anchorRoute.Ref, leaf: leafRoute.Ref,
	}
}

func filePrimaryTypedCorruption(err error) bool {
	return errors.Is(err, storeio.ErrPageCorrupt) ||
		errors.Is(err, storeio.ErrSuperblockCorrupt) ||
		errors.Is(err, storeio.ErrSuperblockNotFound) ||
		errors.Is(err, storeio.ErrGlobalTabletCatalogCorrupt) ||
		errors.Is(err, storeio.ErrSegmentedTabletRouterCorrupt) ||
		errors.Is(err, storeio.ErrCommonPrimaryLeafCorrupt)
}

func TestFilePrimaryPointReadCorruptionFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		selectRef func(filePrimaryCorruptionRefs) storeio.PageRef
		payloadAt int
	}{
		{
			name: "catalog",
			selectRef: func(refs filePrimaryCorruptionRefs) storeio.PageRef {
				return refs.catalog
			},
			payloadAt: 0,
		},
		{
			name: "tablet root",
			selectRef: func(refs filePrimaryCorruptionRefs) storeio.PageRef {
				return refs.tablet
			},
			payloadAt: 0,
		},
		{
			name: "locator",
			selectRef: func(refs filePrimaryCorruptionRefs) storeio.PageRef {
				return refs.locator
			},
			payloadAt: 0,
		},
		{
			name: "anchor",
			selectRef: func(refs filePrimaryCorruptionRefs) storeio.PageRef {
				return refs.anchor
			},
			payloadAt: 6,
		},
		{
			name: "leaf",
			selectRef: func(refs filePrimaryCorruptionRefs) storeio.PageRef {
				return refs.leaf
			},
			payloadAt: 2,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			built, keys, _ := buildFilePrimaryCorpus(t, 2_000)
			options := Options{
				Backend: BackendPortable, ResidentBytes: 32 << 20,
			}
			file := createPrimaryPointFile(
				t, built, options, "corrupt.vibe",
			)
			collection, err := Open(file, options)
			if err != nil {
				t.Fatal(err)
			}
			root := collection.state.Load().root
			target := keys[len(keys)/2]
			refs := locateFilePrimaryCorruptionRefs(
				t, file, root, target,
			)
			if err := collection.Close(); err != nil {
				t.Fatal(err)
			}

			ref := test.selectRef(refs)
			page := readPrimaryOpenTestPage(t, file, ref)
			page[storeio.PageHeaderSize+test.payloadAt] ^= 4
			if _, err := storeio.SealPage(page); err != nil {
				t.Fatal(err)
			}
			writePrimaryOpenTestPage(t, file, ref, page)

			corrupt, openErr := Open(file, options)
			if openErr != nil {
				if !filePrimaryTypedCorruption(openErr) {
					t.Fatalf("Open corruption error = %v", openErr)
				}
				return
			}
			defer corrupt.Close()
			value, found, readErr := corrupt.AppendRaw(nil, target)
			if readErr == nil || found || len(value) != 0 {
				t.Fatalf(
					"corrupt read = %q,%v,%v; want typed failure",
					value, found, readErr,
				)
			}
			if !filePrimaryTypedCorruption(readErr) {
				t.Fatalf("read corruption error = %v", readErr)
			}
		})
	}
}

func TestCreateFromPrimaryRejectsUnsupportedOptions(t *testing.T) {
	built, _, _ := buildFilePrimaryCorpus(t, 1)
	for _, test := range []struct {
		name    string
		options Options
	}{
		{
			name: "indexes",
			options: Options{Indexes: []store.IndexDefinition{
				{Name: "id", Paths: []string{"/id"}},
			}},
		},
		{
			name:    "float64 columns",
			options: Options{Float64Columns: []string{"/id"}},
		},
		{
			name:    "compact",
			options: Options{DocumentFormat: DocumentFormatCompact},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "unsupported-*")
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			if _, err := CreateFromPrimary(
				built, file, test.options,
			); !errors.Is(err, ErrPrimaryCutoverUnsupported) {
				t.Fatalf(
					"CreateFromPrimary = %v, want %v",
					err, ErrPrimaryCutoverUnsupported,
				)
			}
		})
	}
}
