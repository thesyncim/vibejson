package vibejson

import (
	"errors"
	"fmt"
	"io"
	"unicode/utf8"
)

// Reader streams complete, whitespace-separated JSON values from an io.Reader.
// Bytes and zero-copy decodes alias its rolling buffer until the next advance.
// Reader is single-goroutine; use DecodeNext for typed streams.
type Reader struct {
	in     io.Reader
	buf    []byte
	closed bool

	pos int // scan position within buf
	end int // valid bytes end within buf

	valStart int // current value extent
	valEnd   int
	// readErr is delayed until bytes returned with the error are consumed.
	readErr error

	consumed int64 // bytes discarded before buf[0], for error offsets
	maxValue int
	eof      bool
	hasValue bool
	err      error
}

// ReaderOptions configures a Reader before it reads input.
type ReaderOptions struct {
	// BufferSize is the initial rolling-buffer size. Values below 512 are raised
	// to 512; zero selects the default.
	BufferSize int
	// MaxValueBytes rejects values larger than this many bytes. Zero is unbounded.
	MaxValueBytes int
}

// ErrReaderClosed reports DecodeFrom on a closed Reader.
var ErrReaderClosed = errors.New("vibejson: reader closed")

// ValueFrame finds one value across refills with constant framing state.
type ValueFrame struct {
	mode    uint8 // frameContainer, frameString, frameNumber, frameLiteral
	depth   int   // open { and [ for containers
	inStr   bool  // inside a string within a container
	esc     bool  // previous string byte was an unescaped backslash
	numE    bool  // previous number byte was e or E (a following +/- stays in it)
	litLeft int   // literal bytes still expected
	Framed  int   // value bytes consumed so far, from the value start
}

const (
	frameContainer uint8 = iota
	frameString
	frameNumber
	frameLiteral
)

// Init classifies the value from its leading byte and consumes it. An
// unrecognized leading byte is framed as a one-byte number so the caller's
// validator rejects it.
func (f *ValueFrame) Init(c byte) {
	f.Framed = 1
	switch {
	case c == '"':
		f.mode = frameString
	case c == '{' || c == '[':
		f.mode = frameContainer
		f.depth = 1
	case c == 't' || c == 'n':
		f.mode = frameLiteral
		f.litLeft = 3
	case c == 'f':
		f.mode = frameLiteral
		f.litLeft = 4
	default:
		f.mode = frameNumber
	}
}

// scanStringBody advances through a buffered string and carries escapes across
// refills.
func (f *ValueFrame) scanStringBody(src []byte, i, n int) (int, bool) {
	for i < n {
		if f.esc {
			f.esc = false
			i++
			continue
		}
		j := scanStringSpecial(src[:n], i)
		if j >= n {
			return n, false
		}
		switch src[j] {
		case '"':
			return j + 1, true
		case '\\':
			f.esc = true
			i = j + 1
		default: // control or non-ASCII string content
			i = j + 1
		}
	}
	return i, false
}

// Scan advances the frame and reports whether the value has a structural end.
func (f *ValueFrame) Scan(src []byte, start, n int) bool {
	i := start + f.Framed
	switch f.mode {
	case frameString:
		end, done := f.scanStringBody(src, i, n)
		f.Framed = end - start
		return done
	case frameNumber:
		for i < n {
			c := src[i]
			switch {
			case c >= '0' && c <= '9', c == '.':
				f.numE = false
			case c == 'e' || c == 'E':
				f.numE = true
			case c == '+' || c == '-':
				if !f.numE {
					f.Framed = i - start
					return true
				}
				f.numE = false
			default:
				f.Framed = i - start
				return true
			}
			i++
		}
	case frameLiteral:
		for i < n && f.litLeft > 0 {
			i++
			f.litLeft--
		}
		f.Framed = i - start
		return f.litLeft == 0
	default: // frameContainer
		for i < n {
			if f.inStr {
				var done bool
				i, done = f.scanStringBody(src, i, n)
				if done {
					f.inStr = false
				}
				continue
			}
			c := src[i]
			i++
			switch c {
			case '"':
				f.inStr = true
			case '{', '[':
				f.depth++
			case '}', ']':
				f.depth--
				if f.depth == 0 {
					f.Framed = i - start
					return true
				}
			}
		}
	}
	f.Framed = i - start
	return false
}

