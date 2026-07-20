//go:build go1.27

package simdjson

import simdkernels "github.com/thesyncim/simdjson/simd"

const (
	encoderHasDepthLimit  = true
	mapKeyStringKindFirst = false
	// Go 1.27's encoding/json writes the replacement rune literally.
	escapeInvalidUTF8 = false
)

// encodeState keeps the Go 1.27 encoder's layout and depth bookkeeping
// unchanged. That release's encoding/json implementation rejects documents
// nested beyond 10,000 containers, so no identity map is needed here.
type encodeState struct {
	dst   []byte
	depth int
	// ptrRun counts pointer hops along the current path with its own
	// budget, so a pure pointer cycle still terminates while pointers no
	// longer double-count against the container depth limit.
	ptrRun     int
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
