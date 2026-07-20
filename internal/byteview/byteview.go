// Package byteview provides allocation-free, read-only views between byte
// slices and strings. It is the repository's single unsafe boundary for
// slice/string representation conversions.
package byteview

import "unsafe"

// String returns a string view of b without copying. The caller must keep b
// alive and immutable for as long as the returned string is reachable.
func String(b []byte) string {
	return unsafe.String(unsafe.SliceData(b), len(b))
}

// StringRange returns a string view over [start, end) relative to base without
// copying. It performs no bounds checks: base must point to byte zero of one
// live allocation, and the caller must prove 0 <= start <= end and that end is
// within that allocation. The source allocation must remain live and its bytes
// immutable for as long as the returned string is reachable.
func StringRange(base *byte, start, end int) string {
	return unsafe.String((*byte)(unsafe.Add(unsafe.Pointer(base), start)), end-start)
}

// Bytes returns a read-only byte view of s without copying. Callers must not
// mutate the returned slice; doing so can corrupt immutable string storage.
func Bytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
