package simdjson

import (
	"reflect"
	"sync"
)

// unmarshalDecoders caches one default-option decoder per destination type.
// Values are *cachedDecoder[T] stored under reflect.TypeFor[T]().
var unmarshalDecoders sync.Map

type cachedDecoder[T any] struct {
	decoder Decoder[T]
	err     error
}

// anyReflectType keys the *any special case in Unmarshal.
var anyReflectType = reflect.TypeFor[any]()

// Unmarshal decodes exactly one JSON value from src into dst like
// encoding/json.Unmarshal. It merges into existing destination state, treats
// null according to encoding/json's rules, and matches object fields
// case-insensitively after an exact match. Decoded results do not alias src; the
// package does not retain src after the call. Custom unmarshal methods receive
// input bytes under their standard copy-if-retained contract.
//
// A syntax failure is reported as a [SyntaxError], and valid JSON incompatible
// with the destination is reported as a [DecodeError]. On any error dst may be
// partially modified; Unmarshal does not roll changes back.
//
// The decoder for each destination type is compiled once and cached concurrently
// for the lifetime of the process. Calls may run concurrently when each has a
// separately synchronized destination. Hot paths that decode one type
// repeatedly should call [CompileDecoder] once and reuse the returned [Decoder];
// that also unlocks [DecoderOptions.ZeroCopy] and the other options.
func Unmarshal[T any](src []byte, dst *T) error {
	typ := reflect.TypeFor[T]()
	if typ == anyReflectType && dst != nil {
		// A *any destination needs no compiled plan, so it skips the decoder
		// cache: the lookup is a visible fraction of decoding a small
		// document. The dynamic builder applies unless the destination holds
		// a non-nil pointer, which takes encoding/json's decode-into-pointer
		// merge through the compiled cursor path below.
		out := any(dst).(*any)
		if existing := *out; existing == nil || !anyDecodeMerges(existing) {
			value, err := unmarshalAny(src, DecoderOptions{})
			if err != nil {
				return err
			}
			*out = value
			return nil
		}
	}
	entry, ok := unmarshalDecoders.Load(typ)
	if !ok {
		entry = newCachedDecoder[T]()
	}
	cached := entry.(*cachedDecoder[T])
	if cached.err != nil {
		return cached.err
	}
	return cached.decoder.Decode(src, dst)
}

//go:noinline
func newCachedDecoder[T any]() any {
	decoder, err := CompileDecoder[T](DecoderOptions{})
	entry, _ := unmarshalDecoders.LoadOrStore(reflect.TypeFor[T](), &cachedDecoder[T]{decoder: decoder, err: err})
	return entry
}
