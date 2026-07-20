package simdjson

import (
	"fmt"
	"strconv"
)

// SyntaxError describes a JSON syntax error with byte, line, and column
// positions.
type SyntaxError struct {
	// Offset is the byte offset of the syntax failure in the input.
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

// EncodeError reports a Go value that cannot be represented in JSON.
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
		return fmt.Sprintf("simdjson: cannot encode value at %s: %s", e.Path, e.Reason)
	}
	return "simdjson: cannot encode value: " + e.Reason
}

func prependEncodePathField(err error, name string) error {
	if e, ok := err.(*EncodeError); ok {
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
	if e, ok := err.(*EncodeError); ok {
		segment := "[" + strconv.Itoa(index) + "]"
		if e.Path == "" || e.Path[0] == '[' {
			e.Path = segment + e.Path
		} else {
			e.Path = segment + "." + e.Path
		}
	}
	return err
}
