package durable

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

func TestFilePrimaryPutDeleteSnapshotCOW(t *testing.T) {
	for _, durability := range []DurabilityMode{
		DurabilitySync,
		DurabilityBufferedVisible,
	} {
		t.Run(fmt.Sprint(durability), func(t *testing.T) {
			built, keys, values := buildFilePrimaryCorpus(t, 1_000)
			options := Options{
				Backend: BackendPortable, ResidentBytes: 32 << 20,
				Durability: durability,
			}
			file := createPrimaryPointFile(
				t, built, options, "primary-mutate.vibe",
			)
			collection, err := Open(file, options)
			if err != nil {
				t.Fatal(err)
			}
			defer collection.Close()
			snapshot, err := collection.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			defer snapshot.Close()

			updated := []byte(`{"id":500,"updated":true}`)
			if created, err := collection.Put(
				keys[500], updated,
			); err != nil || created {
				t.Fatalf("update = %v,%v", created, err)
			}
			insertedKey := "primary-key-000000500-extra"
			inserted := []byte(`{"inserted":true}`)
			if created, err := collection.Put(
				insertedKey, inserted,
			); err != nil || !created {
				t.Fatalf("insert = %v,%v", created, err)
			}
			if deleted, err := collection.Delete(
				keys[250],
			); err != nil || !deleted {
				t.Fatalf("delete = %v,%v", deleted, err)
			}
			if collection.Len() != 1_000 {
				t.Fatalf("Len = %d, want 1000", collection.Len())
			}
			assertPrimaryRaw(t, collection, keys[500], updated, true)
			assertPrimaryRaw(t, collection, insertedKey, inserted, true)
			assertPrimaryRaw(t, collection, keys[250], nil, false)

			buffer := make([]byte, 0, 128)
			got, ok, err := snapshot.AppendRaw(buffer[:0], keys[500])
			if err != nil || !ok || !bytes.Equal(got, values[500]) {
				t.Fatalf("snapshot update = %q,%v,%v", got, ok, err)
			}
			got, ok, err = snapshot.AppendRaw(buffer[:0], insertedKey)
			if err != nil || ok || len(got) != 0 {
				t.Fatalf("snapshot insert = %q,%v,%v", got, ok, err)
			}
			got, ok, err = snapshot.AppendRaw(buffer[:0], keys[250])
			if err != nil || !ok || !bytes.Equal(got, values[250]) {
				t.Fatalf("snapshot delete = %q,%v,%v", got, ok, err)
			}
			if err := snapshot.Close(); err != nil {
				t.Fatal(err)
			}
			if err := collection.Flush(); err != nil {
				t.Fatal(err)
			}
			if err := collection.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(file, options)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			assertPrimaryRaw(t, reopened, keys[500], updated, true)
			assertPrimaryRaw(t, reopened, insertedKey, inserted, true)
			assertPrimaryRaw(t, reopened, keys[250], nil, false)
		})
	}
}

