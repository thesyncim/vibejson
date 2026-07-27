package storeio

import (
	"errors"
	"io"
	"os"
	"reflect"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"
)

const pageCacheTestPageSize = 4096

func TestPageCacheBoundedAdmissionEvictionAndIdentity(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 3)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 2 * pageCacheTestPageSize,
		StoreID: storeID, PrefetchQueue: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	first, err := cache.Acquire(refs[0])
	if err != nil || len(first.Payload()) != 32 || first.Payload()[0] != 1 {
		t.Fatalf("first Acquire = payload %v, %v", first.Payload(), err)
	}
	if len(first.Page()) != pageCacheTestPageSize || cap(first.Page()) != pageCacheTestPageSize {
		t.Fatalf("first Page = %d/%d", len(first.Page()), cap(first.Page()))
	}
	second, err := cache.Acquire(refs[1])
	if err != nil || second.Payload()[0] != 2 {
		t.Fatalf("second Acquire = payload %v, %v", second.Payload(), err)
	}
	if _, err := cache.Acquire(refs[2]); !errors.Is(err, ErrPageCachePinned) {
		t.Fatalf("fully pinned Acquire error = %v, want %v", err, ErrPageCachePinned)
	}

	first.Release()
	third, err := cache.Acquire(refs[2])
	if err != nil || third.Payload()[0] != 3 {
		t.Fatalf("third Acquire = payload %v, %v", third.Payload(), err)
	}
	third.Release()
	second.Release()

	stats := cache.Stats()
	if stats.CapacityBytes != 2*pageCacheTestPageSize ||
		stats.ResidentBytes != stats.CapacityBytes || stats.PinnedPages != 0 ||
		stats.PageReads != 3 || stats.ReadBytes != 3*pageCacheTestPageSize || stats.Evictions != 1 {
		t.Fatalf("unexpected stats: %+v", stats)
	}

	wrong := refs[2]
	wrong.LogicalID++
	if _, err := cache.Acquire(wrong); !errors.Is(err, ErrPageCacheReference) {
		t.Fatalf("identity mismatch error = %v, want %v", err, ErrPageCacheReference)
	}
}

func TestPageCacheReadyHitIncludesRoutingMetadata(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 1)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lease, err := cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	key, err := cache.validateRef(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	wrong := key
	mask := uint64(len(cache.table) - 1)
	for aux := uint16(1); aux != 0; aux++ {
		wrong.aux = aux
		if cacheKeyHash(wrong)&mask == cacheKeyHash(key)&mask {
			break
		}
	}
	if wrong.aux == 0 {
		t.Fatal("could not construct colliding routing key")
	}
	var wrongLease PageLease
	if cache.tryPinReady(cacheKeyHash(wrong), wrong, &wrongLease) {
		wrongLease.Release()
		t.Fatal("ready hit ignored PageRef routing metadata")
	}
}

func TestPageCacheFingerprintDirectoryIdentityIsNotLegacyKeyDirectory(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-fingerprint-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	storeID := [16]byte{17, 3, 5, 7, 9, 11, 13, 15, 2, 4, 6, 8, 10, 12, 14, 16}
	page := make([]byte, pageCacheTestPageSize)
	payload, err := InitPage(page, PageHeader{
		StoreID: storeID, Generation: 2, LogicalID: 7,
		PageSize: pageCacheTestPageSize, PayloadLength: 32,
		Kind: PageFingerprintDirectory,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 0x5a
	if _, err := SealPage(page); err != nil {
		t.Fatal(err)
	}
	ref := PageRef{
		Offset: 4 * pageCacheTestPageSize, LogicalID: 7, Generation: 2,
		Length: pageCacheTestPageSize, Kind: PageFingerprintDirectory,
	}
	if _, err := file.WriteAt(page, int64(ref.Offset)); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lease, err := cache.Acquire(ref)
	if err != nil {
		t.Fatal(err)
	}
	if lease.Header().Kind != PageFingerprintDirectory || lease.Payload()[0] != 0x5a {
		t.Fatalf("fingerprint lease = header %+v payload %x", lease.Header(), lease.Payload())
	}
	lease.Release()

	legacy := ref
	legacy.Kind = PageKeyDirectory
	if _, err := cache.Acquire(legacy); !errors.Is(err, ErrPageCacheReference) {
		t.Fatalf("legacy-kind reference to fingerprint page = %v, want %v", err, ErrPageCacheReference)
	}
}

func TestPageCachePrefetchOrderingAndHit(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 3)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 3 * pageCacheTestPageSize,
		StoreID: storeID, PrefetchQueue: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if queued, err := cache.Prefetch([]PageRef{refs[1], refs[0]}); queued != 0 ||
		!errors.Is(err, ErrPageCacheReference) {
		t.Fatalf("unordered Prefetch = %d, %v", queued, err)
	}
	if queued, err := cache.Prefetch(refs[:2]); err != nil || queued != 2 {
		t.Fatalf("Prefetch = %d, %v", queued, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for cache.Stats().PageReads < 2 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if stats := cache.Stats(); stats.PageReads != 2 {
		t.Fatalf("prefetch did not complete: %+v", stats)
	}
	lease, err := cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	stats := cache.Stats()
	if stats.CacheHits != 1 || stats.PrefetchHits != 1 || stats.PrefetchQueued != 2 {
		t.Fatalf("unexpected prefetch stats: %+v", stats)
	}
}

func TestPageCacheIOUringPrefetchBatch(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("io_uring requires Linux")
	}
	file, storeID, refs := newPageCacheFixture(t, 8)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 8 * pageCacheTestPageSize,
		StoreID: storeID, PrefetchQueue: 8, ReadConcurrency: 4,
		Backend: BackendIOUring,
	})
	if errors.Is(err, ErrUnavailable) || errors.Is(err, ErrUnsupported) ||
		errors.Is(err, syscall.EPERM) || errors.Is(err, syscall.ENOMEM) {
		t.Skip(err)
	}
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if queued, err := cache.Prefetch(refs); err != nil || queued != len(refs) {
		t.Fatalf("Prefetch = (%d,%v), want (%d,nil)", queued, err, len(refs))
	}
	deadline := time.Now().Add(2 * time.Second)
	for cache.Stats().PageReads < uint64(len(refs)) && time.Now().Before(deadline) {
		runtime.Gosched()
	}
	stats := cache.Stats()
	if stats.ReadBackend != BackendIOUring || stats.PageReads != uint64(len(refs)) ||
		stats.AsyncReadBatches == 0 || stats.LargestReadBatch < 2 || stats.ReadErrors != 0 {
		t.Fatalf("io_uring prefetch stats = %+v", stats)
	}
	for index, ref := range refs {
		lease, err := cache.Acquire(ref)
		if err != nil || lease.Payload()[0] != byte(index+1) {
			t.Fatalf("Acquire(%d) = (%v,%v)", index, lease.Payload(), err)
		}
		lease.Release()
	}
}

