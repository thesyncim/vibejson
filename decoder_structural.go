package vibejson

import (
	"encoding/binary"
	"strings"
	"sync"

	"github.com/thesyncim/vibejson/x/byteview"
	simdkernels "github.com/thesyncim/vibejson/x/kernels"
)

const (
	structuralFieldSlow    uint8 = 0
	structuralFieldMatched uint8 = 1
	structuralFieldEnd     uint8 = 2
	structuralFieldKey     uint8 = 3
	structuralArrayValue   uint8 = 1
	structuralArrayEnd     uint8 = 2

	decoderStructuralTapeRetentionBytes     = 2 << 20
	decoderStructuralTapeRetentionPositions = decoderStructuralTapeRetentionBytes / 4
	decoderStructuralMinBytes               = 4096
	decoderStructuralWindowShift            = 11
	decoderStructuralWindowCount            = decoderStructuralTapeRetentionBytes >> decoderStructuralWindowShift
	decoderStructuralWindowWords            = decoderStructuralWindowCount / 32
)

// Record routing is separate from classifier capability.
const decoderPreferStructuralRecords = simdkernels.Stage1Backend != "scalar" &&
	simdkernels.Stage1Backend != "amd64-runtime"

// decoderStructuralTape is a reusable stage-1 cursor for typed decoding.
type decoderStructuralTape struct {
	positions     []uint32
	index         int
	bad           bool
	nonASCII      bool
	escaped       bool
	detachedColon bool
	// Low and high halves store escape and non-ASCII window bits.
	stringWindows [decoderStructuralWindowWords]uint64
}

var decoderStatePool sync.Pool

func takeDecoderState() *decoderState {
	state, _ := decoderStatePool.Get().(*decoderState)
	if state == nil {
		state = new(decoderState)
	}
	return state
}

// decoderStructuralWorthwhile applies the document-side routing threshold.
func decoderStructuralWorthwhile(src []byte) bool {
	return len(src) >= decoderStructuralMinBytes && uint64(len(src)) <= uint64(^uint32(0))
}

func acquireDecoderState(src []byte) *decoderState {
	state := takeDecoderState()
	state.structuralActive = true
	state.structural.build(src)
	return state
}

func releaseDecoderState(state *decoderState) {
	state.resetOperationState()
	state.structuralActive = false
	state.structural.resetForPool()
	decoderStatePool.Put(state)
}

var spaceBlock64 = [64]byte{
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
	' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ',
}

// resetForPool reuses ordinary tapes and drops oversized buffers.
func (t *decoderStructuralTape) resetForPool() {
	t.index = 0
	t.bad = false
	t.detachedColon = false
	if t.escaped || t.nonASCII {
		clear(t.stringWindows[:])
		t.nonASCII = false
		t.escaped = false
	}
	if cap(t.positions) > decoderStructuralTapeRetentionPositions {
		t.positions = nil
		return
	}
	t.positions = t.positions[:0]
}

