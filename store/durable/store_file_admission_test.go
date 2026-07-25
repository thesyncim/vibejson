package durable

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/store"
)

// The key- and chunk-directory descents reconstruct resident pages instead of
// revalidating them, which is what makes a warm point read cost about a
// microsecond instead of five. That trade is only paid for if the collection's
// admission validator really does perform the full structural check for those
// two kinds, exactly once, before any reader can see the bytes. These tests
// pin that obligation from both ends: the validator must reject a malformed
// directory page, and a directory page corrupted on disk must still be refused
// by the reader rather than reinterpreted.

func testAdmissionValidator() *fileStorePageValidator {
	validator := newFileStorePageValidator(uint32(testAdmissionPageSize), 0, 64)
	validator.fileEnd.Store(64 * testAdmissionPageSize)
	validator.nextLogicalID.Store(100)
	validator.chunkHighWater.Store(20)
	return validator
}

const testAdmissionPageSize = 4096

// TestPageValidatorCoversDirectoryKinds fails against any build whose
// admission validator lets a key- or chunk-directory page through unchecked.
// It is deliberately not an end-to-end test: the descents cannot report a
// missing check, because a missing check is precisely the absence of an error.
func TestPageValidatorCoversDirectoryKinds(t *testing.T) {
	validator := testAdmissionValidator()
	storeID := [16]byte{1, 2, 3}

	keyHeader := storeio.KeyDirectoryHeader{
		StoreID: storeID, Generation: 3, LogicalID: 9,
		PageSize: testAdmissionPageSize,
	}
	keyPage := make([]byte, testAdmissionPageSize)
	keyPage, err := storeio.EncodeKeyDirectoryLeaf(keyPage, keyHeader, []storeio.KeyDirectoryEntry{
		{Key: []byte("alpha"), Location: storeio.KeyLocation{Chunk: 3, Slot: 5}},
		{Key: []byte("omega"), Location: storeio.KeyLocation{Chunk: 7, Slot: 1}},
	}, 100, 20, 64)
	if err != nil {
		t.Fatal(err)
	}
	keyRef := storeio.PageRef{
		Offset: 8 * testAdmissionPageSize, LogicalID: 9, Generation: 3,
		Length: testAdmissionPageSize, Kind: storeio.PageKeyDirectory,
	}
	if err := validator.validate(keyPage, keyRef); err != nil {
		t.Fatalf("valid key-directory page rejected: %v", err)
	}

	chunkPage := make([]byte, testAdmissionPageSize)
	chunkHeader := storeio.ChunkDirectoryHeader{
		StoreID: storeID, Generation: 3, LogicalID: 11,
		PageSize: testAdmissionPageSize, Prefix: 0, Bitmap: 1, Shift: 0,
	}
	documentRef := storeio.PageRef{
		Offset: 12 * testAdmissionPageSize, LogicalID: 12, Generation: 3,
		Length: testAdmissionPageSize, Kind: storeio.PageDocument,
	}
	chunkPage, err = storeio.EncodeChunkDirectoryPage(chunkPage, chunkHeader,
		[]storeio.PageRef{documentRef}, 64*testAdmissionPageSize, 100)
	if err != nil {
		t.Fatal(err)
	}
	chunkRef := storeio.PageRef{
		Offset: 10 * testAdmissionPageSize, LogicalID: 11, Generation: 3,
		Length: testAdmissionPageSize, Kind: storeio.PageChunkDirectory,
	}
	if err := validator.validate(chunkPage, chunkRef); err != nil {
		t.Fatalf("valid chunk-directory page rejected: %v", err)
	}

	// Corrupt each payload in a way the common page checksum cannot see,
	// because the checksum is recomputed over the damage. Only the typed
	// structural check can catch it, and only if the validator runs one.
	for _, corrupt := range []struct {
		name  string
		page  []byte
		ref   storeio.PageRef
		spoil func([]byte)
	}{
		{
			name: "key directory record count",
			page: append([]byte(nil), keyPage...),
			ref:  keyRef,
			spoil: func(page []byte) {
				payload := page[storeio.PageHeaderSize:]
				binary.LittleEndian.PutUint16(payload[6:8], 600)
			},
		},
		{
			name: "key directory entry order",
			page: append([]byte(nil), keyPage...),
			ref:  keyRef,
			spoil: func(page []byte) {
				payload := page[storeio.PageHeaderSize:]
				// Swap the two key-length prefixes so the stored keys decode out
				// of order, which every binary search silently depends on.
				first := storeio.KeyDirectoryPayloadHeaderSize
				second := first + storeio.KeyDirectoryLeafRecordSize
				a := binary.LittleEndian.Uint32(payload[first : first+4])
				b := binary.LittleEndian.Uint32(payload[second : second+4])
				binary.LittleEndian.PutUint32(payload[first:first+4], b)
				binary.LittleEndian.PutUint32(payload[second:second+4], a)
			},
		},
		{
			name: "chunk directory child offset",
			page: append([]byte(nil), chunkPage...),
			ref:  chunkRef,
			spoil: func(page []byte) {
				payload := page[storeio.PageHeaderSize:]
				binary.LittleEndian.PutUint64(
					payload[storeio.ChunkDirectoryPayloadHeaderSize:], ^uint64(0)>>1)
			},
		},
	} {
		t.Run(corrupt.name, func(t *testing.T) {
			corrupt.spoil(corrupt.page)
			if _, err := storeio.SealPage(corrupt.page); err != nil {
				t.Fatalf("reseal: %v", err)
			}
			if err := validator.validate(corrupt.page, corrupt.ref); err == nil {
				t.Fatal("admission accepted a structurally corrupt directory page")
			}
		})
	}
}

