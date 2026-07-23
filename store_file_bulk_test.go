package slopjson

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/bits"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/thesyncim/slopjson/internal/storeio"
)

func TestWriteFileStoreBulkPreservesDocumentsIndexesTTLAndMutation(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 7, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	const documents = 25
	for i := range documents {
		status := "idle"
		if i%3 == 0 {
			status = "active"
		}
		padding := strings.Repeat("x", i*80)
		if i == documents-1 {
			padding = strings.Repeat("x", 60<<10)
		}
		document := fmt.Sprintf(
			`{"id":%d,"meta":{"tenant":%q,"status":%q},"padding":%q}`,
			i, []string{"acme", "other"}[i&1], status, padding,
		)
		if i == 9 {
			document = `{"id":9,"meta":{"tenant":"other","status":"ac\u0074ive"},"padding":"` +
				strings.Repeat("x", 900) + `"}`
		}
		if err := builder.Append(fmt.Sprintf("k%02d", i), []byte(document)); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(24 * time.Hour).Truncate(time.Millisecond)
	if !source.SetDeadline("k04", deadline) {
		t.Fatal("source SetDeadline failed")
	}

	options := testFileStoreOptions()
	options.Store.ChunkDocuments = 4
	options.ResidentBytes = 8 << 20
	options.BufferCount = 128
	options.MaxRetiredExtents = 512
	options.Indexes = []StoreIndexDefinition{
		{Name: "status", Paths: []string{"/meta/status"}},
		{Name: "tenant_status", Paths: []string{"/meta/tenant", "/meta/status"}},
	}
	options.Float64Columns = []string{"/id"}
	file, err := os.CreateTemp(t.TempDir(), "file-store-bulk-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	size, err := source.WriteFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("bulk file size = %d", size)
	}

	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if got := store.Stats().DocumentCount; got != documents {
		t.Fatalf("bulk document count = %d, want %d", got, documents)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	idAggregate, covered, err := snapshot.ReduceFloat64Path("/id")
	if err := snapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if err != nil || !covered ||
		idAggregate != (Float64Aggregate{Count: documents, Sum: 300, Min: 0, Max: documents - 1}) {
		t.Fatalf("bulk id reduction = (%+v,%v,%v)", idAggregate, covered, err)
	}
	for _, row := range []int{0, 9, documents - 1} {
		key := fmt.Sprintf("k%02d", row)
		raw, ok, err := store.AppendRaw(nil, key)
		if err != nil || !ok || !strings.Contains(string(raw), fmt.Sprintf(`"id":%d`, row)) {
			t.Fatalf("bulk %s = (%q,%v,%v)", key, raw, ok, err)
		}
	}
	if got, ok, err := store.Deadline("k04"); err != nil || !ok || !got.Equal(deadline) {
		t.Fatalf("bulk deadline = (%v,%v,%v), want %v", got, ok, err, deadline)
	}

	needle := func(src string) Index {
		t.Helper()
		needed, err := RequiredIndexEntries([]byte(src))
		if err != nil {
			t.Fatal(err)
		}
		index, err := BuildIndex([]byte(src), make([]IndexEntry, needed))
		if err != nil {
			t.Fatal(err)
		}
		return index
	}
	countMasks := func(masks []StoreMask) int {
		count := 0
		for _, mask := range masks {
			count += bits.OnesCount64(mask.Bits)
		}
		return count
	}
	active, acme := needle(`"active"`), needle(`"acme"`)
	indexSnapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var indexWorkspace FileIndexWorkspace
	masks, err := indexSnapshot.AppendIndexMasksInto(
		nil, &indexWorkspace, "status", active,
	)
	if err != nil || countMasks(masks) != 9 {
		t.Fatalf("bulk active masks = (%+v,%v), count %d", masks, err, countMasks(masks))
	}
	if stats := indexWorkspace.LastProbeStats(); stats.CertificateRows != 9 ||
		stats.DocumentRecheckRows != 0 || stats.PostingPages == 0 ||
		stats.PostingPages >= stats.CandidateChunks {
		t.Fatalf("bulk coalesced status probe stats = %+v", stats)
	}
	masks, err = indexSnapshot.AppendIndexMasksInto(
		masks[:0], &indexWorkspace, "tenant_status", acme, active,
	)
	if err != nil || countMasks(masks) != 5 {
		t.Fatalf("bulk compound masks = (%+v,%v), count %d", masks, err, countMasks(masks))
	}
	if stats := indexWorkspace.LastProbeStats(); stats.CertificateRows != 5 ||
		stats.DocumentRecheckRows != 0 || stats.PostingPages == 0 ||
		stats.PostingPages >= stats.CandidateChunks {
		t.Fatalf("bulk coalesced compound probe stats = %+v", stats)
	}
	if err := indexSnapshot.Close(); err != nil {
		t.Fatal(err)
	}

	if created, err := store.Put("k00", []byte(`{"id":0,"meta":{"tenant":"acme","status":"idle"}}`)); err != nil || created {
		t.Fatalf("bulk update = (%v,%v)", created, err)
	}
	if deleted, err := store.Delete("k03"); err != nil || !deleted {
		t.Fatalf("bulk delete = (%v,%v)", deleted, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if _, ok, err := reopened.AppendRaw(nil, "k03"); err != nil || ok {
		t.Fatalf("reopened deleted k03 = (%v,%v)", ok, err)
	}
	snapshot, err = reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	idAggregate, covered, err = snapshot.ReduceFloat64Path("/id")
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered ||
		idAggregate != (Float64Aggregate{Count: documents - 1, Sum: 297, Min: 0, Max: documents - 1}) {
		t.Fatalf("reopened bulk id reduction = (%+v,%v,%v)", idAggregate, covered, err)
	}
	masks, err = reopened.AppendIndexMasks(masks[:0], "status", active)
	if err != nil || countMasks(masks) != 7 {
		t.Fatalf("reopened active masks = (%+v,%v), count %d", masks, err, countMasks(masks))
	}
}

func TestWriteFileStoreBulkGroupsExactDocumentsAndPeelsMutations(t *testing.T) {
	const documents = 1024
	options := testFileStoreOptions()
	options.Store = StoreOptions{ChunkDocuments: 8, ShapeTapes: true}
	options.MaxPageSize = 64 << 10
	options.ResidentBytes = 8 << 20
	options.Float64Columns = []string{"/score"}
	builder, err := NewStoreBuilder(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	for row := range documents {
		document := fmt.Appendf(
			nil,
			`{"tenant":"t%d","status":"s%d","score":%d,"active":%t,"payload":"%s"}`,
			row&3, row%11, row, row&1 == 0, strings.Repeat("x", 96+(row&7)),
		)
		if err := builder.Append(fmt.Sprintf("doc:%04d", row), document); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "file-store-groups-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	size, err := source.WriteFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if size <= 0 {
		t.Fatalf("grouped file size = %d", size)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	state := store.state.Load()
	groupRef, ok, err := storeio.LookupChunkTree(store.cache, state.chunkRoot, 0, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok || groupRef.Kind != storeio.PageDocumentGroup {
		t.Fatalf("chunk zero ref = (%+v,%v,%v)", groupRef, ok, err)
	}
	columnRef, detached, err := storeio.DocumentGroupFloat64Sidecar(
		groupRef, uint32(options.PageSize),
	)
	if err != nil || !detached || columnRef.Kind != storeio.PageFloat64Group {
		t.Fatalf("group float64 sidecar = (%+v,%v,%v)", columnRef, detached, err)
	}
	if state.root.Float64ScanHead.Kind != storeio.PageFloat64Catalog {
		t.Fatalf("float64 scan head = %+v, want catalog", state.root.Float64ScanHead)
	}
	var scanProjectionRefs []storeio.PageRef
	catalogColumn := storeio.PageRef{}
	directoryBounds := storeio.Float64DirectoryBounds{
		FileEnd:       state.super.FileEnd,
		NextLogicalID: state.root.NextLogicalID,
	}
	err = storeio.WalkFloat64DirectoryLeaves(
		store.cache, state.root.Float64ScanHead, directoryBounds,
		uint32(options.PageSize),
		func(leaf storeio.Float64DirectoryView) error {
			for i := 0; i < leaf.Len(); i++ {
				entry, _ := leaf.EntryAt(i)
				if catalogColumn == (storeio.PageRef{}) {
					catalogColumn = entry.Ref
				}
				scanProjectionRefs = append(
					scanProjectionRefs, entry.Ref,
				)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	err = storeio.WalkFloat64DirectoryPages(
		store.cache, state.root.Float64ScanHead, directoryBounds,
		uint32(options.PageSize),
		func(ref storeio.PageRef) error {
			scanProjectionRefs = append(scanProjectionRefs, ref)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	found := catalogColumn != (storeio.PageRef{})
	if !found || catalogColumn.Kind != storeio.PageFloat64Stripe {
		t.Fatalf("float64 catalog first ref = (%+v,%v), want stripe", catalogColumn, found)
	}
	if groupRef.Length >= uint32(16*options.PageSize) {
		t.Fatalf("group extent = %d, did not beat sixteen independent pages", groupRef.Length)
	}
	sameGroup, ok, err := storeio.LookupChunkTree(store.cache, state.chunkRoot, 1, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok || sameGroup != groupRef {
		t.Fatalf("adjacent chunk did not share group: (%+v,%v,%v)", sameGroup, ok, err)
	}
	buffer := make([]byte, 0, 256)
	buffer, ok, err = store.AppendRaw(buffer[:0], "doc:0003")
	if err != nil || !ok || !bytes.Contains(buffer, []byte(`"score":3`)) {
		t.Fatalf("group point = (%q,%v,%v)", buffer, ok, err)
	}
	pointSnapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(100, func() {
		var found bool
		buffer, found, err = pointSnapshot.AppendRaw(buffer[:0], "doc:0003")
		if err != nil || !found {
			panic("group point failed")
		}
	})
	if err := pointSnapshot.Close(); err != nil {
		t.Fatal(err)
	}
	if allocs != 0 {
		t.Fatalf("group point allocations = %.2f, want zero with caller buffer", allocs)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err := snapshot.ReduceFloat64Path("/score")
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered || total.Count != documents ||
		total.Sum != float64(documents*(documents-1)/2) {
		t.Fatalf("detached score reduction = (%+v,%v,%v)", total, covered, err)
	}

	scanHead := state.root.Float64ScanHead
	deadline := time.Unix(2_000_000_000, 0)
	if updated, deadlineErr := store.SetDeadline("doc:0010", deadline); deadlineErr != nil || !updated {
		t.Fatalf("set deadline over clean stripe = (%v,%v)", updated, deadlineErr)
	}
	if got := store.state.Load().root.Float64ScanHead; got != scanHead {
		t.Fatalf("TTL update changed clean float64 scan head: got %+v, want %+v", got, scanHead)
	}
	if updated, deadlineErr := store.SetDeadline("doc:0010", deadline.Add(time.Hour)); deadlineErr != nil || !updated {
		t.Fatalf("change deadline over clean stripe = (%v,%v)", updated, deadlineErr)
	}
	if got := store.state.Load().root.Float64ScanHead; got != scanHead {
		t.Fatalf("TTL change changed clean float64 scan head: got %+v, want %+v", got, scanHead)
	}
	if updated, persistErr := store.Persist("doc:0010"); persistErr != nil || !updated {
		t.Fatalf("persist deadline over clean stripe = (%v,%v)", updated, persistErr)
	}
	if got := store.state.Load().root.Float64ScanHead; got != scanHead {
		t.Fatalf("TTL removal changed clean float64 scan head: got %+v, want %+v", got, scanHead)
	}

	projectionNeutral := []byte(
		`{"tenant":"changed","status":"metadata-only","score":3,"active":false}`,
	)
	if created, err := store.Put(
		"doc:0003", projectionNeutral,
	); err != nil || created {
		t.Fatalf("projection-neutral update = (%v,%v)", created, err)
	}
	if got := store.state.Load().root.Float64ScanHead; got != scanHead {
		t.Fatalf(
			"projection-neutral update changed scan head: got %+v, want %+v",
			got, scanHead,
		)
	}
	snapshot, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err = snapshot.ReduceFloat64Path("/score")
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered || total.Count != documents ||
		total.Sum != float64(documents*(documents-1)/2) {
		t.Fatalf(
			"projection-neutral score reduction = (%+v,%v,%v)",
			total, covered, err,
		)
	}
	historicalScan, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}

	replacement := []byte(`{"tenant":"new","status":"updated","score":3000,"active":true}`)
	if created, err := store.Put("doc:0003", replacement); err != nil || created {
		t.Fatalf("group update = (%v,%v)", created, err)
	}
	updatedScanHead := store.state.Load().root.Float64ScanHead
	if updatedScanHead == (storeio.PageRef{}) || updatedScanHead == scanHead {
		t.Fatalf(
			"changed projection scan head = %+v, want a new catalog",
			updatedScanHead,
		)
	}
	for _, ref := range scanProjectionRefs {
		retired := false
		for _, extent := range store.retireScratch {
			if extent.Offset <= ref.Offset &&
				uint64(ref.Length) <= extent.Length &&
				ref.Offset-extent.Offset <= extent.Length-uint64(ref.Length) {
				retired = true
				break
			}
		}
		wantRetired := ref == scanHead || ref == catalogColumn
		if retired != wantRetired {
			t.Fatalf(
				"changed projection retirement for %+v = %v, want %v",
				ref, retired, wantRetired,
			)
		}
	}
	for _, extent := range store.retireScratch {
		if extent.Offset <= columnRef.Offset &&
			uint64(columnRef.Length) <= extent.Length &&
			columnRef.Offset-extent.Offset <= extent.Length-uint64(columnRef.Length) {
			t.Fatalf("first mutation retired authoritative float64 sidecar %+v", columnRef)
		}
	}
	if deleted, err := store.Delete("doc:0004"); err != nil || !deleted {
		t.Fatalf("group delete = (%v,%v)", deleted, err)
	}
	state = store.state.Load()
	if state.root.Float64ScanHead == (storeio.PageRef{}) ||
		state.root.Float64ScanHead == updatedScanHead {
		t.Fatalf(
			"delete scan head = %+v, want another live catalog",
			state.root.Float64ScanHead,
		)
	}
	peeled, ok, err := storeio.LookupChunkTree(store.cache, state.chunkRoot, 0, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok || peeled.Kind != storeio.PageDocument || peeled == groupRef {
		t.Fatalf("mutated chunk was not peeled to a private page: (%+v,%v,%v)", peeled, ok, err)
	}
	untouched, ok, err := storeio.LookupChunkTree(store.cache, state.chunkRoot, 1, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	})
	if err != nil || !ok || untouched != groupRef {
		t.Fatalf("untouched chunk lost shared base: (%+v,%v,%v)", untouched, ok, err)
	}
	snapshot, err = store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err = snapshot.ReduceFloat64Path("/score")
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	wantSum := float64(documents*(documents-1)/2 - 3 - 4 + 3000)
	if err != nil || !covered || total.Count != documents-1 || total.Sum != wantSum {
		t.Fatalf("mixed detached/peeled reduction = (%+v,%v,%v), want sum %.0f", total, covered, err, wantSum)
	}
	total, covered, err = historicalScan.ReduceFloat64Path("/score")
	if err != nil || !covered || total.Count != documents ||
		total.Sum != float64(documents*(documents-1)/2) {
		t.Fatalf(
			"historical dense reduction = (%+v,%v,%v)",
			total, covered, err,
		)
	}
	if err := historicalScan.Close(); err != nil {
		t.Fatal(err)
	}
	groupLease, err := store.cache.Acquire(groupRef)
	if err != nil {
		t.Fatal(err)
	}
	groupHeader := storeio.AdmittedDocumentGroup(groupLease.Page()).Header()
	groupLease.Release()
	columnLease, err := store.cache.Acquire(columnRef)
	if err != nil {
		t.Fatal(err)
	}
	columnHeader := storeio.AdmittedFloat64Group(columnLease.Page()).Header()
	columnLease.Release()
	if columnHeader.ChunkCount <= groupHeader.ChunkCount {
		t.Fatalf(
			"typed extent covers %d chunks, want more than one %d-chunk document group",
			columnHeader.ChunkCount, groupHeader.ChunkCount,
		)
	}
	for ordinal := uint16(1); ordinal < groupHeader.ChunkCount; ordinal++ {
		row := int(ordinal) * options.Store.ChunkDocuments
		key := fmt.Sprintf("doc:%04d", row)
		replacement := fmt.Appendf(nil, `{"peeled":%d,"score":%d}`, ordinal, row)
		if created, putErr := store.Put(key, replacement); putErr != nil || created {
			t.Fatalf("peel group chunk %d = (%v,%v)", ordinal, created, putErr)
		}
		retiredGroup := false
		retiredColumns := false
		for _, extent := range store.retireScratch {
			if extent.Offset == groupRef.Offset && extent.Length == uint64(groupRef.Length) {
				retiredGroup = true
			}
			if extent.Offset == columnRef.Offset && extent.Length == uint64(columnRef.Length) {
				retiredColumns = true
			}
		}
		wantGroupRetired := ordinal+1 == groupHeader.ChunkCount
		if retiredGroup != wantGroupRetired || retiredColumns {
			t.Fatalf(
				"group retirement after chunk %d = (documents %v, columns %v), want (%v,false)",
				ordinal, retiredGroup, retiredColumns, wantGroupRetired,
			)
		}
	}
	for chunk := uint32(groupHeader.ChunkCount); chunk < uint32(columnHeader.ChunkCount); chunk++ {
		row := int(chunk) * options.Store.ChunkDocuments
		key := fmt.Sprintf("doc:%04d", row)
		value := fmt.Appendf(nil, `{"peeled":%d,"score":%d}`, chunk, row)
		if created, putErr := store.Put(key, value); putErr != nil || created {
			t.Fatalf("peel typed extent chunk %d = (%v,%v)", chunk, created, putErr)
		}
		retiredColumns := false
		for _, extent := range store.retireScratch {
			if extent.Offset == columnRef.Offset && extent.Length == uint64(columnRef.Length) {
				retiredColumns = true
			}
		}
		wantRetired := chunk+1 == uint32(columnHeader.ChunkCount)
		if retiredColumns != wantRetired {
			t.Fatalf(
				"typed extent retirement after chunk %d = %v, want %v",
				chunk, retiredColumns, wantRetired,
			)
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
	got, ok, err := reopened.AppendRaw(buffer[:0], "doc:0003")
	if err != nil || !ok || !bytes.Equal(got, replacement) {
		t.Fatalf("reopened update = (%q,%v,%v)", got, ok, err)
	}
	if _, ok, err := reopened.AppendRaw(buffer[:0], "doc:0004"); err != nil || ok {
		t.Fatalf("reopened delete = (%v,%v)", ok, err)
	}
	got, ok, err = reopened.AppendRaw(buffer[:0], "doc:0010")
	if err != nil || !ok || !bytes.Contains(got, []byte(`"score":10`)) {
		t.Fatalf("reopened untouched group = (%q,%v,%v)", got, ok, err)
	}
	recoveredSnapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err = recoveredSnapshot.ReduceFloat64Path("/score")
	if closeErr := recoveredSnapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered || total.Count != documents-1 ||
		total.Sum != wantSum {
		t.Fatalf(
			"reopened dense reduction = (%+v,%v,%v), want sum %.0f",
			total, covered, err, wantSum,
		)
	}
	if created, err := reopened.Put(
		"doc:insert-after-stripe-cow",
		[]byte(`{"tenant":"new","status":"inserted","score":7,"active":true}`),
	); err != nil || !created {
		t.Fatalf(
			"insert after mutable stripe catalog = (%v,%v)",
			created, err,
		)
	}
	if got := reopened.state.Load().root.Float64ScanHead; got != (storeio.PageRef{}) {
		t.Fatalf("insert retained non-extensible scan catalog %+v", got)
	}
}

func TestWriteFileStoreBulkCatalogedFloat64ScanExact(t *testing.T) {
	const documents = 20000
	options := testFileStoreOptions()
	options.Store = StoreOptions{ChunkDocuments: 8, ShapeTapes: true}
	options.MaxPageSize = 64 << 10
	options.ResidentBytes = 8 << 20
	options.Float64Columns = []string{"/score"}
	builder, err := NewStoreBuilder(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	for row := range documents {
		document := fmt.Appendf(nil, `{"score":%d,"payload":"xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}`, row)
		if err := builder.Append(fmt.Sprintf("doc:%04d", row), document); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "file-store-linked-columns-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	state := store.state.Load()
	coveredChunks := uint32(0)
	extents := 0
	err = storeio.WalkFloat64Directory(
		store.cache, state.root.Float64ScanHead,
		storeio.Float64DirectoryBounds{
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
		uint32(options.PageSize),
		func(entry storeio.Float64DirectoryEntry) error {
			groupLease, acquireErr := store.cache.Acquire(entry.Ref)
			if acquireErr != nil {
				return acquireErr
			}
			header := storeio.AdmittedFloat64Stripe(groupLease.Page()).Header()
			groupLease.Release()
			if entry.FirstChunk != coveredChunks ||
				header.FirstChunk != coveredChunks {
				return fmt.Errorf(
					"linked extent %d starts at (%d,%d), want %d",
					extents, entry.FirstChunk, header.FirstChunk,
					coveredChunks,
				)
			}
			coveredChunks += uint32(header.ChunkCount)
			extents++
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if extents < 1 || coveredChunks != state.root.ChunkHighWater {
		t.Fatalf(
			"catalog typed coverage = (%d extents, %d chunks), high water %d",
			extents, coveredChunks, state.root.ChunkHighWater,
		)
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err := snapshot.ReduceFloat64Path("/score")
	allocs := testing.AllocsPerRun(100, func() {
		total, covered, err = snapshot.ReduceFloat64Path("/score")
		if err != nil || !covered {
			panic("linked score reduction")
		}
	})
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered || total.Count != documents ||
		total.Sum != float64(documents*(documents-1)/2) {
		t.Fatalf("linked score reduction = (%+v,%v,%v)", total, covered, err)
	}
	if allocs != 0 {
		t.Fatalf("linked score warm allocations = %.2f, want zero", allocs)
	}
}

func TestWriteFileStoreBulkFloat64DirectoryMultiLevelChurn(t *testing.T) {
	const (
		documents = 4095
		columns   = 128
	)
	options := testFileStoreOptions()
	options.Store = StoreOptions{
		ChunkDocuments: 2,
		ShapeTapes:     true,
	}
	options.MaxPageSize = 8192
	options.InlineValueBytes = 2048
	options.MaxDocumentBytes = 8192
	options.ResidentBytes = 16 << 20
	options.BufferCount = 128
	options.Float64Columns = make([]string, columns)
	for column := range columns {
		options.Float64Columns[column] = fmt.Sprintf("/c%02d", column)
	}
	document := func(row, first int) []byte {
		dst := make([]byte, 0, 1024)
		dst = append(dst, '{')
		for column := range columns {
			if column != 0 {
				dst = append(dst, ',')
			}
			value := (row + column) & 255
			if column == 0 {
				value = first
			}
			dst = fmt.Appendf(dst, `"c%02d":%d`, column, value)
		}
		return append(dst, '}')
	}
	builder, err := NewStoreBuilder(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	var initialSum float64
	for row := range documents {
		value := row & 255
		initialSum += float64(value)
		if err := builder.Append(
			fmt.Sprintf("doc:%04d", row), document(row, value),
		); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(
		t.TempDir(), "file-store-float64-directory-*",
	)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	state := store.state.Load()
	oldRoot := state.root.Float64ScanHead
	rootLease, err := store.cache.Acquire(oldRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootLevel := storeio.AdmittedFloat64Directory(
		rootLease.Page(),
	).Header().Level
	rootLease.Release()
	if rootLevel == 0 {
		t.Fatal("forced float64 directory remained a single leaf")
	}
	bounds := storeio.Float64DirectoryBounds{
		FileEnd:       state.super.FileEnd,
		NextLogicalID: state.root.NextLogicalID,
	}
	var entries []storeio.Float64DirectoryEntry
	if err := storeio.WalkFloat64Directory(
		store.cache, oldRoot, bounds, uint32(options.PageSize),
		func(entry storeio.Float64DirectoryEntry) error {
			entries = append(entries, entry)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	if len(entries) <= storeio.Float64DirectoryFanout {
		t.Fatalf(
			"float64 stripe count = %d, want more than %d",
			len(entries), storeio.Float64DirectoryFanout,
		)
	}
	targetEntry := entries[storeio.Float64DirectoryFanout+3]
	target := int(targetEntry.FirstChunk) *
		options.Store.ChunkDocuments
	if target <= 0 || target >= documents {
		t.Fatalf("target chunk = %d", target)
	}
	untouched := entries[0].Ref
	var oldDirectoryPages []storeio.PageRef
	if err := storeio.WalkFloat64DirectoryPages(
		store.cache, oldRoot, bounds, uint32(options.PageSize),
		func(ref storeio.PageRef) error {
			oldDirectoryPages = append(oldDirectoryPages, ref)
			return nil
		},
	); err != nil {
		t.Fatal(err)
	}
	historical, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	original := target & 255
	if created, err := store.Put(
		fmt.Sprintf("doc:%04d", target), document(target, 3000),
	); err != nil || created {
		t.Fatalf("multi-level projection update = (%v,%v)", created, err)
	}
	updatedState := store.state.Load()
	if updatedState.root.Float64ScanHead == (storeio.PageRef{}) ||
		updatedState.root.Float64ScanHead == oldRoot {
		t.Fatalf(
			"multi-level update root = %+v, old %+v",
			updatedState.root.Float64ScanHead, oldRoot,
		)
	}
	updatedEntry, found, err := storeio.LookupFloat64Directory(
		store.cache, updatedState.root.Float64ScanHead,
		uint32(target/options.Store.ChunkDocuments),
		storeio.Float64DirectoryBounds{
			FileEnd:       updatedState.super.FileEnd,
			NextLogicalID: updatedState.root.NextLogicalID,
		},
		uint32(options.PageSize),
	)
	if err != nil || !found || updatedEntry.Ref == targetEntry.Ref {
		t.Fatalf(
			"updated stripe = (%+v,%v,%v), old %+v",
			updatedEntry, found, err, targetEntry.Ref,
		)
	}
	untouchedEntry, found, err := storeio.LookupFloat64Directory(
		store.cache, updatedState.root.Float64ScanHead, 0,
		storeio.Float64DirectoryBounds{
			FileEnd:       updatedState.super.FileEnd,
			NextLogicalID: updatedState.root.NextLogicalID,
		},
		uint32(options.PageSize),
	)
	if err != nil || !found || untouchedEntry.Ref != untouched {
		t.Fatalf(
			"untouched stripe = (%+v,%v,%v), want %+v",
			untouchedEntry, found, err, untouched,
		)
	}
	retired := func(ref storeio.PageRef) bool {
		for _, extent := range store.retireScratch {
			if extent.Offset <= ref.Offset &&
				uint64(ref.Length) <= extent.Length &&
				ref.Offset-extent.Offset <=
					extent.Length-uint64(ref.Length) {
				return true
			}
		}
		return false
	}
	if !retired(targetEntry.Ref) || retired(untouched) {
		t.Fatalf(
			"stripe retirement = (target %v, untouched %v)",
			retired(targetEntry.Ref), retired(untouched),
		)
	}
	retiredDirectoryPages := 0
	for _, ref := range oldDirectoryPages {
		if retired(ref) {
			retiredDirectoryPages++
		}
	}
	if retiredDirectoryPages != int(rootLevel)+1 {
		t.Fatalf(
			"retired directory pages = %d, want path length %d",
			retiredDirectoryPages, int(rootLevel)+1,
		)
	}
	if deleted, err := store.Delete(
		fmt.Sprintf("doc:%04d", target),
	); err != nil || !deleted {
		t.Fatalf("multi-level projection delete = (%v,%v)", deleted, err)
	}
	if store.state.Load().root.Float64ScanHead == (storeio.PageRef{}) {
		t.Fatal("in-range delete dropped float64 directory")
	}
	snapshot, err := store.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err := snapshot.ReduceFloat64Path("/c00")
	if err != nil || !covered {
		t.Fatalf(
			"multi-level reduction preflight = (%+v,%v,%v)",
			total, covered, err,
		)
	}
	allocs := testing.AllocsPerRun(100, func() {
		total, covered, err = snapshot.ReduceFloat64Path("/c00")
		if err != nil || !covered {
			panic("multi-level float64 reduction")
		}
	})
	if closeErr := snapshot.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	wantSum := initialSum - float64(original)
	if err != nil || !covered || total.Count != documents-1 ||
		total.Sum != wantSum {
		t.Fatalf(
			"multi-level reduction = (%+v,%v,%v), want sum %.0f",
			total, covered, err, wantSum,
		)
	}
	if allocs != 0 {
		t.Fatalf(
			"multi-level reduction allocations = %.2f, want zero",
			allocs,
		)
	}
	oldTotal, oldCovered, oldErr := historical.ReduceFloat64Path("/c00")
	if closeErr := historical.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if oldErr != nil || !oldCovered || oldTotal.Count != documents ||
		oldTotal.Sum != initialSum {
		t.Fatalf(
			"historical multi-level reduction = (%+v,%v,%v)",
			oldTotal, oldCovered, oldErr,
		)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	recovered, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	total, covered, err = recovered.ReduceFloat64Path("/c00")
	if closeErr := recovered.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	if err != nil || !covered || total.Count != documents-1 ||
		total.Sum != wantSum {
		t.Fatalf(
			"recovered multi-level reduction = (%+v,%v,%v)",
			total, covered, err,
		)
	}
}

func TestWriteFileStoreBulkGroupRejectsResealedInvalidJSON(t *testing.T) {
	const documents = 128
	options := testFileStoreOptions()
	options.Store = StoreOptions{ChunkDocuments: 8, ShapeTapes: true}
	options.MaxPageSize = 64 << 10
	options.ResidentBytes = 8 << 20
	builder, err := NewStoreBuilder(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	for row := range documents {
		document := fmt.Appendf(
			nil, `{"id":%d,"payload":"%s"}`, row, strings.Repeat("x", 96+(row&7)),
		)
		if err := builder.Append(fmt.Sprintf("doc:%04d", row), document); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "file-store-group-corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}

	lastChunk := uint32(documents/options.Store.ChunkDocuments - 1)
	ref := recoveredFileDocumentRef(t, file, options, lastChunk)
	if ref.Kind != storeio.PageDocumentGroup {
		t.Fatalf("last chunk ref = %+v, want document group", ref)
	}
	page := make([]byte, ref.Length)
	if _, err := file.ReadAt(page, int64(ref.Offset)); err != nil {
		t.Fatal(err)
	}
	payload := page[storeio.PageHeaderSize:]
	templateCount := int(binary.LittleEndian.Uint16(payload[12:14]))
	templateStart := storeio.PageHeaderSize + storeio.DocumentGroupPayloadHeaderSize
	for offset := 16; offset < 32; offset += 4 {
		templateStart += int(binary.LittleEndian.Uint32(payload[offset : offset+4]))
	}
	entry := templateStart + templateCount*4
	values := int(binary.LittleEndian.Uint16(page[entry : entry+2]))
	staticStart := entry + 8 + (values+1)*4
	if staticStart >= len(page) {
		t.Fatalf("first template static byte offset %d is outside the page", staticStart)
	}
	if page[staticStart] != '{' {
		t.Fatalf("first template static byte at %d = %q", staticStart, page[staticStart])
	}
	page[staticStart] = 'x'
	if _, err := storeio.SealPage(page); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(page, int64(ref.Offset)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenFileStore(file, options)
	if reopened != nil {
		_ = reopened.Close()
		t.Fatal("OpenFileStore returned a store for invalid grouped JSON")
	}
	if !errors.Is(err, storeio.ErrDocumentGroupCorrupt) {
		t.Fatalf("OpenFileStore invalid grouped JSON = %v, want document-group corruption", err)
	}
}

func TestWriteFileStoreBulkGroupRejectsResealedDetachedColumnCorruption(t *testing.T) {
	const documents = 128
	options := testFileStoreOptions()
	options.Store = StoreOptions{ChunkDocuments: 8, ShapeTapes: true}
	options.MaxPageSize = 64 << 10
	options.ResidentBytes = 8 << 20
	options.Float64Columns = []string{"/score"}
	builder, err := NewStoreBuilder(options.Store)
	if err != nil {
		t.Fatal(err)
	}
	for row := range documents {
		document := fmt.Appendf(
			nil, `{"score":%d,"payload":"%s"}`, row, strings.Repeat("x", 96+(row&7)),
		)
		if err := builder.Append(fmt.Sprintf("doc:%04d", row), document); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.CreateTemp(t.TempDir(), "file-store-group-columns-corrupt-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}

	group := recoveredFileDocumentRef(t, file, options, 0)
	columns, detached, err := storeio.DocumentGroupFloat64Sidecar(
		group, uint32(options.PageSize),
	)
	if err != nil || !detached {
		t.Fatalf("detached columns = (%+v,%v,%v)", columns, detached, err)
	}
	page := make([]byte, columns.Length)
	if _, err := file.ReadAt(page, int64(columns.Offset)); err != nil {
		t.Fatal(err)
	}
	payload := page[storeio.PageHeaderSize:]
	chunkBytes := int(binary.LittleEndian.Uint32(payload[16:20]))
	directoryBytes := int(binary.LittleEndian.Uint32(payload[20:24]))
	firstMask := storeio.PageHeaderSize + storeio.Float64GroupPayloadHeaderSize +
		chunkBytes + directoryBytes
	page[firstMask+1] |= 0x01
	if _, err := storeio.SealPage(page); err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteAt(page, int64(columns.Offset)); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	snapshot, err := reopened.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer snapshot.Close()
	aggregate, covered, err := snapshot.ReduceFloat64Path("/score")
	if err != nil || !covered || aggregate.Count != documents {
		t.Fatalf("independent clean stripe = (%+v, covered %v, %v)", aggregate, covered, err)
	}
	if _, err := reopened.Put("doc:0000", []byte(`{"score":9}`)); !errors.Is(
		err, storeio.ErrFloat64GroupCorrupt,
	) {
		t.Fatalf("mutation over corrupt authoritative sidecar = %v", err)
	}
}

func TestWriteFileStoreBulkEmptyAndNonEmptyGuard(t *testing.T) {
	source := NewStore(StoreOptions{})
	options := testFileStoreOptions()
	file, err := os.CreateTemp(t.TempDir(), "file-store-bulk-empty-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if store.Stats().DocumentCount != 0 {
		t.Fatal("empty bulk source opened non-empty")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := source.WriteFileStore(file, options); err != ErrFileStoreNotEmpty {
		t.Fatalf("non-empty guard = %v, want %v", err, ErrFileStoreNotEmpty)
	}
}

func TestWriteFileStoreBulkKeepsPackedIndexBaseLiveThroughChurn(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 4, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	for i := range 8 {
		if err := builder.Append(fmt.Sprintf("k%d", i), []byte(fmt.Sprintf(
			`{"meta":{"status":%q},"n":%d}`, []string{"idle", "active"}[i&1], i,
		))); err != nil {
			t.Fatal(err)
		}
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.BufferCount = 128
	options.MaxRetiredExtents = 512
	options.Indexes = []StoreIndexDefinition{{Name: "status", Paths: []string{"/meta/status"}}}
	file, err := os.CreateTemp(t.TempDir(), "file-store-bulk-index-churn-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	idleDocument := []byte(`{"meta":{"status":"idle"}}`)
	needed, err := RequiredIndexEntries(idleDocument)
	if err != nil {
		t.Fatal(err)
	}
	idle, err := BuildIndex(idleDocument, make([]IndexEntry, needed))
	if err != nil {
		t.Fatal(err)
	}
	idleHash, ok, err := fileIndexTupleHash(store.options.indexes[0], idle)
	if err != nil || !ok {
		t.Fatalf("idle index hash = (%x,%v,%v)", idleHash, ok, err)
	}
	state := store.state.Load()
	base, found, err := storeio.LookupIndexTree(store.cache, state.indexRoot, storeio.IndexDirectoryKey{
		IndexID: 0, TupleHash: idleHash, Chunk: 0,
	}, storeio.IndexTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
		IndexHighWater: uint32(len(store.options.indexes)),
	})
	if err != nil || !found || base.Flags&storeio.IndexPostingImmutableBase == 0 {
		t.Fatalf("packed base posting = (%+v,%v,%v)", base, found, err)
	}

	put := func(version int) {
		t.Helper()
		status := []string{"idle", "active"}[version&1]
		if created, err := store.Put("k0", []byte(fmt.Sprintf(
			`{"meta":{"status":%q},"n":%d}`, status, version,
		))); err != nil || created {
			t.Fatalf("indexed churn %d = (%v,%v)", version, created, err)
		}
	}
	for version := 1; version <= 24; version++ {
		put(version)
	}
	for version := 25; version <= 64; version++ {
		put(version)
	}
	for _, extent := range store.reusable {
		if extent.Offset < base.Page.Offset+uint64(base.Page.Length) &&
			base.Page.Offset < extent.Offset+extent.Length {
			t.Fatalf("live packed base %+v entered reusable extent %+v", base.Page, extent)
		}
	}
	valueNeeded, err := RequiredIndexEntries([]byte(`"idle"`))
	if err != nil {
		t.Fatal(err)
	}
	idleValue, err := BuildIndex([]byte(`"idle"`), make([]IndexEntry, valueNeeded))
	if err != nil {
		t.Fatal(err)
	}
	masks, err := store.AppendIndexMasks(nil, "status", idleValue)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, mask := range masks {
		count += bits.OnesCount64(mask.Bits)
	}
	if count != 4 {
		t.Fatalf("idle rows after indexed churn = %d, want 4", count)
	}
}

func TestWriteFileStoreBulkRecoveryFallsBackToBaseGeneration(t *testing.T) {
	builder, err := NewStoreBuilder(StoreOptions{ChunkDocuments: 4, ShapeTapes: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := builder.Append("stable", []byte(`{"version":1,"meta":{"status":"active"}}`)); err != nil {
		t.Fatal(err)
	}
	source, err := builder.Build()
	if err != nil {
		t.Fatal(err)
	}
	options := testFileStoreOptions()
	options.BufferCount = 128
	options.Indexes = []StoreIndexDefinition{{Name: "status", Paths: []string{"/meta/status"}}}
	file, err := os.CreateTemp(t.TempDir(), "file-store-bulk-recovery-*")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if _, err := source.WriteFileStore(file, options); err != nil {
		t.Fatal(err)
	}
	store, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	if created, err := store.Put("stable", []byte(`{"version":2,"meta":{"status":"idle"}}`)); err != nil || created {
		t.Fatalf("update = (%v,%v)", created, err)
	}
	newest := store.state.Load()
	if newest.root.Generation != 2 {
		t.Fatalf("newest generation = %d, want 2", newest.root.Generation)
	}
	newestStateOffset := newest.stateRef.Offset
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var one [1]byte
	if _, err := file.ReadAt(one[:], int64(newestStateOffset)+storeio.PageHeaderSize); err != nil {
		t.Fatal(err)
	}
	one[0] ^= 0xff
	if _, err := file.WriteAt(one[:], int64(newestStateOffset)+storeio.PageHeaderSize); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenFileStore(file, options)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if reopened.Generation() != 1 {
		t.Fatalf("recovered generation = %d, want compact base generation 1", reopened.Generation())
	}
	raw, ok, err := reopened.AppendRaw(nil, "stable")
	if err != nil || !ok || string(raw) != `{"version":1,"meta":{"status":"active"}}` {
		t.Fatalf("recovered bulk value = (%q,%v,%v)", raw, ok, err)
	}
}
