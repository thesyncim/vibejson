package simdjson

import (
	"strings"
	"sync"
	"unsafe"

	simdkernels "github.com/thesyncim/simdjson/internal/kernels"
)

const (
	structuralFieldSlow    uint8 = 0
	structuralFieldMatched uint8 = 1
	structuralFieldEnd     uint8 = 2
	structuralArrayValue   uint8 = 1
	structuralArrayEnd     uint8 = 2

	decoderStructuralTapeRetentionBytes     = 2 << 20
	decoderStructuralTapeRetentionPositions = decoderStructuralTapeRetentionBytes / 4
	decoderStructuralMinBytes               = 4096
)

// decoderStructuralTape is the typed decoder's On-Demand-style cursor. Stage
// 1 builds it once, including closing quotes, and the decoder only advances a
// monotonically increasing index. The backing slice is reused across calls.
type decoderStructuralTape struct {
	positions []uint32
	index     int
	bad       bool
	nonASCII  bool
	escaped   bool
}

var decoderStatePool sync.Pool

func takeDecoderState() *decoderState {
	state, _ := decoderStatePool.Get().(*decoderState)
	if state == nil {
		state = new(decoderState)
	}
	return state
}

// decoderStructuralWorthwhile applies the document-side routing contract for
// the forward structural producer. The caller separately checks that its
// compiled shape has a structural executor.
func decoderStructuralWorthwhile(src []byte) bool {
	return len(src) >= decoderStructuralMinBytes && uint64(len(src)) <= uint64(^uint32(0))
}

func acquireDecoderState(src []byte) *decoderState {
	state := takeDecoderState()
	state.strings = nil
	state.structuralActive = true
	state.structural.build(src)
	return state
}

func releaseDecoderState(state *decoderState) {
	// Escaped output strings own their arena backing independently. Dropping
	// the slice prevents a later decode from mutating that retained output.
	state.strings = nil
	state.resetOperationState()
	state.structuralActive = false
	state.structural.resetForPool()
	decoderStatePool.Put(state)
}

// resetForPool keeps ordinary structural tapes reusable while preventing one
// exceptional document from setting the global pool's permanent high-water
// memory. uint32 has a fixed four-byte width, so the byte budget is exact.
func (t *decoderStructuralTape) resetForPool() {
	t.index = 0
	t.bad = false
	t.nonASCII = false
	t.escaped = false
	if cap(t.positions) > decoderStructuralTapeRetentionPositions {
		t.positions = nil
		return
	}
	t.positions = t.positions[:0]
}

func (t *decoderStructuralTape) build(src []byte) {
	t.index = 0
	t.bad = false
	t.positions = t.positions[:0]
	if cap(t.positions) < len(src)/8 {
		t.positions = make([]uint32, 0, len(src)/4)
	}

	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
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
		written := simdkernels.Stage1CursorBlocks(
			(*byte)(unsafe.Add(base, chunk*64)), count, uint32(chunk*64), &stream, t.positions[start:need],
		)
		t.positions = t.positions[:start+written]
	}

	if fullBlocks*64 != n {
		var tail [64]byte
		for i := range tail {
			tail[i] = ' '
		}
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
		written := simdkernels.Stage1CursorBlocks(
			&tail[0], 1, uint32(fullBlocks*64), &stream, t.positions[start:need],
		)
		t.positions = t.positions[:start+written]
	}
	t.bad = stream.Bad
	t.nonASCII = stream.NonASCII
	t.escaped = stream.Escaped
}

// position returns the first tape position at or after target. Callers only
// enter on JSON whitespace. Any non-whitespace byte in the gap is itself a
// structural or scalar-start token, while scalar continuations never enter.
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

// structuralColonGap validates the punctuation omitted from the compact tape.
// Stage 1 has already rejected non-JSON control bytes, so bytes at or below a
// space outside strings are legal JSON whitespace here.
func structuralColonGap(base unsafe.Pointer, n, closePosition, valuePosition int) bool {
	i := closePosition + 1
	for i < valuePosition && uint(i) < uint(n) && fastByteAt(base, i) <= ' ' {
		i++
	}
	if i >= valuePosition || uint(i) >= uint(n) || fastByteAt(base, i) != ':' {
		return false
	}
	i++
	for i < valuePosition && uint(i) < uint(n) && fastByteAt(base, i) <= ' ' {
		i++
	}
	return i == valuePosition
}

