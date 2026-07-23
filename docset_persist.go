package simdjson

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
	"github.com/thesyncim/simdjson/internal/byteview"
)

// Log-structured DocSet persistence: a versioned, mmap-friendly serialization
// of a set's core so a corpus reopens with zero re-parse.
//
// A built DocSet is source arenas plus a structural tape per document (index.go),
// with the shape-deduplicated storage (docset_shape.go) folding the recurring
// keys into a shared shape table. WriteTo lays that state down as a byte image
// and Open reconstructs a DocSet whose arenas view straight back into it, so a
// process that has indexed a corpus once can memory-map the image and answer
// Doc(i) at full speed without re-validating a byte. The format is EXPLICITLY
// UNSTABLE before v1: the header and every generation carry the format version
// and Open rejects a version or magic it does not recognize rather than
// misreading it.
//
// # Layout
//
// The image is a header, an append-only log of self-describing document
// records, a shared shape table, and a manifest that indexes them, closed by a
// fixed footer that locates the manifest from the end:
//
//	  offset 0
//	+----------------------------------------------------------------+
//	| header      magic "SJDOCSET", format version                    |
//	+----------------------------------------------------------------+
//	| doc record 0   [hdr | source bytes | tape entries]  (8-aligned) |
//	| doc record 1   [hdr | source bytes | tape entries]              |
//	|   ...            << the write path appends new versions here >>  |
//	| doc record N-1                                                  |
//	+----------------------------------------------------------------+
//	| shape table    the deduplicated key spellings, once per shape   |
//	+----------------------------------------------------------------+
//	| manifest       magic, flags/options, shape-table span, and the  |
//	|                offsets index: the absolute offset of every live  |
//	|                document record — this is the snapshot            |
//	+----------------------------------------------------------------+
//	| footer         magic, manifest span, manifest checksum, version |
//	+----------------------------------------------------------------+
//	  end of image
//
// A document record is self-describing — its header carries the source length,
// the entry count and width, and (for a shape-taped document) the shape id and
// root span — so the record is the unit the log appends and the manifest merely
// points at. The manifest is the snapshot: it lists the offset of every
// document record the snapshot sees plus the shape table those records resolve
// against. The footer is fixed-size and read from the end, so a reader locates
// the latest manifest in one seek.
//
// # Alignment to the write path (ADR 0002)
//
// This shape is deliberately the substrate the planned MVCC-snapshot write path
// drops into. Mutation there is an append: a writer lays a new document version
// down as a fresh record, appends any new shapes, and publishes a new snapshot
// by appending a new manifest generation and footer at the tail — never
// rewriting a live byte, exactly the never-move arena discipline the in-memory
// set already keeps (docset.go). A manifest is an immutable snapshot of
// pointers into immutable records, so an update reuses every unchanged record
// and only the changed document costs new bytes; older manifests remain in the
// image as the older snapshots an MVCC reader time-travels to, and the newest
// footer names the current one. Reads mmap a manifest and resolve handles
// straight into its records, blocking never and paying no indirection. This
// first cut writes one generation; the format needs no change to grow more.
//
// # Zero-copy and portability
//
// All integers are little-endian. On the overwhelmingly common little-endian
// host the on-disk words already match the in-memory representation, so a
// document's source bytes and its 16-byte entry tape (or 16-byte shape-taped
// value array) are handed to the reconstructed Index as sub-slices that view
// straight into the mapped image — no copy, no parse. Two sections are copied
// rather than viewed, and the reasons are recorded honestly:
//
//   - The narrow (8-byte) shape-taped value arrays are consolidated into the
//     set's single DocSet.narrow slab, which is addressed by a per-document
//     offset; because each record carries its own values, they cannot all be
//     one contiguous view and are copied into the slab on Open.
//   - The shape table is small (keys live once per shape) and is rebuilt into a
//     fresh, fully functional ShapeCache so the reopened set resolves and
//     continues to Append; its bytes are copied there.
//
// On a big-endian host, or when a section's mapped address is not 4-byte
// aligned for an entry view, Open decodes the words individually instead of
// aliasing them; the result is byte-identical, only slower. WriteTo emits the
// canonical little-endian form on every host.
//
// # What is serialized, what is rebuilt
//
// The core is serialized: every document's source and tape (classic, wide
// shape-taped, and narrow shape-taped), the shape table, and the interned key
// spellings the shapes carry. The two opt-in accelerators layered over the core
// — the inverted postings (docset_postings.go) and the value dictionary
// (docset_valuedict.go) — are rebuilt on Open from the reconstructed documents
// when their flags were set, deterministically reproducing the original
// structures; a first cut is free to rebuild them because they never change
// what a read returns, only its cost and at-rest space (both are pure functions
// of the committed documents).
//
// # Lifetime
//
// A set returned by Open borrows the image: its document sources and entry
// tapes view into the bytes passed to Open, which the set pins (DocSet.source)
// so a Go-owned image stays alive for the set's lifetime. When the image is a
// memory map the caller owns the mapping — every borrowed view is valid only
// while it stays mapped, exactly the contract a borrowed arena keeps.
//
// # Bounds
//
// One document's source and entry storage stay within the index's 32-bit
// coordinate space (index.go), so a record is bounded like any built document.
// Open requires the whole image addressable as one byte slice; a memory map
// satisfies this lazily, the OS paging in only the records a reader touches, so
// a corpus larger than RAM reopens without an eager read. WriteTo streams to an
// io.Writer, buffering only the O(documents) manifest, so writing never holds a
// second copy of the corpus.