// TestCollectionRejectsCorruptKeyDirectoryOnDisk drives the same obligation
// through a real collection: a resealed, structurally invalid key-directory
// page must make the read fail, not make the reader index outside the payload.
func TestCollectionRejectsCorruptKeyDirectoryOnDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "corrupt.vibe")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	options := Options{
		Collection: store.Options{ChunkDocuments: 16}, ResidentBytes: 64 << 20,
		Backend: BackendPortable,
	}
	collection, err := Create(file, options)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 400 {
		if _, err := collection.Put(fmt.Sprintf("key-%09d", i), benchDocument(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := collection.Flush(); err != nil {
		t.Fatal(err)
	}
	if err := collection.Close(); err != nil {
		t.Fatal(err)
	}

	if !corruptOneKeyDirectoryLeaf(t, file, options.PageSize) {
		t.Skip("no key-directory leaf found on disk to corrupt")
	}

	reopened, err := Open(file, options)
	if err != nil {
		// Refusing to open is an equally acceptable outcome; the point is that
		// the damage is never trusted.
		return
	}
	defer reopened.Close()
	failures := 0
	for i := range 400 {
		if _, _, err := reopened.AppendRaw(nil, fmt.Sprintf("key-%09d", i)); err != nil {
			failures++
		}
	}
	if failures == 0 {
		t.Fatal("corrupt key-directory leaf was never rejected by any read")
	}
}

// corruptOneKeyDirectoryLeaf scrambles the declared record count of every
// key-directory leaf in the file and reseals each page, so the corruption
// survives the checksum. It damages every leaf rather than the first because
// copy-on-write leaves the file full of superseded ones: corrupting a retired
// generation proves nothing, and which offset holds the live leaf is not
// something a test should have to know.
//
// It returns false when the store's shape produced no leaf at all, which the
// caller treats as an inconclusive run rather than a pass.
func corruptOneKeyDirectoryLeaf(t *testing.T, file *os.File, pageSize int) bool {
	t.Helper()
	if pageSize == 0 {
		pageSize = 4096
	}
	info, err := file.Stat()
	if err != nil {
		t.Fatal(err)
	}
	page := make([]byte, pageSize)
	corrupted := false
	for offset := int64(0); offset+int64(pageSize) <= info.Size(); offset += int64(pageSize) {
		if _, err := file.ReadAt(page, offset); err != nil {
			t.Fatal(err)
		}
		header, payload, err := storeio.OpenPage(page)
		if err != nil || header.Kind != storeio.PageKeyDirectory ||
			len(payload) < storeio.KeyDirectoryPayloadHeaderSize || payload[4] != 0 {
			continue
		}
		binary.LittleEndian.PutUint16(payload[6:8], 600)
		if _, err := storeio.SealPage(page); err != nil {
			t.Fatal(err)
		}
		if _, err := file.WriteAt(page, offset); err != nil {
			t.Fatal(err)
		}
		corrupted = true
	}
	if corrupted {
		if err := file.Sync(); err != nil {
			t.Fatal(err)
		}
	}
	return corrupted
}

// TestCollectionRetirementBackpressureRecovers pins the operational contract
// stated on Options.MaxRetiredExtents: a snapshot held across a write loop
// eventually fails writes, the failure names the snapshot that caused it, and
// closing that snapshot restores the writer without reopening the file.
//
// A wedge here was a real bug once. The bound is small so the loop is short.
func TestCollectionRetirementBackpressureRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "pressure.vibe")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	// MaxBatchDocuments: 1 keeps the single-document transaction geometry. The
	// default batch size reserves for a whole Update, which pushes one
	// worst-case transaction past the deliberately small retirement table this
	// test needs to reach backpressure in a short loop.
	collection, err := Create(file, Options{
		Collection: store.Options{ChunkDocuments: 16}, ResidentBytes: 64 << 20,
		Backend: BackendPortable, MaxRetiredExtents: 256, MaxBatchDocuments: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer collection.Close()
	const keys = 32
	for i := range keys {
		if _, err := collection.Put(fmt.Sprintf("key-%09d", i), benchDocument(i)); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := collection.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var failure error
	accepted := 0
	for i := range 5000 {
		if _, err := collection.Put(fmt.Sprintf("key-%09d", i%keys), benchDocument(i)); err != nil {
			failure = err
			break
		}
		accepted++
	}
	if failure == nil {
		t.Fatalf("no backpressure after %d writes under a held snapshot", accepted)
	}
	if !errors.Is(failure, storeio.ErrRetiredExtentCapacity) {
		t.Fatalf("unexpected failure: %v", failure)
	}
	// The bare sentinel names a resource, not a cause. An operator needs the
	// pinned generation to find the reader responsible.
	message := failure.Error()
	for _, want := range []string{"snapshot", "generation", "MaxRetiredExtents"} {
		if !strings.Contains(message, want) {
			t.Fatalf("failure message %q does not mention %q", message, want)
		}
	}
	if snapshotGeneration := snapshot.Generation(); !strings.Contains(message, fmt.Sprint(snapshotGeneration)) {
		t.Fatalf("failure message %q does not name pinned generation %d", message, snapshotGeneration)
	}

	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	// Recovery must need nothing but the release: no reopen, no compaction.
	for i := range keys {
		if _, err := collection.Put(fmt.Sprintf("key-%09d", i), benchDocument(i)); err != nil {
			t.Fatalf("write %d still fails after releasing the snapshot: %v", i, err)
		}
	}
	for i := range keys {
		got, ok, err := collection.AppendRaw(nil, fmt.Sprintf("key-%09d", i))
		if err != nil || !ok {
			t.Fatalf("read %d after recovery: ok=%v err=%v", i, ok, err)
		}
		if string(got) != string(benchDocument(i)) {
			t.Fatalf("read %d returned %s", i, got)
		}
	}
}

// TestDefaultCommitBufferDepth pins the shipped commit-buffer pool to the depth
// its documented throughput table was measured at. A pool that merely exceeds
// one worst-case transaction gives a serialized writer depth one, which costs
// one durability fence per Put and was measured at 3.5 ms against 197 µs.
func TestDefaultCommitBufferDepth(t *testing.T) {
	for _, c := range []struct {
		name             string
		transactionPages int
		maxPageSize      int
		wantBuffers      int
		wantAtLeastDepth int
	}{
		{"shipped geometry", 113, 64 << 10, 512, defaultCommitDepth},
		{"small extents", 113, 4 << 10, 512, defaultCommitDepth},
		// A transaction whose own worst case already exceeds the staging budget
		// keeps the frugal depth-one pool: the budget is a cap, not a target.
		{"oversized documents", 1074, 64 << 10, 2048, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := defaultBufferCount(c.transactionPages, c.maxPageSize)
			if got != c.wantBuffers {
				t.Fatalf("defaultBufferCount(%d,%d) = %d, want %d",
					c.transactionPages, c.maxPageSize, got, c.wantBuffers)
			}
			if got <= c.transactionPages {
				t.Fatalf("pool of %d cannot hold one %d-page transaction", got, c.transactionPages)
			}
			if depth := got / (c.transactionPages + 1); depth < c.wantAtLeastDepth {
				t.Fatalf("pool of %d gives depth %d, want at least %d",
					got, depth, c.wantAtLeastDepth)
			}
		})
	}
}
