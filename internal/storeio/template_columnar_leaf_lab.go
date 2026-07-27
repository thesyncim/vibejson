package storeio

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
)

// TemplateColumnarLeafLab is an isolated qualification codec. It has no page
// kind, durable version, or production reader wiring.
const (
	templateColumnarLeafLabVersion   = uint16(1)
	templateColumnarLeafLabHeader    = 64
	templateColumnarLeafLabSlots     = 256
	templateColumnarLeafLabEmptyRank = byte(0xff)
	templateColumnarLeafLabRowBytes  = 12
	templateColumnarLeafLabTplBytes  = 48
	templateColumnarLeafLabHoleBytes = 8
	templateColumnarLeafLabColBytes  = 36
)

var (
	templateColumnarLeafLabMagic = [8]byte{'T', 'C', 'L', 'E', 'A', 'F', '1', 0}

	ErrTemplateColumnarLeafLabCorrupt = errors.New("vibejson: corrupt template-columnar leaf lab")
	ErrTemplateColumnarLeafLabShape   = errors.New("vibejson: incompatible template-columnar leaf shape")
)

// TemplateColumnarLeafLabHole describes one typed scalar splice point.
// SkeletonOffset is an offset into Skeleton, not the source document.
type TemplateColumnarLeafLabHole struct {
	SkeletonOffset uint32
	Kind           document.Kind
}

// TemplateColumnarLeafLabExtraction borrows Fields from the source. Skeleton
// is caller-owned and preserves every byte outside scalar values.
type TemplateColumnarLeafLabExtraction struct {
	Skeleton []byte
	Holes    []TemplateColumnarLeafLabHole
	Fields   [][]byte
	ID       [32]byte
}

// ExtractTemplateColumnarLeafLab validates src with BuildIndex and extracts
// all non-key scalar values as typed holes. Whitespace, punctuation, key
// spelling, ordering, escapes, and numeric spelling outside holes are retained
// exactly. Containers are structure, not fields.
func ExtractTemplateColumnarLeafLab(
	src []byte, storage []vibejson.IndexEntry,
) (TemplateColumnarLeafLabExtraction, error) {
	return ExtractTemplateColumnarLeafLabInto(src, storage, nil)
}

// ExtractTemplateColumnarLeafLabInto reuses dst's three slice capacities.
// With sufficient index and result storage, validation plus extraction does
// not allocate.
func ExtractTemplateColumnarLeafLabInto(
	src []byte, storage []vibejson.IndexEntry, dst *TemplateColumnarLeafLabExtraction,
) (TemplateColumnarLeafLabExtraction, error) {
	index, err := vibejson.BuildIndex(src, storage)
	if err != nil {
		return TemplateColumnarLeafLabExtraction{}, err
	}
	return extractTemplateColumnarLeafLabIndex(index, dst)
}

func extractTemplateColumnarLeafLabIndex(
	index vibejson.Index, reuse *TemplateColumnarLeafLabExtraction,
) (TemplateColumnarLeafLabExtraction, error) {
	out := TemplateColumnarLeafLabExtraction{}
	if reuse != nil {
		out.Skeleton = reuse.Skeleton[:0]
		out.Holes = reuse.Holes[:0]
		out.Fields = reuse.Fields[:0]
	}
	cursor := uint32(0)
	for i := range index.Entries {
		e := &index.Entries[i]
		if e.Flags()&vibejson.TapeFlagKey != 0 {
			continue
		}
		switch e.Kind() {
		case document.String, document.Number, document.Bool, document.Null:
		default:
			continue
		}
		if e.Start < cursor || e.End < e.Start || int(e.End) > len(index.Src) {
			return TemplateColumnarLeafLabExtraction{}, ErrTemplateColumnarLeafLabShape
		}
		out.Skeleton = append(out.Skeleton, index.Src[cursor:e.Start]...)
		out.Holes = append(out.Holes, TemplateColumnarLeafLabHole{
			SkeletonOffset: uint32(len(out.Skeleton)), Kind: e.Kind(),
		})
		out.Fields = append(out.Fields, index.Src[e.Start:e.End:e.End])
		cursor = e.End
	}
	out.Skeleton = append(out.Skeleton, index.Src[cursor:]...)
	out.ID = templateColumnarLeafLabTemplateID(out.Skeleton, out.Holes)
	return out, nil
}

func templateColumnarLeafLabTemplateID(
	skeleton []byte, holes []TemplateColumnarLeafLabHole,
) [32]byte {
	// Sum256 is allocation-free. Fold the small typed-hole descriptor into
	// the digest with four independent lanes; exact admission still compares
	// skeleton and holes, so the address is a router rather than authority.
	id := sha256.Sum256(skeleton)
	state := [4]uint64{
		binary.LittleEndian.Uint64(id[0:8]) ^ uint64(len(skeleton)),
		binary.LittleEndian.Uint64(id[8:16]) ^ uint64(len(holes))<<32,
		binary.LittleEndian.Uint64(id[16:24]),
		binary.LittleEndian.Uint64(id[24:32]),
	}
	for i, hole := range holes {
		x := uint64(hole.SkeletonOffset) | uint64(hole.Kind)<<32 | uint64(i)<<40
		lane := i & 3
		state[lane] ^= x + 0x9e3779b97f4a7c15 + state[(lane+3)&3]<<6 +
			state[(lane+3)&3]>>2
		state[lane] = state[lane]<<27 | state[lane]>>(64-27)
	}
	for i := range state {
		binary.LittleEndian.PutUint64(id[i*8:], state[i])
	}
	return id
}

