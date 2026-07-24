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

	vibejson "github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/store"
)

type (
	// Options fixes cache, transaction, snapshot, and I/O resource bounds.
	Options = vibejson.FileStoreOptions
	// Store is a checksummed copy-on-write durable JSON store.
	Store = vibejson.FileStore
	// Snapshot is an explicit immutable durable generation lease.
	Snapshot = vibejson.FileSnapshot
	// Stats reports bounded memory, cache, commit, and reclamation state.
	Stats = vibejson.FileStoreStats

	// Backend selects portable or Linux io_uring I/O.
	Backend = vibejson.FileStoreBackend
	// ReadMode selects buffered or direct cache-miss reads.
	ReadMode = vibejson.FileStoreReadMode
	// WriteMode selects buffered or direct durable writes.
	WriteMode = vibejson.FileStoreWriteMode

	// IndexWorkspace retains reusable zero-allocation probe storage.
	IndexWorkspace = vibejson.FileIndexWorkspace
	// IndexProbeStats reports the last indexed probe's work.
	IndexProbeStats = vibejson.FileIndexProbeStats
	// Float64Aggregate is one typed covering-column reduction.
	Float64Aggregate = vibejson.Float64Aggregate
	// IndexScalarGroup is one categorical index group.
	IndexScalarGroup = vibejson.FileIndexScalarGroup
)

const (
	BackendAuto     = vibejson.FileStoreBackendAuto
	BackendPortable = vibejson.FileStoreBackendPortable
	BackendIOUring  = vibejson.FileStoreBackendIOUring

	ReadBuffered      = vibejson.FileStoreReadBuffered
	ReadDirectTry     = vibejson.FileStoreReadDirectTry
	ReadDirectRequire = vibejson.FileStoreReadDirectRequire

	WriteBuffered      = vibejson.FileStoreWriteBuffered
	WriteDirectTry     = vibejson.FileStoreWriteDirectTry
	WriteDirectRequire = vibejson.FileStoreWriteDirectRequire
)

var (
	ErrClosed                = vibejson.ErrFileStoreClosed
	ErrNotEmpty              = vibejson.ErrFileStoreNotEmpty
	ErrKeyTooLarge           = vibejson.ErrFileStoreKeyTooLarge
	ErrDocumentTooLarge      = vibejson.ErrFileStoreDocumentTooLarge
	ErrDeadlineRange         = vibejson.ErrFileStoreDeadlineRange
	ErrWriterLocked          = vibejson.ErrFileStoreWriterLocked
	ErrWriterLockUnsupported = vibejson.ErrFileStoreWriterLockUnsupported
)

// Create initializes an empty file and fences its first root before returning.
func Create(file *os.File, options Options) (*Store, error) {
	return vibejson.CreateFileStore(file, options)
}

// Open recovers the newest complete generation and acquires its writer lease.
func Open(file *os.File, options Options) (*Store, error) {
	return vibejson.OpenFileStore(file, options)
}

// CreateFrom writes a completed in-memory Store as one durable generation.
// Keys, documents, TTL, schema, and compatible exact indexes are preserved
// without replaying individual mutations.
func CreateFrom(source *store.Store, file *os.File, options Options) (int64, error) {
	return source.WriteFileStore(file, options)
}
