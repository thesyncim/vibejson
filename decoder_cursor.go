package vibejson

//go:generate go run ./internal/cmd/codegen decoder-cursor

import (
	"encoding/binary"
	"math/bits"
	"strconv"
	"unsafe"

	"github.com/thesyncim/vibejson/x/byteview"
)

// signedInteger is the set of integer types accepted by decoderCursor.Int.
type signedInteger interface {
	~int | ~int8 | ~int16 | ~int32 | ~int64
}

// unsignedInteger is the set of integer types accepted by decoderCursor.Uint.
type unsignedInteger interface {
	~uint | ~uint8 | ~uint16 | ~uint32 | ~uint64 | ~uintptr
}

// floatValue is the set of floating-point types accepted by decoderCursor.Float.
type floatValue interface {
	~float32 | ~float64
}

// decoderFlags carries the per-decode switches. All but one mirror
// DecoderOptions; decoderExpectedSlow latches after the first semantic-order
// miss of the packed-key matcher. Formatting whitespace is handled by the
// packed path and does not trip the latch.
type decoderFlags uint8

const (
	decoderZeroCopy decoderFlags = 1 << iota
	decoderDisallowUnknown
	decoderCaseSensitive
	decoderReplace
	decoderExpectedSlow
	decoderUseNumber
	decoderReplaceWideDestination
)

// decoderCursor is the concrete, interface-free parser used by compiled typed
// decoders. Its generic scalar methods are specialized by destination width.
type decoderCursor struct {
	src      []byte
	i        int
	maxDepth int32
	depth    int32
	flags    decoderFlags
	// floatLong is the sticky element-shape hint for fused float array
	// loops: while set, elements skip the short-form probe that uniformly
	// long values (geographic coordinates) always fail.
	floatLong bool
	// strings is the current append-only owned-string block.
	strings *decoderStringBlock
	// state carries uncommon per-decode storage behind one pointer. The
	// destination range detects pointers into sibling value storage.
	state *decoderState
}

// decoderState carries uncommon structural and Replace-mode state between
// parser round trips. Owned string blocks live directly on decoderCursor:
// unlike pooled operation metadata, destination strings must remain alive and
// immutable after Decode returns.
type decoderState struct {
	structural         decoderStructuralTape
	structuralActive   bool
	operation          *decoderOperationState
	replaceDestination unsafe.Pointer
	replaceSpan        uint32
}

// newDecoderCursor starts decoding src with opts.
func newDecoderCursor(src []byte, opts DecoderOptions) decoderCursor {
	var flags decoderFlags
	if opts.ZeroCopy {
		flags |= decoderZeroCopy
	}
	if opts.DisallowUnknownFields {
		flags |= decoderDisallowUnknown
	}
	if opts.CaseSensitive {
		flags |= decoderCaseSensitive
	}
	if opts.Replace {
		flags |= decoderReplace
	}
	if opts.UseNumber {
		flags |= decoderUseNumber
	}
	return decoderCursor{
		src:      src,
		maxDepth: int32(opts.MaxDepth),
		flags:    flags,
	}
}

// Finish verifies that exactly one complete JSON value was consumed.
func (c *decoderCursor) Finish() error {
	if c.i == len(c.src) {
		return nil
	}
	return c.finishSlow()
}

//go:noinline
func (c *decoderCursor) finishSlow() error {
	c.skipSpace()
	if c.i != len(c.src) {
		return c.err(c.i, "unexpected data after top-level value")
	}
	return nil
}

// TryNull consumes null and reports true, or leaves a non-null value untouched.
func (c *decoderCursor) TryNull() (bool, error) {
	i := c.i
	if i < len(c.src) && c.src[i] > ' ' && c.src[i] != 'n' {
		return false, nil
	}
	return c.tryNullSlow()
}

// notNullFast reports that the next byte proves a non-null value with no
// leading whitespace, letting callers skip the TryNull call entirely on the
// common present-value path. TryNull itself cannot fit the inlining budget
// because of its mandatory slow-path call.
func (c *decoderCursor) notNullFast() bool {
	i := c.i
	return i < len(c.src) && c.src[i] > ' ' && c.src[i] != 'n'
}

//go:noinline
func (c *decoderCursor) tryNullSlow() (bool, error) {
	c.skipSpace()
	if c.i >= len(c.src) || c.src[c.i] != 'n' {
		return false, nil
	}
	if !literalNullAt(c.src, c.i) {
		return false, c.err(c.i, "invalid literal")
	}
	c.i += 4
	return true, nil
}

