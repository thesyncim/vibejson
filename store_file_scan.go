package simdjson

import (
	"math/bits"
	"slices"

	"github.com/thesyncim/simdjson/internal/storeio"
)

const fileScanReadAheadLimit = 64

type fileScanPage struct {
	ref    storeio.PageRef
	mask   uint64
	chunk  uint32
	chunks uint32
}

// RangeRaw visits live rows in ascending chunk/slot order. key and value are
// borrowed only for the callback; overflow values reuse one bounded buffer.
// Returning an error stops the scan immediately.
func (s *FileSnapshot) RangeRaw(fn func(key, value []byte) error) error {
	_, err := s.RangeRawBuffer(nil, fn)
	return err
}

// RangeRawBuffer is RangeRaw with caller-owned overflow storage. The returned
// slice preserves any grown capacity for the next scan. Inline-only scans and
// warmed overflow scans allocate nothing when scratch has sufficient capacity.
//
// This method issues document reads serially. Use RangeRawReadAheadBuffer for
// a cold scan whose corpus exceeds the resident page budget.
func (s *FileSnapshot) RangeRawBuffer(scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.store == nil || s.state == nil {
		return scratch, ErrFileStoreClosed
	}
	if fn == nil {
		return scratch, nil
	}
	state := s.state
	err := storeio.WalkChunkTreeRuns(s.store.cache, state.chunkRoot, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	}, func(chunk, chunks uint32, ref storeio.PageRef) error {
		return s.rangeFileDocumentRun(state, chunk, chunks, ref, ^uint64(0), &scratch, fn)
	})
	return scratch, err
}

// RangeRawReadAheadBuffer is the bounded cold-scan form of RangeRawBuffer.
// It discovers a small chunk-ordered window, submits its document extents in
// physical order, and still invokes fn in exact chunk/slot order. The window
// is capped by one half of ResidentBytes, PrefetchQueue, 64 extents, and either
// ReadQueueDepth for io_uring or four requests per portable worker. Queue
// pressure merely shortens read-ahead; demand reads remain authoritative and
// return every validation or I/O error.
//
// Read-ahead is speculative: if fn stops early, at most one bounded window may
// already have been submitted. The method retains no page lease across fn and
// allocates nothing after caller overflow capacity is warm.
func (s *FileSnapshot) RangeRawReadAheadBuffer(scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.store == nil || s.state == nil {
		return scratch, ErrFileStoreClosed
	}
	if fn == nil {
		return scratch, nil
	}
	// Buffered files already receive kernel readahead, and feeding resident
	// hits through the user-space queue costs more than a direct scan. Explicit
	// read-ahead is for O_DIRECT, where each miss otherwise blocks the walker.
	readBackend := s.store.cache.ReadBackend()
	if !s.store.directRead ||
		(readBackend != storeio.BackendIOUring && s.store.options.ReadConcurrency == 1) {
		return s.RangeRawBuffer(scratch, fn)
	}
	state := s.state
	var pages [fileScanReadAheadLimit]fileScanPage
	count := 0
	bytes := uint64(0)
	pageLimit, byteLimit := s.fileScanReadAheadWindow()
	flush := func() error {
		if count == 0 {
			return nil
		}
		err := s.rangeFileReadAheadBatch(state, pages[:count], &scratch, fn)
		count = 0
		bytes = 0
		return err
	}
	err := storeio.WalkChunkTreeRuns(s.store.cache, state.chunkRoot, storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	}, func(chunk, chunks uint32, ref storeio.PageRef) error {
		length := uint64(ref.Length)
		if count != 0 && (count == pageLimit || bytes+length > byteLimit) {
			if err := flush(); err != nil {
				return err
			}
		}
		pages[count] = fileScanPage{
			ref: ref, mask: ^uint64(0), chunk: chunk, chunks: chunks,
		}
		count++
		bytes += length
		return nil
	})
	if err == nil {
		err = flush()
	}
	return scratch, err
}

