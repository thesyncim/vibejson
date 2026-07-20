package kernels

import (
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
//     base, positions, or slab. Root compile-time assertions pin the entry
//     layout, kind values, and flags written here.
//   - slab is zeroed once at document start and preserved across chunks. st is
//     initialized by Stage2IndexReset and preserved with the same slab; on
//     entry, st.EntryOff is 16-byte aligned and at most entCap*16.
//   - Source and positions remain live for the call. Mutable state and output
//     are caller-exclusive, and this function retains no pointers.
func Stage2IndexPositionsFused(base *byte, n int, positions []uint32, slab *[Stage2IndexSlabLen]uint64, ent *byte, entCap int, st *Stage2IndexState) {
	basep := unsafe.Pointer(base)
	posp := unsafe.Pointer(unsafe.SliceData(positions))
	ptp := unsafe.Pointer(&stage2PairBad[0])
	slabp := unsafe.Pointer(&slab[0])
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
	pi := 0
	var j, cls, members, entryIndex, scope, parent, next, isKey uint64
	var c, d byte
	var p unsafe.Pointer
	var info, scalarInfo uint32
	var scan int
	var integer, scalarOK bool

dispatch:
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	pi++
	c = byteAt(basep, int(j))
	if stringState>>32 != 0 {
		if c != '"' || uint64(uint32(stringState))<<4 >= entryOff {
			bad |= 1
			goto done
		}
		*(*uint32)(unsafe.Add(entryp, uintptr(uint32(stringState))*16+4)) = uint32(j + 1)
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
	bad |= uint64(byteAt(ptp, int(prev|cls)))
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
	scope = entryIndex<<32 | count<<4
	info = Stage2IndexInfoArray
	if cls == stage2ccO {
		scope |= 8
		info = Stage2IndexInfoObject
	}
	*(*uint64)(unsafe.Add(slabp, uintptr(uint64(depth)&(Stage2IndexSlabLen-1))*8)) = scope
	p = unsafe.Add(entryp, uintptr(entryOff))
	*(*uint64)(p) = j
	*(*uint64)(unsafe.Add(p, 8)) = uint64(info) << 32
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
	scope = *(*uint64)(unsafe.Add(slabp, uintptr(uint64(depth+1)&(Stage2IndexSlabLen-1))*8))
	if cls == stage2ccC {
		bad |= (scope&8 ^ 8) >> 3
		info = Stage2IndexInfoObject
	} else {
		bad |= scope & 8 >> 3
		info = Stage2IndexInfoArray
	}
	count = scope >> 4 & (1<<26 - 1)
	entryIndex = scope >> 32
	p = unsafe.Add(entryp, uintptr(entryIndex)*16)
	*(*uint32)(unsafe.Add(p, 4)) = uint32(j + 1)
	next = entryOff>>4 - entryIndex
	*(*uint64)(unsafe.Add(p, 8)) = next | uint64(info|uint32(members))<<32
	parent = *(*uint64)(unsafe.Add(slabp, uintptr(uint64(depth)&(Stage2IndexSlabLen-1))*8))
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
	if len(positions)-pi < 3 {
		isKey = 8
		goto writeString
	}
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	next = *(*uint64)(unsafe.Add(posp, uintptr(pi)*4))
	parent = next & uint64(^uint32(0))
	scope = next >> 32
	if byteAt(basep, int(parent)) != '"' || byteAt(basep, int(scope)) != ':' {
		bad |= 1
		goto done
	}
	p = unsafe.Add(entryp, uintptr(entryOff))
	*(*uint64)(p) = j | (parent+1)<<32
	*(*uint64)(unsafe.Add(p, 8)) = 1 | uint64(Stage2IndexInfoString|Stage2IndexKeyFlag)<<32
	entryOff += 16
	j = uint64(posAt(posp, pi+2))
	c = byteAt(basep, int(j))
	pi += 3
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
	p = unsafe.Add(entryp, uintptr(entryOff))
	*(*uint64)(p) = j
	*(*uint64)(unsafe.Add(p, 8)) = 1 | uint64(info)<<32
	entryOff += 16
	stringState = uint64(uint32(entryIndex))
	prev = 96 | isKey<<4 | inObj
	key = 0
	if pi == len(positions) {
		stringState |= 1 << 32
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
	if c != '"' {
		bad |= 1
		goto done
	}
	pi++
	*(*uint32)(unsafe.Add(p, 4)) = uint32(j + 1)
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
	if len(positions)-pi < 2 {
		isKey = 0
		goto writeString
	}
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	next = *(*uint64)(unsafe.Add(posp, uintptr(pi)*4))
	parent = next & uint64(^uint32(0))
	scope = next >> 32
	if byteAt(basep, int(parent)) != '"' {
		bad |= 1
		goto done
	}
	d = byteAt(basep, int(scope))
	p = unsafe.Add(entryp, uintptr(entryOff))
	*(*uint64)(p) = j | (parent+1)<<32
	*(*uint64)(unsafe.Add(p, 8)) = 1 | uint64(Stage2IndexInfoString)<<32
	entryOff += 16
	pi += 2
	j = scope
	c = d
	prev = 96 | inObj
	key = 0
	goto fusedObjectDelimiterKnown

writeScalar:
	if entryOff>>4 >= uint64(entCap) {
		bad |= Stage2IndexFull
		goto done
	}
	scan = int(j)
	scalarOK = true
	switch c {
	case 't':
		if scan+4 > n || *(*uint32)(unsafe.Add(basep, scan)) != 0x65757274 {
			scalarOK = false
		} else {
			scan += 4
			scalarInfo = Stage2IndexInfoBool
		}
	case 'f':
		if scan+5 > n || *(*uint32)(unsafe.Add(basep, scan+1)) != 0x65736c61 {
			scalarOK = false
		} else {
			scan += 5
			scalarInfo = Stage2IndexInfoBool
		}
	case 'n':
		if scan+4 > n || *(*uint32)(unsafe.Add(basep, scan)) != 0x6c6c756e {
			scalarOK = false
		} else {
			scan += 4
			scalarInfo = Stage2IndexInfoNull
		}
	default:
		if c == '-' {
			scan++
			if scan >= n {
				scalarOK = false
				goto scalarScanned
			}
		}
		d = byteAt(basep, scan)
		if d == '0' {
			scan++
		} else if '1' <= d && d <= '9' {
			scan++
			if scan+8 <= n {
				invalid := stage2NonDigitMask8(loadUint64LE(basep, scan))
				if invalid == 0 {
					scan += 8
				} else {
					scan += bits.TrailingZeros64(invalid) >> 3
				}
			}
			for ; scan < n; scan++ {
				d = byteAt(basep, scan)
				if d < '0' || d > '9' {
					break
				}
			}
		} else {
			scalarOK = false
			goto scalarScanned
		}
		integer = true
		if scan < n && byteAt(basep, scan) == '.' {
			integer = false
			scan++
			if scan >= n {
				scalarOK = false
				goto scalarScanned
			}
			d = byteAt(basep, scan)
			if d < '0' || d > '9' {
				scalarOK = false
				goto scalarScanned
			}
			scan++
			if scan+8 <= n {
				invalid := stage2NonDigitMask8(loadUint64LE(basep, scan))
				if invalid == 0 {
					scan += 8
				} else {
					scan += bits.TrailingZeros64(invalid) >> 3
				}
			}
			for ; scan < n; scan++ {
				d = byteAt(basep, scan)
				if d < '0' || d > '9' {
					break
				}
			}
		}
		if scan < n {
			d = byteAt(basep, scan)
			if d == 'e' || d == 'E' {
				integer = false
				scan++
				if scan < n {
					d = byteAt(basep, scan)
					if d == '+' || d == '-' {
						scan++
					}
				}
				if scan >= n {
					scalarOK = false
					goto scalarScanned
				}
				d = byteAt(basep, scan)
				if d < '0' || d > '9' {
					scalarOK = false
					goto scalarScanned
				}
				scan++
				if scan+8 <= n {
					invalid := stage2NonDigitMask8(loadUint64LE(basep, scan))
					if invalid == 0 {
						scan += 8
					} else {
						scan += bits.TrailingZeros64(invalid) >> 3
					}
				}
				for ; scan < n; scan++ {
					d = byteAt(basep, scan)
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
	if scalarOK && scan < n {
		d = byteAt(basep, scan)
		if stage2ScalarEnd[d] == 0 {
			scalarOK = false
		}
	}
	if !scalarOK {
		bad |= 1
		goto done
	}
	p = unsafe.Add(entryp, uintptr(entryOff))
	*(*uint64)(p) = j | uint64(uint32(scan))<<32
	*(*uint64)(unsafe.Add(p, 8)) = 1 | uint64(scalarInfo)<<32
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
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
	if c != '"' {
		pi++
		cls = uint64(stage2Class[c])
		goto handleKnown
	}
	pi++
	goto writeKey

fusedColon:
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
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
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
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
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
	pi++
fusedObjectDelimiterKnown:
	if c == ',' {
		count++
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
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
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
	if pi == len(positions) {
		goto done
	}
	j = uint64(posAt(posp, pi))
	c = byteAt(basep, int(j))
	pi++
	if c == ',' {
		count++
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