// Append reconstructs the exact original spelling into dst.
func (e TemplateColumnarLeafLabExtraction) Append(dst []byte) ([]byte, bool) {
	if len(e.Holes) != len(e.Fields) {
		return dst, false
	}
	start := uint32(0)
	for i, hole := range e.Holes {
		if hole.SkeletonOffset < start ||
			int(hole.SkeletonOffset) > len(e.Skeleton) {
			return dst, false
		}
		dst = append(dst, e.Skeleton[start:hole.SkeletonOffset]...)
		dst = append(dst, e.Fields[i]...)
		start = hole.SkeletonOffset
	}
	dst = append(dst, e.Skeleton[start:]...)
	return dst, true
}

type TemplateColumnarLeafLabRow struct {
	Slot uint8
	Key  []byte
	JSON []byte
}

type templateColumnarLeafLabTemplate struct {
	id       [32]byte
	skeleton []byte
	holes    []TemplateColumnarLeafLabHole
}

type templateColumnarLeafLabBuildRow struct {
	row       TemplateColumnarLeafLabRow
	template  uint16
	extracted TemplateColumnarLeafLabExtraction
}

type templateColumnarLeafLabBuildColumn struct {
	template uint16
	hole     uint16
	kind     document.Kind
	offsets  []uint32
	values   []byte
	present  [32]byte
	min      []byte
	max      []byte
}

// TemplateColumnarLeafLabRawBytes is the comparable promoted-leaf byte count:
// exact common-leaf structural bytes plus unmodified keys and values.
func TemplateColumnarLeafLabRawBytes(rows []TemplateColumnarLeafLabRow) int {
	if len(rows) == 0 || len(rows) >= CommonPrimaryLeafWideSlots {
		return 0
	}
	class := CommonPrimaryLeafNarrow
	if len(rows) > CommonPrimaryLeafNarrowLive {
		class = CommonPrimaryLeafWide
	}
	extent := 64 << 10
	n := CommonPrimaryLeafStructuralBytes(class, len(rows), extent)
	for _, row := range rows {
		n += len(row.Key) + len(row.JSON)
	}
	return n
}

type TemplateColumnarLeafLabPlan struct {
	UseTemplate   bool
	RawBytes      int
	TemplateBytes int
	SelectedBytes int
}

// PlanTemplateColumnarLeafLab is the deterministic measured-size fallback
// decision. It returns the candidate image as evidence even when raw wins.
func PlanTemplateColumnarLeafLab(
	rows []TemplateColumnarLeafLabRow,
) (TemplateColumnarLeafLabPlan, []byte, error) {
	raw := TemplateColumnarLeafLabRawBytes(rows)
	if raw == 0 {
		return TemplateColumnarLeafLabPlan{}, nil,
			fmt.Errorf("%w: raw geometry", ErrInvalidWrite)
	}
	image, err := EncodeTemplateColumnarLeafLab(rows)
	if err != nil {
		return TemplateColumnarLeafLabPlan{}, nil, err
	}
	plan := TemplateColumnarLeafLabPlan{
		RawBytes: raw, TemplateBytes: len(image), SelectedBytes: raw,
	}
	if len(image) < raw {
		plan.UseTemplate = true
		plan.SelectedBytes = len(image)
	}
	return plan, image, nil
}

