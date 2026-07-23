package simdjson

import (
	"encoding/binary"
	"math"
	"math/bits"

	"github.com/thesyncim/simdjson/internal/storeio"
)

// fileFloat64ProjectionEqual reports whether replacing one live document can
// reuse the immutable clean scan stripes byte-for-byte. It compares the exact
// accepted typed projection, not JSON spelling: missing, non-numeric, and
// non-finite values are all absent, while accepted values retain signed-zero
// bits. Reusing the old head adds no overlay branch to the read hot path.
func (s *FileStore) fileFloat64ProjectionEqual(
	oldIndex, newIndex *Index,
) (bool, error) {
	if len(s.options.float64Columns) == 0 ||
		oldIndex == nil || newIndex == nil {
		return false, nil
	}
	for _, column := range s.options.float64Columns {
		oldValue, oldPresent, err := fileFloat64ProjectionValue(
			*oldIndex, column.pointer,
		)
		if err != nil {
			return false, err
		}
		newValue, newPresent, err := fileFloat64ProjectionValue(
			*newIndex, column.pointer,
		)
		if err != nil {
			return false, err
		}
		if oldPresent != newPresent ||
			oldPresent &&
				math.Float64bits(oldValue) != math.Float64bits(newValue) {
			return false, nil
		}
	}
	return true, nil
}

func fileFloat64ProjectionValue(
	index Index,
	pointer CompiledPointer,
) (float64, bool, error) {
	node, found, err := index.PointerCompiled(pointer)
	if err != nil || !found {
		return 0, false, err
	}
	value, ok := node.Raw().Float64()
	if !ok || math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, false, nil
	}
	return value, true, nil
}