// The format identifiers and fixed sizes. The version is bumped on any layout
// change; Open rejects a mismatch (the format is unstable before v1).
const (
	persistVersion = 1

	// Store writes one bounded DocSet image per at-most-64-row micro-page. Keep
	// that manifest and its offsets in fixed scratch so persistence allocation
	// is page-granular rather than row-granular.
	persistSmallManifestDocuments = 64

	persistHeaderMagic   = "SJDOCSET"
	persistManifestMagic = "SJDSMANI"
	persistFooterMagic   = "SJDSFOOT"

	persistHeaderLen       = 16 // magic(8) + version(4) + reserved(4)
	persistRecordHeaderLen = 24 // see the record header layout in writeDocRecord
	persistManifestFixed   = 56 // manifest bytes before the offsets index
	persistFooterLen       = 40 // see the footer layout in WriteTo
)

// The manifest flag bits record the set's opt-in modes and the enrichment
// option, so Open restores the same configuration and rebuilds the same
// accelerators.
const (
	persistFlagShapeTapes = 1 << iota
	persistFlagPostings
	persistFlagValueDict
	persistFlagHashKeys
	persistFlagWideValueTapes
)

// A document record's storage class, in its header: a classic tape, a 16-byte
// shape-taped value array, or an 8-byte narrow value array.
const (
	persistDocClassic uint8 = iota
	persistDocWide
	persistDocNarrow
)

// persistNoShape marks a classic record's absent shape id.
const persistNoShape = ^uint32(0)

// Open and WriteTo report these on a malformed or unrecognized image. They own
// no storage and are safe to compare concurrently.
var (
	// ErrPersistMagic means the image is not a DocSet serialization: a header or
	// footer magic did not match.
	ErrPersistMagic = errors.New("simdjson: not a DocSet image")
	// ErrPersistVersion means the image's format version differs from this
	// build's; the pre-v1 format is unstable and mismatches are rejected rather
	// than misread.
	ErrPersistVersion = errors.New("simdjson: unsupported DocSet image version")
	// ErrPersistCorrupt means the image is structurally invalid: a truncated or
	// out-of-range section, a failed manifest checksum, or an inconsistent
	// record. It is the fail-closed verdict on any input Open cannot trust.
	ErrPersistCorrupt = errors.New("simdjson: corrupt DocSet image")
)

// persistNativeLittleEndian reports whether the host stores integers
// little-endian, so the bulk entry sections can be aliased (native) or must be
// decoded word by word (big-endian). Determined once at init.
var persistNativeLittleEndian = func() bool {
	x := uint16(1)
	return *(*byte)(unsafe.Pointer(&x)) == 1
}()

// persistAlign8 rounds n up to the next multiple of eight, the alignment the
// entry sections take so an aliased IndexEntry view meets its 4-byte load
// requirement and the record after it starts aligned.
func persistAlign8(n uint64) uint64 { return (n + 7) &^ 7 }

// persistChecksum is the FNV-1a 64-bit fold used to seal the manifest. It gates
// only structural trust — a mismatch rejects the image — so a non-cryptographic
// fold is sufficient.
func persistChecksum(b []byte) uint64 {
	const (
		offset uint64 = 1469598103934665603
		prime  uint64 = 1099511628211
	)
	h := offset
	for _, c := range b {
		h = (h ^ uint64(c)) * prime
	}
	return h
}

// WriteTo serializes the set to w in the log-structured image this file
// documents and returns the number of bytes written, satisfying io.WriterTo.
// It streams a header, one self-describing record per document in ordinal
// order, the shared shape table, the manifest indexing them, and a locating
// footer, buffering only the O(documents) manifest so a large corpus never
// costs a second copy. The image reopens through Open into a DocSet whose every
// Doc and accessor is byte-identical to this one's.
func (s *DocSet) WriteTo(w io.Writer) (int64, error) {
	pw := &persistWriter{w: w}
	s.writeToPersistWriter(pw, 0)
	return pw.off, pw.err
}

// writeToNested writes a self-contained image through parent while keeping
// all offsets relative to the nested image. Reusing the outer writer avoids
// allocating one io.Writer adapter and scratch block per Store micro-page.
func (s *DocSet) writeToNested(pw *persistWriter) (int64, error) {
	base := pw.off
	s.writeToPersistWriter(pw, base)
	return pw.off - base, pw.err
}

