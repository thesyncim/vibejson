package kernels

import (
	"encoding/binary"
	"math/bits"
	"unsafe"
)

func stage2NonDigitMask8(x uint64) uint64 {
	const (
		add  = uint64(0x4646464646464646)
		sub  = uint64(0x3030303030303030)
		high = uint64(0x8080808080808080)
	)
	return ((x + add) | (x - sub)) & high
}

// Stage2IndexPositionsFused keeps the common object and array transitions in
// one straight-line machine. A complete string pair is consumed together, and
// object keys additionally fuse their colon, removing three dispatches from
// the dominant object-member sequence.
//
// Unsafe contract:
//   - n is non-negative; base names n live, immutable bytes. base may be nil
//     only when n and len(positions) are zero. positions is monotonically
//     increasing Stage-1 output, and every position is less than n.
//   - entCap is non-negative. ent is nil exactly when entCap is zero; otherwise
//     it names entCap contiguous, writable 16-byte entries that do not overlap
//     base, positions, or slab. Each entry contains four native uint32 fields:
//     start, end, next, and info. Root compile-time assertions pin the entry
//     layout, kind values, and flags written here.
//   - slab is zeroed once at document start and preserved across chunks. st is
//     initialized by Stage2IndexReset and preserved with the same slab; on
//     entry, st.EntryOff is 16-byte aligned and at most entCap*16.
//   - Source and positions remain live for the call. Mutable state and output
//     are caller-exclusive, and this function retains no pointers.
func Stage2IndexPositionsFused(base *byte, n int, positions []uint32, slab *[Stage2IndexSlabLen]uint64, ent *byte, entCap int, st *Stage2IndexState) {
	basep := unsafe.Pointer(base)
	src := unsafe.Slice(base, n)
	entryp := unsafe.Pointer(ent)

	bad := st.Bad
	depth := st.Depth
	prev := st.PrevRowIO
	key := st.KeyRow8
	count := st.Count
	entryOff := st.EntryOff
	// Keep resumable quote state in one register; the entry index matters only
	// while the high-word resume flag is set.
	stringState := uint64(st.StringEntry) | uint64(st.InString)<<32
	objectStringFast := st.ObjectStringFast
	inObj := prev & 8
	pi := uint(0)
	posLen := uint(len(positions))
	var j, cls, members, entryIndex, scope, parent, next, isKey uint64
	var c, d byte
	var e *[4]uint32
	var info, scalarInfo uint32
	var scan uint
	var integer, scalarOK bool

dispatch:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	pi++
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	if stringState>>32 != 0 {
		if c != '"' || uint64(uint32(stringState))<<4 >= entryOff {
			bad |= 1
			goto done
		}
		(*[4]uint32)(unsafe.Add(entryp, uintptr(uint32(stringState))*16))[1] = uint32(j + 1)
		stringState = 0
		if prev>>4&15 == stage2RowQk {
			goto fusedColon
		}
		if inObj != 0 {
			goto fusedObjectComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	}
	cls = uint64(stage2Class[c])

handleKnown:
	bad |= uint64(stage2PairBad[byte(prev|cls)])
	switch cls {
	case stage2ccO, stage2ccA:
		goto openContainer
	case stage2ccC, stage2ccB:
		members = count
		if (cls == stage2ccC && prev != 8) || (cls == stage2ccB && prev != 16) {
			members++
		}
		goto closeContainer
	case stage2ccL:
		prev = 64 | inObj
		key = 0
		goto fusedValue
	case stage2ccM:
		if depth == 0 {
			bad |= 1
		}
		count++
		if count >= Stage2IndexMaxCount {
			bad |= Stage2IndexCount
		}
		prev = 80 | inObj
		key = inObj
		if inObj != 0 {
			goto fusedKey
		}
		goto fusedArrayValue
	case stage2ccQ:
		isKey = key
		if isKey != 0 {
			goto writeKey
		}
		goto writeString
	default:
		goto writeScalar
	}

openContainer:
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	depth++
	if depth > Stage2IndexMaxDepth {
		bad |= Stage2IndexDeep
		goto done
	}
	entryIndex = entryOff >> 4
	if count >= Stage2IndexMaxCount {
		bad |= Stage2IndexCount
	}
	scope = entryIndex<<32 | (count&Stage2IndexMaxCount)<<4
	info = Stage2IndexInfoArray
	if cls == stage2ccO {
		scope |= 8
		info = Stage2IndexInfoObject
	}
	slab[uint64(depth)&(Stage2IndexSlabLen-1)] = scope
	e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryOff)))
	e[0] = uint32(j)
	e[1] = 0
	e[2] = 0
	e[3] = info
	entryOff += 16
	count = 0
	if cls == stage2ccO {
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	}
	inObj = 0
	prev = 16
	key = 0
	goto fusedArrayValue

