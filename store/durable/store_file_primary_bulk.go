package durable

import (
	"bytes"
	"crypto/rand"
	"fmt"
	"os"
	"slices"

	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/store"
)

// CreateFromPrimary is the experimental phase-4 cutover bulk entry. It writes
// one immutable ordered primary graph and publishes it through
// StateRoot.PrimaryRoot. Legacy chunk, fingerprint, index, float64, and zone
// roots remain empty, so the resulting collection supports point reads and
// snapshots but deliberately rejects mutation until the primary COW path is
// wired.
//
// Indexes, float64 columns, schemas, compact document groups, overflow values,
// and scan/zone construction are not implemented by this entry. Use
// CreateFrom when those features are required.
func CreateFromPrimary(
	collection *store.Collection,
	file *os.File,
	options Options,
) (int64, error) {
	if collection == nil || file == nil {
		return 0, fmt.Errorf(
			"vibejson: CreateFromPrimary requires non-nil collection and file",
		)
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() != 0 {
		return 0, ErrNotEmpty
	}
	if len(options.Indexes) != 0 {
		return 0, fmt.Errorf(
			"%w: indexes are not available in CreateFromPrimary",
			ErrPrimaryCutoverUnsupported,
		)
	}
	if len(options.Float64Columns) != 0 {
		return 0, fmt.Errorf(
			"%w: float64 columns are not available in CreateFromPrimary",
			ErrPrimaryCutoverUnsupported,
		)
	}
	if options.Collection.Schema != nil {
		return 0, fmt.Errorf(
			"%w: schemas are not available in CreateFromPrimary",
			ErrPrimaryCutoverUnsupported,
		)
	}
	if options.DocumentFormat != DocumentFormatVerbatim {
		return 0, fmt.Errorf(
			"%w: compact documents are not available in CreateFromPrimary",
			ErrPrimaryCutoverUnsupported,
		)
	}
	requestedBuffers := options.BufferCount
	normalized, err := options.normalized()
	if err != nil {
		return 0, err
	}
	if normalized.PageSize != 4096 ||
		normalized.MaxPageSize < storeio.GlobalTabletCatalogRootBytes {
		return 0, fmt.Errorf(
			"%w: CreateFromPrimary requires 4 KiB pages and a 64 KiB maximum page",
			ErrPrimaryCutoverUnsupported,
		)
	}

	var (
		source  *store.State
		rows    []fileStoreBulkRow
		records []storeio.PrimaryGraphRecord
	)
	err = collection.WithBulkSnapshot(func(snapshot *store.State) error {
		source = snapshot
		var collectErr error
		rows, collectErr = collectFileStoreBulkRows(snapshot, normalized)
		return collectErr
	})
	if err != nil {
		return 0, err
	}
	if len(rows) == 0 {
		return 0, fmt.Errorf(
			"%w: CreateFromPrimary requires at least one document",
			ErrPrimaryCutoverUnsupported,
		)
	}
	records = make([]storeio.PrimaryGraphRecord, len(rows))
	for at, row := range rows {
		chunk := source.Chunks.Get(row.sourceChunk)
		if chunk == nil ||
			chunk.Live&(uint64(1)<<row.sourceSlot) == 0 {
			return 0, storeio.ErrDocumentPageCorrupt
		}
		key := chunk.Key(int(row.sourceSlot))
		value := chunk.Docs.RawAt(int(chunk.Ord[row.sourceSlot]))
		if len(key) > storeio.CommonPrimaryLeafMaxKeyBytes {
			return 0, fmt.Errorf(
				"%w: CreateFromPrimary key exceeds the ordered-leaf bound",
				ErrKeyTooLarge,
			)
		}
		if len(value) == 0 || len(value) > normalized.InlineValueBytes {
			return 0, fmt.Errorf(
				"%w: CreateFromPrimary requires non-empty inline documents",
				ErrPrimaryCutoverUnsupported,
			)
		}
		records[at] = storeio.PrimaryGraphRecord{
			Key: []byte(key), Value: bytes.Clone(value),
		}
	}
	slices.SortFunc(records, func(a, b storeio.PrimaryGraphRecord) int {
		return bytes.Compare(a.Key, b.Key)
	})

	var storeID [16]byte
	if _, err := rand.Read(storeID[:]); err != nil {
		return 0, fmt.Errorf("vibejson: create primary collection identity: %w", err)
	}
	pageCount, err := storeio.PrimaryGraphPageCount(storeID, records)
	if err != nil {
		return 0, err
	}
	bufferCount := pageCount + 1
	if requestedBuffers != 0 {
		if requestedBuffers <= pageCount {
			return 0, fmt.Errorf(
				"%w: CreateFromPrimary needs at least %d commit buffers",
				ErrPrimaryCutoverUnsupported, pageCount+1,
			)
		}
		bufferCount = requestedBuffers
	}
	if bufferCount > 1<<15 {
		return 0, fmt.Errorf(
			"%w: ordered primary graph needs %d transaction pages",
			ErrPrimaryCutoverUnsupported, pageCount,
		)
	}

	layout, err := storeio.MutableStoreLayout(uint32(normalized.PageSize))
	if err != nil {
		return 0, err
	}
	catalog, err := planFilePageCatalog(
		normalized.pageCatalog, storeID, 1,
		uint32(normalized.PageSize), layout.DataStart,
		storeio.StateRootLogicalID+1,
	)
	if err != nil {
		return 0, err
	}
	if err := file.Truncate(int64(catalog.fileEnd)); err != nil {
		return 0, err
	}
	if catalog.segments != 0 {
		scratch := make([]byte, normalized.PageSize)
		if err := catalog.write(
			file, catalog.fileEnd, catalog.nextID,
			scratch,
		); err != nil {
			return 0, err
		}
		if err := file.Sync(); err != nil {
			return 0, err
		}
	}

	committer, err := storeio.NewCommitter(
		file,
		storeio.DeviceOptions{
			Backend:     storeio.Backend(normalized.Backend),
			BufferCount: bufferCount,
			BufferSize:  max(normalized.MaxPageSize, os.Getpagesize()),
			QueueDepth:  bufferCount,
			CheckpointSync: storeio.CheckpointSync(
				normalized.CheckpointStrength,
			),
		},
		storeio.CommitterOptions{
			QueueSlots: 1, MaxPagesPerBatch: pageCount, GroupLimit: 1,
		},
	)
	if err != nil {
		return 0, err
	}
	tx, err := storeio.BeginWriteTransaction(
		committer, nil, pageCount,
		storeio.WriteTransactionOptions{
			StoreID: storeID, Generation: 1,
			PageSize:      uint32(normalized.PageSize),
			FileEnd:       catalog.fileEnd,
			NextLogicalID: storeio.PrimaryFirstDynamicLogicalID,
		},
	)
	if err != nil {
		_ = committer.Close()
		return 0, err
	}
	primaryRoot, err := storeio.BuildPrimaryGraph(tx, records)
	if err != nil {
		_ = tx.Abort()
		_ = committer.Close()
		return 0, err
	}
	root := storeio.StateRoot{
		StoreID: storeID, Generation: 1,
		PageSize:       uint32(normalized.PageSize),
		DocumentCount:  uint64(len(records)),
		NextLogicalID:  tx.NextLogicalID(),
		ChunkDocuments: uint32(normalized.Collection.ChunkDocuments),
		IndexMaxDepth: uint32(max(
			normalized.Collection.IndexOptions.MaxDepth, 0,
		)),
		MaxKeyBytes:      uint32(normalized.MaxKeyBytes),
		InlineValueBytes: uint32(normalized.InlineValueBytes),
		MaxDocumentBytes: uint32(normalized.MaxDocumentBytes),
		PrimaryRoot:      primaryRoot,
	}
	root.Options = fileStoreCollectionOptionFlags(normalized.Collection)
	if err := catalog.apply(
		&root, uint32(normalized.MaxPageSize),
	); err != nil {
		_ = tx.Abort()
		_ = committer.Close()
		return 0, err
	}
	if err := tx.PublishInline(
		root,
		storeio.NewInlineFreeDelta(storeio.PageRef{}, storeio.PageRef{}),
	); err != nil {
		_ = tx.Abort()
		_ = committer.Close()
		return 0, err
	}
	fileEnd := int64(tx.FileEnd())
	if err := committer.Wait(root.Generation); err != nil {
		_ = committer.Close()
		return 0, err
	}
	if err := committer.Close(); err != nil {
		return 0, err
	}
	return fileEnd, nil
}