func TestPageCacheRejectsCorruptionAndShortRead(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 2)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	if _, err := file.WriteAt([]byte{0xff}, int64(refs[0].Offset+PageHeaderSize+3)); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Acquire(refs[0]); !errors.Is(err, ErrPageCorrupt) {
		t.Fatalf("corrupt Acquire error = %v, want %v", err, ErrPageCorrupt)
	}
	if err := file.Truncate(int64(refs[1].Offset + uint64(refs[1].Length)/2)); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Acquire(refs[1]); !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("short Acquire error = %v, want EOF", err)
	}
}

func TestPageCacheCloseRequiresReleasedLeases(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 1)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, err := cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); !errors.Is(err, ErrPageCachePinned) {
		t.Fatalf("Close with lease = %v, want %v", err, ErrPageCachePinned)
	}
	if _, err := cache.Acquire(refs[0]); !errors.Is(err, ErrPageCacheClosed) {
		t.Fatalf("Acquire while closing = %v, want %v", err, ErrPageCacheClosed)
	}
	lease.Release()
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPageCacheWarmAcquireSteadyAllocation(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 1)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	lease, err := cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	allocs := testing.AllocsPerRun(1000, func() {
		lease, acquireErr := cache.Acquire(refs[0])
		if acquireErr != nil {
			panic(acquireErr)
		}
		if lease.Payload()[0] != 1 {
			panic("wrong page")
		}
		lease.Release()
	})
	if allocs != 0 {
		t.Fatalf("warm Acquire/Release allocations = %v, want 0", allocs)
	}
}

func TestPageCacheDemandWaitsForSpeculativeAdmission(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 2)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID, Backend: BackendPortable,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	key, err := cache.validateRef(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	cache.mu.Lock()
	frameIndex, ok := cache.reserveLocked(1)
	if !ok {
		t.Fatal("reserve speculative frame")
	}
	frame := &cache.frames[frameIndex]
	frame.lock.Lock()
	cache.beginExtentLocked(frameIndex, 1, key, cacheKeyHash(key))
	frame.prefetched = true
	cache.activeLoads++
	frame.lock.Unlock()
	page := cache.extentBytes(frameIndex, refs[0].Length)
	cache.mu.Unlock()
	n, err := file.ReadAt(page, int64(refs[0].Offset))
	if err != nil {
		t.Fatal(err)
	}

	type result struct {
		lease PageLease
		err   error
	}
	done := make(chan result, 1)
	go func() {
		lease, acquireErr := cache.Acquire(refs[1])
		done <- result{lease: lease, err: acquireErr}
	}()
	select {
	case got := <-done:
		got.lease.Release()
		t.Fatalf("demand returned before speculative completion: %v", got.err)
	case <-time.After(10 * time.Millisecond):
	}
	cache.completePrefetch(pageCacheRingLoad{
		ref: refs[0], key: key, frame: frameIndex,
	}, n, nil)
	select {
	case got := <-done:
		if got.err != nil || got.lease.Payload()[0] != 2 {
			t.Fatalf("demand after speculative completion = (%v,%v)", got.lease.Payload(), got.err)
		}
		got.lease.Release()
	case <-time.After(2 * time.Second):
		t.Fatal("demand did not resume after speculative completion")
	}
}

