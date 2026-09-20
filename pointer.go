package vibejson

import (
	"strconv"

	"github.com/thesyncim/vibejson/document"
)

// CompiledPointer is a parsed RFC 6901 JSON Pointer.
type CompiledPointer struct {
	pointer string
	Tokens  []CompiledPointerToken
}

// CompiledPointerToken is one precomputed pointer token.
type CompiledPointerToken struct {
	Text string
	// Hash is the precomputed object-key hash.
	Hash         uint32
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

// CompilePointer parses an RFC 6901 JSON Pointer.
func CompilePointer(pointer string) (CompiledPointer, error) {
	if pointer == "" {
		return CompiledPointer{}, nil
	}
	if pointer[0] != '/' {
		return CompiledPointer{}, &document.PointerError{Pointer: pointer, Message: "pointer must be empty or start with slash"}
	}

	var tokens []CompiledPointerToken
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
		tokens = append(tokens, CompiledPointerToken{
			Text:         token,
			Hash:         HashKey(token),
			index:        index,
			indexKind:    kind,
			indexMessage: msg,
		})
		if end == len(pointer) {
			return CompiledPointer{pointer: pointer, Tokens: tokens}, nil
		}
		start = end + 1
	}
}

// MustCompilePointer parses a pointer or panics.
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

// Pointer resolves an RFC 6901 JSON Pointer within v.
func (v Value) Pointer(pointer string) (Value, bool, error) {
	node, ok, err := v.node.Pointer(pointer)
	if err != nil || !ok {
		return Value{}, ok, err
	}
	return v.with(node), true, nil
}

// PointerCompiled resolves a precompiled pointer within v.
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
	return OwnedBytesString(out), nil
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

func (t CompiledPointerToken) arrayIndex() (int, bool, error) {
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
		return 0, false, &document.PointerError{Pointer: t.Text, Message: msg}
	}
}
