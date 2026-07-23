package slopjson

import (
	"bytes"
	"fmt"
	"hash/maphash"
	"runtime"
	"slices"
	"sync"
	"testing"
)

func TestStoreMappedKeysPointerFreeBaseAndOverlay(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 4, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 17; i++ {
		if err := builder.Append(fmt.Sprintf("key:%02d", i), []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	base := reopened.state.Load()
	if base.keys != nil {
		t.Fatal("OpenStore rebuilt a per-key pointer HAMT")
	}
	if base.baseKeys == nil || base.baseKeys.count != 17 || base.baseKeys.keyRefCount() != 17 {
		t.Fatalf("mapped base = %+v", base.baseKeys)
	}
	base.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		if chunk.keys != nil || chunk.mappedKeys != base.baseKeys {
			t.Fatalf("mapped chunk retained heap string table")
		}
		if chunk.docs.mappedDocs != base.mappedDocs || chunk.docs.docs != nil ||
			chunk.docs.narrow != nil || chunk.docs.tapeRefs != nil {
			t.Fatalf("mapped chunk retained pointer-rich document tables")
		}
		return true
	})

	old := reopened.Snapshot()
	if created, err := reopened.Put("key:03", []byte(`{"n":300}`)); err != nil || created {
		t.Fatalf("replace = (%v, %v)", created, err)
	}
	if !reopened.Delete("key:04") {
		t.Fatal("delete mapped-base key missed")
	}
	if created, err := reopened.Put("new", []byte(`{"n":900}`)); err != nil || !created {
		t.Fatalf("insert = (%v, %v)", created, err)
	}
	current := reopened.state.Load()
	if current.baseKeys != base.baseKeys || current.keys == nil {
		t.Fatal("mutation did not preserve compact base plus bounded overlay")
	}
	if raw, ok := old.GetRaw("key:04"); !ok || string(raw.Bytes()) != `{"n":4}` {
		t.Fatalf("retained base snapshot = (%q, %v)", raw.Bytes(), ok)
	}
	if _, ok := reopened.GetRaw("key:04"); ok {
		t.Fatal("deleted mapped-base key remained visible")
	}
	if raw, ok := reopened.GetRaw("key:03"); !ok || string(raw.Bytes()) != `{"n":300}` {
		t.Fatalf("replacement = (%q, %v)", raw.Bytes(), ok)
	}
	if raw, ok := reopened.GetRaw("new"); !ok || string(raw.Bytes()) != `{"n":900}` {
		t.Fatalf("overlay insert = (%q, %v)", raw.Bytes(), ok)
	}

	// Aggressive collection cannot finalize the external table while either a
	// current or retained state can still reach it.
	for i := 0; i < 4; i++ {
		runtime.GC()
		if raw, ok := old.GetRaw("key:03"); !ok || string(raw.Bytes()) != `{"n":3}` {
			t.Fatalf("old snapshot after GC = (%q, %v)", raw.Bytes(), ok)
		}
	}
}

func TestStoreMappedBaseConcurrentReadersAndWriter(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 64, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 128; i++ {
		if err := builder.Append(fmt.Sprintf("key:%03d", i), []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	store, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	store, err = OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	retained := store.Snapshot()

	start := make(chan struct{})
	var readers sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		readers.Add(1)
		go func() {
			defer readers.Done()
			<-start
			for i := 0; i < 2_000; i++ {
				snapshot := store.Snapshot()
				raw, ok := snapshot.GetRaw("key:002")
				if !ok || len(raw.Bytes()) == 0 {
					t.Errorf("concurrent mapped read missed")
					return
				}
			}
		}()
	}
	close(start)
	for i := 0; i < 500; i++ {
		if _, err := store.Put("key:002", []byte(fmt.Sprintf(`{"n":%d}`, 1_000+i))); err != nil {
			t.Fatal(err)
		}
		if i%50 == 0 {
			runtime.GC()
		}
	}
	readers.Wait()
	if raw, ok := retained.GetRaw("key:002"); !ok || string(raw.Bytes()) != `{"n":2}` {
		t.Fatalf("retained mapped snapshot = (%q, %v)", raw.Bytes(), ok)
	}
}

func TestStoreMappedKeysGroupProbeCollisionDifferential(t *testing.T) {
	const count = 257
	source := make([]byte, 0, count*12)
	mapped, err := newStoreMappedKeys(nil, count, false)
	if err != nil {
		t.Fatal(err)
	}
	defer mapped.release()
	seed := maphash.MakeSeed()
	want := make(map[string]storeLocation, count)
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("collision-key-%03d", i)
		off := len(source)
		source = append(source, key...)
		loc := storeLocation{chunk: uint32(i / 64), slot: uint8(i % 64)}
		mapped.refs[i] = storeMappedKeyRef{off: uint64(off), length: uint32(len(key))}
		mapped.setLocation(uint64(i), loc)
		want[key] = loc
	}
	mapped.source = source
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("collision-key-%03d", i)
		// Retain only three initial hash bits to force long, wrapping clusters;
		// exact spelling must still distinguish every key.
		hash := maphash.String(seed, key) & 7
		if !mapped.insert(hash, uint64(i)) {
			t.Fatalf("insert %q reported duplicate", key)
		}
	}
	for key, loc := range want {
		hash := maphash.String(seed, key) & 7
		if got, ok := mapped.lookup(hash, key); !ok || got != loc {
			t.Fatalf("lookup %q = (%+v, %v), want %+v", key, got, ok, loc)
		}
	}
	if _, ok := mapped.lookup(0, "absent"); ok {
		t.Fatal("absent collision lookup hit")
	}
	if mapped.insert(maphash.String(seed, "collision-key-100")&7, 100) {
		t.Fatal("duplicate insertion succeeded")
	}
}

func TestStoreMappedKeysExactRangeAndIndexKeys(t *testing.T) {
	store, _, _ := buildStorePersistFixture(t)
	var image bytes.Buffer
	if _, err := store.WriteTo(&image); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenStore(image.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	var ranged []string
	reopened.Snapshot().Range(func(key string, _ RawValue) bool {
		ranged = append(ranged, key)
		return true
	})
	rows, err := reopened.Snapshot().AppendIndexRawKeys(nil, "country_status", []byte(`"PT"`), []byte(`"active"`))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(rows, []string{"key:00", "key:06"}) {
		t.Fatalf("index keys = %v", rows)
	}
	if len(ranged) != reopened.Len() {
		t.Fatalf("Range keys = %d, want %d", len(ranged), reopened.Len())
	}
}
