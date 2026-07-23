package simdjson

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"unicode/utf8"
)

var (
	// ErrStoreCollectionName reports an empty, invalid UTF-8, or NUL-bearing
	// collection name.
	ErrStoreCollectionName = errors.New(
		"simdjson: invalid Store collection name",
	)
	// ErrStoreCollectionExists reports duplicate collection creation.
	ErrStoreCollectionExists = errors.New(
		"simdjson: Store collection already exists",
	)
)

// Collection is an independently configured keyed JSON namespace: schema,
// indexes, TTL, snapshots, and writer serialization belong to its embedded
// Store. Holding a Collection handle keeps CRUD and query execution on the
// ordinary Store path; the database catalog is not consulted per operation.
//
// Collection is the JSON-native analogue of a table, without requiring every
// document to share a closed relational column set.
type Collection struct {
	*Store
	name string
}

// NewCollection constructs one named collection and freezes options
// immediately rather than deferring configuration errors or option capture to
// the first mutation.
func NewCollection(
	name string,
	options StoreOptions,
) (*Collection, error) {
	if !validStoreCollectionName(name) {
		return nil, ErrStoreCollectionName
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	store := NewStore(normalized)
	if _, err := store.initLocked(); err != nil {
		return nil, err
	}
	return &Collection{
		Store: store,
		name:  strings.Clone(name),
	}, nil
}

// Name returns the immutable collection name.
func (c *Collection) Name() string {
	if c == nil {
		return ""
	}
	return c.name
}

// CollectionInfo is a detached catalog summary. Its name is immutable owned
// text; it does not retain a Collection, Store, or snapshot graph.
type CollectionInfo struct {
	Name       string
	Documents  int
	Generation uint64
	Schema     bool
}

// Database is a concurrency-safe catalog of independent JSON collections.
// Its zero value is ready to use. DDL takes the catalog lock; holding a
// Collection handle removes that lock and name lookup from every data
// operation. Dropping a name does not invalidate handles or snapshots already
// acquired from that collection.
type Database struct {
	mu          sync.RWMutex
	collections map[string]*Collection
}

// CreateCollection atomically publishes a new empty collection. Its options
// are validated and frozen before catalog publication.
func (d *Database) CreateCollection(
	name string,
	options StoreOptions,
) (*Collection, error) {
	if d == nil || !validStoreCollectionName(name) {
		return nil, ErrStoreCollectionName
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	store := NewStore(normalized)
	if _, err := store.initLocked(); err != nil {
		return nil, err
	}
	ownedName := strings.Clone(name)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.collections == nil {
		d.collections = make(map[string]*Collection)
	}
	if _, exists := d.collections[name]; exists {
		return nil, ErrStoreCollectionExists
	}
	collection := &Collection{
		Store: store,
		name:  ownedName,
	}
	d.collections[ownedName] = collection
	return collection, nil
}

// Collection resolves name and reports whether it is currently cataloged.
func (d *Database) Collection(name string) (*Collection, bool) {
	if d == nil {
		return nil, false
	}
	d.mu.RLock()
	collection, ok := d.collections[name]
	d.mu.RUnlock()
	return collection, ok
}

// DropCollection removes name from the catalog. Existing handles and
// snapshots remain valid and continue to own their collection.
func (d *Database) DropCollection(name string) bool {
	if d == nil {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.collections[name]; !ok {
		return false
	}
	delete(d.collections, name)
	return true
}

// AppendCollections appends a name-ordered catalog snapshot to dst.
func (d *Database) AppendCollections(
	dst []CollectionInfo,
) []CollectionInfo {
	if d == nil {
		return dst
	}
	d.mu.RLock()
	start := len(dst)
	for _, collection := range d.collections {
		state := collection.state.Load()
		info := CollectionInfo{
			Name:   collection.name,
			Schema: collection.options.Schema != nil,
		}
		if state != nil {
			info.Documents = state.count
			info.Generation = state.generation
		}
		dst = append(dst, info)
	}
	d.mu.RUnlock()
	slices.SortFunc(
		dst[start:],
		func(a, b CollectionInfo) int {
			return strings.Compare(a.Name, b.Name)
		},
	)
	return dst
}

func validStoreCollectionName(name string) bool {
	return name != "" && utf8.ValidString(name) &&
		!strings.ContainsRune(name, 0)
}