func (s *DocSet) writeToPersistWriter(pw *persistWriter, base int64) {

	var header [persistHeaderLen]byte
	copy(header[0:8], persistHeaderMagic)
	binary.LittleEndian.PutUint32(header[8:12], persistVersion)
	pw.writeSmall(header[:])

	// Shape records are addressed by their compiled id — their position in the
	// cache's shape list, which is stable — so a record names its shape by that
	// index and the shape table is written in the same order.
	shapeRecords := s.persistShapeRecords()
	shapeID := make(map[*shapeRecord]uint32, len(shapeRecords))
	for id, rec := range shapeRecords {
		shapeID[rec] = uint32(id)
	}

	var smallOffsets [persistSmallManifestDocuments]uint64
	var docOffsets []uint64
	docCount := s.Len()
	if docCount <= len(smallOffsets) {
		docOffsets = smallOffsets[:docCount]
	} else {
		docOffsets = make([]uint64, docCount)
	}
	var narrowTotal uint64
	for i := 0; i < docCount; i++ {
		docOffsets[i] = pw.writeDocRecord(s, i, shapeID, &narrowTotal, base)
	}

	pw.pad8()
	shapeTableOffset := uint64(pw.off - base)
	pw.writeShapeTable(shapeRecords)
	shapeTableLength := uint64(pw.off-base) - shapeTableOffset

	pw.pad8()
	manifestOffset := uint64(pw.off - base)
	manifest := s.buildManifest(pw.small[:0], docOffsets, narrowTotal, shapeTableOffset, shapeTableLength)
	pw.write(manifest)

	var footer [persistFooterLen]byte
	copy(footer[0:8], persistFooterMagic)
	binary.LittleEndian.PutUint64(footer[8:16], manifestOffset)
	binary.LittleEndian.PutUint64(footer[16:24], uint64(len(manifest)))
	binary.LittleEndian.PutUint64(footer[24:32], persistChecksum(manifest))
	binary.LittleEndian.PutUint32(footer[32:36], persistVersion)
	pw.writeSmall(footer[:])

}

// writeDocRecord lays down document i's self-describing record and returns its
// absolute offset. The 24-byte header is followed by the source bytes and, once
// realigned to eight, the tape: the classic or wide value entries at 16 bytes,
// or the narrow value array at 8. It accumulates the narrow value total so the
// reader can size the consolidated slab in one allocation.
//
//	 0        4        8        12       16       20  21  22   24
//	+--------+--------+--------+--------+--------+---+---+-----+
//	| srcLen | nEntry | start  |  end   | shape  | k | e | pad |
//	+--------+--------+--------+--------+--------+---+---+-----+
//	 srcLen  source byte length            start/end  shape-taped root span
//	 nEntry  entry/value count             shape      shape id (classic: ^0)
//	 k       storage class (persistDoc*)   e          key-hash enrichment flag
func (pw *persistWriter) writeDocRecord(s *DocSet, i int, shapeID map[*shapeRecord]uint32, narrowTotal *uint64, base int64) uint64 {
	pw.pad8()
	offset := uint64(pw.off - base)

	idx := s.docAt(i)
	ref := s.shapeTapeRefAt(i)
	template, templateOK := s.storeTemplateAt(i)
	if ref.rec != nil {
		ref.start, ref.end = s.shapeTapeRootSpan(idx, ref)
	}

	var (
		kind    uint8
		entries uint32
		sid     = persistNoShape
	)
	switch {
	case templateOK:
		kind = persistDocClassic
		entries = uint32(len(template.index.entries))
	case ref.rec == nil:
		kind = persistDocClassic
		entries = uint32(len(idx.entries))
	case ref.narrow:
		kind = persistDocNarrow
		entries = uint32(len(ref.rec.fields))
		sid = shapeID[ref.rec]
		*narrowTotal += uint64(entries)
	default:
		kind = persistDocWide
		entries = uint32(len(idx.entries))
		sid = shapeID[ref.rec]
	}

	var header [persistRecordHeaderLen]byte
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(idx.src)))
	binary.LittleEndian.PutUint32(header[4:8], entries)
	binary.LittleEndian.PutUint32(header[8:12], ref.start)
	binary.LittleEndian.PutUint32(header[12:16], ref.end)
	binary.LittleEndian.PutUint32(header[16:20], sid)
	header[20] = kind
	if ref.enriched {
		header[21] = 1
	}
	pw.writeSmall(header[:])
	pw.write(idx.src)
	pw.pad8()
	if templateOK {
		pw.writeTemplateDoc(s, i, template)
	} else if kind == persistDocNarrow {
		pw.writeNarrowDoc(s, i, ref, entries)
	} else {
		pw.writeEntries(idx.entries)
	}
	return offset
}

// writeTemplateDoc expands a builder-only repeated-layout template directly
// into the stable checkpoint format. It uses fixed scratch and never creates
// a second in-memory tape, so checkpointing a compact Store remains bounded.
func (pw *persistWriter) writeTemplateDoc(s *DocSet, doc int, template *storeDocumentTemplate) {
	var raw [16]byte
	for ordinal := range template.index.entries {
		entry := template.index.entries[ordinal]
		if ordinal == 0 {
			span := s.storeTemplateSpan(doc, template, ordinal)
			entry.start, entry.end = span&0xffff, span>>16
		} else if template.spanIndex[ordinal] == ^uint16(0) {
			entry.start, entry.end = s.storeTemplateKeySpan(doc, template, ordinal)
		} else {
			span := s.storeTemplateSpan(doc, template, ordinal)
			entry.start, entry.end = span&0xffff, span>>16
		}
		binary.LittleEndian.PutUint32(raw[0:4], entry.start)
		binary.LittleEndian.PutUint32(raw[4:8], entry.end)
		binary.LittleEndian.PutUint32(raw[8:12], entry.next)
		binary.LittleEndian.PutUint32(raw[12:16], entry.info)
		pw.writeSmall(raw[:])
	}
}

