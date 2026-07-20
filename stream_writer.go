package simdjson

import (
	"errors"
	"io"
	"time"

	simdkernels "github.com/thesyncim/simdjson/simd"
)

// Writer streams JSON to an io.Writer through one reused buffer. Use EncodeTo
// for compiled Go values and the token methods for documents assembled a field
// at a time. For a single in-memory value, Encoder.AppendJSON is simpler.
//
// Two levels are available and may be mixed between top-level values.
// EncodeTo appends one complete value through a compiled [Encoder]. The token
// methods (BeginObject, Key, Int, ...) build a value by hand with the same
// byte output as Marshal; the writer tracks container state, inserts commas,
// and turns any call that would produce malformed JSON into an error instead
// of corrupt output.
//
// Writer owns its buffer and framing state but not the underlying io.Writer.
// Successful flushes retain buffer capacity for reuse; a value larger than the
// current capacity grows that buffer and is still emitted whole. Errors are
// sticky: after an encoding, usage, or sink error, output methods make no
// further progress and Err, Flush, and Close report the first failure. A sink
// may have accepted a prefix before reporting an error, so failed output cannot
// be retried through the Writer. A Writer is not safe for concurrent use.
type Writer struct {
	out        io.Writer
	buf        []byte
	flushAt    int
	err        error
	escapeHTML bool
	timeCache  simdkernels.TimeCache

	// Container state for the token layer. Each level records the kind of
	// open container, whether it has members, and — inside objects —
	// whether a key is pending its value. Depth zero is the stream itself,
	// where values are separated by nothing (callers add Newline for
	// NDJSON).
	stack   []streamFrame
	started bool // the current top-level token value emitted something
}

type streamFrame struct {
	kind     byte // '{' or '['
	members  int
	afterKey bool
}

// defaultWriterSize is the flush threshold: large enough to amortize the
// io.Writer call, small enough to stay cache-friendly.
const defaultWriterSize = 32 << 10

// NewWriter returns a Writer with a 32 KiB flush threshold. It allocates
// reusable buffering and framing state but writes nothing to out. Output
// matches encoding/json, including HTML escaping; see SetEscapeHTML.
func NewWriter(out io.Writer) *Writer {
	return NewWriterSize(out, defaultWriterSize)
}

// NewWriterSize is NewWriter with an explicit flush threshold in bytes.
// Values below 512, including non-positive values, are rounded up to 512. The
// threshold is not a value-size limit: one value may grow the buffer beyond it,
// then is flushed whole after completion. Construction writes nothing to out.
func NewWriterSize(out io.Writer, size int) *Writer {
	if size < 512 {
		size = 512
	}
	return &Writer{
		out:        out,
		buf:        make([]byte, 0, size),
		flushAt:    size,
		escapeHTML: true,
		stack:      make([]streamFrame, 0, 16),
	}
}

// SetEscapeHTML controls whether subsequent strings escape <, >, and &, like
// json.Encoder.SetEscapeHTML. It does not rewrite buffered output or reject a
// policy change inside an unfinished token-built value; callers requiring one
// policy per document must change it only between top-level values.
func (w *Writer) SetEscapeHTML(escape bool) {
	w.escapeHTML = escape
}

// Err returns the first encoding, usage, or sink error, if any. The error is
// sticky. A nil result does not imply buffered output has been flushed.
func (w *Writer) Err() error {
	return w.err
}

// EncodeTo appends one complete top-level value to w through a compiled
// encoder and does not retain src. Encoding or usage errors leave the buffered
// prefix unchanged and become sticky. A sink error during an automatic flush
// may occur after the sink accepted a prefix. It is an error to call EncodeTo
// while a token-built value is unfinished, and consecutive top-level values
// need Newline between them, exactly as with token values—without a separator,
// adjacent numbers would merge into one. Apart from growing the Writer buffer,
// its allocation behavior is that of [Encoder.AppendJSON].
func EncodeTo[T any](w *Writer, enc Encoder[T], src *T) error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("simdjson: EncodeTo inside an unfinished token value"))
	}
	if w.started {
		return w.fail(errors.New("simdjson: second top-level value without Newline"))
	}
	dst, err := enc.AppendJSON(w.buf, src)
	if err != nil {
		return w.fail(err)
	}
	w.buf = dst
	w.started = true
	return w.maybeFlush()
}

// Newline appends a line feed and opens the next top-level value position for
// NDJSON framing. It is an error while a token-built value is unfinished.
func (w *Writer) Newline() error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("simdjson: Newline inside an unfinished token value"))
	}
	w.buf = append(w.buf, '\n')
	w.started = false
	return w.maybeFlush()
}

// RawUnchecked copies an already encoded JSON value into the Writer verbatim,
// like json.RawMessage. It does not retain value; the caller is responsible for
// validity.
func (w *Writer) RawUnchecked(value []byte) error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = append(w.buf, value...)
	return w.afterValue()
}

// BeginObject opens an object member position.
func (w *Writer) BeginObject() error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = append(w.buf, '{')
	w.stack = append(w.stack, streamFrame{kind: '{'})
	return nil
}

