package simdjson

// Persistent bitmap postings for declared Store indexes.
//
// Store slots are stable for the lifetime of a key and a chunk holds at most
// 64 slots, so one uint64 is the native posting unit. A posting starts inline
// for the overwhelmingly common one-to-four-chunk case. Wider postings promote
// to a sparse persistent radix vector: an update path-copies only the nodes
// leading to one chunk word, old snapshots keep their words, and an absent
// address range costs no leaf storage. Shrinking postings demote immediately.

const storeIndexInlineMasks = 4

type storeIndexChunkMask struct {
	chunk uint32
	mask  uint64
}

type storeIndexMasks struct {
	// Split ids from words to avoid the four bytes of alignment padding that
	// an array-of-structs would pay for every inline entry.
	chunks [storeIndexInlineMasks]uint32
	masks  [storeIndexInlineMasks]uint64
	n      uint8
	wide   storeIndexMaskVector
}

func (m storeIndexMasks) get(chunk uint32) uint64 {
	if m.wide.root != nil {
		return m.wide.get(chunk)
	}
	for i := 0; i < int(m.n); i++ {
		if m.chunks[i] == chunk {
			return m.masks[i]
		}
	}
	return 0
}

func (m storeIndexMasks) set(chunk uint32, mask uint64) storeIndexMasks {
	if m.wide.root != nil {
		m.wide = m.wide.set(chunk, mask)
		if m.wide.words <= storeIndexInlineMasks {
			var compact storeIndexMasks
			m.wide.each(func(id uint32, word uint64) bool {
				compact = compact.set(id, word)
				return true
			})
			return compact
		}
		return m
	}
	for i := 0; i < int(m.n); i++ {
		if m.chunks[i] != chunk {
			continue
		}
		if mask == 0 {
			copy(m.chunks[i:], m.chunks[i+1:int(m.n)])
			copy(m.masks[i:], m.masks[i+1:int(m.n)])
			m.n--
			m.chunks[m.n] = 0
			m.masks[m.n] = 0
		} else {
			m.masks[i] = mask
		}
		return m
	}
	if mask == 0 {
		return m
	}
	if m.n < storeIndexInlineMasks {
		i := int(m.n)
		for i > 0 && m.chunks[i-1] > chunk {
			m.chunks[i] = m.chunks[i-1]
			m.masks[i] = m.masks[i-1]
			i--
		}
		m.chunks[i] = chunk
		m.masks[i] = mask
		m.n++
		return m
	}
	for i := 0; i < int(m.n); i++ {
		m.wide = m.wide.set(m.chunks[i], m.masks[i])
	}
	m.chunks = [storeIndexInlineMasks]uint32{}
	m.masks = [storeIndexInlineMasks]uint64{}
	m.n = 0
	m.wide = m.wide.set(chunk, mask)
	return m
}

func (m storeIndexMasks) empty() bool {
	return m.n == 0 && m.wide.words == 0
}

func (m storeIndexMasks) each(fn func(uint32, uint64) bool) {
	if m.wide.root != nil {
		m.wide.each(fn)
		return
	}
	for i := 0; i < int(m.n); i++ {
		if !fn(m.chunks[i], m.masks[i]) {
			return
		}
	}
}

func (m storeIndexMasks) next(from uint64) (uint32, uint64, bool) {
	if from > uint64(^uint32(0)) {
		return 0, 0, false
	}
	if m.wide.root != nil {
		return m.wide.next(from)
	}
	for i := 0; i < int(m.n); i++ {
		if uint64(m.chunks[i]) >= from {
			return m.chunks[i], m.masks[i], true
		}
	}
	return 0, 0, false
}

// storeIndexMasksFromSorted builds one immutable posting without the
// intermediate path copies used by online single-word updates. entries must be
// strictly ascending by chunk and contain no zero masks.
func storeIndexMasksFromSorted(entries []storeIndexChunkMask) storeIndexMasks {
	if len(entries) <= storeIndexInlineMasks {
		var out storeIndexMasks
		out.n = uint8(len(entries))
		for i, entry := range entries {
			out.chunks[i], out.masks[i] = entry.chunk, entry.mask
		}
		return out
	}
	maxID := entries[len(entries)-1].chunk
	var depth uint8
	for uint64(maxID) >= storeIndexMaskCapacity(depth) {
		depth++
	}
	return storeIndexMasks{wide: storeIndexMaskVector{
		root:  storeIndexMaskBuild(entries, depth),
		depth: depth,
		words: uint32(len(entries)),
	}}
}

// storeIndexMaskNode is a level-tagged persistent radix node. Only leaves use
// masks and only branches use children. Keeping both arrays in one concrete
// type avoids interfaces and indirect type dispatch on lookup; the maximum
// path is seven nodes for a uint32 chunk id.
type storeIndexMaskNode struct {
	children [32]*storeIndexMaskNode
	masks    [32]uint64
}

type storeIndexMaskVector struct {
	root  *storeIndexMaskNode
	depth uint8
	words uint32
}

