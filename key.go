package slopjson

// Compiled queries move per-call query work to compile time. A lookup on a
// key-hash-enriched index hashes its query to use the hash gate; when one
// query runs against many documents that hash is loop-invariant, so
// CompileKey (here) and CompilePointer (pointer.go) compute it once and the
// *Compiled lookup spellings reuse it everywhere.

// CompiledKey is an object member key with its lookup hash precomputed.
//
// Compile once and reuse it on hot lookup paths that resolve the same key
// repeatedly, typically across many documents indexed with per-key hashes
// (document.IndexOptions.HashKeys): [Node.GetCompiled] and
// [FieldCursor.FindCompiled] then skip rehashing the query on every call. On
// an unenriched object a compiled key behaves exactly like its plain
// spelling. A CompiledKey is immutable and safe to share across goroutines.
// The zero CompiledKey is the empty key.
type CompiledKey struct {
	key  string
	hash uint32
}

// CompileKey returns key with its lookup hash precomputed. Every string is a
// valid key, so compilation cannot fail.
func CompileKey(key string) CompiledKey {
	return CompiledKey{key: key, hash: hashKeyString(key)}
}

// String returns the key spelling.
func (k CompiledKey) String() string {
	return k.key
}
