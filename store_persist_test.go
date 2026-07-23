package simdjson

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"slices"
	"testing"
	"time"

	"github.com/thesyncim/simdjson/document"
)

func buildStorePersistFixture(t testing.TB) (*Store, map[string]string, map[string]time.Time) {
	t.Helper()
	options := StoreOptions{
		ChunkDocuments: 3,
		ShapeTapes:     true,
		ValueDict:      true,
		IndexOptions:   document.IndexOptions{HashKeys: true, MaxDepth: 37},
	}
	builder, err := NewStoreBuilder(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.CreateIndex(StoreIndexDefinition{
		Name: "country_status", Paths: []string{"/profile/geo/country", "/status"},
	}); err != nil {
		t.Fatal(err)
	}
	want := make(map[string]string)
	for i := 0; i < 12; i++ {
		country := []string{"PT", "US", "DE"}[i%3]
		status := []string{"active", "idle"}[i%2]
		key := fmt.Sprintf("key:%02d", i)
		doc := fmt.Sprintf(`{"id":%d,"profile":{"geo":{"country":%q}},"status":%q,"label":"shared"}`, i, country, status)
		if err := builder.Append(key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
		want[key] = doc
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	info, err := store.AddIndex("search", StoreIndexPostings)
	if err != nil {
		t.Fatal(err)
	}
	for info.State != StoreIndexReady {
		info, err = store.BackfillIndex(info.Name, 2)
		if err != nil {
			t.Fatal(err)
		}
	}
	// Remove one complete page so persistence must retain a sparse high-water
	// vector and its reusable empty id rather than compacting stable addresses.
	for i := 3; i < 6; i++ {
		key := fmt.Sprintf("key:%02d", i)
		if !store.Delete(key) {
			t.Fatalf("Delete(%q) missed", key)
		}
		delete(want, key)
	}
	deadlines := map[string]time.Time{
		"key:00": time.Date(2101, 2, 3, 4, 5, 6, 7, time.UTC),
		"key:08": time.Date(2102, 3, 4, 5, 6, 7, 8, time.FixedZone("ignored-on-reopen", 3600)),
	}
	for key, deadline := range deadlines {
		if !store.SetDeadline(key, deadline) {
			t.Fatalf("SetDeadline(%q) missed", key)
		}
		deadlines[key] = deadline.UTC()
	}
	return store, want, deadlines
}

func TestStorePersistRoundTripIndexesTTLAndMutation(t *testing.T) {
	store, want, deadlines := buildStorePersistFixture(t)
	beforeStats := store.Stats()
	var image bytes.Buffer
	n, err := store.WriteTo(&image)
	if err != nil {
		t.Fatal(err)
	}
	if n != int64(image.Len()) {
		t.Fatalf("WriteTo bytes = %d, buffer = %d", n, image.Len())
	}

	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Options != store.Options {
		t.Fatalf("Options = %+v, want %+v", reopened.Options, store.Options)
	}
	afterStats := reopened.Stats()
	if afterStats.MappedImageBytes != uint64(image.Len()) || afterStats.ExternalKeyBytes == 0 ||
		afterStats.ExternalDocumentBytes == 0 {
		t.Fatalf("mapped Stats = %+v, want image=%d and external key metadata", afterStats, image.Len())
	}
	afterComparable := afterStats
	afterComparable.MappedImageBytes = 0
	afterComparable.ExternalKeyBytes = 0
	afterComparable.ExternalDocumentBytes = 0
	afterComparable.ExternalIndexBytes = 0
	beforeStats.MappedImageBytes = 0
	beforeStats.ExternalKeyBytes = 0
	beforeStats.ExternalDocumentBytes = 0
	beforeStats.ExternalIndexBytes = 0
	if afterComparable != beforeStats {
		t.Fatalf("Stats = %+v, want operational fields %+v", afterStats, beforeStats)
	}
	checkStoreSnapshot(t, reopened.Snapshot(), want)
	for key, deadline := range deadlines {
		got, ok := reopened.Deadline(key)
		if !ok || !got.Equal(deadline) {
			t.Errorf("Deadline(%q) = (%v,%v), want %v", key, got, ok, deadline)
		}
	}

	infos := reopened.Snapshot().AppendIndexes(nil)
	if len(infos) != 2 || infos[0].Name != "country_status" || infos[1].Name != "search" {
		t.Fatalf("indexes = %+v", infos)
	}
	for _, info := range infos {
		if info.State != StoreIndexReady || info.CoveredChunks != info.TotalChunks {
			t.Fatalf("index not Ready: %+v", info)
		}
	}
	keys, err := reopened.Snapshot().AppendIndexRawKeys(nil, "country_status", []byte(`"PT"`), []byte(`"active"`))
	if err != nil || !slices.Equal(keys, []string{"key:00", "key:06"}) {
		t.Fatalf("compound lookup = (%v,%v)", keys, err)
	}
	needle := testScalarIndex(t, `"active"`)
	keys = reopened.AppendWhereContainsIndexKeys(nil, "status", needle)
	if !slices.Equal(keys, []string{"key:00", "key:02", "key:06", "key:08", "key:10"}) {
		t.Fatalf("posting lookup = %v", keys)
	}

	compiled := reopened.CompileKey("key:00")
	copyBuf := make([]byte, 3, len(want["key:00"])+3)
	copy(copyBuf, "pre")
	copyBuf, ok := reopened.AppendRawKey(copyBuf, compiled)
	if !ok || string(copyBuf) != "pre"+want["key:00"] {
		t.Fatalf("AppendRawKey = (%q,%v)", copyBuf, ok)
	}
	unchanged := len(copyBuf)
	if copyBuf, ok = reopened.AppendRaw(copyBuf, "missing"); ok || len(copyBuf) != unchanged {
		t.Fatalf("AppendRaw miss changed destination: (%d,%v)", len(copyBuf), ok)
	}

	retained := reopened.Snapshot()
	if _, err := reopened.Put("key:00", []byte(`{"id":0,"profile":{"geo":{"country":"FR"}},"status":"idle"}`)); err != nil {
		t.Fatal(err)
	}
	if !reopened.Delete("key:06") {
		t.Fatal("Delete after OpenStore missed")
	}
	if _, err := reopened.Put("new", []byte(`{"id":99,"profile":{"geo":{"country":"PT"}},"status":"active"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, ok := retained.GetRaw("key:00"); !ok || string(raw.Bytes()) != want["key:00"] {
		t.Fatalf("mutation changed retained mapped snapshot: (%q,%v)", raw.Bytes(), ok)
	}
	keys, err = reopened.Snapshot().AppendIndexRawKeys(nil, "country_status", []byte(`"PT"`), []byte(`"active"`))
	if err != nil || !slices.Equal(keys, []string{"new"}) {
		t.Fatalf("post-open exact maintenance = (%v,%v)", keys, err)
	}

	var second bytes.Buffer
	if _, err := reopened.WriteTo(&second); err != nil {
		t.Fatal(err)
	}
	secondStore, err := OpenStore(second.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if raw, ok := secondStore.GetRaw("new"); !ok || string(raw.Bytes()) != `{"id":99,"profile":{"geo":{"country":"PT"}},"status":"active"}` {
		t.Fatalf("second generation value = (%q,%v)", raw.Bytes(), ok)
	}
}

func TestStorePersistEmptyAndBuildingIndex(t *testing.T) {
	empty := NewStore(StoreOptions{ChunkDocuments: 7, IndexOptions: document.IndexOptions{MaxDepth: -1}})
	var image bytes.Buffer
	if _, err := empty.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Len() != 0 || reopened.Generation() != 0 || reopened.Options != (StoreOptions{
		ChunkDocuments: 7, IndexOptions: document.IndexOptions{MaxDepth: -1},
	}) {
		t.Fatalf("empty reopened Store = len %d generation %d options %+v", reopened.Len(), reopened.Generation(), reopened.Options)
	}
	if _, err := reopened.Put("first", []byte(`1`)); err != nil {
		t.Fatal(err)
	}

	building := NewStore(StoreOptions{ChunkDocuments: 2})
	_, _ = building.Put("a", []byte(`{"v":1}`))
	_, _ = building.Put("b", []byte(`{"v":2}`))
	if _, err := building.CreateIndex(StoreIndexDefinition{Name: "v", Paths: []string{"/v"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := building.WriteTo(&bytes.Buffer{}); !errors.Is(err, ErrStorePersistIndexBuilding) {
		t.Fatalf("building-index WriteTo error = %v", err)
	}
}

func TestOpenStoreRejectsMalformedFramingAndManifest(t *testing.T) {
	store := NewStore(StoreOptions{ChunkDocuments: 2, ShapeTapes: true})
	_, _ = store.Put("a", []byte(`{"v":1}`))
	_, _ = store.Put("b", []byte(`{"v":2}`))
	var buf bytes.Buffer
	if _, err := store.WriteTo(&buf); err != nil {
		t.Fatal(err)
	}
	image := buf.Bytes()
	for cut := 0; cut < len(image); cut++ {
		if _, err := OpenStore(image[:cut]); err == nil {
			t.Fatalf("truncation at %d bytes opened", cut)
		}
	}

	mutate := func(name string, fn func([]byte)) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			bad := append([]byte(nil), image...)
			fn(bad)
			if _, err := OpenStore(bad); err == nil {
				t.Fatal("malformed image opened")
			}
		})
	}
	mutate("header magic", func(b []byte) { b[0] ^= 0xff })
	mutate("header version", func(b []byte) { binary.LittleEndian.PutUint32(b[8:12], storePersistVersion+1) })
	mutate("header reserved", func(b []byte) { b[12] = 1 })
	mutate("footer magic", func(b []byte) { b[len(b)-storePersistFooterLen] ^= 0xff })
	mutate("footer reserved", func(b []byte) { b[len(b)-1] = 1 })
	mutate("manifest checksum", func(b []byte) {
		off := binary.LittleEndian.Uint64(b[len(b)-storePersistFooterLen+8:])
		b[off+storePersistManifestFixed] ^= 0xff
	})
	mutate("impossible live count", func(b []byte) {
		footer := b[len(b)-storePersistFooterLen:]
		off := binary.LittleEndian.Uint64(footer[8:16])
		manifest := b[off : off+binary.LittleEndian.Uint64(footer[16:24])]
		binary.LittleEndian.PutUint32(manifest[44:48], 3)
		binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	})
	mutate("manifest reserved", func(b []byte) {
		footer := b[len(b)-storePersistFooterLen:]
		off := binary.LittleEndian.Uint64(footer[8:16])
		manifest := b[off : off+binary.LittleEndian.Uint64(footer[16:24])]
		manifest[64] = 1
		binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	})
	mutate("count allocation bomb", func(b []byte) {
		footer := b[len(b)-storePersistFooterLen:]
		off := binary.LittleEndian.Uint64(footer[8:16])
		manifest := b[off : off+binary.LittleEndian.Uint64(footer[16:24])]
		binary.LittleEndian.PutUint32(manifest[40:44], math.MaxUint32)
		binary.LittleEndian.PutUint32(manifest[44:48], 0)
		binary.LittleEndian.PutUint32(manifest[60:64], math.MaxUint32)
		binary.LittleEndian.PutUint64(manifest[32:40], 0)
		binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	})
	mutate("unaligned chunk", func(b []byte) {
		footer := b[len(b)-storePersistFooterLen:]
		off := binary.LittleEndian.Uint64(footer[8:16])
		manifest := b[off : off+binary.LittleEndian.Uint64(footer[16:24])]
		chunk := storePersistManifestFixed
		imageOffset := binary.LittleEndian.Uint64(manifest[chunk+16 : chunk+24])
		binary.LittleEndian.PutUint64(manifest[chunk+16:chunk+24], imageOffset+1)
		binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	})
	mutate("invalid stable slots", func(b []byte) {
		footer := b[len(b)-storePersistFooterLen:]
		off := binary.LittleEndian.Uint64(footer[8:16])
		manifest := b[off : off+binary.LittleEndian.Uint64(footer[16:24])]
		chunk := storePersistManifestFixed
		binary.LittleEndian.PutUint64(manifest[chunk+8:chunk+16], 0)
		binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	})
}

func TestStoreAppendRawSteadyAllocs(t *testing.T) {
	store := NewStore(StoreOptions{})
	doc := `{"value":"caller-owned"}`
	if _, err := store.Put("key", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	snapshot := store.Snapshot()
	key := snapshot.CompileKey("key")
	dst := make([]byte, 0, len(doc))
	if allocs := testing.AllocsPerRun(1000, func() {
		out, ok := snapshot.AppendRawKey(dst[:0], key)
		if !ok || len(out) != len(doc) {
			panic("AppendRawKey failed")
		}
	}); allocs != 0 {
		t.Fatalf("AppendRawKey allocated %.2f times, want 0", allocs)
	}
}

type storePersistShortWriter struct{}

func (storePersistShortWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	return len(p) - 1, nil
}

func TestStorePersistShortWrite(t *testing.T) {
	store := NewStore(StoreOptions{})
	if _, err := store.Put("key", []byte(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	n, err := store.WriteTo(storePersistShortWriter{})
	if !errors.Is(err, io.ErrShortWrite) || n != storePersistHeaderLen-1 {
		t.Fatalf("WriteTo short write = (%d,%v), want (%d,%v)", n, err, storePersistHeaderLen-1, io.ErrShortWrite)
	}
}
