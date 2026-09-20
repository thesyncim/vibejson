package vibejson

import "github.com/thesyncim/vibejson/document"

// ValueCursor reads the Reader's current value in one forward pass without an
// index. It is single-consumer and follows the Reader's validity window.
type ValueCursor struct {
	c decoderCursor
	// first marks the next field or element as the container's first.
	first bool
}

// Cursor returns a forward cursor over the current value.
func (r *Reader) Cursor() ValueCursor {
	if !r.hasValue {
		return newValueCursor(nil)
	}
	return newValueCursor(r.buf[r.valStart:r.valEnd])
}

// newValueCursor starts a cursor over one complete validated value.
func newValueCursor(src []byte) ValueCursor {
	return ValueCursor{c: decoderCursor{src: src, maxDepth: DefaultMaxDepth, flags: decoderZeroCopy}}
}

// peek returns the current significant byte, or 0 at the end.
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
	case b == '-' || IsDigit(b):
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
	if b := v.peek(); b != '-' && !IsDigit(b) {
		return 0, v.expected("number")
	}
	var out int64
	var err error
	err = v.c.Int(&out)
	return out, err
}

// Uint64 consumes a non-negative integer number value.
func (v *ValueCursor) Uint64() (uint64, error) {
	if !IsDigit(v.peek()) {
		return 0, v.expected("number")
	}
	var out uint64
	var err error
	err = v.c.Uint(&out)
	return out, err
}

// Float64 consumes a number value.
func (v *ValueCursor) Float64() (float64, error) {
	if b := v.peek(); b != '-' && !IsDigit(b) {
		return 0, v.expected("number")
	}
	var out float64
	var err error
	err = v.c.Float(&out)
	return out, err
}

// Text consumes a string value.
func (v *ValueCursor) Text() (string, error) {
	if v.peek() != '"' {
		return "", v.expected("string")
	}
	var out string
	err := v.c.String(&out)
	return out, err
}

// NumberText consumes a number value and returns its original spelling.
func (v *ValueCursor) NumberText() (string, error) {
	if b := v.peek(); b != '-' && !IsDigit(b) {
		return "", v.expected("number")
	}
	var out string
	err := v.c.Number(&out)
	return out, err
}

// BeginObject enters an object value.
func (v *ValueCursor) BeginObject() error {
	if err := v.c.BeginObject(""); err != nil {
		return err
	}
	v.first = true
	return nil
}

// NextField advances to the next object field.
func (v *ValueCursor) NextField() (key string, ok bool, err error) {
	key, ok, err = v.c.NextObjectField(v.first)
	v.first = false
	return key, ok, err
}

// BeginArray enters an array value.
func (v *ValueCursor) BeginArray() error {
	if err := v.c.BeginArray(""); err != nil {
		return err
	}
	v.first = true
	return nil
}

// NextElement reports whether another array element is available.
func (v *ValueCursor) NextElement() (bool, error) {
	ok, err := v.c.NextArrayElement(v.first)
	v.first = false
	return ok, err
}

// Skip consumes the value at the cursor without decoding it.
func (v *ValueCursor) Skip() error {
	end, ok := skipValidValue(v.c.src, v.c.i)
	if !ok {
		return v.expected("value")
	}
	v.c.i = end
	return nil
}

// Finish confirms that the cursor consumed the complete value.
func (v *ValueCursor) Finish() error {
	return v.c.Finish()
}

func (v *ValueCursor) expected(what string) error {
	return &DecodeError{Offset: v.c.i, Reason: "expected " + what}
}

// skipValidValue returns the position after a value in validated JSON.
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
	case c == '-' || IsDigit(c):
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

// skipValidString returns the position after a string, or -1 if unterminated.
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
