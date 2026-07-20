package simdjson

// BuildIndex validates src and builds a navigable index in caller-owned
// storage. The returned Index aliases both src and storage. It performs no
// heap allocations for valid input when storage is sufficient. Insufficient
// storage returns document.ErrIndexFull; inputs outside the 32-bit index
// representation return document.ErrIndexTooLarge.
func BuildIndex(src []byte, storage []IndexEntry) (Index, error) {
	return buildIndexOptions(src, storage, IndexOptions{})
}

// BuildIndexOptions is BuildIndex with depth control.
func BuildIndexOptions(src []byte, storage []IndexEntry, opts IndexOptions) (Index, error) {
	return buildIndexOptions(src, storage, opts)
}