func TestFilePrimaryBufferedCanonicalFrame(t *testing.T) {
	built, keys, _ := buildFilePrimaryCorpus(t, 1_000)
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
		Durability: DurabilityBufferedVisible,
	}
	file := createPrimaryPointFile(
		t, built, options, "primary-buffered.vibe",
	)
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	first := []byte(`{"id":500,"hot":"first"}`)
	second := []byte(`{"id":500,"hot":"other"}`)
	third := []byte(`{"id":500,"hot":"third"}`)
	fourth := []byte(`{"id":500,"hot":"final"}`)
	if len(first) != len(second) || len(second) != len(third) ||
		len(third) != len(fourth) {
		t.Fatal("same-size fixture drift")
	}
	if created, err := collection.Put(keys[500], first); err != nil || created {
		t.Fatalf("first update = %v,%v", created, err)
	}
	afterFirst := collection.Stats()
	if afterFirst.BufferedInplaceUpdates != 0 {
		t.Fatalf(
			"first-touch in-place updates = %d, want 0",
			afterFirst.BufferedInplaceUpdates,
		)
	}
	if created, err := collection.Put(keys[500], second); err != nil || created {
		t.Fatalf("second update = %v,%v", created, err)
	}
	afterSecond := collection.Stats()
	if afterSecond.BufferedInplaceUpdates != 0 {
		t.Fatalf(
			"same-size first-touch in-place updates = %d, want 0",
			afterSecond.BufferedInplaceUpdates,
		)
	}
	if created, err := collection.Put(keys[500], third); err != nil || created {
		t.Fatalf("third update = %v,%v", created, err)
	}
	afterThird := collection.Stats()
	if afterThird.BufferedInplaceUpdates != 1 {
		t.Fatalf(
			"same-size second-touch in-place updates = %d, want 1",
			afterThird.BufferedInplaceUpdates,
		)
	}
	assertPrimaryRaw(t, collection, keys[500], third, true)
	state := collection.state.Load()
	buffer := make([]byte, 0, 128)
	got, ok, err := collection.resolvePrimaryGraphPageWalk(
		buffer[:0], state, keys[500],
	)
	if err != nil || !ok || !bytes.Equal(got, third) {
		t.Fatalf("page-walk = %q,%v,%v", got, ok, err)
	}
	snapshot, err := collection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	if created, err := collection.Put(keys[500], fourth); err != nil || created {
		t.Fatalf("snapshot-forced COW = %v,%v", created, err)
	}
	assertPrimaryRaw(t, collection, keys[500], fourth, true)
	got, ok, err = snapshot.AppendRaw(buffer[:0], keys[500])
	if err != nil || !ok || !bytes.Equal(got, third) {
		t.Fatalf("snapshot after forced COW = %q,%v,%v", got, ok, err)
	}
	if collection.Stats().BufferedInplaceFallbacks == 0 {
		t.Fatal("snapshot-forced COW did not count an in-place fallback")
	}
}

func TestFilePrimaryBufferedCrashBoundary(t *testing.T) {
	built, keys, values := buildFilePrimaryCorpus(t, 1_000)
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
		Durability: DurabilityBufferedVisible,
	}
	file := createPrimaryPointFile(
		t, built, options, "primary-crash-source.vibe",
	)
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	updated := []byte(`{"id":500,"crash":"buffered"}`)
	if created, err := collection.Put(
		keys[500], updated,
	); err != nil || created {
		t.Fatalf("buffered update = %v,%v", created, err)
	}
	assertPrimaryRaw(t, collection, keys[500], updated, true)

	before := clonePrimaryCrashFile(t, file, "before-checkpoint.vibe")
	beforeCollection, err := Open(before, options)
	if err != nil {
		t.Fatal(err)
	}
	assertPrimaryRaw(t, beforeCollection, keys[500], values[500], true)
	if err := beforeCollection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := before.Close(); err != nil {
		t.Fatal(err)
	}

	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}
	after := clonePrimaryCrashFile(t, file, "after-checkpoint.vibe")
	afterCollection, err := Open(after, options)
	if err != nil {
		t.Fatal(err)
	}
	assertPrimaryRaw(t, afterCollection, keys[500], updated, true)
	if err := afterCollection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := after.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestFilePrimaryLeafSplitSignal(t *testing.T) {
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append("seed", []byte(`{"v":0}`)); err != nil {
		t.Fatal(err)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
		Durability: DurabilityBufferedVisible,
	}
	file := createPrimaryPointFile(t, built, options, "primary-split.vibe")
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	var splitErr error
	for at := 0; at < 512; at++ {
		key := fmt.Sprintf("key-%04d", at)
		_, splitErr = collection.Put(key, []byte(`{"v":1}`))
		if splitErr != nil {
			generationAtSplit := collection.Generation()
			if _, retryErr := collection.Put(
				key, []byte(`{"v":1}`),
			); !errors.Is(retryErr, ErrPrimaryLeafSplitRequired) {
				t.Fatalf("split retry = %v", retryErr)
			}
			if collection.Generation() != generationAtSplit {
				t.Fatal("split signal published a generation")
			}
			break
		}
	}
	if !errors.Is(splitErr, ErrPrimaryLeafSplitRequired) {
		t.Fatalf(
			"full leaf error = %v, want %v",
			splitErr, ErrPrimaryLeafSplitRequired,
		)
	}
	if collection.Stats().PrimaryLeafSplitRequired != 2 {
		t.Fatalf(
			"split counter = %d, want 2",
			collection.Stats().PrimaryLeafSplitRequired,
		)
	}
}