func (t *decoderStructuralTape) build(src []byte) {
	t.index = 0
	t.bad = false
	t.detachedColon = false
	t.positions = t.positions[:0]
	if t.escaped || t.nonASCII {
		clear(t.stringWindows[:])
		t.nonASCII = false
		t.escaped = false
	}
	if cap(t.positions) < len(src)/8 {
		t.positions = make([]uint32, 0, len(src)/4)
	}

	n := len(src)
	fullBlocks := n / 64
	var stream simdkernels.Stage1IndexStream

	for chunk := 0; chunk < fullBlocks; chunk += simdkernels.Stage1ChunkBlocks {
		count := fullBlocks - chunk
		if count > simdkernels.Stage1ChunkBlocks {
			count = simdkernels.Stage1ChunkBlocks
		}
		start := len(t.positions)
		need := start + count*64 + 64
		if cap(t.positions) < need {
			capacity := cap(t.positions) * 2
			if capacity < need {
				capacity = need
			}
			grown := make([]uint32, start, capacity)
			copy(grown, t.positions)
			t.positions = grown
		}
		t.positions = t.positions[:need]
		var meta simdkernels.Stage1CursorMeta
		written := simdkernels.Stage1CursorBlocksMeta(
			src[chunk*64:], count, uint32(chunk*64), &stream, t.positions[start:need], &meta,
		)
		t.positions = t.positions[:start+written]
		t.markStringWindow(chunk*64, meta)
	}

	if fullBlocks*64 != n {
		tail := spaceBlock64
		copy(tail[:], src[fullBlocks*64:])
		start := len(t.positions)
		need := start + 128
		if cap(t.positions) < need {
			capacity := cap(t.positions) * 2
			if capacity < need {
				capacity = need
			}
			grown := make([]uint32, start, capacity)
			copy(grown, t.positions)
			t.positions = grown
		}
		t.positions = t.positions[:need]
		var meta simdkernels.Stage1CursorMeta
		written := simdkernels.Stage1CursorBlocksMeta(
			tail[:], 1, uint32(fullBlocks*64), &stream, t.positions[start:need], &meta,
		)
		t.positions = t.positions[:start+written]
		t.markStringWindow(fullBlocks*64, meta)
	}
	t.bad = stream.Bad
	t.nonASCII = stream.NonASCII
	t.escaped = stream.Escaped
	t.detachedColon = stream.DetachedColon
}

func (t *decoderStructuralTape) markStringWindow(base int, meta simdkernels.Stage1CursorMeta) {
	window := base >> decoderStructuralWindowShift
	if uint(window) >= decoderStructuralWindowCount {
		return
	}
	bit := uint64(1) << uint(window&31)
	if meta.Escaped {
		t.stringWindows[window>>5] |= bit
	}
	if meta.NonASCII {
		t.stringWindows[window>>5] |= bit << 32
	}
}

func (t *decoderStructuralTape) stringRangeDirty(start, end int, nonASCII bool) bool {
	if start >= end {
		return false
	}
	first := start >> decoderStructuralWindowShift
	last := (end - 1) >> decoderStructuralWindowShift
	if uint(first) >= decoderStructuralWindowCount || uint(last) >= decoderStructuralWindowCount {
		return true
	}
	shift := uint(0)
	if nonASCII {
		shift = 32
	}
	for window := first; window <= last; window++ {
		if t.stringWindows[window>>5]&(uint64(1)<<uint(window&31)<<shift) != 0 {
			return true
		}
	}
	return false
}

// position returns the first tape position at or after target.
func (t *decoderStructuralTape) position(target, end int) int {
	i := t.index
	positions := t.positions
	for i < len(positions) && int(positions[i]) < target {
		i++
	}
	t.index = i
	if i == len(positions) {
		return end
	}
	return int(positions[i])
}

func (t *decoderStructuralTape) stringEnd(src []byte, start int) (end int, ok bool) {
	i := t.index
	for i < len(t.positions) && int(t.positions[i]) < start {
		i++
	}
	if i+1 >= len(t.positions) || int(t.positions[i]) != start || src[start] != '"' {
		return 0, false
	}
	entry := t.positions[i+1]
	end = int(entry)
	if end >= len(src) || src[end] != '"' {
		return 0, false
	}
	t.index = i + 1
	return end, true
}

func (t *decoderStructuralTape) seekFrom(index, target, end int) (next, position int, ok bool) {
	for index < len(t.positions) && int(t.positions[index]) < target {
		index++
	}
	if index == len(t.positions) {
		return index, end, false
	}
	return index, int(t.positions[index]), true
}

// structuralColonGap validates punctuation omitted from the compact tape.
func structuralColonGap(src []byte, closePosition, valuePosition int) bool {
	if valuePosition > len(src) {
		valuePosition = len(src)
	}
	i := closePosition + 1
	for i < valuePosition && src[i] <= ' ' {
		i++
	}
	if i >= valuePosition || src[i] != ':' {
		return false
	}
	i++
	for i < valuePosition && src[i] <= ' ' {
		i++
	}
	return i == valuePosition
}

