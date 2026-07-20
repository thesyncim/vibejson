package simdjson

import (
	"strconv"

	"github.com/thesyncim/simdjson/document"
)

// CompiledPointer is a parsed RFC 6901 JSON Pointer.
//
// Compile once and reuse it on hot lookup paths to avoid reparsing and
// unescaping pointer tokens on every call. A CompiledPointer is immutable and
// safe to share across goroutines.
type CompiledPointer struct {
	pointer string
	tokens  []compiledPointerToken
}

type compiledPointerToken struct {
	text         string
	index        int
	indexKind    pointerIndexKind
	indexMessage string
}

type pointerIndexKind uint8

const (
	pointerIndexInvalid pointerIndexKind = iota
	pointerIndexNumber
	pointerIndexDash
)

// CompilePointer parses pointer as an RFC 6901 JSON Pointer.
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

// MustCompilePointer is like CompilePointer but panics on invalid syntax.
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

// Pointer returns the RFC 6901 JSON Pointer target within v.
func (v Value) Pointer(pointer string) (Value, bool, error) {
	node, ok, err := v.node.Pointer(pointer)
	if err != nil || !ok {
		return Value{}, ok, err
	}
	return v.with(node), true, nil
}

// PointerCompiled returns the precompiled JSON Pointer target within v.
func (v Value) PointerCompiled(pointer CompiledPointer) (Value, bool, error) {
	node, ok, err := v.node.PointerCompiled(pointer)
	if err != nil || !ok {
		return Value{}, ok, err
	}
	return v.with(node), true, nil
}

func unescapePointerToken(s string) (string, error) {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			return unescapePointerTokenSlow(s, i)
		}
	}
	return s, nil
}

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
