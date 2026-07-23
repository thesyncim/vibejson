package slopjson

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/thesyncim/slopjson/document"
	"github.com/thesyncim/slopjson/internal/byteview"
	"github.com/thesyncim/slopjson/internal/storeio"
)

// WriteFileStore creates one compact, mutable FileStore generation in an
// empty file. Unlike repeated Put calls, bulk creation writes each live
// document, directory node, posting stream, and TTL record exactly once, then
// publishes one double-root durability fence. The resulting file opens with
// [OpenFileStore] and supports the ordinary update, delete, index, and TTL
// operations immediately.
//
// The method borrows the selected immutable Store state while writing. It
// does not retain source slices, and file remains owned by the caller.
// Indexes are rebuilt from options.Indexes even when the source Store has no
// corresponding heap index. A failed call may leave an unpublished partial
// file; as with [Store.WritePageFile], discard it instead of retrying in place.
func (s *Store) WriteFileStore(file *os.File, options FileStoreOptions) (int64, error) {
	if s == nil || file == nil {
		return 0, fmt.Errorf("slopjson: WriteFileStore requires non-nil Store and file")
	}
	info, err := file.Stat()
	if err != nil {
		return 0, err
	}
	if info.Size() != 0 {
		return 0, ErrFileStoreNotEmpty
	}
	normalized, err := options.normalized()
	if err != nil {
		return 0, err
	}

	s.mu.Lock()
	state := s.state.Load()
	if state == nil {
		sourceOptions, normalizeErr := s.Options.normalized()
		if normalizeErr != nil {
			s.mu.Unlock()
			return 0, normalizeErr
		}
		state = &storeState{options: sourceOptions.stateOptions()}
	}
	rows, collectErr := collectFileStoreBulkRows(state, &s.ttl, normalized)
	s.mu.Unlock()
	if collectErr != nil {
		return 0, collectErr
	}

	var storeID [16]byte
	if _, err := rand.Read(storeID[:]); err != nil {
		return 0, fmt.Errorf("slopjson: create FileStore identity: %w", err)
	}
	build := fileStoreBulkBuild{
		source: state, rows: rows, options: normalized, storeID: storeID,
		allocator: fileStoreBulkAllocator{
			offset:      2 * uint64(normalized.PageSize),
			nextLogical: storeio.StateRootLogicalID + 1,
			generation:  1,
			pageSize:    uint32(normalized.PageSize),
		},
	}
	if err := build.plan(); err != nil {
		return 0, err
	}
	if err := build.write(file); err != nil {
		return 0, err
	}
	return int64(build.fileEnd), nil
}

type fileStoreBulkRow struct {
	sourceChunk  uint32
	sourceSlot   uint8
	deadline     int64
	overflowBase int
	overflowN    int
}

func collectFileStoreBulkRows(state *storeState, ttl *storeTTLState, options normalizedFileStoreOptions) ([]fileStoreBulkRow, error) {
	if state.count < 0 || uint64(state.count) > uint64(^uint32(0))*uint64(options.Store.ChunkDocuments) {
		return nil, ErrStoreTooLarge
	}
	rows := make([]fileStoreBulkRow, 0, state.count)
	var collectErr error
	state.chunks.each(func(chunkID uint32, chunk *storeChunk) bool {
		for live := chunk.live; live != 0; live &= live - 1 {
			slot := uint8(bits.TrailingZeros64(live))
			key := chunk.key(int(slot))
			raw := chunk.docs.rawAt(int(chunk.ord[slot]))
			if len(key) > options.MaxKeyBytes {
				collectErr = ErrFileStoreKeyTooLarge
				return false
			}
			if len(raw) > options.MaxDocumentBytes {
				collectErr = ErrFileStoreDocumentTooLarge
				return false
			}
			row := fileStoreBulkRow{sourceChunk: chunkID, sourceSlot: slot, overflowBase: -1}
			if ttl != nil {
				if position, ok := ttl.pos[storeTTLKeyOf(storeLocation{chunk: chunkID, slot: slot})]; ok {
					deadline := ttl.heap[position].deadline.time()
					nanos := deadline.UnixNano()
					if nanos == 0 || !time.Unix(0, nanos).Equal(deadline) {
						collectErr = ErrFileStoreDeadlineRange
						return false
					}
					row.deadline = nanos
				}
			}
			rows = append(rows, row)
		}
		return true
	})
	if collectErr != nil {
		return nil, collectErr
	}
	if len(rows) != state.count {
		return nil, fmt.Errorf("slopjson: FileStore bulk source count invariant")
	}
	return rows, nil
}

type fileStoreBulkAllocator struct {
	offset      uint64
	nextLogical uint64
	generation  uint64
	pageSize    uint32
}

func (a *fileStoreBulkAllocator) allocate(kind storeio.PageKind, length uint32) (storeio.PageRef, error) {
	if length < a.pageSize || length%a.pageSize != 0 || a.nextLogical == 0 ||
		a.nextLogical == math.MaxUint64 || a.offset > math.MaxInt64-uint64(length) {
		return storeio.PageRef{}, ErrStorePersistTooLarge
	}
	ref := storeio.PageRef{
		Offset: a.offset, LogicalID: a.nextLogical, Generation: a.generation,
		Length: length, Kind: kind,
	}
	a.offset += uint64(length)
	a.nextLogical++
	return ref, nil
}

func (a *fileStoreBulkAllocator) allocateStateRoot() (storeio.PageRef, error) {
	if a.offset > math.MaxInt64-uint64(a.pageSize) {
		return storeio.PageRef{}, ErrStorePersistTooLarge
	}
	ref := storeio.PageRef{
		Offset: a.offset, LogicalID: storeio.StateRootLogicalID, Generation: a.generation,
		Length: a.pageSize, Kind: storeio.PageStateRoot,
	}
	a.offset += uint64(a.pageSize)
	return ref, nil
}

type fileStoreBulkOverflowPlan struct {
	row        int
	start, end int
	ref, next  storeio.PageRef
}

type fileStoreBulkDocumentPlan struct {
	first, last int
	chunk       uint32
	live        uint64
	ref         storeio.PageRef
	required    int
	overflow    bool
	group       int // group plan index + 1; zero is an ordinary document page
}

type fileStoreBulkDocumentGroupPlan struct {
	first, last             int // logical document-plan range
	columnFirst, columnLast int // non-empty only on the shared sidecar owner
	ref                     storeio.PageRef
	columns                 storeio.PageRef
}

type fileStoreBulkFloat64DirectoryPlan struct {
	level       uint8
	first, last int
	children    []storeio.Float64DirectoryEntry
	ref         storeio.PageRef
}

type fileStoreBulkFloat64StripePlan struct {
	first, last int
	rows        uint32
	ref         storeio.PageRef
}

type fileStoreBulkIndexGroupPlan struct {
	first, last int
	ref, next   storeio.PageRef
}

type fileStoreBulkKeyPlan struct {
	level       uint8
	first, last int
	children    []fileStoreBulkKeyChild
	ref         storeio.PageRef
}

type fileStoreBulkKeyChild struct {
	lower int
	ref   storeio.PageRef
}

type fileStoreBulkPostingMask struct {
	key        storeio.IndexDirectoryKey
	bits       uint64
	certStart  uint32
	certLength uint16
	collision  bool
}

type fileStoreBulkPostingPlan struct {
	indexID     uint32
	first, last int
	ref         storeio.PageRef
}

type fileStoreBulkIndexPlan struct {
	level       uint8
	first, last int
	children    []storeio.IndexDirectoryChild
	ref         storeio.PageRef
}

type fileStoreBulkTTLPlan struct {
	level       uint8
	first, last int
	children    []storeio.TTLDirectoryChild
	ref         storeio.PageRef
}

type fileStoreBulkBuild struct {
	source  *storeState
	rows    []fileStoreBulkRow
	options normalizedFileStoreOptions
	storeID [16]byte

	allocator fileStoreBulkAllocator
	fileEnd   uint64

	overflows            []fileStoreBulkOverflowPlan
	documents            []fileStoreBulkDocumentPlan
	documentGroups       []fileStoreBulkDocumentGroupPlan
	float64Directories   []fileStoreBulkFloat64DirectoryPlan
	float64DirectoryRows []storeio.Float64DirectoryEntry
	float64Stripes       []fileStoreBulkFloat64StripePlan
	float64ScanDocuments int
	indexGroupEntries    []storeio.IndexGroupCatalogEntry
	indexGroupCatalogs   []fileStoreBulkIndexGroupPlan
	indexGroupRef        storeio.PageRef
	indexGroupCovered    uint64
	indexGroupBlocked    uint64
	indexGroupMissing    [64]uint64
	indexGroupFirst      [64]uint64
	chunks               []storeChunkDirectoryPlan
	keys                 []fileStoreBulkKeyPlan
	keyOrder             []int
	postings             []fileStoreBulkPostingPlan
	masks                []fileStoreBulkPostingMask
	indexes              []fileStoreBulkIndexPlan
	indexRows            []storeio.IndexDirectoryEntry
	indexCertificates    []byte
	ttls                 []fileStoreBulkTTLPlan
	ttlRows              []storeio.TTLKey

	chunkRoot   storeio.PageRef
	keyRoot     storeio.PageRef
	indexRoot   storeio.PageRef
	ttlRoot     storeio.PageRef
	float64Head storeio.PageRef
	stateRef    storeio.PageRef
	root        storeio.StateRoot

	groupChunks  []storeio.DocumentGroupChunk
	groupRecords []storeio.DocumentGroupRecord
	groupSpans   []storeio.DocumentGroupSpan
	groupMasks   []uint64
	groupValues  []float64
	groupCodec   storeio.DocumentGroupWorkspace

	float64Counts        []uint8
	float64Encodings     []uint8
	float64StripeValues  []byte
	float64StripeColumns []storeio.Float64StripeColumn
}

func (b *fileStoreBulkBuild) sourceRow(row int) (*storeChunk, string, []byte) {
	entry := b.rows[row]
	chunk := b.source.chunks.get(entry.sourceChunk)
	return chunk, chunk.key(int(entry.sourceSlot)), chunk.docs.rawAt(int(chunk.ord[entry.sourceSlot]))
}

func (b *fileStoreBulkBuild) sourceFloat64(row, column int) (float64, bool, error) {
	entry := b.rows[row]
	chunk := b.source.chunks.get(entry.sourceChunk)
	sourceRow := [1]int{int(chunk.ord[entry.sourceSlot])}
	var storage [1]RawValue
	values, err := chunk.docs.AppendPointerRows(
		storage[:0], sourceRow[:], b.options.float64Columns[column].pointer,
	)
	if err != nil || len(values) != 1 {
		return 0, false, err
	}
	value, ok := values[0].Float64()
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false, nil
	}
	return value, true, nil
}