// defaultReaderSize holds several typical NDJSON records per read.
const defaultReaderSize = 64 << 10

// NewReader returns a Reader with the default buffer and no value-size limit.
func NewReader(in io.Reader) *Reader {
	return &Reader{in: in, buf: make([]byte, defaultReaderSize)}
}

// NewReaderWithOptions allocates a configured rolling buffer without reading.
func NewReaderWithOptions(in io.Reader, options ReaderOptions) (*Reader, error) {
	if options.BufferSize < 0 {
		return nil, fmt.Errorf("vibejson: negative Reader buffer size %d", options.BufferSize)
	}
	if options.MaxValueBytes < 0 {
		return nil, fmt.Errorf("vibejson: negative Reader value limit %d", options.MaxValueBytes)
	}
	size := options.BufferSize
	if size == 0 {
		size = defaultReaderSize
	}
	if size < 512 {
		size = 512
	}
	return &Reader{
		in:       in,
		buf:      make([]byte, size),
		maxValue: options.MaxValueBytes,
	}, nil
}

// Close releases the input and rolling buffer. It is idempotent.
func (r *Reader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	r.hasValue = false
	r.in = nil
	r.buf = nil
	return nil
}

// Err returns the first sticky input, framing, validation, or decoding error.
func (r *Reader) Err() error {
	return r.err
}

// InputOffset returns the exclusive input offset after the current value.
func (r *Reader) InputOffset() int64 {
	return r.consumed + int64(r.valEnd)
}

// Bytes returns the current value as a rolling-buffer alias, or nil.
func (r *Reader) Bytes() []byte {
	if !r.hasValue {
		return nil
	}
	return r.buf[r.valStart:r.valEnd]
}

// DecodeFrom decodes the current value without advancing the Reader.
func DecodeFrom[T any](r *Reader, dec Decoder[T], dst *T) error {
	if !r.hasValue {
		if r.closed {
			return ErrReaderClosed
		}
		if r.err != nil {
			return r.err
		}
		return errors.New("vibejson: DecodeFrom without a current value; call Next first")
	}
	return dec.Decode(r.buf[r.valStart:r.valEnd], dst)
}

