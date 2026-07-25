// Package store is the canonical keyed JSON store API.
//
// It contains the in-memory engine, immutable snapshots, optional schema and
// exact-index definitions, TTL, and bulk construction. Durable page-file
// storage lives in package store/durable.
package store

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
