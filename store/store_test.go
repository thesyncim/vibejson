package store

import (
	"context"
	"errors"
	"fmt"
	"math/bits"
	"math/rand"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
)

func TestStoreChunkVectorSparseTraversal(t *testing.T) {
	var vector storeChunkVector
	keep := map[uint32]bool{0: true, 31: true, 32: true, 1023: true, 1024: true, 1099: true}
	for id := uint32(0); id < 1100; id++ {
		vector, _ = vector.append(&Chunk{})
	}
	for id := uint32(0); id < 1100; id++ {
		if !keep[id] {
			vector = vector.set(id, nil)
		}
	}
	var got []uint32
	vector.Each(func(id uint32, _ *Chunk) bool {
		got = append(got, id)
		return true
	})
	want := []uint32{0, 31, 32, 1023, 1024, 1099}
	if !slices.Equal(got, want) {
		t.Fatalf("sparse traversal = %v, want %v", got, want)
	}
	for from := uint64(0); from < uint64(vector.Count); from++ {
		wantID, found := uint32(0), false
		for _, id := range want {
			if uint64(id) >= from {
				wantID, found = id, true
				break
			}
		}
		id, chunk, ok := vector.next(from)
		if ok != found || ok && (id != wantID || chunk == nil) {
			t.Fatalf("next(%d) = (%d,%p,%v), want id=%d found=%v", from, id, chunk, ok, wantID, found)
		}
	}
	if id, chunk, ok := vector.next(uint64(vector.Count)); ok || chunk != nil || id != 0 {
		t.Fatalf("terminal next = (%d,%p,%v), want zero miss", id, chunk, ok)
	}
}

