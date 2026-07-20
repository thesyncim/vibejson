package document

import "errors"

// ErrIndexFull means the caller-provided entry buffer has insufficient
// capacity. It owns no input or storage and is safe to compare concurrently.
var ErrIndexFull = errors.New("simdjson: index entry buffer is full")

// ErrIndexTooLarge means the source or entry count exceeds the index's 32-bit
// address space. It owns no input or storage and is safe to compare
// concurrently.
var ErrIndexTooLarge = errors.New("simdjson: indexed input exceeds 32-bit offsets")

// IndexOptions controls zero-copy structural indexing. It owns no storage and
// is safe to copy or use concurrently.
type IndexOptions struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int
}
