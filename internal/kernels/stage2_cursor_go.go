package kernels

import "unsafe"

// Stage2CursorGo consumes one complete colon-elided Stage1CursorBlocks stream.
// Positions are absolute source offsets and include both boundaries of every
// string. The machine pairs those boundaries, validates each omitted key colon
// from the raw gap, and records absolute scalar starts. Call Stage2Reset before
// the walk and Stage2Finish after it.
func Stage2CursorGo(base *byte, n int, positions []uint32, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	if len(scalars) < len(positions) {
		panic("simdjson: Stage2CursorGo scalars shorter than the position bound")
	}
	return stage2CursorGo(base, n, positions, kinds, scalars, st)
}

func stage2CursorGo(base *byte, n int, positions []uint32, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	basep := unsafe.Pointer(base)
	posp := unsafe.Pointer(unsafe.SliceData(positions))
	ptp := unsafe.Pointer(&stage2PairBad[0])
	kindp := unsafe.Pointer(&kinds[0])
	scalarp := unsafe.Pointer(unsafe.SliceData(scalars))

	bad := st.Bad
	depth := st.Depth
	prev := st.PrevRowIO
	key := st.KeyRow8
	inObj := prev & 8
	nscalars := 0

	for i := 0; i < len(positions); {
		j := int(*(*uint32)(unsafe.Add(posp, uintptr(i)*4)))
		i++
		if uint(j) >= uint(n) {
			bad |= 1
			continue
		}
		c := *(*byte)(unsafe.Add(basep, j))
		cls := uint64(stage2Class[c])
		bad |= uint64(*(*byte)(unsafe.Add(ptp, prev|cls)))

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
		case stage2ccA:
			depth++
			if depth > Stage2MaxDepth {
				bad |= 1
			}
			*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1)))) = 0
			inObj = 0
			prev = 16
			key = 0
		case stage2ccC:
			bad |= (inObj ^ 8) >> 3
			depth--
			if depth < 0 {
				bad |= 1
			}
			inObj = uint64(*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1))))) & 8
			prev = 32 | inObj
			key = 0
		case stage2ccB:
			bad |= inObj >> 3
			depth--
			if depth < 0 {
				bad |= 1
			}
			inObj = uint64(*(*byte)(unsafe.Add(kindp, uintptr(uint64(depth)&(Stage2KindsLen-1))))) & 8
			prev = 48 | inObj
			key = 0
		case stage2ccL:
			// Cursor streams omit colons. Seeing one means the stream contract
			// was not honored; retain exact grammar handling and fail closed.
			bad |= 1
			prev = 64 | inObj
			key = 0
		case stage2ccM:
			if depth == 0 {
				bad |= 1
			}
			prev = 80 | inObj
			key = inObj
		case stage2ccQ:
			if i >= len(positions) {
				bad |= 1
				prev = 96 | key<<4 | inObj
				key = 0
				continue
			}
			close := int(*(*uint32)(unsafe.Add(posp, uintptr(i)*4)))
			i++
			if uint(close) >= uint(n) || close <= j || *(*byte)(unsafe.Add(basep, close)) != '"' {
				bad |= 1
			}
			if key != 0 {
				value := n
				if i < len(positions) {
					value = int(*(*uint32)(unsafe.Add(posp, uintptr(i)*4)))
				}
				if !stage2CursorColonGap(basep, n, close, value) {
					bad |= 1
				}
				prev = 64 | inObj
				key = 0
			} else {
				prev = 96 | inObj
			}
		default:
			*(*uint32)(unsafe.Add(scalarp, uintptr(nscalars)*4)) = uint32(j)
			nscalars++
			prev = 112 | inObj
			key = 0
		}
	}

	st.Bad = bad
	st.Depth = depth
	st.PrevRowIO = prev
	st.KeyRow8 = key
	return nscalars
}

func stage2CursorColonGap(base unsafe.Pointer, n, close, value int) bool {
	i := close + 1
	for i < value && uint(i) < uint(n) && stage2JSONSpace(*(*byte)(unsafe.Add(base, i))) {
		i++
	}
	if i >= value || uint(i) >= uint(n) || *(*byte)(unsafe.Add(base, i)) != ':' {
		return false
	}
	for i++; i < value && uint(i) < uint(n) && stage2JSONSpace(*(*byte)(unsafe.Add(base, i))); i++ {
	}
	return i == value
}

func stage2JSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r'
}