func TestPageCacheVariableDocumentExtent(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-variable-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	storeID := [16]byte{9, 7, 5, 3, 1, 2, 4, 6, 8, 10, 12, 14, 16, 15, 13, 11}
	const extentSize = 2 * pageCacheTestPageSize
	page := make([]byte, extentSize)
	payload, err := InitPage(page, PageHeader{
		StoreID: storeID, Generation: 3, LogicalID: 7,
		PageSize: extentSize, PayloadLength: 17, Kind: PageDocument,
	})
	if err != nil {
		t.Fatal(err)
	}
	copy(payload, "variable document")
	if _, err := SealPage(page); err != nil {
		t.Fatal(err)
	}
	ref := PageRef{
		Offset: 4 * pageCacheTestPageSize, LogicalID: 7, Generation: 3,
		Length: extentSize, Kind: PageDocument,
	}
	if _, err := file.WriteAt(page, int64(ref.Offset)); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, MaxPageSize: extentSize,
		ResidentBytes: extentSize, StoreID: storeID, ReadConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	lease, err := cache.Acquire(ref)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(lease.Payload()); got != "variable document" {
		t.Fatalf("Payload = %q", got)
	}
	if got := lease.Header().PageSize; got != extentSize {
		t.Fatalf("PageSize = %d, want %d", got, extentSize)
	}
	lease.Release()

	metadata := ref
	metadata.Kind = PageKeyDirectory
	if _, err := cache.Acquire(metadata); !errors.Is(err, ErrPageCacheReference) {
		t.Fatalf("oversize metadata error = %v, want %v", err, ErrPageCacheReference)
	}
	for _, kind := range []PageKind{
		PagePrimaryCatalog,
		PageTabletDirectory,
		PagePrimaryLocator,
		PageTabletRoute,
		PagePrimaryAnchor,
		PagePrimaryLeaf,
	} {
		hybrid := ref
		hybrid.Kind = kind
		if _, err := cache.validateRef(hybrid); err != nil {
			t.Fatalf("variable hybrid kind %d rejected: %v", kind, err)
		}
	}
	stats := cache.Stats()
	if stats.CapacityBytes != extentSize || stats.ResidentBytes != extentSize ||
		stats.PageReads != 1 || stats.ReadBytes != extentSize {
		t.Fatalf("unexpected variable-extent stats: %+v", stats)
	}
}

func TestPageCacheNonPowerOfTwoDocumentExtentDemandAndEviction(t *testing.T) {
	tests := [...]struct {
		name         string
		logicalPages int
	}{
		{name: "12KiB", logicalPages: 3},
		{name: "20KiB", logicalPages: 5},
		{name: "28KiB", logicalPages: 7},
		{name: "60KiB", logicalPages: 15},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file, err := os.CreateTemp(t.TempDir(), "store-page-cache-non-power-*")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = file.Close() })
			storeID := [16]byte{11, 7, 5, 3, 1, 2, 4, 6, 8, 10, 12, 14, 16, 15, 13, 9}
			length := uint32(test.logicalPages * pageCacheTestPageSize)
			reservedPages := nextPowerOfTwo(test.logicalPages)
			if _, initErr := InitPage(make([]byte, length), PageHeader{
				StoreID: storeID, Generation: 1, LogicalID: 99,
				PageSize: length, PayloadLength: 32, Kind: PageKeyDirectory,
			}); !errors.Is(initErr, ErrInvalidWrite) {
				t.Fatalf("non-power metadata InitPage = %v, want %v", initErr, ErrInvalidWrite)
			}
			refs := make([]PageRef, 3)
			offset := uint64(4 * pageCacheTestPageSize)
			for i := range refs {
				page := make([]byte, length)
				payload, initErr := InitPage(page, PageHeader{
					StoreID: storeID, Generation: 1, LogicalID: uint64(i + 2),
					PageSize: length, PayloadLength: 32, Kind: PageDocument,
				})
				if initErr != nil {
					t.Fatal(initErr)
				}
				payload[0] = byte(i + 1)
				if _, sealErr := SealPage(page); sealErr != nil {
					t.Fatal(sealErr)
				}
				if _, writeErr := file.WriteAt(page, int64(offset)); writeErr != nil {
					t.Fatal(writeErr)
				}
				refs[i] = PageRef{
					Offset: offset, LogicalID: uint64(i + 2), Generation: 1,
					Length: length, Kind: PageDocument,
				}
				offset += uint64(length)
			}

			reservedBytes := reservedPages * pageCacheTestPageSize
			cache, err := NewPageCache(file, PageCacheOptions{
				PageSize: pageCacheTestPageSize, MaxPageSize: reservedBytes,
				ResidentBytes: int64(2 * reservedBytes), StoreID: storeID,
				ReadConcurrency: 1,
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := cache.Close(); err != nil {
					t.Errorf("Close: %v", err)
				}
			})

			first, err := cache.Acquire(refs[0])
			if err != nil || len(first.Page()) != int(length) || first.Payload()[0] != 1 {
				t.Fatalf("first Acquire = (%d,%v,%v)", len(first.Page()), first.Payload(), err)
			}
			second, err := cache.Acquire(refs[1])
			if err != nil || len(second.Page()) != int(length) || second.Payload()[0] != 2 {
				t.Fatalf("second Acquire = (%d,%v,%v)", len(second.Page()), second.Payload(), err)
			}
			if _, err := cache.Acquire(refs[2]); !errors.Is(err, ErrPageCachePinned) {
				t.Fatalf("fully pinned third Acquire = %v, want %v", err, ErrPageCachePinned)
			}
			first.Release()
			third, err := cache.Acquire(refs[2])
			if err != nil || len(third.Page()) != int(length) || third.Payload()[0] != 3 {
				t.Fatalf("third Acquire = (%d,%v,%v)", len(third.Page()), third.Payload(), err)
			}
			second.Release()
			third.Release()

			ready := 0
			cache.mu.Lock()
			for i := range cache.frames {
				frame := &cache.frames[i]
				if frame.state == pageCacheReady {
					ready++
					if 1<<frame.reservationOrder != reservedPages ||
						frame.key.length != length {
						cache.mu.Unlock()
						t.Fatalf(
							"head reservation = %d pages for %d-byte extent",
							1<<frame.reservationOrder, frame.key.length,
						)
					}
				}
			}
			cache.mu.Unlock()
			if ready != 2 {
				t.Fatalf("ready extents = %d, want 2", ready)
			}

			stats := cache.Stats()
			if stats.CapacityBytes != uint64(2*reservedBytes) ||
				stats.ResidentBytes != 2*uint64(length) ||
				stats.ReservedBytes != stats.CapacityBytes ||
				stats.ReadyFrames != 2 || stats.PinnedPages != 0 ||
				stats.PageReads != 3 || stats.ReadBytes != 3*uint64(length) ||
				stats.Evictions != 1 {
				t.Fatalf("non-power extent stats = %+v", stats)
			}

			metadata := refs[2]
			metadata.Kind = PageKeyDirectory
			if _, err := cache.Acquire(metadata); !errors.Is(err, ErrPageCacheReference) {
				t.Fatalf("non-power metadata error = %v, want %v", err, ErrPageCacheReference)
			}
		})
	}
}

