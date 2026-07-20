package kernels

import "unsafe"

// Stage2PositionsTrusted consumes the validation-only stream produced by
// Stage1ValidBlocks. Callers must prove len(scalars) >= len(positions); keeping
// that check outside the grammar kernel avoids panic edges and stack spills.
func Stage2PositionsTrusted(base *byte, positions []uint32, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	basep := unsafe.Pointer(base)
	pos := unsafe.Pointer(unsafe.SliceData(positions))
	posEnd := unsafe.Add(pos, uintptr(len(positions))*4)
	ptp := unsafe.Pointer(&stage2PairBad[0])
	kindp := unsafe.Pointer(&kinds[0])
	scalar := unsafe.Pointer(unsafe.SliceData(scalars))

	bad := st.Bad
	depth := st.Depth
	prev := st.PrevRowIO
	key := st.KeyRow8
	inObj := prev & 8
	nscalars := 0
	var j int
	var cls uint64
	var c byte

dispatch:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	pos = unsafe.Add(pos, 4)
	cls = uint64(stage2Class[byteAt(basep, j)])

handleKnown:
	bad |= uint64(byteAt(ptp, int(prev|cls)))
	switch cls {
	case stage2ccO:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 8
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	case stage2ccA:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 0
		inObj = 0
		prev = 16
		key = 0
		goto fusedArrayValue
	case stage2ccC:
		bad |= (inObj ^ 8) >> 3
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(byteAt(kindp, int(uint64(depth)&(Stage2KindsLen-1)))) & 8
		prev = 32 | inObj
		key = 0
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	case stage2ccB:
		bad |= inObj >> 3
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(byteAt(kindp, int(uint64(depth)&(Stage2KindsLen-1)))) & 8
		prev = 48 | inObj
		key = 0
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	case stage2ccL:
		prev = 64 | inObj
		key = 0
		goto dispatch
	case stage2ccM:
		if depth == 0 {
			bad |= 1
		}
		prev = 80 | inObj
		key = inObj
		if inObj != 0 {
			goto fusedKey
		}
		if depth > 0 {
			goto fusedArrayValue
		}
		goto dispatch
	case stage2ccQ:
		isKey := key
		prev = 96 | key<<4 | inObj
		key = 0
		if isKey != 0 {
			goto fusedColon
		}
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	default:
		*(*uint32)(scalar) = uint32(j)
		scalar = unsafe.Add(scalar, 4)
		nscalars++
		prev = 112 | inObj
		key = 0
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	}

fusedKey:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	if byteAt(basep, j) != '"' {
		pos = unsafe.Add(pos, 4)
		cls = uint64(stage2Class[byteAt(basep, j)])
		goto handleKnown
	}
	pos = unsafe.Add(pos, 4)
	prev = 224 | inObj
	goto fusedColon

fusedColon:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	if byteAt(basep, j) != ':' {
		pos = unsafe.Add(pos, 4)
		cls = uint64(stage2Class[byteAt(basep, j)])
		goto handleKnown
	}
	pos = unsafe.Add(pos, 4)
	prev = 64 | inObj
	key = 0
	goto fusedValue

fusedValue:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	switch c := byteAt(basep, j); c {
	case '"':
		pos = unsafe.Add(pos, 4)
		prev = 96 | inObj
		key = 0
		goto fusedComma
	case '{':
		pos = unsafe.Add(pos, 4)
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 8
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	case '[':
		pos = unsafe.Add(pos, 4)
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 0
		inObj = 0
		prev = 16
		key = 0
		goto fusedArrayValue
	default:
		cls = uint64(stage2Class[c])
		pos = unsafe.Add(pos, 4)
		if cls != stage2ccS {
			goto handleKnown
		}
		*(*uint32)(scalar) = uint32(j)
		scalar = unsafe.Add(scalar, 4)
		nscalars++
		prev = 112 | inObj
		key = 0
		goto fusedComma
	}

fusedComma:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	switch byteAt(basep, j) {
	case ',':
		pos = unsafe.Add(pos, 4)
		prev = 80 | inObj
		key = inObj
		goto fusedKey
	case '}':
		pos = unsafe.Add(pos, 4)
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(byteAt(kindp, int(uint64(depth)&(Stage2KindsLen-1)))) & 8
		prev = 32 | inObj
		key = 0
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	default:
		pos = unsafe.Add(pos, 4)
		cls = uint64(stage2Class[byteAt(basep, j)])
		goto handleKnown
	}

fusedArrayComma:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	switch byteAt(basep, j) {
	case ',':
		pos = unsafe.Add(pos, 4)
		prev = 80
		key = 0
		goto fusedArrayValue
	case ']':
		pos = unsafe.Add(pos, 4)
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(byteAt(kindp, int(uint64(depth)&(Stage2KindsLen-1)))) & 8
		prev = 48 | inObj
		key = 0
		if inObj != 0 {
			goto fusedComma
		}
		if depth > 0 {
			goto fusedArrayComma
		}
		goto dispatch
	default:
		pos = unsafe.Add(pos, 4)
		cls = uint64(stage2Class[byteAt(basep, j)])
		goto handleKnown
	}

fusedArrayValue:
	if pos == posEnd {
		goto done
	}
	j = int(*(*uint32)(pos))
	c = byteAt(basep, j)
	pos = unsafe.Add(pos, 4)
	switch uint64(stage2Class[c]) {
	case stage2ccQ:
		prev = 96
		key = 0
		goto fusedArrayComma
	case stage2ccS:
		*(*uint32)(scalar) = uint32(j)
		scalar = unsafe.Add(scalar, 4)
		nscalars++
		prev = 112
		key = 0
		goto fusedArrayComma
	case stage2ccA:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 0
		inObj = 0
		prev = 16
		key = 0
		goto fusedArrayValue
	case stage2ccO:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 8
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	default:
		cls = uint64(stage2Class[c])
		goto handleKnown
	}

done:
	st.Bad = bad
	st.Depth = depth
	st.PrevRowIO = prev
	st.KeyRow8 = key
	return nscalars
}
