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

// Bytes returns a read-only byte view of s without copying. Callers must not
// mutate the returned slice; doing so can corrupt immutable string storage.
func Bytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