// structuralPackedColonTail is used after the packed key word has already
// proved the quote and colon. The whole-shape path accepts only the two
// dominant layouts; uncommon whitespace misses transactionally and is
// validated by the generic cursor.
func structuralPackedColonTail(base unsafe.Pointer, closePosition, valuePosition int) bool {
	gap := valuePosition - closePosition
	return gap == 2 || gap == 3 && fastByteAt(base, closePosition+2) <= ' '
}

// structuralFirstValueGapOK verifies the one gap where the colon-elided stream
// has no preceding value to protect it. The next tape entry normally follows
// the opener directly or after whitespace; a colon is the only structural byte
// Stage 1 can hide there. Structural container decoders establish this once so
// all of their specialized executors share the same grammar invariant.
func (c *decoderCursor) structuralFirstValueGapOK() bool {
	tape := &c.state.structural
	index := tape.index + 1
	if uint(index) >= uint(len(tape.positions)) {
		return true
	}
	base := unsafe.Pointer(unsafe.SliceData(c.src))
	end := int(tape.positions[index])
	for start := c.i; start < end; start++ {
		if fastByteAt(base, start) == ':' {
			return false
		}
	}
	return true
}

// syncStructuralValue restores the forward-cursor invariant after a value was
// consumed by a generic decoder. Exact structural decoders already leave the
// tape on their final token; generic maps, hooks, and skipped subtrees may not.
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

// matchObjectFieldStructural is the expected-order ASCII path. The structural
// decoder maintains tape.index on the final token of the preceding value, so
// a member is a fixed sequence of increments with no byte/tape rescan.
func (c *decoderCursor) matchObjectFieldStructural(first bool, expected *typedField) uint8 {
	if expected == nil || expected.keyMask == 0 {
		return structuralFieldSlow
	}
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	if uint(index) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	src := c.src
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	if !first && uint(c.i) < uint(n) {
		tail := fastByteAt(base, c.i)
		if tail > ' ' && tail != ',' && tail != '}' {
			return structuralFieldSlow
		}
	}
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	position := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	if !first {
		switch fastByteAt(base, position) {
		case '}':
			tape.index = index
			c.i = position + 1
			c.depth--
			return structuralFieldEnd
		case ',':
			index++
		default:
			return structuralFieldSlow
		}
	} else if fastByteAt(base, position) == '}' {
		tape.index = index
		c.i = position + 1
		c.depth--
		return structuralFieldEnd
	}
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	openPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	closeEntry := *(*uint32)(unsafe.Add(entries, uintptr(index+1)*4))
	closePosition := int(closeEntry)
	valuePosition := int(*(*uint32)(unsafe.Add(entries, uintptr(valueIndex)*4)))
	if fastByteAt(base, openPosition) != '"' || fastByteAt(base, closePosition) != '"' ||
		!structuralColonGap(base, n, closePosition, valuePosition) {
		return structuralFieldSlow
	}
	keyStart := openPosition + 1
	keyLen := closePosition - keyStart
	if keyLen != int(expected.keyLen) || keyStart+8 > n {
		return structuralFieldSlow
	}
	mask := expected.keyMask
	if expected.keyLen <= 6 {
		mask >>= 8
	}
	diff := (loadUint64LE(unsafe.Add(base, keyStart)) ^ expected.key) & mask
	if diff != 0 || keyLen > 8 && !matchStringAt(src, keyStart+8, expected.name[8:]) {
		return structuralFieldSlow
	}
	tape.index = valueIndex
	c.i = valuePosition
	return structuralFieldMatched
}