// EncodeTemplateColumnarLeafLab creates the isolated candidate image. Rows
// are lexical by key; Slot is stable and independent of rank.
func EncodeTemplateColumnarLeafLab(
	rows []TemplateColumnarLeafLabRow,
) ([]byte, error) {
	if len(rows) == 0 || len(rows) >= templateColumnarLeafLabSlots {
		return nil, fmt.Errorf("%w: row count", ErrInvalidWrite)
	}
	buildRows := make([]templateColumnarLeafLabBuildRow, len(rows))
	templates := make([]templateColumnarLeafLabTemplate, 0, 8)
	var occupied [4]uint64
	for rank, row := range rows {
		if len(row.Key) == 0 || len(row.JSON) == 0 ||
			rank != 0 && bytes.Compare(rows[rank-1].Key, row.Key) >= 0 {
			return nil, fmt.Errorf("%w: key/value/order", ErrInvalidWrite)
		}
		bit := uint64(1) << uint(row.Slot&63)
		if occupied[row.Slot>>6]&bit != 0 {
			return nil, fmt.Errorf("%w: duplicate slot", ErrInvalidWrite)
		}
		occupied[row.Slot>>6] |= bit
		storage := make([]vibejson.IndexEntry, len(row.JSON)+2)
		extracted, err := ExtractTemplateColumnarLeafLab(row.JSON, storage)
		if err != nil {
			return nil, err
		}
		template := -1
		for i := range templates {
			if templates[i].id == extracted.ID &&
				bytes.Equal(templates[i].skeleton, extracted.Skeleton) &&
				templateColumnarLeafLabHolesEqual(templates[i].holes, extracted.Holes) {
				template = i
				break
			}
		}
		if template < 0 {
			if len(templates) == math.MaxUint16 {
				return nil, fmt.Errorf("%w: templates", ErrInvalidWrite)
			}
			template = len(templates)
			templates = append(templates, templateColumnarLeafLabTemplate{
				id: extracted.ID, skeleton: extracted.Skeleton, holes: extracted.Holes,
			})
		}
		buildRows[rank] = templateColumnarLeafLabBuildRow{
			row: row, template: uint16(template), extracted: extracted,
		}
	}
	columns := make([]templateColumnarLeafLabBuildColumn, 0)
	columnBase := make([]uint16, len(templates))
	for ti := range templates {
		if len(columns)+len(templates[ti].holes) > math.MaxUint16 {
			return nil, fmt.Errorf("%w: columns", ErrInvalidWrite)
		}
		columnBase[ti] = uint16(len(columns))
		for hi, hole := range templates[ti].holes {
			columns = append(columns, templateColumnarLeafLabBuildColumn{
				template: uint16(ti), hole: uint16(hi), kind: hole.Kind,
				offsets: make([]uint32, len(rows)+1),
			})
		}
	}
	for rank := range buildRows {
		br := &buildRows[rank]
		for ci := range columns {
			columns[ci].offsets[rank+1] = uint32(len(columns[ci].values))
		}
		base := int(columnBase[br.template])
		for hi, field := range br.extracted.Fields {
			col := &columns[base+hi]
			col.present[rank>>3] |= byte(1) << uint(rank&7)
			col.values = append(col.values, field...)
			col.offsets[rank+1] = uint32(len(col.values))
			if col.min == nil || bytes.Compare(field, col.min) < 0 {
				col.min = append(col.min[:0], field...)
			}
			if col.max == nil || bytes.Compare(field, col.max) > 0 {
				col.max = append(col.max[:0], field...)
			}
		}
		for ci := range columns {
			if columns[ci].offsets[rank+1] == 0 && len(columns[ci].values) != 0 {
				columns[ci].offsets[rank+1] = uint32(len(columns[ci].values))
			}
		}
	}
	return encodeTemplateColumnarLeafLabImage(buildRows, templates, columnBase, columns)
}

