package document

import "strconv"

// PointerError describes an invalid JSON Pointer expression.
type PointerError struct {
	// Pointer is the invalid pointer spelling supplied by the caller.
	Pointer string
	// Message describes the violated RFC 6901 rule.
	Message string
}

// Error formats the invalid pointer and the violated rule.
func (e *PointerError) Error() string {
	return "invalid JSON pointer " + strconv.Quote(e.Pointer) + ": " + e.Message
}
