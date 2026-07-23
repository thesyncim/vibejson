package slopjson

import (
	"bytes"
	"unsafe"
)

// storeDocumentTemplate interns one exactly verified structural tape for a
// repeated, possibly nested JSON layout. Per-row storage keeps only compact
// source spans. The representative source is private and is used solely to
// run the ordinary, battle-tested compiled-pointer resolver once per template
// encountered by a batch; returned values always borrow the queried row.
type storeDocumentTemplate struct {
	index     Index
	spanIndex []uint16
	spanCount uint16
}

const (
	storeDocumentTemplateNone = ^uint32(0)
	// Template discovery is deliberately bounded. A hostile stream of unique
	// layouts must not turn bulk construction into one map/object per row.
	storeDocumentTemplateLimit = 4096
)

// storeOwnedRowLayout is the builder's transient write plan for one row. The
// high nibble is the packed tape kind. The low 28 bits hold template ID + 1;
// zero means the row is not templated. One zero-filled array therefore covers
// classic, shaped, and templated rows without a separate width table or
// sentinel initialization pass.
type storeOwnedRowLayout uint32

const storeOwnedRowTemplateMask = 1<<28 - 1

func storeMakeOwnedRowLayout(kind uint8, templateID uint32, template bool) storeOwnedRowLayout {
	id := uint32(0)
	if template {
		id = templateID + 1
	}
	return storeOwnedRowLayout(uint32(kind)<<28 | id)
}

func (l storeOwnedRowLayout) kind() uint8 { return uint8(l >> 28) }

func (l storeOwnedRowLayout) template() (uint32, bool) {
	id := uint32(l) & storeOwnedRowTemplateMask
	return id - 1, id != 0
}

type storeDocumentTemplateCandidate struct {
	index    Index
	firstRow int
	template uint32
	saving   uint64
}

func storeDocumentTemplateEligible(index Index) bool {
	if len(index.src) > shapeNarrowMaxEnd || len(index.entries) == 0 || len(index.entries) > shapeNarrowMaxEnd {
		return false
	}
	for i := range index.entries {
		if index.entries[i].start > shapeNarrowMaxEnd || index.entries[i].end > shapeNarrowMaxEnd {
			return false
		}
	}
	return true
}

// storeDocumentTemplateHash is only a routing fingerprint. Admission always
// calls storeDocumentTemplateEqual, including exact key spelling comparison.
func storeDocumentTemplateHash(index Index) uint64 {
	h := uint64(0x9e3779b97f4a7c15) ^ uint64(len(index.entries))*0xbf58476d1ce4e5b9
	for i := range index.entries {
		e := &index.entries[i]
		h ^= uint64(e.info)*0x94d049bb133111eb + uint64(e.next)
		h = h<<27 | h>>(64-27)
		if e.flags()&tapeFlagKey == 0 {
			continue
		}
		for _, b := range index.src[e.start:e.end] {
			h ^= uint64(b)
			h *= 0x100000001b3
		}
	}
	h ^= h >> 30
	h *= 0xbf58476d1ce4e5b9
	h ^= h >> 27
	h *= 0x94d049bb133111eb
	return h ^ h>>31
}

func storeDocumentTemplateEqual(a, b Index) bool {
	if len(a.entries) != len(b.entries) {
		return false
	}
	for i := range a.entries {
		ae, be := &a.entries[i], &b.entries[i]
		if ae.info != be.info || ae.next != be.next {
			return false
		}
		if ae.flags()&tapeFlagKey != 0 &&
			!bytes.Equal(a.src[ae.start:ae.end], b.src[be.start:be.end]) {
			return false
		}
	}
	return true
}

func newStoreDocumentTemplate(index Index) *storeDocumentTemplate {
	src := bytes.Clone(index.src)
	entries := make([]IndexEntry, len(index.entries))
	copy(entries, index.entries)
	spanIndex := make([]uint16, len(entries))
	spanCount := uint16(0)
	for i := range entries {
		if i == 0 || entries[i].flags()&tapeFlagKey != 0 {
			spanIndex[i] = ^uint16(0)
			continue
		}
		spanIndex[i] = spanCount
		spanCount++
	}
	return &storeDocumentTemplate{
		index: Index{src: src, entries: entries}, spanIndex: spanIndex, spanCount: spanCount,
	}
}

