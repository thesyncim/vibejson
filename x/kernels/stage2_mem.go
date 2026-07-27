package kernels

import (
	"encoding/binary"
	"unsafe"
)

// byteAt returns the byte at base+i. The caller proves 0 <= i and that the
// addressed byte is inside the allocation base names, and keeps that
// allocation live for the read.
func byteAt(base unsafe.Pointer, i int) byte {
	return *(*byte)(unsafe.Add(base, uintptr(i)))
}

// posAt returns element i of the uint32 position stream starting at p. The
// caller proves i is within the stream it derived p from.
func posAt(p unsafe.Pointer, i int) uint32 {
	return *(*uint32)(unsafe.Add(p, uintptr(i)*4))
}

// loadUint64LE returns the eight bytes at base+i as a little-endian word. The
// caller proves the whole window [i, i+8) is in bounds.
func loadUint64LE(base unsafe.Pointer, i int) uint64 {
	return binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, uintptr(i)))[:])
}