// writeNarrowDoc streams one compact tape from either the ordinary Go slab or
// a Store page image. The fixed eight-byte scratch keeps re-checkpointing an
// OpenStore zero-allocation per row and avoids materializing a second tape.
func (pw *persistWriter) writeNarrowDoc(s *DocSet, doc int, ref shapeTapeRef, entries uint32) {
	var raw [8]byte
	for i := uint32(0); i < entries; i++ {
		value := s.narrowAt(doc, ref, int(i))
		binary.LittleEndian.PutUint32(raw[0:4], value.span)
		binary.LittleEndian.PutUint32(raw[4:8], value.info)
		pw.writeSmall(raw[:])
	}
}

// writeShapeTable serializes the shared shapes: a count, then per shape a field
// count and each field's raw key spelling (the content between the quotes,
// escapes included). Only the raw spellings are stored; Open reconstructs the
// decoded names, the info words, the name table, and the fingerprint by
// resolving each shape back through a fresh ShapeCache, so the table is the
// interned key store and nothing about a shape is duplicated on disk.
func (s *DocSet) persistShapeRecords() []*shapeRecord {
	if len(s.mappedShapes) != 0 {
		return s.mappedShapes
	}
	return s.shapes.shapes
}

func (pw *persistWriter) writeShapeTable(shapes []*shapeRecord) {
	var scratch [4]byte
	binary.LittleEndian.PutUint32(scratch[:], uint32(len(shapes)))
	pw.writeSmall(scratch[:])
	for _, rec := range shapes {
		binary.LittleEndian.PutUint32(scratch[:], uint32(len(rec.fields)))
		pw.writeSmall(scratch[:])
		for f := range rec.fields {
			raw := rec.fields[f].raw
			binary.LittleEndian.PutUint32(scratch[:], uint32(len(raw)))
			pw.writeSmall(scratch[:])
			if len(raw) > 0 {
				pw.write(byteview.Bytes(raw))
			}
		}
	}
}

// buildManifest assembles the manifest bytes: the fixed prologue (magic,
// version, flags, options, counts, and the shape table span) followed by the
// offsets index — the absolute offset of every document record, which is the
// snapshot's live set. It is buffered whole so WriteTo can checksum it.
func (s *DocSet) buildManifest(dst []byte, docOffsets []uint64, narrowTotal, shapeTableOffset, shapeTableLength uint64) []byte {
	need := persistManifestFixed + 8*len(docOffsets)
	if cap(dst) < need {
		dst = make([]byte, need)
	} else {
		dst = dst[:need]
		clear(dst)
	}
	buf := dst
	copy(buf[0:8], persistManifestMagic)
	binary.LittleEndian.PutUint32(buf[8:12], persistVersion)
	binary.LittleEndian.PutUint32(buf[12:16], s.persistFlags())
	binary.LittleEndian.PutUint64(buf[16:24], uint64(s.Options.MaxDepth))
	binary.LittleEndian.PutUint32(buf[24:28], s.valueFloor)
	// buf[28:32] reserved
	binary.LittleEndian.PutUint32(buf[32:36], uint32(len(docOffsets)))
	binary.LittleEndian.PutUint32(buf[36:40], uint32(narrowTotal))
	binary.LittleEndian.PutUint64(buf[40:48], shapeTableOffset)
	binary.LittleEndian.PutUint64(buf[48:56], shapeTableLength)
	for i, off := range docOffsets {
		binary.LittleEndian.PutUint64(buf[persistManifestFixed+8*i:], off)
	}
	return buf
}

// persistFlags packs the set's modes and enrichment option for the manifest.
func (s *DocSet) persistFlags() uint32 {
	var f uint32
	if s.ShapeTapes {
		f |= persistFlagShapeTapes
	}
	if s.Postings {
		f |= persistFlagPostings
	}
	if s.ValueDict {
		f |= persistFlagValueDict
	}
	if s.Options.HashKeys {
		f |= persistFlagHashKeys
	}
	if s.wideValueTapes {
		f |= persistFlagWideValueTapes
	}
	return f
}

// A persistWriter threads a running offset and the first write error through
// WriteTo so callers stream without tracking either. Once an error is latched
// every later write is a no-op and WriteTo returns it.
type persistWriter struct {
	w     io.Writer
	off   int64
	err   error
	small [persistManifestFixed + 8*persistSmallManifestDocuments]byte
}

// Write lets nested persistence formats stream through one offset/error
// tracker. It also upgrades a short write with no explicit error to
// io.ErrShortWrite, as io.WriterTo requires.
func (pw *persistWriter) Write(b []byte) (int, error) {
	if pw.err != nil || len(b) == 0 {
		return 0, pw.err
	}
	n, err := pw.w.Write(b)
	pw.off += int64(n)
	if err == nil && n != len(b) {
		err = io.ErrShortWrite
	}
	if err != nil {
		pw.err = err
	}
	return n, err
}

func (pw *persistWriter) write(b []byte) {
	_, _ = pw.Write(b)
}