// BeginObject consumes an opening object delimiter.
func (c *decoderCursor) BeginObject(typeName string) error {
	i := c.i
	if i < len(c.src) && c.src[i] == '{' && c.depth < c.maxDepth {
		c.depth++
		c.i = i + 1
		return nil
	}
	return c.beginObjectSlow(typeName)
}

//go:noinline
func (c *decoderCursor) beginObjectSlow(typeName string) error {
	c.skipSpace()
	if c.i >= len(c.src) || c.src[c.i] != '{' {
		return c.expected(typeName, "object")
	}
	if c.depth >= c.maxDepth {
		return c.err(c.i, "maximum nesting depth exceeded")
	}
	c.depth++
	c.i++
	return nil
}

// NextObjectField returns the next decoded key. first must be true only for
// the first call after BeginObject. The key aliases the source (or the
// string arena when escaped) — callers that retain it own the aliasing
// rules of the current decode mode.
func (c *decoderCursor) NextObjectField(first bool) (key string, ok bool, err error) {
	i := c.i
	if uint(i) < uint(len(c.src)) && c.src[i] <= ' ' {
		// Pretty-printed documents lead every member and closing brace with a
		// newline-and-indent run. Consuming it here keeps such documents on
		// the packed key match below; without this, every member of an
		// indented object detours through the slow parser. Whitespace
		// consumption never needs rollback, so the position commits at once.
		i = c.skipSpaceAt(i)
		c.i = i
	}
	if i >= len(c.src) {
		return c.nextObjectFieldSlow(first)
	}
	if first {
		if c.src[i] == '}' {
			c.i = i + 1
			c.depth--
			return "", false, nil
		}
		if c.src[i] != '"' {
			return c.nextObjectFieldSlow(first)
		}
	} else {
		switch c.src[i] {
		case '}':
			c.i = i + 1
			c.depth--
			return "", false, nil
		case ',':
			i++
			if uint(i) < uint(len(c.src)) && c.src[i] <= ' ' {
				// The gap between comma and key stays local: on the rare
				// fallthrough the slow parser re-reads from the committed
				// pre-comma position and owns every error offset.
				i = c.skipSpaceAt(i)
			}
			if i >= len(c.src) || c.src[i] != '"' {
				return c.nextObjectFieldSlow(first)
			}
		default:
			return c.nextObjectFieldSlow(first)
		}
	}

	keyStart := i + 1
	keyEnd := keyStart
	if keyStart+8 <= len(c.src) {
		mask := stringSpecialMask(binary.LittleEndian.Uint64(c.src[keyStart:]))
		if mask == 0 {
			return c.nextObjectFieldSlow(first)
		}
		keyEnd += bits.TrailingZeros64(mask) / 8
	} else {
		keyEnd = scanStringSpecial(c.src, keyStart)
	}
	// One 16-bit load checks the closing quote and colon together; the
	// length guard covers both bytes.
	if keyEnd+2 > len(c.src) ||
		loadUint16LE(unsafe.Add(sliceBase(c.src), keyEnd)) != quoteColonLE {
		return c.nextObjectFieldSlow(first)
	}
	key = byteview.String(c.src[keyStart:keyEnd])
	c.i = keyEnd + 2
	if i := c.i; i < len(c.src) && c.src[i] <= ' ' {
		// A pretty-printer writes exactly one space between colon and value;
		// that shape advances without a call. Wider or structural gaps take
		// the shared skipper. (An inlineable helper does not fit: the
		// skipSpace call alone costs 57 of the 80-node budget.) The manual
		// advance is safe under an active structural tape — position lookups
		// are monotonic and realign lazily on the next query.
		if c.src[i] == ' ' && uint(i+1) < uint(len(c.src)) && c.src[i+1] > ' ' {
			c.i = i + 1
		} else {
			c.skipSpace()
		}
	}
	return key, true, nil
}