func (v storeIndexMaskVector) get(id uint32) uint64 {
	if v.root == nil || uint64(id) >= storeIndexMaskCapacity(v.depth) {
		return 0
	}
	node := v.root
	for level := v.depth; level > 0; level-- {
		node = node.children[(id>>(uint(level)*5))&31]
		if node == nil {
			return 0
		}
	}
	return node.masks[id&31]
}

func (v storeIndexMaskVector) set(id uint32, mask uint64) storeIndexMaskVector {
	old := v.get(id)
	if old == mask {
		return v
	}
	for uint64(id) >= storeIndexMaskCapacity(v.depth) {
		v.root = &storeIndexMaskNode{children: [32]*storeIndexMaskNode{v.root}}
		v.depth++
	}
	v.root = storeIndexMaskSet(v.root, v.depth, id, mask)
	if old == 0 {
		v.words++
	} else if mask == 0 {
		v.words--
	}
	if v.words == 0 {
		return storeIndexMaskVector{}
	}
	for v.depth > 0 && storeIndexMaskOnlyFirstChild(v.root) {
		v.root = v.root.children[0]
		v.depth--
	}
	return v
}

func storeIndexMaskOnlyFirstChild(node *storeIndexMaskNode) bool {
	if node == nil || node.children[0] == nil {
		return false
	}
	for _, child := range node.children[1:] {
		if child != nil {
			return false
		}
	}
	return true
}

func storeIndexMaskCapacity(depth uint8) uint64 {
	return uint64(32) << (uint(depth) * 5)
}

func storeIndexMaskSet(node *storeIndexMaskNode, level uint8, id uint32, mask uint64) *storeIndexMaskNode {
	var out storeIndexMaskNode
	if node != nil {
		out = *node
	}
	if level == 0 {
		out.masks[id&31] = mask
	} else {
		i := (id >> (uint(level) * 5)) & 31
		out.children[i] = storeIndexMaskSet(out.children[i], level-1, id, mask)
	}
	if mask == 0 && storeIndexMaskNodeEmpty(&out, level) {
		return nil
	}
	return &out
}

func storeIndexMaskBuild(entries []storeIndexChunkMask, level uint8) *storeIndexMaskNode {
	if len(entries) == 0 {
		return nil
	}
	node := new(storeIndexMaskNode)
	if level == 0 {
		for _, entry := range entries {
			node.masks[entry.chunk&31] = entry.mask
		}
		return node
	}
	shift := uint(level) * 5
	for first := 0; first < len(entries); {
		i := (entries[first].chunk >> shift) & 31
		last := first + 1
		for last < len(entries) && (entries[last].chunk>>shift)&31 == i {
			last++
		}
		node.children[i] = storeIndexMaskBuild(entries[first:last], level-1)
		first = last
	}
	return node
}

func storeIndexMaskNodeEmpty(node *storeIndexMaskNode, level uint8) bool {
	if level == 0 {
		for _, mask := range node.masks {
			if mask != 0 {
				return false
			}
		}
		return true
	}
	for _, child := range node.children {
		if child != nil {
			return false
		}
	}
	return true
}

func (v storeIndexMaskVector) each(fn func(uint32, uint64) bool) {
	storeIndexMaskEach(v.root, v.depth, 0, fn)
}

func (v storeIndexMaskVector) next(from uint64) (uint32, uint64, bool) {
	if v.root == nil || from > uint64(^uint32(0)) || from >= storeIndexMaskCapacity(v.depth) {
		return 0, 0, false
	}
	return storeIndexMaskNext(v.root, v.depth, 0, from)
}

func storeIndexMaskNext(node *storeIndexMaskNode, level uint8, prefix uint64, from uint64) (uint32, uint64, bool) {
	if node == nil {
		return 0, 0, false
	}
	if level == 0 {
		start := 0
		if from > prefix {
			start = int(from - prefix)
			if start >= len(node.masks) {
				return 0, 0, false
			}
		}
		for i := start; i < len(node.masks); i++ {
			if node.masks[i] != 0 {
				return uint32(prefix + uint64(i)), node.masks[i], true
			}
		}
		return 0, 0, false
	}
	shift := uint(level) * 5
	start := 0
	if from > prefix {
		start = int((from - prefix) >> shift)
		if start >= len(node.children) {
			return 0, 0, false
		}
	}
	for i := start; i < len(node.children); i++ {
		childPrefix := prefix + uint64(i)<<shift
		childFrom := max(from, childPrefix)
		if id, mask, ok := storeIndexMaskNext(node.children[i], level-1, childPrefix, childFrom); ok {
			return id, mask, true
		}
	}
	return 0, 0, false
}

func storeIndexMaskEach(node *storeIndexMaskNode, level uint8, prefix uint32, fn func(uint32, uint64) bool) bool {
	if node == nil {
		return true
	}
	if level == 0 {
		for i, mask := range node.masks {
			if mask != 0 && !fn(prefix|uint32(i), mask) {
				return false
			}
		}
		return true
	}
	shift := uint(level) * 5
	for i, child := range node.children {
		if child != nil && !storeIndexMaskEach(child, level-1, prefix|uint32(i)<<shift, fn) {
			return false
		}
	}
	return true
}

