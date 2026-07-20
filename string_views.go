package simdjson

import (
	"bytes"
	"unsafe"
)

// appendJSONString exposes text as a read-only byte view for the shared JSON
// string encoder. The view is retained only for the duration of the call.
func appendJSONString(dst []byte, text string) []byte {
	return appendJSONStringBytes(dst, unsafe.Slice(unsafe.StringData(text), len(text)))
}

// string converts a subslice of p.src into a result string. Zero-copy results
// alias p.src directly. Owned results alias one lazily made private copy of
// the input, so a document's strings cost one allocation in total rather than
// one allocation each; retaining any decoded string retains that copy.
func (p *parser) string(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	if p.zeroCopy {
		return unsafe.String(unsafe.SliceData(b), len(b))
	}
	if p.ownedSrc == nil {
		p.ownedSrc = bytes.Clone(p.src)
	}
	offset := uintptr(unsafe.Pointer(unsafe.SliceData(b))) - uintptr(unsafe.Pointer(unsafe.SliceData(p.src)))
	return unsafe.String(&p.ownedSrc[offset], len(b))
}

// ownedBytesString exposes owned bytes as a string without copying. Callers
// must keep the backing bytes alive and immutable for the string's lifetime.
func ownedBytesString(b []byte) string {
	if len(b) == 0 {
		return ""
	}
	return unsafe.String(unsafe.SliceData(b), len(b))
}
