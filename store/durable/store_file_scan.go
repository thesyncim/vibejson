package durable

import (
	"math/bits"
	"slices"

	"github.com/thesyncim/vibejson/internal/storeio"
	"github.com/thesyncim/vibejson/store"
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
func (s *Snapshot) RangeRaw(fn func(key, value []byte) error) error {
	_, err := s.RangeRawBuffer(nil, fn)
	return err
}

// RangeRawBuffer is RangeRaw with caller-owned overflow storage. The returned
// slice preserves any grown capacity for the next scan. Inline-only scans and
// warmed overflow scans allocate nothing when scratch has sufficient capacity.
//
// This method issues document reads serially. Use RangeRawReadAheadBuffer for
// a cold scan whose corpus exceeds the resident page budget.
func (s *Snapshot) RangeRawBuffer(scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return scratch, ErrClosed
	}
	if fn == nil {
		return scratch, nil
	}
	state := s.state
	err := storeio.WalkChunkTreeRuns(s.collection.cache, state.chunkRoot, storeio.ChunkTreeBounds{
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
func (s *Snapshot) RangeRawReadAheadBuffer(scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return scratch, ErrClosed
	}
	if fn == nil {
		return scratch, nil
	}
	// Buffered files already receive kernel readahead, and feeding resident
	// hits through the user-space queue costs more than a direct scan. Explicit
	// read-ahead is for O_DIRECT, where each miss otherwise blocks the walker.
	readBackend := s.collection.cache.ReadBackend()
	if !s.collection.directRead ||
		(readBackend != storeio.BackendIOUring && s.collection.options.ReadConcurrency == 1) {
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
	err := storeio.WalkChunkTreeRuns(s.collection.cache, state.chunkRoot, storeio.ChunkTreeBounds{
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
func (s *Snapshot) RangeMasksRaw(masks []store.Mask, fn func(key, value []byte) error) error {
	_, err := s.RangeMasksRawBuffer(masks, nil, fn)
	return err
}

// RangeMasksRawBuffer is RangeMasksRaw with caller-owned overflow storage.
// The returned slice preserves capacity even when iteration stops with an
// error, allowing a retry loop to remain allocation-free.
func (s *Snapshot) RangeMasksRawBuffer(masks []store.Mask, scratch []byte, fn func(key, value []byte) error) ([]byte, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return scratch, ErrClosed
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
			return scratch, store.ErrMaskOrder
		}
		previous = mask.Chunk
		if mask.Bits == 0 {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(s.collection.cache, state.chunkRoot, mask.Chunk, bounds)
		if err != nil {
			return scratch, err
		}
		if !ok {
			return scratch, store.ErrMaskChunk
		}
		if err := s.rangeFileDocumentPage(state, mask.Chunk, ref, mask.Bits, &scratch, fn); err != nil {
			return scratch, err
		}
	}
	return scratch, nil
}

// RangeMasksRawRowsBuffer is the location-aware form of
// [Snapshot.RangeMasksRawBuffer]. The callback receives the stable row
// address selected from this snapshot. It is useful when a covering index
// decides most rows and a query must preserve first-row ordering while
// rechecking only the residual candidates.
//
// Masks must be strictly increasing by Chunk. Zero and dead bits are ignored.
// key and value borrow one cache lease for the callback; overflow storage is
// returned for reuse.
func (s *Snapshot) RangeMasksRawRowsBuffer(
	masks []store.Mask,
	scratch []byte,
	fn func(row store.Location, key, value []byte) error,
) ([]byte, error) {
	if s == nil || s.collection == nil || s.state == nil {
		return scratch, ErrClosed
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
			return scratch, store.ErrMaskOrder
		}
		previous = mask.Chunk
		if mask.Bits == 0 {
			continue
		}
		ref, ok, err := storeio.LookupChunkTree(
			s.collection.cache, state.chunkRoot, mask.Chunk, bounds,
		)
		if err != nil {
			return scratch, err
		}
		if !ok {
			return scratch, store.ErrMaskChunk
		}
		if err := s.rangeFileDocumentRows(
			state, mask.Chunk, ref, mask.Bits, &scratch, fn,
		); err != nil {
			return scratch, err
		}
	}
	return scratch, nil
}

func (s *Snapshot) rangeFileDocumentRows(
	state *fileStoreState,
	chunk uint32,
	ref storeio.PageRef,
	mask uint64,
	overflow *[]byte,
	fn func(row store.Location, key, value []byte) error,
) error {
	lease, err := s.collection.cache.Acquire(ref)
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
		state, &view, mask, overflow,
		func(key, value []byte) error {
			if selected == 0 {
				return storeio.ErrDocumentPageCorrupt
			}
			slot := uint8(bits.TrailingZeros64(selected))
			selected &= selected - 1
			return fn(store.Location{Chunk: chunk, Slot: slot}, key, value)
		},
	)
}

func (s *Snapshot) fileScanReadAheadWindow() (int, uint64) {
	options := s.collection.options
	parallelLimit := options.ReadConcurrency * 4
	if s.collection.cache.ReadBackend() == storeio.BackendIOUring {
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

func (s *Snapshot) rangeFileReadAheadBatch(
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
	if _, err := s.collection.cache.Prefetch(physical); err != nil {
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

func (s *Snapshot) rangeFileDocumentPage(
	state *fileStoreState,
	chunk uint32,
	ref storeio.PageRef,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	return s.rangeFileDocumentRun(state, chunk, 1, ref, mask, overflow, fn)
}

func (s *Snapshot) rangeFileDocumentRun(
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
	lease, err := s.collection.cache.Acquire(ref)
	if err != nil {
		return err
	}
	defer lease.Release()
	for ordinal := uint32(0); ordinal < chunks; ordinal++ {
		view, viewErr := admittedFileDocumentChunk(lease.Page(), ref, first+ordinal)
		if viewErr != nil {
			return viewErr
		}
		if err := s.rangeFileDocumentView(state, &view, mask, overflow, fn); err != nil {
			return err
		}
	}
	return nil
}

// rangeFileDocumentView delivers one chunk's selected rows in ascending
// stable-slot order.
//
// view is a pointer, not a value: fileDocumentChunk is 416 bytes because it
// unions an ordinary page view, a compact-group chunk view, and a detached
// float64 view, and passing it by value copied all of it once per chunk and —
// through the old fileDocumentChunk.lookup value receiver — once more per
// document. That copy alone was over a fifth of scan time.
//
// The grouped and ungrouped representations get separate loops rather than a
// per-row branch, because they share nothing in the row-extraction step: an
// ordinary page hands back a borrowed contiguous JSON slice, while a compact
// group must decode a token stream into the caller's overflow buffer.
func (s *Snapshot) rangeFileDocumentView(
	state *fileStoreState,
	view *fileDocumentChunk,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	chunk := view.chunk
	if chunk >= state.root.ChunkHighWater {
		return storeio.ErrDocumentPageCorrupt
	}
	if view.grouped {
		return s.rangeFileGroupedView(view, mask, overflow, fn)
	}
	rows := view.page.Rows(mask)
	for {
		slot, key, json, overflowed, ok := rows.Next()
		if !ok {
			if rows.Err() {
				return storeio.ErrDocumentPageCorrupt
			}
			return nil
		}
		value := json
		if overflowed {
			ref, length, valid := rows.OverflowDescriptor(json)
			if !valid {
				return storeio.ErrDocumentPageCorrupt
			}
			*overflow = (*overflow)[:0]
			var err error
			*overflow, err = s.collection.appendFileValue(*overflow, state, storeio.DocumentValue{
				Overflow: ref, Length: length,
			}, storeio.KeyLocation{Chunk: chunk, Slot: slot})
			if err != nil {
				return err
			}
			value = *overflow
		}
		if err := fn(key, value); err != nil {
			return err
		}
	}
}

// rangeFileGroupedView is the compact-generation half of rangeFileDocumentView.
// Every row is materialized into the shared overflow buffer because a grouped
// row has no contiguous JSON spelling on the page at all.
func (s *Snapshot) rangeFileGroupedView(
	view *fileDocumentChunk,
	mask uint64,
	overflow *[]byte,
	fn func(key, value []byte) error,
) error {
	for live := view.group.Live() & mask; live != 0; live &= live - 1 {
		slot := uint8(bits.TrailingZeros64(live))
		*overflow = (*overflow)[:0]
		value, key, decoded := view.group.AppendRowJSON(*overflow, slot)
		*overflow = value
		if !decoded {
			return storeio.ErrDocumentGroupCorrupt
		}
		if err := fn(key, value); err != nil {
			return err
		}
	}
	return nil
}
