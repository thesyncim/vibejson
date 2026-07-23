package slopjson

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"github.com/thesyncim/slopjson/internal/storemem"
)

// compactDocuments moves a completed builder's exact source bytes and
// pointer-free structural arrays into one owned external block. Chunks retain
// only packed row ordinals into one descriptor owner and one shared immutable
// shape table; no source, Index, shape-ref, or narrow-value object remains per
// document on the Go heap.
func (b *StoreBuilder) compactDocuments(state *storeState) error {
	if b.count == 0 {
		return nil
	}
	var templates []*storeDocumentTemplate
	var rowLayouts []storeOwnedRowLayout
	var dataBytes int
	var err error
	// Value-dictionary references are source offsets and compose with templates.
	// Wildcard postings still verify their classic remainder through Root, so
	// those builds retain classic nested tapes until that verifier is native.
	if !b.options.Postings {
		templates, rowLayouts, dataBytes, err = buildStoreDocumentTemplates(state.chunks, b.count)
	} else {
		dataBytes, err = storeOwnedDocumentDataBytes(state.chunks)
	}
	if err != nil {
		return err
	}
	layout := storeOwnedDocRefGeneral
	if storeCanUseOwnedDocRefs(state.chunks, len(b.shapes)) {
		layout = storeOwnedDocRefWide
		if storeCanUseCompactDocRefs(state.chunks, len(b.shapes)) {
			layout = storeOwnedDocRefCompact
		}
	}
	owned, err := newStoreOwnedDocuments(b.count, dataBytes, layout)
	if err != nil {
		return fmt.Errorf("slopjson: compact StoreBuilder documents: %w", err)
	}
	shapeIDs := make(map[*shapeRecord]uint32, len(b.shapes))
	for id, rec := range b.shapes {
		shapeIDs[rec] = uint32(id)
	}

	data := owned.sourceBlock.Bytes()
	position, rowBase := 0, uint64(0)
	valid := true
	state.chunks.each(func(_ uint32, chunk *storeChunk) bool {
		docs := &chunk.docs
		narrow := 0
		for i := 0; i < docs.Len(); i++ {
			index := docs.docAt(i)
			ref := docs.shapeTapeRefAt(i)
			rowIndex := int(rowBase) + i
			narrowKind, narrowWidth := uint8(0), uint64(0)
			if len(rowLayouts) != 0 {
				narrowKind = rowLayouts[rowIndex].kind()
				if ref.narrow {
					narrowWidth = storeOwnedNarrowStorageWidth(narrowKind)
				}
			} else {
				narrowKind, narrowWidth = storeOwnedNarrowStorage(docs, i, ref)
			}
			rootStart, rootEnd := ref.start, ref.end
			templateID, templated := uint32(0), false
			if len(rowLayouts) != 0 {
				templateID, templated = rowLayouts[rowIndex].template()
			}
			start := position
			position += copy(data[position:], index.src)
			kind, count, shapeID := uint8(persistDocClassic), len(index.entries), storeMappedNoShape
			switch {
			case templated:
				kind = rowLayouts[rowIndex].kind()
				shapeID = templateID
				rootStart, rootEnd = index.entries[0].start, index.entries[0].end
				position = int(storeMappedTapeOffset(uint64(position), kind))
				template := templates[templateID]
				width := int(storeOwnedTemplateSpanWidth(kind))
				for ordinal := range index.entries {
					spanIndex := template.spanIndex[ordinal]
					if spanIndex == ^uint16(0) {
						continue
					}
					entry := &index.entries[ordinal]
					off := position + int(spanIndex)*width
					switch kind {
					case storeOwnedDocTemplate8:
						data[off], data[off+1] = byte(entry.start), byte(entry.end)
					case storeOwnedDocTemplateLength8:
						binary.LittleEndian.PutUint16(data[off:off+2], uint16(entry.start))
						data[off+2] = byte(entry.end - entry.start)
					default:
						binary.LittleEndian.PutUint32(data[off:off+storeOwnedTemplateSpanLen],
							entry.start|entry.end<<16)
					}
				}
				position += int(template.spanCount) * width
				position = int(persistAlign8(uint64(position)))
			case ref.rec == nil:
				position = int(storeMappedTapeOffset(uint64(position), kind))
				copyStoreOwnedEntries(data[position:], index.entries)
				position += len(index.entries) * int(unsafe.Sizeof(IndexEntry{}))
			case ref.narrow:
				kind, count = narrowKind, len(ref.rec.fields)
				id, ok := shapeIDs[ref.rec]
				if !ok {
					valid = false
					return false
				}
				shapeID = id
				position = int(storeMappedTapeOffset(uint64(position), kind))
				width := int(narrowWidth)
				for ordinal := 0; ordinal < count; ordinal++ {
					value := docs.narrowAt(i, ref, ordinal)
					if value.info&infoCountMask != 0 || value.info>>infoKindShift > uint32(^uint8(0)) {
						valid = false
						return false
					}
					off := position + ordinal*width
					switch kind {
					case storeOwnedDocNarrow8:
						data[off], data[off+1] = byte(value.span), byte(value.span>>16)
						data[off+2] = byte(value.info >> infoKindShift)
					case storeOwnedDocNarrow9:
						start, end := value.span&0xffff, value.span>>16
						packed := start | (end-start)<<9 | (value.info>>infoKindShift)<<17
						data[off], data[off+1], data[off+2] = byte(packed), byte(packed>>8), byte(packed>>16)
					case storeOwnedDocNarrowLength8:
						start, end := value.span&0xffff, value.span>>16
						binary.LittleEndian.PutUint16(data[off:off+2], uint16(start))
						data[off+2] = byte(end - start)
						data[off+3] = byte(value.info >> infoKindShift)
					default:
						binary.LittleEndian.PutUint32(data[off:off+4], value.span)
						data[off+4] = byte(value.info >> infoKindShift)
					}
				}
				position += count * width
				narrow += count
			default:
				kind = persistDocWide
				id, ok := shapeIDs[ref.rec]
				if !ok {
					valid = false
					return false
				}
				shapeID = id
				position = int(storeMappedTapeOffset(uint64(position), kind))
				copyStoreOwnedEntries(data[position:], index.entries)
				position += len(index.entries) * int(unsafe.Sizeof(IndexEntry{}))
			}
			owned.setRef(rowBase+uint64(i), storeMappedDocRef{
				sourceOff: uint64(start), srcLen: uint32(len(index.src)),
				entryCount: uint32(count), start: rootStart, end: rootEnd,
				shapeID: shapeID, kind: kind, enriched: ref.enriched,
			})
		}
		docs.docs = nil
		docs.srcChunk = nil
		docs.entryChunk = nil
		docs.scratch = nil
		docs.tapeRefs = nil
		docs.narrow = nil
		docs.shapes = ShapeCache{}
		docs.widened = nil
		docs.source = data
		docs.mappedDocs = owned
		docs.mappedShapes = b.shapes
		docs.mappedBase = rowBase
		docs.mappedCount = int(chunk.count)
		docs.mappedNarrow = narrow
		rowBase += uint64(chunk.count)
		return true
	})
	if !valid || position != len(data) || rowBase != uint64(b.count) {
		owned.release()
		return errors.New("slopjson: StoreBuilder compact document invariant")
	}
	state.mappedDocs = owned
	state.mappedDocChunks = state.chunkCount
	owned.templates = templates
	owned.shapes = b.shapes
	return nil
}