func TestStoreCompiledKeyAcrossSnapshotsAndStores(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 2, ShapeTapes: true})
	for key, doc := range map[string]string{
		"":  `{"v":"empty"}`,
		"a": `{"v":"old"}`,
		"b": `{"v":"other"}`,
	} {
		if _, err := store.Put(key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	old, _ := store.Snapshot()
	compiled := old.CompileKey("a")
	empty := old.CompileKey("")
	absent := old.CompileKey("later")
	if raw, ok := old.GetRawKey(compiled); !ok || string(raw.Bytes()) != `{"v":"old"}` {
		t.Fatalf("compiled old read = (%q,%v)", raw.Bytes(), ok)
	}
	if raw, ok := old.GetRawKey(empty); !ok || string(raw.Bytes()) != `{"v":"empty"}` {
		t.Fatalf("compiled empty-key read = (%q,%v)", raw.Bytes(), ok)
	}

	if _, err := store.Put("a", []byte(`{"v":"new"}`)); err != nil {
		t.Fatal(err)
	}
	del47, _ := store.Delete("a")
	if !del47 {
		t.Fatal("delete a")
	}
	if _, err := store.Put("filler", []byte(`{"v":"reuses-slot"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("a", []byte(`{"v":"moved"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Put("later", []byte(`{"v":"appeared"}`)); err != nil {
		t.Fatal(err)
	}
	current, _ := store.Snapshot()
	if raw, ok := current.GetRawKey(compiled); !ok || string(raw.Bytes()) != `{"v":"moved"}` {
		t.Fatalf("compiled moved read = (%q,%v)", raw.Bytes(), ok)
	}
	if raw, ok := current.GetRawKey(absent); !ok || string(raw.Bytes()) != `{"v":"appeared"}` {
		t.Fatalf("compiled absent-then-present read = (%q,%v)", raw.Bytes(), ok)
	}
	if raw, ok := old.GetRawKey(compiled); !ok || string(raw.Bytes()) != `{"v":"old"}` {
		t.Fatalf("compiled retained-snapshot read = (%q,%v)", raw.Bytes(), ok)
	}

	other := newStore(Options{})
	if _, err := other.Put("a", []byte(`{"v":"other-store"}`)); err != nil {
		t.Fatal(err)
	}
	if raw, ok := other.GetRawKey(compiled); !ok || string(raw.Bytes()) != `{"v":"other-store"}` {
		t.Fatalf("cross-Store fallback = (%q,%v)", raw.Bytes(), ok)
	}
}

func TestStoreCompiledKeySteadyAllocs(t *testing.T) {
	store := newStore(Options{ShapeTapes: true})
	if _, err := store.Put("key", []byte(`{"value":1}`)); err != nil {
		t.Fatal(err)
	}
	snapshot, _ := store.Snapshot()
	key := snapshot.CompileKey("key")
	if allocs := testing.AllocsPerRun(1000, func() {
		if raw, ok := snapshot.GetRawKey(key); !ok || len(raw.Bytes()) == 0 {
			panic("compiled key miss")
		}
	}); allocs != 0 {
		t.Fatalf("Snapshot.GetRawKey allocated %.2f times, want 0", allocs)
	}
}

func TestStoreCompiledKeyMutationDifferential(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 7, ShapeTapes: true})
	const keyCount = 96
	keys := make([]string, keyCount)
	compiled := make([]Key, keyCount)
	for i := range keys {
		keys[i] = fmt.Sprintf("key-%02d", i)
		if i%3 != 0 {
			if _, err := store.Put(keys[i], []byte(fmt.Sprintf(`{"step":0,"key":%d}`, i))); err != nil {
				t.Fatal(err)
			}
		}
	}
	for i := range keys {
		compiled[i] = store.CompileKey(keys[i])
	}

	rng := rand.New(rand.NewSource(23))
	for step := 1; step <= 4000; step++ {
		i := rng.Intn(keyCount)
		if rng.Intn(4) == 0 {
			store.Delete(keys[i])
		} else {
			doc := fmt.Sprintf(`{"step":%d,"key":%d}`, step, i)
			if _, err := store.Put(keys[i], []byte(doc)); err != nil {
				t.Fatal(err)
			}
		}
		if step%37 == 0 {
			j := rng.Intn(keyCount)
			compiled[j] = store.CompileKey(keys[j])
		}
		if step%19 != 0 {
			continue
		}
		snapshot, _ := store.Snapshot()
		for j := range keys {
			plain, plainOK := snapshot.GetRaw(keys[j])
			fast, fastOK := snapshot.GetRawKey(compiled[j])
			if fastOK != plainOK || fastOK && string(fast.Bytes()) != string(plain.Bytes()) {
				t.Fatalf("step %d key %q: GetRawKey=(%q,%v), GetRaw=(%q,%v)",
					step, keys[j], fast.Bytes(), fastOK, plain.Bytes(), plainOK)
			}
		}
	}
}

func TestStoreCoverageSparsePagesAndClone(t *testing.T) {
	ids := []uint32{1, (1 << storeCoveragePageShift) - 1, 1 << 31, ^uint32(0)}
	var coverage storeCoverage
	for _, id := range ids {
		if !coverage.mark(id) || coverage.mark(id) {
			t.Fatalf("mark(%d) did not have set semantics", id)
		}
	}
	if len(coverage.pages) != 3 {
		t.Fatalf("sparse coverage pages = %d, want 3", len(coverage.pages))
	}
	clone := coverage.clone()
	for _, id := range ids {
		if !coverage.has(id) || !clone.has(id) {
			t.Fatalf("coverage or clone lost %d", id)
		}
	}
	for _, id := range ids {
		if !coverage.unmark(id) || coverage.unmark(id) {
			t.Fatalf("unmark(%d) did not have set semantics", id)
		}
	}
	if coverage.pages != nil {
		t.Fatalf("empty coverage retained %d pages", len(coverage.pages))
	}
	for _, id := range ids {
		if !clone.has(id) {
			t.Fatalf("mutating original changed clone at %d", id)
		}
	}
}

func TestStoreTTLHeapDifferential(t *testing.T) {
	var ttl TTLState
	want := make(map[storeTTLKey]Instant)
	rng := rand.New(rand.NewSource(19))
	for step := 0; step < 10_000; step++ {
		key := storeTTLKey(rng.Intn(200))
		if rng.Intn(4) == 0 {
			removed := ttl.remove(key)
			_, existed := want[key]
			if removed != existed {
				t.Fatalf("step %d remove(%d) = %v, want %v", step, key, removed, existed)
			}
			delete(want, key)
		} else {
			deadline := Instant{sec: int64(rng.Intn(10_000) - 5_000), nsec: int32(rng.Intn(1_000_000_000))}
			ttl.upsert(key, deadline)
			want[key] = deadline
		}
		if len(ttl.Heap) != len(want) || len(ttl.Pos) != len(want) {
			t.Fatalf("step %d heap/pos/model lengths = %d/%d/%d", step, len(ttl.Heap), len(ttl.Pos), len(want))
		}
		for i, entry := range ttl.Heap {
			if ttl.Pos[entry.key] != i || want[entry.key] != entry.Deadline {
				t.Fatalf("step %d inconsistent heap entry %d: %+v", step, i, entry)
			}
			if i > 0 && entry.Deadline.before(ttl.Heap[(i-1)/4].Deadline) {
				t.Fatalf("step %d heap order violation at %d", step, i)
			}
		}
	}
}

func TestStoreMutationSnapshotDifferential(t *testing.T) {
	for _, options := range []Options{
		{ChunkDocuments: 1},
		{ChunkDocuments: 3, IndexOptions: document.IndexOptions{HashKeys: true}},
		{ChunkDocuments: 17, ShapeTapes: true, ValueDict: true, Postings: true},
		{ChunkDocuments: 64, ShapeTapes: true, IndexOptions: document.IndexOptions{HashKeys: true}},
	} {
		t.Run(fmt.Sprintf("chunk=%d/shape=%v/post=%v/dict=%v", options.ChunkDocuments, options.ShapeTapes, options.Postings, options.ValueDict), func(t *testing.T) {
			store := newStore(options)
			want := make(map[string]string)
			rng := rand.New(rand.NewSource(7))
			var held []Snapshot
			for step := 0; step < 4000; step++ {
				key := fmt.Sprintf("key-%03d", rng.Intn(300))
				switch rng.Intn(5) {
				case 0:
					before, _ := store.Snapshot()
					got, _ := store.Delete(key)
					if got != (want[key] != "") {
						t.Fatalf("step %d Delete(%q) = %v, existed %v", step, key, got, want[key] != "")
					}
					delete(want, key)
					if step%251 == 0 {
						held = append(held, before)
					}
				default:
					doc := fmt.Sprintf(`{"id":%d,"key":%q,"g":%d,"value":"v-%03d"}`, step, key, step%11, rng.Intn(80))
					created, err := store.Put(key, []byte(doc))
					if err != nil {
						t.Fatal(err)
					}
					if created != (want[key] == "") {
						t.Fatalf("step %d Put(%q) created=%v, existed=%v", step, key, created, want[key] != "")
					}
					want[key] = doc
				}
				if step%97 == 0 {
					snap51, _ := store.Snapshot()
					checkStoreSnapshot(t, snap51, want)
				}
			}
			snap50, _ := store.Snapshot()
			checkStoreSnapshot(t, snap50, want)
			for _, snapshot := range held {
				// Holding old versions while the writer churns is the lifetime
				// assertion; a traversal under checkptr/race must remain sound.
				count := 0
				snapshot.Range(func(_ string, value vibejson.RawValue) bool {
					if !vibejson.Valid(value.Bytes()) {
						t.Fatal("held snapshot contains invalid JSON")
					}
					count++
					return true
				})
				if count != snapshot.Len() {
					t.Fatalf("held snapshot Range count = %d, Len = %d", count, snapshot.Len())
				}
			}
		})
	}
}

func TestStoreMutationReusesOnlyLiveImmutableStorage(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 8; i++ {
		doc := fmt.Sprintf(`{"id":%d,"group":1,"active":true,"name":"old"}`, i)
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	lookupSource := func(key string) []byte {
		state := store.state.Load()
		loc, ok := storeKeyLookup(state.keys, maphashString(state.seed, key), key)
		if !ok {
			t.Fatalf("missing key %q", key)
		}
		chunk := state.Chunks.Get(loc.Chunk)
		return chunk.Docs.RawAt(int(chunk.Ord[loc.Slot]))
	}
	before, _ := store.Snapshot()
	beforeChunk := before.state.Chunks.Get(0)
	beforeSources := make([][]byte, 8)
	for i := range beforeSources {
		beforeSources[i] = lookupSource(fmt.Sprintf("k%d", i))
		ord := int(beforeChunk.Ord[i])
		ref := beforeChunk.Docs.ShapeTapeRefAt(ord)
		if ref.Rec == nil || !ref.Narrow {
			t.Fatalf("slot %d was not promoted to a narrow shape tape", i)
		}
		if cap(beforeChunk.Docs.docs[ord].Entries) != 0 {
			t.Fatalf("slot %d narrow tape retained %d unused classic entries", i, cap(beforeChunk.Docs.docs[ord].Entries))
		}
		if cap(ref.Rec.Fields) != len(ref.Rec.Fields) || cap(ref.Rec.table) != len(ref.Rec.table) {
			t.Fatalf("slot %d shape retained oversized fields/table: %d/%d and %d/%d",
				i, len(ref.Rec.Fields), cap(ref.Rec.Fields), len(ref.Rec.table), cap(ref.Rec.table))
		}
	}
	if got := len(beforeChunk.Docs.shapes.shapes); got != 1 {
		t.Fatalf("compiled shapes = %d, want 1", got)
	}

	replacement := []byte(`{"id":3,"group":9,"active":false,"name":"new"}`)
	if created, err := store.Put("k3", replacement); err != nil || created {
		t.Fatalf("Put update = (%v,%v), want (false,nil)", created, err)
	}
	replacement[7] = '8'
	after, _ := store.Snapshot()
	afterChunk := after.state.Chunks.Get(0)
	for i := 0; i < 8; i++ {
		current := lookupSource(fmt.Sprintf("k%d", i))
		if i == 3 {
			if &current[0] == &beforeSources[i][0] {
				t.Fatal("replacement reused the old source")
			}
			continue
		}
		if &current[0] != &beforeSources[i][0] {
			t.Fatalf("unchanged slot %d copied its source", i)
		}
	}
	if &afterChunk.Docs.narrow[0] == &beforeChunk.Docs.narrow[0] {
		t.Fatal("new chunk reused the published narrow-value slab")
	}
	if raw, ok := before.GetRaw("k3"); !ok || string(raw.Bytes()) != `{"id":3,"group":1,"active":true,"name":"old"}` {
		t.Fatalf("old snapshot changed after update: %q, %v", raw.Bytes(), ok)
	}
	if raw, ok := after.GetRaw("k3"); !ok || string(raw.Bytes()) != `{"id":3,"group":9,"active":false,"name":"new"}` {
		t.Fatalf("new snapshot aliases caller input: %q, %v", raw.Bytes(), ok)
	}

	del49, _ := store.Delete("k4")
	if !del49 {
		t.Fatal("Delete(k4) missed")
	}
	deleted, _ := store.Snapshot()
	for i := 0; i < 8; i++ {
		if i == 4 {
			continue
		}
		current := lookupSource(fmt.Sprintf("k%d", i))
		state := after.state
		loc, _ := storeKeyLookup(state.keys, maphashString(state.seed, fmt.Sprintf("k%d", i)), fmt.Sprintf("k%d", i))
		chunk := state.Chunks.Get(loc.Chunk)
		prior := chunk.Docs.RawAt(int(chunk.Ord[loc.Slot]))
		if &current[0] != &prior[0] {
			t.Fatalf("delete copied surviving source %d", i)
		}
	}
	if _, ok := deleted.GetRaw("k4"); ok {
		t.Fatal("deleted snapshot retained k4")
	}
	if _, ok := after.GetRaw("k4"); !ok {
		t.Fatal("older snapshot lost k4")
	}

	// Replace the complete live A layout with B. The final cache must contain
	// only B: sharing live immutable records cannot turn into shape history.
	churn := newStore(Options{ChunkDocuments: 3, ShapeTapes: true})
	for i := 0; i < 3; i++ {
		_, _ = churn.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"a":%d,"x":true}`, i)))
	}
	oldRec := churn.state.Load().Chunks.Get(0).Docs.shapes.shapes[0]
	for i := 0; i < 3; i++ {
		if _, err := churn.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"b":%d,"y":false}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	currentShapes := churn.state.Load().Chunks.Get(0).Docs.shapes.shapes
	if len(currentShapes) != 1 || currentShapes[0] == oldRec {
		t.Fatalf("live shape cache retained obsolete records: %p old=%p", currentShapes[0], oldRec)
	}
}

func checkStoreSnapshot(t testing.TB, snapshot Snapshot, want map[string]string) {
	t.Helper()
	if snapshot.Len() != len(want) {
		t.Fatalf("Snapshot.Len = %d, want %d", snapshot.Len(), len(want))
	}
	seen := make(map[string]bool, len(want))
	snapshot.Range(func(key string, value vibejson.RawValue) bool {
		if seen[key] {
			t.Fatalf("Range repeated %q", key)
		}
		seen[key] = true
		if string(value.Bytes()) != want[key] {
			t.Fatalf("Range(%q) = %s, want %s", key, value.Bytes(), want[key])
		}
		return true
	})
	for key, doc := range want {
		raw, ok := snapshot.GetRaw(key)
		if !ok || string(raw.Bytes()) != doc {
			t.Fatalf("GetRaw(%q) = (%s,%v), want (%s,true)", key, raw.Bytes(), ok, doc)
		}
		index, ok := snapshot.Get(key)
		if !ok || string(index.Root().Raw().Bytes()) != doc {
			t.Fatalf("Get(%q) mismatch", key)
		}
	}
}

func TestStoreInvalidPutRollbackAndChunkReuse(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 4, ShapeTapes: true, Postings: true})
	if _, err := store.Put("bad", []byte(`{"x":`)); err == nil {
		t.Fatal("invalid Put succeeded")
	}
	if store.Len() != 0 || store.Generation() != 0 {
		t.Fatalf("failed Put changed visible state: Len=%d Generation=%d", store.Len(), store.Generation())
	}
	for i := 0; i < 100; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"n":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	snap48, _ := store.Snapshot()
	high := snap48.state.Chunks.Count
	old, _ := store.Snapshot()
	for i := 0; i < 100; i++ {
		del55, _ := store.Delete(fmt.Sprintf("k%d", i))
		if !del55 {
			t.Fatal("delete miss")
		}
	}
	if stats := store.Stats(); stats.Chunks != 0 || stats.ChunkHighWater != high {
		t.Fatalf("post-delete stats = %+v, want zero live chunks and high-water %d", stats, high)
	}
	for i := 0; i < 100; i++ {
		if _, err := store.Put(fmt.Sprintf("r%d", i), []byte(fmt.Sprintf(`{"n":%d}`, -i))); err != nil {
			t.Fatal(err)
		}
	}
	snap54, _ := store.Snapshot()
	if got := snap54.state.Chunks.Count; got != high {
		t.Fatalf("delete/insert churn grew chunk address space from %d to %d", high, got)
	}
	if stats := store.Stats(); stats.Chunks != high || stats.ReusableChunks != 0 {
		t.Fatalf("post-reuse stats = %+v, want %d full chunks", stats, high)
	}
	if old.Len() != 100 {
		t.Fatalf("old snapshot Len = %d, want 100", old.Len())
	}
}

func TestStoreAddressSpaceOverflow(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 1})
	store.mu.Lock()
	state, err := store.initLocked()
	if err != nil {
		t.Fatal(err)
	}
	next := *state
	next.Chunks.Count = ^uint32(0)
	store.state.Store(&next)
	store.mu.Unlock()
	if _, err := store.Put("overflow", []byte(`{"v":1}`)); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("Put overflow error = %v, want %v", err, ErrTooLarge)
	}
}

func TestStoreSnapshotReadSteadyAllocs(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 8, ShapeTapes: true})
	for i := 0; i < 32; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"n":%d,"s":"x"}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, _ := store.Snapshot()
	if _, ok := snapshot.GetRaw("k17"); !ok {
		t.Fatal("warm GetRaw miss")
	}
	allocs := testing.AllocsPerRun(100, func() {
		if raw, ok := snapshot.GetRaw("k17"); !ok || len(raw.Bytes()) == 0 {
			panic("GetRaw miss")
		}
	})
	if allocs != 0 {
		t.Fatalf("Snapshot.GetRaw allocated %.2f times, want 0", allocs)
	}
	allocs = testing.AllocsPerRun(100, func() {
		count := 0
		snapshot.Range(func(_ string, _ vibejson.RawValue) bool {
			count++
			return true
		})
		if count != snapshot.Len() {
			panic("Range count")
		}
	})
	if allocs != 0 {
		t.Fatalf("Snapshot.Range allocated %.2f times, want 0", allocs)
	}

	base := time.Now().Add(24 * time.Hour)
	deadlineOK53, _ := store.SetDeadline("k17", base)
	if !deadlineOK53 {
		t.Fatal("warm SetDeadline miss")
	}
	allocs = testing.AllocsPerRun(100, func() {
		deadlineOK52, _ := store.SetDeadline("k17", base.Add(time.Second))
		if !deadlineOK52 {
			panic("SetDeadline miss")
		}
	})
	if allocs != 0 {
		t.Fatalf("warmed SetDeadline allocated %.2f times, want 0", allocs)
	}
	var stats Stats
	allocs = testing.AllocsPerRun(100, func() {
		stats = store.Stats()
	})
	if allocs != 0 || uint64(stats.Keys) != store.Len() {
		t.Fatalf("Store.Stats = (%+v, %.2f allocs), want current zero-allocation counters", stats, allocs)
	}
}

func TestStoreConcurrentSnapshots(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 8, ShapeTapes: true, ValueDict: true, Postings: true})
	for i := 0; i < 64; i++ {
		_, _ = store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"n":%d,"s":"same"}`, i)))
	}
	var wg sync.WaitGroup
	for reader := 0; reader < 8; reader++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 1000; i++ {
				snapshot, _ := store.Snapshot()
				if raw, ok := snapshot.GetRaw(fmt.Sprintf("k%d", i%64)); ok && !vibejson.Valid(raw.Bytes()) {
					t.Error("reader observed invalid JSON")
				}
			}
		}()
	}
	for i := 0; i < 1000; i++ {
		key := fmt.Sprintf("k%d", i%64)
		if i%7 == 0 {
			store.Delete(key)
		} else if _, err := store.Put(key, []byte(fmt.Sprintf(`{"n":%d,"s":"same"}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	wg.Wait()
}

func TestStoreTTLChangeCancelAndSnapshotIsolation(t *testing.T) {
	var store Store
	for _, key := range []string{"a", "b", "c"} {
		if _, err := store.Put(key, []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	base := time.Now().Add(time.Hour)
	deadlineOK58, _ := store.SetDeadline("a", base.Add(3*time.Second))
	deadlineOK62, _ := store.SetDeadline("b", base.Add(2*time.Second))
	if !deadlineOK58 || !deadlineOK62 {
		t.Fatal("SetDeadline miss")
	}
	// Change in both heap directions and cancel without leaving stale nodes.
	store.SetDeadline("a", base.Add(time.Second))
	store.SetDeadline("b", base.Add(4*time.Second))
	persistOK57, _ := store.Persist("b")
	persistOK61, _ := store.Persist("b")
	if !persistOK57 || persistOK61 {
		t.Fatal("Persist contract")
	}
	far := time.Date(2500, time.January, 2, 3, 4, 5, 6, time.UTC)
	store.SetDeadline("b", far)
	if got, ok := store.Deadline("b"); !ok || !got.Equal(far) {
		t.Fatalf("far Deadline = (%v,%v), want %v", got, ok, far)
	}
	store.Persist("b")
	store.SetDeadline("c", base.Add(2*time.Second))
	if stats := store.Stats(); stats.ExpiringKeys != 2 {
		t.Fatalf("TTL stats = %+v, want 2 expiring keys", stats)
	}
	before, _ := store.Snapshot()
	if got := store.ExpireDue(base.Add(1500*time.Millisecond), 1); got != 1 {
		t.Fatalf("ExpireDue = %d, want 1", got)
	}
	if _, ok := store.GetRaw("a"); ok {
		t.Fatal("expired a remains current")
	}
	if _, ok := before.GetRaw("a"); !ok {
		t.Fatal("old snapshot lost expired a")
	}
	if got := store.ExpireDue(base.Add(10*time.Second), 0); got != 1 {
		t.Fatalf("second ExpireDue = %d, want c only", got)
	}
	if _, ok := store.GetRaw("b"); !ok {
		t.Fatal("Persisted b expired")
	}
	if len(store.ttl.Heap) != 0 || len(store.ttl.Pos) != 0 {
		t.Fatalf("TTL state retained stale entries: heap=%d pos=%d", len(store.ttl.Heap), len(store.ttl.Pos))
	}
	if _, ok := store.NextExpiration(); ok {
		t.Fatal("NextExpiration survived empty TTL heap")
	}
}

func TestStoreExpiryBatchSinglePublication(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 8, ShapeTapes: true, Postings: true})
	base := time.Now().Add(time.Hour)
	for i := 0; i < 8; i++ {
		key := fmt.Sprintf("k%d", i)
		if _, err := store.Put(key, []byte(fmt.Sprintf(`{"v":%d}`, i))); err != nil {
			t.Fatal(err)
		}
		deadlineOK56, _ := store.SetDeadline(key, base.Add(time.Duration(i%2)*time.Second))
		if !deadlineOK56 {
			t.Fatal("SetDeadline miss")
		}
	}
	before, _ := store.Snapshot()
	generation := store.Generation()
	if got := store.ExpireDue(base.Add(500*time.Millisecond), 0); got != 4 {
		t.Fatalf("ExpireDue = %d, want 4", got)
	}
	if got := store.Generation(); got != generation+1 {
		t.Fatalf("batch publication advanced generation by %d, want 1", got-generation)
	}
	if store.Len() != 4 || before.Len() != 8 {
		t.Fatalf("current/old lengths = %d/%d, want 4/8", store.Len(), before.Len())
	}
	for i := 0; i < 8; i++ {
		_, current := store.GetRaw(fmt.Sprintf("k%d", i))
		if current != (i%2 == 1) {
			t.Fatalf("k%d current presence = %v", i, current)
		}
	}
}

func TestStoreRunExpiryDeadlineDriven(t *testing.T) {
	var store Store
	if _, err := store.Put("key", []byte(`{"v":1}`)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		store.RunExpiry(ctx, time.Millisecond)
		close(done)
	}()
	deadlineOK60, _ := store.SetDeadline("key", time.Now().Add(20*time.Millisecond))
	if !deadlineOK60 {
		t.Fatal("SetDeadline miss")
	}
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := store.GetRaw("key"); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("deadline-driven expiry did not publish")
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("RunExpiry did not stop on cancellation")
	}
}

func TestStoreOnlinePostingsIndex(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 4, ShapeTapes: true})
	for i := 0; i < 14; i++ {
		_, _ = store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"g":%d,"v":%d}`, i%3, i)))
	}
	old, _ := store.Snapshot()
	info, err := store.AddIndex("search", IndexPostings)
	if err != nil || info.State != IndexBuilding {
		t.Fatalf("AddIndex = (%+v,%v)", info, err)
	}
	needle := refIndex(t, `1`)
	wantContains := []string{"k1", "k4", "k7", "k10", "k13"}
	prefix := []string{"keep"}
	if got, err := store.AppendWhereContainsKeys(prefix, "g", []byte(`{"bad":`)); err == nil || !slices.Equal(got, prefix) {
		t.Fatalf("invalid contains = (%v,%v), want unchanged prefix and error", got, err)
	}
	snap59, _ := store.Snapshot()
	if got := snap59.AppendWhereContainsIndexKeys(nil, "g", needle); !slices.Equal(got, wantContains) {
		t.Fatalf("building-index contains = %v, want %v", got, wantContains)
	}
	// A write into an uncovered chunk dual-maintains and covers it.
	if _, err := store.Put("k0", []byte(`{"g":9,"v":0}`)); err != nil {
		t.Fatal(err)
	}
	previous := uint32(0)
	for info.State != IndexReady {
		info, err = store.BackfillIndex("search", 1)
		if err != nil {
			t.Fatal(err)
		}
		if info.CoveredChunks < previous || info.CoveredChunks > info.TotalChunks {
			t.Fatalf("invalid progress %+v", info)
		}
		previous = info.CoveredChunks
	}
	current, _ := store.Snapshot()
	current.state.Chunks.Each(func(_ uint32, chunk *Chunk) bool {
		if !chunk.Docs.Postings {
			t.Fatal("ready index left an uncovered chunk")
		}
		return true
	})
	if got := current.AppendIndexes(nil); len(got) != 1 || got[0].State != IndexReady {
		t.Fatalf("Snapshot indexes = %+v", got)
	}
	if stats := store.Stats(); stats.Indexes != 1 || stats.IndexedChunks != int(stats.Chunks) {
		t.Fatalf("ready index stats = %+v", stats)
	}
	keys := make([]string, 0, current.Len())
	keys = current.AppendWhereExistsKeys(keys, "v")
	if len(keys) != current.Len() {
		t.Fatalf("exists keys = %d, want %d", len(keys), current.Len())
	}
	contains := make([]string, 0, current.Len())
	contains = current.AppendWhereContainsIndexKeys(contains, "g", needle)
	if !slices.Equal(contains, wantContains) {
		t.Fatalf("ready-index contains = %v, want %v", contains, wantContains)
	}
	if err := store.DropIndex("search"); err != nil {
		t.Fatal(err)
	}
	if got := current.AppendIndexes(nil); len(got) != 1 || got[0].Name != "search" {
		t.Fatalf("old snapshot lost index metadata: %+v", got)
	}
	snap65, _ := store.Snapshot()
	if got := snap65.AppendIndexes(nil); len(got) != 0 {
		t.Fatalf("dropped index remains logical: %+v", got)
	}
	if !store.Stats().IndexReclaiming {
		t.Fatal("last index drop did not expose reclamation state")
	}
	for done := false; !done; {
		_, done = store.ReclaimIndexes(1)
	}
	snap64, _ := store.Snapshot()
	snap64.state.Chunks.Each(func(_ uint32, chunk *Chunk) bool {
		if chunk.Docs.Postings {
			t.Fatal("reclamation left postings")
		}
		return true
	})
	snap63, _ := store.Snapshot()
	if got := snap63.AppendWhereContainsIndexKeys(nil, "g", needle); !slices.Equal(got, wantContains) {
		t.Fatalf("post-reclaim scan contains = %v, want %v", got, wantContains)
	}
	// Pre-DDL snapshots retain their original physical representation and data.
	if old.Len() != 14 {
		t.Fatalf("old snapshot Len = %d", old.Len())
	}
}

