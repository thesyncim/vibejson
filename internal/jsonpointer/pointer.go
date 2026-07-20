// Package jsonpointer contains the dependency-neutral RFC 6901 grammar and
// compiled-token representation shared by document navigation implementations.
// It deliberately reports compact reasons as values: the public package that
// owns an operation constructs its own documented error type on the cold path.
package jsonpointer

import (
	"strconv"

	"github.com/thesyncim/simdjson/internal/byteview"
)

// Reason identifies one pointer grammar or array-index failure.
type Reason uint8

const (
	ReasonNone Reason = iota
	ReasonMustStartWithSlash
	ReasonDanglingTilde
	ReasonUnknownTilde
	ReasonEmptyArrayIndex
	ReasonArrayIndexLeadingZero
	ReasonArrayIndexNotNumeric
	ReasonArrayIndexOverflow
)

// Message returns the public diagnostic text for r.
func (r Reason) Message() string {
	switch r {
	case ReasonMustStartWithSlash:
		return "pointer must be empty or start with slash"
	case ReasonDanglingTilde:
		return "dangling tilde escape"
	case ReasonUnknownTilde:
		return "unknown tilde escape"
	case ReasonEmptyArrayIndex:
		return "empty array index"
	case ReasonArrayIndexLeadingZero:
		return "array index has leading zero"
	case ReasonArrayIndexNotNumeric:
		return "array index is not numeric"
	case ReasonArrayIndexOverflow:
		return "array index overflows int"
	default:
		return ""
	}
}

// IndexKind identifies how a decoded token behaves in an array.
type IndexKind uint8

const (
	IndexInvalid IndexKind = iota
	IndexNumber
	IndexDash
)

// Token is one decoded pointer token together with its preclassified array
// index. Its representation is private to this internal package; callers use
// the small accessors below, which are intended to inline into lookup loops.
type Token struct {
	text        string
	index       int
	indexKind   IndexKind
	indexReason Reason
}

// Text returns the decoded object-member spelling of t.
func (t Token) Text() string { return t.text }

// ArrayIndex returns t's preclassified array-index parts. A numeric token has
// kind IndexNumber. The RFC 6902 append token "-" has kind IndexDash and
// ReasonNone because it names no existing JSON Pointer element. Other invalid
// tokens carry the reason a public boundary should report.
func (t Token) ArrayIndex() (index int, kind IndexKind, reason Reason) {
	return t.index, t.indexKind, t.indexReason
}

// Compile validates pointer, decodes its tokens, and preclassifies their array
// interpretations. A nonzero reason means compilation failed; tokens is nil in
// that case. The caller retains pointer separately for public diagnostics.
func Compile(pointer string) (tokens []Token, reason Reason) {
	if pointer == "" {
		return nil, ReasonNone
	}
	if pointer[0] != '/' {
		return nil, ReasonMustStartWithSlash
	}

	for start := 1; ; {
		end := start
		for end < len(pointer) && pointer[end] != '/' {
			if pointer[end] == '~' {
				if end+1 >= len(pointer) {
					return nil, ReasonDanglingTilde
				}
				if pointer[end+1] != '0' && pointer[end+1] != '1' {
					return nil, ReasonUnknownTilde
				}
				end += 2
				continue
			}
			end++
		}

		token, reason := Unescape(pointer[start:end])
		if reason != ReasonNone {
			return nil, reason
		}
		index, indexKind, indexReason := ClassifyIndex(token)
		tokens = append(tokens, Token{
			text:        token,
			index:       index,
			indexKind:   indexKind,
			indexReason: indexReason,
		})
		if end == len(pointer) {
			return tokens, ReasonNone
		}
		start = end + 1
	}
}

// Validate reports the RFC 6901 syntax error message for pointer, or "" when
// it is valid. It does not allocate.
func Validate(pointer string) Reason {
	if pointer == "" {
		return ReasonNone
	}
	if pointer[0] != '/' {
		return ReasonMustStartWithSlash
	}
	for i := 1; i < len(pointer); i++ {
		if pointer[i] != '~' {
			continue
		}
		if i+1 >= len(pointer) {
			return ReasonDanglingTilde
		}
		if pointer[i+1] != '0' && pointer[i+1] != '1' {
			return ReasonUnknownTilde
		}
		i++
	}
	return ReasonNone
}

// NextToken returns the end of the token starting at start and the next token
// start. A next value one past len(pointer) marks the final token.
func NextToken(pointer string, start int) (end, next int) {
	end = start
	for end < len(pointer) && pointer[end] != '/' {
		end++
	}
	if end == len(pointer) {
		return end, len(pointer) + 1
	}
	return end, end + 1
}

// Unescape decodes one RFC 6901 token. A token without a tilde aliases s and
// allocates nothing; an escaped token owns its decoded storage.
func Unescape(s string) (token string, reason Reason) {
	for i := 0; i < len(s); i++ {
		if s[i] == '~' {
			return unescapeSlow(s, i)
		}
	}
	return s, ReasonNone
}

func unescapeSlow(s string, first int) (token string, reason Reason) {
	var out []byte
	out = append(out, s[:first]...)
	for i := first; i < len(s); i++ {
		if s[i] != '~' {
			out = append(out, s[i])
			continue
		}
		if i+1 >= len(s) {
			return "", ReasonDanglingTilde
		}
		switch s[i+1] {
		case '0':
			out = append(out, '~')
		case '1':
			out = append(out, '/')
		default:
			return "", ReasonUnknownTilde
		}
		i++
	}
	return byteview.String(out), ReasonNone
}

// ClassifyIndex classifies one decoded array token.
func ClassifyIndex(s string) (index int, kind IndexKind, reason Reason) {
	if s == "-" {
		return 0, IndexDash, ReasonNone
	}
	if s == "" {
		return 0, IndexInvalid, ReasonEmptyArrayIndex
	}
	if len(s) > 1 && s[0] == '0' {
		return 0, IndexInvalid, ReasonArrayIndexLeadingZero
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return 0, IndexInvalid, ReasonArrayIndexNotNumeric
		}
	}
	index, err := strconv.Atoi(s)
	if err != nil {
		return 0, IndexInvalid, ReasonArrayIndexOverflow
	}
	return index, IndexNumber, ReasonNone
}