func (b *fileStoreBulkBuild) prepareDocumentGroup(first, last int) ([]storeio.DocumentGroupChunk, error) {
	return b.prepareGroupedChunks(first, last, true)
}

func (b *fileStoreBulkBuild) prepareFloat64Group(first, last int) ([]storeio.DocumentGroupChunk, error) {
	return b.prepareGroupedChunks(first, last, false)
}

// prepareGroupedChunks materializes one bounded encoder view into reusable
// builder storage. Detached typed extents skip keys, JSON, and structural
// spans entirely; document groups retain them. Both paths share column
// extraction and the stable-slot invariants.
func (b *fileStoreBulkBuild) prepareGroupedChunks(first, last int, documents bool) ([]storeio.DocumentGroupChunk, error) {
	chunkCount := last - first
	rowCount := 0
	if documents {
		for i := first; i < last; i++ {
			rowCount += b.documents[i].last - b.documents[i].first
		}
	}
	columnCount := len(b.options.float64Columns)
	if cap(b.groupChunks) < chunkCount {
		b.groupChunks = make([]storeio.DocumentGroupChunk, chunkCount)
	} else {
		b.groupChunks = b.groupChunks[:chunkCount]
		clear(b.groupChunks)
	}
	if cap(b.groupRecords) < rowCount {
		b.groupRecords = make([]storeio.DocumentGroupRecord, 0, rowCount)
	} else {
		b.groupRecords = b.groupRecords[:0]
	}
	b.groupSpans = b.groupSpans[:0]
	maskCount := chunkCount * columnCount
	if cap(b.groupMasks) < maskCount {
		b.groupMasks = make([]uint64, maskCount)
	} else {
		b.groupMasks = b.groupMasks[:maskCount]
		clear(b.groupMasks)
	}
	valueCount := maskCount * 64
	if cap(b.groupValues) < valueCount {
		b.groupValues = make([]float64, valueCount)
	} else {
		b.groupValues = b.groupValues[:valueCount]
		clear(b.groupValues)
	}

	for chunkOrdinal := 0; chunkOrdinal < chunkCount; chunkOrdinal++ {
		plan := &b.documents[first+chunkOrdinal]
		if plan.overflow {
			return nil, fmt.Errorf("%w: overflow row in document group", storeio.ErrInvalidWrite)
		}
		recordStart := len(b.groupRecords)
		if documents {
			for row := plan.first; row < plan.last; row++ {
				_, key, raw := b.sourceRow(row)
				spanStart := len(b.groupSpans)
				var err error
				b.groupSpans, err = b.appendDocumentGroupSpans(b.groupSpans, row)
				if err != nil {
					return nil, err
				}
				b.groupRecords = append(b.groupRecords, storeio.DocumentGroupRecord{
					Key: byteview.Bytes(key), JSON: raw,
					Spans: b.groupSpans[spanStart:len(b.groupSpans)], Slot: uint8(row - plan.first),
				})
			}
		}
		maskBase := chunkOrdinal * columnCount
		valueBase := maskBase * 64
		for column := range b.options.float64Columns {
			for row := plan.first; row < plan.last; row++ {
				value, ok, err := b.sourceFloat64(row, column)
				if err != nil {
					return nil, err
				}
				if !ok {
					continue
				}
				slot := row - plan.first
				b.groupMasks[maskBase+column] |= uint64(1) << uint(slot)
				b.groupValues[valueBase+column*64+slot] = value
			}
		}
		b.groupChunks[chunkOrdinal] = storeio.DocumentGroupChunk{
			ChunkID: plan.chunk, Live: plan.live,
			Rows: b.groupRecords[recordStart:len(b.groupRecords)],
			Columns: storeio.DocumentFloat64Columns{
				Masks:  b.groupMasks[maskBase : maskBase+columnCount],
				Values: b.groupValues[valueBase : valueBase+columnCount*64],
			},
		}
	}
	return b.groupChunks, nil
}

// appendDocumentGroupSpans reads compact source metadata directly. It never
// widens or caches a classic tape per document.
func (b *fileStoreBulkBuild) appendDocumentGroupSpans(dst []storeio.DocumentGroupSpan, row int) ([]storeio.DocumentGroupSpan, error) {
	entry := b.rows[row]
	chunk := b.source.chunks.get(entry.sourceChunk)
	ordinal := int(chunk.ord[entry.sourceSlot])
	raw := chunk.docs.rawAt(ordinal)
	start := len(dst)
	if template, ok := chunk.docs.storeTemplateAt(ordinal); ok {
		for i := range template.index.entries {
			tape := &template.index.entries[i]
			if tape.flags()&tapeFlagKey != 0 || tape.next != 1 {
				continue
			}
			span := chunk.docs.storeTemplateSpan(ordinal, template, i)
			dst = append(dst, storeio.DocumentGroupSpan{Start: span & 0xffff, End: span >> 16})
		}
	} else if ref := chunk.docs.shapeTapeRefAt(ordinal); ref.rec != nil {
		if ref.narrow {
			for field := range ref.rec.fields {
				value := chunk.docs.narrowAt(ordinal, ref, field)
				dst = append(dst, storeio.DocumentGroupSpan{Start: value.start(), End: value.end()})
			}
		} else {
			index := chunk.docs.docAt(ordinal)
			for i := range index.entries {
				value := &index.entries[i]
				dst = append(dst, storeio.DocumentGroupSpan{Start: value.start, End: value.end})
			}
		}
	} else {
		index := chunk.docs.docAt(ordinal)
		if len(index.entries) == 0 {
			return nil, fmt.Errorf("%w: missing document tape for group encoding", storeio.ErrInvalidWrite)
		}
		for i := range index.entries {
			tape := &index.entries[i]
			if tape.flags()&tapeFlagKey == 0 && tape.next == 1 {
				dst = append(dst, storeio.DocumentGroupSpan{Start: tape.start, End: tape.end})
			}
		}
	}
	for i := start + 1; i < len(dst); i++ {
		if dst[i-1].End > dst[i].Start || uint64(dst[i].End) > uint64(len(raw)) {
			return nil, fmt.Errorf("%w: unordered document-group spans", storeio.ErrInvalidWrite)
		}
	}
	if start < len(dst) && (dst[start].Start >= dst[start].End ||
		uint64(dst[len(dst)-1].End) > uint64(len(raw))) {
		return nil, fmt.Errorf("%w: invalid document-group span bounds", storeio.ErrInvalidWrite)
	}
	return dst, nil
}

func (b *fileStoreBulkBuild) documentFloat64Bytes(first, last int) (int, error) {
	bytes := len(b.options.float64Columns) * 8
	var counts [fileStoreMaxFloat64Columns]uint8
	var encodings [fileStoreMaxFloat64Columns]uint8
	for column := range b.options.float64Columns {
		for row := first; row < last; row++ {
			value, ok, err := b.sourceFloat64(row, column)
			if err != nil {
				return 0, err
			}
			if ok {
				bytes += 8
				counts[column]++
				encodings[column] = max(encodings[column], fileStoreFloat64Encoding(value))
			}
		}
	}
	columns := len(b.options.float64Columns)
	b.float64Counts = append(b.float64Counts, counts[:columns]...)
	b.float64Encodings = append(b.float64Encodings, encodings[:columns]...)
	return bytes, nil
}

// fileStoreFloat64Encoding orders exact representations from narrowest to
// widest. Three is the general IEEE float64 fallback.
func fileStoreFloat64Encoding(value float64) uint8 {
	switch {
	case math.Signbit(value) || value > math.MaxUint32 || value != math.Trunc(value):
		return 3
	case value <= math.MaxUint8:
		return 0
	case value <= math.MaxUint16:
		return 1
	default:
		return 2
	}
}

func (b *fileStoreBulkBuild) targetLocation(row int) storeio.KeyLocation {
	chunkDocuments := b.options.Store.ChunkDocuments
	return storeio.KeyLocation{
		Chunk: uint32(row / chunkDocuments), Slot: uint8(row % chunkDocuments),
		Deadline: b.rows[row].deadline,
	}
}

func (b *fileStoreBulkBuild) plan() error {
	if err := b.validateSchema(); err != nil {
		return err
	}
	if err := b.planDocuments(); err != nil {
		return err
	}
	if err := b.planFloat64Directory(); err != nil {
		return err
	}
	items := make([]storeChunkDirectoryItem, len(b.documents))
	for i := range b.documents {
		items[i] = storeChunkDirectoryItem{id: b.documents[i].chunk, ref: b.documents[i].ref}
	}
	var err error
	b.chunks, b.chunkRoot, err = planFileStoreBulkChunkDirectories(items, &b.allocator)
	if err != nil {
		return err
	}
	if err := b.planKeys(); err != nil {
		return err
	}
	if err := b.planPostings(); err != nil {
		return err
	}
	if err := b.planIndexGroups(); err != nil {
		return err
	}
	if err := b.planIndexTree(); err != nil {
		return err
	}
	if err := b.planTTLTree(); err != nil {
		return err
	}
	stateRef, err := b.allocator.allocateStateRoot()
	if err != nil {
		return err
	}
	b.stateRef = stateRef
	b.fileEnd = b.allocator.offset

	chunkHighWater := uint32(len(b.documents))
	freeChunkHint := chunkHighWater
	if len(b.rows) != 0 && len(b.rows)%b.options.Store.ChunkDocuments != 0 {
		freeChunkHint--
	}
	b.root = storeio.StateRoot{
		StoreID: b.storeID, Generation: b.allocator.generation, PageSize: b.allocator.pageSize,
		DocumentCount: uint64(len(b.rows)), TTLCount: uint64(len(b.ttlRows)),
		NextLogicalID: b.allocator.nextLogical, ChunkHighWater: chunkHighWater,
		LiveChunks: chunkHighWater, ChunkDocuments: uint32(b.options.Store.ChunkDocuments),
		IndexCount: uint32(len(b.options.indexes)), IndexCatalogHash: b.options.indexCatalogHash,
		IndexMaxDepth: uint32(max(b.options.Store.IndexOptions.MaxDepth, 0)),
		FreeChunkHint: freeChunkHint, ChunkDirectory: b.chunkRoot, KeyDirectory: b.keyRoot,
		IndexDirectory: b.indexRoot, TTLDirectory: b.ttlRoot, Float64ScanHead: b.float64Head,
		IndexGroupHead: b.indexGroupRef,
	}
	if len(b.options.float64Columns) != 0 {
		b.root.Options |= storeio.StateOptionFloat64Columns
	}
	if b.options.Store.Schema != nil {
		b.root.Options |= storeio.StateOptionSchema
	}
	return nil
}