type storeOwnedDocRefLayout uint8

const (
	storeOwnedDocRefGeneral storeOwnedDocRefLayout = iota
	storeOwnedDocRefWide
	storeOwnedDocRefCompact
)

func newStoreOwnedDocuments(count, dataBytes int, layout storeOwnedDocRefLayout) (*storeMappedDocs, error) {
	var owned *storeMappedDocs
	var err error
	switch layout {
	case storeOwnedDocRefCompact:
		if count < 0 || count > maxInt()/storeCompactDocRefBytes {
			return nil, ErrStorePersistTooLarge
		}
		block, allocErr := storemem.Allocate(count * storeCompactDocRefBytes)
		if allocErr != nil {
			return nil, allocErr
		}
		owned = &storeMappedDocs{
			refData:   unsafe.Pointer(unsafe.SliceData(block.Bytes())),
			refStride: unsafe.Sizeof(storeCompactDocRef{}), block: block,
		}
		if count != 0 {
			owned.compactRefs = unsafe.Slice((*storeCompactDocRef)(unsafe.Pointer(unsafe.SliceData(block.Bytes()))), count)
		}
		runtime.SetFinalizer(owned, (*storeMappedDocs).release)
	case storeOwnedDocRefWide:
		if count < 0 || count > maxInt()/storeOwnedDocRefBytes {
			return nil, ErrStorePersistTooLarge
		}
		block, allocErr := storemem.Allocate(count * storeOwnedDocRefBytes)
		if allocErr != nil {
			return nil, allocErr
		}
		owned = &storeMappedDocs{
			refData:   unsafe.Pointer(unsafe.SliceData(block.Bytes())),
			refStride: unsafe.Sizeof(storeOwnedDocRef{}), block: block,
		}
		if count != 0 {
			owned.ownedRefs = unsafe.Slice((*storeOwnedDocRef)(unsafe.Pointer(unsafe.SliceData(block.Bytes()))), count)
		}
		runtime.SetFinalizer(owned, (*storeMappedDocs).release)
	default:
		owned, err = newStoreMappedDocs(count)
	}
	if err != nil {
		return nil, err
	}
	block, err := storemem.Allocate(dataBytes)
	if err != nil {
		owned.release()
		return nil, err
	}
	owned.sourceBlock = block
	return owned, nil
}

