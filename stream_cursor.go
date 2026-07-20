package simdjson

import "github.com/thesyncim/simdjson/document"

// This file implements the tape-free forward cursor for streamed values.
//
// Parse builds a structural index so callers can navigate a document in any
// order, revisit values, and hold on to subtrees. A streaming consumer that
// reads each value once, in document order, needs none of that: the index is
// built, copied, and thrown away per value. ValueCursor is the single-pass
// alternative: Reader.Next has already validated the current value in full,
// so the cursor only navigates, reading scalars on demand with the same
// kernels the compiled typed decoders use and skipping unwanted values at
// structural speed. One pass, no tape, and no per-value allocation on
// escape-free input.

// ValueCursor reads the Reader's current value in one forward pass, straight
// off the reader's buffer, without building a structural index.
//
// The cursor is forward-only and must be driven to match the document. After
// NextField or NextElement positions it on a value, the caller consumes
// exactly one value there — a scalar read, a Begin/Next iteration run to
// completion, or Skip — before asking for the next. Kind classifies the value
// at the cursor without consuming it. Finish confirms the whole value was
// consumed.
//
// Strings returned by Text, NumberText, and NextField follow the Bytes
// validity window: they may alias the reader's buffer and are valid only
// until the next call to Next or DecodeNext. Errors report offsets relative
// to the start of the current value. A ValueCursor is not safe for
// concurrent use.
type ValueCursor struct {
	c decoderCursor
	// first is true when the cursor just entered a container, so the next
	// NextField or NextElement call is that container's first. One bool
	// suffices for arbitrary nesting: a child container can only open after
	// its parent yielded at least one entry, so whenever a child closes the
	// parent's answer is always "not first".
	first bool
}

// Cursor returns a forward cursor over the current value. It is valid only
// until the next call to Next or DecodeNext. Without a current value the
// cursor is empty: Kind reports Invalid and every read fails.
func (r *Reader) Cursor() ValueCursor {
	if !r.hasValue {
		return newValueCursor(nil)
	}
	return newValueCursor(r.buf[r.valStart:r.valEnd])
}

// newValueCursor starts a cursor over one complete, already-validated JSON
// value with no surrounding whitespace, exactly what Reader.Next frames.
func newValueCursor(src []byte) ValueCursor {
	return ValueCursor{c: decoderCursor{src: src, maxDepth: defaultMaxDepth, flags: decoderZeroCopy}}
}

// peek returns the byte at the cursor, or 0 at the end of the value. The
// cursor rests on a significant byte at every value position (Reader trims
// inter-value whitespace and the iteration methods skip interior whitespace),
// so no whitespace skip is needed here.
func (v *ValueCursor) peek() byte {
	if v.c.i < len(v.c.src) {
		return v.c.src[v.c.i]
	}
	return 0
}

// Kind classifies the value at the cursor without consuming it. It is
// meaningful only at value positions: at the start, after NextField or
// NextElement, and never between a Begin and its first Next.
func (v *ValueCursor) Kind() document.Kind {
	switch b := v.peek(); {
	case b == '{':
		return document.Object
	case b == '[':
		return document.Array
	case b == '"':
		return document.String
	case b == 't' || b == 'f':
		return document.Bool
	case b == 'n':
		return document.Null
	case b == '-' || isDigit(b):
		return document.Number
	default:
		return document.Invalid
	}
}

// Null consumes a null value and reports whether one was present. A non-null
// value is left in place.
func (v *ValueCursor) Null() bool {
	if v.peek() == 'n' && literalNullAt(v.c.src, v.c.i) {
		v.c.i += 4
		return true
	}
	return false
}

// Bool consumes a true or false value.
func (v *ValueCursor) Bool() (bool, error) {
	switch v.peek() {
	case 't', 'f':
		var out bool
		err := v.c.Bool(&out)
		return out, err
	}
	return false, v.expected("bool")
}

// Int64 consumes an integer number value.
func (v *ValueCursor) Int64() (int64, error) {
	if b := v.peek(); b != '-' && !isDigit(b) {
		return 0, v.expected("number")
	}
	var out int64
	var err error
	if useStableNumericMethods {
		err = v.c.Int64(&out)
	} else {
		err = v.c.Int(&out)
	}
	return out, err
}

// Uint64 consumes a non-negative integer number value.
func (v *ValueCursor) Uint64() (uint64, error) {
	if !isDigit(v.peek()) {
		return 0, v.expected("number")
	}
	var out uint64
	var err error
	if useStableNumericMethods {
		err = v.c.Uint64(&out)
	} else {
		err = v.c.Uint(&out)
	}
	return out, err
}

// Float64 consumes a number value.
func (v *ValueCursor) Float64() (float64, error) {
	if b := v.peek(); b != '-' && !isDigit(b) {
		return 0, v.expected("number")
	}
	var out float64
	var err error
	if useStableNumericMethods {
		err = v.c.Float64(&out)
	} else {
		err = v.c.Float(&out)
	}
	return out, err
}

