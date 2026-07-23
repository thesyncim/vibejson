package slopjson

// Persistent 32-way radix vector for immutable document chunks. Updating one
// chunk path-copies O(log32(chunks)) fixed nodes; appending grows the root by
// one level only when its address space is full. There is no corpus-wide chunk
// pointer slice to copy and no relocation or compaction event.

type storeChunkNode struct {
	children [32]*storeChunkNode
	leaves   [32]*storeChunk
}

type storeChunkVector struct {
	root  *storeChunkNode
	depth uint8 // zero means root.leaves; each higher level consumes five bits
	count uint32
}

func (v storeChunkVector) get(id uint32) *storeChunk {
	if id >= v.count || v.root == nil {
		return nil
	}
	node := v.root
	for level := v.depth; level > 0; level-- {
		node = node.children[(id>>(uint(level)*5))&31]
		if node == nil {
			return nil
		}
	}
	return node.leaves[id&31]
}

func (v storeChunkVector) set(id uint32, chunk *storeChunk) storeChunkVector {
	if id >= v.count {
		panic("slopjson: store chunk vector index out of range")
	}
	v.root = storeChunkSet(v.root, v.depth, id, chunk)
	return v
}

func storeChunkSet(node *storeChunkNode, level uint8, id uint32, chunk *storeChunk) *storeChunkNode {
	var out storeChunkNode
	if node != nil {
		out = *node
	}
	if level == 0 {
		out.leaves[id&31] = chunk
		return &out
	}
	i := (id >> (uint(level) * 5)) & 31
	out.children[i] = storeChunkSet(out.children[i], level-1, id, chunk)
	return &out
}

func (v storeChunkVector) append(chunk *storeChunk) (storeChunkVector, uint32) {
	id := v.count
	capacity := uint64(32) << (uint(v.depth) * 5)
	if uint64(v.count) == capacity {
		v.root = &storeChunkNode{children: [32]*storeChunkNode{v.root}}
		v.depth++
	}
	v.count++
	v.root = storeChunkSet(v.root, v.depth, id, chunk)
	return v, id
}

// appendTransient is StoreBuilder's uniquely-owned append. It creates only
// missing radix nodes and mutates existing ones in place; after Build publishes
// the vector, ordinary Store updates use the persistent append/set methods.
func (v *storeChunkVector) appendTransient(chunk *storeChunk) uint32 {
	id := v.count
	capacity := uint64(32) << (uint(v.depth) * 5)
	if uint64(v.count) == capacity {
		v.root = &storeChunkNode{children: [32]*storeChunkNode{v.root}}
		v.depth++
	}
	v.count++
	storeChunkSetTransient(&v.root, v.depth, id, chunk)
	return id
}

func storeChunkSetTransient(node **storeChunkNode, level uint8, id uint32, chunk *storeChunk) {
	if *node == nil {
		*node = &storeChunkNode{}
	}
	if level == 0 {
		(*node).leaves[id&31] = chunk
		return
	}
	i := (id >> (uint(level) * 5)) & 31
	storeChunkSetTransient(&(*node).children[i], level-1, id, chunk)
}

func (v storeChunkVector) each(fn func(uint32, *storeChunk) bool) {
	storeChunkEach(v.root, v.depth, 0, v.count, fn)
}

// next returns the first materialized chunk whose id is at least from. It
// searches radix children rather than integer ids, so advancing an online
// maintenance cursor never walks a deleted high-water gap. from is uint64 so
// count can be represented as the terminal cursor even at the uint32 limit.
func (v storeChunkVector) next(from uint64) (uint32, *storeChunk, bool) {
	if v.root == nil || from >= uint64(v.count) {
		return 0, nil, false
	}
	return storeChunkNext(v.root, v.depth, 0, uint64(v.count), from)
}

func storeChunkNext(node *storeChunkNode, level uint8, prefix, count, from uint64) (uint32, *storeChunk, bool) {
	if node == nil || prefix >= count {
		return 0, nil, false
	}
	if level == 0 {
		start := 0
		if from > prefix {
			delta := from - prefix
			if delta >= uint64(len(node.leaves)) {
				return 0, nil, false
			}
			start = int(delta)
		}
		for i := start; i < len(node.leaves); i++ {
			id := prefix + uint64(i)
			if id >= count {
				break
			}
			if chunk := node.leaves[i]; chunk != nil {
				return uint32(id), chunk, true
			}
		}
		return 0, nil, false
	}

	shift := uint(level) * 5
	start := 0
	if from > prefix {
		start = int((from - prefix) >> shift)
		if start >= len(node.children) {
			return 0, nil, false
		}
	}
	for i := start; i < len(node.children); i++ {
		childPrefix := prefix + uint64(i)<<shift
		if childPrefix >= count {
			break
		}
		childFrom := max(from, childPrefix)
		if id, chunk, ok := storeChunkNext(node.children[i], level-1, childPrefix, count, childFrom); ok {
			return id, chunk, true
		}
	}
	return 0, nil, false
}

// storeChunkEach descends only materialized branches. Empty chunks therefore
// never turn a delete-heavy scan into an O(historical high-water mark) walk;
// no compaction pass is needed to recover read performance.
func storeChunkEach(node *storeChunkNode, level uint8, prefix, count uint32, fn func(uint32, *storeChunk) bool) bool {
	if node == nil {
		return true
	}
	if level == 0 {
		for i, chunk := range node.leaves {
			id := prefix | uint32(i)
			if id >= count {
				return true
			}
			if chunk != nil && !fn(id, chunk) {
				return false
			}
		}
		return true
	}
	shift := uint(level) * 5
	for i, child := range node.children {
		if child == nil {
			continue
		}
		childPrefix := prefix | uint32(i)<<shift
		if childPrefix >= count {
			return true
		}
		if !storeChunkEach(child, level-1, childPrefix, count, fn) {
			return false
		}
	}
	return true
}