// writeSmall copies a short stack-backed field into writer-owned storage
// before crossing the io.Writer interface. Without this boundary, record
// headers and endian scratch arrays escape once per document. All callers are
// synchronous and len(b) is at most the fixed footer width.
func (pw *persistWriter) writeSmall(b []byte) {
	if len(b) > len(pw.small) {
		panic("simdjson: internal persistence field exceeds scratch")
	}
	copy(pw.small[:], b)
	pw.write(pw.small[:len(b)])
}

// pad8 writes zero bytes up to the next eight-byte boundary.
func (pw *persistWriter) pad8() {
	if r := pw.off & 7; r != 0 {
		clear(pw.small[:8])
		pw.write(pw.small[:8-r])
	}
}

// writeEntries emits a 16-byte entry array little-endian: a raw copy on a
// little-endian host, where the words already match, and a per-word encode
// otherwise.
func (pw *persistWriter) writeEntries(e []IndexEntry) {
	if len(e) == 0 {
		return
	}
	if persistNativeLittleEndian {
		pw.write(unsafe.Slice((*byte)(unsafe.Pointer(&e[0])), len(e)*int(unsafe.Sizeof(IndexEntry{}))))
		return
	}
	for i := range e {
		binary.LittleEndian.PutUint32(pw.small[0:4], e[i].start)
		binary.LittleEndian.PutUint32(pw.small[4:8], e[i].end)
		binary.LittleEndian.PutUint32(pw.small[8:12], e[i].next)
		binary.LittleEndian.PutUint32(pw.small[12:16], e[i].info)
		pw.write(pw.small[:16])
	}
}

// writeNarrow emits an 8-byte narrow value array little-endian, native copy or
// per-word encode like writeEntries.
func (pw *persistWriter) writeNarrow(v []shapeNarrowValue) {
	if len(v) == 0 {
		return
	}
	if persistNativeLittleEndian {
		pw.write(unsafe.Slice((*byte)(unsafe.Pointer(&v[0])), len(v)*int(unsafe.Sizeof(shapeNarrowValue{}))))
		return
	}
	for i := range v {
		binary.LittleEndian.PutUint32(pw.small[0:4], v[i].span)
		binary.LittleEndian.PutUint32(pw.small[4:8], v[i].info)
		pw.write(pw.small[:8])
	}
}

// Open reconstructs a DocSet from an image WriteTo produced. The returned set
// borrows data: its document sources and entry tapes view into it (a memory map
// pages in only what a reader touches), so data must stay valid — and a memory
// map stay mapped — for the set's lifetime. Every Doc and accessor is
// byte-identical to the set that was written. Open validates the header,
// footer, manifest checksum, and every section bound, returning ErrPersistMagic,
// ErrPersistVersion, or ErrPersistCorrupt without panicking on any truncated or
// malformed input.
func Open(data []byte) (*DocSet, error) {
	set := new(DocSet)
	if err := openDocSetInto(set, data); err != nil {
		return nil, err
	}
	return set, nil
}

// openDocSetInto reconstructs an image directly into caller-owned storage.
// Store persistence uses it to initialize an embedded chunk DocSet without
// copying a value that contains a synchronization primitive.
func openDocSetInto(set *DocSet, data []byte) error {
	return openDocSetIntoMode(set, data, nil, 0)
}

// openDocSetIntoStore reconstructs one Store micro-page with its per-document
// slice/shape headers in the Store-wide pointer-free external descriptor
// block. Public Open deliberately keeps its existing append-capable layout.
func openDocSetIntoStore(set *DocSet, data []byte, mapped *storeMappedDocs, base uint64) error {
	return openDocSetIntoMode(set, data, mapped, base)
}