func (b *fileStoreBulkBuild) validateSchema() error {
	schema := b.options.Store.Schema
	if schema == nil {
		return nil
	}
	var (
		rows   [storeMaxChunkDocuments]int
		values [storeMaxChunkDocuments]RawValue
	)
	for first := 0; first < len(b.rows); {
		chunkID := b.rows[first].sourceChunk
		chunk := b.source.chunks.get(chunkID)
		if chunk == nil {
			return storeio.ErrDocumentPageCorrupt
		}
		last := first
		for last < len(b.rows) &&
			b.rows[last].sourceChunk == chunkID {
			entry := b.rows[last]
			if last-first == len(rows) ||
				chunk.live&(uint64(1)<<entry.sourceSlot) == 0 {
				return storeio.ErrDocumentPageCorrupt
			}
			rows[last-first] = int(chunk.ord[entry.sourceSlot])
			last++
		}
		failed, err := schema.validateDocSetRows(
			&chunk.docs, rows[:last-first], values[:0],
		)
		if err != nil {
			if failed < 0 {
				return err
			}
			return fmt.Errorf(
				"slopjson: row %d: %w", first+failed, err,
			)
		}
		first = last
	}
	return nil
}

// FileStore chunk lookup has a fixed six-node radix path (shifts
// 30,24,18,12,6,0). The read-only page checkpoint may stop at a shallower
// root, so its otherwise shared planner cannot be used here.
func planFileStoreBulkChunkDirectories(items []storeChunkDirectoryItem, allocator *fileStoreBulkAllocator) ([]storeChunkDirectoryPlan, storeio.PageRef, error) {
	if len(items) == 0 {
		return nil, storeio.PageRef{}, nil
	}
	all := make([]storeChunkDirectoryPlan, 0, (len(items)+62)/63+5)
	for shift := uint8(0); ; shift += 6 {
		next := make([]storeChunkDirectoryItem, 0, (len(items)+63)/64)
		for start := 0; start < len(items); {
			covered := uint(shift) + 6
			group := items[start].id
			if covered < 32 {
				group >>= covered
			} else {
				group = 0
			}
			end := start + 1
			for end < len(items) {
				other := items[end].id
				if covered < 32 {
					other >>= covered
				} else {
					other = 0
				}
				if other != group {
					break
				}
				end++
			}
			children := make([]storeio.PageRef, end-start)
			var bitmap uint64
			for i := start; i < end; i++ {
				bitmap |= uint64(1) << uint(items[i].id>>shift&63)
				children[i-start] = items[i].ref
			}
			prefix := items[start].id
			if covered < 32 {
				prefix &^= uint32(1)<<covered - 1
			} else {
				prefix = 0
			}
			ref, err := allocator.allocate(storeio.PageChunkDirectory, allocator.pageSize)
			if err != nil {
				return nil, storeio.PageRef{}, err
			}
			all = append(all, storeChunkDirectoryPlan{
				prefix: prefix, shift: shift, bitmap: bitmap, children: children, ref: ref,
			})
			next = append(next, storeChunkDirectoryItem{id: prefix, ref: ref})
			start = end
		}
		if shift == 30 {
			if len(next) != 1 {
				return nil, storeio.PageRef{}, fmt.Errorf(
					"%w: FileStore chunk radix root", storeio.ErrInvalidWrite,
				)
			}
			return all, next[0].ref, nil
		}
		items = next
	}
}

func (b *fileStoreBulkBuild) planDocuments() error {
	if len(b.rows) == 0 {
		return nil
	}
	chunkDocuments := b.options.Store.ChunkDocuments
	chunkCount := (len(b.rows) + chunkDocuments - 1) / chunkDocuments
	if uint64(chunkCount) > uint64(^uint32(0)) {
		return ErrStoreTooLarge
	}
	overflowPayload := b.options.MaxPageSize - storeio.PageHeaderSize -
		storeio.PageTrailerSize - storeio.OverflowPagePayloadHeaderSize
	b.documents = make([]fileStoreBulkDocumentPlan, 0, chunkCount)
	for first := 0; first < len(b.rows); first += chunkDocuments {
		last := min(first+chunkDocuments, len(b.rows))
		chunkID := uint32(first / chunkDocuments)
		required := storeio.PageHeaderSize + storeio.PageTrailerSize +
			storeio.DocumentPagePayloadHeaderSize + (last-first)*storeio.DocumentPageRecordSize
		for row := first; row < last; row++ {
			_, key, raw := b.sourceRow(row)
			required += len(key) + len(raw)
		}
		float64Bytes, err := b.documentFloat64Bytes(first, last)
		if err != nil {
			return err
		}
		required += float64Bytes
		// InlineValueBytes is the ordinary online-write threshold, not a
		// format limit. A compact generation instead keeps complete values in
		// the document extent while the chunk fits, avoiding a 64 KiB overflow
		// extent for a value that only pushed one 4 KiB chunk to 8 KiB. If the
		// chunk is genuinely too large, spill the largest remaining value
		// first; this reaches the bound with the fewest overflow chains.
		var overflowMask uint64
		for required > b.options.MaxPageSize {
			largestRow, largestBytes := -1, -1
			for row := first; row < last; row++ {
				bit := uint64(1) << uint(row-first)
				if overflowMask&bit != 0 {
					continue
				}
				_, _, raw := b.sourceRow(row)
				if len(raw) > largestBytes {
					largestRow, largestBytes = row, len(raw)
				}
			}
			if largestRow < 0 {
				return ErrFileStoreDocumentTooLarge
			}
			overflowMask |= uint64(1) << uint(largestRow-first)
			required -= largestBytes
			required += storeio.DocumentOverflowDescriptorSize
		}
		for row := first; row < last; row++ {
			if overflowMask&(uint64(1)<<uint(row-first)) == 0 {
				continue
			}
			_, _, raw := b.sourceRow(row)
			base := len(b.overflows)
			for start := 0; start < len(raw); start += overflowPayload {
				end := min(start+overflowPayload, len(raw))
				ref, err := b.allocator.allocate(storeio.PageOverflow, uint32(b.options.MaxPageSize))
				if err != nil {
					return err
				}
				b.overflows = append(b.overflows, fileStoreBulkOverflowPlan{
					row: row, start: start, end: end, ref: ref,
				})
			}
			b.rows[row].overflowBase = base
			b.rows[row].overflowN = len(b.overflows) - base
			for i := base; i+1 < len(b.overflows); i++ {
				b.overflows[i].next = b.overflows[i+1].ref
			}
		}
		count := last - first
		live := ^uint64(0)
		if count < 64 {
			live = uint64(1)<<uint(count) - 1
		}
		b.documents = append(b.documents, fileStoreBulkDocumentPlan{
			first: first, last: last, chunk: chunkID, live: live,
			required: required, overflow: overflowMask != 0,
		})
	}
	return b.planDocumentGroups()
}

const (
	fileStoreBulkDocumentGroupRows   = 128
	fileStoreBulkFloat64RegionGroups = 120
)

type fileStoreBulkFloat64Segment struct {
	firstGroup, lastGroup       int
	firstDocument, lastDocument int
	columnSize                  uint32
}