//go:noinline
func (c *decoderCursor) nextObjectFieldSlow(first bool) (key string, ok bool, err error) {
	c.skipSpace()
	if c.i >= len(c.src) {
		return "", false, c.err(c.i, "unterminated object")
	}
	if !first {
		switch c.src[c.i] {
		case '}':
			c.i++
			c.depth--
			return "", false, nil
		case ',':
			c.i++
		default:
			return "", false, c.err(c.i, "expected comma or closing brace in object")
		}
	} else if c.src[c.i] == '}' {
		c.i++
		c.depth--
		return "", false, nil
	}

	c.skipSpace()
	if c.i >= len(c.src) || c.src[c.i] != '"' {
		return "", false, c.err(c.i, "expected object key string")
	}
	keyStart := c.i + 1
	if keyStart+8 <= len(c.src) {
		mask := stringSpecialMask(binary.LittleEndian.Uint64(c.src[keyStart:]))
		keyEnd := keyStart + bits.TrailingZeros64(mask)/8
		if mask != 0 && c.src[keyEnd] == '"' {
			key = byteview.String(c.src[keyStart:keyEnd])
			c.i = keyEnd + 1
		} else {
			key, err = c.typedKey()
		}
	} else {
		key, err = c.typedKey()
	}
	if err != nil {
		return "", false, err
	}
	c.skipSpace()
	if c.i >= len(c.src) || c.src[c.i] != ':' {
		return "", false, c.err(c.i, "expected colon after object key")
	}
	c.i++
	c.skipSpace()
	return key, true, nil
}

// BeginArray consumes an opening array delimiter.
func (c *decoderCursor) BeginArray(typeName string) error {
	i := c.i
	if i < len(c.src) && c.src[i] == '[' && c.depth < c.maxDepth {
		c.depth++
		c.i = i + 1
		return nil
	}
	return c.beginArraySlow(typeName)
}

//go:noinline
func (c *decoderCursor) beginArraySlow(typeName string) error {
	c.skipSpace()
	if c.i >= len(c.src) || c.src[c.i] != '[' {
		return c.expected(typeName, "array")
	}
	if c.depth >= c.maxDepth {
		return c.err(c.i, "maximum nesting depth exceeded")
	}
	c.depth++
	c.i++
	return nil
}

// NextArrayElement reports whether another value is available. first must be
// true only for the first call after BeginArray.
func (c *decoderCursor) NextArrayElement(first bool) (bool, error) {
	i := c.i
	if uint(i) < uint(len(c.src)) && c.src[i] <= ' ' {
		// Indented arrays open with a newline before the first element and
		// close with one before the bracket; consuming the run here keeps
		// both transitions off the slow path, as in NextObjectField.
		i = c.skipSpaceAt(i)
		c.i = i
	}
	if i >= len(c.src) {
		return c.nextArrayElementSlow(first)
	}
	if first {
		if c.src[i] == ']' {
			c.i = i + 1
			c.depth--
			return false, nil
		}
		if c.src[i] > ' ' {
			return true, nil
		}
		return c.nextArrayElementSlow(first)
	}
	switch c.src[i] {
	case ']':
		c.i = i + 1
		c.depth--
		return false, nil
	case ',':
		c.i = i + 1
		if c.i < len(c.src) && c.src[c.i] <= ' ' {
			c.i = c.skipSpaceAt(c.i)
		}
		return true, nil
	}
	return c.nextArrayElementSlow(first)
}

//go:noinline
func (c *decoderCursor) nextArrayElementSlow(first bool) (bool, error) {
	c.skipSpace()
	if c.i >= len(c.src) {
		return false, c.err(c.i, "unterminated array")
	}
	if !first {
		switch c.src[c.i] {
		case ']':
			c.i++
			c.depth--
			return false, nil
		case ',':
			c.i++
		default:
			return false, c.err(c.i, "expected comma or closing bracket in array")
		}
	} else if c.src[c.i] == ']' {
		c.i++
		c.depth--
		return false, nil
	}
	c.skipSpace()
	return true, nil
}

// Skip validates and consumes the next value without materializing it.
func (c *decoderCursor) Skip() error {
	p := c.slowParser()
	err := p.skipTypedValue(int(c.depth))
	c.i = p.i
	return err
}

// Unknown skips key unless unknown fields are disallowed.
func (c *decoderCursor) Unknown(typeName, key string) error {
	if c.flags&decoderDisallowUnknown != 0 {
		return &DecodeError{Offset: c.i, TypeName: typeName, Reason: "unknown field " + strconv.Quote(key)}
	}
	return c.Skip()
}

// CaseSensitive reports whether folded field matching is disabled.
func (c *decoderCursor) CaseSensitive() bool {
	return c.flags&decoderCaseSensitive != 0
}

