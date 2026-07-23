package storeio

import (
	"errors"
	"os"
	"testing"
)

func TestFreeTreeCopyOnWriteSplitUpdateDeleteAndBoundedWalk(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "free-tree-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	committer, err := NewCommitter(file, DeviceOptions{
		Backend: BackendPortable, BufferCount: 32, BufferSize: os.Getpagesize(),
	}, CommitterOptions{QueueSlots: 4, MaxPagesPerBatch: 16, GroupLimit: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer committer.Close()
	cache, err := NewPageCache(file, PageCacheOptions{
		PageSize: int(testSuperblockPageSize), ResidentBytes: 64 * int64(testSuperblockPageSize),
		StoreID: testStoreID, ReadConcurrency: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer cache.Close()

	pageSize := uint64(testSuperblockPageSize)
	fileEnd := 512 * pageSize
	nextID := uint64(2)
	generation := uint64(0)
	var root PageRef
	mutate := func(extent FreeExtent, deleting bool) FreeTreeMutation {
		t.Helper()
		generation++
		tx, beginErr := BeginWriteTransaction(committer, cache, 16, WriteTransactionOptions{
			StoreID: testStoreID, Generation: generation, PageSize: testSuperblockPageSize,
			FileEnd: fileEnd, NextLogicalID: nextID,
		})
		if beginErr != nil {
			t.Fatal(beginErr)
		}
		bounds := FreeTreeBounds{FileEnd: fileEnd, NextLogicalID: nextID}
		var mutation FreeTreeMutation
		if deleting {
			mutation, err = DeleteFreeTree(cache, tx, root, extent.Offset, bounds)
		} else {
			mutation, err = UpsertFreeTree(cache, tx, root, extent, bounds)
		}
		if err != nil {
			_ = tx.Abort()
			t.Fatal(err)
		}
		statePage, allocateErr := tx.Allocate(PageStateRoot, testSuperblockPageSize, StateRootLogicalID)
		if allocateErr != nil {
			_ = tx.Abort()
			t.Fatal(allocateErr)
		}
		state := StateRoot{
			StoreID: testStoreID, Generation: generation, PageSize: testSuperblockPageSize,
			NextLogicalID: tx.NextLogicalID(), ChunkDocuments: 64,
		}
		if _, encodeErr := EncodeStateRootPage(statePage.Bytes(), state, tx.FileEnd()); encodeErr != nil {
			_ = tx.Abort()
			t.Fatal(encodeErr)
		}
		if stageErr := statePage.Stage(); stageErr != nil {
			_ = tx.Abort()
			t.Fatal(stageErr)
		}
		freeRoot := mutation.Root
		var checksum uint32
		if freeRoot != (PageRef{}) {
			lease, acquireErr := cache.Acquire(freeRoot)
			if acquireErr != nil {
				t.Fatal(acquireErr)
			}
			checksum = PageChecksum(lease.Page())
			lease.Release()
		}
		if publishErr := tx.Publish(statePage.Ref(), PageChecksum(statePage.Bytes()), freeRoot.Offset, freeRoot.Length, checksum); publishErr != nil {
			_ = tx.Abort()
			t.Fatal(publishErr)
		}
		if waitErr := committer.Wait(generation); waitErr != nil {
			t.Fatal(waitErr)
		}
		cache.MarkDurable(generation)
		root, fileEnd, nextID = mutation.Root, tx.FileEnd(), tx.NextLogicalID()
		return mutation
	}

	const extentCount = 180
	for i := range extentCount {
		mutation := mutate(FreeExtent{
			Offset: (2 + uint64(i)) * pageSize, Length: pageSize, RetiredGeneration: 1,
		}, false)
		if !mutation.Changed || mutation.Root == (PageRef{}) {
			t.Fatalf("insert %d mutation = %+v", i, mutation)
		}
	}
	lease, err := cache.Acquire(root)
	if err != nil {
		t.Fatal(err)
	}
	view, err := OpenFreeDirectoryPage(lease.Page(), fileEnd, nextID)
	lease.Release()
	if err != nil {
		t.Fatal(err)
	}
	if view.Header().Level == 0 {
		t.Fatal("free tree did not split")
	}
	extents, err := AppendFreeTreeExtents(cache, root, FreeTreeBounds{FileEnd: fileEnd, NextLogicalID: nextID}, nil, extentCount)
	if err != nil || len(extents) != extentCount {
		t.Fatalf("AppendFreeTreeExtents = (%d,%v), want %d", len(extents), err, extentCount)
	}
	if _, err := AppendFreeTreeExtents(cache, root, FreeTreeBounds{FileEnd: fileEnd, NextLogicalID: nextID}, nil, 10); !errors.Is(err, ErrRetiredExtentCapacity) {
		t.Fatalf("bounded walk = %v, want %v", err, ErrRetiredExtentCapacity)
	}

	updated := extents[90]
	updated.RetiredGeneration = generation
	mutation := mutate(updated, false)
	if !mutation.Found || !mutation.Changed || mutation.RetiredCount < 2 {
		t.Fatalf("update mutation = %+v", mutation)
	}
	mutation = mutate(extents[0], true)
	if !mutation.Found || !mutation.Changed {
		t.Fatalf("delete mutation = %+v", mutation)
	}
	extents, err = AppendFreeTreeExtents(cache, root, FreeTreeBounds{FileEnd: fileEnd, NextLogicalID: nextID}, extents[:0], extentCount)
	if err != nil || len(extents) != extentCount-1 || extents[0].Offset != 3*pageSize {
		t.Fatalf("walk after delete = first %+v len %d err %v", extents[0], len(extents), err)
	}
}