func TestPageCacheNonPowerOfTwoDirtyReservationAccounting(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-non-power-dirty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	storeID := [16]byte{13, 7, 5, 3, 1, 2, 4, 6, 8, 10, 12, 14, 16, 15, 11, 9}
	const length = 3 * pageCacheTestPageSize
	page := make([]byte, length)
	payload, err := InitPage(page, PageHeader{
		StoreID: storeID, Generation: 1, LogicalID: 2,
		PageSize: length, PayloadLength: 32, Kind: PageDocument,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 1
	if _, err := SealPage(page); err != nil {
		t.Fatal(err)
	}
	ref := PageRef{
		Offset: 4 * pageCacheTestPageSize, LogicalID: 2, Generation: 1,
		Length: length, Kind: PageDocument,
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, MaxPageSize: 4 * pageCacheTestPageSize,
		ResidentBytes: 8 * pageCacheTestPageSize, StoreID: storeID,
		ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	if err := cache.AdmitDirty(ref, page, 2); err != nil {
		t.Fatal(err)
	}
	stats := cache.Stats()
	if stats.ResidentBytes != length || stats.ReservedBytes != 4*pageCacheTestPageSize ||
		stats.DirtyBytes != length ||
		cache.DirtyCapacityAvailable() != 4*pageCacheTestPageSize {
		t.Fatalf("dirty non-power stats = %+v, available=%d", stats, cache.DirtyCapacityAvailable())
	}
	cache.MarkDurable(1)
	if stats := cache.Stats(); stats.DirtyBytes != length ||
		cache.DirtyCapacityAvailable() != 4*pageCacheTestPageSize {
		t.Fatalf("early durability stats = %+v, available=%d", stats, cache.DirtyCapacityAvailable())
	}
	cache.MarkDurable(2)
	if stats := cache.Stats(); stats.DirtyBytes != 0 ||
		cache.DirtyCapacityAvailable() != 8*pageCacheTestPageSize {
		t.Fatalf("durable stats = %+v, available=%d", stats, cache.DirtyCapacityAvailable())
	}
	if !cache.Invalidate(ref) {
		t.Fatal("Invalidate durable non-power extent")
	}
	if stats := cache.Stats(); stats.ResidentBytes != 0 || stats.ReservedBytes != 0 {
		t.Fatalf("invalidated non-power stats = %+v", stats)
	}
}

func TestPageCacheMixedNonPowerReservationSymmetry(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-mixed-reservations-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	storeID := [16]byte{17, 7, 5, 3, 1, 2, 4, 6, 8, 10, 12, 14, 16, 15, 11, 9}
	logicalPages := [...]int{3, 5, 7, 15, 16}
	reservedPages := [...]int{4, 8, 8, 16, 16}
	refs := make([]PageRef, len(logicalPages))
	pages := make([][]byte, len(logicalPages))
	offset := uint64(4 * pageCacheTestPageSize)
	for i, span := range logicalPages {
		length := uint32(span * pageCacheTestPageSize)
		page := make([]byte, length)
		payload, initErr := InitPage(page, PageHeader{
			StoreID: storeID, Generation: 1, LogicalID: uint64(i + 2),
			PageSize: length, PayloadLength: 32, Kind: PageDocument,
		})
		if initErr != nil {
			t.Fatal(initErr)
		}
		payload[0] = byte(i + 1)
		if _, sealErr := SealPage(page); sealErr != nil {
			t.Fatal(sealErr)
		}
		if _, writeErr := file.WriteAt(page, int64(offset)); writeErr != nil {
			t.Fatal(writeErr)
		}
		refs[i] = PageRef{
			Offset: offset, LogicalID: uint64(i + 2), Generation: 1,
			Length: length, Kind: PageDocument,
		}
		pages[i] = page
		offset += uint64(length)
	}

	const capacityPages = 32
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, MaxPageSize: 16 * pageCacheTestPageSize,
		ResidentBytes: capacityPages * pageCacheTestPageSize, StoreID: storeID,
		ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	for i, ref := range refs {
		want := uint64(reservedPages[i] * pageCacheTestPageSize)
		if got := cache.ReservationBytes(ref.Length); got != want {
			t.Fatalf("ReservationBytes(%d pages) = %d, want %d", logicalPages[i], got, want)
		}
	}
	if cache.ReservationBytes(0) != 0 ||
		cache.ReservationBytes(3*pageCacheTestPageSize+1) != 0 ||
		cache.ReservationBytes(17*pageCacheTestPageSize) != 0 {
		t.Fatal("ReservationBytes accepted zero, unaligned, or oversized length")
	}
	var nilCache *PageCache
	if nilCache.ReservationBytes(3*pageCacheTestPageSize) != 0 {
		t.Fatal("nil cache returned a reservation")
	}
	if allocs := testing.AllocsPerRun(1000, func() {
		if cache.ReservationBytes(15*pageCacheTestPageSize) != 16*pageCacheTestPageSize {
			panic("wrong reservation")
		}
	}); allocs != 0 {
		t.Fatalf("ReservationBytes allocations = %v, want 0", allocs)
	}

	for i := 0; i < 3; i++ {
		lease, acquireErr := cache.Acquire(refs[i])
		if acquireErr != nil || lease.Payload()[0] != byte(i+1) {
			t.Fatalf("mixed Acquire(%d) = (%v,%v)", i, lease.Payload(), acquireErr)
		}
		lease.Release()
	}
	if stats := cache.Stats(); stats.ResidentBytes != 15*pageCacheTestPageSize ||
		stats.ReservedBytes != 20*pageCacheTestPageSize {
		t.Fatalf("initial mixed stats = %+v", stats)
	}
	large, err := cache.Acquire(refs[3])
	if err != nil || large.Payload()[0] != 4 {
		t.Fatalf("evicting mixed Acquire = (%v,%v)", large.Payload(), err)
	}
	large.Release()
	if stats := cache.Stats(); stats.Evictions == 0 {
		t.Fatalf("15-page demand did not evict mixed reservations: %+v", stats)
	}
	for i := 0; i < 4; i++ {
		cache.Invalidate(refs[i])
	}
	if stats := cache.Stats(); stats.ResidentBytes != 0 || stats.ReservedBytes != 0 {
		t.Fatalf("mixed demand reservations did not return to zero: %+v", stats)
	}

	for i := 0; i < 3; i++ {
		if err := cache.AdmitDirty(refs[i], pages[i], 2); err != nil {
			t.Fatalf("AdmitDirty(%d): %v", i, err)
		}
	}
	if stats := cache.Stats(); stats.DirtyBytes != 15*pageCacheTestPageSize ||
		stats.ReservedBytes != 20*pageCacheTestPageSize ||
		cache.DirtyCapacityAvailable() != 12*pageCacheTestPageSize {
		t.Fatalf("mixed dirty stats = %+v, available=%d", stats, cache.DirtyCapacityAvailable())
	}
	cache.MarkDurable(2)
	for i := 0; i < 3; i++ {
		if !cache.Invalidate(refs[i]) {
			t.Fatalf("Invalidate durable mixed extent %d", i)
		}
	}
	if stats := cache.Stats(); stats.DirtyBytes != 0 ||
		stats.ResidentBytes != 0 || stats.ReservedBytes != 0 {
		t.Fatalf("mixed dirty reservations did not return to zero: %+v", stats)
	}

	full, err := cache.Acquire(refs[4])
	if err != nil || len(full.Page()) != 16*pageCacheTestPageSize ||
		full.Payload()[0] != 5 {
		t.Fatalf("full-zone Acquire = (%d,%v,%v)", len(full.Page()), full.Payload(), err)
	}
	full.Release()
	if stats := cache.Stats(); stats.ReservedBytes != 16*pageCacheTestPageSize {
		t.Fatalf("full-zone reservation stats = %+v", stats)
	}
	if !cache.Invalidate(refs[4]) {
		t.Fatal("Invalidate full-zone extent")
	}
	if stats := cache.Stats(); stats.ResidentBytes != 0 || stats.ReservedBytes != 0 {
		t.Fatalf("full-zone reservation did not return to zero: %+v", stats)
	}
}

func TestPageCachePacksMetadataAndVariableExtentByQuantum(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-packed-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	storeID := [16]byte{4, 2, 8, 1, 6, 3, 7, 5, 9, 10, 11, 12, 13, 14, 15, 16}
	refs := make([]PageRef, 0, 5)
	offset := uint64(4 * pageCacheTestPageSize)
	for pageID := range 5 {
		length := uint32(pageCacheTestPageSize)
		kind := PageChunkDirectory
		if pageID == 4 {
			length = 4 * pageCacheTestPageSize
			kind = PageDocument
		}
		page := make([]byte, length)
		payload, initErr := InitPage(page, PageHeader{
			StoreID: storeID, Generation: 1, LogicalID: uint64(pageID + 2),
			PageSize: length, PayloadLength: 32, Kind: kind,
		})
		if initErr != nil {
			t.Fatal(initErr)
		}
		payload[0] = byte(pageID + 1)
		if _, sealErr := SealPage(page); sealErr != nil {
			t.Fatal(sealErr)
		}
		if _, writeErr := file.WriteAt(page, int64(offset)); writeErr != nil {
			t.Fatal(writeErr)
		}
		refs = append(refs, PageRef{
			Offset: offset, LogicalID: uint64(pageID + 2), Generation: 1,
			Length: length, Kind: kind,
		})
		offset += uint64(length)
	}

	const resident = 8 * pageCacheTestPageSize
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, MaxPageSize: 4 * pageCacheTestPageSize,
		ResidentBytes: resident, StoreID: storeID, ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	leases := make([]PageLease, len(refs))
	for index, ref := range refs {
		leases[index], err = cache.Acquire(ref)
		if err != nil || leases[index].Payload()[0] != byte(index+1) {
			t.Fatalf("Acquire(%d) = (%v,%v)", index, leases[index].Payload(), err)
		}
	}
	stats := cache.Stats()
	if stats.CapacityBytes != resident || stats.ResidentBytes != resident ||
		stats.Frames != 8 || stats.ReadyFrames != 5 || stats.PinnedPages != 5 {
		t.Fatalf("packed stats = %+v", stats)
	}
	for index := range leases {
		leases[index].Release()
	}
}

func TestPageCacheFrameControlIsPointerFree(t *testing.T) {
	frameType := reflect.TypeFor[pageCacheFrame]()
	var visit func(reflect.Type) bool
	visit = func(typ reflect.Type) bool {
		switch typ.Kind() {
		case reflect.Array:
			return visit(typ.Elem())
		case reflect.Struct:
			for field := range typ.NumField() {
				if !visit(typ.Field(field).Type) {
					return false
				}
			}
			return true
		case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map,
			reflect.Pointer, reflect.Slice, reflect.String, reflect.UnsafePointer:
			return false
		default:
			return true
		}
	}
	if !visit(frameType) {
		t.Fatalf("pageCacheFrame contains GC-visible pointer-bearing state: %v", frameType)
	}
	if frameType.Size() > 64 {
		t.Fatalf("pageCacheFrame is %d bytes, want at most one cache line", frameType.Size())
	}
	loadType := reflect.TypeFor[pageCacheRingLoad]()
	if !visit(loadType) {
		t.Fatalf("pageCacheRingLoad contains GC-visible pointer-bearing state: %v", loadType)
	}
	linkType := reflect.TypeFor[pageCacheBlockLink]()
	if !visit(linkType) {
		t.Fatalf("pageCacheBlockLink contains GC-visible pointer-bearing state: %v", linkType)
	}
	if linkType.Size() != 12 {
		t.Fatalf("pageCacheBlockLink is %d bytes, want 12", linkType.Size())
	}
}

func TestPageCacheBlockAllocatorSplitsMergesAndHandlesPartialZone(t *testing.T) {
	blocks := newPageCacheBlocks(37, 16)
	var slots [37]bool
	alloc := func(span int) int {
		t.Helper()
		index, ok := blocks.take(span)
		if !ok {
			t.Fatalf("take(%d) failed", span)
		}
		if index%span != 0 {
			t.Fatalf("take(%d) = %d, want aligned start", span, index)
		}
		for slot := index; slot < index+span; slot++ {
			if slots[slot] {
				t.Fatalf("take(%d) overlaps slot %d", span, slot)
			}
			slots[slot] = true
		}
		return index
	}
	free := func(index, span int) {
		t.Helper()
		for slot := index; slot < index+span; slot++ {
			if !slots[slot] {
				t.Fatalf("put(%d,%d) frees empty slot %d", index, span, slot)
			}
			slots[slot] = false
		}
		blocks.put(index, span)
	}

	first := alloc(16)
	second := alloc(16)
	tail4 := alloc(4)
	tail1 := alloc(1)
	if _, ok := blocks.take(1); ok {
		t.Fatal("take from exhausted allocator succeeded")
	}
	free(second, 16)
	free(tail1, 1)
	free(first, 16)
	free(tail4, 4)

	pair := newPageCacheBlocks(16, 16)
	first, _ = pair.take(1)
	second, _ = pair.take(1)
	pair.put(first, 1)
	pair.put(second, 1)
	if merged, ok := pair.take(2); !ok || merged != min(first, second) {
		t.Fatalf("merged take = (%d,%v), want (%d,true)", merged, ok, min(first, second))
	}
}

func TestPageCacheBlockAllocatorCoalescesFragmentedSmallExtents(t *testing.T) {
	blocks := newPageCacheBlocks(32, 16)
	allocated := make([]int, 32)
	for index := range allocated {
		var ok bool
		allocated[index], ok = blocks.take(1)
		if !ok {
			t.Fatalf("take slot %d failed", index)
		}
	}
	for _, index := range allocated[8:16] {
		blocks.put(index, 1)
	}
	index, ok := blocks.take(8)
	if want := allocated[8]; !ok || index != want {
		t.Fatalf("coalesced take = (%d,%v), want (%d,true)", index, ok, want)
	}
}

func TestPageCacheConcurrentHitsAndEvictions(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 32)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 4 * pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	const workers = 16
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for worker := range workers {
		go func() {
			defer group.Done()
			<-start
			for iteration := range 1000 {
				index := (worker*17 + iteration*13) & (len(refs) - 1)
				for {
					lease, acquireErr := cache.Acquire(refs[index])
					if errors.Is(acquireErr, ErrPageCachePinned) {
						runtime.Gosched()
						continue
					}
					if acquireErr != nil {
						errorsByWorker <- acquireErr
						return
					}
					if lease.Payload()[0] != byte(index+1) {
						lease.Release()
						errorsByWorker <- errors.New("wrong concurrent page payload")
						return
					}
					lease.Release()
					break
				}
			}
		}()
	}
	close(start)
	group.Wait()
	close(errorsByWorker)
	for workerErr := range errorsByWorker {
		t.Fatal(workerErr)
	}
	stats := cache.Stats()
	if stats.ResidentBytes > stats.CapacityBytes || stats.Evictions == 0 || stats.PinnedPages != 0 {
		t.Fatalf("concurrent stats = %+v", stats)
	}
}

func TestPageCacheConcurrentCloseDrainsResidentHits(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 8)
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 8 * pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		lease, acquireErr := cache.Acquire(ref)
		if acquireErr != nil {
			t.Fatal(acquireErr)
		}
		lease.Release()
	}
	const workers = 16
	var ready sync.WaitGroup
	var group sync.WaitGroup
	ready.Add(workers)
	group.Add(workers)
	workerErrors := make(chan error, workers)
	for worker := range workers {
		go func() {
			defer group.Done()
			announced := false
			for {
				lease, acquireErr := cache.Acquire(refs[worker&(len(refs)-1)])
				if errors.Is(acquireErr, ErrPageCacheClosed) {
					if !announced {
						ready.Done()
					}
					return
				}
				if acquireErr != nil {
					workerErrors <- acquireErr
					if !announced {
						ready.Done()
					}
					return
				}
				if !announced {
					announced = true
					ready.Done()
				}
				if lease.Payload()[0] != byte(worker&(len(refs)-1)+1) {
					lease.Release()
					workerErrors <- errors.New("wrong page during concurrent close")
					return
				}
				lease.Release()
			}
		}()
	}
	ready.Wait()
	closeErr := cache.Close()
	if closeErr != nil && !errors.Is(closeErr, ErrPageCachePinned) {
		t.Fatalf("concurrent Close = %v", closeErr)
	}
	group.Wait()
	close(workerErrors)
	for workerErr := range workerErrors {
		t.Fatal(workerErr)
	}
	if err := cache.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPageCacheDirtyAdmissionWaitsForDurability(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 2)
	page := make([]byte, pageCacheTestPageSize)
	if _, err := file.ReadAt(page, int64(refs[0].Offset)); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := cache.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})
	if err := cache.AdmitDirty(refs[0], page, 2); err != nil {
		t.Fatal(err)
	}
	if len(cache.dirtyFrames) != 1 {
		t.Fatalf("dirty queue = %d, want 1", len(cache.dirtyFrames))
	}
	if _, err := file.WriteAt([]byte{0xff}, int64(refs[0].Offset+PageHeaderSize)); err != nil {
		t.Fatal(err)
	}
	lease, err := cache.Acquire(refs[0])
	if err != nil || lease.Payload()[0] != 1 {
		t.Fatalf("Acquire admitted = (%v,%v)", lease.Payload(), err)
	}
	lease.Release()
	if _, err := cache.Acquire(refs[1]); !errors.Is(err, ErrPageCachePinned) {
		t.Fatalf("Acquire with dirty cache = %v, want %v", err, ErrPageCachePinned)
	}
	stats := cache.Stats()
	if stats.DirtyBytes != pageCacheTestPageSize || stats.PageReads != 0 || stats.CacheHits != 1 {
		t.Fatalf("dirty Stats = %+v", stats)
	}
	cache.MarkDurable(1)
	if len(cache.dirtyFrames) != 1 {
		t.Fatalf("early durability queue = %d, want 1", len(cache.dirtyFrames))
	}
	if stats := cache.Stats(); stats.DirtyBytes != pageCacheTestPageSize {
		t.Fatalf("early MarkDurable cleared page: %+v", stats)
	}
	cache.MarkDurable(2)
	if len(cache.dirtyFrames) != 0 {
		t.Fatalf("durable queue = %d, want 0", len(cache.dirtyFrames))
	}
	second, err := cache.Acquire(refs[1])
	if err != nil {
		t.Fatal(err)
	}
	second.Release()
	if stats := cache.Stats(); stats.DirtyBytes != 0 || stats.PageReads != 1 || stats.Evictions != 1 {
		t.Fatalf("durable Stats = %+v", stats)
	}
}