func (c *decoderCursor) stringStructural(dst *string) error {
	i := c.i
	if i >= len(c.src) || c.src[i] != '"' {
		return c.stringStructuralSlow(dst)
	}
	tape := &c.state.structural
	index := tape.index
	positions := tape.positions
	if uint(index+1) >= uint(len(positions)) || int(positions[index]) != i {
		return c.stringStructuralSlow(dst)
	}
	entry := positions[index+1]
	end := int(entry)
	if end >= len(c.src) || c.src[end] != '"' {
		return c.stringStructuralSlow(dst)
	}
	tape.index = index + 1
	start := i + 1
	if !tape.nonASCII && !tape.escaped || c.structuralStringLocallyDirect(start, end) {
		if c.flags&decoderZeroCopy != 0 {
			*dst = byteview.String(c.src[start:end])
		} else if c.reuseOwnedString(*dst, start, end) {
			// Keep the already-owned identical value.
		} else {
			*dst = c.ownedString(start, end)
		}
		c.i = end + 1
		return nil
	}
	return c.stringStructuralExactSlow(dst, start, end)
}

// structuralStringLocallyDirect proves that one structural string can use the
// source-backed path after another string made the document-wide facts dirty.
// It is only called after the all-clean branch misses.
func (c *decoderCursor) structuralStringLocallyDirect(start, end int) bool {
	if uint(start) > uint(end) || uint(end) > uint(len(c.src)) {
		return false
	}
	span := c.src[start:end]
	tape := &c.state.structural
	if tape.escaped && tape.stringRangeDirty(start, end, false) && scanStringSyntax(span, 0) != len(span) {
		return false
	}
	return !tape.nonASCII || !tape.stringRangeDirty(start, end, true) || validUTF8Fast(span)
}

//go:noinline
func (c *decoderCursor) stringStructuralExactSlow(dst *string, start, end int) error {
	tape := &c.state.structural
	if !tape.escaped && (!tape.nonASCII || validUTF8Fast(c.src[start:end])) ||
		c.structuralStringLocallyDirect(start, end) {
		if !c.reuseOwnedString(*dst, start, end) {
			*dst = c.ownedString(start, end)
		}
		c.i = end + 1
		return nil
	}
	_, decodedCapacity := rawJSONStringLayoutHint(c.src, start)
	text, err := c.parseOwnedString(end, decodedCapacity)
	if err != nil {
		return err
	}
	*dst = text
	return nil
}

// parseOwnedString decodes the string at c.i into the cursor's owned block.
// end is a proven or conservative raw closing-quote position and decodedCapacity
// bounds the decoded bytes. The reservation proves parser appends cannot move.
func (c *decoderCursor) parseOwnedString(end, decodedCapacity int) (string, error) {
	start := c.i + 1
	if end < start || end > len(c.src) {
		end = len(c.src)
	}
	if decodedCapacity < 0 || decodedCapacity > end-start {
		decodedCapacity = end - start
	}
	c.ensureStringArenaCapacity(decodedCapacity + stringArenaHeadroom)
	block := c.strings
	p := c.slowParser()
	p.strings = block.bytes()[:block.used:block.capacity]
	text, err := p.parseString()
	c.i = p.i
	data := block.bytes()
	if unsafe.SliceData(p.strings) == unsafe.SliceData(data) {
		block.used = len(p.strings)
	}
	if err != nil {
		return "", err
	}
	return text, nil
}

// rawJSONStringLayoutHint locates the next unescaped quote and returns a safe
// decoded-size bound without validating syntax. parseString remains
// authoritative; this pass only sizes owned escaped output.
func rawJSONStringLayoutHint(src []byte, i int) (end, decodedCapacity int) {
	for i < len(src) {
		switch src[i] {
		case '"':
			return i, decodedCapacity
		case '\\':
			i++
			if i >= len(src) {
				return len(src), decodedCapacity
			}
			if src[i] == 'u' {
				if i > len(src)-5 {
					return len(src), decodedCapacity
				}
				decodedCapacity += 3
				i += 5
				continue
			}
			decodedCapacity++
			i++
			continue
		}
		decodedCapacity++
		i++
	}
	return len(src), decodedCapacity
}

//go:noinline
func (c *decoderCursor) stringStructuralSlow(dst *string) error {
	if i := c.i; i < len(c.src) && c.src[i] == '"' {
		c.structuralStringEnd(i)
	}
	return c.String(dst)
}

