package simdjson

import "github.com/thesyncim/simdjson/document"

// This file is the exported build surface of the structural index. The tape
// layout and the engines live in index.go; options and errors are owned by
// the document package so other builders can share them.

// BuildIndex validates src and builds a navigable index in caller-owned
// storage. The returned Index aliases both src and storage. It performs no
// heap allocations for valid input when storage is sufficient. Insufficient
// storage returns document.ErrIndexFull; inputs outside the 32-bit index
// representation return document.ErrIndexTooLarge.
func BuildIndex(src []byte, storage []IndexEntry) (Index, error) {
	return buildIndexOptions(src, storage, document.IndexOptions{})
}

// BuildIndexOptions is BuildIndex with depth control. The document package
// owns structural-index options and errors.
func BuildIndexOptions(src []byte, storage []IndexEntry, opts document.IndexOptions) (Index, error) {
	return buildIndexOptions(src, storage, opts)
}
