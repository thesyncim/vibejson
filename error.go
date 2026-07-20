package simdjson

import "fmt"

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