// planDocumentGroups keeps the online stable-slot unit unchanged while
// packing consecutive compact-generation chunks into larger immutable
// extents. A later update redirects only its touched logical chunk to an
// ordinary page; untouched lanes continue sharing the original group.
func (b *fileStoreBulkBuild) planDocumentGroups() error {
	var segments [fileStoreBulkFloat64RegionGroups]fileStoreBulkFloat64Segment
	segmentCount := 0
	regionGroups := 0
	flushColumns := func() error {
		if segmentCount == 0 {
			return nil
		}
		for segmentIndex := 0; segmentIndex < segmentCount; segmentIndex++ {
			segment := segments[segmentIndex]
			columns, err := b.allocator.allocate(storeio.PageFloat64Group, segment.columnSize)
			if err != nil {
				return err
			}
			for groupIndex := segment.firstGroup; groupIndex < segment.lastGroup; groupIndex++ {
				group := &b.documentGroups[groupIndex]
				group.ref, err = storeio.AttachDocumentGroupFloat64Sidecar(
					group.ref, columns, uint32(b.options.PageSize),
				)
				if err != nil {
					return err
				}
				group.columns = columns
				for document := group.first; document < group.last; document++ {
					b.documents[document].ref = group.ref
				}
			}
			owner := &b.documentGroups[segment.firstGroup]
			owner.columnFirst = segment.firstDocument
			owner.columnLast = segment.lastDocument
		}
		clear(segments[:segmentCount])
		segmentCount = 0
		regionGroups = 0
		return nil
	}
	regionFits := func(groupSize, columnSize uint32, newSegment bool) bool {
		endGroups := b.allocator.offset + uint64(groupSize)
		sidecarOffset := endGroups
		maxForward := storeio.DocumentGroupFloat64MaxForwardBytes(uint32(b.options.PageSize))
		for segmentIndex := 0; segmentIndex < segmentCount; segmentIndex++ {
			segment := segments[segmentIndex]
			size := segment.columnSize
			if !newSegment && segmentIndex == segmentCount-1 {
				size = columnSize
			}
			firstOffset := b.documentGroups[segment.firstGroup].ref.Offset
			if sidecarOffset < firstOffset || sidecarOffset-firstOffset > maxForward {
				return false
			}
			sidecarOffset += uint64(size)
		}
		if newSegment &&
			(sidecarOffset < b.allocator.offset ||
				sidecarOffset-b.allocator.offset > maxForward) {
			return false
		}
		return true
	}

	for first := 0; first < len(b.documents); {
		last, rows := first, 0
		for last < len(b.documents) && !b.documents[last].overflow {
			next := b.documents[last].last - b.documents[last].first
			if rows+next > fileStoreBulkDocumentGroupRows {
				break
			}
			rows += next
			last++
		}
		grouped := false
		if last-first >= 2 {
			chunks, err := b.prepareDocumentGroup(first, last)
			if err != nil {
				return err
			}
			columnSize := uint32(0)
			if len(b.options.float64Columns) != 0 {
				if size, ok := b.float64GroupExtent(first, last); ok {
					columnSize = size
					clearDocumentGroupColumns(chunks)
				}
			}
			groupSize, ok := storeio.DocumentGroupSize(
				chunks, uint32(b.options.PageSize), &b.groupCodec,
			)
			individualBytes := 0
			for i := first; i < last; i++ {
				size, fits := fileStoreBulkExtent(
					b.documents[i].required, b.options.PageSize, b.options.MaxPageSize,
				)
				if !fits {
					return ErrFileStoreDocumentTooLarge
				}
				individualBytes += int(size)
			}
			canGroup := ok && groupSize <= uint32(b.options.MaxPageSize) &&
				int(groupSize)+int(columnSize) < individualBytes
			newSegment := false
			segmentColumnSize := columnSize
			if canGroup && columnSize != 0 {
				newSegment = segmentCount == 0
				if !newSegment {
					current := segments[segmentCount-1]
					combinedSize, combined := b.float64GroupExtent(
						current.firstDocument, last,
					)
					if combined {
						segmentColumnSize = combinedSize
					} else {
						newSegment = true
					}
				}
				if regionGroups == fileStoreBulkFloat64RegionGroups ||
					!regionFits(groupSize, segmentColumnSize, newSegment) {
					if err := flushColumns(); err != nil {
						return err
					}
					newSegment = true
					segmentColumnSize = columnSize
				}
				canGroup = regionFits(groupSize, segmentColumnSize, newSegment)
			}
			if canGroup {
				if columnSize == 0 {
					if err := flushColumns(); err != nil {
						return err
					}
				}
				ref, allocateErr := b.allocator.allocate(storeio.PageDocumentGroup, groupSize)
				if allocateErr != nil {
					return allocateErr
				}
				group := len(b.documentGroups)
				if columnSize != 0 {
					if newSegment {
						if segmentCount == len(segments) {
							return fmt.Errorf("%w: float64 region segments", storeio.ErrInvalidWrite)
						}
						segments[segmentCount] = fileStoreBulkFloat64Segment{
							firstGroup: group, firstDocument: first, columnSize: segmentColumnSize,
						}
						segmentCount++
					}
					current := &segments[segmentCount-1]
					current.lastGroup = group + 1
					current.lastDocument = last
					current.columnSize = segmentColumnSize
					regionGroups++
				}
				b.documentGroups = append(b.documentGroups, fileStoreBulkDocumentGroupPlan{
					first: first, last: last, ref: ref,
				})
				for i := first; i < last; i++ {
					b.documents[i].ref = ref
					b.documents[i].group = group + 1
				}
				first = last
				grouped = true
			}
		}
		if grouped {
			continue
		}
		if err := flushColumns(); err != nil {
			return err
		}
		size, ok := fileStoreBulkExtent(
			b.documents[first].required, b.options.PageSize, b.options.MaxPageSize,
		)
		if !ok {
			return ErrFileStoreDocumentTooLarge
		}
		ref, err := b.allocator.allocate(storeio.PageDocument, size)
		if err != nil {
			return err
		}
		b.documents[first].ref = ref
		first++
	}
	if err := flushColumns(); err != nil {
		return err
	}
	b.prepareFloat64ScanPlan()
	return nil
}

// prepareFloat64ScanPlan records the immutable source range used by the
// aggregate-only stripe. The stripe is independent of document grouping and
// covers ordinary pages, shared sidecars, and overflow-backed documents.
func (b *fileStoreBulkBuild) prepareFloat64ScanPlan() {
	b.float64Head = storeio.PageRef{}
	b.float64DirectoryRows = b.float64DirectoryRows[:0]
	b.float64ScanDocuments = 0
	if len(b.options.float64Columns) != 0 {
		b.float64ScanDocuments = len(b.documents)
	}
}

// planFloat64Directory builds a 64-way ordered tree over immutable packed
// stripes. Nodes stay at PageSize so online path copying never scales with a
// large MaxPageSize; full scans still touch fewer than one directory page per
// 64 stripes.
func (b *fileStoreBulkBuild) planFloat64Directory() error {
	if b.float64ScanDocuments == 0 {
		return nil
	}
	if err := b.planFloat64Stripes(); err != nil {
		return err
	}
	b.float64DirectoryRows = b.float64DirectoryRows[:0]
	for _, stripe := range b.float64Stripes {
		if stripe.first < 0 || stripe.first >= len(b.documents) {
			return storeio.ErrInvalidWrite
		}
		b.float64DirectoryRows = append(
			b.float64DirectoryRows,
			storeio.Float64DirectoryEntry{
				FirstChunk: b.documents[stripe.first].chunk,
				Ref:        stripe.ref,
			},
		)
	}
	if len(b.float64DirectoryRows) == 0 ||
		b.float64DirectoryRows[0].FirstChunk != 0 {
		return storeio.ErrInvalidWrite
	}
	levelStart := 0
	for first := 0; first < len(b.float64DirectoryRows); first += storeio.Float64DirectoryFanout {
		last := min(
			first+storeio.Float64DirectoryFanout,
			len(b.float64DirectoryRows),
		)
		ref, err := b.allocator.allocate(
			storeio.PageFloat64Catalog, b.allocator.pageSize,
		)
		if err != nil {
			return err
		}
		b.float64Directories = append(
			b.float64Directories,
			fileStoreBulkFloat64DirectoryPlan{
				first: first, last: last, ref: ref,
			},
		)
	}
	levelEnd := len(b.float64Directories)
	for level := uint8(1); levelEnd-levelStart > 1; level++ {
		if level > storeio.Float64DirectoryMaxLevel {
			return storeio.ErrFloat64DirectoryDepth
		}
		nextStart := len(b.float64Directories)
		for first := levelStart; first < levelEnd; first += storeio.Float64DirectoryFanout {
			last := min(
				first+storeio.Float64DirectoryFanout, levelEnd,
			)
			children := make(
				[]storeio.Float64DirectoryEntry, last-first,
			)
			for i := first; i < last; i++ {
				children[i-first] = storeio.Float64DirectoryEntry{
					FirstChunk: b.float64DirectoryLower(
						b.float64Directories[i],
					),
					Ref: b.float64Directories[i].ref,
				}
			}
			ref, err := b.allocator.allocate(
				storeio.PageFloat64Catalog, b.allocator.pageSize,
			)
			if err != nil {
				return err
			}
			b.float64Directories = append(
				b.float64Directories,
				fileStoreBulkFloat64DirectoryPlan{
					level: level, children: children, ref: ref,
				},
			)
		}
		levelStart, levelEnd = nextStart, len(b.float64Directories)
	}
	b.float64Head = b.float64Directories[levelStart].ref
	return nil
}

func (b *fileStoreBulkBuild) float64DirectoryLower(
	plan fileStoreBulkFloat64DirectoryPlan,
) uint32 {
	if plan.level == 0 {
		return b.float64DirectoryRows[plan.first].FirstChunk
	}
	return plan.children[0].FirstChunk
}

func (b *fileStoreBulkBuild) planFloat64Stripes() error {
	columns := len(b.options.float64Columns)
	if columns == 0 || b.float64ScanDocuments == 0 {
		return nil
	}
	var counts [fileStoreMaxFloat64Columns]uint64
	var encodings [fileStoreMaxFloat64Columns]uint8
	first := 0
	rows := uint64(0)
	for document := 0; document < b.float64ScanDocuments; {
		required := uint64(
			storeio.PageHeaderSize + storeio.PageTrailerSize +
				storeio.Float64StripePayloadHeaderSize + columns*storeio.Float64StripeColumnSize,
		)
		for column := 0; column < columns; column++ {
			offset := document*columns + column
			nextCount := counts[column] + uint64(b.float64Counts[offset])
			nextEncoding := max(encodings[column], b.float64Encodings[offset])
			width := [...]uint64{1, 2, 4, 8}[nextEncoding]
			required += nextCount * width
		}
		nextRows := rows + uint64(bits.OnesCount64(b.documents[document].live))
		tooLarge := required > uint64(b.options.MaxPageSize) ||
			uint64(document-first)+1 > math.MaxUint32 ||
			nextRows > math.MaxUint32
		if tooLarge && document > first {
			if err := b.allocateFloat64Stripe(
				first, document, uint32(rows), counts[:columns], encodings[:columns],
			); err != nil {
				return err
			}
			clear(counts[:columns])
			clear(encodings[:columns])
			first = document
			rows = 0
			continue
		}
		if tooLarge {
			return storeio.ErrInvalidWrite
		}
		for column := 0; column < columns; column++ {
			offset := document*columns + column
			counts[column] += uint64(b.float64Counts[offset])
			encodings[column] = max(encodings[column], b.float64Encodings[offset])
		}
		rows = nextRows
		document++
	}
	return b.allocateFloat64Stripe(
		first, b.float64ScanDocuments, uint32(rows), counts[:columns], encodings[:columns],
	)
}

func (b *fileStoreBulkBuild) allocateFloat64Stripe(
	first, last int,
	rows uint32,
	counts []uint64,
	encodings []uint8,
) error {
	if first < 0 || last <= first || last > b.float64ScanDocuments || rows == 0 ||
		uint64(last-first) > math.MaxUint32 {
		return storeio.ErrInvalidWrite
	}
	required := uint64(
		storeio.PageHeaderSize + storeio.PageTrailerSize +
			storeio.Float64StripePayloadHeaderSize +
			len(counts)*storeio.Float64StripeColumnSize,
	)
	for column, count := range counts {
		required += count * [...]uint64{1, 2, 4, 8}[encodings[column]]
	}
	if required > uint64(b.options.MaxPageSize) {
		return storeio.ErrInvalidWrite
	}
	size, ok := fileStoreBulkExtent(int(required), b.options.PageSize, b.options.MaxPageSize)
	if !ok {
		return storeio.ErrInvalidWrite
	}
	ref, err := b.allocator.allocate(storeio.PageFloat64Stripe, size)
	if err != nil {
		return err
	}
	b.float64Stripes = append(b.float64Stripes, fileStoreBulkFloat64StripePlan{
		first: first, last: last, rows: rows, ref: ref,
	})
	return nil
}

