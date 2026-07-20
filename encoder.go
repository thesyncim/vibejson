package simdjson

import (
	"reflect"
	"sync"
	"sync/atomic"
)

// EncoderOptions controls encoding output.
type EncoderOptions struct {
	// DisableHTMLEscaping leaves <, >, and & unescaped in strings, like
	// json.Encoder.SetEscapeHTML(false). The zero value matches
	// encoding/json.Marshal, which escapes them.
	DisableHTMLEscaping bool

	// InlineFields activates the ",inline" struct-tag extension: a
	// map[string]T field tagged `json:",inline"` re-emits its members at the
	// enclosing object's own level, after the declared fields. It mirrors
	// DecoderOptions.InlineFields and is opt-in so the tag stays inert, and
	// free, for every type that does not request it.
	InlineFields bool
}

// Encoder is an immutable compiled encoder for one concrete Go type. Use
// Marshal for occasional calls; use an Encoder when encoding the type
// repeatedly, when options are required, or when output storage should be
// reused. An Encoder may be used concurrently because AppendJSON keeps mutable
// state local to the call. Output matches encoding/json byte for byte: compact,
// with U+2028 and U+2029 escaped and invalid UTF-8 replaced.
type Encoder[T any] struct {
	root       *typedNode
	escapeHTML bool
	scratch    *sync.Pool
}

// CompileEncoder builds an encoder for T. It supports the same types as
// CompileDecoder plus the omitempty and string tag options.
func CompileEncoder[T any](opts EncoderOptions) (Encoder[T], error) {
	typ := reflect.TypeFor[T]()
	escapeHTML := !opts.DisableHTMLEscaping
	compiler := newTypedCompiler(typedCompileEncode)
	compiler.escapeHTML = escapeHTML
	compiler.inlineFields = opts.InlineFields
	root, err := compiler.compile(typ, typ.String())
	if err != nil {
		return Encoder[T]{}, err
	}
	computeEncPtrMarshaler(root, make(map[*typedNode]bool))
	return Encoder[T]{
		root:       root,
		escapeHTML: escapeHTML,
		scratch: newEncoderScratchPool(
			compiler.encScratchTypes, compiler.encBackingSlots, compiler.encHasMap,
		),
	}, nil
}

// marshalEncoders caches one encoder per source type for Marshal.
var marshalEncoders sync.Map

type cachedEncoder[T any] struct {
	encoder  Encoder[T]
	err      error
	sizeHint atomic.Uint64
}

// Marshal encodes src like encoding/json.Marshal, including HTML escaping.
// The encoder for each source type is compiled once and cached for the
// process lifetime, along with an adaptive output-size estimate that requires
// two large observations before exact presizing. Hot paths that encode one type repeatedly should call
// CompileEncoder once and reuse the returned Encoder with AppendJSON to
// recycle output buffers.
func Marshal[T any](src *T) ([]byte, error) {
	entry, ok := marshalEncoders.Load(reflect.TypeFor[T]())
	if !ok {
		entry = newCachedEncoder[T]()
	}
	cached := entry.(*cachedEncoder[T])
	if cached.err != nil {
		return nil, cached.err
	}
	out, err := cached.encoder.AppendJSON(make([]byte, 0, loadMarshalSizeHint(&cached.sizeHint)), src)
	if err != nil {
		return nil, err
	}
	updateMarshalSizeHint(&cached.sizeHint, uint64(len(out)))
	return out, nil
}

// newCachedEncoder stays out of line so Marshal's cache-hit path does not
// carry compilation code in its frame.
//
//go:noinline
func newCachedEncoder[T any]() any {
	encoder, err := CompileEncoder[T](EncoderOptions{})
	entry, _ := marshalEncoders.LoadOrStore(reflect.TypeFor[T](), &cachedEncoder[T]{encoder: encoder, err: err})
	return entry
}
