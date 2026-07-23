package slopjson

import (
	"encoding/binary"
	"runtime"
	"unsafe"
)

// Template reads decode one row's spans against an immutable structural
// tape. Batch paths retain compact spans; only the general Index API widens
// and caches a classic tape.

func (s *DocSet) storeTemplateAt(i int) (*storeDocumentTemplate, bool) {
	if s.mappedDocs == nil {
		return nil, false
	}
	index := s.mappedBase + uint64(i)
	var kind uint8
	var shapeID uint32
	if s.mappedDocs.compactRefs != nil {
		r := &s.mappedDocs.compactRefs[index]
		kind, shapeID = r.kind, uint32(r.meta)
	} else if s.mappedDocs.ownedRefs != nil {
		r := &s.mappedDocs.ownedRefs[index]
		kind, shapeID = r.kind, uint32(r.shapeID)
	} else {
		r := &s.mappedDocs.refs[index]
		kind, shapeID = r.kind, r.shapeID
	}
	if !storeOwnedDocIsTemplate(kind) || int(shapeID) >= len(s.mappedDocs.templates) {
		runtime.KeepAlive(s.mappedDocs)
		return nil, false
	}
	template := s.mappedDocs.templates[shapeID]
	runtime.KeepAlive(s.mappedDocs)
	return template, true
}

func (s *DocSet) storeTemplateSpan(i int, template *storeDocumentTemplate, ordinal int) uint32 {
	index := s.mappedBase + uint64(i)
	if ordinal == 0 {
		start, end := storeRootSpan(s.rawAt(i))
		return start | end<<16
	}
	var sourceOff uint64
	var srcLen uint32
	var kind uint8
	if s.mappedDocs.compactRefs != nil {
		r := &s.mappedDocs.compactRefs[index]
		sourceOff, srcLen, kind = r.sourceOff, r.srcLen, r.kind
	} else if s.mappedDocs.ownedRefs != nil {
		r := &s.mappedDocs.ownedRefs[index]
		sourceOff, srcLen, kind = r.sourceOff, r.srcLen, r.kind
	} else {
		r := &s.mappedDocs.refs[index]
		sourceOff, srcLen, kind = r.sourceOff, r.srcLen, r.kind
	}
	spanIndex := template.spanIndex[ordinal]
	if spanIndex == ^uint16(0) {
		return 0
	}
	width := storeOwnedTemplateSpanWidth(kind)
	off := storeMappedTapeOffset(sourceOff+uint64(srcLen), kind) + uint64(spanIndex)*width
	var span uint32
	switch kind {
	case storeOwnedDocTemplate8:
		span = uint32(s.source[off]) | uint32(s.source[off+1])<<16
	case storeOwnedDocTemplateLength8:
		start := uint32(binary.LittleEndian.Uint16(s.source[off : off+2]))
		span = start | (start+uint32(s.source[off+2]))<<16
	default:
		span = binary.LittleEndian.Uint32(s.source[off : off+storeOwnedTemplateSpanLen])
	}
	runtime.KeepAlive(s.mappedDocs)
	return span
}

func (s *DocSet) storeTemplateKeySpan(i int, template *storeDocumentTemplate, ordinal int) (uint32, uint32) {
	valueSpan := s.storeTemplateSpan(i, template, ordinal+1)
	src := s.docAt(i).src
	j := int(valueSpan&0xffff) - 1
	for isJSONWhitespace(src[j]) {
		j--
	}
	j--
	for isJSONWhitespace(src[j]) {
		j--
	}
	end := uint32(j) + 1
	representative := &template.index.entries[ordinal]
	return end - (representative.end - representative.start), end
}

func (s *DocSet) synthStoreTemplate(i int, template *storeDocumentTemplate, dst []IndexEntry) []IndexEntry {
	dst = append(dst, template.index.entries...)
	base := len(dst) - len(template.index.entries)
	rootSpan := s.storeTemplateSpan(i, template, 0)
	dst[base].start, dst[base].end = rootSpan&0xffff, rootSpan>>16
	for ordinal := 1; ordinal < len(template.index.entries); ordinal++ {
		if template.spanIndex[ordinal] == ^uint16(0) {
			continue
		}
		span := s.storeTemplateSpan(i, template, ordinal)
		dst[base+ordinal].start = span & 0xffff
		dst[base+ordinal].end = span >> 16
	}
	for ordinal := range template.index.entries {
		entry := &dst[base+ordinal]
		if entry.flags()&tapeFlagKey == 0 {
			continue
		}
		entry.start, entry.end = s.storeTemplateKeySpan(i, template, ordinal)
	}
	return dst
}

func (s *DocSet) widenStoreTemplate(i int, template *storeDocumentTemplate) Index {
	s.widenMu.Lock()
	defer s.widenMu.Unlock()
	if entries, ok := s.widened[i]; ok {
		return Index{src: s.docAt(i).src, entries: entries}
	}
	entries := s.synthStoreTemplate(i, template, make([]IndexEntry, 0, len(template.index.entries)))
	if s.widened == nil {
		s.widened = make(map[int][]IndexEntry)
	}
	s.widened[i] = entries
	return Index{src: s.docAt(i).src, entries: entries}
}

type storeTemplatePointerHint struct {
	template *storeDocumentTemplate
	ordinal  int
	ok       bool
	err      error
}

type storeTemplateFieldHint struct {
	template *storeDocumentTemplate
	ordinal  int32
}

func (h *storeTemplateFieldHint) lookup(template *storeDocumentTemplate, key CompiledKey) int {
	if h.template == template {
		return int(h.ordinal)
	}
	node, ok := template.index.Root().GetCompiled(key)
	ordinal := -1
	if ok {
		base := unsafe.Pointer(unsafe.SliceData(template.index.entries))
		delta := uintptr(unsafe.Pointer(node.entry)) - uintptr(base)
		candidate := int(delta / unsafe.Sizeof(IndexEntry{}))
		if candidate >= 0 && candidate < len(template.index.entries) {
			ordinal = candidate
		}
	}
	h.template, h.ordinal = template, int32(ordinal)
	return ordinal
}

func (h *storeTemplatePointerHint) resolve(template *storeDocumentTemplate, pointer CompiledPointer) (int, bool, error) {
	if h.template == template {
		return h.ordinal, h.ok, h.err
	}
	node, ok, err := template.index.PointerCompiled(pointer)
	ordinal := 0
	if ok {
		base := unsafe.Pointer(unsafe.SliceData(template.index.entries))
		delta := uintptr(unsafe.Pointer(node.entry)) - uintptr(base)
		ordinal = int(delta / unsafe.Sizeof(IndexEntry{}))
		if ordinal < 0 || ordinal >= len(template.index.entries) {
			ok = false
		}
	}
	h.template, h.ordinal, h.ok, h.err = template, ordinal, ok, err
	return ordinal, ok, err
}
