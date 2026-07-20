package simdjson

import (
	"encoding/binary"
	"unsafe"

	"github.com/thesyncim/simdjson/document"
)

// Precomputed key hashes accelerate object field lookups. A key entry's next
// word is never followed by navigation — subtree skipping traces only value
// and container spans — so the optional enrichment pass repurposes it to hold
// a 32-bit hash of the key's content bytes. Node.Get and FieldCursor.Find then
// reject nearly every non-matching member on one word compare before the byte
// comparison, which still runs on a hash match so collisions never mislead.
//
// Enrichment is opt-in (document.IndexOptions.HashKeys): the default build
// paths write next = 1 for keys exactly as before, so an unenriched index is
// byte-identical and equally fast. enrichKeyHashes marks each Object header
// with tapeFlagObjectKeysHashed; the reader consults per-key hashes only under
// that marker and only for unescaped keys, since an escaped key's stored hash
// covers its raw spelling, which a decoded query cannot be compared against.

// keyHashSeed and keyHashMul are the FxHash-style mixing constants: an odd
// golden-ratio seed and an odd avalanching multiplier.
const (
	keyHashSeed = 0x9E3779B97F4A7C15
	keyHashMul  = 0xFF51AFD7ED558CCD
)

// keyHashInit spreads the length across the whole state word before any
// content folds in; a plain XOR would share a lane with short-tail bytes and
// let pairs like "a"/"ba" cancel to systematic collisions.
func keyHashInit(n int) uint64 {
	return keyHashSeed ^ uint64(n)*keyHashMul
}

// keyHashMix folds one gathered content word into the state.
func keyHashMix(h, w uint64) uint64 {
	return (h ^ w) * keyHashMul
}

// keyHashFinish avalanches the state and returns its best-mixed high word.
func keyHashFinish(h uint64) uint32 {
	h ^= h >> 29
	h *= keyHashSeed
	return uint32(h >> 32)
}

// hashKeyContent hashes a key's content bytes — those strictly between its
// quotes, escapes included — the value enrichment stores in the entry's next
// word.
//
// Content shorter than a word is mixed as the zero-padded little-endian word
// holding exactly its n bytes. The sub-word gathers below rebuild that word
// from in-bounds loads whose overlapping regions repeat identical bytes, so
// their OR is exact — and hashKeyContentWord produces it with one mask, which
// is what keeps the register variant inlineable at the enrichment call.
//
// Unsafe contract: content names len(content) live, readable bytes. No byte
// outside the slice is read.
func hashKeyContent(content []byte) uint32 {
	base := unsafe.Pointer(unsafe.SliceData(content))
	n := len(content)
	h := keyHashInit(n)
	switch {
	case n >= 8:
		for i := 8; i < n; i += 8 {
			h = keyHashMix(h, binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, i-8))[:]))
		}
		// The final chunk re-reads up to seven bytes of its predecessor,
		// keeping every load inside the content with no tail switch.
		h = keyHashMix(h, binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, n-8))[:]))
	case n >= 4:
		lo := uint64(binary.LittleEndian.Uint32((*[4]byte)(base)[:]))
		hi := uint64(binary.LittleEndian.Uint32((*[4]byte)(unsafe.Add(base, n-4))[:]))
		h = keyHashMix(h, lo|hi<<(8*(uint(n)-4)))
	default:
		var w uint64
		if n > 0 {
			w = uint64(*(*byte)(base)) |
				uint64(*(*byte)(unsafe.Add(base, n>>1)))<<(8*(uint(n)>>1)) |
				uint64(*(*byte)(unsafe.Add(base, n-1)))<<(8*(uint(n)-1))
		}
		h = keyHashMix(h, w)
	}
	return keyHashFinish(h)
}

// hashKeyContentWord is hashKeyContent for content already sitting in a
// register: the first n bytes of the little-endian word, 0 <= n <= 8. Masking
// to those bytes yields exactly the zero-padded word hashKeyContent mixes, so
// the two return identical hashes for identical content and bytes of word at
// index n and beyond never influence the result. Its straight-line body
// inlines into the enrichment loop, hashing a short key without a second load.
func hashKeyContentWord(word uint64, n int) uint32 {
	word &= ^uint64(0) >> (8 * (8 - uint(n)))
	return keyHashFinish(keyHashMix(keyHashInit(n), word))
}

// hashKeyString hashes a query key over the same byte sequence, so a stored
// hash and a query hash agree exactly when their bytes agree.
func hashKeyString(key string) uint32 {
	return hashKeyContent(unsafe.Slice(unsafe.StringData(key), len(key)))
}

// enrichKeyHashes runs one allocation-free linear pass over the tape, marking
// each Object header as key-hashed and writing every key entry's content hash
// into its next word. Value strings and every other entry are untouched, so
// the pass is safe to run once on a freshly built tape. It is called from
// buildIndexOptions when HashKeys is set, whichever builder produced the tape.
func enrichKeyHashes(index *Index) {
	src := index.src
	base := unsafe.Pointer(unsafe.SliceData(src))
	n := len(src)
	entries := index.entries
	for i := range entries {
		e := &entries[i]
		if e.flags()&tapeFlagKey != 0 {
			// content is src[start+1 : end-1]; the length excludes both quotes.
			length := int(e.end-e.start) - 2
			if length <= 8 && int(e.start)+9 <= n {
				// The eight bytes after the opening quote are in bounds, so a
				// short key hashes from one register load without reslicing.
				word := binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, uintptr(e.start)+1))[:])
				e.next = hashKeyContentWord(word, length)
			} else {
				e.next = hashKeyContent(src[e.start+1 : e.end-1])
			}
			continue
		}
		if e.Kind() == document.Object {
			e.info |= uint32(tapeFlagObjectKeysHashed) << infoFlagsShift
		}
	}
}