func TestStoreIndexedSnapshotProbeSteadyAllocs(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 4, ShapeTapes: true})
	for i := 0; i < 32; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"g":%d,"v":%d}`, i%3, i))); err != nil {
			t.Fatal(err)
		}
	}
	needle := refIndex(t, `1`)
	scan, _ := store.Snapshot()
	info, err := store.AddIndex("search", IndexPostings)
	if err != nil {
		t.Fatal(err)
	}
	for info.State != IndexReady {
		info, err = store.BackfillIndex("search", 0)
		if err != nil {
			t.Fatal(err)
		}
	}
	indexed, _ := store.Snapshot()
	for _, test := range []struct {
		name     string
		snapshot Snapshot
	}{
		{"scan", scan},
		{"indexed", indexed},
	} {
		t.Run(test.name, func(t *testing.T) {
			exists := make([]string, 0, test.snapshot.Len())
			contains := make([]string, 0, test.snapshot.Len())
			exists = test.snapshot.AppendWhereExistsKeys(exists, "v")
			contains = test.snapshot.AppendWhereContainsIndexKeys(contains, "g", needle)
			if len(exists) != test.snapshot.Len() || len(contains) == 0 {
				t.Fatalf("warm probes returned exists=%d contains=%d", len(exists), len(contains))
			}
			if allocs := testing.AllocsPerRun(100, func() {
				exists = test.snapshot.AppendWhereExistsKeys(exists[:0], "v")
			}); allocs != 0 {
				t.Fatalf("AppendWhereExistsKeys allocated %.2f times, want 0", allocs)
			}
			if allocs := testing.AllocsPerRun(100, func() {
				contains = test.snapshot.AppendWhereContainsIndexKeys(contains[:0], "g", needle)
			}); allocs != 0 {
				t.Fatalf("AppendWhereContainsIndexKeys allocated %.2f times, want 0", allocs)
			}
		})
	}
}

func TestStoreSharedIndexAndReclaimRestart(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 2})
	for i := 0; i < 12; i++ {
		_, _ = store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"v":%d}`, i)))
	}
	a, err := store.AddIndex("a", IndexPostings)
	if err != nil {
		t.Fatal(err)
	}
	a, err = store.BackfillIndex("a", 2)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.AddIndex("b", IndexPostings)
	if err != nil || a.CoveredChunks != b.CoveredChunks {
		t.Fatalf("shared coverage a=%+v b=%+v err=%v", a, b, err)
	}
	for b.State != IndexReady {
		b, err = store.BackfillIndex("b", 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	snap67, _ := store.Snapshot()
	infos := snap67.AppendIndexes(nil)
	if len(infos) != 2 || infos[0].State != IndexReady || infos[1].State != IndexReady {
		t.Fatalf("shared indexes not ready: %+v", infos)
	}
	if err := store.DropIndex("a"); err != nil {
		t.Fatal(err)
	}
	if store.Stats().IndexReclaiming {
		t.Fatal("reclamation started with one logical consumer")
	}
	if err := store.DropIndex("b"); err != nil {
		t.Fatal(err)
	}
	if rebuilt, done := store.ReclaimIndexes(1); rebuilt != 1 || done {
		t.Fatalf("first reclaim = (%d,%v), want (1,false)", rebuilt, done)
	}
	c, err := store.AddIndex("c", IndexPostings)
	if err != nil {
		t.Fatal(err)
	}
	if store.Stats().IndexReclaiming {
		t.Fatal("new index did not cancel reclamation")
	}
	for c.State != IndexReady {
		c, err = store.BackfillIndex("c", 1)
		if err != nil {
			t.Fatal(err)
		}
	}
	if stats := store.Stats(); stats.IndexedChunks != int(stats.Chunks) {
		t.Fatalf("restart coverage stats = %+v", stats)
	}
}

func TestStoreIndexBackfillBudgetIncludesCoveredCandidates(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 1})
	for i := 0; i < 100; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"v":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	info, err := store.AddIndex("search", IndexPostings)
	if err != nil {
		t.Fatal(err)
	}
	// Concurrent writes cover the first 99 chunks before backfill visits them.
	// A budget of one must still examine only one start-snapshot candidate per
	// call instead of scanning through all 99 to find the remaining rebuild.
	for i := 0; i < 99; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(`{"v":%d}`, i+1000))); err != nil {
			t.Fatal(err)
		}
	}
	build := store.indexes["search"]
	for wantCursor := uint64(1); wantCursor <= 99; wantCursor++ {
		generation := store.Generation()
		info, err = store.BackfillIndex("search", 1)
		if err != nil {
			t.Fatal(err)
		}
		if build.cursor != wantCursor {
			t.Fatalf("call %d cursor=%d, want %d", wantCursor, build.cursor, wantCursor)
		}
		if store.Generation() != generation {
			t.Fatalf("covered-only call %d published a redundant generation", wantCursor)
		}
		if info.State != IndexBuilding {
			t.Fatalf("call %d state=%v, want Building", wantCursor, info.State)
		}
	}
	info, err = store.BackfillIndex("search", 1)
	if err != nil || info.State != IndexReady {
		t.Fatalf("final BackfillIndex = (%+v,%v)", info, err)
	}
	if !build.all || build.coverage.pages != nil || build.scan.root != nil || build.scan.Count != 0 || build.cursor != 0 {
		t.Fatalf("ready build retained progress state: all=%v coverage-pages=%d scan=%d cursor=%d", build.all, len(build.coverage.pages), build.scan.Count, build.cursor)
	}
}

