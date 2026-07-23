package simdjson

import (
	"fmt"
	"math/bits"
	"sync"
	"sync/atomic"

	"github.com/thesyncim/simdjson/internal/storeio"
)

// fileStorePageValidator carries monotonic publication bounds into PageCache's
// one-time admission check. Document pages written by this process were
// already validated by their encoder; pages read back from storage receive the
// same typed validation before any zero-copy admitted view can observe them.
type fileStorePageValidator struct {
	fileEnd        atomic.Uint64
	nextLogicalID  atomic.Uint64
	chunkHighWater atomic.Uint32
	pageSize       uint32
	indexHighWater uint32
	chunkDocuments uint32
	groupScratch   sync.Pool
}

func newFileStorePageValidator(pageSize, indexHighWater, chunkDocuments uint32) *fileStorePageValidator {
	v := &fileStorePageValidator{
		pageSize: pageSize, indexHighWater: indexHighWater,
		chunkDocuments: chunkDocuments,
	}
	v.groupScratch.New = func() any {
		scratch := make([]byte, 0, pageSize)
		return &scratch
	}
	return v
}

func (v *fileStorePageValidator) update(state *fileStoreState) {
	if v == nil || state == nil {
		return
	}
	// These bounds never decrease. Publish them before the corresponding state
	// pointer so a reader of that state cannot validate against older bounds.
	v.fileEnd.Store(state.super.FileEnd)
	v.nextLogicalID.Store(state.root.NextLogicalID)
	v.chunkHighWater.Store(state.root.ChunkHighWater)
}

func (v *fileStorePageValidator) validate(page []byte, ref storeio.PageRef) error {
	if v == nil {
		return nil
	}
	switch ref.Kind {
	case storeio.PageDocument:
		view, err := storeio.OpenAdmittedDocumentPageWithOverflow(
			page, v.chunkHighWater.Load(), v.nextLogicalID.Load(),
			v.fileEnd.Load(), v.pageSize,
		)
		if err != nil {
			return err
		}
		for live := view.Header().Live; live != 0; live &= live - 1 {
			value, ok := view.LookupValue(uint8(bits.TrailingZeros64(live)))
			if !ok {
				return storeio.ErrDocumentPageCorrupt
			}
			if value.Overflow == (storeio.PageRef{}) && !Valid(value.Inline) {
				return fmt.Errorf("%w: invalid inline JSON", storeio.ErrDocumentPageCorrupt)
			}
		}
	case storeio.PageDocumentGroup:
		group, err := storeio.OpenAdmittedDocumentGroup(
			page, v.chunkHighWater.Load(), v.nextLogicalID.Load(),
		)
		if err != nil {
			return err
		}
		pooled := v.groupScratch.Get().(*[]byte)
		scratch := (*pooled)[:0]
		defer func() {
			clear(scratch)
			*pooled = scratch[:0]
			v.groupScratch.Put(pooled)
		}()
		header := group.Header()
		for ordinal := uint32(0); ordinal < uint32(header.ChunkCount); ordinal++ {
			chunk, ok := group.Chunk(header.FirstChunk + ordinal)
			if !ok {
				return storeio.ErrDocumentGroupCorrupt
			}
			for rank := 0; rank < chunk.Len(); rank++ {
				record, ok := chunk.RecordAt(rank)
				if !ok {
					return storeio.ErrDocumentGroupCorrupt
				}
				scratch = scratch[:0]
				scratch, ok = chunk.AppendJSON(scratch, record.Slot)
				if !ok || !Valid(scratch) {
					return fmt.Errorf("%w: invalid decoded JSON", storeio.ErrDocumentGroupCorrupt)
				}
			}
		}
	case storeio.PageFloat64Group:
		_, err := storeio.OpenAdmittedFloat64Group(
			page, v.chunkHighWater.Load(), v.nextLogicalID.Load(),
		)
		return err
	case storeio.PageFloat64Catalog:
		_, err := storeio.OpenAdmittedFloat64Directory(
			page, v.fileEnd.Load(), v.nextLogicalID.Load(), v.pageSize,
		)
		return err
	case storeio.PageFloat64Stripe:
		_, err := storeio.OpenAdmittedFloat64Stripe(
			page, v.chunkHighWater.Load(), v.nextLogicalID.Load(),
		)
		return err
	case storeio.PageIndexGroupCatalog:
		catalog, err := storeio.OpenAdmittedIndexGroupCatalog(
			page, v.indexHighWater, v.chunkHighWater.Load(), v.chunkDocuments,
			v.fileEnd.Load(), v.nextLogicalID.Load(), v.pageSize,
		)
		if err != nil {
			return err
		}
		iterator := catalog.Iterator()
		for {
			entry, ok := iterator.Next()
			if !ok {
				break
			}
			if !fileIndexCertificateValid(entry.Value, 1) {
				return storeio.ErrIndexGroupCatalogCorrupt
			}
		}
		return nil
	}
	return nil
}