func templateColumnarLeafLabHolesEqual(a, b []TemplateColumnarLeafLabHole) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func encodeTemplateColumnarLeafLabImage(
	rows []templateColumnarLeafLabBuildRow,
	templates []templateColumnarLeafLabTemplate,
	columnBase []uint16,
	columns []templateColumnarLeafLabBuildColumn,
) ([]byte, error) {
	metadataBytes := templateColumnarLeafLabHeader + templateColumnarLeafLabSlots +
		len(rows) + len(rows)*templateColumnarLeafLabRowBytes
	for _, row := range rows {
		metadataBytes += len(row.row.Key)
	}
	metadataBytes += len(templates) * templateColumnarLeafLabTplBytes
	for _, tpl := range templates {
		metadataBytes += len(tpl.holes) * templateColumnarLeafLabHoleBytes
	}
	metadataBytes += len(columns) * templateColumnarLeafLabColBytes
	for _, col := range columns {
		if len(col.min) > math.MaxUint16 || len(col.max) > math.MaxUint16 {
			return nil, fmt.Errorf("%w: zone value too wide", ErrInvalidWrite)
		}
		metadataBytes += len(col.present) + len(col.min) + len(col.max)
	}
	dictionaryBytes := 0
	for _, tpl := range templates {
		dictionaryBytes += len(tpl.skeleton)
	}
	dataBytes := 0
	for _, col := range columns {
		dataBytes += 4*len(col.offsets) + len(col.values)
	}
	checksumBytes := 4 * (3 + len(columns))
	total := metadataBytes + dictionaryBytes + dataBytes + checksumBytes
	if total > math.MaxUint32 {
		return nil, fmt.Errorf("%w: image too large", ErrInvalidWrite)
	}
	image := make([]byte, total)
	copy(image[:8], templateColumnarLeafLabMagic[:])
	binary.LittleEndian.PutUint16(image[8:10], templateColumnarLeafLabVersion)
	binary.LittleEndian.PutUint16(image[10:12], templateColumnarLeafLabHeader)
	binary.LittleEndian.PutUint16(image[12:14], uint16(len(rows)))
	binary.LittleEndian.PutUint16(image[14:16], uint16(len(templates)))
	binary.LittleEndian.PutUint16(image[16:18], uint16(len(columns)))
	binary.LittleEndian.PutUint32(image[20:24], uint32(metadataBytes))
	binary.LittleEndian.PutUint32(image[24:28], uint32(metadataBytes+dictionaryBytes))
	binary.LittleEndian.PutUint32(image[28:32], uint32(metadataBytes+dictionaryBytes+dataBytes))
	binary.LittleEndian.PutUint32(image[32:36], uint32(total))
	cursor := templateColumnarLeafLabHeader
	clear(image[cursor : cursor+templateColumnarLeafLabSlots])
	for i := range templateColumnarLeafLabSlots {
		image[cursor+i] = templateColumnarLeafLabEmptyRank
	}
	for rank, row := range rows {
		image[cursor+int(row.row.Slot)] = byte(rank)
	}
	cursor += templateColumnarLeafLabSlots
	for _, row := range rows {
		image[cursor] = row.row.Slot
		cursor++
	}
	rowDir := cursor
	cursor += len(rows) * templateColumnarLeafLabRowBytes
	for rank, row := range rows {
		start := cursor
		cursor += copy(image[cursor:], row.row.Key)
		at := rowDir + rank*templateColumnarLeafLabRowBytes
		binary.LittleEndian.PutUint32(image[at:at+4], uint32(start))
		binary.LittleEndian.PutUint32(image[at+4:at+8], uint32(cursor))
		binary.LittleEndian.PutUint16(image[at+8:at+10], row.template)
	}
	tplDir := cursor
	cursor += len(templates) * templateColumnarLeafLabTplBytes
	holeStart := cursor
	holeOrdinal := 0
	for ti, tpl := range templates {
		at := tplDir + ti*templateColumnarLeafLabTplBytes
		copy(image[at:at+32], tpl.id[:])
		binary.LittleEndian.PutUint32(image[at+32:at+36], uint32(holeOrdinal))
		binary.LittleEndian.PutUint16(image[at+36:at+38], uint16(len(tpl.holes)))
		binary.LittleEndian.PutUint16(image[at+38:at+40], columnBase[ti])
		for _, hole := range tpl.holes {
			ha := holeStart + holeOrdinal*templateColumnarLeafLabHoleBytes
			binary.LittleEndian.PutUint32(image[ha:ha+4], hole.SkeletonOffset)
			image[ha+4] = byte(hole.Kind)
			holeOrdinal++
		}
	}
	cursor += holeOrdinal * templateColumnarLeafLabHoleBytes
	colDir := cursor
	cursor += len(columns) * templateColumnarLeafLabColBytes
	for ci, col := range columns {
		at := colDir + ci*templateColumnarLeafLabColBytes
		binary.LittleEndian.PutUint16(image[at:at+2], col.template)
		binary.LittleEndian.PutUint16(image[at+2:at+4], col.hole)
		image[at+4] = byte(col.kind)
		binary.LittleEndian.PutUint32(image[at+8:at+12], uint32(cursor))
		cursor += copy(image[cursor:], col.present[:])
		binary.LittleEndian.PutUint32(image[at+12:at+16], uint32(cursor))
		binary.LittleEndian.PutUint16(image[at+16:at+18], uint16(len(col.min)))
		cursor += copy(image[cursor:], col.min)
		binary.LittleEndian.PutUint32(image[at+20:at+24], uint32(cursor))
		binary.LittleEndian.PutUint16(image[at+24:at+26], uint16(len(col.max)))
		cursor += copy(image[cursor:], col.max)
	}
	if cursor != metadataBytes {
		panic("template-columnar lab metadata size mismatch")
	}
	dictCursor := metadataBytes
	for ti, tpl := range templates {
		at := tplDir + ti*templateColumnarLeafLabTplBytes
		binary.LittleEndian.PutUint32(image[at+40:at+44], uint32(dictCursor))
		dictCursor += copy(image[dictCursor:], tpl.skeleton)
		binary.LittleEndian.PutUint32(image[at+44:at+48], uint32(dictCursor))
	}
	dataCursor := metadataBytes + dictionaryBytes
	for ci, col := range columns {
		at := colDir + ci*templateColumnarLeafLabColBytes
		binary.LittleEndian.PutUint32(image[at+28:at+32], uint32(dataCursor))
		for _, off := range col.offsets {
			binary.LittleEndian.PutUint32(image[dataCursor:dataCursor+4], off)
			dataCursor += 4
		}
		dataCursor += copy(image[dataCursor:], col.values)
		binary.LittleEndian.PutUint32(image[at+32:at+36], uint32(dataCursor))
	}
	if dataCursor != metadataBytes+dictionaryBytes+dataBytes {
		panic("template-columnar lab data size mismatch")
	}
	sealTemplateColumnarLeafLab(image)
	return image, nil
}

// TemplateColumnarLeafLabView is fully checked on Open and then provides
// allocation-free borrowed field access and bounded reconstruction.
type TemplateColumnarLeafLabView struct {
	image       []byte
	count       uint16
	templates   uint16
	columns     uint16
	metadataEnd uint32
	dictEnd     uint32
	dataEnd     uint32
	slotRanks   []byte
	rankSlots   []byte
	rowDir      []byte
	tplDir      []byte
	holeDir     []byte
	colDir      []byte
}

type TemplateColumnarLeafLabZone struct {
	Kind     document.Kind
	Min      []byte
	Max      []byte
	Presence []byte
}

func sealTemplateColumnarLeafLab(image []byte) {
	metadataEnd := int(binary.LittleEndian.Uint32(image[20:24]))
	dictEnd := int(binary.LittleEndian.Uint32(image[24:28]))
	dataEnd := int(binary.LittleEndian.Uint32(image[28:32]))
	columns := int(binary.LittleEndian.Uint16(image[16:18]))
	table := image[dataEnd:]
	binary.LittleEndian.PutUint32(table[0:4], PageChecksum(image[:metadataEnd]))
	binary.LittleEndian.PutUint32(table[4:8], PageChecksum(image[metadataEnd:dictEnd]))
	view, _ := parseTemplateColumnarLeafLabDirectories(image)
	for ci := range columns {
		start, end := view.columnRegion(ci)
		binary.LittleEndian.PutUint32(table[8+ci*4:], PageChecksum(image[start:end]))
	}
	rootAt := 8 + columns*4
	binary.LittleEndian.PutUint32(table[rootAt:], PageChecksum(table[:rootAt]))
}

