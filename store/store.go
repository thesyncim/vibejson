// Package store is the canonical keyed JSON store API.
//
// It contains the in-memory engine, immutable snapshots, optional schema and
// exact-index definitions, TTL, and bulk construction. Durable page-file
// storage lives in package store/durable.
package store

type (
	// Options fixes an in-memory Store's chunk and indexing representation.
	Options = StoreOptions
	// Builder bulk-loads a Store without per-row publication.
	Builder = StoreBuilder

	// Row is a Snapshot-scoped stable row address.
	Row = StoreRow
	// Mask is one chunk's stable-slot bitmap.
	Mask = StoreMask
	// Stats reports current in-memory Store resource use.
	Stats = StoreStats

	// IndexDefinition declares an exact scalar or compound index.
	IndexDefinition = StoreIndexDefinition
	// IndexInfo is immutable index catalog state.
	IndexInfo = StoreIndexInfo
	// IndexStats reports the physical footprint of one index.
	IndexStats = StoreIndexStats
	// IndexKind identifies an index family.
	IndexKind = StoreIndexKind
	// IndexState identifies online index build state.
	IndexState = StoreIndexState

	// Schema is an immutable compiled JSON constraint set.
	Schema = StoreSchema
	// SchemaDefinition is the declarative input to CompileSchema.
	SchemaDefinition = StoreSchemaDefinition
	// SchemaField constrains one nested RFC 6901 path.
	SchemaField = StoreSchemaField
)

const (
	MaxIndexColumns = StoreIndexMaxColumns

	IndexPostings = StoreIndexPostings
	IndexExact    = StoreIndexExact
	IndexBuilding = StoreIndexBuilding
	IndexReady    = StoreIndexReady
)

var (
	ErrTooLarge           = ErrStoreTooLarge
	ErrDuplicateKey       = ErrStoreDuplicateKey
	ErrBuilderClosed      = ErrStoreBuilderClosed
	ErrIndexExists        = ErrStoreIndexExists
	ErrIndexNotFound      = ErrStoreIndexNotFound
	ErrIndexKind          = ErrStoreIndexKind
	ErrIndexDefinition    = ErrStoreIndexDefinition
	ErrIndexArity         = ErrStoreIndexArity
	ErrIndexScalar        = ErrStoreIndexScalar
	ErrMaskOrder          = ErrStoreMaskOrder
	ErrMaskChunk          = ErrStoreMaskChunk
	ErrSchemaDefinition   = ErrStoreSchemaDefinition
	ErrSchemaViolation    = ErrStoreSchemaViolation
	ErrCollectionName     = ErrStoreCollectionName
	ErrCollectionExists   = ErrStoreCollectionExists
	ErrCheckpointMagic    = ErrStorePersistMagic
	ErrCheckpointVersion  = ErrStorePersistVersion
	ErrCheckpointCorrupt  = ErrStorePersistCorrupt
	ErrCheckpointTooLarge = ErrStorePersistTooLarge
)

// New validates options and returns an initialized in-memory Store. Unlike the
// legacy zero-value constructor, configuration errors are reported before any
// mutation can observe the Store.
func New(options Options) (*Store, error) {
	collection, err := NewCollection("_", options)
	if err != nil {
		return nil, err
	}
	return collection.Store, nil
}