// float64GroupExtent computes the exact adaptive shared-sidecar extent from
// compact count/encoding metadata gathered for ordinary document pages. It
// avoids a second JSON extraction pass and never under-allocates after a
// segment widens from an integer lane to general float64.
func (b *fileStoreBulkBuild) float64GroupExtent(first, last int) (uint32, bool) {
	if first < 0 || last > len(b.documents) || last-first < 2 ||
		len(b.options.float64Columns) == 0 {
		return 0, false
	}
	required := storeio.PageHeaderSize + storeio.PageTrailerSize +
		storeio.Float64GroupPayloadHeaderSize +
		(last-first)*storeio.Float64GroupChunkSize +
		len(b.options.float64Columns)*4 +
		(last-first)*len(b.options.float64Columns)*8
	columns := len(b.options.float64Columns)
	for column := 0; column < columns; column++ {
		encoding := uint8(0)
		count := 0
		for document := first; document < last; document++ {
			offset := document*columns + column
			encoding = max(encoding, b.float64Encodings[offset])
			count += int(b.float64Counts[offset])
		}
		width := [...]int{1, 2, 4, 8}[encoding]
		required += count * width
	}
	return fileStoreBulkExtent(required, b.options.PageSize, b.options.MaxPageSize)
}

func clearDocumentGroupColumns(chunks []storeio.DocumentGroupChunk) {
	for i := range chunks {
		chunks[i].Columns = storeio.DocumentFloat64Columns{}
	}
}

func fileStoreBulkExtent(required, minimum, maximum int) (uint32, bool) {
	if required < 0 || required > maximum {
		return 0, false
	}
	size := minimum
	for size < required {
		if size > maximum/2 {
			return 0, false
		}
		size <<= 1
	}
	return uint32(size), true
}

func (b *fileStoreBulkBuild) planKeys() error {
	if len(b.rows) == 0 {
		return nil
	}
	b.keyOrder = make([]int, len(b.rows))
	for i := range b.keyOrder {
		b.keyOrder[i] = i
	}
	slices.SortFunc(b.keyOrder, func(a, c int) int {
		_, ak, _ := b.sourceRow(a)
		_, ck, _ := b.sourceRow(c)
		return strings.Compare(ak, ck)
	})
	for i := 1; i < len(b.keyOrder); i++ {
		_, previous, _ := b.sourceRow(b.keyOrder[i-1])
		_, current, _ := b.sourceRow(b.keyOrder[i])
		if previous == current {
			return fmt.Errorf("%w %q", ErrStoreDuplicateKey, current)
		}
	}

	levelStart := 0
	for first := 0; first < len(b.keyOrder); {
		used := storeio.PageHeaderSize + storeio.PageTrailerSize + storeio.KeyDirectoryPayloadHeaderSize
		last := first
		for last < len(b.keyOrder) {
			_, key, _ := b.sourceRow(b.keyOrder[last])
			next := used + storeio.KeyDirectoryLeafRecordSize + len(key)
			if next > b.options.PageSize {
				break
			}
			used = next
			last++
		}
		if last == first {
			return ErrFileStoreKeyTooLarge
		}
		ref, err := b.allocator.allocate(storeio.PageKeyDirectory, b.allocator.pageSize)
		if err != nil {
			return err
		}
		b.keys = append(b.keys, fileStoreBulkKeyPlan{first: first, last: last, ref: ref})
		first = last
	}
	levelEnd := len(b.keys)
	for level := uint8(1); levelEnd-levelStart > 1; level++ {
		if level > 10 {
			return storeio.ErrKeyTreeDepth
		}
		nextStart := len(b.keys)
		for first := levelStart; first < levelEnd; {
			used := storeio.PageHeaderSize + storeio.PageTrailerSize + storeio.KeyDirectoryPayloadHeaderSize
			children := make([]fileStoreBulkKeyChild, 0, min(64, levelEnd-first))
			for last := first; last < levelEnd && len(children) < 64; last++ {
				lower := b.keyPlanLower(b.keys[last])
				_, key, _ := b.sourceRow(lower)
				next := used + storeio.KeyDirectoryBranchRecordSize + len(key)
				if next > b.options.PageSize {
					break
				}
				used = next
				children = append(children, fileStoreBulkKeyChild{lower: lower, ref: b.keys[last].ref})
			}
			if len(children) == 0 {
				return ErrFileStoreKeyTooLarge
			}
			ref, err := b.allocator.allocate(storeio.PageKeyDirectory, b.allocator.pageSize)
			if err != nil {
				return err
			}
			b.keys = append(b.keys, fileStoreBulkKeyPlan{level: level, children: children, ref: ref})
			first += len(children)
		}
		levelStart, levelEnd = nextStart, len(b.keys)
	}
	b.keyRoot = b.keys[levelStart].ref
	return nil
}

func (b *fileStoreBulkBuild) keyPlanLower(plan fileStoreBulkKeyPlan) int {
	if plan.level == 0 {
		return b.keyOrder[plan.first]
	}
	return plan.children[0].lower
}

func (b *fileStoreBulkBuild) planPostings() error {
	if len(b.options.indexes) == 0 || len(b.rows) == 0 {
		return nil
	}
	if len(b.rows) > int(^uint(0)>>1)/len(b.options.indexes) {
		return ErrStoreTooLarge
	}
	b.masks = make([]fileStoreBulkPostingMask, 0, len(b.rows)*len(b.options.indexes))
	var textScratch []byte
	for row := range b.rows {
		chunk, _, _ := b.sourceRow(row)
		location := b.targetLocation(row)
		for indexID, exact := range b.options.indexes {
			hash, ok, values, scratch, err := fileStoreBulkTupleHash(
				exact, chunk, int(b.rows[row].sourceSlot), textScratch[:0],
			)
			textScratch = scratch
			if err != nil {
				return err
			}
			if !ok {
				if exact.n == 1 {
					if len(values[0].Bytes()) == 0 {
						if b.indexGroupMissing[indexID] == 0 {
							b.indexGroupFirst[indexID] =
								uint64(location.Chunk)<<6 | uint64(location.Slot)
						}
						b.indexGroupMissing[indexID]++
					} else {
						b.indexGroupBlocked |= uint64(1) << uint(indexID)
					}
				}
				continue
			}
			mask := fileStoreBulkPostingMask{
				key: storeio.IndexDirectoryKey{
					IndexID: uint32(indexID), TupleHash: hash, Chunk: location.Chunk,
				},
				bits: uint64(1) << location.Slot,
			}
			maxCertificate := b.options.PageSize - storeio.PageHeaderSize -
				storeio.PageTrailerSize - storeio.PostingPagePayloadHeaderSize -
				storeio.PostingSegmentHeaderSize - 16
			certificateStart := len(b.indexCertificates)
			certificates, certified := appendFileIndexCertificate(
				b.indexCertificates, values[:exact.n], maxCertificate,
			)
			certificateLength := len(certificates) - certificateStart
			if certified && uint64(certificateStart) <= uint64(^uint32(0)) &&
				certificateLength <= int(^uint16(0)) {
				b.indexCertificates = certificates
				mask.certStart = uint32(certificateStart)
				mask.certLength = uint16(certificateLength)
			}
			b.masks = append(b.masks, mask)
		}
	}
	slices.SortFunc(b.masks, func(a, c fileStoreBulkPostingMask) int {
		return compareFileStoreBulkIndexKey(a.key, c.key)
	})
	if len(b.masks) != 0 {
		out := b.masks[:1]
		for _, entry := range b.masks[1:] {
			last := &out[len(out)-1]
			if compareFileStoreBulkIndexKey(last.key, entry.key) == 0 {
				last.bits |= entry.bits
				if last.certLength != 0 && entry.certLength != 0 {
					left := b.indexCertificates[last.certStart : last.certStart+uint32(last.certLength) : last.certStart+uint32(last.certLength)]
					right := b.indexCertificates[entry.certStart : entry.certStart+uint32(entry.certLength) : entry.certStart+uint32(entry.certLength)]
					columns := int(b.options.indexes[last.key.IndexID].n)
					if !fileIndexCertificatesEqual(left, right, columns) {
						last.collision = true
					}
				} else {
					last.certStart, last.certLength, last.collision = 0, 0, false
				}
				continue
			}
			out = append(out, entry)
		}
		clear(b.masks[len(out):])
		b.masks = out
	}

	payloadLimit := b.options.PageSize - storeio.PageHeaderSize - storeio.PageTrailerSize
	// The compact generation densely packs streams from one declared index.
	// Directory refs mark these pages as an immutable base: an online mutation
	// redirects only its changed stream to an isolated delta page and never
	// retires shared base storage. Repeated churn therefore plateaus without a
	// durable page-level reference-count tree.
	for first := 0; first < len(b.masks); {
		indexID := b.masks[first].key.IndexID
		used := storeio.PostingPagePayloadHeaderSize
		last := first
		for last < len(b.masks) && b.masks[last].key.IndexID == indexID {
			entry := storeio.PostingEntry{Chunk: b.masks[last].key.Chunk, Bits: b.masks[last].bits}
			encoded, err := storeio.PostingEntryEncodedSize(entry.Chunk, entry, true)
			if err != nil {
				return err
			}
			next := used + storeio.PostingSegmentHeaderSize + encoded +
				int(b.masks[last].certLength)
			if next > payloadLimit {
				break
			}
			used = next
			last++
		}
		if last == first {
			return storeio.ErrInvalidWrite
		}
		ref, err := b.allocator.allocate(storeio.PageIndexPosting, b.allocator.pageSize)
		if err != nil {
			return err
		}
		for position := first; position < last; position++ {
			b.indexRows = append(b.indexRows, storeio.IndexDirectoryEntry{
				Key: b.masks[position].key,
				Posting: storeio.IndexPostingRef{
					Page: ref, Segment: uint16(position - first),
					Flags: storeio.IndexPostingImmutableBase,
				},
			})
		}
		b.postings = append(b.postings, fileStoreBulkPostingPlan{
			indexID: indexID, first: first, last: last, ref: ref,
		})
		first = last
	}
	return nil
}

