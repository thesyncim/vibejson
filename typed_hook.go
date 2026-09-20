package vibejson

// Hooks provide custom typed decode and encode paths. Cursors and appenders are
// passed by value; retained copies do not advance the enclosing operation.

import "reflect"

// UnmarshalerSimd is a custom decode hook. It must consume exactly one value
// and return the cursor positioned after it.
type UnmarshalerSimd interface {
	UnmarshalVibeJSON(c DecodeCursor) (DecodeCursor, error)
}

// MarshalerSimd is a custom encode hook. Its output must be valid compact JSON
// and the returned appender must be the advanced value.
type MarshalerSimd interface {
	MarshalVibeJSON(w TrustedAppender) TrustedAppender
}

var (
	unmarshalerSimdReflectType = reflect.TypeFor[UnmarshalerSimd]()
	marshalerSimdReflectType   = reflect.TypeFor[MarshalerSimd]()
)

// DecodeCursor exposes the typed parser to an UnmarshalerSimd hook. It must be
// threaded linearly and returned after consuming one value.
type DecodeCursor struct {
	d decoderCursor
}

// TrustedAppender is the by-value output builder passed to MarshalVibeJSON.
// Errors are sticky; the appender and its buffer are call-scoped.
type TrustedAppender struct {
	dst        []byte
	escapeHTML bool
	bad        bool
}

// RawUnchecked appends lit without validation or escaping.
func (w TrustedAppender) RawUnchecked(lit string) TrustedAppender {
	w.dst = append(w.dst, lit...)
	return w
}

// RawBytesUnchecked appends lit without validation or escaping.
func (w TrustedAppender) RawBytesUnchecked(lit []byte) TrustedAppender {
	w.dst = append(w.dst, lit...)
	return w
}

// RawByteUnchecked appends one byte without validation.
func (w TrustedAppender) RawByteUnchecked(b byte) TrustedAppender {
	w.dst = append(w.dst, b)
	return w
}

// Null appends the JSON null literal.
func (w TrustedAppender) Null() TrustedAppender {
	w.dst = append(w.dst, "null"...)
	return w
}

// Bool appends true or false.
func (w TrustedAppender) Bool(v bool) TrustedAppender {
	if v {
		w.dst = append(w.dst, "true"...)
	} else {
		w.dst = append(w.dst, "false"...)
	}
	return w
}

// Int appends v in base 10.
func (w TrustedAppender) Int(v int64) TrustedAppender {
	w.dst = appendCompactInt(w.dst, v)
	return w
}

// Uint appends v in base 10.
func (w TrustedAppender) Uint(v uint64) TrustedAppender {
	w.dst = appendCompactUint(w.dst, v)
	return w
}

// String appends s as a JSON string using the encoder's escaping options.
func (w TrustedAppender) String(s string) TrustedAppender {
	w.dst = appendEncodedJSONString(w.dst, s, w.escapeHTML)
	return w
}

// Float64 appends v in encoding/json's shortest form; NaN and infinity poison it.
func (w TrustedAppender) Float64(v float64) TrustedAppender {
	dst, err := appendJSONFloat(w.dst, v, 64)
	if err != nil {
		w.bad = true
		return w
	}
	w.dst = dst
	return w
}

// Float32 appends v in encoding/json's shortest 32-bit form.
func (w TrustedAppender) Float32(v float32) TrustedAppender {
	dst, err := appendJSONFloat(w.dst, float64(v), 32)
	if err != nil {
		w.bad = true
		return w
	}
	w.dst = dst
	return w
}

// EscapeHTML reports whether HTML-sensitive bytes are escaped.
func (w TrustedAppender) EscapeHTML() bool { return w.escapeHTML }

// BeginObject consumes an object opening brace.
func (c *DecodeCursor) BeginObject(typeName string) error { return c.d.BeginObject(typeName) }

// BeginArray consumes an array opening bracket.
func (c *DecodeCursor) BeginArray(typeName string) error { return c.d.BeginArray(typeName) }

// NextField returns the next object member. Pass first=true after BeginObject.
func (c *DecodeCursor) NextField(first bool) (key string, ok bool, err error) {
	return c.d.NextObjectField(first)
}

// Field matches and consumes one expected member, leaving the cursor on its value.
func (c *DecodeCursor) Field(first bool, f *Field) bool {
	return c.d.matchObjectFieldExpected(first, &f.f)
}

// CaseSensitive reports the decoder's field matching mode.
func (c *DecodeCursor) CaseSensitive() bool { return c.d.CaseSensitive() }

// NextElement reports whether another array element follows. Pass first=true
// after BeginArray.
func (c *DecodeCursor) NextElement(first bool) (bool, error) { return c.d.NextArrayElement(first) }

// Expect consumes ch when it is the next byte.
func (c *DecodeCursor) Expect(ch byte) bool {
	d := &c.d
	if i := d.i; i < len(d.src) && d.src[i] == ch {
		d.i = i + 1
		return true
	}
	return false
}

// ExpectObjectClose consumes a closing brace and updates depth.
func (c *DecodeCursor) ExpectObjectClose() bool {
	d := &c.d
	if i := d.i; i < len(d.src) && d.src[i] == '}' {
		d.i = i + 1
		d.depth--
		return true
	}
	return false
}

// Skip validates and consumes one JSON value without materializing it.
func (c *DecodeCursor) Skip() error { return c.d.Skip() }

// Null consumes a null literal when present.
func (c *DecodeCursor) Null() (bool, error) { return c.d.TryNull() }

// Raw validates and consumes one value, returning a source alias.
func (c *DecodeCursor) Raw() (RawValue, error) {
	d := &c.d
	start := d.i
	if err := d.Skip(); err != nil {
		return RawValue{}, err
	}
	return RawValue{Src: d.src[start:d.i]}, nil
}

// Bool decodes a JSON boolean into dst, including a defined boolean type.
func (c *DecodeCursor) Bool[T ~bool](dst *T) error { return c.d.Bool(dst) }

// Int decodes a JSON integer into dst.
func (c *DecodeCursor) Int[T signedInteger](dst *T) error { return c.d.Int(dst) }

// Uint decodes a JSON integer into dst.
func (c *DecodeCursor) Uint[T unsignedInteger](dst *T) error { return c.d.Uint(dst) }

// Float decodes a JSON number into dst.
func (c *DecodeCursor) Float[T floatValue](dst *T) error { return c.d.Float(dst) }

// String decodes a JSON string into dst.
func (c *DecodeCursor) String[T ~string](dst *T) error { return c.d.String(dst) }

// NumberText decodes a JSON number as literal text.
func (c *DecodeCursor) NumberText[T ~string](dst *T) error { return c.d.Number(dst) }
