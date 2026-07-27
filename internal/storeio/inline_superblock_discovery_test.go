package storeio

import (
	"errors"
	"os"
	"strconv"
	"testing"
)

func TestDiscoverMutableInlinePageSize(t *testing.T) {
	for _, pageSize := range []uint32{4096, 16 << 10, 64 << 10} {
		t.Run(strconv.FormatUint(uint64(pageSize), 10), func(t *testing.T) {
			file := openDiscoveryFixture(t)
			root := discoveryInlineRoot(t, pageSize, 7, [16]byte{1})
			writeDiscoveryRoot(t, file, root, 0)
			if got, err := DiscoverMutableInlinePageSize(file); err != nil || got != pageSize {
				t.Fatalf("DiscoverMutableInlinePageSize() = %d, %v; want %d", got, err, pageSize)
			}
		})
	}
}

func TestDiscoverMutableInlinePageSizeFromAlternate(t *testing.T) {
	const pageSize = uint32(64 << 10)
	file := openDiscoveryFixture(t)
	root := discoveryInlineRoot(t, pageSize, 9, [16]byte{2})
	writeDiscoveryRoot(t, file, root, int64(pageSize))
	if got, err := DiscoverMutableInlinePageSize(file); err != nil || got != pageSize {
		t.Fatalf("DiscoverMutableInlinePageSize() = %d, %v; want %d", got, err, pageSize)
	}
}

func TestDiscoverMutableInlinePageSizeRejectsConflictingCandidates(t *testing.T) {
	file := openDiscoveryFixture(t)
	root4K := discoveryInlineRoot(t, 4096, 3, [16]byte{3})
	root8K := discoveryInlineRoot(t, 8192, 4, [16]byte{4})
	writeDiscoveryRoot(t, file, root4K, 4096)
	writeDiscoveryRoot(t, file, root8K, 8192)
	if _, err := DiscoverMutableInlinePageSize(file); !errors.Is(err, ErrSuperblockConflict) {
		t.Fatalf("DiscoverMutableInlinePageSize() error = %v; want %v", err, ErrSuperblockConflict)
	}
}

func TestDiscoverMutableInlinePageSizeRejectsMissingRoots(t *testing.T) {
	file := openDiscoveryFixture(t)
	if err := file.Truncate(128 << 10); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverMutableInlinePageSize(file); !errors.Is(err, ErrSuperblockNotFound) {
		t.Fatalf("DiscoverMutableInlinePageSize() error = %v; want %v", err, ErrSuperblockNotFound)
	}
}

func openDiscoveryFixture(t *testing.T) *os.File {
	t.Helper()
	file, err := os.CreateTemp(t.TempDir(), "inline-discovery-*")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func discoveryInlineRoot(
	t *testing.T, pageSize uint32, generation uint64, storeID [16]byte,
) InlineSuperblock {
	t.Helper()
	layout, err := MutableStoreLayout(pageSize)
	if err != nil {
		t.Fatal(err)
	}
	state := StateRoot{
		StoreID: storeID, Generation: generation, PageSize: pageSize,
		NextLogicalID: 2, ChunkDocuments: 64,
	}
	return InlineSuperblock{
		StoreID: storeID, Generation: generation, FileEnd: layout.DataStart,
		PageSize: pageSize, State: state,
		FreeDelta: NewInlineFreeDelta(PageRef{}, PageRef{}),
	}
}

func writeDiscoveryRoot(t *testing.T, file *os.File, root InlineSuperblock, offset int64) {
	t.Helper()
	var page [InlineSuperblockSize]byte
	encoded, err := EncodeInlineSuperblock(page[:], root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(encoded, offset); err != nil {
		t.Fatal(err)
	}
}