func TestStoreOwnershipOptionsAndEmptyKey(t *testing.T) {
	store := newStore(Options{ChunkDocuments: 2, ShapeTapes: true})
	src := []byte(`{"v":"owned"}`)
	created, err := store.Put("", src)
	if err != nil || !created {
		t.Fatalf("Put empty key = (%v,%v), want (true,nil)", created, err)
	}
	for i := range src {
		src[i] = 'x'
	}
	raw, ok := store.GetRaw("")
	if !ok || string(raw.Bytes()) != `{"v":"owned"}` {
		t.Fatalf("Store retained caller source: (%s,%v)", raw.Bytes(), ok)
	}

	// Options are a construction-time policy. Mutating the public field after
	// initialization must not change the representation of later chunks.
	store.Options.ChunkDocuments = 64
	for i := 0; i < 4; i++ {
		if _, err := store.Put(fmt.Sprintf("k%d", i), []byte(`{"v":1}`)); err != nil {
			t.Fatal(err)
		}
	}
	stats := store.Stats()
	if stats.ChunkDocuments != 2 || stats.Chunks != 3 {
		t.Fatalf("frozen options stats = %+v, want chunk size 2 and 3 chunks", stats)
	}

	invalid := newStore(Options{ChunkDocuments: MaxChunkDocuments + 1})
	if _, err := invalid.Put("k", []byte(`null`)); err == nil {
		t.Fatal("Put accepted invalid Options")
	}
	if _, err := invalid.AddIndex("i", IndexPostings); err == nil {
		t.Fatal("AddIndex accepted invalid Options")
	}
	if invalid.Len() != 0 || invalid.Generation() != 0 {
		t.Fatalf("invalid options published state: Len=%d Generation=%d", invalid.Len(), invalid.Generation())
	}
}