func shortTypedFloatAt(base unsafe.Pointer, n, start int) (value float64, end int, ok bool) {
	if uint(start) >= uint(n) {
		return 0, start, false
	}
	negative := false
	i := start
	if fastByteAt(base, i) == '-' {
		negative = true
		i++
	}
	if i >= n || !IsDigit(fastByteAt(base, i)) {
		return 0, start, false
	}
	value = float64(fastByteAt(base, i) - '0')
	i++
	if typedNumberEnd(base, n, i) {
		if negative {
			value = -value
		}
		return value, i, true
	}
	if i >= n {
		return 0, start, false
	}
	switch fastByteAt(base, i) {
	case '.':
		i++
		if i >= n || !IsDigit(fastByteAt(base, i)) {
			return 0, start, false
		}
		value += float64(fastByteAt(base, i)-'0') / 10
		i++
	case 'e', 'E':
		i++
		exponentNegative := false
		if i < n && (fastByteAt(base, i) == '+' || fastByteAt(base, i) == '-') {
			exponentNegative = fastByteAt(base, i) == '-'
			i++
		}
		if i >= n || !IsDigit(fastByteAt(base, i)) {
			return 0, start, false
		}
		exponent := int(fastByteAt(base, i) - '0')
		if exponentNegative {
			value /= anyPow10[exponent]
		} else {
			value *= anyPow10[exponent]
		}
		i++
	default:
		return 0, start, false
	}
	if !typedNumberEnd(base, n, i) {
		return 0, start, false
	}
	if negative {
		value = -value
	}
	return value, i, true
}

func typedNumberEnd(base unsafe.Pointer, n, i int) bool {
	if i == n {
		return true
	}
	if uint(i) >= uint(n) {
		return false
	}
	switch fastByteAt(base, i) {
	case ',', ']', '}', ' ', '\n', '\r', '\t':
		return true
	default:
		return false
	}
}

// ownedString copies one source span into a compact result-owned string block.
// Blocks are append-only and never recycled across Decode calls, so strings
// already returned to the destination remain immutable for their full lifetime.
func (c *decoderCursor) ownedString(start, end int) string {
	if start == end {
		return ""
	}
	if c.flags&decoderZeroCopy != 0 {
		return byteview.String(c.src[start:end])
	}
	c.ensureStringArenaCapacity(end - start)
	block := c.strings
	offset := block.used
	copy(block.bytes()[offset:], c.src[start:end])
	block.used += end - start
	return OwnedBytesString(block.bytes()[offset:block.used])
}

// reuseOwnedString reports whether existing already contains the source span
// and is independently owned. The source-range check prevents a caller from
// seeding dst with a string view into this decode's mutable input and having
// that alias survive an owned Decode.
func (c *decoderCursor) reuseOwnedString(existing string, start, end int) bool {
	if c.flags&decoderZeroCopy != 0 || len(existing) != end-start {
		return false
	}
	if len(existing) == 0 {
		return true
	}
	source := uintptr(unsafe.Pointer(unsafe.SliceData(c.src)))
	data := uintptr(unsafe.Pointer(unsafe.StringData(existing)))
	if data >= source && data-source <= uintptr(len(c.src)-len(existing)) {
		return false
	}
	return existing == byteview.String(c.src[start:end])
}

// ownedText copies text only when it aliases the decode source. Escaped text
// already lives in a result-owned arena block and can be retained as-is.
func (c *decoderCursor) ownedText(text string) string {
	if len(text) == 0 || c.flags&decoderZeroCopy != 0 {
		return text
	}
	source := uintptr(unsafe.Pointer(unsafe.SliceData(c.src)))
	data := uintptr(unsafe.Pointer(unsafe.StringData(text)))
	if data < source || data-source > uintptr(len(c.src)-len(text)) {
		return text
	}
	return c.ownedString(int(data-source), int(data-source)+len(text))
}

func (c *decoderCursor) skipSpace() {
	if c.state != nil && c.state.structuralActive {
		if uint(c.i) >= uint(len(c.src)) || !IsJSONWhitespace(c.src[c.i]) {
			return
		}
		c.i = c.state.structural.position(c.i, len(c.src))
		return
	}
	c.i = SkipSpace(c.src, c.i)
}

func (c *decoderCursor) err(offset int, reason string) error {
	return syntaxError(c.src, offset, reason)
}

func (c *decoderCursor) slowParser() parser {
	return parser{src: c.src, i: c.i, maxDepth: int(c.maxDepth), zeroCopy: true}
}

// prepareOwnedParser lends the cursor's current result-owned block to a
// dynamic parser when the next value can retain text. The block is
// pessimistically sealed while parsing: if the parser outgrows it, strings
// already returned from the old block must never be overwritten.
// finishOwnedParser restores the exact used prefix when no relocation occurred.
func (c *decoderCursor) prepareOwnedParser(p *parser, useNumber bool) *decoderStringBlock {
	if p.zeroCopy || !anyValueMayRetainText(p.src, p.i, useNumber) || anyValueIsEmpty(p.src, p.i) {
		return nil
	}
	c.ensureStringArenaCapacity(stringArenaHeadroom)
	block := c.strings
	data := block.bytes()
	p.strings = data[:block.used:block.capacity]
	block.used = block.capacity
	return block
}

