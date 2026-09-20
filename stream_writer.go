package vibejson

import (
	"errors"
	"io"
	"time"

	simdkernels "github.com/thesyncim/vibejson/simd"
)

// Writer streams JSON through one reused buffer. It supports compiled values
// and token methods and is not safe for concurrent use.
type Writer struct {
	out        io.Writer
	buf        []byte
	flushAt    int
	err        error
	escapeHTML bool
	timeCache  simdkernels.TimeCache

	// Container state for token methods.
	stack   []streamFrame
	started bool // the current top-level token value emitted something
}

type streamFrame struct {
	kind     byte // '{' or '['
	members  int
	afterKey bool
}

// defaultWriterSize is the automatic-flush threshold.
const defaultWriterSize = 32 << 10

// NewWriter returns a Writer with the default flush threshold.
func NewWriter(out io.Writer) *Writer {
	return newWriterWithFlushThreshold(out, defaultWriterSize)
}

// newWriterWithFlushThreshold constructs a Writer with an automatic-flush
// threshold. Values below 512 are rounded up.
func newWriterWithFlushThreshold(out io.Writer, size int) *Writer {
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

// SetEscapeHTML controls HTML escaping for subsequent strings.
func (w *Writer) SetEscapeHTML(escape bool) {
	w.escapeHTML = escape
}

// Err returns the first encoding, usage, or sink error.
func (w *Writer) Err() error {
	return w.err
}

// EncodeTo appends one complete top-level value through a compiled Encoder.
func EncodeTo[T any](w *Writer, enc Encoder[T], src *T) error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("vibejson: EncodeTo inside an unfinished token value"))
	}
	if w.started {
		return w.fail(errors.New("vibejson: second top-level value without Newline"))
	}
	dst, err := enc.AppendJSON(w.buf, src)
	if err != nil {
		return w.fail(err)
	}
	w.buf = dst
	w.started = true
	return w.maybeFlush()
}

// Newline appends a line feed between top-level values.
func (w *Writer) Newline() error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("vibejson: Newline inside an unfinished token value"))
	}
	w.buf = append(w.buf, '\n')
	w.started = false
	return w.maybeFlush()
}

// RawUnchecked copies an already encoded JSON value without validation.
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
		return w.fail(errors.New("vibejson: EndObject without a matching open object"))
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
		return w.fail(errors.New("vibejson: EndArray without a matching open array"))
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
		return w.fail(errors.New("vibejson: Key outside an object member position"))
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

// Float64 writes a JSON number.
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

// Time writes an RFC 3339 string value.
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

// Flush writes all buffered output.
func (w *Writer) Flush() error {
	if w.err != nil {
		return w.err
	}
	if len(w.stack) != 0 {
		return w.fail(errors.New("vibejson: Flush inside an unfinished token value"))
	}
	return w.flush()
}

// Close flushes the buffer; it does not close the underlying writer.
func (w *Writer) Close() error {
	return w.Flush()
}

// beforeValue validates a value position and writes sibling separators.
func (w *Writer) beforeValue() bool {
	if w.err != nil {
		return false
	}
	top := len(w.stack) - 1
	if top < 0 {
		if w.started {
			w.fail(errors.New("vibejson: second top-level token value without Newline"))
			return false
		}
		w.started = true
		return true
	}
	frame := &w.stack[top]
	switch frame.kind {
	case '{':
		if !frame.afterKey {
			w.fail(errors.New("vibejson: object value without a preceding Key"))
			return false
		}
	default:
		if frame.members > 0 {
			w.buf = append(w.buf, ',')
		}
	}
	return true
}

// afterValue updates container state and flushes completed top-level values.
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
	if err == nil && n != len(w.buf) {
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