closeContainer:
	depth--
	if depth < 0 {
		bad |= 1
		goto done
	}
	scope = slab[uint64(depth+1)&(Stage2IndexSlabLen-1)]
	if cls == stage2ccC {
		bad |= (scope&8 ^ 8) >> 3
		info = Stage2IndexInfoObject
	} else {
		bad |= scope & 8 >> 3
		info = Stage2IndexInfoArray
	}
	if members > Stage2IndexMaxCount {
		bad |= Stage2IndexCount
	}
	count = scope >> 4 & (1<<26 - 1)
	entryIndex = scope >> 32
	e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryIndex)*16))
	e[1] = uint32(j + 1)
	next = entryOff>>4 - entryIndex
	e[2] = uint32(next)
	e[3] = info | uint32(members&Stage2IndexMaxCount)
	parent = slab[uint64(depth)&(Stage2IndexSlabLen-1)]
	inObj = parent & 8
	if cls == stage2ccC {
		prev = 32 | inObj
	} else {
		prev = 48 | inObj
	}
	key = 0
	if inObj != 0 {
		goto fusedObjectComma
	}
	if depth > 0 {
		goto fusedArrayComma
	}
	goto dispatch

writeKey:
	// A complete key contributes closer, colon, and value as three consecutive
	// positions. Load the first pair together and finish the key in two stores.
	if pi > uint(len(positions)) || uint(len(positions))-pi < 3 {
		isKey = 8
		goto writeString
	}
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	{
		rem := positions[pi:]
		if len(rem) < 3 {
			isKey = 8
			goto writeString
		}
		_ = rem[2]
		parent = uint64(rem[0])
		scope = uint64(rem[1])
		if *(*byte)(unsafe.Add(basep, uintptr(parent))) != '"' || *(*byte)(unsafe.Add(basep, uintptr(scope))) != ':' {
			bad |= 1
			goto done
		}
		e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryOff)))
		e[0] = uint32(j)
		e[1] = uint32(parent + 1)
		e[2] = 1
		e[3] = Stage2IndexInfoString | Stage2IndexKeyFlag
		entryOff += 16
		j = uint64(rem[2])
		c = *(*byte)(unsafe.Add(basep, uintptr(j)))
		pi += 3
	}
	prev = 64 | inObj
	key = 0
	goto fusedValueKnown

writeString:
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	entryIndex = entryOff >> 4
	info = Stage2IndexInfoString
	if isKey != 0 {
		info |= Stage2IndexKeyFlag
	}
	e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryOff)))
	e[0] = uint32(j)
	e[1] = 0
	e[2] = 1
	e[3] = info
	entryOff += 16
	stringState = uint64(uint32(entryIndex))
	prev = 96 | isKey<<4 | inObj
	key = 0
	if pi >= posLen {
		stringState |= 1 << 32
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	if c != '"' {
		bad |= 1
		goto done
	}
	pi++
	e[1] = uint32(j + 1)
	if isKey != 0 {
		goto fusedColon
	}
	if inObj != 0 {
		goto fusedObjectComma
	}
	if depth > 0 {
		goto fusedArrayComma
	}
	goto dispatch

writeObjectString:
	// String-dominant object documents can finish the value and its following
	// delimiter from one packed position load. Chunk-boundary pairs keep using
	// the resumable string path.
	if pi > uint(len(positions)) || uint(len(positions))-pi < 2 {
		isKey = 0
		goto writeString
	}
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	{
		rem := positions[pi:]
		if len(rem) < 2 {
			isKey = 0
			goto writeString
		}
		_ = rem[1]
		parent = uint64(rem[0])
		scope = uint64(rem[1])
		if *(*byte)(unsafe.Add(basep, uintptr(parent))) != '"' {
			bad |= 1
			goto done
		}
		d = *(*byte)(unsafe.Add(basep, uintptr(scope)))
		e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryOff)))
		e[0] = uint32(j)
		e[1] = uint32(parent + 1)
		e[2] = 1
		e[3] = Stage2IndexInfoString
		entryOff += 16
		pi += 2
		j = scope
		c = d
	}
	prev = 96 | inObj
	key = 0
	goto fusedObjectDelimiterKnown