// DecodeNext advances to the next value and decodes it in one pass. It returns
// false at end of stream or after recording an error in Err.
func DecodeNext[T any](r *Reader, dec Decoder[T], dst *T) bool {
	if r.closed {
		return false
	}
	if r.err != nil {
		return false
	}
	r.hasValue = false
	i := r.pos
	// Skip inter-value whitespace, refilling as needed, to the value start.
	for {
		i = SkipSpace(r.buf[:r.end], i)
		if i < r.end {
			break
		}
		r.pos = i
		if !r.fill(&i) {
			if r.err == nil {
				r.err = r.readErr
			}
			return false
		}
	}

	// Frame the value's end resumably, so a value spanning many refills is
	// scanned once instead of re-decoded from the start each time, then decode
	// the fully-buffered extent exactly once. decodedN caches that decode
	// (relative to the value start) while a scalar awaits its confirming byte.
	var fr ValueFrame
	fr.Init(r.buf[i])
	framed := false
	decodedN := -1
	for {
		if !framed {
			framed = fr.Scan(r.buf, i, r.end)
		}
		if decodedN < 0 && (framed || r.eof) {
			if r.terminalScalarSourceError(i, fr.Framed) {
				extent := r.buf[i : i+fr.Framed]
				err := Validate(extent)
				if err == nil || incompleteFramedValue(extent, err, framed) {
					err = r.readErr
				}
				r.err = fmt.Errorf("vibejson: value at input offset %d: %w", r.consumed+int64(i), err)
				return false
			}
			if r.maxValue > 0 && fr.Framed > r.maxValue {
				r.err = fmt.Errorf("vibejson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
				return false
			}
			n, err := dec.DecodePrefix(r.buf[i:i+fr.Framed], dst)
			if err != nil {
				// The value is fully buffered (framed) or the input ended
				// mid-value; the error is real. Diagnose the framed extent so
				// the reason does not depend on trailing input.
				extent := r.buf[i : i+fr.Framed]
				if verr := Validate(extent); verr != nil {
					err = verr
					if r.readErr != nil &&
						incompleteFramedValue(extent, verr, framed) {
						err = r.readErr
					}
				}
				r.err = fmt.Errorf("vibejson: value at input offset %d: %w", r.consumed+int64(i), err)
				return false
			}
			decodedN = n
		}
		if decodedN >= 0 {
			end := i + decodedN
			if end < r.end || r.eof {
				r.valStart, r.valEnd = i, end
				r.pos = end
				r.hasValue = true
				return true
			}
			// end == r.end && !r.eof: read one more byte to confirm the scalar
			// boundary, without re-decoding the already-decoded value.
		}
		r.pos = i
		if !r.fill(&i) {
			if r.err != nil {
				return false
			}
			// End of input reached; the loop re-evaluates with r.eof set.
		}
	}
}

// Next invalidates the previous value and advances to the next validated value.
func (r *Reader) Next() bool {
	if r.closed {
		return false
	}
	if r.err != nil {
		return false
	}
	r.hasValue = false
	i := r.pos
	// Skip inter-value whitespace, refilling as needed.
	for {
		i = SkipSpace(r.buf[:r.end], i)
		if i < r.end {
			break
		}
		r.pos = i
		if !r.fill(&i) {
			if r.err == nil {
				r.err = r.readErr
			}
			return false
		}
	}

	// Validate buffered values first; a scalar at the boundary needs one more
	// byte to confirm that it has ended.
	{
		window := r.buf[:r.end]
		if end, ok := validRootValueFast(window, r.end, i, window[i]); ok && (end < r.end || r.eof) {
			if r.terminalScalarSourceError(i, end-i) {
				r.err = fmt.Errorf("vibejson: invalid value at input offset %d: %w", r.consumed+int64(i), r.readErr)
				return false
			}
			if r.maxValue > 0 && end-i > r.maxValue {
				r.err = fmt.Errorf("vibejson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
				return false
			}
			r.valStart, r.valEnd = i, end
			r.pos = end
			r.hasValue = true
			return true
		}
	}

	// Frame across refills and validate the buffered extent once.
	var fr ValueFrame
	fr.Init(r.buf[i])
	framed := false
	validLen := -1
	for {
		if !framed {
			framed = fr.Scan(r.buf, i, r.end)
		}
		if validLen < 0 && (framed || r.eof) {
			window := r.buf[:r.end]
			end, ok := validRootValueFast(window, r.end, i, window[i])
			if !ok {
				// Diagnose the framed extent so trailing input cannot change the error.
				extent := r.buf[i : i+fr.Framed]
				verr := Validate(extent)
				if verr == nil {
					verr = io.ErrUnexpectedEOF
				}
				if r.readErr != nil &&
					incompleteFramedValue(extent, verr, framed) {
					verr = r.readErr
				}
				r.err = fmt.Errorf("vibejson: invalid value at input offset %d: %w", r.consumed+int64(i), verr)
				return false
			}
			validLen = end - i
		}
		if validLen >= 0 {
			end := i + validLen
			if end < r.end || r.eof {
				if r.terminalScalarSourceError(i, validLen) {
					r.err = fmt.Errorf("vibejson: invalid value at input offset %d: %w", r.consumed+int64(i), r.readErr)
					return false
				}
				// A scalar at the buffer edge needs a confirming byte.
				if r.maxValue > 0 && validLen > r.maxValue {
					r.err = fmt.Errorf("vibejson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
					return false
				}
				r.valStart, r.valEnd = i, end
				r.pos = end
				r.hasValue = true
				return true
			}
			// end == r.end && !r.eof: read one more byte to confirm the
			// boundary, without re-validating the already-checked value.
		}
		r.pos = i
		if !r.fill(&i) {
			if r.err != nil {
				return false
			}
			// End of input reached; the loop re-evaluates with r.eof set.
		}
	}
}

// terminalScalarSourceError handles a source error at an unconfirmed scalar.
func (r *Reader) terminalScalarSourceError(start, length int) bool {
	if r.readErr == nil || start+length != r.end {
		return false
	}
	return terminalValueNeedsSourceBoundary(r.buf[start])
}

// Strings and fixed-length literals need no delimiter.
func terminalValueNeedsSourceBoundary(leading byte) bool {
	switch leading {
	case '{', '[', '"', 'n', 't', 'f':
		return false
	default:
		return true
	}
}

// incompleteFramedValue reports whether a syntax error is caused by EOF.
func incompleteFramedValue(src []byte, err error, framed bool) bool {
	if framed || len(src) == 0 {
		return false
	}
	var syntax *SyntaxError
	if !errors.As(err, &syntax) {
		return errors.Is(err, io.ErrUnexpectedEOF)
	}
	switch syntax.Message {
	case "expected value", "expected object key string",
		"expected colon after object key", "unterminated array",
		"unterminated object":
		return syntax.Offset == len(src)
	case "unterminated string", "unterminated escape sequence":
		return true
	case "invalid number", "invalid number fraction", "invalid number exponent":
		end, message := scanNumber(src, syntax.Offset)
		return message == syntax.Message && end == len(src)
	case "invalid literal":
		return incompleteLiteralAt(src, syntax.Offset)
	case "invalid unicode escape":
		return incompleteHexEscapeAt(src, syntax.Offset)
	case "missing low surrogate", "invalid low surrogate":
		return incompleteLowSurrogateAt(src, syntax.Offset)
	case "invalid UTF-8 in string":
		return incompleteUTF8At(src, syntax.Offset)
	default:
		return false
	}
}

func incompleteLiteralAt(src []byte, offset int) bool {
	if uint(offset) >= uint(len(src)) {
		return false
	}
	var literal string
	switch src[offset] {
	case 'n':
		literal = "null"
	case 't':
		literal = "true"
	case 'f':
		literal = "false"
	default:
		return false
	}
	remaining := src[offset:]
	if len(remaining) >= len(literal) {
		return false
	}
	for i := range remaining {
		if remaining[i] != literal[i] {
			return false
		}
	}
	return true
}

func incompleteHexEscapeAt(src []byte, offset int) bool {
	if offset < 0 || offset+2 > len(src) ||
		src[offset] != '\\' || src[offset+1] != 'u' ||
		offset+6 <= len(src) {
		return false
	}
	for _, c := range src[offset+2:] {
		if hexNibbleTable[c] >= 0x10 {
			return false
		}
	}
	return true
}

func incompleteLowSurrogateAt(src []byte, offset int) bool {
	const highEscapeBytes = 6
	low := offset + highEscapeBytes
	if offset < 0 || low > len(src) {
		return false
	}
	remaining := src[low:]
	if len(remaining) >= highEscapeBytes {
		return false
	}
	if len(remaining) >= 1 && remaining[0] != '\\' {
		return false
	}
	if len(remaining) >= 2 && remaining[1] != 'u' {
		return false
	}
	if len(remaining) < 2 {
		return true
	}
	for _, c := range remaining[2:] {
		if hexNibbleTable[c] >= 0x10 {
			return false
		}
	}
	return true
}

func incompleteUTF8At(src []byte, offset int) bool {
	return uint(offset) < uint(len(src)) && !utf8.FullRune(src[offset:])
}

// fill reads more input, compacting or growing the buffer as needed.
func (r *Reader) fill(keep *int) bool {
	if r.eof {
		return false
	}
	if r.end == len(r.buf) {
		if r.maxValue > 0 && r.end-*keep > r.maxValue {
			r.err = fmt.Errorf("vibejson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(*keep), r.maxValue)
			return false
		}
		if *keep > 0 {
			// Discard bytes before the candidate value.
			n := copy(r.buf, r.buf[*keep:r.end])
			r.consumed += int64(*keep)
			r.end = n
			r.pos -= *keep
			r.valStart -= *keep
			r.valEnd -= *keep
			*keep = 0
		} else {
			grown := make([]byte, len(r.buf)*2)
			copy(grown, r.buf[:r.end])
			r.buf = grown
		}
	}
	for {
		available := len(r.buf) - r.end
		n, err := r.in.Read(r.buf[r.end:])
		if uint(n) > uint(available) {
			r.eof = true
			r.err = fmt.Errorf("vibejson: invalid Read count %d for %d-byte buffer", n, available)
			return false
		}
		r.end += n
		switch {
		case err == io.EOF:
			r.eof = true
			return n > 0
		case err != nil:
			r.eof = true
			r.readErr = err
			return n > 0
		case n > 0:
			return true
		}
	}
}