// gapClean reports whether src[start:end] contains no elided colon.
// Stage 1 cursor mode omits ':' from structural positions, but tracks whether
// any ':' outside strings was not immediately preceded by a closing '"'. When
// detachedColon is false (the common case for both compact and pretty-printed
// JSON), no whitespace gap after punctuation or a validated value can contain
// an elided colon, so this inlines to a single branch without touching src.
func (t *decoderStructuralTape) gapClean(src []byte, start, end int) bool {
	if !t.detachedColon || start == end {
		return true
	}
	return structuralGapCleanNonEmpty(src, start, end)
}

// structuralGapCleanNonEmpty checks non-empty whitespace gaps using the fact
// that all JSON whitespace bytes (0x09, 0x0a, 0x0d, 0x20) have bit 4 (0x10)
// clear while ':' (0x3a) has bit 4 set.
//
//go:noinline
func structuralGapCleanNonEmpty(src []byte, start, end int) bool {
	if uint(start) > uint(end) || uint(end) > uint(len(src)) {
		return false
	}
	gap := end - start
	if gap <= 8 && start+8 <= len(src) {
		mask := uint64(0x1010101010101010) >> (uint(8-gap) * 8)
		return binary.LittleEndian.Uint64(src[start:start+8])&mask == 0
	}
	if gap >= 8 && gap <= 16 {
		return (binary.LittleEndian.Uint64(src[start:start+8])|
			binary.LittleEndian.Uint64(src[end-8:end]))&0x1010101010101010 == 0
	}
	for start+8 <= end {
		if binary.LittleEndian.Uint64(src[start:start+8])&0x1010101010101010 != 0 {
			return false
		}
		start += 8
	}
	for ; start < end; start++ {
		if src[start] == ':' {
			return false
		}
	}
	return true
}

// structuralFirstValueGapOK verifies the colon-elided first value gap.
func (c *decoderCursor) structuralFirstValueGapOK() bool {
	tape := &c.state.structural
	if !tape.detachedColon {
		return true
	}
	index := tape.index + 1
	if uint(index) >= uint(len(tape.positions)) {
		return true
	}
	src := c.src
	end := int(tape.positions[index])
	if end > len(src) {
		end = len(src)
	}
	for start := c.i; start < end; start++ {
		if src[start] == ':' {
			return false
		}
	}
	return true
}

// syncStructuralValue restores the tape position after generic decoding.
func (c *decoderCursor) syncStructuralValue() {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index
	end := c.i
	for index+1 < len(positions) && int(positions[index+1]) < end {
		index++
	}
	tape.index = index
}

// matchObjectFieldStructural steps one object field on the structural tape.
// It returns structuralFieldMatched on an expected-field hit,
// structuralFieldEnd on '}', structuralFieldKey with the unescaped ASCII key
// when the key is valid but did not match expected, or structuralFieldSlow
// without advancing the cursor when the general path must handle escapes,
// non-ASCII bytes, or syntax errors.
func (c *decoderCursor) matchObjectFieldStructural(first bool, expected *typedField) (string, uint8) {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	if uint(index) >= uint(len(positions)) {
		return "", structuralFieldSlow
	}
	src := c.src
	n := len(src)
	if !first && uint(c.i) < uint(n) {
		tail := src[c.i]
		if tail > ' ' && tail != ',' && tail != '}' {
			return "", structuralFieldSlow
		}
	}
	position := int(positions[index])
	if uint(position) >= uint(n) || !tape.gapClean(src, c.i, position) {
		return "", structuralFieldSlow
	}
	gapStart := c.i
	if !first {
		switch src[position] {
		case '}':
			tape.index = index
			c.i = position + 1
			c.depth--
			return "", structuralFieldEnd
		case ',':
			gapStart = position + 1
			index++
		default:
			return "", structuralFieldSlow
		}
	} else if src[position] == '}' {
		tape.index = index
		c.i = position + 1
		c.depth--
		return "", structuralFieldEnd
	}
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return "", structuralFieldSlow
	}
	// BCE hint: slicing to valueIndex+1 proves all three position loads.
	pos := positions[index : valueIndex+1]
	_ = pos[2]
	openPosition := int(pos[0])
	closePosition := int(pos[1])
	valuePosition := int(pos[2])
	keyStart := openPosition + 1
	if uint(openPosition) >= uint(n) || uint(closePosition) >= uint(n) ||
		closePosition < keyStart ||
		!tape.gapClean(src, gapStart, openPosition) ||
		src[openPosition] != '"' || src[closePosition] != '"' ||
		!structuralColonGap(src, closePosition, valuePosition) {
		return "", structuralFieldSlow
	}
	keyLen := closePosition - keyStart
	if expected != nil && expected.keyMask != 0 && keyLen == int(expected.keyLen) && keyStart+8 <= n {
		mask := expected.keyMask
		if expected.keyLen <= 6 {
			mask >>= 8
		}
		diff := (binary.LittleEndian.Uint64(src[keyStart:keyStart+8]) ^ expected.key) & mask
		if diff == 0 && (keyLen <= 8 || matchStringAt(src, keyStart+8, expected.name[8:])) {
			tape.index = valueIndex
			c.i = valuePosition
			return "", structuralFieldMatched
		}
	}
	if (!tape.escaped && !tape.nonASCII) || c.structuralStringLocallyDirect(keyStart, closePosition) {
		tape.index = valueIndex
		c.i = valuePosition
		return byteview.String(src[keyStart:closePosition]), structuralFieldKey
	}
	return "", structuralFieldSlow
}

