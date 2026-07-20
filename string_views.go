package simdjson

import (
	"bytes"

	"github.com/thesyncim/simdjson/internal/byteview"
)

// appendJSONString exposes text as a read-only byte view for the shared JSON
// string encoder. The view is retained only for the duration of the call.
func appendJSONString(dst []byte, text string) []byte {
	return appendJSONStringBytes(dst, byteview.Bytes(text))
}

// string converts p.src[start:end] into a result string. Zero-copy results
// alias p.src directly. Owned results alias one lazily made private copy of
// the input, so a document's strings cost one allocation in total rather than
// one allocation each; retaining any decoded string retains that copy.
func (p *parser) string(start, end int) string {
	if start == end {
		return ""
	}
	if p.zeroCopy {
		return byteview.String(p.src[start:end])
	}
	if p.ownedSrc == nil {
		p.ownedSrc = bytes.Clone(p.src)
	}
	return byteview.String(p.ownedSrc[start:end])
}

// ownedBytesString exposes owned bytes as a string without copying. Callers
// must keep the backing bytes alive and immutable for the string's lifetime.
func ownedBytesString(b []byte) string {
	return byteview.String(b)
}