// buildStoreDocumentTemplates performs repeat-sighting admission over the
// completed builder. layouts is temporary construction memory and is
// discarded before publication. First sightings remain classic unless an
// exactly equal second layout promotes both rows. Classic and template rows
// both end aligned to eight bytes, so promotion cannot change the alignment of
// intervening rows; the asserted saving is an exact multiple of eight.
func buildStoreDocumentTemplates(chunks storeChunkVector, count int) ([]*storeDocumentTemplate, []storeOwnedRowLayout, int, error) {
	layouts := make([]storeOwnedRowLayout, count)
	buckets := make(map[uint64][]uint16)
	candidates := make([]storeDocumentTemplateCandidate, 0, 64)
	templates := make([]*storeDocumentTemplate, 0, 16)
	var recent [4]uint16
	for i := range recent {
		recent[i] = ^uint16(0)
	}
	recentNext := 0
	row := 0
	var total uint64
	valid := true
	chunks.each(func(_ uint32, chunk *storeChunk) bool {
		for i := 0; i < chunk.docs.Len(); i++ {
			index := chunk.docs.docAt(i)
			ref := chunk.docs.shapeTapeRefAt(i)
			narrowKind, narrowWidth := storeOwnedNarrowStorage(&chunk.docs, i, ref)
			if ref.narrow {
				layouts[row] = storeMakeOwnedRowLayout(narrowKind, 0, false)
			}
			if ref.rec != nil || !storeDocumentTemplateEligible(index) {
				var ok bool
				total, ok = storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, false)
				if !ok {
					valid = false
					return false
				}
				row++
				continue
			}
			matched := false
			admit := func(candidateID uint16) bool {
				candidate := &candidates[candidateID]
				if !storeDocumentTemplateEqual(candidate.index, index) {
					return false
				}
				if candidate.template == storeDocumentTemplateNone {
					total -= candidate.saving
					candidate.template = uint32(len(templates))
					templates = append(templates, newStoreDocumentTemplate(candidate.index))
					kind, _ := storeDocumentTemplateStorage(candidate.index)
					layouts[candidate.firstRow] = storeMakeOwnedRowLayout(kind, candidate.template, true)
				}
				kind, _ := storeDocumentTemplateStorage(index)
				layouts[row] = storeMakeOwnedRowLayout(kind, candidate.template, true)
				matched = true
				recent[recentNext] = candidateID
				recentNext = (recentNext + 1) & (len(recent) - 1)
				return true
			}
			for _, candidateID := range recent {
				if candidateID != ^uint16(0) && admit(candidateID) {
					break
				}
			}
			if !matched {
				hash := storeDocumentTemplateHash(index)
				for _, candidateID := range buckets[hash] {
					if admit(candidateID) {
						break
					}
				}
				if !matched && len(candidates) < storeDocumentTemplateLimit {
					classicEnd, ok := storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, false)
					if !ok {
						valid = false
						return false
					}
					templateEnd, ok := storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, true)
					if !ok || templateEnd > classicEnd || (classicEnd-templateEnd)&7 != 0 {
						valid = false
						return false
					}
					candidateID := uint16(len(candidates))
					candidates = append(candidates, storeDocumentTemplateCandidate{
						index: index, firstRow: row, template: storeDocumentTemplateNone,
						saving: classicEnd - templateEnd,
					})
					buckets[hash] = append(buckets[hash], candidateID)
					recent[recentNext] = candidateID
					recentNext = (recentNext + 1) & (len(recent) - 1)
					total = classicEnd
				}
			}
			if matched {
				var ok bool
				total, ok = storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, true)
				if !ok {
					valid = false
					return false
				}
			} else if len(candidates) >= storeDocumentTemplateLimit {
				// The row that fills the bounded catalog was already counted
				// above. Later unrecognized rows still need their classic size.
				last := len(candidates) - 1
				if last < 0 || candidates[last].firstRow != row {
					var ok bool
					total, ok = storeOwnedDocumentEnd(total, index, ref, narrowKind, narrowWidth, false)
					if !ok {
						valid = false
						return false
					}
				}
			}
			row++
		}
		return true
	})
	if !valid || total > uint64(maxInt()) {
		return nil, nil, 0, ErrStorePersistTooLarge
	}
	return templates, layouts, int(total), nil
}