func (c *decoderCursor) matchFirstObjectFieldStructuralExpected(expected *typedField) uint8 {
	tape := &c.state.structural
	index := tape.index + 1
	positions := tape.positions
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	pos := positions[index : valueIndex+1]
	_ = pos[2]
	openPosition := int(pos[0])
	src := c.src
	if uint(openPosition) >= uint(len(src)) || !tape.gapClean(src, c.i, openPosition) {
		return structuralFieldSlow
	}
	if src[openPosition] == '}' {
		tape.index = index
		c.i = openPosition + 1
		c.depth--
		return structuralFieldEnd
	}
	closePosition := int(pos[1])
	valuePosition := int(pos[2])
	keyStart := openPosition + 1
	if keyStart+8 > len(src) || closePosition-keyStart != int(expected.keyLen) ||
		uint(closePosition) >= uint(len(src)) ||
		src[openPosition] != '"' || src[closePosition] != '"' ||
		!structuralColonGap(src, closePosition, valuePosition) {
		return structuralFieldSlow
	}
	mask := expected.keyMask
	if expected.keyLen <= 6 {
		mask >>= 8
	}
	if (binary.LittleEndian.Uint64(src[keyStart:keyStart+8])^expected.key)&mask != 0 {
		return structuralFieldSlow
	}
	tape.index = valueIndex
	c.i = valuePosition
	return structuralFieldMatched
}

func (c *decoderCursor) matchNextObjectFieldStructuralExpected(expected *typedField) uint8 {
	tape := &c.state.structural
	index := tape.index + 1
	positions := tape.positions
	if uint(index) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	src := c.src
	if uint(c.i) < uint(len(src)) {
		tail := src[c.i]
		if tail > ' ' && tail != ',' && tail != '}' {
			return structuralFieldSlow
		}
	}
	delimiterPosition := int(positions[index])
	if uint(delimiterPosition) >= uint(len(src)) || !tape.gapClean(src, c.i, delimiterPosition) {
		return structuralFieldSlow
	}
	switch src[delimiterPosition] {
	case '}':
		tape.index = index
		c.i = delimiterPosition + 1
		c.depth--
		return structuralFieldEnd
	case ',':
		index++
	default:
		return structuralFieldSlow
	}
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	pos := positions[index : valueIndex+1]
	_ = pos[2]
	openPosition := int(pos[0])
	closePosition := int(pos[1])
	valuePosition := int(pos[2])
	keyStart := openPosition + 1
	if keyStart+8 > len(src) || closePosition-keyStart != int(expected.keyLen) ||
		uint(openPosition) >= uint(len(src)) || uint(closePosition) >= uint(len(src)) ||
		!tape.gapClean(src, delimiterPosition+1, openPosition) ||
		src[openPosition] != '"' || src[closePosition] != '"' ||
		!structuralColonGap(src, closePosition, valuePosition) {
		return structuralFieldSlow
	}
	mask := expected.keyMask
	if expected.keyLen <= 6 {
		mask >>= 8
	}
	if (binary.LittleEndian.Uint64(src[keyStart:keyStart+8])^expected.key)&mask != 0 {
		return structuralFieldSlow
	}
	tape.index = valueIndex
	c.i = valuePosition
	return structuralFieldMatched
}

