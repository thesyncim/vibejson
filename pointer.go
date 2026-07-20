package simdjson

import (
	"github.com/thesyncim/simdjson/document"
	"github.com/thesyncim/simdjson/internal/jsonpointer"
)

// CompiledPointer is a parsed RFC 6901 JSON Pointer.
//
// Compile once and reuse it on hot lookup paths to avoid reparsing and
// unescaping pointer tokens on every call. A CompiledPointer is immutable and
// safe to share across goroutines. The zero CompiledPointer represents the
// empty pointer, which selects the value on which it is evaluated.
type CompiledPointer struct {
	pointer string
	tokens  []jsonpointer.Token
}

// CompilePointer parses pointer as an RFC 6901 JSON Pointer. Invalid syntax
// returns a zero CompiledPointer and a [document.PointerError].
func CompilePointer(pointer string) (CompiledPointer, error) {
	tokens, reason := jsonpointer.Compile(pointer)
	if reason != jsonpointer.ReasonNone {
		return CompiledPointer{}, pointerError(pointer, reason)
	}
	return CompiledPointer{pointer: pointer, tokens: tokens}, nil
}

// MustCompilePointer is like [CompilePointer] but panics with its error on
// invalid syntax.
func MustCompilePointer(pointer string) CompiledPointer {
	p, err := CompilePointer(pointer)
	if err != nil {
		panic(err)
	}
	return p
}

// String returns the original pointer spelling.
func (p CompiledPointer) String() string {
	return p.pointer
}

// Pointer returns the RFC 6901 JSON Pointer target within v. The result shares
// v's document lifetime. An absent target returns a zero Value, false, and nil;
// pointer or array-index errors return a zero Value and false with the error.
func (v Value) Pointer(pointer string) (Value, bool, error) {
	node, ok, err := v.node.Pointer(pointer)
	if err != nil || !ok {
		return Value{}, ok, err
	}
	return v.with(node), true, nil
}

// PointerCompiled is [Value.Pointer] with a precompiled pointer.
func (v Value) PointerCompiled(pointer CompiledPointer) (Value, bool, error) {
	node, ok, err := v.node.PointerCompiled(pointer)
	if err != nil || !ok {
		return Value{}, ok, err
	}
	return v.with(node), true, nil
}

// pointerError is the cold public-error boundary for the dependency-neutral
// pointer grammar. Navigation keeps compact reasons on its success path.
func pointerError(pointer string, reason jsonpointer.Reason) error {
	return &document.PointerError{Pointer: pointer, Message: reason.Message()}
}

func unescapePointerToken(s string) (string, error) {
	token, reason := jsonpointer.Unescape(s)
	if reason != jsonpointer.ReasonNone {
		return "", pointerError(s, reason)
	}
	return token, nil
}

func validatePointerSyntax(pointer string) error {
	reason := jsonpointer.Validate(pointer)
	if reason != jsonpointer.ReasonNone {
		return pointerError(pointer, reason)
	}
	return nil
}

func parsePointerIndex(s string) (int, bool, error) {
	index, kind, reason := jsonpointer.ClassifyIndex(s)
	switch kind {
	case jsonpointer.IndexNumber:
		return index, true, nil
	case jsonpointer.IndexDash:
		return 0, false, nil
	default:
		return 0, false, pointerError(s, reason)
	}
}

func compiledTokenArrayIndex(token jsonpointer.Token) (int, bool, error) {
	index, kind, reason := token.ArrayIndex()
	switch kind {
	case jsonpointer.IndexNumber:
		return index, true, nil
	case jsonpointer.IndexDash:
		return 0, false, nil
	default:
		return 0, false, pointerError(token.Text(), reason)
	}
}