func TestPageCacheSeparatesReusedOffsetGenerations(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 1)
	oldPage := make([]byte, pageCacheTestPageSize)
	if _, err := file.ReadAt(oldPage, int64(refs[0].Offset)); err != nil {
		t.Fatal(err)
	}
	newRef := refs[0]
	newRef.Generation++
	newPage := make([]byte, pageCacheTestPageSize)
	payload, err := InitPage(newPage, PageHeader{
		StoreID: storeID, Generation: newRef.Generation, LogicalID: newRef.LogicalID,
		PageSize: newRef.Length, PayloadLength: 32, Kind: newRef.Kind,
	})
	if err != nil {
		t.Fatal(err)
	}
	payload[0] = 9
	if _, err := SealPage(newPage); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: 2 * pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.AdmitDirty(refs[0], oldPage, 2); err != nil {
		t.Fatal(err)
	}
	if err := cache.AdmitDirty(newRef, newPage, 3); err != nil {
		t.Fatal(err)
	}
	if len(cache.dirtyFrames) != 2 {
		t.Fatalf("generation dirty queue = %d, want 2", len(cache.dirtyFrames))
	}
	cache.MarkDurable(2)
	if len(cache.dirtyFrames) != 1 || cache.dirtyFrames[0].generation != 3 {
		t.Fatalf("partial durability queue = %+v", cache.dirtyFrames)
	}
	oldLease, err := cache.Acquire(refs[0])
	if err != nil || oldLease.Payload()[0] != 1 {
		t.Fatalf("old generation = (%v,%v)", oldLease.Payload(), err)
	}
	newLease, err := cache.Acquire(newRef)
	if err != nil || newLease.Payload()[0] != 9 {
		t.Fatalf("new generation = (%v,%v)", newLease.Payload(), err)
	}
	oldLease.Release()
	newLease.Release()
	if stats := cache.Stats(); stats.ReadyFrames != 2 || stats.ResidentBytes != 2*pageCacheTestPageSize {
		t.Fatalf("generation collision stats = %+v", stats)
	}
}

