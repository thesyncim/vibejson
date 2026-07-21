package simdjson

import (
	"strconv"

	"github.com/thesyncim/simdjson/document"
)

// CompiledPointer is a parsed RFC 6901 JSON Pointer.
//
// Compile once and reuse it on hot lookup paths to avoid reparsing and
// unescaping pointer tokens on every call. A CompiledPointer is immutable and
// safe to share across goroutines. The zero CompiledPointer represents the
// empty pointer, which selects the value on which it is evaluated.
type CompiledPointer struct {
	pointer string
	tokens  []compiledPointerToken
}

// A compiledPointerToken is one reference token of a compiled pointer, fully
// resolved at compile time: the decoded text, its lookup hash for object
// steps, and its array-index classification for array steps. A token cannot
// know which it will be applied to — /a/0 may step an object member named
// "0" — so both interpretations are precomputed.
type compiledPointerToken struct {
	text string
	// hash is the key-lookup hash of the decoded token text, precomputed so an
	// object step on an enriched index (see enrichKeyHashes) skips rehashing
	// the query at every document the pointer is applied to. Array-shaped
	// tokens are hashed too: an object member may spell a numeric key.
	hash         uint32
	index        int
	indexKind    pointerIndexKind
	indexMessage string
}

// pointerIndexKind classifies a token's reading as an array index: a valid
// number, the RFC 6901 "-" (past-the-end, always absent here), or invalid.
type pointerIndexKind uint8

const (
	pointerIndexInvalid pointerIndexKind = iota
	pointerIndexNumber
	pointerIndexDash
)

// CompilePointer parses pointer as an RFC 6901 JSON Pointer. Invalid syntax
// returns a zero CompiledPointer and a [document.PointerError].
func CompilePointer(pointer string) (CompiledPointer, error) {
	if pointer == "" {
		return CompiledPointer{}, nil
	}
	if pointer[0] != '/' {
		return CompiledPointer{}, &document.PointerError{Pointer: pointer, Message: "pointer must be empty or start with slash"}
	}

	var tokens []compiledPointerToken
	for start := 1; ; {
		end := start
		for end < len(pointer) && pointer[end] != '/' {
			if pointer[end] == '~' {
				if end+1 >= len(pointer) {
					return CompiledPointer{}, &document.PointerError{Pointer: pointer, Message: "dangling tilde escape"}
				}
				if pointer[end+1] != '0' && pointer[end+1] != '1' {
					return CompiledPointer{}, &document.PointerError{Pointer: pointer, Message: "unknown tilde escape"}
				}
				end += 2
				continue
			}
			end++
		}

		token, err := unescapePointerToken(pointer[start:end])
		if err != nil {
			return CompiledPointer{}, err
		}
		index, kind, msg := classifyPointerIndex(token)
		tokens = append(tokens, compiledPointerToken{
			text:         token,
			hash:         hashKeyString(token),
			index:        index,
			indexKind:    kind,
			indexMessage: msg,
		})
		if end == len(pointer) {
			return CompiledPointer{pointer: pointer, tokens: tokens}, nil
		}
		start = end + 1
	}
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

// unescapePointerToken decodes a token's ~0 and ~1 escapes, returning the
// input unchanged (and allocation-free) when it contains none.
func unescapePointerToken(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			return unescapePointerTokenSlow(s, i)
		}
	}
	return s, nil
}

// unescapePointerTokenSlow materializes the decoded token once a tilde was
// found at s[first].
func unescapePointerTokenSlow(s string, first int) (string, error) {
	var out []byte
	out = append(out, s[:first]...)
	for i := first; i < len(s); i++ {
		if s[i] != '~' {
			out = append(out, s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", &document.PointerError{Pointer: s, Message: "dangling tilde escape"}
		}
		switch s[i+1] {
		case '0':
			out = append(out, '~')
		case '1':
			out = append(out, '/')
		default:
			return "", &document.PointerError{Pointer: s, Message: "unknown tilde escape"}
		}
		i++
	}
	return ownedBytesString(out), nil
}

// parsePointerIndex is the uncompiled spelling of arrayIndex: it classifies
// and reports a token's array reading in one call for Node.Pointer.
func parsePointerIndex(s string) (int, bool, error) {
	idx, kind, msg := classifyPointerIndex(s)
	switch kind {
	case pointerIndexNumber:
		return idx, true, nil
	case pointerIndexDash:
		return 0, false, nil
	default:
		return 0, false, &document.PointerError{Pointer: s, Message: msg}
	}
}

// classifyPointerIndex applies RFC 6901's array-index grammar — "-", or
// digits with no leading zero — and carries the rejection message for the
// error path.
func classifyPointerIndex(s string) (int, pointerIndexKind, string) {
	if s == "-" {
		return 0, pointerIndexDash, ""
	}
	if s == "" {
		return 0, pointerIndexInvalid, "empty array index"
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, pointerIndexInvalid, "array index has leading zero"
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, pointerIndexInvalid, "array index is not numeric"
		}
	}
	idx, err := strconv.Atoi(s)
	if err != nil {
		return 0, pointerIndexInvalid, "array index overflows int"
	}
	return idx, pointerIndexNumber, ""
}

// arrayIndex returns the token's array reading, precomputed at compile time:
// the index and true for a number, false without error for "-" (always
// absent), and the compile-time diagnosis for anything else.
func (t compiledPointerToken) arrayIndex() (int, bool, error) {
	switch t.indexKind {
	case pointerIndexNumber:
		return t.index, true, nil
	case pointerIndexDash:
		return 0, false, nil
	default:
		msg := t.indexMessage
		if msg == "" {
			msg = "array index is not numeric"
		}
		return 0, false, &document.PointerError{Pointer: t.text, Message: msg}
	}
}