func storeCanUseCompactDocRefs(chunks storeChunkVector, shapeCount int) bool {
	if shapeCount >= int(storeOwnedNoShape) {
		return false
	}
	compact := true
	chunks.each(func(_ uint32, chunk *storeChunk) bool {
		for i := 0; i < chunk.docs.Len(); i++ {
			index := chunk.docs.docAt(i)
			if uint64(len(index.src)) > uint64(^uint32(0)) || len(index.entries) > int(^uint16(0)) {
				compact = false
				return false
			}
		}
		return true
	})
	return compact
}

func storeCanUseOwnedDocRefs(chunks storeChunkVector, shapeCount int) bool {
	if shapeCount >= int(storeOwnedNoShape) {
		return false
	}
	compact := true
	chunks.each(func(_ uint32, chunk *storeChunk) bool {
		for i := 0; i < chunk.docs.Len(); i++ {
			ref := chunk.docs.shapeTapeRefAt(i)
			if ref.rec != nil && (ref.start > shapeNarrowMaxEnd || ref.end > shapeNarrowMaxEnd) {
				compact = false
				return false
			}
		}
		return true
	})
	return compact
}

func storeOwnedDocumentDataBytes(chunks storeChunkVector) (int, error) {
	var total uint64
	valid := true
	chunks.each(func(_ uint32, chunk *storeChunk) bool {
		for i := 0; i < chunk.docs.Len(); i++ {
			index := chunk.docs.docAt(i)
			ref := chunk.docs.shapeTapeRefAt(i)
			narrowKind, narrowWidth := storeOwnedNarrowStorage(&chunk.docs, i, ref)
			var ok bool
			total, ok = storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, false)
			if !ok {
				valid = false
				return false
			}
		}
		return true
	})
	if !valid {
		return 0, ErrStorePersistTooLarge
	}
	return int(total), nil
}

func copyStoreOwnedEntries(dst []byte, entries []IndexEntry) {
	if len(entries) == 0 {
		return
	}
	out := unsafe.Slice((*IndexEntry)(unsafe.Pointer(unsafe.SliceData(dst))), len(entries))
	copy(out, entries)
}