func (c *decoderCursor) matchFirstObjectFieldStructuralExpected(expected *typedField) uint8 {
	tape := &c.state.structural
	index := tape.index + 1
	positions := tape.positions
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	openPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	src := c.src
	base := unsafe.Pointer(unsafe.SliceData(src))
	if fastByteAt(base, openPosition) == '}' {
		tape.index = index
		c.i = openPosition + 1
		c.depth--
		return structuralFieldEnd
	}
	closeEntry := *(*uint32)(unsafe.Add(entries, uintptr(index+1)*4))
	closePosition := int(closeEntry)
	valuePosition := int(*(*uint32)(unsafe.Add(entries, uintptr(valueIndex)*4)))
	keyStart := openPosition + 1
	if keyStart+8 > len(src) || closePosition-keyStart != int(expected.keyLen) ||
		fastByteAt(base, openPosition) != '"' || fastByteAt(base, closePosition) != '"' ||
		!structuralColonGap(base, len(src), closePosition, valuePosition) {
		return structuralFieldSlow
	}
	mask := expected.keyMask
	if expected.keyLen <= 6 {
		mask >>= 8
	}
	if (loadUint64LE(unsafe.Add(base, keyStart))^expected.key)&mask != 0 {
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
	base := unsafe.Pointer(unsafe.SliceData(src))
	if uint(c.i) < uint(len(src)) {
		tail := fastByteAt(base, c.i)
		if tail > ' ' && tail != ',' && tail != '}' {
			return structuralFieldSlow
		}
	}
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	delimiterPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	switch fastByteAt(base, delimiterPosition) {
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
	openPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	closeEntry := *(*uint32)(unsafe.Add(entries, uintptr(index+1)*4))
	closePosition := int(closeEntry)
	valuePosition := int(*(*uint32)(unsafe.Add(entries, uintptr(valueIndex)*4)))
	keyStart := openPosition + 1
	if keyStart+8 > len(src) || closePosition-keyStart != int(expected.keyLen) ||
		fastByteAt(base, openPosition) != '"' || fastByteAt(base, closePosition) != '"' ||
		!structuralColonGap(base, len(src), closePosition, valuePosition) {
		return structuralFieldSlow
	}
	mask := expected.keyMask
	if expected.keyLen <= 6 {
		mask >>= 8
	}
	if (loadUint64LE(unsafe.Add(base, keyStart))^expected.key)&mask != 0 {
		return structuralFieldSlow
	}
	tape.index = valueIndex
	c.i = valuePosition
	return structuralFieldMatched
}

// matchFirstObjectFieldStructuralShape is the positive-only first transition
// for a compiled whole-object superinstruction. Packed plans up to six bytes
// compare name, quote, and colon in one load; seven-byte names add one colon
// check. Empty objects and uncommon formatting fail transactionally.
func (c *decoderCursor) matchFirstObjectFieldStructuralShape(expected *typedField) bool {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	valueIndex := index + 2
	if uint(valueIndex) >= uint(len(positions)) {
		return false
	}
	src := c.src
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	openPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index)*4)))
	valuePosition := int(*(*uint32)(unsafe.Add(entries, uintptr(valueIndex)*4)))
	keyStart := openPosition + 1
	if keyStart+8 > n || fastByteAt(base, openPosition) != '"' ||
		(loadUint64LE(unsafe.Add(base, keyStart))^expected.key)&expected.keyMask != 0 ||
		expected.keyLen == 7 && (keyStart+8 >= n || fastByteAt(base, keyStart+8) != ':') ||
		!structuralPackedColonTail(base, keyStart+int(expected.keyLen), valuePosition) {
		return false
	}
	tape.index = valueIndex
	c.i = valuePosition
	return true
}

// matchNextObjectFieldStructuralShape is the positive-only next transition
// used by compiled whole-object superinstructions. The shape requires another
// field in compile order, so early object ends and uncommon key forms fall
// into the general decoder without paying for their branches on the hot path.
func (c *decoderCursor) matchNextObjectFieldStructuralShape(expected *typedField) bool {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	valueIndex := index + 3
	if uint(valueIndex) >= uint(len(positions)) {
		return false
	}
	src := c.src
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	if uint(c.i) >= uint(n) || fastByteAt(base, c.i) != ',' {
		return false
	}
	entries := unsafe.Pointer(unsafe.SliceData(positions))
	if *(*uint32)(unsafe.Add(entries, uintptr(index)*4)) != uint32(c.i) {
		return false
	}
	openPosition := int(*(*uint32)(unsafe.Add(entries, uintptr(index+1)*4)))
	valuePosition := int(*(*uint32)(unsafe.Add(entries, uintptr(valueIndex)*4)))
	keyStart := openPosition + 1
	if keyStart+8 > n || fastByteAt(base, openPosition) != '"' {
		return false
	}
	if (loadUint64LE(unsafe.Add(base, keyStart))^expected.key)&expected.keyMask != 0 ||
		expected.keyLen == 7 && (keyStart+8 >= n || fastByteAt(base, keyStart+8) != ':') ||
		!structuralPackedColonTail(base, keyStart+int(expected.keyLen), valuePosition) {
		return false
	}
	tape.index = valueIndex
	c.i = valuePosition
	return true
}

