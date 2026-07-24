// Package durable provides the bounded-residency, automatically persisted
// Store.
//
// A Store owns a single mutable generation stream but not the caller's
// *os.File lifetime. Create and Open acquire an exclusive writer lease before
// inspecting mutable state. Readers use explicit immutable Snapshot leases;
// Close releases both I/O resources and the writer lease.
package durable

import (
	"os"

	"github.com/thesyncim/vibejson/store"
)

type (
	// Options fixes cache, transaction, snapshot, and I/O resource bounds.
	Options = FileStoreOptions
	// Store is a checksummed copy-on-write durable JSON store.
	Store = FileStore
	// Snapshot is an explicit immutable durable generation lease.
	Snapshot = FileSnapshot
	// Stats reports bounded memory, cache, commit, and reclamation state.
	Stats = FileStoreStats

	// Backend selects portable or Linux io_uring I/O.
	Backend = FileStoreBackend
	// ReadMode selects buffered or direct cache-miss reads.
	ReadMode = FileStoreReadMode
	// WriteMode selects buffered or direct durable writes.
	WriteMode = FileStoreWriteMode

	// IndexWorkspace retains reusable zero-allocation probe storage.
	IndexWorkspace = FileIndexWorkspace
	// IndexProbeStats reports the last indexed probe's work.
	IndexProbeStats = FileIndexProbeStats
	// IndexScalarGroup is one categorical index group.
	IndexScalarGroup = FileIndexScalarGroup
)

const (
	BackendAuto     = FileStoreBackendAuto
	BackendPortable = FileStoreBackendPortable
	BackendIOUring  = FileStoreBackendIOUring

	ReadBuffered      = FileStoreReadBuffered
	ReadDirectTry     = FileStoreReadDirectTry
	ReadDirectRequire = FileStoreReadDirectRequire

	WriteBuffered      = FileStoreWriteBuffered
	WriteDirectTry     = FileStoreWriteDirectTry
	WriteDirectRequire = FileStoreWriteDirectRequire
)

var (
	ErrClosed                = ErrFileStoreClosed
	ErrNotEmpty              = ErrFileStoreNotEmpty
	ErrKeyTooLarge           = ErrFileStoreKeyTooLarge
	ErrDocumentTooLarge      = ErrFileStoreDocumentTooLarge
	ErrDeadlineRange         = ErrFileStoreDeadlineRange
	ErrWriterLocked          = ErrFileStoreWriterLocked
	ErrWriterLockUnsupported = ErrFileStoreWriterLockUnsupported
)

// Create initializes an empty file and fences its first root before returning.
func Create(file *os.File, options Options) (*FileStore, error) {
	return CreateFileStore(file, options)
}

// Open recovers the newest complete generation and acquires its writer lease.
func Open(file *os.File, options Options) (*FileStore, error) {
	return OpenFileStore(file, options)
}

// CreateFrom writes a completed in-memory Store as one durable generation.
// Keys, documents, TTL, schema, and compatible exact indexes are preserved
// without replaying individual mutations.
func CreateFrom(source *store.Store, file *os.File, options Options) (int64, error) {
	return WriteFileStore(source, file, options)
}