// planIndexGroups condenses scalar single-column exact indexes into linked,
// bounded aggregate-only pages. It reuses posting certificates and never adds
// per-row storage. High cardinality grows the number of pages, not one giant
// extent; only a single representative that cannot fit MaxPageSize declines.
// Containers and uncertified collisions retain the streaming exact-index
// execution path.
func (b *fileStoreBulkBuild) planIndexGroups() error {
	if len(b.rows) == 0 || len(b.options.indexes) == 0 {
		return nil
	}
	b.indexCertificates = slices.Grow(b.indexCertificates, 4*len(b.options.indexes))
	maskAt := 0
	for indexID, exact := range b.options.indexes {
		for maskAt < len(b.masks) && b.masks[maskAt].key.IndexID < uint32(indexID) {
			maskAt++
		}
		firstMask := maskAt
		for maskAt < len(b.masks) && b.masks[maskAt].key.IndexID == uint32(indexID) {
			maskAt++
		}
		if exact == nil || exact.n != 1 ||
			b.indexGroupBlocked&(uint64(1)<<uint(indexID)) != 0 {
			continue
		}

		entryStart := len(b.indexGroupEntries)
		eligible := true
		indexedRows := uint64(0)
		var (
			haveHash  bool
			hash      uint64
			hashStart int
		)
		for position := firstMask; position < maskAt; position++ {
			mask := b.masks[position]
			if mask.collision || mask.certLength == 0 {
				eligible = false
				break
			}
			certificate := b.indexCertificates[mask.certStart : mask.certStart+uint32(mask.certLength) : mask.certStart+uint32(mask.certLength)]
			if !fileIndexCertificateValid(certificate, 1) {
				return storeio.ErrInvalidWrite
			}
			if !haveHash || hash != mask.key.TupleHash {
				haveHash = true
				hash = mask.key.TupleHash
				hashStart = len(b.indexGroupEntries)
			}
			group := -1
			for candidate := hashStart; candidate < len(b.indexGroupEntries); candidate++ {
				if fileIndexCertificatesEqual(
					b.indexGroupEntries[candidate].Value, certificate, 1,
				) {
					group = candidate
					break
				}
			}
			rows := uint64(bits.OnesCount64(mask.bits))
			if rows == 0 || indexedRows > ^uint64(0)-rows {
				return storeio.ErrInvalidWrite
			}
			first := uint64(mask.key.Chunk)<<6 |
				uint64(bits.TrailingZeros64(mask.bits))
			if group < 0 {
				b.indexGroupEntries = append(
					b.indexGroupEntries,
					storeio.IndexGroupCatalogEntry{
						IndexID: uint32(indexID), Value: certificate,
						Count: rows, First: first,
					},
				)
			} else {
				entry := &b.indexGroupEntries[group]
				if entry.Count > ^uint64(0)-rows {
					return storeio.ErrInvalidWrite
				}
				entry.Count += rows
				entry.First = min(entry.First, first)
			}
			indexedRows += rows
		}
		missing := b.indexGroupMissing[indexID]
		if !eligible || indexedRows > uint64(len(b.rows)) ||
			missing != uint64(len(b.rows))-indexedRows {
			b.indexGroupEntries = b.indexGroupEntries[:entryStart]
			continue
		}
		if missing != 0 {
			group := -1
			for candidate := entryStart; candidate < len(b.indexGroupEntries); candidate++ {
				if fileIndexCertificatesEqual(
					b.indexGroupEntries[candidate].Value, []byte("null"), 1,
				) {
					group = candidate
					break
				}
			}
			if group < 0 {
				start := len(b.indexCertificates)
				b.indexCertificates = append(b.indexCertificates, "null"...)
				b.indexGroupEntries = append(
					b.indexGroupEntries,
					storeio.IndexGroupCatalogEntry{
						IndexID: uint32(indexID),
						Value:   b.indexCertificates[start : start+4 : start+4],
						Count:   missing, First: b.indexGroupFirst[indexID],
					},
				)
			} else {
				entry := &b.indexGroupEntries[group]
				entry.Count += missing
				entry.First = min(entry.First, b.indexGroupFirst[indexID])
			}
		}

		for _, entry := range b.indexGroupEntries[entryStart:] {
			size, err := storeio.IndexGroupCatalogEntryEncodedSize(entry)
			if err != nil ||
				storeio.PageHeaderSize+storeio.PageTrailerSize+
					storeio.SegmentedIndexGroupCatalogPayloadHeaderSize+
					size > b.options.MaxPageSize {
				eligible = false
				break
			}
		}
		if !eligible {
			b.indexGroupEntries = b.indexGroupEntries[:entryStart]
			continue
		}
		b.indexGroupCovered |= uint64(1) << uint(indexID)
	}
	if b.indexGroupCovered == 0 {
		return nil
	}
	payloadLimit := b.options.MaxPageSize -
		storeio.PageHeaderSize - storeio.PageTrailerSize
	for first := 0; first < len(b.indexGroupEntries); {
		used := storeio.SegmentedIndexGroupCatalogPayloadHeaderSize
		last := first
		for last < len(b.indexGroupEntries) {
			size, err := storeio.IndexGroupCatalogEntryEncodedSize(
				b.indexGroupEntries[last],
			)
			if err != nil {
				return err
			}
			if used > payloadLimit-size {
				break
			}
			used += size
			last++
		}
		if last == first {
			return storeio.ErrInvalidWrite
		}
		size, ok := fileStoreBulkExtent(
			storeio.PageHeaderSize+storeio.PageTrailerSize+used,
			b.options.PageSize, b.options.MaxPageSize,
		)
		if !ok {
			return storeio.ErrInvalidWrite
		}
		ref, err := b.allocator.allocate(
			storeio.PageIndexGroupCatalog, size,
		)
		if err != nil {
			return err
		}
		if len(b.indexGroupCatalogs) != 0 {
			b.indexGroupCatalogs[len(b.indexGroupCatalogs)-1].next = ref
		}
		b.indexGroupCatalogs = append(
			b.indexGroupCatalogs,
			fileStoreBulkIndexGroupPlan{
				first: first, last: last, ref: ref,
			},
		)
		first = last
	}
	b.indexGroupRef = b.indexGroupCatalogs[0].ref
	return nil
}

// fileStoreBulkTupleHash extracts directly from compact Store chunks. It
// avoids widening shape tapes into one cached classic Index per row while
// producing the same process-independent hash used by FileStore probes.
func fileStoreBulkTupleHash(exact *storeExactIndex, chunk *storeChunk, slot int, textScratch []byte) (uint64, bool, [StoreIndexMaxColumns]RawValue, []byte, error) {
	var values [StoreIndexMaxColumns]RawValue
	if !storeIndexExtractValues(chunk, slot, exact, &values) {
		return 0, false, values, textScratch, nil
	}
	hash := uint64(14695981039346656037)
	for _, raw := range values[:exact.n] {
		hash = fileIndexHashBytes(hash, []byte{byte(raw.Kind()), 0xff})
		switch raw.Kind() {
		case document.Null:
		case document.Bool:
			value, _ := raw.Bool()
			if value {
				hash = fileIndexHashBytes(hash, []byte{1})
			} else {
				hash = fileIndexHashBytes(hash, []byte{0})
			}
		case document.Number:
			if value, ok := raw.Float64(); ok {
				if value == 0 {
					value = 0
				}
				var encoded [8]byte
				binary.LittleEndian.PutUint64(encoded[:], math.Float64bits(value))
				hash = fileIndexHashBytes(hash, encoded[:])
			} else {
				hash = fileIndexHashBytes(hash, []byte{0x7f})
			}
		case document.String:
			if text, clean := raw.StringBytes(); clean {
				hash = fileIndexHashBytes(hash, text)
			} else {
				text, ok, err := raw.AppendText(textScratch[:0])
				if err != nil || !ok {
					return 0, false, values, textScratch, err
				}
				textScratch = text
				hash = fileIndexHashBytes(hash, text)
			}
		default:
			return 0, false, values, textScratch, nil
		}
		hash = fileIndexHashBytes(hash, []byte{0xfe})
	}
	return hash, true, values, textScratch, nil
}

func compareFileStoreBulkIndexKey(a, b storeio.IndexDirectoryKey) int {
	if a.IndexID < b.IndexID {
		return -1
	}
	if a.IndexID > b.IndexID {
		return 1
	}
	if a.TupleHash < b.TupleHash {
		return -1
	}
	if a.TupleHash > b.TupleHash {
		return 1
	}
	if a.Chunk < b.Chunk {
		return -1
	}
	if a.Chunk > b.Chunk {
		return 1
	}
	return 0
}

func (b *fileStoreBulkBuild) planIndexTree() error {
	if len(b.indexRows) == 0 {
		return nil
	}
	leafCapacity := (b.options.PageSize - storeio.PageHeaderSize - storeio.PageTrailerSize -
		storeio.IndexDirectoryPayloadHeaderSize) / storeio.IndexDirectoryLeafRecordSize
	branchCapacity := min(64, (b.options.PageSize-storeio.PageHeaderSize-storeio.PageTrailerSize-
		storeio.IndexDirectoryPayloadHeaderSize)/storeio.IndexDirectoryBranchRecordSize)
	if leafCapacity < 1 || branchCapacity < 2 {
		return storeio.ErrInvalidWrite
	}
	levelStart := 0
	for first := 0; first < len(b.indexRows); first += leafCapacity {
		last := min(first+leafCapacity, len(b.indexRows))
		ref, err := b.allocator.allocate(storeio.PageIndexDirectory, b.allocator.pageSize)
		if err != nil {
			return err
		}
		b.indexes = append(b.indexes, fileStoreBulkIndexPlan{first: first, last: last, ref: ref})
	}
	levelEnd := len(b.indexes)
	for level := uint8(1); levelEnd-levelStart > 1; level++ {
		if level > 10 {
			return storeio.ErrIndexTreeDepth
		}
		nextStart := len(b.indexes)
		for first := levelStart; first < levelEnd; first += branchCapacity {
			last := min(first+branchCapacity, levelEnd)
			children := make([]storeio.IndexDirectoryChild, last-first)
			for i := first; i < last; i++ {
				children[i-first] = storeio.IndexDirectoryChild{
					Lower: b.indexPlanLower(b.indexes[i]), Ref: b.indexes[i].ref,
				}
			}
			ref, err := b.allocator.allocate(storeio.PageIndexDirectory, b.allocator.pageSize)
			if err != nil {
				return err
			}
			b.indexes = append(b.indexes, fileStoreBulkIndexPlan{
				level: level, children: children, ref: ref,
			})
		}
		levelStart, levelEnd = nextStart, len(b.indexes)
	}
	b.indexRoot = b.indexes[levelStart].ref
	return nil
}

func (b *fileStoreBulkBuild) indexPlanLower(plan fileStoreBulkIndexPlan) storeio.IndexDirectoryKey {
	if plan.level == 0 {
		return b.indexRows[plan.first].Key
	}
	return plan.children[0].Lower
}