func OpenTemplateColumnarLeafLab(src []byte) (TemplateColumnarLeafLabView, error) {
	view, ok := parseTemplateColumnarLeafLabDirectories(src)
	if !ok {
		return TemplateColumnarLeafLabView{}, ErrTemplateColumnarLeafLabCorrupt
	}
	table := src[view.dataEnd:]
	if PageChecksum(src[:view.metadataEnd]) != binary.LittleEndian.Uint32(table[0:4]) ||
		PageChecksum(src[view.metadataEnd:view.dictEnd]) != binary.LittleEndian.Uint32(table[4:8]) {
		return TemplateColumnarLeafLabView{}, ErrTemplateColumnarLeafLabCorrupt
	}
	for ci := 0; ci < int(view.columns); ci++ {
		start, end := view.columnRegion(ci)
		if start < int(view.dictEnd) || end < start || end > int(view.dataEnd) ||
			PageChecksum(src[start:end]) != binary.LittleEndian.Uint32(table[8+ci*4:]) {
			return TemplateColumnarLeafLabView{}, ErrTemplateColumnarLeafLabCorrupt
		}
	}
	rootAt := 8 + int(view.columns)*4
	if PageChecksum(table[:rootAt]) != binary.LittleEndian.Uint32(table[rootAt:]) ||
		!view.validate() {
		return TemplateColumnarLeafLabView{}, ErrTemplateColumnarLeafLabCorrupt
	}
	return view, nil
}

func parseTemplateColumnarLeafLabDirectories(src []byte) (TemplateColumnarLeafLabView, bool) {
	if len(src) < templateColumnarLeafLabHeader+templateColumnarLeafLabSlots+12 ||
		!bytes.Equal(src[:8], templateColumnarLeafLabMagic[:]) ||
		binary.LittleEndian.Uint16(src[8:10]) != templateColumnarLeafLabVersion ||
		binary.LittleEndian.Uint16(src[10:12]) != templateColumnarLeafLabHeader {
		return TemplateColumnarLeafLabView{}, false
	}
	count := int(binary.LittleEndian.Uint16(src[12:14]))
	templates := int(binary.LittleEndian.Uint16(src[14:16]))
	columns := int(binary.LittleEndian.Uint16(src[16:18]))
	metadataEnd := int(binary.LittleEndian.Uint32(src[20:24]))
	dictEnd := int(binary.LittleEndian.Uint32(src[24:28]))
	dataEnd := int(binary.LittleEndian.Uint32(src[28:32]))
	total := int(binary.LittleEndian.Uint32(src[32:36]))
	if count == 0 || count >= templateColumnarLeafLabSlots || templates == 0 ||
		metadataEnd < templateColumnarLeafLabHeader+templateColumnarLeafLabSlots+count+
			count*templateColumnarLeafLabRowBytes+templates*templateColumnarLeafLabTplBytes+
			columns*templateColumnarLeafLabColBytes ||
		metadataEnd > dictEnd || dictEnd > dataEnd ||
		total != len(src) || dataEnd+4*(3+columns) != total {
		return TemplateColumnarLeafLabView{}, false
	}
	cursor := templateColumnarLeafLabHeader
	slotRanks := src[cursor : cursor+templateColumnarLeafLabSlots]
	cursor += templateColumnarLeafLabSlots
	rankSlots := src[cursor : cursor+count]
	cursor += count
	rowDir := src[cursor : cursor+count*templateColumnarLeafLabRowBytes]
	cursor += len(rowDir)
	keyEnd := cursor
	for rank := range count {
		at := rank * templateColumnarLeafLabRowBytes
		end := int(binary.LittleEndian.Uint32(rowDir[at+4 : at+8]))
		if end > keyEnd {
			keyEnd = end
		}
	}
	if keyEnd < cursor || keyEnd > metadataEnd {
		return TemplateColumnarLeafLabView{}, false
	}
	cursor = keyEnd
	tplDir := src[cursor : cursor+templates*templateColumnarLeafLabTplBytes]
	cursor += len(tplDir)
	holeCount := 0
	for ti := range templates {
		at := ti * templateColumnarLeafLabTplBytes
		first := int(binary.LittleEndian.Uint32(tplDir[at+32 : at+36]))
		n := int(binary.LittleEndian.Uint16(tplDir[at+36 : at+38]))
		if first != holeCount {
			return TemplateColumnarLeafLabView{}, false
		}
		holeCount += n
	}
	if cursor+holeCount*templateColumnarLeafLabHoleBytes+
		columns*templateColumnarLeafLabColBytes > metadataEnd {
		return TemplateColumnarLeafLabView{}, false
	}
	holeDir := src[cursor : cursor+holeCount*templateColumnarLeafLabHoleBytes]
	cursor += len(holeDir)
	colDir := src[cursor : cursor+columns*templateColumnarLeafLabColBytes]
	return TemplateColumnarLeafLabView{
		image: src, count: uint16(count), templates: uint16(templates),
		columns: uint16(columns), metadataEnd: uint32(metadataEnd),
		dictEnd: uint32(dictEnd), dataEnd: uint32(dataEnd),
		slotRanks: slotRanks, rankSlots: rankSlots, rowDir: rowDir,
		tplDir: tplDir, holeDir: holeDir, colDir: colDir,
	}, true
}