// A separate persistent HAMT maps a composite scalar fingerprint to its
// posting. Fingerprints are candidate routing only: a full-hash collision
// deliberately shares one posting and the lookup's exact JSON recheck removes
// false positives. That removes stored composite-key copies without weakening
// correctness.
type storeIndexPostingLeaf struct {
	hash  uint64
	masks storeIndexMasks
}

type storeIndexPostingSlot struct {
	child *storeIndexPostingNode
	leaf  *storeIndexPostingLeaf
}

type storeIndexPostingNode struct {
	slots [1 << storeTrieBits]storeIndexPostingSlot
}

func storeIndexPostingLookup(root *storeIndexPostingNode, hash uint64) (storeIndexMasks, bool) {
	for shift := uint(0); root != nil; shift += storeTrieBits {
		slot := root.slots[(hash>>shift)&31]
		if slot.leaf != nil {
			if slot.leaf.hash == hash {
				return slot.leaf.masks, true
			}
			return storeIndexMasks{}, false
		}
		root = slot.child
	}
	return storeIndexMasks{}, false
}

func storeIndexPostingSet(root *storeIndexPostingNode, hash uint64, chunk uint32, bit uint64, present bool) *storeIndexPostingNode {
	return storeIndexPostingSetMask(root, hash, chunk, bit, present)
}

func storeIndexPostingSetMask(root *storeIndexPostingNode, hash uint64, chunk uint32, change uint64, present bool) *storeIndexPostingNode {
	masks, _ := storeIndexPostingLookup(root, hash)
	word := masks.get(chunk)
	if present {
		word |= change
	} else {
		word &^= change
	}
	masks = masks.set(chunk, word)
	if masks.empty() {
		return storeIndexPostingDelete(root, 0, hash)
	}
	return storeIndexPostingInsert(root, 0, &storeIndexPostingLeaf{hash: hash, masks: masks})
}

// storeIndexPostingBuild creates a complete directory from unique leaves in
// low-radix-first order. It allocates each reachable HAMT node exactly once.
func storeIndexPostingBuild(leaves []*storeIndexPostingLeaf, shift uint) *storeIndexPostingNode {
	if len(leaves) == 0 {
		return nil
	}
	node := new(storeIndexPostingNode)
	for first := 0; first < len(leaves); {
		i := (leaves[first].hash >> shift) & 31
		last := first + 1
		for last < len(leaves) && (leaves[last].hash>>shift)&31 == i {
			last++
		}
		if last-first == 1 {
			node.slots[i].leaf = leaves[first]
		} else {
			node.slots[i].child = storeIndexPostingBuild(leaves[first:last], shift+storeTrieBits)
		}
		first = last
	}
	return node
}

func storeIndexPostingInsert(root *storeIndexPostingNode, shift uint, add *storeIndexPostingLeaf) *storeIndexPostingNode {
	var out storeIndexPostingNode
	if root != nil {
		out = *root
	}
	i := (add.hash >> shift) & 31
	slot := out.slots[i]
	if slot.child != nil {
		slot.child = storeIndexPostingInsert(slot.child, shift+storeTrieBits, add)
		out.slots[i] = slot
		return &out
	}
	if slot.leaf == nil || slot.leaf.hash == add.hash {
		slot.leaf = add
		out.slots[i] = slot
		return &out
	}
	child := storeIndexPostingInsert(nil, shift+storeTrieBits, slot.leaf)
	child = storeIndexPostingInsert(child, shift+storeTrieBits, add)
	out.slots[i] = storeIndexPostingSlot{child: child}
	return &out
}

func storeIndexPostingDelete(root *storeIndexPostingNode, shift uint, hash uint64) *storeIndexPostingNode {
	if root == nil {
		return nil
	}
	i := (hash >> shift) & 31
	slot := root.slots[i]
	if slot.child != nil {
		next := storeIndexPostingDelete(slot.child, shift+storeTrieBits, hash)
		if next == slot.child {
			return root
		}
		slot.child = next
		if leaf, ok := storeIndexPostingSingleton(next); ok {
			slot = storeIndexPostingSlot{leaf: leaf}
		}
	} else {
		if slot.leaf == nil || slot.leaf.hash != hash {
			return root
		}
		slot.leaf = nil
	}
	out := *root
	out.slots[i] = slot
	if storeIndexPostingNodeEmpty(&out) {
		return nil
	}
	return &out
}

func storeIndexPostingNodeEmpty(node *storeIndexPostingNode) bool {
	for i := range node.slots {
		if node.slots[i].child != nil || node.slots[i].leaf != nil {
			return false
		}
	}
	return true
}

func storeIndexPostingSingleton(node *storeIndexPostingNode) (*storeIndexPostingLeaf, bool) {
	if node == nil {
		return nil, false
	}
	var one *storeIndexPostingLeaf
	for i := range node.slots {
		slot := node.slots[i]
		if slot.child != nil {
			return nil, false
		}
		if slot.leaf != nil {
			if one != nil {
				return nil, false
			}
			one = slot.leaf
		}
	}
	return one, one != nil
}