// RangeMasksRaw visits only the live stable slots named by ordered masks.
// Masks must be strictly increasing by Chunk; zero and dead bits are ignored.
// The callback order is identical to filtering RangeRaw, so query execution
// can push an exact index bound into page reads without changing LIMIT,
// grouping, or stable tie semantics. Inline key/value slices borrow one cache
// lease for the callback. One overflow buffer is reused for the complete call.
func (s *FileSnapshot) RangeMasksRaw(masks []StoreMask, fn func(key, value []byte) error) error {
	_, err := s.RangeMasksRawBuffer(masks, nil, fn)
	return err
}

// RangeMasksRawBuffer is RangeMasksRaw with caller-owned overflow storage.
// The returned slice preserves capacity even when iteration stops with an
// error, allowing a retry loop to remain allocation-free.
func (s *FileSnapshot) RangeMasksRawBuffer(masks []StoreMask, scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.store == nil || s.state == nil {
		return scratch, ErrFileStoreClosed
	}
	if fn == nil {
		return scratch, nil
	}
	state := s.state
	bounds := storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	}
	var previous uint32
	for i, mask := range masks {
		if i != 0 && mask.Chunk <= previous {
			return scratch, ErrStoreMaskOrder
		}
		previous = mask.Chunk
		if mask.Bits == 0 {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(s.store.cache, state.chunkRoot, mask.Chunk, bounds)
		if err != nil {
			return scratch, err
		}
		if !ok {
			return scratch, ErrStoreMaskChunk
		}
		if err := s.rangeFileDocumentPage(state, mask.Chunk, ref, mask.Bits, &scratch, fn); err != nil {
			return scratch, err
		}
	}
	return scratch, nil
}