func (c *decoderCursor) finishObjectStructural(first bool) bool {
	tape := &c.state.structural
	index := tape.index + 1
	if uint(index) >= uint(len(tape.positions)) {
		return false
	}
	src := c.src
	if !first && uint(c.i) < uint(len(src)) {
		tail := src[c.i]
		if tail > ' ' && tail != '}' {
			return false
		}
	}
	position := int(tape.positions[index])
	if uint(position) >= uint(len(src)) || src[position] != '}' || !tape.gapClean(src, c.i, position) {
		return false
	}
	tape.index = index
	c.i = position + 1
	c.depth--
	return true
}

// nextObjectFieldStructural fuses the On-Demand object sequence into one
// forward tape operation: delimiter, key open/close, and value start. The
// omitted colon is validated from the raw gap.
// Escaped keys fall back transactionally without moving either cursor.
func (c *decoderCursor) nextObjectFieldStructural(first bool, expected *typedField) (key string, matched, ok, handled bool, err error) {
	if c.state == nil || !c.state.structuralActive || c.state.structural.bad {
		return "", false, false, false, nil
	}
	tape := &c.state.structural
	if tape.escaped {
		return "", false, false, false, nil
	}
	src := c.src
	n := len(src)
	if !first && uint(c.i) < uint(n) {
		tail := src[c.i]
		if tail > ' ' && tail != ',' && tail != '}' {
			return "", false, false, true, c.err(c.i, "expected comma or closing brace in object")
		}
	}
	index, position, exists := tape.seekFrom(tape.index, c.i, n)
	if !exists {
		if !tape.gapClean(src, c.i, n) {
			return "", false, false, false, nil
		}
		return "", false, false, true, c.err(position, "unterminated object")
	}
	if !tape.gapClean(src, c.i, position) {
		return "", false, false, false, nil
	}
	if first {
		if src[position] == '}' {
			tape.index = index
			c.i = position + 1
			c.depth--
			return "", false, false, true, nil
		}
	} else {
		switch src[position] {
		case '}':
			tape.index = index
			c.i = position + 1
			c.depth--
			return "", false, false, true, nil
		case ',':
			commaPos := position
			index++
			if index >= len(tape.positions) {
				if !tape.gapClean(src, commaPos+1, n) {
					return "", false, false, false, nil
				}
				return "", false, false, true, c.err(position, "unterminated object")
			}
			position = int(tape.positions[index])
			if !tape.gapClean(src, commaPos+1, position) {
				return "", false, false, false, nil
			}
		default:
			return "", false, false, true, c.err(position, "expected comma or closing brace in object")
		}
	}
	if src[position] != '"' {
		return "", false, false, true, c.err(position, "expected object key string")
	}
	if index+1 >= len(tape.positions) {
		return "", false, false, true, c.err(position, "unterminated string")
	}
	closeEntry := tape.positions[index+1]
	closePosition := int(closeEntry)
	if closePosition >= n || src[closePosition] != '"' {
		return "", false, false, true, c.err(position, "unterminated string")
	}
	keyStart := position + 1
	if tape.nonASCII && !validUTF8Fast(src[keyStart:closePosition]) {
		return "", false, false, true, c.err(keyStart, "invalid UTF-8 in string")
	}
	valueIndex := index + 2
	valuePosition := n
	if valueIndex < len(tape.positions) {
		valuePosition = int(tape.positions[valueIndex])
	}
	if !structuralColonGap(src, closePosition, valuePosition) {
		return "", false, false, true, c.err(closePosition+1, "expected colon after object key")
	}
	keyLen := closePosition - keyStart
	if expected != nil {
		if keyLen == int(expected.keyLen) && expected.keyMask != 0 && keyStart+8 <= n {
			mask := ^uint64(0)
			if keyLen < 7 {
				mask >>= uint(7-keyLen) * 8
			}
			diff := (binary.LittleEndian.Uint64(src[keyStart:keyStart+8]) ^ expected.key) & mask
			matched = diff == 0 && (keyLen <= 8 || matchStringAt(src, keyStart+8, expected.name[8:]))
		}
		if !matched && c.flags&decoderCaseSensitive == 0 {
			actual := byteview.String(src[keyStart:closePosition])
			matched = strings.EqualFold(actual, expected.name)
		}
	}
	if !matched {
		key = byteview.String(src[keyStart:closePosition])
	}
	tape.index = valueIndex
	c.i = valuePosition
	return key, matched, true, true, nil
}