func anyValueMayRetainText(src []byte, i int, useNumber bool) bool {
	if uint(i) >= uint(len(src)) {
		return false
	}
	switch src[i] {
	case '"', '[', '{':
		return true
	default:
		return useNumber && (src[i] == '-' || IsDigit(src[i]))
	}
}

func anyValueIsEmpty(src []byte, i int) bool {
	if uint(i) >= uint(len(src)) {
		return false
	}
	close := byte(0)
	switch src[i] {
	case '"':
		close = '"'
	case '[':
		close = ']'
	case '{':
		close = '}'
	default:
		return false
	}
	i++
	if close != '"' {
		i = SkipSpace(src, i)
	}
	return i < len(src) && src[i] == close
}

func (c *decoderCursor) finishOwnedParser(p *parser, block *decoderStringBlock) {
	if block == nil {
		return
	}
	data := block.bytes()
	if unsafe.SliceData(p.strings) == unsafe.SliceData(data) {
		block.used = len(p.strings)
	}
}

func (c *decoderCursor) typedKey() (string, error) {
	start := c.i + 1
	special := scanStringSpecial(c.src, start)
	if special < len(c.src) {
		switch c.src[special] {
		case '"':
			c.i = special + 1
			return byteview.String(c.src[start:special]), nil
		case '\\':
			end, decodedCapacity := rawJSONStringLayoutHint(c.src, start)
			return c.parseOwnedString(end, decodedCapacity)
		}
	}
	p := c.slowParser()
	key, err := p.typedKey()
	c.i = p.i
	return key, err
}

// stringArenaSeed sizes the first arena block; stringArenaHeadroom is the
// free space below which the parser starts a fresh block of twice the size
// instead of letting append copy retained content. Real documents carry
// little escaped content, so the arena starts small; escape-dense documents
// pay a handful of block allocations but never re-copy a written byte.
const (
	stringArenaSeed     = 2048
	stringArenaHeadroom = 2048
)

func (c *decoderCursor) ensureStringArenaCapacity(need int) {
	if c.strings == nil {
		capacity := stringArenaSeed
		if capacity > len(c.src) {
			capacity = len(c.src) + 1
		}
		if capacity < need {
			capacity = need
		}
		c.strings = newDecoderStringBlock(capacity)
		return
	}
	if c.strings.capacity-c.strings.used >= need {
		return
	}
	const ownedStringBlockCapacity = 16 << 10
	capacity := ownedStringBlockCapacity
	if minimum := need + stringArenaHeadroom; capacity < minimum {
		capacity = minimum
	}
	c.strings = newDecoderStringBlock(capacity)
}

func (c *decoderCursor) expected(typeName, jsonType string) error {
	return &DecodeError{Offset: c.i, TypeName: typeName, Reason: "expected " + jsonType}
}

// stringToken consumes a JSON string and returns its contents: unescaped
// strings alias the source, escaped strings alias a transient buffer that
// callers must not retain.
func (c *decoderCursor) stringToken() ([]byte, error) {
	i := c.i
	if i >= len(c.src) || c.src[i] != '"' {
		return nil, &DecodeError{Offset: i, Reason: "expected string"}
	}
	start := i + 1
	end := scanStringSpecial(c.src, start)
	if end < len(c.src) && c.src[end] == '"' {
		c.i = end + 1
		return c.src[start:end], nil
	}
	p := c.slowParser()
	text, err := p.parseString()
	c.i = p.i
	if err != nil {
		return nil, err
	}
	return byteview.Bytes(text), nil
}

func (p *parser) typedKey() (string, error) {
	start := p.i + 1
	end := scanStringSpecial(p.src, start)
	if end < len(p.src) && p.src[end] == '"' {
		p.i = end + 1
		return byteview.String(p.src[start:end]), nil
	}
	zeroCopy := p.zeroCopy
	p.zeroCopy = true
	key, err := p.parseString()
	p.zeroCopy = zeroCopy
	return key, err
}

func (p *parser) skipTypedValue(depth int) error {
	value := validator{src: p.src, i: p.i, maxDepth: p.maxDepth}
	if err := value.parseValue(depth); err != nil {
		return err
	}
	p.i = value.i
	return nil
}