// RangeMasksRawRowsBuffer is the location-aware form of
// [FileSnapshot.RangeMasksRawBuffer]. The callback receives the stable row
// address selected from this snapshot. It is useful when a covering index
// decides most rows and a query must preserve first-row ordering while
// rechecking only the residual candidates.
//
// Masks must be strictly increasing by Chunk. Zero and dead bits are ignored.
// key and value borrow one cache lease for the callback; overflow storage is
// returned for reuse.
func (s *FileSnapshot) RangeMasksRawRowsBuffer(
	masks []StoreMask,
	scratch []byte,
	fn func(row StoreRow, key, value []byte) error,
) ([]byte, error) {
	if s == nil || s.store == nil || s.state == nil {
		return scratch, ErrFileStoreClosed
	}
	if fn == nil {
		return scratch, nil
	}
	state := s.state
	bounds := storeio.ChunkTreeBounds{
		FileEnd: state.super.FileEnd, NextLogicalID: state.root.NextLogicalID,
	}
	var previous uint32
	for i, mask := range masks {
		if i != 0 && mask.Chunk <= previous {
			return scratch, ErrStoreMaskOrder
		}
		previous = mask.Chunk
		if mask.Bits == 0 {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(
			s.store.cache, state.chunkRoot, mask.Chunk, bounds,
		)
		if err != nil {
			return scratch, err
		}
		if !ok {
			return scratch, ErrStoreMaskChunk
		}
		if err := s.rangeFileDocumentRows(
			state, mask.Chunk, ref, mask.Bits, &scratch, fn,
		); err != nil {
			return scratch, err
		}
	}
	return scratch, nil
}

func (s *FileSnapshot) rangeFileDocumentRows(
	state *fileStoreState,
	chunk uint32,
	ref storeio.PageRef,
	mask uint64,
	overflow *[]byte,
	fn func(row StoreRow, key, value []byte) error,
) error {
	lease, err := s.store.cache.Acquire(ref)
	if err != nil {
		return err
	}
	defer lease.Release()
	view, err := admittedFileDocumentChunk(lease.Page(), ref, chunk)
	if err != nil {
		return err
	}
	selected := view.live() & mask
	return s.rangeFileDocumentView(
		state, view, mask, overflow,
		func(key, value []byte) error {
			if selected == 0 {
				return storeio.ErrDocumentPageCorrupt
			}
			slot := uint8(bits.TrailingZeros64(selected))
			selected &= selected - 1
			return fn(StoreRow{Chunk: chunk, Slot: slot}, key, value)
		},
	)
}

func (s *FileSnapshot) fileScanReadAheadWindow() (int, uint64) {
	options := s.store.options
	parallelLimit := options.ReadConcurrency * 4
	if s.store.cache.ReadBackend() == storeio.BackendIOUring {
		parallelLimit = options.ReadQueueDepth
	}
	pageLimit := min(fileScanReadAheadLimit, options.PrefetchQueue, parallelLimit)
	if pageLimit < 1 {
		pageLimit = 1
	}
	byteLimit := uint64(options.ResidentBytes / 2)
	if byteLimit < uint64(options.MaxPageSize) {
		byteLimit = uint64(options.MaxPageSize)
	}
	return pageLimit, byteLimit
}

func (s *FileSnapshot) rangeFileReadAheadBatch(
	state *fileStoreState,
	pages []fileScanPage,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	var refs [fileScanReadAheadLimit]storeio.PageRef
	for i := range pages {
		refs[i] = pages[i].ref
	}
	physical := refs[:len(pages)]
	slices.SortFunc(physical, func(a, b storeio.PageRef) int {
		switch {
		case a.Offset < b.Offset:
			return -1
		case a.Offset > b.Offset:
			return 1
		default:
			return 0
		}
	})
	if _, err := s.store.cache.Prefetch(physical); err != nil {
		return err
	}
	for i := range pages {
		page := pages[i]
		if err := s.rangeFileDocumentRun(
			state, page.chunk, page.chunks, page.ref, page.mask, overflow, fn,
		); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileSnapshot) rangeFileDocumentPage(
	state *fileStoreState,
	chunk uint32,
	ref storeio.PageRef,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	return s.rangeFileDocumentRun(state, chunk, 1, ref, mask, overflow, fn)
}

func (s *FileSnapshot) rangeFileDocumentRun(
	state *fileStoreState,
	first, chunks uint32,
	ref storeio.PageRef,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	if chunks == 0 || ref.Kind == storeio.PageDocument && chunks != 1 {
		return storeio.ErrChunkDirectoryCorrupt
	}
	lease, err := s.store.cache.Acquire(ref)
	if err != nil {
		return err
	}
	defer lease.Release()
	for ordinal := uint32(0); ordinal < chunks; ordinal++ {
		view, viewErr := admittedFileDocumentChunk(lease.Page(), ref, first+ordinal)
		if viewErr != nil {
			return viewErr
		}
		if err := s.rangeFileDocumentView(state, view, mask, overflow, fn); err != nil {
			return err
		}
	}
	return nil
}

func (s *FileSnapshot) rangeFileDocumentView(
	state *fileStoreState,
	view fileDocumentChunk,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	chunk := view.chunk
	if chunk >= state.root.ChunkHighWater {
		return storeio.ErrDocumentPageCorrupt
	}
	for live := view.live() & mask; live != 0; live &= live - 1 {
		slot := uint8(bits.TrailingZeros64(live))
		record, ok := view.lookup(slot)
		if !ok {
			return storeio.ErrDocumentPageCorrupt
		}
		value := record.value.value.Inline
		if record.value.grouped {
			*overflow = (*overflow)[:0]
			var decoded bool
			*overflow, decoded = view.appendJSON(*overflow, record.value)
			if !decoded {
				return storeio.ErrDocumentGroupCorrupt
			}
			value = *overflow
		} else if record.value.value.Overflow != (storeio.PageRef{}) {
			*overflow = (*overflow)[:0]
			var err error
			*overflow, err = s.store.appendFileValue(*overflow, state, storeio.DocumentValue{
				Overflow: record.value.value.Overflow, Length: record.value.value.Length,
			}, storeio.KeyLocation{Chunk: chunk, Slot: slot})
			if err != nil {
				return err
			}
			value = *overflow
		}
		if err := fn(record.key, value); err != nil {
			return err
		}
	}
	return nil
}
