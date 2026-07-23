//go:build linux

package simdjson

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/thesyncim/simdjson/internal/storeio"
)

func TestFileStoreRequiredDirectReads(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-required-direct-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.ReadMode = FileStoreReadDirectRequire
	store, err := CreateFileStore(file, options)
	if errors.Is(err, ErrStoreDirectIOUnsupported) {
		t.Skipf("test filesystem has no O_DIRECT support: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if !store.Stats().DirectReads {
		t.Fatal("required direct reads were not reported active")
	}
	for row := range 64 {
		key := fmt.Sprintf("linux:direct:%02d", row)
		value := fmt.Appendf(nil, `{"v":%d}`, row)
		if _, err := store.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok, err := reopened.AppendRaw(nil, "linux:direct:01"); err != nil || !ok || string(got) != `{"v":1}` {
		t.Fatalf("required direct read = (%q,%v,%v)", got, ok, err)
	}
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	if _, err := snapshot.RangeRawReadAheadBuffer(nil, func(_, _ []byte) error {
		rows++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if rows != 64 {
		t.Fatalf("required direct read-ahead rows = %d, want 64", rows)
	}
	if stats := reopened.Stats(); !stats.DirectReads || stats.PageReads == 0 ||
		stats.PrefetchQueued == 0 || stats.PrefetchHits+stats.CoalescedReads == 0 {
		t.Fatalf("required direct stats = %+v", stats)
	}
}

func TestFileStoreRequiredDirectWrites(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-required-direct-write-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.WriteMode = FileStoreWriteDirectRequire
	store, err := CreateFileStore(file, options)
	if errors.Is(err, ErrStoreDirectIOUnsupported) {
		t.Skipf("test filesystem has no O_DIRECT support: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); !stats.DirectWrites || stats.DirectReads {
		t.Fatalf("required direct-write stats = %+v", stats)
	}
	for row := range 64 {
		key := fmt.Sprintf("linux:direct-write:%02d", row)
		value := fmt.Appendf(nil, `{"v":%d}`, row)
		if _, err := store.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if stats := reopened.Stats(); !stats.DirectWrites || stats.DirectReads {
		t.Fatalf("reopened direct-write stats = %+v", stats)
	}
	if got, ok, err := reopened.AppendRaw(nil, "linux:direct-write:63"); err != nil || !ok || string(got) != `{"v":63}` {
		t.Fatalf("required direct write = (%q,%v,%v)", got, ok, err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
	var magic [8]byte
	if _, err := file.ReadAt(magic[:], 0); err != nil {
		t.Fatalf("caller descriptor after direct writer Close: %v", err)
	}
}

func TestFileStoreRequiredDirectReadWrite(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-required-direct-rw-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.ReadMode = FileStoreReadDirectRequire
	options.WriteMode = FileStoreWriteDirectRequire
	store, err := CreateFileStore(file, options)
	if errors.Is(err, ErrStoreDirectIOUnsupported) {
		t.Skipf("test filesystem has no O_DIRECT support: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	for row := range 64 {
		key := fmt.Sprintf("linux:direct-rw:%02d", row)
		value := fmt.Appendf(nil, `{"v":%d}`, row)
		if _, err := store.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if got, ok, err := reopened.AppendRaw(nil, "linux:direct-rw:31"); err != nil || !ok || string(got) != `{"v":31}` {
		t.Fatalf("required direct read/write = (%q,%v,%v)", got, ok, err)
	}
	if stats := reopened.Stats(); !stats.DirectReads || !stats.DirectWrites || stats.PageReads == 0 {
		t.Fatalf("required direct read/write stats = %+v", stats)
	}
}

func TestFileStoreDirectReadWriteUnderCachePressure(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-direct-pressure-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.Synchronous = false
	options.ReadMode = FileStoreReadDirectRequire
	options.WriteMode = FileStoreWriteDirectRequire
	options.MaxSnapshotLeases = 64
	options.MaxRetiredExtents = 1 << 14
	normalized, err := options.normalized()
	if err != nil {
		t.Fatal(err)
	}
	options.ResidentBytes = int64(2 * normalized.maxTransactionBytes)
	store, err := CreateFileStore(file, options)
	if errors.Is(err, ErrStoreDirectIOUnsupported) {
		t.Skipf("test filesystem has no O_DIRECT support: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const records = 2048
	for row := range records {
		key := fmt.Sprintf("pressure:%04d", row)
		value := fmt.Appendf(nil, `{"id":%d,"version":0,"payload":"%064d"}`, row, row)
		if _, err := store.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	initial := store.Stats()
	if initial.FileEnd <= 10*initial.CapacityBytes || !initial.DirectReads || !initial.DirectWrites {
		t.Fatalf("direct pressure setup = %+v", initial)
	}

	stop := make(chan struct{})
	failures := make(chan error, 8)
	var readers sync.WaitGroup
	for worker := range 4 {
		readers.Add(1)
		go func(worker int) {
			defer readers.Done()
			row := uint32(worker)
			dst := make([]byte, 0, 128)
			for {
				select {
				case <-stop:
					return
				default:
				}
				row = row*1664525 + 1013904223
				key := fmt.Sprintf("pressure:%04d", row%records)
				snapshot, err := store.Snapshot()
				if err != nil {
					failures <- err
					return
				}
				value, ok, readErr := snapshot.AppendRaw(dst[:0], key)
				closeErr := snapshot.Close()
				if readErr != nil {
					failures <- readErr
					return
				}
				if closeErr != nil {
					failures <- closeErr
					return
				}
				// The writer briefly deletes one key before reinserting it.
				if ok && len(value) == 0 {
					failures <- fmt.Errorf("empty value for %s", key)
					return
				}
			}
		}(worker)
	}

	for version := 1; version <= 256; version++ {
		row := version & 63
		key := fmt.Sprintf("pressure:%04d", row)
		if version%32 == 0 {
			if deleted, err := store.Delete(key); err != nil || !deleted {
				close(stop)
				readers.Wait()
				t.Fatalf("Delete(%s) = (%v,%v)", key, deleted, err)
			}
		}
		value := fmt.Appendf(nil, `{"id":%d,"version":%d,"payload":"%064d"}`, row, version, version)
		if _, err := store.Put(key, value); err != nil {
			close(stop)
			readers.Wait()
			t.Fatal(err)
		}
		select {
		case err := <-failures:
			close(stop)
			readers.Wait()
			t.Fatal(err)
		default:
		}
	}
	close(stop)
	readers.Wait()
	select {
	case err := <-failures:
		t.Fatal(err)
	default:
	}
	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}
	stats := store.Stats()
	if stats.ResidentBytes > stats.CapacityBytes || stats.DirtyBytes != 0 ||
		stats.PinnedPages != 0 || stats.Evictions == 0 || stats.PageReads == 0 ||
		!stats.DirectReads || !stats.DirectWrites {
		t.Fatalf("direct pressure stats = %+v", stats)
	}
}

func TestFileStoreDirectIOUring(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "file-store-direct-ring-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := testFileStoreOptions()
	options.Backend = FileStoreBackendIOUring
	options.ReadMode = FileStoreReadDirectRequire
	options.WriteMode = FileStoreWriteDirectRequire
	store, err := CreateFileStore(file, options)
	if errors.Is(err, ErrStoreDirectIOUnsupported) ||
		errors.Is(err, storeio.ErrUnavailable) ||
		errors.Is(err, storeio.ErrUnsupported) {
		t.Skipf("test host cannot provide direct io_uring I/O: %v", err)
	}
	if err != nil {
		t.Fatal(err)
	}
	if stats := store.Stats(); stats.Backend != FileStoreBackendIOUring ||
		stats.ReadBackend != FileStoreBackendIOUring ||
		!stats.DirectReads || !stats.DirectWrites {
		t.Fatalf("direct io_uring stats = %+v", stats)
	}
	for row := range 64 {
		key := fmt.Sprintf("ring:%02d", row)
		value := fmt.Appendf(nil, `{"v":%d}`, row)
		if _, err := store.Put(key, value); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	keys := make([]string, 16)
	for row := range keys {
		keys[row] = fmt.Sprintf("ring:%02d", row)
	}
	if queued, err := reopened.PrefetchKeys(keys); err != nil || queued == 0 {
		t.Fatalf("direct io_uring prefetch = (%d,%v)", queued, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for reopened.Stats().AsyncReadBatches == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if got, ok, err := reopened.AppendRaw(nil, "ring:63"); err != nil || !ok || string(got) != `{"v":63}` {
		t.Fatalf("direct io_uring read = (%q,%v,%v)", got, ok, err)
	}
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	rows := 0
	if _, err := snapshot.RangeRawReadAheadBuffer(nil, func(_, _ []byte) error {
		rows++
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	stats := reopened.Stats()
	if rows != 64 || stats.AsyncReadBatches == 0 || stats.LargestReadBatch < 2 ||
		stats.ReadBackend != FileStoreBackendIOUring {
		t.Fatalf("direct io_uring batch read = rows %d stats %+v", rows, stats)
	}
}