func (v TemplateColumnarLeafLabView) validate() bool {
	var seen [4]uint64
	var previous []byte
	for rank := 0; rank < int(v.count); rank++ {
		slot := v.rankSlots[rank]
		if v.slotRanks[slot] != byte(rank) {
			return false
		}
		bit := uint64(1) << uint(slot&63)
		if seen[slot>>6]&bit != 0 {
			return false
		}
		seen[slot>>6] |= bit
		key, template, ok := v.row(rank)
		if !ok || rank != 0 && bytes.Compare(previous, key) >= 0 ||
			int(template) >= int(v.templates) {
			return false
		}
		previous = key
	}
	for slot := range templateColumnarLeafLabSlots {
		rank := v.slotRanks[slot]
		if rank != templateColumnarLeafLabEmptyRank &&
			(int(rank) >= int(v.count) || v.rankSlots[rank] != byte(slot)) {
			return false
		}
	}
	for ti := 0; ti < int(v.templates); ti++ {
		at := ti * templateColumnarLeafLabTplBytes
		first := int(binary.LittleEndian.Uint32(v.tplDir[at+32 : at+36]))
		n := int(binary.LittleEndian.Uint16(v.tplDir[at+36 : at+38]))
		start := int(binary.LittleEndian.Uint32(v.tplDir[at+40 : at+44]))
		end := int(binary.LittleEndian.Uint32(v.tplDir[at+44 : at+48]))
		if start < int(v.metadataEnd) || end < start || end > int(v.dictEnd) ||
			first+n > len(v.holeDir)/templateColumnarLeafLabHoleBytes {
			return false
		}
		var prior uint32
		for i := 0; i < n; i++ {
			ha := (first + i) * templateColumnarLeafLabHoleBytes
			offset := binary.LittleEndian.Uint32(v.holeDir[ha : ha+4])
			kind := document.Kind(v.holeDir[ha+4])
			if int(offset) > end-start || i != 0 && offset < prior ||
				kind < document.Null || kind > document.String {
				return false
			}
			prior = offset
		}
		want := templateColumnarLeafLabTemplateIDEncoded(
			v.image[start:end], v.holeDir, first, n,
		)
		if !bytes.Equal(want[:], v.tplDir[at:at+32]) {
			return false
		}
	}
	for ci := 0; ci < int(v.columns); ci++ {
		start, end := v.columnRegion(ci)
		if start < int(v.dictEnd) || end < start+4*(int(v.count)+1) ||
			end > int(v.dataEnd) {
			return false
		}
		offsets := v.image[start : start+4*(int(v.count)+1)]
		values := v.image[start+len(offsets) : end]
		prior := uint32(0)
		for i := 0; i <= int(v.count); i++ {
			off := binary.LittleEndian.Uint32(offsets[i*4:])
			if off < prior || int(off) > len(values) {
				return false
			}
			prior = off
		}
		if int(prior) != len(values) {
			return false
		}
		at := ci * templateColumnarLeafLabColBytes
		template := binary.LittleEndian.Uint16(v.colDir[at : at+2])
		hole := binary.LittleEndian.Uint16(v.colDir[at+2 : at+4])
		zone, ok := v.Zone(template, hole)
		if !ok || int(template) >= int(v.templates) {
			return false
		}
		var gotMin, gotMax []byte
		for rank := 0; rank < int(v.count); rank++ {
			_, rowTemplate, rowOK := v.row(rank)
			a := int(binary.LittleEndian.Uint32(offsets[rank*4:]))
			b := int(binary.LittleEndian.Uint32(offsets[(rank+1)*4:]))
			present := zone.Presence[rank>>3]&(byte(1)<<uint(rank&7)) != 0
			wantPresent := rowOK && rowTemplate == template
			if present != wantPresent || (wantPresent && a == b) ||
				(!wantPresent && a != b) {
				return false
			}
			if !wantPresent {
				continue
			}
			field := values[a:b]
			if gotMin == nil || bytes.Compare(field, gotMin) < 0 {
				gotMin = field
			}
			if gotMax == nil || bytes.Compare(field, gotMax) > 0 {
				gotMax = field
			}
		}
		usedPresence := (int(v.count) + 7) / 8
		if !bytes.Equal(gotMin, zone.Min) || !bytes.Equal(gotMax, zone.Max) ||
			!commonPrimaryLeafUnusedBitsZero(zone.Presence[:usedPresence], int(v.count)) ||
			!allZero(zone.Presence[usedPresence:]) {
			return false
		}
	}
	return true
}

func (v TemplateColumnarLeafLabView) row(rank int) ([]byte, uint16, bool) {
	if rank < 0 || rank >= int(v.count) {
		return nil, 0, false
	}
	at := rank * templateColumnarLeafLabRowBytes
	start := int(binary.LittleEndian.Uint32(v.rowDir[at : at+4]))
	end := int(binary.LittleEndian.Uint32(v.rowDir[at+4 : at+8]))
	template := binary.LittleEndian.Uint16(v.rowDir[at+8 : at+10])
	if start < 0 || end <= start || end > int(v.metadataEnd) {
		return nil, 0, false
	}
	return v.image[start:end:end], template, true
}

