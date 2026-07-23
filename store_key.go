package slopjson

import "hash/maphash"

// StoreKey is a reusable keyed-Store lookup compiled against one Store's hash
// seed and current stable slot. Compile it once with [Store.CompileKey] or
// [Snapshot.CompileKey], then use [Snapshot.GetRawKey] or [Snapshot.GetKey] on
// repeated reads.
//
// A StoreKey remains correct across updates and snapshots. When its key is
// still live at the cached stable slot, lookup bypasses both string hashing and
// the key directories. Delete/reinsert movement, an initially absent key, or
// use with another Store falls back to a complete seeded-hash and full-key
// lookup. The key spelling is borrowed like [CompiledKey]; keep that string
// immutable.
// The zero value names the empty key and is safe to use.
type StoreKey struct {
	key  string
	seed maphash.Seed
	hash uint64
	loc  storeLocation
	// generation proves that loc was resolved against the exact current
	// publication. It adds no GC edge and lets repeated reads skip a redundant
	// full-key comparison until any mutation publishes a newer state.
	generation uint64
	located    bool
}

// String returns the key spelling.
func (k StoreKey) String() string { return k.key }

// CompileKey returns a reusable lookup for key in this snapshot. Compilation
// performs one ordinary key lookup so later hits can take the verified stable-
// slot fast path. Compiling against an empty snapshot is valid; later use on a
// non-empty Store falls back to its ordinary lookup.
func (s Snapshot) CompileKey(key string) StoreKey {
	compiled := StoreKey{key: key}
	if s.state == nil {
		return compiled
	}
	compiled.seed = s.state.seed
	compiled.hash = maphash.String(compiled.seed, key)
	compiled.loc, compiled.located = storeStateKeyLookup(s.state, compiled.hash, key)
	compiled.generation = s.state.generation
	return compiled
}

// CompileKey is the current-snapshot convenience form of Snapshot.CompileKey.
func (s *Store) CompileKey(key string) StoreKey { return s.Snapshot().CompileKey(key) }

// storeKeyCompiledFallback resolves a compiled-key cache miss. Callers check
// the stable slot inline so a hit pays no extra helper call. Fingerprints and
// cached addresses are never authoritative without a complete-key check.
func storeKeyCompiledFallback(state *storeState, key StoreKey) (*storeChunk, storeLocation, bool) {
	if state == nil {
		return nil, storeLocation{}, false
	}
	hash := key.hash
	if key.seed != state.seed {
		hash = maphash.String(state.seed, key.key)
	}
	chunk, loc, ok := storeStateKeyLookupChunk(state, hash, key.key)
	if !ok {
		return nil, storeLocation{}, false
	}
	return chunk, loc, true
}

// GetRawKey returns key's exact JSON bytes through a compiled Store lookup.
// The returned value has the same borrowing and lifetime contract as GetRaw.
func (s Snapshot) GetRawKey(key StoreKey) (RawValue, bool) {
	state := s.state
	if state != nil && key.located && key.seed == state.seed {
		loc := key.loc
		chunk := state.chunks.get(loc.chunk)
		if chunk != nil && chunk.live&(uint64(1)<<loc.slot) != 0 &&
			(key.generation == state.generation || chunk.key(int(loc.slot)) == key.key) {
			return RawValue{src: chunk.docs.rawAt(int(chunk.ord[loc.slot]))}, true
		}
	}
	chunk, loc, ok := storeKeyCompiledFallback(state, key)
	if !ok {
		return RawValue{}, false
	}
	return RawValue{src: chunk.docs.rawAt(int(chunk.ord[loc.slot]))}, true
}

// GetKey returns key's navigable Index through a compiled Store lookup. Shape-
// tape widening has the same one-time allocation behavior as Get.
func (s Snapshot) GetKey(key StoreKey) (Index, bool) {
	state := s.state
	if state != nil && key.located && key.seed == state.seed {
		loc := key.loc
		chunk := state.chunks.get(loc.chunk)
		if chunk != nil && chunk.live&(uint64(1)<<loc.slot) != 0 &&
			(key.generation == state.generation || chunk.key(int(loc.slot)) == key.key) {
			return chunk.docs.Doc(int(chunk.ord[loc.slot])), true
		}
	}
	chunk, loc, ok := storeKeyCompiledFallback(state, key)
	if !ok {
		return Index{}, false
	}
	return chunk.docs.Doc(int(chunk.ord[loc.slot])), true
}

// GetRawKey is the current-snapshot convenience form of Snapshot.GetRawKey.
func (s *Store) GetRawKey(key StoreKey) (RawValue, bool) {
	return s.Snapshot().GetRawKey(key)
}

// GetKey is the current-snapshot convenience form of Snapshot.GetKey.
func (s *Store) GetKey(key StoreKey) (Index, bool) { return s.Snapshot().GetKey(key) }

// AppendRaw appends key's exact JSON spelling to caller-owned storage. It is
// the lifetime-independent counterpart to GetRaw: with sufficient capacity it
// allocates nothing, and the returned bytes remain valid after the Snapshot or
// a caller-owned mapped Store image is released. A miss leaves dst unchanged.
func (s Snapshot) AppendRaw(dst []byte, key string) ([]byte, bool) {
	raw, ok := s.GetRaw(key)
	if !ok {
		return dst, false
	}
	return append(dst, raw.Bytes()...), true
}

// AppendRawKey is AppendRaw through a reusable compiled Store key.
func (s Snapshot) AppendRawKey(dst []byte, key StoreKey) ([]byte, bool) {
	raw, ok := s.GetRawKey(key)
	if !ok {
		return dst, false
	}
	return append(dst, raw.Bytes()...), true
}

// AppendRaw is the current-snapshot convenience form of Snapshot.AppendRaw.
func (s *Store) AppendRaw(dst []byte, key string) ([]byte, bool) {
	return s.Snapshot().AppendRaw(dst, key)
}

// AppendRawKey is the current-snapshot convenience form of
// Snapshot.AppendRawKey.
func (s *Store) AppendRawKey(dst []byte, key StoreKey) ([]byte, bool) {
	return s.Snapshot().AppendRawKey(dst, key)
}
