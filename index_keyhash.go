package vibejson

import "github.com/thesyncim/vibejson/x/keyhash"

// Key-hash enrichment: precomputed lookup hashes in the tape's free words.
// Object headers mark enriched tapes before readers interpret key Next words.
// Escaped keys retain raw-spelling hashes and always require decoded byte
// comparison; hash matches and collisions are prefilters only.
//
// The hash function and enrichment pass live in x/keyhash so sibling engines
// such as vibedb share the exact same constants and tape-enrichment contract.

func hashKeyContent(content []byte) uint32 {
	return keyhash.Bytes(content)
}

func hashKeyContentWord(word uint64, n int) uint32 {
	return keyhash.Word(word, n)
}

func hashKey(key string) uint32 {
	return keyhash.String(key)
}

func enrichKeyHashes(index *Index) {
	keyhash.EnrichIndex(index.Src, index.Entries)
}
