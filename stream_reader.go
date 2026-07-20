package simdjson

import (
	"errors"
	"fmt"
	"io"
)

// Reader streams top-level JSON values from an io.Reader: NDJSON, other
// whitespace-separated values, and directly concatenated values all work.
// Next advances to the next complete value, validating it in full, so a
// true result guarantees Bytes holds exactly one valid JSON value.
//
// The reader owns one rolling buffer. Values are exposed as aliases into it:
// Bytes, and any zero-copy decode of the current value, are valid only until
// the next call to Next or DecodeNext. Every such call invalidates the previous
// current value before attempting to advance, including a call that returns
// false. Close also clears the current value. A value that arrives split across
// reads costs one compacting copy of its partial prefix; everything else is
// read straight into place, and framing allocates nothing once the buffer has
// enough capacity for the working value.
// ReaderOptions.MaxValueBytes limits accepted value length when nonzero; it
// does not cap buffer capacity. All reads and decoding happen on the caller's
// goroutine; Reader does not start background workers and is not safe for
// concurrent use. Use DecodeNext for typed streams and Cursor for a forward
// dynamic pass. Use Parse or BuildIndex instead when a value must support
// retained, out-of-order navigation.
type Reader struct {
	in     io.Reader
	buf    []byte
	closed bool

	pos int // scan position within buf
	end int // valid bytes end within buf

	valStart int // current value extent
	valEnd   int
	// readErr is a non-EOF source error, held until the bytes that arrived
	// with or before it have been scanned; io.Reader delivers data and
	// error together and the data comes first.
	readErr error

	consumed int64 // bytes discarded before buf[0], for error offsets
	maxValue int
	eof      bool
	hasValue bool
	err      error
}

// ReaderOptions configures a Reader before input consumption begins. The
// constructor copies the options and does not read from the input. A zero
// BufferSize uses the default, and a zero MaxValueBytes leaves value size
// unbounded.
type ReaderOptions struct {
	// BufferSize is the initial rolling-buffer size, not a capacity limit. Zero
	// uses the default; negative values are rejected, and positive values below
	// 512 are rounded up to 512. The buffer grows when a value does not fit.
	BufferSize int
	// MaxValueBytes rejects a framed value larger than this many bytes, excluding
	// inter-value whitespace. Zero is unbounded and negative values are rejected.
	MaxValueBytes int
}

// ErrReaderClosed is returned by DecodeFrom when the Reader has been closed.
// Next and DecodeNext instead report a closed Reader by returning false; Close
// itself does not add an error to Err.
var ErrReaderClosed = errors.New("simdjson: reader closed")

// valueFrame resumably locates the end of one JSON value across buffer refills.
// It advances a cursor through newly available bytes only, keeping O(1) state,
// so a value that arrives in K chunks is framed in O(value length) total rather
// than the O(K·length) of re-scanning it from the start on every refill. It
// tracks structure only; the caller validates the framed extent once. framed
// counts value bytes consumed relative to the value start, so buffer compaction
// (which shifts the start) needs no adjustment.
type valueFrame struct {
	mode    uint8 // frameContainer, frameString, frameNumber, frameLiteral
	depth   int   // open { and [ for containers
	inStr   bool  // inside a string within a container
	esc     bool  // previous string byte was an unescaped backslash
	numE    bool  // previous number byte was e or E (a following +/- stays in it)
	litLeft int   // literal bytes still expected
	framed  int   // value bytes consumed so far, from the value start
}

const (
	frameContainer uint8 = iota
	frameString
	frameNumber
	frameLiteral
)

// init classifies the value from its leading byte and consumes it. An
// unrecognized leading byte is framed as a one-byte number so the caller's
// validator rejects it.
func (f *valueFrame) init(c byte) {
	f.framed = 1
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

// scanStringBody advances i over string content in src[:n], using the SIMD
// special-byte scanner to skip runs of ordinary content in vector-width strides
// rather than one byte at a time. It resumes across chunk boundaries through
// f.esc (a pending escape) and returns the index just past the closing quote
// with done=true when the string closes within [i,n); otherwise it returns n
// and done=false, carrying f.esc for the next chunk. The scanner also halts on
// control and non-ASCII bytes, which are plain content for framing and are
// skipped; only the quote and backslash change structural state, so the framed
// extent is identical to a byte-by-byte scan. Bounding the scan with src[:n]
// keeps it inside the buffered bytes and away from the unread tail.
func (f *valueFrame) scanStringBody(src []byte, i, n int) (int, bool) {
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

// scan advances the frame over src[start+framed : n], resuming its state. It
// returns true once the value is structurally complete: the closing bracket or
// quote is consumed, a fixed-length literal is filled, or (for a number) a
// delimiter byte follows. A number that reaches n without a delimiter stays
// incomplete; at end of input the caller treats it as ending at n.
func (f *valueFrame) scan(src []byte, start, n int) bool {
	i := start + f.framed
	switch f.mode {
	case frameString:
		end, done := f.scanStringBody(src, i, n)
		f.framed = end - start
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
					f.framed = i - start
					return true
				}
				f.numE = false
			default:
				f.framed = i - start
				return true
			}
			i++
		}
	case frameLiteral:
		for i < n && f.litLeft > 0 {
			i++
			f.litLeft--
		}
		f.framed = i - start
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
					f.framed = i - start
					return true
				}
			}
		}
	}
	f.framed = i - start
	return false
}

