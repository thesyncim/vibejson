package simdjson

import (
	"fmt"
	"reflect"
	"sync"
	"sync/atomic"
)

// EncoderOptions controls encoding output. [CompileEncoder] copies the value;
// later changes to the caller's options do not affect the compiled encoder.
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
// [Marshal] for occasional calls; use an Encoder when encoding the type
// repeatedly, when options are required, or when output storage should be
// reused. The same Encoder may be used concurrently when each call owns its
// writable destination and the caller synchronizes any mutable source data.
// With zero-value options, output matches encoding/json byte for byte: compact,
// with U+2028 and U+2029 escaped and invalid UTF-8 replaced.
type Encoder[T any] struct {
	root       *typedNode
	escapeHTML bool
	scratch    *sync.Pool
}

// CompileEncoder builds an immutable encoder for T and copies opts. Compilation
// applies encoding/json field selection, struct tags, and custom marshal
// interfaces while allocating the reusable type plan and scratch metadata once.
// A static type that cannot be encoded is reported as an
// [UnsupportedTypeError].
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

// AppendJSON appends src encoded as compact JSON to dst. On success the returned
// caller-owned slice may reuse dst's backing storage. The backing storage of dst
// must not overlap storage reachable from src. The Encoder itself retains
// neither argument after the call, although a custom marshal method can retain
// its receiver under the usual Go method-call rules.
//
// Ordinary compiled sources remain stack eligible unless an addressable custom
// method can retain its receiver; in that case ordinary escape analysis keeps
// caller storage alive. Sufficient dst capacity avoids an output-buffer
// allocation for ordinary compiled fields; dynamic values and custom methods
// may allocate independently.
//
// On error AppendJSON returns dst unchanged in length and with its visible
// prefix intact, but bytes in unused capacity may contain partial output.
func (plan Encoder[T]) AppendJSON(dst []byte, src *T) ([]byte, error) {
	if plan.root == nil {
		return dst, fmt.Errorf("simdjson: zero Encoder")
	}
	if src == nil {
		return dst, fmt.Errorf("simdjson: encode source is nil")
	}
	e := encodeState{dst: dst, escapeHTML: plan.escapeHTML}
	if plan.scratch != nil {
		e.scratch = plan.scratch.Get().(*encoderScratch)
	}
	err := plan.encodeTypedSource(&e, src)
	if plan.scratch != nil {
		e.scratch.reset()
		plan.scratch.Put(e.scratch)
	}
	if err != nil {
		return dst, err
	}
	return e.dst, nil
}

// marshalEncoders caches one encoder per source type for Marshal.
var marshalEncoders sync.Map

type cachedEncoder[T any] struct {
	encoder  Encoder[T]
	err      error
	sizeHint atomic.Uint64
}

// Marshal encodes src like encoding/json.Marshal, including HTML escaping. On
// success it returns a newly owned output slice; on error it returns nil. The
// package does not retain src after the call, although a custom marshal method
// can retain its receiver under the usual Go method-call rules.
//
// The encoder for each source type is compiled once and cached concurrently for
// the process lifetime, along with an adaptive output-size estimate that
// requires two large observations before exact presizing. Hot paths that encode
// one type repeatedly should call [CompileEncoder] once and reuse the returned
// [Encoder] with [Encoder.AppendJSON] to recycle output buffers.
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