func (b *fileStoreBulkBuild) planTTLTree() error {
	for row := range b.rows {
		location := b.targetLocation(row)
		if location.Deadline == 0 {
			continue
		}
		b.ttlRows = append(b.ttlRows, storeio.TTLKey{
			Deadline: location.Deadline, Chunk: location.Chunk, Slot: location.Slot,
		})
	}
	if len(b.ttlRows) == 0 {
		return nil
	}
	slices.SortFunc(b.ttlRows, func(a, c storeio.TTLKey) int {
		if a.Deadline < c.Deadline {
			return -1
		}
		if a.Deadline > c.Deadline {
			return 1
		}
		if a.Chunk < c.Chunk {
			return -1
		}
		if a.Chunk > c.Chunk {
			return 1
		}
		return int(a.Slot) - int(c.Slot)
	})
	leafCapacity := (b.options.PageSize - storeio.PageHeaderSize - storeio.PageTrailerSize -
		storeio.TTLDirectoryPayloadHeaderSize) / storeio.TTLDirectoryLeafRecordSize
	branchCapacity := min(64, (b.options.PageSize-storeio.PageHeaderSize-storeio.PageTrailerSize-
		storeio.TTLDirectoryPayloadHeaderSize)/storeio.TTLDirectoryBranchRecordSize)
	if leafCapacity < 1 || branchCapacity < 2 {
		return storeio.ErrInvalidWrite
	}
	levelStart := 0
	for first := 0; first < len(b.ttlRows); first += leafCapacity {
		last := min(first+leafCapacity, len(b.ttlRows))
		ref, err := b.allocator.allocate(storeio.PageTTLDirectory, b.allocator.pageSize)
		if err != nil {
			return err
		}
		b.ttls = append(b.ttls, fileStoreBulkTTLPlan{first: first, last: last, ref: ref})
	}
	levelEnd := len(b.ttls)
	for level := uint8(1); levelEnd-levelStart > 1; level++ {
		if level > 10 {
			return storeio.ErrTTLTreeDepth
		}
		nextStart := len(b.ttls)
		for first := levelStart; first < levelEnd; first += branchCapacity {
			last := min(first+branchCapacity, levelEnd)
			children := make([]storeio.TTLDirectoryChild, last-first)
			for i := first; i < last; i++ {
				children[i-first] = storeio.TTLDirectoryChild{
					Lower: b.ttlPlanLower(b.ttls[i]), Ref: b.ttls[i].ref,
				}
			}
			ref, err := b.allocator.allocate(storeio.PageTTLDirectory, b.allocator.pageSize)
			if err != nil {
				return err
			}
			b.ttls = append(b.ttls, fileStoreBulkTTLPlan{
				level: level, children: children, ref: ref,
			})
		}
		levelStart, levelEnd = nextStart, len(b.ttls)
	}
	b.ttlRoot = b.ttls[levelStart].ref
	return nil
}

func (b *fileStoreBulkBuild) ttlPlanLower(plan fileStoreBulkTTLPlan) storeio.TTLKey {
	if plan.level == 0 {
		return b.ttlRows[plan.first]
	}
	return plan.children[0].Lower
}