func TestPageCacheDiscardDirtyGeneration(t *testing.T) {
	file, storeID, refs := newPageCacheFixture(t, 1)
	page := make([]byte, pageCacheTestPageSize)
	if _, err := file.ReadAt(page, int64(refs[0].Offset)); err != nil {
		t.Fatal(err)
	}
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: pageCacheTestPageSize, ResidentBytes: pageCacheTestPageSize,
		StoreID: storeID, ReadConcurrency: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()
	if err := cache.AdmitDirty(refs[0], page, 3); err != nil {
		t.Fatal(err)
	}
	lease, err := cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	if err := cache.DiscardDirty(3); !errors.Is(err, ErrPageCachePinned) {
		t.Fatalf("pinned DiscardDirty = %v, want %v", err, ErrPageCachePinned)
	}
	if len(cache.dirtyFrames) != 1 {
		t.Fatalf("failed discard changed dirty queue: %+v", cache.dirtyFrames)
	}
	lease.Release()
	if err := cache.DiscardDirty(3); err != nil {
		t.Fatal(err)
	}
	if len(cache.dirtyFrames) != 0 {
		t.Fatalf("discard retained dirty queue: %+v", cache.dirtyFrames)
	}
	if stats := cache.Stats(); stats.ResidentBytes != 0 || stats.DirtyBytes != 0 {
		t.Fatalf("Stats after discard = %+v", stats)
	}
	lease, err = cache.Acquire(refs[0])
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if stats := cache.Stats(); stats.PageReads != 1 {
		t.Fatalf("discarded page did not read from file: %+v", stats)
	}
}

func newPageCacheFixture(t testing.TB, count int) (*os.File, [16]byte, []PageRef) {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "store-page-cache-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	storeID := [16]byte{1, 3, 5, 7, 9, 11, 13, 15, 2, 4, 6, 8, 10, 12, 14, 16}
	refs := make([]PageRef, count)
	for i := range refs {
		offset := uint64(4+i) * pageCacheTestPageSize
		logicalID := uint64(i + 2)
		page := make([]byte, pageCacheTestPageSize)
		payload, initErr := InitPage(page, PageHeader{
			StoreID: storeID, Generation: 1, LogicalID: logicalID,
			PageSize: pageCacheTestPageSize, PayloadLength: 32, Kind: PageDocument,
		})
		if initErr != nil {
			t.Fatal(initErr)
		}
		payload[0] = byte(i + 1)
		if _, sealErr := SealPage(page); sealErr != nil {
			t.Fatal(sealErr)
		}
		if _, writeErr := file.WriteAt(page, int64(offset)); writeErr != nil {
			t.Fatal(writeErr)
		}
		refs[i] = PageRef{
			Offset: offset, LogicalID: logicalID, Generation: 1,
			Length: pageCacheTestPageSize, Kind: PageDocument,
		}
	}
	return file, storeID, refs
}