// EndObject closes the innermost open object.
func (w *Writer) EndObject() error {
	if w.err != nil {
		return w.err
	}
	top := len(w.stack) - 1
	if top < 0 || w.stack[top].kind != '{' || w.stack[top].afterKey {
		return w.fail(errors.New("simdjson: EndObject without a matching open object"))
	}
	w.stack = w.stack[:top]
	w.buf = append(w.buf, '}')
	return w.afterValue()
}

// BeginArray opens an array element position.
func (w *Writer) BeginArray() error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = append(w.buf, '[')
	w.stack = append(w.stack, streamFrame{kind: '['})
	return nil
}

// EndArray closes the innermost open array.
func (w *Writer) EndArray() error {
	if w.err != nil {
		return w.err
	}
	top := len(w.stack) - 1
	if top < 0 || w.stack[top].kind != '[' {
		return w.fail(errors.New("simdjson: EndArray without a matching open array"))
	}
	w.stack = w.stack[:top]
	w.buf = append(w.buf, ']')
	return w.afterValue()
}

// Key writes the next member name of the innermost open object.
func (w *Writer) Key(name string) error {
	if w.err != nil {
		return w.err
	}
	top := len(w.stack) - 1
	if top < 0 || w.stack[top].kind != '{' || w.stack[top].afterKey {
		return w.fail(errors.New("simdjson: Key outside an object member position"))
	}
	if w.stack[top].members > 0 {
		w.buf = append(w.buf, ',')
	}
	w.buf = appendEncodedJSONString(w.buf, name, w.escapeHTML)
	w.buf = append(w.buf, ':')
	w.stack[top].afterKey = true
	return nil
}

// String writes a string value.
func (w *Writer) String(s string) error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = appendEncodedJSONString(w.buf, s, w.escapeHTML)
	return w.afterValue()
}

// Int writes an integer value.
func (w *Writer) Int(v int64) error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = appendCompactInt(w.buf, v)
	return w.afterValue()
}

// Uint writes an unsigned integer value.
func (w *Writer) Uint(v uint64) error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = appendCompactUint(w.buf, v)
	return w.afterValue()
}

// Float64 writes a number value spelled exactly like Marshal.
func (w *Writer) Float64(v float64) error {
	if !w.beforeValue() {
		return w.err
	}
	dst, err := appendJSONFloat(w.buf, v, 64)
	if err != nil {
		return w.fail(err)
	}
	w.buf = dst
	return w.afterValue()
}

// Bool writes a boolean value.
func (w *Writer) Bool(v bool) error {
	if !w.beforeValue() {
		return w.err
	}
	if v {
		w.buf = append(w.buf, "true"...)
	} else {
		w.buf = append(w.buf, "false"...)
	}
	return w.afterValue()
}

// Null writes a null value.
func (w *Writer) Null() error {
	if !w.beforeValue() {
		return w.err
	}
	w.buf = append(w.buf, "null"...)
	return w.afterValue()
}

// Time writes an RFC 3339 string value, like Marshal on a time.Time.
func (w *Writer) Time(t time.Time) error {
	if !w.beforeValue() {
		return w.err
	}
	dst, err := simdkernels.AppendTimeCached(w.buf, t, &w.timeCache)
	if err != nil {
		return w.fail(err)
	}
	w.buf = dst
	return w.afterValue()
}

// Flush attempts to write all buffered output without opening another
// top-level value position. It is an error while a token-built value is
// unfinished. A successful flush retains buffer capacity; a sink error is
// sticky and may occur after the sink accepted a prefix.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("simdjson: Flush inside an unfinished token value"))
	}
	return w.flush()
}

// Close is equivalent to Flush. It does not close the underlying writer or
// make the Writer terminal. Existing framing state remains: after a completed
// top-level value, Newline is still required before another one.
func (w *Writer) Close() error {
	return w.Flush()
}

// beforeValue validates that a value may start here and writes the comma
// separating it from a preceding sibling.
func (w *Writer) beforeValue() bool {
	if w.err != nil {
		return false
	}
	top := len(w.stack) - 1
	if top < 0 {
		if w.started {
			w.fail(errors.New("simdjson: second top-level token value without Newline"))
			return false
		}
		w.started = true
		return true
	}
	frame := &w.stack[top]
	switch frame.kind {
	case '{':
		if !frame.afterKey {
			w.fail(errors.New("simdjson: object value without a preceding Key"))
			return false
		}
	default:
		if frame.members > 0 {
			w.buf = append(w.buf, ',')
		}
	}
	return true
}

// afterValue closes out one value: bookkeeping, then the flush check when
// the value completed a top-level token document.
func (w *Writer) afterValue() error {
	if top := len(w.stack) - 1; top >= 0 {
		w.stack[top].members++
		w.stack[top].afterKey = false
		return nil
	}
	return w.maybeFlush()
}

func (w *Writer) maybeFlush() error {
	if len(w.buf) < w.flushAt {
		return nil
	}
	return w.flush()
}

func (w *Writer) flush() error {
	if len(w.buf) == 0 {
		return nil
	}
	n, err := w.out.Write(w.buf)
	if err == nil && n < len(w.buf) {
		err = io.ErrShortWrite
	}
	if err != nil {
		return w.fail(err)
	}
	w.buf = w.buf[:0]
	return nil
}

func (w *Writer) fail(err error) error {
	if w.err == nil {
		w.err = err
	}
	return w.err
}