func templateColumnarLeafLabTemplateIDEncoded(
	skeleton, holeDir []byte, first, count int,
) [32]byte {
	id := sha256.Sum256(skeleton)
	state := [4]uint64{
		binary.LittleEndian.Uint64(id[0:8]) ^ uint64(len(skeleton)),
		binary.LittleEndian.Uint64(id[8:16]) ^ uint64(count)<<32,
		binary.LittleEndian.Uint64(id[16:24]),
		binary.LittleEndian.Uint64(id[24:32]),
	}
	for i := 0; i < count; i++ {
		at := (first + i) * templateColumnarLeafLabHoleBytes
		offset := binary.LittleEndian.Uint32(holeDir[at : at+4])
		kind := holeDir[at+4]
		x := uint64(offset) | uint64(kind)<<32 | uint64(i)<<40
		lane := i & 3
		state[lane] ^= x + 0x9e3779b97f4a7c15 + state[(lane+3)&3]<<6 +
			state[(lane+3)&3]>>2
		state[lane] = state[lane]<<27 | state[lane]>>(64-27)
	}
	for i := range state {
		binary.LittleEndian.PutUint64(id[i*8:], state[i])
	}
	return id
}

func (v TemplateColumnarLeafLabView) columnRegion(index int) (int, int) {
	at := index * templateColumnarLeafLabColBytes
	return int(binary.LittleEndian.Uint32(v.colDir[at+28 : at+32])),
		int(binary.LittleEndian.Uint32(v.colDir[at+32 : at+36]))
}

func (v TemplateColumnarLeafLabView) columnFor(template, hole uint16) (int, bool) {
	if int(template) >= int(v.templates) {
		return 0, false
	}
	at := int(template) * templateColumnarLeafLabTplBytes
	count := binary.LittleEndian.Uint16(v.tplDir[at+36 : at+38])
	base := binary.LittleEndian.Uint16(v.tplDir[at+38 : at+40])
	if hole >= count || int(base)+int(hole) >= int(v.columns) {
		return 0, false
	}
	return int(base + hole), true
}

// Zone returns the exact bytewise min/max and rank presence mask for one
// template field. Numeric spellings deliberately remain bytewise in this lab;
// promotion would need the design's canonical-number specialization.
func (v TemplateColumnarLeafLabView) Zone(
	template, hole uint16,
) (TemplateColumnarLeafLabZone, bool) {
	ci, ok := v.columnFor(template, hole)
	if !ok {
		return TemplateColumnarLeafLabZone{}, false
	}
	at := ci * templateColumnarLeafLabColBytes
	presenceStart := int(binary.LittleEndian.Uint32(v.colDir[at+8 : at+12]))
	minStart := int(binary.LittleEndian.Uint32(v.colDir[at+12 : at+16]))
	minLen := int(binary.LittleEndian.Uint16(v.colDir[at+16 : at+18]))
	maxStart := int(binary.LittleEndian.Uint32(v.colDir[at+20 : at+24]))
	maxLen := int(binary.LittleEndian.Uint16(v.colDir[at+24 : at+26]))
	if presenceStart < 0 || presenceStart+32 > int(v.metadataEnd) ||
		minStart < presenceStart+32 || minStart+minLen > int(v.metadataEnd) ||
		maxStart < minStart+minLen || maxStart+maxLen > int(v.metadataEnd) {
		return TemplateColumnarLeafLabZone{}, false
	}
	return TemplateColumnarLeafLabZone{
		Kind:     document.Kind(v.colDir[at+4]),
		Presence: v.image[presenceStart : presenceStart+32 : presenceStart+32],
		Min:      v.image[minStart : minStart+minLen : minStart+minLen],
		Max:      v.image[maxStart : maxStart+maxLen : maxStart+maxLen],
	}, true
}

// Field returns one exact borrowed scalar through the column offset index.
func (v TemplateColumnarLeafLabView) Field(
	slot uint8, hole uint16,
) ([]byte, document.Kind, bool) {
	rank := v.slotRanks[slot]
	if rank == templateColumnarLeafLabEmptyRank || int(rank) >= int(v.count) {
		return nil, document.Invalid, false
	}
	_, template, ok := v.row(int(rank))
	if !ok {
		return nil, document.Invalid, false
	}
	ci, ok := v.columnFor(template, hole)
	if !ok {
		return nil, document.Invalid, false
	}
	at := ci * templateColumnarLeafLabColBytes
	start, end := v.columnRegion(ci)
	offsets := v.image[start : start+4*(int(v.count)+1)]
	a := int(binary.LittleEndian.Uint32(offsets[int(rank)*4:]))
	b := int(binary.LittleEndian.Uint32(offsets[(int(rank)+1)*4:]))
	values := v.image[start+len(offsets) : end]
	if a > b || b > len(values) || a == b {
		return nil, document.Invalid, false
	}
	return values[a:b:b], document.Kind(v.colDir[at+4]), true
}