func TestStoreMixedLifecycleDifferential(t *testing.T) {
	type modelEntry struct {
		doc      string
		tag      string
		deadline Instant
		expires  bool
	}
	type heldSnapshot struct {
		snapshot Snapshot
		want     map[string]string
	}

	store := newStore(Options{
		ChunkDocuments: 5,
		ShapeTapes:     true,
		ValueDict:      true,
	})
	want := make(map[string]modelEntry)
	rng := rand.New(rand.NewSource(91))
	base := time.Now().Add(24 * time.Hour)
	needleEven := refIndex(t, `"even"`)
	var held []heldSnapshot
	activeIndex := ""
	var info IndexInfo

	check := func(step int) {
		t.Helper()
		snapshot, _ := store.Snapshot()
		if snapshot.Len() != len(want) {
			t.Fatalf("step %d Len=%d, want %d", step, snapshot.Len(), len(want))
		}
		wantExists := make([]string, 0, len(want))
		wantEven := make([]string, 0, len(want))
		for key, entry := range want {
			wantExists = append(wantExists, key)
			if entry.tag == "even" {
				wantEven = append(wantEven, key)
			}
			raw, ok := snapshot.GetRaw(key)
			if !ok || string(raw.Bytes()) != entry.doc {
				t.Fatalf("step %d GetRaw(%q)=(%s,%v), want %s", step, key, raw.Bytes(), ok, entry.doc)
			}
		}
		gotExists := snapshot.AppendWhereExistsKeys(make([]string, 0, len(want)), "v")
		gotEven := snapshot.AppendWhereContainsIndexKeys(make([]string, 0, len(want)), "tag", needleEven)
		slices.Sort(gotExists)
		slices.Sort(gotEven)
		slices.Sort(wantExists)
		slices.Sort(wantEven)
		if !slices.Equal(gotExists, wantExists) || !slices.Equal(gotEven, wantEven) {
			t.Fatalf("step %d probes exists=%v/%v even=%v/%v", step, gotExists, wantExists, gotEven, wantEven)
		}
		expiring := 0
		for _, entry := range want {
			if entry.expires {
				expiring++
			}
		}
		stats := store.Stats()
		if stats.Keys != len(want) || stats.ExpiringKeys != expiring {
			t.Fatalf("step %d stats=%+v, want keys=%d expiring=%d", step, stats, len(want), expiring)
		}
	}

	for step := 0; step < 5000; step++ {
		switch step {
		case 100:
			var err error
			activeIndex = "first"
			info, err = store.AddIndex(activeIndex, IndexPostings)
			if err != nil {
				t.Fatal(err)
			}
		case 1800:
			if err := store.DropIndex(activeIndex); err != nil {
				t.Fatal(err)
			}
			activeIndex = ""
		case 1810:
			// Re-add while physical reclamation is in flight. Coverage must
			// restart conservatively and remain exact on mixed chunks.
			var err error
			activeIndex = "second"
			info, err = store.AddIndex(activeIndex, IndexPostings)
			if err != nil {
				t.Fatal(err)
			}
		case 3600:
			if err := store.DropIndex(activeIndex); err != nil {
				t.Fatal(err)
			}
			activeIndex = ""
		}

		key := fmt.Sprintf("k%03d", rng.Intn(240))
		switch rng.Intn(8) {
		case 0:
			deleted, _ := store.Delete(key)
			_, existed := want[key]
			if deleted != existed {
				t.Fatalf("step %d Delete(%q)=%v, want %v", step, key, deleted, existed)
			}
			delete(want, key)
		case 1:
			deadline := base.Add(time.Duration(rng.Intn(7200)) * time.Second)
			set, _ := store.SetDeadline(key, deadline)
			entry, existed := want[key]
			if set != existed {
				t.Fatalf("step %d SetDeadline(%q)=%v, want %v", step, key, set, existed)
			}
			if existed {
				entry.deadline = instantOf(deadline)
				entry.expires = true
				want[key] = entry
			}
		case 2:
			persisted, _ := store.Persist(key)
			entry, existed := want[key]
			wantPersisted := existed && entry.expires
			if persisted != wantPersisted {
				t.Fatalf("step %d Persist(%q)=%v, want %v", step, key, persisted, wantPersisted)
			}
			if wantPersisted {
				entry.expires = false
				want[key] = entry
			}
		case 3:
			now := base.Add(time.Duration(step*2) * time.Second)
			expired := 0
			for modelKey, entry := range want {
				if entry.expires && !entry.deadline.after(instantOf(now)) {
					delete(want, modelKey)
					expired++
				}
			}
			if got := store.ExpireDue(now, 0); got != expired {
				t.Fatalf("step %d ExpireDue=%d, want %d", step, got, expired)
			}
		default:
			tag := "odd"
			if step%2 == 0 {
				tag = "even"
			}
			doc := fmt.Sprintf(`{"v":%d,"tag":%q}`, step, tag)
			_, existed := want[key]
			created, err := store.Put(key, []byte(doc))
			if err != nil || created == existed {
				t.Fatalf("step %d Put(%q)=(%v,%v), existed=%v", step, key, created, err, existed)
			}
			entry := want[key]
			entry.doc = doc
			entry.tag = tag
			want[key] = entry
		}

		if activeIndex != "" && info.State != IndexReady {
			var err error
			info, err = store.BackfillIndex(activeIndex, 1)
			if err != nil {
				t.Fatal(err)
			}
		} else if activeIndex == "" {
			store.ReclaimIndexes(1)
		}
		if step%173 == 0 {
			check(step)
		}
		if step%997 == 0 {
			copyModel := make(map[string]string, len(want))
			for modelKey, entry := range want {
				copyModel[modelKey] = entry.doc
			}
			snap66, _ := store.Snapshot()
			held = append(held, heldSnapshot{snapshot: snap66, want: copyModel})
		}
	}
	check(5000)
	for i, old := range held {
		checkStoreSnapshot(t, old.snapshot, old.want)
		if old.snapshot.Generation() > store.Generation() {
			t.Fatalf("held snapshot %d generation moved forward", i)
		}
	}
}

