// Package store is the canonical keyed JSON store API.
//
// It contains the in-memory engine, immutable snapshots, optional schema and
// exact-index definitions, TTL, and bulk construction. Durable page-file
// storage lives in package store/durable. The aliases in this extraction
// boundary are compile-time identities: they add no wrapper calls, interface
// dispatch, allocations, or representation changes while implementation files
// move out of the JSON root package.
package store

import vibejson "github.com/thesyncim/vibejson"

type (
	// Options fixes an in-memory Store's chunk and indexing representation.
	Options = vibejson.StoreOptions
	// Store is a concurrent keyed collection with immutable snapshots.
	Store = vibejson.Store
	// Snapshot is one immutable Store generation.
	Snapshot = vibejson.Snapshot
	// Builder bulk-loads a Store without per-row publication.
	Builder = vibejson.StoreBuilder

	// Row is a Snapshot-scoped stable row address.
	Row = vibejson.StoreRow
	// Mask is one chunk's stable-slot bitmap.
	Mask = vibejson.StoreMask
	// Stats reports current in-memory Store resource use.
	Stats = vibejson.StoreStats

	// IndexDefinition declares an exact scalar or compound index.
	IndexDefinition = vibejson.StoreIndexDefinition
	// IndexInfo is immutable index catalog state.
	IndexInfo = vibejson.StoreIndexInfo
	// IndexStats reports the physical footprint of one index.
	IndexStats = vibejson.StoreIndexStats
	// IndexKind identifies an index family.
	IndexKind = vibejson.StoreIndexKind
	// IndexState identifies online index build state.
	IndexState = vibejson.StoreIndexState

	// Schema is an immutable compiled JSON constraint set.
	Schema = vibejson.StoreSchema
	// SchemaDefinition is the declarative input to CompileSchema.
	SchemaDefinition = vibejson.StoreSchemaDefinition
	// SchemaField constrains one nested RFC 6901 path.
	SchemaField = vibejson.StoreSchemaField
	// SchemaType is a set of accepted JSON kinds.
	SchemaType = vibejson.SchemaType
	// SchemaViolationError describes one rejected document.
	SchemaViolationError = vibejson.SchemaViolationError

	// Collection is one named independently configured Store.
	Collection = vibejson.Collection
	// CollectionInfo is detached collection catalog metadata.
	CollectionInfo = vibejson.CollectionInfo
	// Database is a concurrent catalog of named collections.
	Database = vibejson.Database
)

const (
	MaxIndexColumns = vibejson.StoreIndexMaxColumns

	IndexPostings = vibejson.StoreIndexPostings
	IndexExact    = vibejson.StoreIndexExact
	IndexBuilding = vibejson.StoreIndexBuilding
	IndexReady    = vibejson.StoreIndexReady

	SchemaNull    = vibejson.SchemaNull
	SchemaBool    = vibejson.SchemaBool
	SchemaNumber  = vibejson.SchemaNumber
	SchemaInteger = vibejson.SchemaInteger
	SchemaString  = vibejson.SchemaString
	SchemaArray   = vibejson.SchemaArray
	SchemaObject  = vibejson.SchemaObject
	SchemaAny     = vibejson.SchemaAny
)

var (
	ErrTooLarge           = vibejson.ErrStoreTooLarge
	ErrDuplicateKey       = vibejson.ErrStoreDuplicateKey
	ErrBuilderClosed      = vibejson.ErrStoreBuilderClosed
	ErrIndexExists        = vibejson.ErrStoreIndexExists
	ErrIndexNotFound      = vibejson.ErrStoreIndexNotFound
	ErrIndexKind          = vibejson.ErrStoreIndexKind
	ErrIndexDefinition    = vibejson.ErrStoreIndexDefinition
	ErrIndexArity         = vibejson.ErrStoreIndexArity
	ErrIndexScalar        = vibejson.ErrStoreIndexScalar
	ErrMaskOrder          = vibejson.ErrStoreMaskOrder
	ErrMaskChunk          = vibejson.ErrStoreMaskChunk
	ErrSchemaDefinition   = vibejson.ErrStoreSchemaDefinition
	ErrSchemaViolation    = vibejson.ErrStoreSchemaViolation
	ErrCollectionName     = vibejson.ErrStoreCollectionName
	ErrCollectionExists   = vibejson.ErrStoreCollectionExists
	ErrCheckpointMagic    = vibejson.ErrStorePersistMagic
	ErrCheckpointVersion  = vibejson.ErrStorePersistVersion
	ErrCheckpointCorrupt  = vibejson.ErrStorePersistCorrupt
	ErrCheckpointTooLarge = vibejson.ErrStorePersistTooLarge
)

// New validates options and returns an initialized in-memory Store. Unlike the
// legacy zero-value constructor, configuration errors are reported before any
// mutation can observe the Store.
func New(options Options) (*Store, error) {
	collection, err := vibejson.NewCollection("_", options)
	if err != nil {
		return nil, err
	}
	return collection.Store, nil
}

// NewBuilder returns a validated bulk loader.
func NewBuilder(options Options) (*Builder, error) {
	return vibejson.NewStoreBuilder(options)
}

// Open validates and opens an immutable checkpoint as a mutable Store.
func Open(data []byte) (*Store, error) { return vibejson.OpenStore(data) }

// CompileSchema validates and canonicalizes a schema definition.
func CompileSchema(definition SchemaDefinition) (*Schema, error) {
	return vibejson.CompileStoreSchema(definition)
}

// NewCollection constructs one named collection.
func NewCollection(name string, options Options) (*Collection, error) {
	return vibejson.NewCollection(name, options)
}