func openDocSetIntoMode(set *DocSet, data []byte, mapped *storeMappedDocs, mappedBase uint64) error {
	if uint64(len(data)) < persistHeaderLen+persistFooterLen {
		return fmt.Errorf("%w: image shorter than its framing", ErrPersistCorrupt)
	}
	if string(data[0:8]) != persistHeaderMagic {
		return fmt.Errorf("%w: header magic", ErrPersistMagic)
	}
	if v := binary.LittleEndian.Uint32(data[8:12]); v != persistVersion {
		return fmt.Errorf("%w: header version %d != %d", ErrPersistVersion, v, persistVersion)
	}

	footer := data[uint64(len(data))-persistFooterLen:]
	if string(footer[0:8]) != persistFooterMagic {
		return fmt.Errorf("%w: footer magic", ErrPersistMagic)
	}
	if v := binary.LittleEndian.Uint32(footer[32:36]); v != persistVersion {
		return fmt.Errorf("%w: footer version %d != %d", ErrPersistVersion, v, persistVersion)
	}
	manifestOff := binary.LittleEndian.Uint64(footer[8:16])
	manifestLen := binary.LittleEndian.Uint64(footer[16:24])
	checksum := binary.LittleEndian.Uint64(footer[24:32])

	limit := uint64(len(data)) - persistFooterLen
	if manifestOff < persistHeaderLen || manifestLen < persistManifestFixed ||
		manifestOff > limit || manifestLen > limit-manifestOff {
		return fmt.Errorf("%w: manifest span out of range", ErrPersistCorrupt)
	}
	manifest := data[manifestOff : manifestOff+manifestLen]
	if persistChecksum(manifest) != checksum {
		return fmt.Errorf("%w: manifest checksum", ErrPersistCorrupt)
	}

	if string(manifest[0:8]) != persistManifestMagic ||
		binary.LittleEndian.Uint32(manifest[8:12]) != persistVersion {
		return fmt.Errorf("%w: manifest header", ErrPersistCorrupt)
	}
	flags := binary.LittleEndian.Uint32(manifest[12:16])
	maxDepth := int64(binary.LittleEndian.Uint64(manifest[16:24]))
	valueFloor := binary.LittleEndian.Uint32(manifest[24:28])
	docCount := binary.LittleEndian.Uint32(manifest[32:36])
	narrowTotal := binary.LittleEndian.Uint32(manifest[36:40])
	shapeTableOffset := binary.LittleEndian.Uint64(manifest[40:48])
	shapeTableLength := binary.LittleEndian.Uint64(manifest[48:56])

	// The offsets index must fit the manifest, and the shape table must lie in
	// the records region before the manifest; both bound the untrusted counts
	// against real bytes so no allocation trusts the header. The span check is
	// unconditional so even a zero-length table cannot slice past the image.
	if uint64(docCount) > (manifestLen-persistManifestFixed)/8 {
		return fmt.Errorf("%w: document count exceeds manifest", ErrPersistCorrupt)
	}
	if shapeTableOffset > manifestOff || shapeTableLength > manifestOff-shapeTableOffset ||
		(shapeTableLength != 0 && shapeTableOffset < persistHeaderLen) {
		return fmt.Errorf("%w: shape table span out of range", ErrPersistCorrupt)
	}
	if uint64(narrowTotal) > limit/uint64(unsafe.Sizeof(shapeNarrowValue{})) {
		return fmt.Errorf("%w: narrow value total exceeds image", ErrPersistCorrupt)
	}

	compact := mapped != nil && persistNativeLittleEndian
	*set = DocSet{source: data}
	if compact {
		set.mappedDocs = mapped
		set.mappedBase = mappedBase
		set.mappedCount = int(docCount)
		if mappedBase+uint64(docCount) > uint64(len(mapped.refs)) {
			return fmt.Errorf("%w: Store document directory span", ErrPersistCorrupt)
		}
	}
	set.ShapeTapes = flags&persistFlagShapeTapes != 0
	set.wideValueTapes = flags&persistFlagWideValueTapes != 0
	set.Options = document.IndexOptions{HashKeys: flags&persistFlagHashKeys != 0}
	if maxDepth > 0 {
		set.Options.MaxDepth = int(maxDepth)
	}
	set.valueFloor = valueFloor

	shapeRecs, err := set.openShapes(data[shapeTableOffset : shapeTableOffset+shapeTableLength])
	if err != nil {
		return err
	}

	if compact {
		set.mappedShapes = shapeRecs
	} else {
		set.docs = make([]Index, docCount)
		set.narrow = make([]shapeNarrowValue, 0, narrowTotal)
	}
	var tapeRefs []shapeTapeRef
	if !compact {
		tapeRefs = make([]shapeTapeRef, docCount)
	}
	hasShape := false
	for i := 0; i < int(docCount); i++ {
		recOff := binary.LittleEndian.Uint64(manifest[persistManifestFixed+8*i:])
		ref, err := set.openDocRecord(data, recOff, manifestOff, shapeRecs, i, compact)
		if err != nil {
			return err
		}
		if ref.rec != nil && !compact {
			tapeRefs[i] = ref
			hasShape = true
		}
	}
	// tapeRefs stays empty unless some document is shape-taped, matching the
	// commit-time invariant that it is either empty or exactly docs-aligned.
	if hasShape {
		set.tapeRefs = tapeRefs
	}

	set.rebuildAccelerators(flags)
	return nil
}