// Text consumes a string value and returns its decoded (unescaped) contents.
// Unescaped strings alias the reader's buffer under the Bytes validity
// window; escaped strings decode into fresh storage.
func (v *ValueCursor) Text() (string, error) {
	if v.peek() != '"' {
		return "", v.expected("string")
	}
	var out string
	err := v.c.String(&out)
	return out, err
}

// NumberText consumes a number value and returns its original spelling,
// aliasing the reader's buffer under the Bytes validity window.
func (v *ValueCursor) NumberText() (string, error) {
	if b := v.peek(); b != '-' && !isDigit(b) {
		return "", v.expected("number")
	}
	var out string
	err := v.c.Number(&out)
	return out, err
}

// BeginObject enters an object value. The caller then alternates NextField
// with consuming each field's value until NextField reports false, which
// leaves the cursor past the object.
func (v *ValueCursor) BeginObject() error {
	if err := v.c.BeginObject(""); err != nil {
		return err
	}
	v.first = true
	return nil
}

// NextField advances to the next object field and returns its decoded key.
// It reports false, consuming the closing brace, when the object ends. The
// key follows the Bytes validity window.
func (v *ValueCursor) NextField() (key string, ok bool, err error) {
	key, ok, err = v.c.NextObjectField(v.first)
	v.first = false
	return key, ok, err
}

// BeginArray enters an array value. The caller then alternates NextElement
// with consuming each element until NextElement reports false, which leaves
// the cursor past the array.
func (v *ValueCursor) BeginArray() error {
	if err := v.c.BeginArray(""); err != nil {
		return err
	}
	v.first = true
	return nil
}

// NextElement reports whether another array element is available, consuming
// the closing bracket when the array ends.
func (v *ValueCursor) NextElement() (bool, error) {
	ok, err := v.c.NextArrayElement(v.first)
	v.first = false
	return ok, err
}

// Skip consumes the value at the cursor without decoding it. Reader.Next
// already validated the value, so Skip counts structure without re-checking
// content, hopping string interiors with the vector scanner.
func (v *ValueCursor) Skip() error {
	end, ok := skipValidValue(v.c.src, v.c.i)
	if !ok {
		return v.expected("value")
	}
	v.c.i = end
	return nil
}

// Finish confirms the cursor consumed the value exactly. It is the guard for
// mis-driven walks: a consumer that forgot to finish a container or to
// consume a field's value fails here (or earlier) instead of silently
// misreading.
func (v *ValueCursor) Finish() error {
	return v.c.Finish()
}

func (v *ValueCursor) expected(what string) error {
	return &DecodeError{Offset: v.c.i, Reason: "expected " + what}
}

// skipValidValue returns the position just past the value starting at i,
// which must be a significant byte. src holds known-valid JSON (the Reader
// validated it), so structure alone determines the extent: strings hop
// special bytes with the vector scanner and skip escape pairs blindly,
// containers count brackets iteratively (no recursion, so depth costs no
// stack), and literals take their fixed widths. On input that is not a value
// start it reports false; on truncated input it runs out of bytes and
// reports false rather than reading past the buffer.
func skipValidValue(src []byte, i int) (int, bool) {
	if i >= len(src) {
		return i, false
	}
	switch c := src[i]; {
	case c == '{' || c == '[':
		depth := 1
		i++
		for i < len(src) {
			switch src[i] {
			case '"':
				end := skipValidString(src, i)
				if end < 0 {
					return i, false
				}
				i = end
			case '{', '[':
				depth++
				i++
			case '}', ']':
				depth--
				i++
				if depth == 0 {
					return i, true
				}
			default:
				i++
			}
		}
		return i, false
	case c == '"':
		end := skipValidString(src, i)
		if end < 0 {
			return i, false
		}
		return end, true
	case c == 't' || c == 'n':
		if i+4 > len(src) {
			return i, false
		}
		return i + 4, true
	case c == 'f':
		if i+5 > len(src) {
			return i, false
		}
		return i + 5, true
	case c == '-' || isDigit(c):
		for i++; i < len(src); i++ {
			switch src[i] {
			case ',', ']', '}', ' ', '\t', '\n', '\r':
				return i, true
			}
		}
		return i, true
	default:
		return i, false
	}
}

// skipValidString returns the position just past the string whose opening
// quote is at quote, or -1 when the string does not close within src. Escape
// pairs are skipped without inspection: on validated input only the quote
// and backslash change where the string ends.
func skipValidString(src []byte, quote int) int {
	i := quote + 1
	for i <= len(src) {
		j := scanStringSpecial(src, i)
		if j >= len(src) {
			return -1
		}
		switch src[j] {
		case '"':
			return j + 1
		case '\\':
			i = j + 2
		default: // control or non-ASCII content byte, already validated
			i = j + 1
		}
	}
	return -1
}