func (c *decoderCursor) finishObjectStructural(first bool) bool {
	tape := &c.state.structural
	index := tape.index + 1
	if uint(index) >= uint(len(tape.positions)) {
		return false
	}
	src := c.src
	base := unsafe.Pointer(unsafe.SliceData(src))
	if !first && uint(c.i) < uint(len(src)) {
		tail := fastByteAt(base, c.i)
		if tail > ' ' && tail != '}' {
			return false
		}
	}
	position := int(tape.positions[index])
	if fastByteAt(base, position) != '}' {
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
		return "", false, false, true, c.err(position, "unterminated object")
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
			index++
			if index >= len(tape.positions) {
				return "", false, false, true, c.err(position, "unterminated object")
			}
			position = int(tape.positions[index])
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
	if !structuralColonGap(unsafe.Pointer(unsafe.SliceData(src)), n, closePosition, valuePosition) {
		return "", false, false, true, c.err(closePosition+1, "expected colon after object key")
	}
	keyLen := closePosition - keyStart
	if expected != nil {
		if keyLen == int(expected.keyLen) && expected.keyMask != 0 && keyStart+8 <= n {
			mask := ^uint64(0)
			if keyLen < 7 {
				mask >>= uint(7-keyLen) * 8
			}
			diff := (loadUint64LE(unsafe.Add(unsafe.Pointer(unsafe.SliceData(src)), keyStart)) ^ expected.key) & mask
			matched = diff == 0 && (keyLen <= 8 || matchStringAt(src, keyStart+8, expected.name[8:]))
		}
		if !matched && c.flags&decoderCaseSensitive == 0 {
			actual := unsafe.String(unsafe.SliceData(src[keyStart:closePosition]), keyLen)
			matched = strings.EqualFold(actual, expected.name)
		}
	}
	if !matched {
		key = unsafe.String(unsafe.SliceData(src[keyStart:closePosition]), keyLen)
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
		return false, true, c.err(position, "unterminated array")
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
		index++
		if index >= len(tape.positions) {
			return false, true, c.err(position, "unterminated array")
		}
		position = int(tape.positions[index])
	}
	tape.index = index
	c.i = position
	return true, true, nil
}

// nextArrayElementExact advances from the final token of the preceding value.
// It moves neither cursor on a miss, leaving the seek-based parser to retain
// full malformed-input diagnostics for uncommon operations.
func (c *decoderCursor) nextArrayElementExact(first bool) uint8 {
	tape := &c.state.structural
	positions := tape.positions
	index := tape.index + 1
	if uint(index) >= uint(len(positions)) {
		return structuralFieldSlow
	}
	src := c.src
	n := len(src)
	base := unsafe.Pointer(unsafe.SliceData(src))
	if !first && uint(c.i) < uint(n) {
		tail := fastByteAt(base, c.i)
		if tail > ' ' && tail != ',' && tail != ']' {
			return structuralFieldSlow
		}
	}
	position := int(positions[index])
	if uint(position) >= uint(n) {
		return structuralFieldSlow
	}
	if fastByteAt(base, position) == ']' {
		tape.index = index
		c.i = position + 1
		c.depth--
		return structuralArrayEnd
	}
	if !first {
		if fastByteAt(base, position) != ',' {
			return structuralFieldSlow
		}
		index++
		if uint(index) >= uint(len(positions)) {
			return structuralFieldSlow
		}
		position = int(positions[index])
		if uint(position) >= uint(n) || fastByteAt(base, position) == ']' {
			return structuralFieldSlow
		}
	}
	tape.index = index
	c.i = position
	return structuralArrayValue
}

func (c *decoderCursor) structuralPosition(i int) int {
	if c.state == nil || !c.state.structuralActive {
		return i
	}
	return c.state.structural.position(i, len(c.src))
}

func (c *decoderCursor) skipSpaceAt(i int) int {
	if c.state == nil || !c.state.structuralActive {
		return skipSpaceIndent(c.src, i)
	}
	if uint(i) >= uint(len(c.src)) || c.src[i] > ' ' {
		return i
	}
	return c.state.structural.position(i, len(c.src))
}

func (c *decoderCursor) structuralStringEnd(start int) (end int, ok bool) {
	if c.state == nil || !c.state.structuralActive || c.state.structural.bad {
		return 0, false
	}
	return c.state.structural.stringEnd(c.src, start)
}
