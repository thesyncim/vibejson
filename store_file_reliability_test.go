package slopjson

import (
	"fmt"
	"math/bits"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesyncim/slopjson/internal/storeio"
)

func TestFileStoreRandomizedHeapDifferentialAndReopen(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-differential-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.ResidentBytes = 8 << 20
	options.BufferCount = 128
	options.MaxRetiredExtents = 2048
	options.Indexes = []StoreIndexDefinition{{Name: "status", Paths: []string{"/status"}}}
	fileStore, err := CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	heapStore := NewStore(options.Store)
	rng := rand.New(rand.NewSource(0x5eed))
	base := time.Now().Add(2 * time.Hour).Truncate(time.Second)

	for step := range 160 {
		key := fmt.Sprintf("key-%02d", rng.Intn(32))
		switch rng.Intn(6) {
		case 0, 1:
			status := []string{"active", "idle", "paused"}[rng.Intn(3)]
			doc := []byte(fmt.Sprintf(`{"step":%d,"status":%q,"value":%d,"padding":%q}`,
				step, status, rng.Int63(), strings.Repeat("x", rng.Intn(900))))
			heapCreated, heapErr := heapStore.Put(key, doc)
			fileCreated, fileErr := fileStore.Put(key, doc)
			if heapErr != nil || fileErr != nil || heapCreated != fileCreated {
				t.Fatalf("step %d Put = heap(%v,%v) file(%v,%v)", step, heapCreated, heapErr, fileCreated, fileErr)
			}
		case 2:
			heapDeleted := heapStore.Delete(key)
			fileDeleted, fileErr := fileStore.Delete(key)
			if fileErr != nil || heapDeleted != fileDeleted {
				t.Fatalf("step %d Delete = heap %v file(%v,%v)", step, heapDeleted, fileDeleted, fileErr)
			}
		case 3:
			deadline := base.Add(time.Duration(1+rng.Intn(90)) * time.Minute)
			heapOK := heapStore.SetDeadline(key, deadline)
			fileOK, fileErr := fileStore.SetDeadline(key, deadline)
			if fileErr != nil || heapOK != fileOK {
				t.Fatalf("step %d SetDeadline = heap %v file(%v,%v)", step, heapOK, fileOK, fileErr)
			}
		case 4:
			heapOK := heapStore.Persist(key)
			fileOK, fileErr := fileStore.Persist(key)
			if fileErr != nil || heapOK != fileOK {
				t.Fatalf("step %d Persist = heap %v file(%v,%v)", step, heapOK, fileOK, fileErr)
			}
		case 5:
			now := base.Add(time.Duration(rng.Intn(60)) * time.Minute)
			limit := rng.Intn(5)
			heapCount := heapStore.ExpireDue(now, limit)
			fileCount, fileErr := fileStore.ExpireDue(now, limit)
			if fileErr != nil || heapCount != fileCount {
				t.Fatalf("step %d ExpireDue = heap %d file(%d,%v)", step, heapCount, fileCount, fileErr)
			}
		}

		if step%13 == 0 {
			assertFileStoreMatchesHeap(t, fileStore, heapStore, base, 32)
		}
		if step == 79 {
			heapSnapshot := heapStore.Snapshot()
			fileSnapshot, snapshotErr := fileStore.Snapshot()
			if snapshotErr != nil {
				t.Fatal(snapshotErr)
			}
			for i := range 16 {
				key := fmt.Sprintf("key-%02d", i)
				doc := []byte(fmt.Sprintf(`{"snapshot-churn":%d,"status":"new"}`, i))
				_, _ = heapStore.Put(key, doc)
				if _, err := fileStore.Put(key, doc); err != nil {
					t.Fatal(err)
				}
			}
			assertFileSnapshotMatchesHeap(t, fileSnapshot, heapSnapshot, 32)
			if err := fileSnapshot.Close(); err != nil {
				t.Fatal(err)
			}
			if err := fileStore.Close(); err != nil {
				t.Fatal(err)
			}
			fileStore, err = OpenFileStore(file, options)
			if err != nil {
				t.Fatal(err)
			}
			assertFileStoreMatchesHeap(t, fileStore, heapStore, base, 32)
		}
	}
	assertFileStoreMatchesHeap(t, fileStore, heapStore, base, 32)
	if err := fileStore.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertFileStoreMatchesHeap(t *testing.T, fileStore *FileStore, heapStore *Store, now time.Time, keys int) {
	t.Helper()
	fileSnapshot, err := fileStore.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer fileSnapshot.Close()
	heapSnapshot := heapStore.Snapshot()
	assertFileSnapshotMatchesHeap(t, fileSnapshot, heapSnapshot, keys)
	if fileSnapshot.Len() != uint64(heapSnapshot.Len()) {
		t.Fatalf("snapshot lengths = file %d heap %d", fileSnapshot.Len(), heapSnapshot.Len())
	}
	for i := range keys {
		key := fmt.Sprintf("key-%02d", i)
		heapTTL, heapOK := heapStore.TTLAt(key, now)
		fileTTL, fileOK, fileErr := fileStore.TTLAt(key, now)
		if fileErr != nil || heapOK != fileOK || heapTTL != fileTTL {
			t.Fatalf("TTLAt(%s) = heap(%s,%v) file(%s,%v,%v)", key, heapTTL, heapOK, fileTTL, fileOK, fileErr)
		}
	}
}

func assertFileSnapshotMatchesHeap(t *testing.T, fileSnapshot *FileSnapshot, heapSnapshot Snapshot, keys int) {
	t.Helper()
	for i := range keys {
		key := fmt.Sprintf("key-%02d", i)
		heapRaw, heapOK := heapSnapshot.GetRaw(key)
		fileRaw, fileOK, err := fileSnapshot.AppendRaw(nil, key)
		if err != nil || heapOK != fileOK || (heapOK && string(heapRaw.Bytes()) != string(fileRaw)) {
			t.Fatalf("GetRaw(%s) = heap(%q,%v) file(%q,%v,%v)", key, heapRaw.Bytes(), heapOK, fileRaw, fileOK, err)
		}
	}
}

func TestFileStoreCrashImagesRecoverWholeGeneration(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-crash-source-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.ResidentBytes = 8 << 20
	options.BufferCount = 128
	options.MaxRetiredExtents = 1024
	options.Indexes = []StoreIndexDefinition{{Name: "status", Paths: []string{"/status"}}}
	store, err := CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 12 {
		doc := []byte(fmt.Sprintf(`{"id":%d,"status":"old","padding":%q}`, i, strings.Repeat("a", i*80)))
		if _, err := store.Put(fmt.Sprintf("key-%02d", i), doc); err != nil {
			t.Fatal(err)
		}
	}
	deadline := time.Now().Add(24 * time.Hour).Truncate(time.Second)
	if ok, err := store.SetDeadline("key-03", deadline); err != nil || !ok {
		t.Fatalf("SetDeadline = (%v,%v)", ok, err)
	}
	oldGeneration := store.Generation()
	oldValue, ok, err := store.AppendRaw(nil, "key-03")
	if err != nil || !ok {
		t.Fatal(err)
	}
	before, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	newValue := []byte(fmt.Sprintf(`{"id":3,"status":"new","padding":%q}`, strings.Repeat("z", 7000)))
	if created, err := store.Put("key-03", newValue); err != nil || created {
		t.Fatalf("update = (%v,%v)", created, err)
	}
	newGeneration := store.Generation()
	after, err := os.ReadFile(file.Name())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	pageSize := options.PageSize
	dataStart := 2 * pageSize
	dataCuts := changedPageCrashCuts(before, after, dataStart, pageSize)
	for _, cut := range dataCuts {
		image := make([]byte, max(len(before), len(after)))
		copy(image, before)
		copy(image[dataStart:dataStart+cut], after[dataStart:dataStart+cut])
		assertCrashImage(t, image, options, oldGeneration, newGeneration, string(oldValue), string(newValue), deadline,
			fmt.Sprintf("data-cut-%d", cut))
	}

	rootOffset := int((newGeneration-1)&1) * pageSize
	for cut := 0; cut <= storeio.SuperblockSize; cut++ {
		image := append([]byte(nil), after...)
		copy(image[rootOffset:rootOffset+pageSize], before[rootOffset:rootOffset+pageSize])
		copy(image[rootOffset:rootOffset+cut], after[rootOffset:rootOffset+cut])
		assertCrashImage(t, image, options, oldGeneration, newGeneration, string(oldValue), string(newValue), deadline,
			fmt.Sprintf("root-cut-%d", cut))
	}
}

// changedPageCrashCuts models the sorted positional-write phase used by both
// commit devices. Each returned prefix stops immediately before, inside, or
// after one changed physical page. Unchanged holes do not multiply the test
// matrix, and the final cut represents the completed data barrier.
func changedPageCrashCuts(before, after []byte, start, quantum int) []int {
	length := max(len(before), len(after)) - start
	cuts := make([]int, 0, 16)
	appendCut := func(cut int) {
		if cut < 0 || cut > length {
			return
		}
		if len(cuts) == 0 || cuts[len(cuts)-1] != cut {
			cuts = append(cuts, cut)
		}
	}
	appendCut(0)
	for blockStart := 0; blockStart < length; blockStart += quantum {
		blockEnd := min(blockStart+quantum, length)
		changed := false
		for offset := blockStart; offset < blockEnd; offset++ {
			absolute := start + offset
			var oldByte, newByte byte
			if absolute < len(before) {
				oldByte = before[absolute]
			}
			if absolute < len(after) {
				newByte = after[absolute]
			}
			if oldByte != newByte {
				changed = true
				break
			}
		}
		if changed {
			appendCut(blockStart)
			appendCut(blockStart + 1)
			appendCut(blockEnd - 1)
			appendCut(blockEnd)
		}
	}
	appendCut(length)
	return cuts
}

func assertCrashImage(t *testing.T, image []byte, options FileStoreOptions, oldGeneration, newGeneration uint64, oldValue, newValue string, deadline time.Time, name string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		_ = file.Close()
		t.Fatalf("%s recovery: %v", name, err)
	}
	got, ok, getErr := store.AppendRaw(nil, "key-03")
	if getErr != nil || !ok {
		t.Fatalf("%s GetRaw = (%q,%v,%v)", name, got, ok, getErr)
	}
	switch store.Generation() {
	case oldGeneration:
		if string(got) != oldValue {
			t.Fatalf("%s recovered old generation with mixed value %q", name, got)
		}
		assertRecoveredIndexCounts(t, store, name, 12, 0)
	case newGeneration:
		if string(got) != newValue {
			t.Fatalf("%s recovered new generation with mixed value %q", name, got)
		}
		assertRecoveredIndexCounts(t, store, name, 11, 1)
	default:
		t.Fatalf("%s recovered generation %d, want %d or %d", name, store.Generation(), oldGeneration, newGeneration)
	}
	gotDeadline, deadlineOK, deadlineErr := store.Deadline("key-03")
	if deadlineErr != nil || !deadlineOK || !gotDeadline.Equal(deadline) {
		t.Fatalf("%s deadline = (%s,%v,%v), want %s", name, gotDeadline, deadlineOK, deadlineErr, deadline)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertRecoveredIndexCounts(t *testing.T, store *FileStore, name string, oldCount, newCount int) {
	t.Helper()
	needle := func(src []byte) Index {
		needed, err := RequiredIndexEntries(src)
		if err != nil {
			t.Fatal(err)
		}
		index, err := BuildIndex(src, make([]IndexEntry, needed))
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	count := func(value string) int {
		masks, err := store.AppendIndexMasks(nil, "status", needle([]byte(value)))
		if err != nil {
			t.Fatalf("%s index probe %s: %v", name, value, err)
		}
		rows := 0
		for _, mask := range masks {
			rows += bits.OnesCount64(mask.Bits)
		}
		return rows
	}
	if got := count(`"old"`); got != oldCount {
		t.Fatalf("%s old index count = %d, want %d", name, got, oldCount)
	}
	if got := count(`"new"`); got != newCount {
		t.Fatalf("%s new index count = %d, want %d", name, got, newCount)
	}
}

func TestFileSnapshotInlineReadSteadyAllocations(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-alloc-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	store, err := CreateFileStore(file, testFileStoreOptions())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	value := []byte(`{"id":1,"status":"active"}`)
	if _, err := store.Put("key", value); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	dst := make([]byte, 0, len(value))
	if _, ok, err := snapshot.AppendRaw(dst[:0], "key"); err != nil || !ok {
		t.Fatalf("warm read = (%v,%v)", ok, err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		got, ok, err := snapshot.AppendRaw(dst[:0], "key")
		if err != nil || !ok || len(got) != len(value) {
			panic("file snapshot read failed")
		}
	})
	if allocs != 0 {
		t.Fatalf("inline cache-hit AppendRaw allocated %.2f times, want 0", allocs)
	}
}

func TestFileStoreAsyncPublicationFlushesDurably(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-async-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.Synchronous = false
	options.QueueSlots = 8
	options.GroupLimit = 8
	store, err := CreateFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 40 {
		if _, err := store.Put(fmt.Sprintf("key-%02d", i), []byte(fmt.Sprintf(`{"id":%d}`, i))); err != nil {
			t.Fatal(err)
		}
	}
	if store.Generation() != 41 || store.DurableGeneration() > store.Generation() {
		t.Fatalf("pre-flush generations = published %d durable %d", store.Generation(), store.DurableGeneration())
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	if store.DurableGeneration() != store.Generation() {
		t.Fatalf("post-flush generations = published %d durable %d", store.Generation(), store.DurableGeneration())
	}
	stats := store.Stats()
	if stats.PublishedGeneration != 41 || stats.DurableGeneration != 41 || stats.CommittedBatches != 41 || stats.DeviceCommits == 0 {
		t.Fatalf("async stats = %+v", stats)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	for i := range 40 {
		got, ok, err := reopened.AppendRaw(nil, fmt.Sprintf("key-%02d", i))
		if err != nil || !ok || string(got) != fmt.Sprintf(`{"id":%d}`, i) {
			t.Fatalf("reopened key %d = (%q,%v,%v)", i, got, ok, err)
		}
	}
}