func TestFilePrimaryEmptyLeafAccounting(t *testing.T) {
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	value := []byte(`{"v":0}`)
	if err := builder.Append("seed", value); err != nil {
		t.Fatal(err)
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
		Durability: DurabilityBufferedVisible,
	}
	file := createPrimaryPointFile(t, built, options, "primary-empty.vibe")
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	if deleted, err := collection.Delete("seed"); err != nil || !deleted {
		t.Fatalf("delete = %v,%v", deleted, err)
	}
	if collection.Stats().PrimaryEmptyLeaves != 1 {
		t.Fatalf(
			"empty leaves = %d, want 1",
			collection.Stats().PrimaryEmptyLeaves,
		)
	}
	if created, err := collection.Put("seed", value); err != nil || !created {
		t.Fatalf("refill = %v,%v", created, err)
	}
	if collection.Stats().PrimaryEmptyLeaves != 0 {
		t.Fatalf(
			"empty leaves after refill = %d, want 0",
			collection.Stats().PrimaryEmptyLeaves,
		)
	}
}

func TestFilePrimaryMutationDifferentialSmoke(t *testing.T) {
	runPrimaryMutationDifferential(
		t, DurabilityBufferedVisible, 1_000, 512,
	)
	runPrimaryMutationDifferential(
		t, DurabilitySync, 1_000, 64,
	)
}

func TestFilePrimaryConcurrentRouterPublication(t *testing.T) {
	built, keys, values := buildFilePrimaryCorpus(t, 1_000)
	options := Options{
		Backend: BackendPortable, ResidentBytes: 32 << 20,
		Durability: DurabilityBufferedVisible,
	}
	file := createPrimaryPointFile(
		t, built, options, "primary-concurrent.vibe",
	)
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	snapshot, err := collection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	key := keys[500]
	first := []byte(`{"v":"first"}`)
	second := []byte(`{"v":"a-longer-second"}`)
	var (
		done      atomic.Bool
		writerWG  sync.WaitGroup
		writerErr error
	)
	writerWG.Add(1)
	go func() {
		defer writerWG.Done()
		defer done.Store(true)
		for at := 0; at < 200; at++ {
			value := first
			if at&1 != 0 {
				value = second
			}
			if _, putErr := collection.Put(key, value); putErr != nil {
				writerErr = putErr
				return
			}
		}
	}()
	currentBuffer := make([]byte, 0, 128)
	snapshotBuffer := make([]byte, 0, 128)
	for !done.Load() {
		current, ok, readErr := collection.AppendRaw(
			currentBuffer[:0], key,
		)
		if readErr != nil || !ok ||
			!bytes.Equal(current, values[500]) &&
				!bytes.Equal(current, first) &&
				!bytes.Equal(current, second) {
			t.Fatalf("current read = %q,%v,%v", current, ok, readErr)
		}
		pinned, pinnedOK, pinnedErr := snapshot.AppendRaw(
			snapshotBuffer[:0], key,
		)
		if pinnedErr != nil || !pinnedOK ||
			!bytes.Equal(pinned, values[500]) {
			t.Fatalf(
				"snapshot read = %q,%v,%v",
				pinned, pinnedOK, pinnedErr,
			)
		}
		currentBuffer = current
		snapshotBuffer = pinned
	}
	writerWG.Wait()
	if writerErr != nil {
		t.Fatal(writerErr)
	}
}

func TestFilePrimaryMutationDifferential10KAnd100K(t *testing.T) {
	if os.Getenv("VIBEJSON_PRIMARY_LONG") == "" {
		t.Skip("set VIBEJSON_PRIMARY_LONG=1 for 10k/100k mutation traces")
	}
	for _, operations := range []int{10_000, 100_000} {
		for _, durability := range []DurabilityMode{
			DurabilityBufferedVisible,
			DurabilitySync,
		} {
			t.Run(
				fmt.Sprintf("%d/%d", operations, durability),
				func(t *testing.T) {
					runPrimaryMutationDifferential(
						t, durability, 10_000, operations,
					)
				},
			)
		}
	}
}

