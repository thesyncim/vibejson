package kernels

// Stage2PositionsTrusted consumes the validation-only stream produced by
// Stage1ValidBlocks. Callers must prove len(scalars) >= len(positions); keeping
// that check outside the grammar kernel avoids panic edges and stack spills.
func Stage2PositionsTrusted(src []byte, positions []uint32, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	n := len(positions)
	if n == 0 {
		return 0
	}
	scalars = scalars[:n]

	bad := st.Bad
	depth := st.Depth
	prev := st.PrevRowIO
	key := st.KeyRow8
	inObj := prev & 8
	pi := 0
	nscalars := 0
	var j int
	var cls uint64
	var c byte

dispatch:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	pi++
	cls = uint64(stage2Class[src[j]])

handleKnown:
	bad |= uint64(stage2PairBad[byte(prev|cls)])
	switch cls {
	case stage2ccO:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 8
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	case stage2ccA:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 0
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
		inObj = uint64(kinds[uint64(depth)&(Stage2KindsLen-1)]) & 8
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
		inObj = uint64(kinds[uint64(depth)&(Stage2KindsLen-1)]) & 8
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
		if uint(nscalars) < uint(len(scalars)) {
			scalars[nscalars] = uint32(j)
			nscalars++
		}
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
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	c = src[j]
	pi++
	if c != '"' {
		cls = uint64(stage2Class[c])
		goto handleKnown
	}
	prev = 224 | inObj
	goto fusedColon

fusedColon:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	c = src[j]
	pi++
	if c != ':' {
		cls = uint64(stage2Class[c])
		goto handleKnown
	}
	prev = 64 | inObj
	key = 0
	goto fusedValue

fusedValue:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	pi++
	switch c = src[j]; c {
	case '"':
		prev = 96 | inObj
		key = 0
		goto fusedComma
	case '{':
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 8
		inObj = 8
		prev = 8
		key = 8
		goto fusedKey
	case '[':
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 0
		inObj = 0
		prev = 16
		key = 0
		goto fusedArrayValue
	default:
		cls = uint64(stage2Class[c])
		if cls != stage2ccS {
			goto handleKnown
		}
		if uint(nscalars) < uint(len(scalars)) {
			scalars[nscalars] = uint32(j)
			nscalars++
		}
		prev = 112 | inObj
		key = 0
		goto fusedComma
	}

fusedComma:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	c = src[j]
	pi++
	switch c {
	case ',':
		prev = 80 | inObj
		key = inObj
		goto fusedKey
	case '}':
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(kinds[uint64(depth)&(Stage2KindsLen-1)]) & 8
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
		cls = uint64(stage2Class[c])
		goto handleKnown
	}

fusedArrayComma:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	c = src[j]
	pi++
	switch c {
	case ',':
		prev = 80
		key = 0
		goto fusedArrayValue
	case ']':
		depth--
		if depth < 0 {
			bad |= 1
		}
		inObj = uint64(kinds[uint64(depth)&(Stage2KindsLen-1)]) & 8
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
		cls = uint64(stage2Class[c])
		goto handleKnown
	}

fusedArrayValue:
	if uint(pi) >= uint(len(positions)) {
		goto done
	}
	j = int(positions[pi])
	c = src[j]
	pi++
	switch uint64(stage2Class[c]) {
	case stage2ccQ:
		prev = 96
		key = 0
		goto fusedArrayComma
	case stage2ccS:
		if uint(nscalars) < uint(len(scalars)) {
			scalars[nscalars] = uint32(j)
			nscalars++
		}
		prev = 112
		key = 0
		goto fusedArrayComma
	case stage2ccA:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 0
		inObj = 0
		prev = 16
		key = 0
		goto fusedArrayValue
	case stage2ccO:
		depth++
		if depth > Stage2MaxDepth {
			bad |= 1
		}
		kinds[uint64(depth)&(Stage2KindsLen-1)] = 8
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
