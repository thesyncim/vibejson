// Package keyhash exposes vibejson's object-key hash function and tape
// key-hash enrichment pass for sibling engines operating directly on
// [document.IndexEntry] slices.
//
// Sharing this package guarantees that precomputed hashes written into
// [document.IndexEntry.Next] stay bit-identical to the hashes expected by
// vibejson's compiled key lookups and object navigation.
//
// This package is exported for sibling engines such as vibedb and is explicitly
// unstable. Applications should use [vibejson.BuildIndexOptions] with
// [document.IndexOptions.HashKeys] and [vibejson.CompileKey].
package keyhash

import (
	"encoding/binary"

	"github.com/thesyncim/vibejson/document"
	"github.com/thesyncim/vibejson/x/byteview"
)

// seed and mul are the FxHash-style mixing constants: an odd golden-ratio seed
// and an odd avalanching multiplier.
const (
	seed = 0x9E3779B97F4A7C15
	mul  = 0xFF51AFD7ED558CCD
)

// initState spreads the length across the whole state word before any content
// folds in; a plain XOR would share a lane with short-tail bytes and let pairs
// like "a"/"ba" cancel to systematic collisions.
func initState(n int) uint64 {
	return seed ^ uint64(n)*mul
}

// mix folds one gathered content word into the state.
func mix(h, w uint64) uint64 {
	return (h ^ w) * mul
}

// finish avalanches the state and returns its best-mixed high word.
func finish(h uint64) uint32 {
	h ^= h >> 29
	h *= seed
	return uint32(h >> 32)
}

// Bytes hashes a key's raw content bytes — those strictly between its quotes,
// escapes included — producing the value stored in [document.IndexEntry.Next]
// on key-hash-enriched tapes.
func Bytes(content []byte) uint32 {
	n := len(content)
	h := initState(n)
	switch {
	case n >= 8:
		for i := 8; i < n; i += 8 {
			h = mix(h, binary.LittleEndian.Uint64(content[i-8:i]))
		}
		// The final chunk re-reads up to seven bytes of its predecessor,
		// keeping every load inside the content with no tail switch.
		h = mix(h, binary.LittleEndian.Uint64(content[n-8:n]))
	case n >= 4:
		lo := uint64(binary.LittleEndian.Uint32(content[:4]))
		hi := uint64(binary.LittleEndian.Uint32(content[n-4:]))
		h = mix(h, lo|hi<<(8*(uint(n)-4)))
	default:
		var w uint64
		if n > 0 {
			w = uint64(content[0]) |
				uint64(content[n>>1])<<(8*(uint(n)>>1)) |
				uint64(content[n-1])<<(8*(uint(n)-1))
		}
		h = mix(h, w)
	}
	return finish(h)
}

// Word hashes the first n bytes (0 <= n <= 8) of a little-endian word already
// held in a register. It returns the exact same hash as [Bytes] over those n
// bytes, ignoring any bytes at index n and beyond.
func Word(word uint64, n int) uint32 {
	word &= ^uint64(0) >> (8 * (8 - uint(n)))
	return finish(mix(initState(n), word))
}

// String hashes key over the same byte sequence as [Bytes], so stored key
// hashes and query hashes agree exactly when their bytes agree.
func String(key string) uint32 {
	return Bytes(byteview.Bytes(key))
}

// EnrichIndex runs one allocation-free linear pass over a structural tape,
// marking each [document.Object] header with [document.TapeFlagObjectKeysHashed]
// and writing every [document.TapeFlagKey] entry's raw content hash into its
// Next word. Value strings and all other entries are untouched.
//
// Escaped keys retain raw-spelling hashes and always require decoded byte
// comparison; hash matches are prefilters only.
func EnrichIndex(src []byte, entries []document.IndexEntry) {
	n := len(src)
	for i := range entries {
		e := &entries[i]
		if e.Flags()&document.TapeFlagKey != 0 {
			// content is src[start+1 : end-1]; the length excludes both quotes.
			length := int(e.End-e.Start) - 2
			if length <= 8 && int(e.Start)+9 <= n {
				// The eight bytes after the opening quote are in bounds, so a
				// short key hashes from one register load without reslicing.
				start := int(e.Start) + 1
				word := binary.LittleEndian.Uint64(src[start : start+8])
				e.Next = Word(word, length)
			} else {
				e.Next = Bytes(src[e.Start+1 : e.End-1])
			}
			continue
		}
		if e.Kind() == document.Object {
			e.Info |= uint32(document.TapeFlagObjectKeysHashed) << document.InfoFlagsShift
		}
	}
}
