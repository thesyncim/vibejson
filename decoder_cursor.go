package simdjson

import (
	"bytes"
	"encoding/binary"
	"math/bits"
	"strconv"
	"unsafe"
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

// stringValue is the set of string types accepted by decoderCursor.String.
type stringValue interface {
	~string
}

// boolValue is the set of boolean types accepted by decoderCursor.Bool.
type boolValue interface {
	~bool
}

// decoderFlags carries the per-decode switches. All but two mirror
// DecoderOptions; decoderSourceOwned records that ownSource already copied
// the input, and decoderExpectedSlow latches after the first semantic-order
// miss of the packed-key matcher. Formatting whitespace is handled by the
// packed path and does not trip the latch.
type decoderFlags uint8

const (
	decoderZeroCopy decoderFlags = 1 << iota
	decoderDisallowUnknown
	decoderCaseSensitive
	decoderSourceOwned
	decoderReplace
	decoderExpectedSlow
	decoderUseNumber
)

// decoderCursor is the concrete, interface-free parser used by compiled typed
// decoders. Its generic scalar methods are specialized by destination width.
type decoderCursor struct {
	src      []byte
	i        int
	maxDepth int
	depth    int
	flags    decoderFlags
	// floatLong is the sticky element-shape hint for fused float array
	// loops: while set, elements skip the short-form probe that uniformly
	// long values (geographic coordinates) always fail.
	floatLong bool
	// state carries uncommon per-decode storage behind one pointer, keeping
	// the cursor to a cache line. It is allocated lazily for escaped strings;
	// the structural decoder supplies a stack-local state instead.
	state *decoderState
}

// decoderState carries uncommon state between parser round trips. strings'
// length is the retained arena prefix; appends past its capacity relocate the
// block, which is safe because strings already handed out keep aliasing the
// old one.
type decoderState struct {
	strings          []byte
	structural       decoderStructuralTape
	structuralActive bool
	operation        *decoderOperationState
}

// newDecoderCursor starts decoding src with opts.
func newDecoderCursor(src []byte, opts DecoderOptions) decoderCursor {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = defaultMaxDepth
	}
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
		maxDepth: maxDepth,
		flags:    flags,
	}
}

func (c *decoderCursor) releasePlanState(plan *decoderPlanState) {
	if c.state == nil {
		return
	}
	plan.release(c.state)
	c.state = nil
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
		loadUint16LE(unsafe.Add(unsafe.Pointer(unsafe.SliceData(c.src)), keyEnd)) != quoteColonLE {
		return c.nextObjectFieldSlow(first)
	}
	key = unsafe.String(unsafe.SliceData(c.src[keyStart:keyEnd]), keyEnd-keyStart)
	c.i = keyEnd + 2
	if c.i < len(c.src) && c.src[c.i] <= ' ' {
		c.skipSpace()
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
			key = unsafe.String(unsafe.SliceData(c.src[keyStart:keyEnd]), keyEnd-keyStart)
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
			c.skipSpace()
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
	err := p.skipTypedValue(c.depth)
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
	if !tape.nonASCII && !tape.escaped &&
		c.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
		*dst = unsafe.String(unsafe.SliceData(c.src[start:end]), end-start)
		c.i = end + 1
		return nil
	}
	return c.stringStructuralExactSlow(dst, start, end)
}

//go:noinline
func (c *decoderCursor) stringStructuralExactSlow(dst *string, start, end int) error {
	if !c.state.structural.escaped &&
		(!c.state.structural.nonASCII || validUTF8Fast(c.src[start:end])) {
		c.ownSource()
		*dst = unsafe.String(unsafe.SliceData(c.src[start:end]), end-start)
		c.i = end + 1
		return nil
	}
	return c.String(dst)
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
	if i >= n || !isDigit(fastByteAt(base, i)) {
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
		if i >= n || !isDigit(fastByteAt(base, i)) {
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
		if i >= n || !isDigit(fastByteAt(base, i)) {
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

// ownSource lazily takes ownership of source-backed strings with one copy.
// Result strings keep the cloned backing array alive after the cursor returns.
func (c *decoderCursor) ownSource() {
	if c.flags&(decoderZeroCopy|decoderSourceOwned) != 0 {
		return
	}
	c.src = bytes.Clone(c.src)
	c.flags |= decoderSourceOwned
}

func (c *decoderCursor) skipSpace() {
	if c.state != nil && c.state.structuralActive {
		if uint(c.i) >= uint(len(c.src)) || !isJSONWhitespace(c.src[c.i]) {
			return
		}
		c.i = c.state.structural.position(c.i, len(c.src))
		return
	}
	c.i = skipSpace(c.src, c.i)
}

func (c *decoderCursor) err(offset int, reason string) error {
	return syntaxError(c.src, offset, reason)
}

func (c *decoderCursor) slowParser() parser {
	return parser{src: c.src, i: c.i, maxDepth: c.maxDepth, zeroCopy: true, strings: c.stringArena()}
}

func (c *decoderCursor) typedKey() (string, error) {
	start := c.i + 1
	special := scanStringSpecial(c.src, start)
	if special < len(c.src) {
		switch c.src[special] {
		case '"':
			c.i = special + 1
			return unsafe.String(unsafe.SliceData(c.src[start:special]), special-start), nil
		case '\\':
			c.ensureStringArena()
		}
	}
	p := c.slowParser()
	key, err := p.typedKey()
	c.i = p.i
	c.adoptStringArena(p.strings)
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

func (c *decoderCursor) ensureStringArena() {
	if c.state != nil && c.state.strings != nil {
		return
	}
	if c.state == nil {
		c.state = new(decoderState)
	}
	capacity := stringArenaSeed
	if capacity > len(c.src) {
		capacity = len(c.src) + 1
	}
	c.state.strings = make([]byte, 0, capacity)
}

func (c *decoderCursor) stringArena() []byte {
	if c.state == nil {
		return nil
	}
	return c.state.strings
}

// adoptStringArena records the arena state after a parser round trip,
// following the block if appends relocated it. A parser that started with no
// arena grew a private block; adopting it lets the rest of the decode reuse
// that storage.
func (c *decoderCursor) adoptStringArena(arena []byte) {
	if c.state == nil {
		if cap(arena) == 0 {
			return
		}
		c.state = &decoderState{strings: arena}
		return
	}
	c.state.strings = arena
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
	return unsafe.Slice(unsafe.StringData(text), len(text)), nil
}

func (p *parser) typedKey() (string, error) {
	start := p.i + 1
	end := scanStringSpecial(p.src, start)
	if end < len(p.src) && p.src[end] == '"' {
		p.i = end + 1
		return unsafe.String(unsafe.SliceData(p.src[start:end]), end-start), nil
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
