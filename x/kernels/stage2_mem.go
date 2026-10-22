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

// posPairAt packs two native uint32 positions with the first in the low word.
// The caller proves that both positions are in the stream.
func posPairAt(p unsafe.Pointer, i int) uint64 {
	pair := (*[2]uint32)(unsafe.Add(p, uintptr(i)*4))
	return uint64(pair[0]) | uint64(pair[1])<<32
}

// storeUint32Pair writes the low and high words to consecutive native uint32
// fields. The caller proves the eight-byte window is writable.
func storeUint32Pair(p unsafe.Pointer, pair uint64) {
	fields := (*[2]uint32)(p)
	fields[0] = uint32(pair)
	fields[1] = uint32(pair >> 32)
}

// loadUint32LE returns the four bytes at base+i as a little-endian word. The
// caller proves the whole window [i, i+4) is in bounds.
func loadUint32LE(base unsafe.Pointer, i int) uint32 {
	return binary.LittleEndian.Uint32((*[4]byte)(unsafe.Add(base, uintptr(i)))[:])
}

// loadUint64LE returns the eight bytes at base+i as a little-endian word. The
// caller proves the whole window [i, i+8) is in bounds.
func loadUint64LE(base unsafe.Pointer, i int) uint64 {
	return binary.LittleEndian.Uint64((*[8]byte)(unsafe.Add(base, uintptr(i)))[:])
}