writeScalar:
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	scan = uint(uint32(j))
	scalarOK = true
	switch c {
	case 't':
		if scan > uint(len(src)) {
			scalarOK = false
		} else if rem := src[scan:]; len(rem) < 4 || binary.LittleEndian.Uint32(rem) != 0x65757274 {
			scalarOK = false
		} else {
			scan += 4
			scalarInfo = Stage2IndexInfoBool
		}
	case 'f':
		if scan > uint(len(src)) {
			scalarOK = false
		} else if rem := src[scan:]; len(rem) < 5 || binary.LittleEndian.Uint32(rem[1:]) != 0x65736c61 {
			scalarOK = false
		} else {
			scan += 5
			scalarInfo = Stage2IndexInfoBool
		}
	case 'n':
		if scan > uint(len(src)) {
			scalarOK = false
		} else if rem := src[scan:]; len(rem) < 4 || binary.LittleEndian.Uint32(rem) != 0x6c6c756e {
			scalarOK = false
		} else {
			scan += 4
			scalarInfo = Stage2IndexInfoNull
		}
	default:
		if c == '-' {
			scan++
			if scan >= uint(len(src)) {
				scalarOK = false
				goto scalarScanned
			}
		}
		if scan >= uint(len(src)) {
			scalarOK = false
			goto scalarScanned
		}
		d = src[scan]
		if d == '0' {
			scan++
		} else if '1' <= d && d <= '9' {
			scan++
			if scan <= uint(len(src)) {
				if rem := src[scan:]; len(rem) >= 8 {
					invalid := stage2NonDigitMask8(binary.LittleEndian.Uint64(rem))
					if invalid == 0 {
						scan += 8
					} else {
						scan += uint(bits.TrailingZeros64(invalid) >> 3)
					}
				}
			}
			for ; scan < uint(len(src)); scan++ {
				d = src[scan]
				if d < '0' || d > '9' {
					break
				}
			}
		} else {
			scalarOK = false
			goto scalarScanned
		}
		integer = true
		if scan < uint(len(src)) && src[scan] == '.' {
			integer = false
			scan++
			if scan >= uint(len(src)) {
				scalarOK = false
				goto scalarScanned
			}
			d = src[scan]
			if d < '0' || d > '9' {
				scalarOK = false
				goto scalarScanned
			}
			scan++
			if scan <= uint(len(src)) {
				if rem := src[scan:]; len(rem) >= 8 {
					invalid := stage2NonDigitMask8(binary.LittleEndian.Uint64(rem))
					if invalid == 0 {
						scan += 8
					} else {
						scan += uint(bits.TrailingZeros64(invalid) >> 3)
					}
				}
			}
			for ; scan < uint(len(src)); scan++ {
				d = src[scan]
				if d < '0' || d > '9' {
					break
				}
			}
		}
		if scan < uint(len(src)) {
			d = src[scan]
			if d == 'e' || d == 'E' {
				integer = false
				scan++
				if scan < uint(len(src)) {
					d = src[scan]
					if d == '+' || d == '-' {
						scan++
					}
				}
				if scan >= uint(len(src)) {
					scalarOK = false
					goto scalarScanned
				}
				d = src[scan]
				if d < '0' || d > '9' {
					scalarOK = false
					goto scalarScanned
				}
				scan++
				if scan <= uint(len(src)) && uint(len(src))-scan >= 8 {
					invalid := stage2NonDigitMask8(binary.LittleEndian.Uint64(src[scan:]))
					if invalid == 0 {
						scan += 8
					} else {
						scan += uint(bits.TrailingZeros64(invalid) >> 3)
					}
				}
				for ; scan < uint(len(src)); scan++ {
					d = src[scan]
					if d < '0' || d > '9' {
						break
					}
				}
			}
		}
		scalarInfo = Stage2IndexInfoNumber
		if integer {
			scalarInfo |= Stage2IndexIntFlag
		}
	}

