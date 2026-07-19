package simdjson

import (
	"fmt"
	"sync/atomic"
)

// CodecOptions configures both directions of a Codec.
type CodecOptions struct {
	Decoder DecoderOptions
	Encoder EncoderOptions
}

// Codec bundles a compiled Encoder and Decoder for one type. Use it when a
// protocol or stream needs both directions under one options value. AppendJSON
// reuses caller buffers, Marshal uses a per-codec adaptive size estimate, and
// EncodeTo and DecodeFrom connect directly to Writer and Reader. A Codec is
// immutable after compilation and may be used concurrently.
type Codec[T any] struct {
	enc  Encoder[T]
	dec  Decoder[T]
	hint *atomic.Uint64
}

// CompileCodec builds both directions for T.
func CompileCodec[T any](opts CodecOptions) (Codec[T], error) {
	dec, err := CompileDecoder[T](opts.Decoder)
	if err != nil {
		return Codec[T]{}, err
	}
	enc, err := CompileEncoder[T](opts.Encoder)
	if err != nil {
		return Codec[T]{}, err
	}
	return Codec[T]{enc: enc, dec: dec, hint: new(atomic.Uint64)}, nil
}

// Encoder returns the underlying compiled encoder.
func (c Codec[T]) Encoder() Encoder[T] { return c.enc }

// Decoder returns the underlying compiled decoder.
func (c Codec[T]) Decoder() Decoder[T] { return c.dec }

// Unmarshal decodes one complete JSON value into dst.
func (c Codec[T]) Unmarshal(src []byte, dst *T) error {
	return c.dec.Decode(src, dst)
}

// UnmarshalArray decodes a top-level array, reusing dst's capacity.
func (c Codec[T]) UnmarshalArray(src []byte, dst []T) ([]T, error) {
	return c.dec.DecodeArray(src, dst)
}

// AppendJSON appends src encoded as compact JSON to dst, under the same
// contract as Encoder.AppendJSON.
func (c Codec[T]) AppendJSON(dst []byte, src *T) ([]byte, error) {
	return c.enc.AppendJSON(dst, src)
}

// Marshal encodes src into a new buffer presized by the codec's adaptive
// output-size estimate. The estimate retains only an integer, not output
// storage; a single exceptional large result is not used for exact presizing
// until an equal second observation confirms that workload.
func (c Codec[T]) Marshal(src *T) ([]byte, error) {
	if c.hint == nil {
		return nil, fmt.Errorf("simdjson: zero Codec")
	}
	out, err := c.enc.AppendJSON(make([]byte, 0, loadMarshalSizeHint(c.hint)), src)
	if err != nil {
		return nil, err
	}
	updateMarshalSizeHint(c.hint, uint64(len(out)))
	return out, nil
}

// EncodeTo appends one value to a streaming Writer.
func (c Codec[T]) EncodeTo(w *Writer, src *T) error {
	return EncodeTo(w, c.enc, src)
}

// DecodeFrom decodes the Reader's current value. Zero-copy decodes alias the
// reader's buffer and follow the Bytes validity window.
func (c Codec[T]) DecodeFrom(r *Reader, dst *T) error {
	return DecodeFrom(r, c.dec, dst)
}