// defaultReaderSize holds several typical NDJSON records per read.
const defaultReaderSize = 64 << 10

// NewReader returns a Reader with a 64 KiB initial rolling buffer and no
// per-value size bound. It allocates the buffer but does not read from in. Use
// NewReaderWithOptions for a bounded Reader.
func NewReader(in io.Reader) *Reader {
	return &Reader{in: in, buf: make([]byte, defaultReaderSize)}
}

// NewReaderWithOptions allocates a configured rolling buffer without reading
// from in. Invalid negative sizes are rejected; positive buffer sizes below
// 512 bytes are rounded up to preserve the Reader's minimum working capacity.
func NewReaderWithOptions(in io.Reader, options ReaderOptions) (*Reader, error) {
	if options.BufferSize < 0 {
		return nil, fmt.Errorf("simdjson: negative Reader buffer size %d", options.BufferSize)
	}
	if options.MaxValueBytes < 0 {
		return nil, fmt.Errorf("simdjson: negative Reader value limit %d", options.MaxValueBytes)
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

// Close transitions a Reader to its terminal state. It is safe to call at any
// point and is idempotent. It releases the Reader's references to its input and
// rolling buffer; slices returned by Bytes before Close remain caller-held
// aliases. After Close, Bytes is nil and every Next or DecodeNext returns
// false. Close does not report stream errors; use Err for those.
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

// Err returns the first input, framing, validation, size-limit, or DecodeNext
// decoding error. The error is sticky. Err is nil after clean end of stream and
// after Close unless an earlier stream error was already recorded. DecodeFrom
// returns destination decoding errors directly without recording them here.
func (r *Reader) Err() error {
	return r.err
}

// InputOffset returns the exclusive input offset at the end of the most recent
// value produced by Next or DecodeNext. It is meaningful after a successful
// advance and remains unchanged by Close.
func (r *Reader) InputOffset() int64 {
	return r.consumed + int64(r.valEnd)
}

// Bytes returns the current value as an alias of the Reader's rolling buffer.
// It returns nil unless the most recent Next or DecodeNext call succeeded, and
// also returns nil after Close. The alias is valid only until the next advance;
// Close drops the Reader's reference but does not mutate a caller-held slice.
func (r *Reader) Bytes() []byte {
	if !r.hasValue {
		return nil
	}
	return r.buf[r.valStart:r.valEnd]
}

// DecodeFrom decodes the current value through a compiled decoder without
// advancing the Reader. The most recent Next or DecodeNext call must have
// succeeded. Decoders compiled with ZeroCopy alias the rolling buffer and
// follow the Bytes validity window; owned decoders copy and are safe to retain.
// A destination decoding error is returned directly and does not poison the
// Reader, so the current value may be decoded again or skipped by advancing.
func DecodeFrom[T any](r *Reader, dec Decoder[T], dst *T) error {
	if !r.hasValue {
		if r.closed {
			return ErrReaderClosed
		}
		if r.err != nil {
			return r.err
		}
		return errors.New("simdjson: DecodeFrom without a current value; call Next first")
	}
	return dec.Decode(r.buf[r.valStart:r.valEnd], dst)
}

// DecodeNext advances to the next value and decodes it in one pass, combining
// Next and DecodeFrom. The value's extent is located by a resumable structural
// frame, so a value split across reads is scanned once and decoded once, even
// when it spans many refills. It returns false at the end of the stream or on
// error; Err distinguishes those cases. A closed Reader also returns false
// without adding ErrReaderClosed to Err. Decode and stream errors are recorded
// in Err and make later advances return false. After a true result, Bytes and
// InputOffset describe the decoded value; a ZeroCopy destination follows the
// same invalidation window as Bytes.
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
		i = skipSpace(r.buf[:r.end], i)
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
	var fr valueFrame
	fr.init(r.buf[i])
	framed := false
	decodedN := -1
	for {
		if !framed {
			framed = fr.scan(r.buf, i, r.end)
		}
		if decodedN < 0 && (framed || r.eof) {
			n, err := dec.DecodePrefix(r.buf[i:r.end], dst)
			if err != nil {
				// The value is fully buffered (framed) or the input ended
				// mid-value; the error is real. Diagnose the framed extent so
				// the reason does not depend on trailing input.
				if verr := Validate(r.buf[i : i+fr.framed]); verr != nil {
					err = verr
				}
				r.err = fmt.Errorf("simdjson: value at input offset %d: %w", r.consumed+int64(i), err)
				return false
			}
			decodedN = n
		}
		if decodedN >= 0 {
			end := i + decodedN
			if end < r.end || r.eof {
				if r.maxValue > 0 && decodedN > r.maxValue {
					r.err = fmt.Errorf("simdjson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
					return false
				}
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

// Next invalidates the previous current value and advances to the next fully
// validated value. It returns false at clean end of stream, after Close, or on
// error; Err distinguishes an input or validation error from clean termination.
// After a true result, Bytes and InputOffset describe the new current value.
func (r *Reader) Next() bool {
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
		i = skipSpace(r.buf[:r.end], i)
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

	// Fast path: the value is usually already fully buffered, and one
	// validation pass both checks it and locates its end. It counts only when
	// something confirms the boundary — a byte after the value or the end of
	// input — since a number or literal ending exactly at the buffer edge may
	// continue in unread input. Anything unconfirmed or invalid falls through
	// to the resumable framer below, which settles incomplete-versus-invalid
	// exactly as before; the retried prefix is bounded by one buffer, so a
	// value spanning refills still frames in linear time overall.
	{
		window := r.buf[:r.end]
		if end, ok := validRootValueFast(window, r.end, i, window[i]); ok && (end < r.end || r.eof) {
			if r.maxValue > 0 && end-i > r.maxValue {
				r.err = fmt.Errorf("simdjson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
				return false
			}
			r.valStart, r.valEnd = i, end
			r.pos = end
			r.hasValue = true
			return true
		}
	}

	// Locate the value's end by resumable framing, so a value spanning many
	// refills is scanned once rather than re-scanned from the start each time.
	// Once the value is fully buffered it is validated exactly once; validLen
	// caches that result (relative to the value start, so buffer compaction
	// needs no adjustment) while a scalar awaits the byte that confirms its end.
	var fr valueFrame
	fr.init(r.buf[i])
	framed := false
	validLen := -1
	for {
		if !framed {
			framed = fr.scan(r.buf, i, r.end)
		}
		if validLen < 0 && (framed || r.eof) {
			window := r.buf[:r.end]
			end, ok := validRootValueFast(window, r.end, i, window[i])
			if !ok {
				// The value is fully buffered (framed) or the input ended
				// mid-value; either way it will not become valid. Diagnose the
				// framed extent, not the whole buffer, so the reported reason
				// does not depend on how much trailing input has arrived.
				verr := Validate(r.buf[i : i+fr.framed])
				if verr == nil {
					verr = io.ErrUnexpectedEOF
				}
				if r.readErr != nil {
					verr = r.readErr
				}
				r.err = fmt.Errorf("simdjson: invalid value at input offset %d: %w", r.consumed+int64(i), verr)
				return false
			}
			validLen = end - i
		}
		if validLen >= 0 {
			end := i + validLen
			if end < r.end || r.eof {
				// A number or literal ending exactly at the buffer edge may
				// continue in unread input, so it only counts once a byte
				// follows it or the input ended.
				if r.maxValue > 0 && validLen > r.maxValue {
					r.err = fmt.Errorf("simdjson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(i), r.maxValue)
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

// fill reads more input, compacting or growing the buffer so the candidate
// value starting at *keep stays available. It returns false when no new
// bytes will ever arrive; r.err is set only for real read errors.
func (r *Reader) fill(keep *int) bool {
	if r.eof {
		return false
	}
	if r.end == len(r.buf) {
		if r.maxValue > 0 && r.end-*keep > r.maxValue {
			r.err = fmt.Errorf("simdjson: value at input offset %d exceeds the %d byte limit", r.consumed+int64(*keep), r.maxValue)
			return false
		}
		if *keep > 0 {
			// Drop everything before the candidate value, keeping the
			// last delivered value's offsets pointing at the same input
			// positions.
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
		n, err := r.in.Read(r.buf[r.end:])
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