// openDocRecord reconstructs document i from the record at recOff, storing its
// Index in set.docs, appending any narrow values to the shared slab, and
// returning its shape header (the zero ref for a classic document). It bounds
// every span against the image so a malformed record fails closed rather than
// aliasing out of range.
func (set *DocSet) openDocRecord(data []byte, recOff, recLimit uint64, shapeRecs []*shapeRecord, i int, compact bool) (shapeTapeRef, error) {
	if recOff < persistHeaderLen || recOff&7 != 0 || recOff > recLimit || recLimit-recOff < persistRecordHeaderLen {
		return shapeTapeRef{}, fmt.Errorf("%w: record %d header out of range", ErrPersistCorrupt, i)
	}
	h := data[recOff : recOff+persistRecordHeaderLen]
	srcLen := uint64(binary.LittleEndian.Uint32(h[0:4]))
	entryCount := uint64(binary.LittleEndian.Uint32(h[4:8]))
	start := binary.LittleEndian.Uint32(h[8:12])
	end := binary.LittleEndian.Uint32(h[12:16])
	sid := binary.LittleEndian.Uint32(h[16:20])
	kind := h[20]
	enriched := h[21] != 0

	srcOff := recOff + persistRecordHeaderLen
	if srcLen > recLimit-srcOff {
		return shapeTapeRef{}, fmt.Errorf("%w: record %d source out of range", ErrPersistCorrupt, i)
	}
	src := data[srcOff : srcOff+srcLen : srcOff+srcLen]

	entriesOff := persistAlign8(srcOff + srcLen)
	var width uint64
	switch kind {
	case persistDocNarrow:
		width = uint64(unsafe.Sizeof(shapeNarrowValue{}))
	default:
		width = uint64(unsafe.Sizeof(IndexEntry{}))
	}
	if entriesOff > recLimit || entryCount > (recLimit-entriesOff)/width {
		return shapeTapeRef{}, fmt.Errorf("%w: record %d entries out of range", ErrPersistCorrupt, i)
	}

	switch kind {
	case persistDocClassic:
		if compact {
			set.mappedDocs.refs[set.mappedBase+uint64(i)] = storeMappedDocRef{
				sourceOff: srcOff, srcLen: uint32(srcLen),
				entryCount: uint32(entryCount), shapeID: storeMappedNoShape, kind: kind,
			}
			return shapeTapeRef{}, nil
		}
		set.docs[i] = Index{src: src, entries: openEntries(data, entriesOff, entryCount)}
		return shapeTapeRef{}, nil
	case persistDocWide:
		rec, err := shapeAt(shapeRecs, sid, i)
		if err != nil {
			return shapeTapeRef{}, err
		}
		if entryCount != uint64(len(rec.fields)) {
			return shapeTapeRef{}, fmt.Errorf("%w: record %d value count != shape width", ErrPersistCorrupt, i)
		}
		if compact {
			set.mappedDocs.refs[set.mappedBase+uint64(i)] = storeMappedDocRef{
				sourceOff: srcOff, srcLen: uint32(srcLen),
				entryCount: uint32(entryCount), start: start, end: end, shapeID: sid,
				kind: kind, enriched: enriched,
			}
			return shapeTapeRef{rec: rec, start: start, end: end, enriched: enriched}, nil
		}
		set.docs[i] = Index{src: src, entries: openEntries(data, entriesOff, entryCount)}
		return shapeTapeRef{rec: rec, start: start, end: end, enriched: enriched}, nil
	case persistDocNarrow:
		rec, err := shapeAt(shapeRecs, sid, i)
		if err != nil {
			return shapeTapeRef{}, err
		}
		if entryCount != uint64(len(rec.fields)) {
			return shapeTapeRef{}, fmt.Errorf("%w: record %d narrow count != shape width", ErrPersistCorrupt, i)
		}
		if compact {
			set.mappedDocs.refs[set.mappedBase+uint64(i)] = storeMappedDocRef{
				sourceOff: srcOff, srcLen: uint32(srcLen),
				entryCount: uint32(entryCount), start: start, end: end, shapeID: sid,
				kind: kind, enriched: enriched,
			}
			set.mappedNarrow += int(entryCount)
			return shapeTapeRef{rec: rec, start: start, end: end, narrow: true, enriched: enriched}, nil
		}
		slabOff := uint32(len(set.narrow))
		set.narrow = appendNarrow(set.narrow, data, entriesOff, entryCount)
		set.docs[i] = Index{src: src}
		return shapeTapeRef{rec: rec, start: start, end: end, off: slabOff, narrow: true, enriched: enriched}, nil
	default:
		return shapeTapeRef{}, fmt.Errorf("%w: record %d unknown storage class %d", ErrPersistCorrupt, i, kind)
	}
}

// shapeAt resolves a record's shape id against the reconstructed table.
func shapeAt(shapeRecs []*shapeRecord, sid uint32, doc int) (*shapeRecord, error) {
	if uint64(sid) >= uint64(len(shapeRecs)) {
		return nil, fmt.Errorf("%w: record %d shape id %d out of range", ErrPersistCorrupt, doc, sid)
	}
	return shapeRecs[sid], nil
}

// openShapes rebuilds the shared shapes from the serialized key spellings. Each
// shape is reconstructed by resolving a synthetic flat object of its keys back
// through the set's ShapeCache — the identical machinery that compiled it —
// which reproduces the fingerprint, decoded names, info words, name table, and
// duplicate-key flag exactly, so the reopened cache resolves and continues to
// Append against the same shapes. Records are reconstructed in id order, so a
// record's stored shape id indexes the returned slice directly.
func (set *DocSet) openShapes(table []byte) ([]*shapeRecord, error) {
	if len(table) == 0 {
		return nil, nil
	}
	r := persistReader{b: table, ok: true}
	shapeCount := r.u32()
	if !r.ok || uint64(shapeCount) > uint64(len(table))/8 {
		return nil, fmt.Errorf("%w: shape count exceeds table", ErrPersistCorrupt)
	}
	recs := make([]*shapeRecord, 0, shapeCount)
	var synth []byte
	for k := uint32(0); k < shapeCount; k++ {
		fieldCount := r.u32()
		if !r.ok || fieldCount == 0 || fieldCount > shapeMaxFields {
			return nil, fmt.Errorf("%w: shape %d field count %d", ErrPersistCorrupt, k, fieldCount)
		}
		synth = append(synth[:0], '{')
		for m := uint32(0); m < fieldCount; m++ {
			rawLen := r.u32()
			raw := r.bytes(uint64(rawLen))
			if !r.ok {
				return nil, fmt.Errorf("%w: shape %d key %d out of range", ErrPersistCorrupt, k, m)
			}
			if m > 0 {
				synth = append(synth, ',')
			}
			synth = append(synth, '"')
			synth = append(synth, raw...)
			synth = append(synth, '"', ':', '0')
		}
		synth = append(synth, '}')
		rec, err := set.rebuildShape(synth, int(fieldCount))
		if err != nil {
			return nil, err
		}
		recs = append(recs, rec)
	}
	return recs, nil
}