func (c *decoderCursor) nextArrayElementStructural(first bool) (more, handled bool, err error) {
	if c.state == nil || !c.state.structuralActive || c.state.structural.bad {
		return false, false, nil
	}
	tape := &c.state.structural
	src := c.src
	n := len(src)
	if !first && uint(c.i) < uint(n) {
		tail := src[c.i]
		if tail > ' ' && tail != ',' && tail != ']' {
			return false, true, c.err(c.i, "expected comma or closing bracket in array")
		}
	}
	index, position, exists := tape.seekFrom(tape.index, c.i, n)
	if !exists {
		if !tape.gapClean(src, c.i, n) {
			return false, false, nil
		}
		return false, true, c.err(position, "unterminated array")
	}
	if !tape.gapClean(src, c.i, position) {
		return false, false, nil
	}
	if src[position] == ']' {
		tape.index = index
		c.i = position + 1
		c.depth--
		return false, true, nil
	}
	if !first {
		if src[position] != ',' {
			return false, true, c.err(position, "expected comma or closing bracket in array")
		}
		commaPos := position
		index++
		if index >= len(tape.positions) {
			if !tape.gapClean(src, commaPos+1, n) {
				return false, false, nil
			}
			return false, true, c.err(position, "unterminated array")
		}
		position = int(tape.positions[index])
		if !tape.gapClean(src, commaPos+1, position) {
			return false, false, nil
		}
	}
	tape.index = index
	c.i = position
	return true, true, nil
}

// nextArrayElementExact advances from the final token of the preceding value.
// On a miss it leaves the cursor unchanged so seek-based parsing can retain
// malformed-input diagnostics.
func (c *decoderCursor) nextArrayElementExact(first bool) uint8 {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	if uint(index) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	src := c.src
	n := len(src)
	if !first && uint(c.i) < uint(n) {
		tail := src[c.i]
		if tail > ' ' && tail != ',' && tail != ']' {
			return structuralFieldSlow
		}
	}
	position := int(positions[index])
	if uint(position) >= uint(n) || !tape.gapClean(src, c.i, position) {
		return structuralFieldSlow
	}
	if src[position] == ']' {
		tape.index = index
		c.i = position + 1
		c.depth--
		return structuralArrayEnd
	}
	if !first {
		if src[position] != ',' {
			return structuralFieldSlow
		}
		commaPos := position
		index++
		if uint(index) >= uint(len(positions)) {
			return structuralFieldSlow
		}
		position = int(positions[index])
		if uint(position) >= uint(n) || src[position] == ']' || !tape.gapClean(src, commaPos+1, position) {
			return structuralFieldSlow
		}
	}
	tape.index = index
	c.i = position
	return structuralArrayValue
}

func (c *decoderCursor) skipSpaceAt(i int) int {
	return skipSpaceIndent(c.src, i)
}

func (c *decoderCursor) structuralStringEnd(start int) (end int, ok bool) {
	if c.state == nil || !c.state.structuralActive || c.state.structural.bad {
		return 0, false
	}
	return c.state.structural.stringEnd(c.src, start)
}