// AppendRaw reconstructs one row with bounds checks on every slot offset.
func (v TemplateColumnarLeafLabView) AppendRaw(
	dst []byte, slot uint8, key []byte,
) ([]byte, bool) {
	rank := v.slotRanks[slot]
	if rank == templateColumnarLeafLabEmptyRank || int(rank) >= int(v.count) {
		return dst, false
	}
	got, templateIndex, ok := v.row(int(rank))
	if !ok || !bytes.Equal(got, key) {
		return dst, false
	}
	if int(templateIndex) >= int(v.templates) {
		return dst, false
	}
	tat := int(templateIndex) * templateColumnarLeafLabTplBytes
	first := int(binary.LittleEndian.Uint32(v.tplDir[tat+32 : tat+36]))
	holeCount := int(binary.LittleEndian.Uint16(v.tplDir[tat+36 : tat+38]))
	columnBase := int(binary.LittleEndian.Uint16(v.tplDir[tat+38 : tat+40]))
	skeletonStart := int(binary.LittleEndian.Uint32(v.tplDir[tat+40 : tat+44]))
	skeletonEnd := int(binary.LittleEndian.Uint32(v.tplDir[tat+44 : tat+48]))
	if skeletonStart < int(v.metadataEnd) || skeletonEnd < skeletonStart ||
		skeletonEnd > int(v.dictEnd) ||
		first+holeCount > len(v.holeDir)/templateColumnarLeafLabHoleBytes {
		return dst, false
	}
	skeleton := v.image[skeletonStart:skeletonEnd:skeletonEnd]
	cursor := uint32(0)
	for hi := 0; hi < holeCount; hi++ {
		hat := (first + hi) * templateColumnarLeafLabHoleBytes
		holeOffset := binary.LittleEndian.Uint32(v.holeDir[hat : hat+4])
		if holeOffset < cursor || int(holeOffset) > len(skeleton) {
			return dst, false
		}
		ci := columnBase + hi
		if ci < 0 || ci >= int(v.columns) {
			return dst, false
		}
		start, end := v.columnRegion(ci)
		offsetBytes := 4 * (int(v.count) + 1)
		if start < int(v.dictEnd) || start+offsetBytes > end ||
			end > int(v.dataEnd) {
			return dst, false
		}
		a := int(binary.LittleEndian.Uint32(v.image[start+int(rank)*4:]))
		b := int(binary.LittleEndian.Uint32(v.image[start+(int(rank)+1)*4:]))
		values := v.image[start+offsetBytes : end]
		if a >= b || b > len(values) {
			return dst, false
		}
		dst = append(dst, skeleton[cursor:holeOffset]...)
		dst = append(dst, values[a:b]...)
		cursor = holeOffset
	}
	dst = append(dst, skeleton[cursor:]...)
	return dst, true
}

// PatchFieldFixed replaces a same-width field, reseals that column and the
// checksum root, and leaves all other region checksums untouched.
func PatchTemplateColumnarLeafLabFieldFixed(
	image []byte, slot uint8, hole uint16, replacement []byte,
) error {
	view, err := OpenTemplateColumnarLeafLab(image)
	if err != nil {
		return err
	}
	return patchTemplateColumnarLeafLabFieldFixedAdmitted(view, slot, hole, replacement)
}

func patchTemplateColumnarLeafLabFieldFixedAdmitted(
	view TemplateColumnarLeafLabView, slot uint8, hole uint16, replacement []byte,
) error {
	image := view.image
	rank := view.slotRanks[slot]
	if rank == templateColumnarLeafLabEmptyRank {
		return ErrTemplateColumnarLeafLabShape
	}
	_, template, ok := view.row(int(rank))
	if !ok {
		return ErrTemplateColumnarLeafLabCorrupt
	}
	ci, ok := view.columnFor(template, hole)
	if !ok {
		return ErrTemplateColumnarLeafLabShape
	}
	start, end := view.columnRegion(ci)
	offsetBytes := 4 * (int(view.count) + 1)
	a := int(binary.LittleEndian.Uint32(image[start+int(rank)*4:]))
	b := int(binary.LittleEndian.Uint32(image[start+(int(rank)+1)*4:]))
	if b-a != len(replacement) || start+offsetBytes+b > end {
		return ErrTemplateColumnarLeafLabShape
	}
	old := image[start+offsetBytes+a : start+offsetBytes+b]
	zone, ok := view.Zone(template, hole)
	if !ok || bytes.Compare(replacement, zone.Min) < 0 ||
		bytes.Compare(replacement, zone.Max) > 0 ||
		(!bytes.Equal(old, replacement) &&
			(bytes.Equal(old, zone.Min) || bytes.Equal(old, zone.Max))) {
		// Updating an extremum also dirties zone metadata. This narrow lab path
		// measures the common non-extremum region-only patch; callers must use
		// a whole rebuild when the zone vector changes.
		return ErrTemplateColumnarLeafLabShape
	}
	copy(image[start+offsetBytes+a:start+offsetBytes+b], replacement)
	table := image[view.dataEnd:]
	binary.LittleEndian.PutUint32(table[8+ci*4:], PageChecksum(image[start:end]))
	rootAt := 8 + int(view.columns)*4
	binary.LittleEndian.PutUint32(table[rootAt:], PageChecksum(table[:rootAt]))
	return nil
}

// ResealTemplateColumnarLeafLab recomputes every region and is the benchmark
// comparison for the fixed-field patch path.
func ResealTemplateColumnarLeafLab(image []byte) error {
	if _, ok := parseTemplateColumnarLeafLabDirectories(image); !ok {
		return ErrTemplateColumnarLeafLabCorrupt
	}
	sealTemplateColumnarLeafLab(image)
	return nil
}
