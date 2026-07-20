// Package byteview provides allocation-free, read-only views over byte and
// string storage. It is the repository's single unsafe boundary for
// slice/string representation conversions and caller-validated byte ranges.
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

// ByteAt reads one byte at index relative to base. It performs no bounds
// checks: base must point to byte zero of one live allocation, and the caller
// must prove that index is within that allocation.
func ByteAt(base *byte, index uintptr) byte {
	return *(*byte)(unsafe.Add(unsafe.Pointer(base), index))
}

// SliceRange returns a read-only byte view over [start, end) relative to base.
// It performs no bounds checks: base must point to byte zero of one live
// allocation, and the caller must prove 0 <= start <= end and that end is
// within that allocation. The source allocation must remain live and its bytes
// immutable for as long as the returned slice is reachable.
func SliceRange(base *byte, start, end uint32) []byte {
	return unsafe.Slice((*byte)(unsafe.Add(unsafe.Pointer(base), uintptr(start))), int(end-start))
}

// Bytes returns a read-only byte view of s without copying. Callers must not
// mutate the returned slice; doing so can corrupt immutable string storage.
func Bytes(s string) []byte {
	return unsafe.Slice(unsafe.StringData(s), len(s))
}