func runPrimaryMutationDifferential(
	t testing.TB,
	durability DurabilityMode,
	count, operations int,
) {
	t.Helper()
	builder, err := store.NewBuilder(store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	keys := make([]string, count)
	oracle := make(map[string][]byte, count)
	for at := range count {
		key := fmt.Sprintf("trace-key-%06d", at)
		value := []byte(fmt.Sprintf(`{"v":"%06d","p":"a"}`, at))
		keys[at] = key
		oracle[key] = append([]byte(nil), value...)
		if err := builder.Append(key, value); err != nil {
			t.Fatal(err)
		}
	}
	built, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Backend: BackendPortable, ResidentBytes: 64 << 20,
		Durability: durability,
	}
	file := createPrimaryPointFile(t, built, options, "primary-trace.vibe")
	collection, err := Open(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()

	random := uint64(0x9e3779b97f4a7c15)
	buffer := make([]byte, 0, 64)
	oracleBuffer := make([]byte, 0, 64)
	for operation := range operations {
		random ^= random << 7
		random ^= random >> 9
		random ^= random << 8
		key := keys[int(random%uint64(len(keys)))]
		if random>>63 == 0 {
			deleted, deleteErr := collection.Delete(key)
			_, existed := oracle[key]
			if deleteErr != nil || deleted != existed {
				t.Fatalf(
					"delete %d %q = %v,%v, want %v,nil",
					operation, key, deleted, deleteErr, existed,
				)
			}
			delete(oracle, key)
		} else {
			value := []byte(fmt.Sprintf(
				`{"v":"%06d","p":"b"}`, operation%1_000_000,
			))
			_, existed := oracle[key]
			created, putErr := collection.Put(key, value)
			if putErr != nil || created == existed {
				t.Fatalf(
					"put %d %q = %v,%v, existed=%v",
					operation, key, created, putErr, existed,
				)
			}
			oracle[key] = append(oracle[key][:0], value...)
		}
		want, wantOK := oracle[key]
		got, ok, readErr := collection.AppendRaw(buffer[:0], key)
		if readErr != nil || ok != wantOK || !bytes.Equal(got, want) {
			t.Fatalf(
				"read %d %q = %q,%v,%v; want %q,%v,nil",
				operation, key, got, ok, readErr, want, wantOK,
			)
		}
		state := collection.state.Load()
		pageWalk, pageWalkOK, pageWalkErr :=
			collection.resolvePrimaryGraphPageWalk(
				oracleBuffer[:0], state, key,
			)
		if pageWalkErr != nil || pageWalkOK != wantOK ||
			!bytes.Equal(pageWalk, want) {
			t.Fatalf(
				"page-walk %d %q = %q,%v,%v; want %q,%v,nil",
				operation, key, pageWalk, pageWalkOK,
				pageWalkErr, want, wantOK,
			)
		}
		buffer = got
		oracleBuffer = pageWalk
	}
	if collection.Len() != uint64(len(oracle)) {
		t.Fatalf(
			"Len = %d, want %d",
			collection.Len(), len(oracle),
		)
	}
	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}
}

func assertPrimaryRaw(
	t testing.TB,
	collection *Collection,
	key string,
	want []byte,
	wantOK bool,
) {
	t.Helper()
	got, ok, err := collection.AppendRaw(
		make([]byte, 0, max(len(want), 1)), key,
	)
	if err != nil || ok != wantOK || !bytes.Equal(got, want) {
		t.Fatalf(
			"AppendRaw(%q) = %q,%v,%v; want %q,%v,nil",
			key, got, ok, err, want, wantOK,
		)
	}
}

func clonePrimaryCrashFile(
	t testing.TB,
	source *os.File,
	name string,
) *os.File {
	t.Helper()
	image, err := os.ReadFile(source.Name())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, image, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(path, os.O_RDWR, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	return file
}
