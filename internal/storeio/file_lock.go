package storeio

import "errors"

var (
	// ErrWriterLocked reports that another mutable page-file owner already
	// holds the process/filesystem advisory writer lease.
	ErrWriterLocked = errors.New("simdjson: Store page file already has a writer")
	// ErrWriterLockUnsupported rejects mutable open on a platform where this
	// package cannot enforce the single-writer invariant.
	ErrWriterLockUnsupported = errors.New("simdjson: Store page writer locking is unsupported")
)
