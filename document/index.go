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
	// HashKeys precomputes per-key hashes to accelerate object field lookups;
	// it adds a build pass, so enable it for lookup-heavy or repeated-access
	// workloads and leave it off when the index is scanned once.
	HashKeys bool
}