func (b *fileStoreBulkBuild) write(file *os.File) error {
	if err := file.Truncate(int64(b.fileEnd)); err != nil {
		return err
	}
	scratch := make([]byte, b.options.MaxPageSize)
	if err := b.writeOverflowPages(file, scratch); err != nil {
		return err
	}
	if err := b.writeDocumentPages(file, scratch); err != nil {
		return err
	}
	if err := b.writeFloat64StripePages(file, scratch); err != nil {
		return err
	}
	if err := b.writeFloat64DirectoryPages(file, scratch); err != nil {
		return err
	}
	if err := b.writeIndexGroupCatalogPages(file, scratch); err != nil {
		return err
	}
	for _, plan := range b.chunks {
		page, err := storeio.EncodeChunkDirectoryPage(scratch[:b.options.PageSize], storeio.ChunkDirectoryHeader{
			StoreID: b.storeID, Generation: b.allocator.generation, LogicalID: plan.ref.LogicalID,
			PageSize: b.allocator.pageSize, Prefix: plan.prefix, Bitmap: plan.bitmap, Shift: plan.shift,
		}, plan.children, b.fileEnd, b.allocator.nextLogical)
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	if err := b.writeKeyPages(file, scratch); err != nil {
		return err
	}
	if err := b.writePostingPages(file, scratch); err != nil {
		return err
	}
	if err := b.writeIndexPages(file, scratch); err != nil {
		return err
	}
	if err := b.writeTTLPages(file, scratch); err != nil {
		return err
	}
	statePage, err := storeio.EncodeStateRootPage(scratch[:b.options.PageSize], b.root, b.fileEnd)
	if err != nil {
		return err
	}
	if err := writeStorePageAt(file, statePage, b.stateRef.Offset); err != nil {
		return err
	}
	if err := file.Sync(); err != nil {
		return err
	}
	super := storeio.Superblock{
		StoreID: b.storeID, Generation: b.allocator.generation,
		StateOffset: b.stateRef.Offset, StateLength: b.stateRef.Length,
		StateChecksum: storeio.PageChecksum(statePage), FileEnd: b.fileEnd,
		PageSize: b.allocator.pageSize,
	}
	rootPage := scratch[:b.options.PageSize]
	clear(rootPage)
	if _, err := storeio.EncodeSuperblock(rootPage[:storeio.SuperblockSize], super); err != nil {
		return err
	}
	rootOffset, err := storeio.SuperblockOffset(b.allocator.generation, b.allocator.pageSize)
	if err != nil {
		return err
	}
	if err := writeStorePageAt(file, rootPage, uint64(rootOffset)); err != nil {
		return err
	}
	return file.Sync()
}

func (b *fileStoreBulkBuild) writeIndexGroupCatalogPages(
	file *os.File,
	scratch []byte,
) error {
	for _, plan := range b.indexGroupCatalogs {
		header := storeio.IndexGroupCatalogHeader{
			StoreID: b.storeID, Generation: b.allocator.generation,
			LogicalID: plan.ref.LogicalID, PageSize: plan.ref.Length,
			CoveredIndexes: b.indexGroupCovered,
			DocumentCount:  uint64(len(b.rows)), Next: plan.next,
		}
		var page []byte
		var err error
		if len(b.indexGroupCatalogs) == 1 {
			page, err = storeio.EncodeIndexGroupCatalogPage(
				scratch[:plan.ref.Length], header,
				b.indexGroupEntries[plan.first:plan.last],
				uint32(len(b.options.indexes)),
				uint32(len(b.documents)),
				uint32(b.options.Store.ChunkDocuments),
			)
		} else {
			page, err = storeio.EncodeSegmentedIndexGroupCatalogPage(
				scratch[:plan.ref.Length], header,
				b.indexGroupEntries[plan.first:plan.last],
				uint32(len(b.options.indexes)),
				uint32(len(b.documents)),
				uint32(b.options.Store.ChunkDocuments),
				b.fileEnd, b.allocator.nextLogical,
				b.allocator.pageSize,
			)
		}
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *fileStoreBulkBuild) writeOverflowPages(file *os.File, scratch []byte) error {
	for _, plan := range b.overflows {
		_, _, raw := b.sourceRow(plan.row)
		location := b.targetLocation(plan.row)
		page, err := storeio.EncodeOverflowPage(scratch[:b.options.MaxPageSize], storeio.OverflowPageHeader{
			StoreID: b.storeID, Generation: b.allocator.generation, LogicalID: plan.ref.LogicalID,
			PageSize: plan.ref.Length, Chunk: location.Chunk, Slot: location.Slot,
			Total: uint64(len(raw)), Offset: uint64(plan.start), Next: plan.next,
		}, raw[plan.start:plan.end], b.fileEnd, b.allocator.nextLogical,
			b.allocator.pageSize, uint32(len(b.documents)), uint8(b.options.Store.ChunkDocuments))
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *fileStoreBulkBuild) writeDocumentPages(file *os.File, scratch []byte) error {
	var storage [storeMaxChunkDocuments]storeio.DocumentRecord
	masks := make([]uint64, len(b.options.float64Columns))
	values := make([]float64, len(b.options.float64Columns)*64)
	for document := range b.documents {
		plan := b.documents[document]
		if plan.group != 0 {
			group := b.documentGroups[plan.group-1]
			if document != group.first {
				continue
			}
			if group.columnLast > group.columnFirst {
				columnChunks, prepareErr := b.prepareFloat64Group(
					group.columnFirst, group.columnLast,
				)
				if prepareErr != nil {
					return prepareErr
				}
				page, encodeErr := storeio.EncodeFloat64Group(
					scratch[:group.columns.Length],
					storeio.Float64GroupHeader{
						StoreID: b.storeID, Generation: b.allocator.generation,
						LogicalID: group.columns.LogicalID, PageSize: group.columns.Length,
						FirstChunk: columnChunks[0].ChunkID, ChunkCount: uint16(len(columnChunks)),
						RowCount:    uint16(groupRows(columnChunks)),
						ColumnCount: uint16(len(b.options.float64Columns)),
					},
					columnChunks, b.allocator.nextLogical,
				)
				if encodeErr != nil {
					return encodeErr
				}
				if writeErr := writeStorePageAt(file, page, group.columns.Offset); writeErr != nil {
					return writeErr
				}
			}
			chunks, err := b.prepareDocumentGroup(group.first, group.last)
			if err != nil {
				return err
			}
			columnCount := len(b.options.float64Columns)
			if group.columns != (storeio.PageRef{}) {
				clearDocumentGroupColumns(chunks)
				columnCount = 0
			}
			page, err := storeio.EncodeDocumentGroup(
				scratch[:group.ref.Length],
				storeio.DocumentGroupHeader{
					StoreID: b.storeID, Generation: b.allocator.generation,
					LogicalID: group.ref.LogicalID, PageSize: group.ref.Length,
					FirstChunk: chunks[0].ChunkID, ChunkCount: uint16(len(chunks)),
					RowCount:    uint16(groupRows(chunks)),
					ColumnCount: uint16(columnCount), Flags: uint16(group.ref.Flags),
				},
				chunks, b.allocator.nextLogical, &b.groupCodec,
			)
			if err != nil {
				return err
			}
			if err := writeStorePageAt(file, page, group.ref.Offset); err != nil {
				return err
			}
			continue
		}
		rows := storage[:plan.last-plan.first]
		for i := range rows {
			rowIndex := plan.first + i
			_, key, raw := b.sourceRow(rowIndex)
			record := storeio.DocumentRecord{Key: byteview.Bytes(key), Slot: uint8(i)}
			if b.rows[rowIndex].overflowN == 0 {
				record.JSON = raw
			} else {
				record.Overflow = b.overflows[b.rows[rowIndex].overflowBase].ref
				record.JSONLength = uint64(len(raw))
			}
			rows[i] = record
		}
		clear(masks)
		for column := range b.options.float64Columns {
			for i := range rows {
				value, ok, err := b.sourceFloat64(plan.first+i, column)
				if err != nil {
					return err
				}
				if !ok {
					continue
				}
				masks[column] |= uint64(1) << uint(i)
				values[column*64+i] = value
			}
		}
		page, err := storeio.EncodeDocumentPageWithColumns(scratch[:plan.ref.Length], storeio.DocumentPageHeader{
			StoreID: b.storeID, Generation: b.allocator.generation, LogicalID: plan.ref.LogicalID,
			PageSize: plan.ref.Length, ChunkID: plan.chunk, Live: plan.live,
		}, rows, storeio.DocumentFloat64Columns{Masks: masks, Values: values},
			b.allocator.nextLogical, b.fileEnd, b.allocator.pageSize)
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
		clear(rows)
	}
	return nil
}

func (b *fileStoreBulkBuild) writeFloat64DirectoryPages(
	file *os.File,
	scratch []byte,
) error {
	for _, plan := range b.float64Directories {
		header := storeio.Float64DirectoryHeader{
			StoreID: b.storeID, Generation: b.allocator.generation,
			LogicalID: plan.ref.LogicalID, PageSize: plan.ref.Length,
			Level: plan.level,
		}
		var page []byte
		var err error
		if plan.level == 0 {
			page, err = storeio.EncodeFloat64DirectoryLeaf(
				scratch[:plan.ref.Length], header,
				b.float64DirectoryRows[plan.first:plan.last],
				b.fileEnd, b.allocator.nextLogical,
				b.allocator.pageSize,
			)
		} else {
			page, err = storeio.EncodeFloat64DirectoryBranch(
				scratch[:plan.ref.Length], header, plan.children,
				b.fileEnd, b.allocator.nextLogical,
				b.allocator.pageSize,
			)
		}
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *fileStoreBulkBuild) writeFloat64StripePages(file *os.File, scratch []byte) error {
	columns := len(b.options.float64Columns)
	var starts [fileStoreMaxFloat64Columns]int
	var ends [fileStoreMaxFloat64Columns]int
	if cap(b.float64StripeColumns) < columns {
		b.float64StripeColumns = make([]storeio.Float64StripeColumn, columns)
	} else {
		b.float64StripeColumns = b.float64StripeColumns[:columns]
	}
	for _, plan := range b.float64Stripes {
		b.float64StripeValues = b.float64StripeValues[:0]
		if cap(b.float64StripeValues) < int(plan.ref.Length) {
			b.float64StripeValues = make([]byte, 0, plan.ref.Length)
		}
		for column := 0; column < columns; column++ {
			encodingRank := uint8(0)
			for document := plan.first; document < plan.last; document++ {
				encodingRank = max(
					encodingRank, b.float64Encodings[document*columns+column],
				)
			}
			encoding := [...]storeio.Float64GroupEncoding{
				storeio.Float64GroupUint8,
				storeio.Float64GroupUint16,
				storeio.Float64GroupUint32,
				storeio.Float64GroupFloat64LE,
			}[encodingRank]
			starts[column] = len(b.float64StripeValues)
			for document := plan.first; document < plan.last; document++ {
				chunk := b.documents[document]
				for row := chunk.first; row < chunk.last; row++ {
					value, ok, err := b.sourceFloat64(row, column)
					if err != nil {
						return err
					}
					if !ok {
						continue
					}
					switch encoding {
					case storeio.Float64GroupUint8:
						b.float64StripeValues = append(b.float64StripeValues, byte(value))
					case storeio.Float64GroupUint16:
						offset := len(b.float64StripeValues)
						b.float64StripeValues = append(b.float64StripeValues, 0, 0)
						binary.LittleEndian.PutUint16(b.float64StripeValues[offset:], uint16(value))
					case storeio.Float64GroupUint32:
						offset := len(b.float64StripeValues)
						b.float64StripeValues = append(b.float64StripeValues, 0, 0, 0, 0)
						binary.LittleEndian.PutUint32(b.float64StripeValues[offset:], uint32(value))
					default:
						offset := len(b.float64StripeValues)
						b.float64StripeValues = append(b.float64StripeValues, 0, 0, 0, 0, 0, 0, 0, 0)
						binary.LittleEndian.PutUint64(b.float64StripeValues[offset:], math.Float64bits(value))
					}
				}
			}
			ends[column] = len(b.float64StripeValues)
			b.float64StripeColumns[column] = storeio.Float64StripeColumn{
				Encoding: encoding,
			}
		}
		for column := 0; column < columns; column++ {
			b.float64StripeColumns[column].Values =
				b.float64StripeValues[starts[column]:ends[column]:ends[column]]
		}
		page, err := storeio.EncodeFloat64Stripe(
			scratch[:plan.ref.Length],
			storeio.Float64StripeHeader{
				StoreID: b.storeID, Generation: b.allocator.generation,
				LogicalID: plan.ref.LogicalID, PageSize: plan.ref.Length,
				FirstChunk: b.documents[plan.first].chunk,
				ChunkCount: uint32(plan.last - plan.first), RowCount: plan.rows,
				ColumnCount: uint16(columns),
			},
			b.float64StripeColumns, b.allocator.nextLogical,
		)
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func groupRows(chunks []storeio.DocumentGroupChunk) int {
	rows := 0
	for _, chunk := range chunks {
		rows += bits.OnesCount64(chunk.Live)
	}
	return rows
}

func (b *fileStoreBulkBuild) writeKeyPages(file *os.File, scratch []byte) error {
	entries := make([]storeio.KeyDirectoryEntry, 0, 128)
	children := make([]storeio.KeyDirectoryChild, 0, 64)
	for _, plan := range b.keys {
		header := storeio.KeyDirectoryHeader{
			StoreID: b.storeID, Generation: b.allocator.generation,
			LogicalID: plan.ref.LogicalID, PageSize: b.allocator.pageSize, Level: plan.level,
		}
		var page []byte
		var err error
		if plan.level == 0 {
			entries = entries[:0]
			for _, row := range b.keyOrder[plan.first:plan.last] {
				_, key, _ := b.sourceRow(row)
				entries = append(entries, storeio.KeyDirectoryEntry{
					Key: byteview.Bytes(key), Location: b.targetLocation(row),
				})
			}
			page, err = storeio.EncodeKeyDirectoryLeaf(
				scratch[:b.options.PageSize], header, entries, b.allocator.nextLogical,
				uint32(len(b.documents)), uint8(b.options.Store.ChunkDocuments),
			)
		} else {
			children = children[:0]
			for _, child := range plan.children {
				_, key, _ := b.sourceRow(child.lower)
				children = append(children, storeio.KeyDirectoryChild{
					Lower: byteview.Bytes(key), Ref: child.ref,
				})
			}
			page, err = storeio.EncodeKeyDirectoryBranch(
				scratch[:b.options.PageSize], header, children, b.fileEnd, b.allocator.nextLogical,
			)
		}
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *fileStoreBulkBuild) writePostingPages(file *os.File, scratch []byte) error {
	entries := make([]storeio.PostingEntry, 0, 80)
	segments := make([]storeio.PostingSegment, 0, 80)
	for _, plan := range b.postings {
		count := plan.last - plan.first
		if cap(entries) < count {
			entries = make([]storeio.PostingEntry, count)
		} else {
			entries = entries[:count]
		}
		if cap(segments) < count {
			segments = make([]storeio.PostingSegment, count)
		} else {
			segments = segments[:count]
		}
		for i := 0; i < count; i++ {
			mask := b.masks[plan.first+i]
			entries[i] = storeio.PostingEntry{Chunk: mask.key.Chunk, Bits: mask.bits}
			certificate := b.indexCertificates[mask.certStart : mask.certStart+uint32(mask.certLength) : mask.certStart+uint32(mask.certLength)]
			flags := uint16(0)
			if mask.collision {
				flags |= storeio.PostingSegmentCollision
			}
			segments[i] = storeio.PostingSegment{
				StreamID: uint32(i + 1), TupleHash: mask.key.TupleHash,
				Flags: flags, Certificate: certificate, Entries: entries[i : i+1],
			}
		}
		page, err := storeio.EncodePostingPage(scratch[:b.options.PageSize], storeio.PostingPageHeader{
			StoreID: b.storeID, Generation: b.allocator.generation, LogicalID: plan.ref.LogicalID,
			PageSize: b.allocator.pageSize, IndexID: plan.indexID,
		}, segments, b.allocator.nextLogical, uint32(len(b.options.indexes)))
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
		clear(entries)
		clear(segments)
	}
	return nil
}

func (b *fileStoreBulkBuild) writeIndexPages(file *os.File, scratch []byte) error {
	for _, plan := range b.indexes {
		header := storeio.IndexDirectoryHeader{
			StoreID: b.storeID, Generation: b.allocator.generation,
			LogicalID: plan.ref.LogicalID, PageSize: b.allocator.pageSize, Level: plan.level,
		}
		var page []byte
		var err error
		if plan.level == 0 {
			page, err = storeio.EncodeIndexDirectoryLeaf(
				scratch[:b.options.PageSize], header, b.indexRows[plan.first:plan.last],
				b.fileEnd, b.allocator.nextLogical, uint32(len(b.options.indexes)),
			)
		} else {
			page, err = storeio.EncodeIndexDirectoryBranch(
				scratch[:b.options.PageSize], header, plan.children, b.fileEnd, b.allocator.nextLogical,
			)
		}
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}

func (b *fileStoreBulkBuild) writeTTLPages(file *os.File, scratch []byte) error {
	for _, plan := range b.ttls {
		header := storeio.TTLDirectoryHeader{
			StoreID: b.storeID, Generation: b.allocator.generation,
			LogicalID: plan.ref.LogicalID, PageSize: b.allocator.pageSize, Level: plan.level,
		}
		var page []byte
		var err error
		if plan.level == 0 {
			page, err = storeio.EncodeTTLDirectoryLeaf(
				scratch[:b.options.PageSize], header, b.ttlRows[plan.first:plan.last],
				b.allocator.nextLogical, uint32(len(b.documents)), uint8(b.options.Store.ChunkDocuments),
			)
		} else {
			page, err = storeio.EncodeTTLDirectoryBranch(
				scratch[:b.options.PageSize], header, plan.children, b.fileEnd, b.allocator.nextLogical,
			)
		}
		if err != nil {
			return err
		}
		if err := writeStorePageAt(file, page, plan.ref.Offset); err != nil {
			return err
		}
	}
	return nil
}