// maintainFileFloat64Scan keeps the dense scan chain across an existing-row
// update, delete, or in-range insert. Projection-neutral updates reuse the old
// directory without writing metadata. A changed projection rebuilds only the
// stripe containing the touched chunk and its bounded ordered-directory path;
// every other stripe and subtree remains shared. An insert that extends beyond
// existing stripe coverage declines to the authoritative chunk-tree scan.
//
// retireWhole is true only when the caller must clear and retire the complete
// old projection. Successful micro-rebuilds append their replaced stripe and
// directory path to retireScratch directly.
func (s *FileStore) maintainFileFloat64Scan(
	tx *storeio.WriteTransaction,
	state *fileStoreState,
	chunkRoot storeio.PageRef,
	location storeio.KeyLocation,
	oldIndex, newIndex *Index,
	created bool,
) (head storeio.PageRef, retireWhole bool, err error) {
	oldHead := state.root.Float64ScanHead
	if oldHead == (storeio.PageRef{}) {
		return storeio.PageRef{}, false, nil
	}
	if !created && newIndex != nil {
		equal, equalErr := s.fileFloat64ProjectionEqual(
			oldIndex, newIndex,
		)
		if equalErr != nil {
			return storeio.PageRef{}, false, equalErr
		}
		if equal {
			return oldHead, false, nil
		}
	}
	head, rebuilt, err := s.rebuildFileFloat64Stripe(
		tx, state, chunkRoot, location.Chunk,
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	if !rebuilt {
		return storeio.PageRef{}, true, nil
	}
	return head, false, nil
}

func (s *FileStore) rebuildFileFloat64Stripe(
	tx *storeio.WriteTransaction,
	state *fileStoreState,
	chunkRoot storeio.PageRef,
	target uint32,
) (storeio.PageRef, bool, error) {
	head := state.root.Float64ScanHead
	entry, found, err := storeio.LookupFloat64Directory(
		s.cache, head, target,
		storeio.Float64DirectoryBounds{
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
		uint32(s.options.PageSize),
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	if !found {
		return storeio.PageRef{}, false, nil
	}
	oldStripe := entry.Ref
	stripeLease, err := s.cache.Acquire(oldStripe)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	stripeHeader := storeio.AdmittedFloat64Stripe(
		stripeLease.Page(),
	).Header()
	stripeLease.Release()
	if stripeHeader.FirstChunk != entry.FirstChunk ||
		target < stripeHeader.FirstChunk ||
		uint64(target) >=
			uint64(stripeHeader.FirstChunk)+
				uint64(stripeHeader.ChunkCount) {
		return storeio.PageRef{}, false, nil
	}

	nextState := *state
	nextState.chunkRoot = chunkRoot
	nextState.root.ChunkDirectory = chunkRoot
	nextState.root.NextLogicalID = tx.NextLogicalID()
	nextState.super.FileEnd = tx.FileEnd()
	var ranks [fileStoreMaxFloat64Columns]uint8
	var counts [fileStoreMaxFloat64Columns]uint32
	rows, err := s.visitFileFloat64StripeRange(
		&nextState, stripeHeader.FirstChunk, stripeHeader.ChunkCount,
		func(column int, value float64) error {
			if counts[column] == ^uint32(0) {
				return ErrStoreTooLarge
			}
			counts[column]++
			ranks[column] = max(
				ranks[column], fileStoreFloat64Encoding(value),
			)
			return nil
		},
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	if rows == 0 || rows > uint64(^uint32(0)) {
		return storeio.PageRef{}, false, nil
	}

	columns := len(s.options.float64Columns)
	var starts [fileStoreMaxFloat64Columns]int
	var cursors [fileStoreMaxFloat64Columns]int
	var ends [fileStoreMaxFloat64Columns]int
	var encodings [fileStoreMaxFloat64Columns]storeio.Float64GroupEncoding
	dataBytes := 0
	for column := 0; column < columns; column++ {
		encoding := fileFloat64StripeEncoding(ranks[column])
		width := encoding.ByteWidth()
		bytes := uint64(counts[column]) * uint64(width)
		if bytes > uint64(s.options.MaxPageSize) ||
			dataBytes > s.options.MaxPageSize-int(bytes) {
			return storeio.PageRef{}, false, nil
		}
		encodings[column] = encoding
		starts[column] = dataBytes
		cursors[column] = dataBytes
		dataBytes += int(bytes)
		ends[column] = dataBytes
	}
	required := storeio.PageHeaderSize + storeio.PageTrailerSize +
		storeio.Float64StripePayloadHeaderSize +
		columns*storeio.Float64StripeColumnSize + dataBytes
	pageSize, ok := fileStoreBulkExtent(
		required, s.options.PageSize, s.options.MaxPageSize,
	)
	if !ok {
		return storeio.PageRef{}, false, nil
	}
	if cap(s.float64StripeBytes) < dataBytes {
		s.float64StripeBytes = make([]byte, dataBytes)
	} else {
		s.float64StripeBytes = s.float64StripeBytes[:dataBytes]
	}
	_, err = s.visitFileFloat64StripeRange(
		&nextState, stripeHeader.FirstChunk, stripeHeader.ChunkCount,
		func(column int, value float64) error {
			cursor := cursors[column]
			switch encodings[column] {
			case storeio.Float64GroupUint8:
				s.float64StripeBytes[cursor] = byte(value)
				cursor++
			case storeio.Float64GroupUint16:
				binary.LittleEndian.PutUint16(
					s.float64StripeBytes[cursor:cursor+2], uint16(value),
				)
				cursor += 2
			case storeio.Float64GroupUint32:
				binary.LittleEndian.PutUint32(
					s.float64StripeBytes[cursor:cursor+4], uint32(value),
				)
				cursor += 4
			default:
				binary.LittleEndian.PutUint64(
					s.float64StripeBytes[cursor:cursor+8],
					math.Float64bits(value),
				)
				cursor += 8
			}
			cursors[column] = cursor
			return nil
		},
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	for column := 0; column < columns; column++ {
		if cursors[column] != ends[column] {
			return storeio.PageRef{}, false, storeio.ErrFloat64StripeCorrupt
		}
	}
	if cap(s.float64StripeColumns) < columns {
		s.float64StripeColumns = make(
			[]storeio.Float64StripeColumn, columns,
		)
	} else {
		s.float64StripeColumns = s.float64StripeColumns[:columns]
	}
	for column := 0; column < columns; column++ {
		s.float64StripeColumns[column] = storeio.Float64StripeColumn{
			Encoding: encodings[column],
			Values:   s.float64StripeBytes[starts[column]:ends[column]:ends[column]],
		}
	}

	stripePage, err := tx.Allocate(
		storeio.PageFloat64Stripe, pageSize, oldStripe.LogicalID,
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	if _, err := storeio.EncodeFloat64Stripe(
		stripePage.Bytes(),
		storeio.Float64StripeHeader{
			StoreID: s.storeID, Generation: tx.Generation(),
			LogicalID:  stripePage.Ref().LogicalID,
			PageSize:   stripePage.Ref().Length,
			FirstChunk: stripeHeader.FirstChunk,
			ChunkCount: stripeHeader.ChunkCount,
			RowCount:   uint32(rows), ColumnCount: uint16(columns),
		},
		s.float64StripeColumns, tx.NextLogicalID(),
	); err != nil {
		return storeio.PageRef{}, false, err
	}
	if err := stripePage.Stage(); err != nil {
		return storeio.PageRef{}, false, err
	}
	directory, err := storeio.ReplaceFloat64Directory(
		s.cache, tx, head, stripeHeader.FirstChunk, stripePage.Ref(),
		storeio.Float64DirectoryBounds{
			FileEnd:       state.super.FileEnd,
			NextLogicalID: state.root.NextLogicalID,
		},
	)
	if err != nil {
		return storeio.PageRef{}, false, err
	}
	if !directory.Found || !directory.Changed ||
		directory.Root == (storeio.PageRef{}) {
		return storeio.PageRef{}, false,
			storeio.ErrFloat64CatalogCorrupt
	}
	if err := s.appendIndexRetiredRef(state, oldStripe); err != nil {
		return storeio.PageRef{}, false, err
	}
	for i := range int(directory.RetiredCount) {
		if err := s.appendIndexRetiredRef(
			state, directory.Retired[i],
		); err != nil {
			return storeio.PageRef{}, false, err
		}
	}
	return directory.Root, true, nil
}

func (s *FileStore) visitFileFloat64StripeRange(
	state *fileStoreState,
	first, count uint32,
	fn func(column int, value float64) error,
) (uint64, error) {
	var rows uint64
	for ordinal := uint32(0); ordinal < count; ordinal++ {
		chunk := first + ordinal
		_, view, leases, err := s.loadFileChunk(state, chunk)
		if err != nil {
			return 0, err
		}
		if view == nil {
			continue
		}
		if view.float64ColumnCount() != len(s.options.float64Columns) {
			leases.Release()
			return 0, storeio.ErrFloat64StripeCorrupt
		}
		rows += uint64(bits.OnesCount64(view.live()))
		for column := range s.options.float64Columns {
			values, ok := view.float64Column(column)
			if !ok {
				leases.Release()
				return 0, storeio.ErrFloat64StripeCorrupt
			}
			iterator := values.Values()
			for {
				value, present := iterator.Next()
				if !present {
					break
				}
				if err := fn(column, value); err != nil {
					leases.Release()
					return 0, err
				}
			}
		}
		leases.Release()
	}
	return rows, nil
}

func fileFloat64StripeEncoding(
	rank uint8,
) storeio.Float64GroupEncoding {
	switch rank {
	case 0:
		return storeio.Float64GroupUint8
	case 1:
		return storeio.Float64GroupUint16
	case 2:
		return storeio.Float64GroupUint32
	default:
		return storeio.Float64GroupFloat64LE
	}
}
