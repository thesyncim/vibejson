// Package store is the canonical keyed JSON store API.
//
// It contains the in-memory engine, immutable snapshots, optional schema and
// exact-index definitions, TTL, and bulk construction. Durable page-file
// storage lives in package store/durable.
package store

// New validates options and returns an initialized in-memory collection. It freezes
// options immediately rather than deferring configuration errors or option
// capture to the first mutation, so no mutation can observe a misconfigured
// collection.
//
// The returned collection is standalone and unnamed; only [Database.CreateCollection]
// gives a collection a catalog name.
func New(options Options) (*Collection, error) {
	normalized, err := options.Normalized()
	if err != nil {
		return nil, err
	}
	collection := &Collection{Options: normalized}
	if _, err := collection.initLocked(); err != nil {
		return nil, err
	}
	return collection, nil
}