scalarScanned:
	if scalarOK && scan < uint(len(src)) {
		d = src[scan]
		if stage2ScalarEnd[d] == 0 {
			scalarOK = false
		}
	}
	if !scalarOK {
		bad |= 1
		goto done
	}
	e = (*[4]uint32)(unsafe.Add(entryp, uintptr(entryOff)))
	e[0] = uint32(j)
	e[1] = uint32(scan)
	e[2] = 1
	e[3] = scalarInfo
	entryOff += 16
	prev = 112 | inObj
	key = 0
	if inObj != 0 {
		goto fusedObjectComma
	}
	if depth > 0 {
		goto fusedArrayComma
	}
	goto dispatch

fusedKey:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	if c != '"' {
		pi++
		cls = uint64(stage2Class[c])
		goto handleKnown
	}
	pi++
	goto writeKey

fusedColon:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	if c != ':' {
		pi++
		cls = uint64(stage2Class[c])
		goto handleKnown
	}
	pi++
	prev = 64 | inObj
	key = 0
	goto fusedValue

fusedValue:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	pi++
fusedValueKnown:
	switch c {
	case '"':
		if objectStringFast != 0 && inObj != 0 {
			goto writeObjectString
		}
		isKey = 0
		goto writeString
	case '{':
		cls = stage2ccO
		goto openContainer
	case '[':
		cls = stage2ccA
		goto openContainer
	default:
		cls = uint64(stage2Class[c])
		if cls == stage2ccS {
			goto writeScalar
		}
		goto handleKnown
	}

fusedObjectComma:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	pi++
fusedObjectDelimiterKnown:
	if c == ',' {
		count++
		if count >= Stage2IndexMaxCount {
			bad |= Stage2IndexCount
		}
		prev = 88
		key = 8
		goto fusedKey
	}
	if c == '}' {
		cls = stage2ccC
		members = count + 1
		goto closeContainer
	}
	cls = uint64(stage2Class[c])
	goto handleKnown

fusedArrayValue:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	pi++
	switch c {
	case '"':
		isKey = 0
		goto writeString
	case '{':
		cls = stage2ccO
		goto openContainer
	case '[':
		cls = stage2ccA
		goto openContainer
	case ']':
		cls = stage2ccB
		if prev == 16 {
			members = count
			goto closeContainer
		}
		goto handleKnown
	default:
		cls = uint64(stage2Class[c])
		if cls == stage2ccS {
			goto writeScalar
		}
		goto handleKnown
	}

fusedArrayComma:
	if pi >= posLen {
		goto done
	}
	j = uint64(positions[pi])
	c = *(*byte)(unsafe.Add(basep, uintptr(j)))
	pi++
	if c == ',' {
		count++
		if count >= Stage2IndexMaxCount {
			bad |= Stage2IndexCount
		}
		prev = 80
		key = 0
		goto fusedArrayValue
	}
	if c == ']' {
		cls = stage2ccB
		members = count + 1
		goto closeContainer
	}
	cls = uint64(stage2Class[c])
	goto handleKnown
done:
	st.Bad = bad
	st.Depth = depth
	st.PrevRowIO = prev
	st.KeyRow8 = key
	st.Count = count
	st.EntryOff = entryOff
	st.StringEntry = uint32(stringState)
	st.InString = uint32(stringState >> 32)
}