func storeOwnedDocumentEnd(start uint64, index Index, ref shapeTapeRef, narrowKind uint8, narrowWidth uint64, template bool) (uint64, bool) {
	if uint64(len(index.src)) > uint64(maxInt())-start {
		return 0, false
	}
	end := start + uint64(len(index.src))
	kind := uint8(persistDocClassic)
	count, width := len(index.entries), uint64(unsafe.Sizeof(IndexEntry{}))
	switch {
	case template:
		kind, width = storeDocumentTemplateStorage(index)
		count = storeDocumentTemplateSpanCount(index)
	case ref.narrow:
		kind, count, width = narrowKind, len(ref.rec.fields), narrowWidth
	case ref.rec != nil:
		kind = persistDocWide
	}
	end = storeMappedTapeOffset(end, kind)
	if width != 0 && uint64(count) > uint64(maxInt())/width {
		return 0, false
	}
	bytes := uint64(count) * width
	if end > uint64(maxInt()) || bytes > uint64(maxInt())-end {
		return 0, false
	}
	end += bytes
	if template {
		end = persistAlign8(end)
	}
	return end, end <= uint64(maxInt())
}

// storeOwnedNarrowStorage selects a scalar-tape width for one flat shape row.
// The five-byte form is the universal fallback. Short documents use two
// one-byte coordinates; longer documents use a one-byte length only when
// every scalar span proves it fits.
func storeOwnedNarrowStorage(docs *DocSet, row int, ref shapeTapeRef) (uint8, uint64) {
	if !ref.narrow {
		return storeOwnedDocNarrow, storeOwnedNarrowValueLen
	}
	if ref.end <= uint32(^uint8(0)) {
		return storeOwnedDocNarrow8, storeOwnedNarrow8ValueLen
	}
	packed9 := ref.end <= 0x1ff
	for ordinal := range ref.rec.fields {
		value := docs.narrowAt(row, ref, ordinal)
		start, end := value.span&0xffff, value.span>>16
		if end-start > uint32(^uint8(0)) || value.info>>infoKindShift > 0x7f {
			return storeOwnedDocNarrow, storeOwnedNarrowValueLen
		}
		packed9 = packed9 && start <= 0x1ff
	}
	if packed9 {
		return storeOwnedDocNarrow9, storeOwnedNarrow9ValueLen
	}
	return storeOwnedDocNarrowLength8, storeOwnedNarrowLength8ValueLen
}

func storeOwnedNarrowStorageWidth(kind uint8) uint64 {
	switch kind {
	case storeOwnedDocNarrow8, storeOwnedDocNarrow9:
		return 3
	case storeOwnedDocNarrowLength8:
		return storeOwnedNarrowLength8ValueLen
	default:
		return storeOwnedNarrowValueLen
	}
}

// storeDocumentTemplateStorage selects the narrowest span encoding for one
// row. The root span lives in the row descriptor, so only non-key descendants
// participate. The choice is per row; one template may serve mixed document
// sizes without widening its small rows.
func storeDocumentTemplateStorage(index Index) (uint8, uint64) {
	if len(index.src) <= int(^uint8(0)) {
		return storeOwnedDocTemplate8, storeOwnedTemplate8SpanLen
	}
	for i := 1; i < len(index.entries); i++ {
		entry := &index.entries[i]
		if entry.flags()&tapeFlagKey == 0 && entry.end-entry.start > uint32(^uint8(0)) {
			return storeOwnedDocTemplate, storeOwnedTemplateSpanLen
		}
	}
	return storeOwnedDocTemplateLength8, storeOwnedTemplateLength8SpanLen
}

func storeDocumentTemplateSpanCount(index Index) int {
	count := 0
	for i := 1; i < len(index.entries); i++ {
		if index.entries[i].flags()&tapeFlagKey == 0 {
			count++
		}
	}
	return count
}
