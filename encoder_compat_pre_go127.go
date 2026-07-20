//go:build !go1.27

package simdjson

import simdkernels "github.com/thesyncim/simdjson/simd"

const (
	// encoding/json v1 has no encoder nesting limit. Recursive
	// reference-bearing values are protected independently by delayed
	// identity-based cycle checks.
	encoderHasDepthLimit = false
	// encoding/json v1 gives string kinds precedence over TextMarshaler.
	mapKeyStringKindFirst = true
	// Go 1.26's encoding/json v1 writes the replacement rune as an escape.
	escapeInvalidUTF8 = true
)

// encodeState follows encoding/json v1 on stable Go releases. In addition to
// the ordinary encoder state, it keeps a cold identity set once a recursive
// pointer, map, or slice path grows deep enough to need cycle detection.
type encodeState struct {
	dst   []byte
	depth int
	// ptrRun counts reference-bearing values along the current path. The
	// identity map remains nil for ordinary documents and is populated only
	// beyond encoderStartDetectingCyclesAfter.
	ptrRun     int
	ptrSeen    map[encoderCycleKey]struct{}
	escapeHTML bool
	// nonAddr is set while encoding a value reached without addressability —
	// a map value or interface content, inherited through structs and
	// arrays, cleared through pointers and slices. It reroutes a
	// pointer-receiver marshaler to its default encoding, matching
	// encoding/json's condAddrEncoder.
	nonAddr   bool
	scratch   *encoderScratch
	timeCache simdkernels.TimeCache
}