// A published chunk is immutable and every edit rebuilds it by copy, so the
// working state ingest leaves behind — buildDoc's spill build buffer, the
// arena headroom a spilled document's replacement tape would carry, and the
// value dictionary's first-sighting gate — has no consumer once the chunk is
// live and would otherwise stay pinned for the whole generation's lifetime.
// The release is a space property only, so every assertion here is paired with
// a differential over the exact bytes written.
func TestStoreChunkRetainsNoIngestScratch(t *testing.T) {
	// Seventeen entries — a root plus eight key/value pairs — exceed the
	// sixteen-entry first entry arena initChunkDocSet gives a Store chunk, so
	// each document is guaranteed to take buildDoc's spill path rather than
	// building in the arena tail. The repeated eighteen-byte string span clears
	// the dictionary's length floor and recurs, so the sighting gate is
	// guaranteed to be populated before the chunk is sealed.
	const label = `"aaaaaaaaaaaaaaaa"`
	const tapeEntries = 17
	doc := func(i int) string {
		return fmt.Sprintf(
			`{"id":%d,"tag":%s,"dup":%s,"a":1,"b":2,"c":3,"d":4,"e":5}`,
			i, label, label,
		)
	}
	checkSealed := func(t *testing.T, store *Store, want map[string]string) {
		t.Helper()
		state := store.state.Load()
		for id := uint32(0); id < state.Chunks.Count; id++ {
			chunk := state.Chunks.Get(id)
			if chunk == nil {
				continue
			}
			if chunk.Docs.scratch != nil {
				t.Fatalf("chunk %d retained a %d-entry spill build buffer",
					id, cap(chunk.Docs.scratch))
			}
			if chunk.Docs.valueSeen != nil {
				t.Fatalf("chunk %d retained a %d-hash first-sighting gate",
					id, len(chunk.Docs.valueSeen))
			}
			if chunk.Docs.values.Len() == 0 {
				t.Fatalf("chunk %d interned no value: the sighting gate the "+
					"test means to observe was never populated", id)
			}
			// No document here is stored as a template, so nothing may be
			// appended to the entry arena after the one replacement is parsed:
			// a chunk holding one is holding entries no tape aliases.
			if cap(chunk.Docs.entryChunk) != 0 {
				t.Fatalf("chunk %d retained a %d-entry entry arena holding %d "+
					"committed entries", id,
					cap(chunk.Docs.entryChunk), len(chunk.Docs.entryChunk))
			}
			for left := chunk.Live; left != 0; left &= left - 1 {
				ord := int(chunk.Ord[bits.TrailingZeros64(left)])
				entries := chunk.Docs.docs[ord].Entries
				if len(entries) != tapeEntries {
					t.Fatalf("chunk %d ordinal %d tape = %d entries, want %d",
						id, ord, len(entries), tapeEntries)
				}
				if cap(entries) != len(entries) {
					t.Fatalf("chunk %d ordinal %d retained %d unwritten tape "+
						"entries", id, ord, cap(entries)-len(entries))
				}
			}
		}
		snapshot, _ := store.Snapshot()
		checkStoreSnapshot(t, snapshot, want)
	}

	schema, err := CompileSchema(SchemaDefinition{
		Root:   SchemaObject,
		Fields: []SchemaField{{Path: "/id", Types: SchemaInteger, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Both build paths, because buildDocSchema specializes buildDoc rather than
	// branching inside it and so owns a second copy of the spill tail.
	for _, variant := range []struct {
		name    string
		options Options
	}{
		{"schemaless", Options{ChunkDocuments: 4, ValueDict: true}},
		{"schema", Options{ChunkDocuments: 4, ValueDict: true, Schema: schema}},
	} {
		t.Run(variant.name, func(t *testing.T) {
			store := newStore(variant.options)
			want := make(map[string]string, 10)
			for i := 0; i < 10; i++ {
				key := fmt.Sprintf("k%d", i)
				if _, err := store.Put(key, []byte(doc(i))); err != nil {
					t.Fatal(err)
				}
				want[key] = doc(i)
			}
			checkSealed(t, store, want)

			// A replacement is the one rebuild that parses a document, and a
			// delete is the one that parses none; both must publish a sealed
			// chunk, and the surviving rows carried in by reference must keep
			// the exact tapes they were given.
			if created, err := store.Put("k3", []byte(doc(300))); err != nil || created {
				t.Fatalf("Put update = (%v,%v), want (false,nil)", created, err)
			}
			want["k3"] = doc(300)
			checkSealed(t, store, want)

			if deleted, _ := store.Delete("k7"); !deleted {
				t.Fatal("Delete(k7) missed")
			}
			delete(want, "k7")
			checkSealed(t, store, want)
		})
	}

	// A Builder page shares initChunkDocSet but parses every one of its
	// rows, so it keeps the arena growth policy and seals at flush instead —
	// the point past which the page takes no further row. Its documents are
	// compacted into pointer-free owned blocks by Build, so only the ingest
	// state is observable here.
	t.Run("builder", func(t *testing.T) {
		builder, err := NewBuilder(Options{ChunkDocuments: 4, ValueDict: true})
		if err != nil {
			t.Fatal(err)
		}
		want := make(map[string]string, 10)
		for i := 0; i < 10; i++ {
			key := fmt.Sprintf("k%d", i)
			if err := builder.Append(key, []byte(doc(i))); err != nil {
				t.Fatal(err)
			}
			want[key] = doc(i)
		}
		store, err := builder.Build()
		if err != nil {
			t.Fatal(err)
		}
		state := store.state.Load()
		for id := uint32(0); id < state.Chunks.Count; id++ {
			chunk := state.Chunks.Get(id)
			if chunk == nil {
				continue
			}
			if chunk.Docs.scratch != nil {
				t.Fatalf("page %d retained a %d-entry spill build buffer",
					id, cap(chunk.Docs.scratch))
			}
			if chunk.Docs.valueSeen != nil {
				t.Fatalf("page %d retained a %d-hash first-sighting gate",
					id, len(chunk.Docs.valueSeen))
			}
			if chunk.Docs.values.Len() == 0 {
				t.Fatalf("page %d interned no value: the sighting gate the "+
					"test means to observe was never populated", id)
			}
		}
		snapshot, _ := store.Snapshot()
		checkStoreSnapshot(t, snapshot, want)
	})
}
