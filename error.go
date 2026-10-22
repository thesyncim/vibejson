package vibejson

import (
	"fmt"
	"slices"
	"strconv"
)

// SyntaxError describes a JSON syntax error with byte, line, and column
// positions. It does not retain the decoder input.
type SyntaxError struct {
	// Offset is the zero-based byte offset of the syntax failure in the input.
	Offset int
	// Line is the one-based input line containing Offset.
	Line int
	// Column is the one-based byte column containing Offset.
	Column int
	// Message describes the violated JSON grammar rule.
	Message string
}

// Error formats the byte, line, column, and grammar failure.
func (e *SyntaxError) Error() string {
	return fmt.Sprintf("json syntax error at byte %d, line %d, column %d: %s", e.Offset, e.Line, e.Column, e.Message)
}

func syntaxError(src []byte, off int, msg string) *SyntaxError {
	if off < 0 {
		off = 0
	}
	if off > len(src) {
		off = len(src)
	}
	line, col := 1, 1
	for i := 0; i < off; i++ {
		if src[i] == '\n' {
			line++
			col = 1
			continue
		}
		col++
	}
	return &SyntaxError{Offset: off, Line: line, Column: col, Message: msg}
}

// EncodeError reports a Go value that cannot be represented in JSON. The
// encoder does not attach the source value to the error.
type EncodeError struct {
	// Path locates the offending value using JSON member names and array
	// indexes, for example "items[3].scores[1]". It is empty when the
	// top-level value itself failed. Building the path costs nothing until
	// an error actually unwinds.
	Path string

	// Reason describes why the value cannot be represented as JSON.
	Reason string
}

// Error formats the encode failure and its optional value path.
func (e *EncodeError) Error() string {
	if e.Path != "" {
		return fmt.Sprintf("vibejson: cannot encode value at %s: %s", e.Path, e.Reason)
	}
	return "vibejson: cannot encode value: " + e.Reason
}

// Long paths use an append-only reversed buffer while the error unwinds, so
// a deeply nested failure does not repeatedly copy the entire growing path.
// Keep short errors on the direct string path without an extra allocation.
const encodePathBufferThreshold = 256

type encodePathError struct {
	cause    *EncodeError
	reversed []byte
}

func (e *encodePathError) Error() string {
	// Diagnostics must not change the buffer's order: another caller may
	// still prepend context before the public encoder boundary finalizes it.
	path := slices.Clone(e.reversed)
	slices.Reverse(path)
	value := EncodeError{Path: string(path), Reason: e.cause.Reason}
	return value.Error()
}

func newEncodePathError(e *EncodeError) *encodePathError {
	path := []byte(e.Path)
	slices.Reverse(path)
	return &encodePathError{cause: e, reversed: path}
}

func (e *encodePathError) prepend(segment string) {
	if len(e.reversed) > 0 && e.reversed[len(e.reversed)-1] != '[' {
		e.reversed = append(e.reversed, '.')
	}
	for i := len(segment) - 1; i >= 0; i-- {
		e.reversed = append(e.reversed, segment[i])
	}
}

func finishEncodeError(err error) error {
	if e, ok := err.(*encodePathError); ok {
		slices.Reverse(e.reversed)
		e.cause.Path = string(e.reversed)
		return e.cause
	}
	return err
}

func prependEncodePathField(err error, name string) error {
	if e, ok := err.(*encodePathError); ok {
		e.prepend(name)
		return e
	}
	if e, ok := err.(*EncodeError); ok {
		if len(e.Path) >= encodePathBufferThreshold {
			path := newEncodePathError(e)
			path.prepend(name)
			return path
		}
		switch {
		case e.Path == "":
			e.Path = name
		case e.Path[0] == '[':
			e.Path = name + e.Path
		default:
			e.Path = name + "." + e.Path
		}
	}
	return err
}

func prependEncodePathIndex(err error, index int) error {
	if e, ok := err.(*encodePathError); ok {
		var digits [32]byte
		segment := strconv.AppendInt(digits[:1], int64(index), 10)
		segment[0] = '['
		segment = append(segment, ']')
		e.prepend(string(segment))
		return e
	}
	if e, ok := err.(*EncodeError); ok {
		segment := "[" + strconv.Itoa(index) + "]"
		if len(e.Path) >= encodePathBufferThreshold {
			path := newEncodePathError(e)
			path.prepend(segment)
			return path
		}
		if e.Path == "" || e.Path[0] == '[' {
			e.Path = segment + e.Path
		} else {
			e.Path = segment + "." + e.Path
		}
	}
	return err
}
