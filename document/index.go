package document

// IndexOptions controls zero-copy structural indexing. It owns no storage and
// is safe to copy or use concurrently.
type IndexOptions struct {
	// MaxDepth limits nested arrays and objects. Values <= 0 use the default.
	MaxDepth int
}