// rebuildShape compiles one shape from a synthetic object of its keys. The
// values are placeholders — the fingerprint and fields depend only on the keys
// — and the object is resolved twice to clear the cache's sighting gate, so the
// second resolve compiles it; a key sequence already compiled (a duplicate in a
// malformed table) resolves on the first probe and is returned as is, never
// panicking.
func (set *DocSet) rebuildShape(synth []byte, fieldCount int) (*shapeRecord, error) {
	idx, err := buildIndexOptions(synth, make([]IndexEntry, 2*fieldCount+2), document.IndexOptions{})
	if err != nil {
		return nil, fmt.Errorf("%w: shape rebuild: %v", ErrPersistCorrupt, err)
	}
	node := idx.Root()
	if shape, ok := set.shapes.Resolve(node); ok {
		return shape.rec, nil
	}
	shape, ok := set.shapes.Resolve(node)
	if !ok || shape.rec == nil {
		return nil, fmt.Errorf("%w: shape did not compile", ErrPersistCorrupt)
	}
	return shape.rec, nil
}

// rebuildAccelerators reconstructs the opt-in postings and value dictionary
// from the loaded documents when their flags were set, replaying each
// document's ingest hook in ordinal order. Both are pure functions of the
// committed documents (and, for the dictionary, the length floor already
// restored), so the replay reproduces the original structures and the reopened
// set answers WhereExists, WhereContains, DocValue, and Stats identically.
func (set *DocSet) rebuildAccelerators(flags uint32) {
	if flags&persistFlagPostings != 0 {
		set.Postings = true
		for i := 0; i < set.Len(); i++ {
			set.indexPostings(i, set.docAt(i), set.shapeTapeRefAt(i))
		}
	}
	if flags&persistFlagValueDict != 0 {
		set.ValueDict = true
		for i := 0; i < set.Len(); i++ {
			set.valueDictAppend(i, set.shapeTapeRefAt(i))
		}
	}
}

// openEntries returns a 16-byte entry array from the image: a zero-copy view
// aliasing the mapped bytes when the host is little-endian and the address is
// 4-byte aligned, otherwise a decoded copy. The caller has bounded [off, off +
// count*16) within data.
func openEntries(data []byte, off, count uint64) []IndexEntry {
	if count == 0 {
		return nil
	}
	if persistNativeLittleEndian {
		p := unsafe.Pointer(&data[off])
		if uintptr(p)%unsafe.Alignof(IndexEntry{}) == 0 {
			return unsafe.Slice((*IndexEntry)(p), int(count))
		}
	}
	out := make([]IndexEntry, count)
	for i := range out {
		b := data[off+uint64(i)*16:]
		out[i] = IndexEntry{
			start: binary.LittleEndian.Uint32(b[0:4]),
			end:   binary.LittleEndian.Uint32(b[4:8]),
			next:  binary.LittleEndian.Uint32(b[8:12]),
			info:  binary.LittleEndian.Uint32(b[12:16]),
		}
	}
	return out
}

// appendNarrow appends count 8-byte narrow values from the image to dst. The
// consolidated slab cannot alias the scattered records, so the values are
// always copied — natively when the host and alignment allow, else per word.
// The caller has bounded [off, off + count*8) within data.
func appendNarrow(dst []shapeNarrowValue, data []byte, off, count uint64) []shapeNarrowValue {
	if count == 0 {
		return dst
	}
	if persistNativeLittleEndian {
		p := unsafe.Pointer(&data[off])
		if uintptr(p)%unsafe.Alignof(shapeNarrowValue{}) == 0 {
			return append(dst, unsafe.Slice((*shapeNarrowValue)(p), int(count))...)
		}
	}
	for i := uint64(0); i < count; i++ {
		b := data[off+i*8:]
		dst = append(dst, shapeNarrowValue{
			span: binary.LittleEndian.Uint32(b[0:4]),
			info: binary.LittleEndian.Uint32(b[4:8]),
		})
	}
	return dst
}

// A persistReader is a bounds-checked cursor over a section, valid while ok is
// set. A read that would exceed the section clears ok and yields a zero, and a
// read on a cleared cursor is a no-op, so once any read fails every later one
// does too and a caller checks ok once after a run of reads — a truncated
// section can never panic. ok must start true.
type persistReader struct {
	b   []byte
	pos uint64
	ok  bool
}

func (r *persistReader) u32() uint32 {
	if !r.ok || r.pos+4 > uint64(len(r.b)) {
		r.ok = false
		return 0
	}
	v := binary.LittleEndian.Uint32(r.b[r.pos:])
	r.pos += 4
	return v
}

func (r *persistReader) bytes(n uint64) []byte {
	if !r.ok || n > uint64(len(r.b))-r.pos {
		r.ok = false
		return nil
	}
	v := r.b[r.pos : r.pos+n]
	r.pos += n
	return v
}
