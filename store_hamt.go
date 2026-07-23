package simdjson

// Persistent hash trie for the mutable Store key directory.
//
// The first 15 hash bits use cache-hot fixed 32-way nodes. Their terminal
// slots keep up to two distinct hashes in a short leaf bucket; only the rarer
// third collision allocates another node. This preserves the small, inlinable
// lookup loop while avoiding the mostly empty deep nodes that a conventional
// fixed HAMT creates around ordinary key counts.
//
// Every mutation path-copies its route. Full-hash collisions remain an
// immutable leaf chain and compare complete keys. A per-Store maphash seed
// makes attacker-selected collision families infeasible.

const (
	storeTrieBits        = 5
	storeKeyBucketShift  = 10 // The third fixed level consumes bits 10..14.
	storeKeyFixedBits    = 15
	storeKeyLeafBucket   = 2
	storeKeyBranchFactor = 1 << storeTrieBits
)

type storeLocation struct {
	chunk uint32
	slot  uint8
}

type storeKeyLeaf struct {
	hash uint64
	key  string
	loc  storeLocation
	next *storeKeyLeaf
}

type storeKeySlot struct {
	child *storeKeyNode
	leaf  *storeKeyLeaf
}

type storeKeyNode struct {
	slots [storeKeyBranchFactor]storeKeySlot
}

func storeKeyLookup(root *storeKeyNode, hash uint64, key string) (storeLocation, bool) {
	remaining := hash
	for root != nil {
		slot := &root.slots[remaining&31]
		for leaf := slot.leaf; leaf != nil; leaf = leaf.next {
			if leaf.hash == hash && leaf.key == key {
				return leaf.loc, true
			}
		}
		root = slot.child
		remaining >>= storeTrieBits
	}
	return storeLocation{}, false
}

// storeKeyInsert returns a new root containing key. The caller has already
// established whether this is an insert or a location replacement.
func storeKeyInsert(root *storeKeyNode, hash uint64, key string, loc storeLocation) *storeKeyNode {
	return storeKeyInsertAt(root, 0, &storeKeyLeaf{hash: hash, key: key, loc: loc})
}

func storeKeyInsertAt(root *storeKeyNode, shift uint, add *storeKeyLeaf) *storeKeyNode {
	var out storeKeyNode
	if root != nil {
		out = *root
	}
	i := (add.hash >> shift) & 31
	slot := out.slots[i]
	if slot.child != nil {
		slot.child = storeKeyInsertAt(slot.child, shift+storeTrieBits, add)
		out.slots[i] = slot
		return &out
	}
	if slot.leaf == nil {
		slot.leaf = add
		out.slots[i] = slot
		return &out
	}

	// Complete-hash collisions stay in one leaf chain. Copying the chain also
	// permits an existing key's location to change without mutating a snapshot.
	if storeKeyLeafHasHash(slot.leaf, add.hash) {
		slot.leaf = storeLeafInsert(slot.leaf, add)
		out.slots[i] = slot
		return &out
	}

	// After the cache-hot 15-bit prefix, a two-leaf bucket is cheaper in both
	// bytes and dependent loads than another 512-byte node. Promote only the
	// third distinct hash. The same policy in rare deeper nodes bounds skewed
	// tries without adding a branch to lookup.
	if shift >= storeKeyBucketShift && storeKeyLeafCount(slot.leaf) < storeKeyLeafBucket {
		slot.leaf = storeLeafInsert(slot.leaf, add)
		out.slots[i] = slot
		return &out
	}

	var child *storeKeyNode
	for leaf := slot.leaf; leaf != nil; leaf = leaf.next {
		child = storeKeyInsertAt(child, shift+storeTrieBits, &storeKeyLeaf{
			hash: leaf.hash, key: leaf.key, loc: leaf.loc,
		})
	}
	child = storeKeyInsertAt(child, shift+storeTrieBits, add)
	out.slots[i] = storeKeySlot{child: child}
	return &out
}

func storeLeafInsert(head, add *storeKeyLeaf) *storeKeyLeaf {
	if head == nil {
		return add
	}
	if head.key == add.key {
		return &storeKeyLeaf{hash: add.hash, key: add.key, loc: add.loc, next: head.next}
	}
	return &storeKeyLeaf{
		hash: head.hash, key: head.key, loc: head.loc,
		next: storeLeafInsert(head.next, add),
	}
}

func storeKeyLeafHasHash(leaf *storeKeyLeaf, hash uint64) bool {
	for ; leaf != nil; leaf = leaf.next {
		if leaf.hash == hash {
			return true
		}
	}
	return false
}

func storeKeyLeafCount(leaf *storeKeyLeaf) int {
	n := 0
	for ; leaf != nil && n < storeKeyLeafBucket; leaf = leaf.next {
		n++
	}
	return n
}

func storeKeyDelete(root *storeKeyNode, hash uint64, key string) *storeKeyNode {
	out, _ := storeKeyDeleteAt(root, 0, hash, key)
	return out
}

func storeKeyDeleteAt(root *storeKeyNode, shift uint, hash uint64, key string) (*storeKeyNode, bool) {
	if root == nil {
		return nil, false
	}
	i := (hash >> shift) & 31
	slot := root.slots[i]
	var changed bool
	if slot.child != nil {
		slot.child, changed = storeKeyDeleteAt(slot.child, shift+storeTrieBits, hash, key)
		if !changed {
			return root, false
		}
		if shift >= storeKeyBucketShift {
			if leaf, ok := storeKeyNodeLeafBucket(slot.child); ok {
				slot = storeKeySlot{leaf: leaf}
			}
		} else if leaf, ok := storeKeySingleton(slot.child); ok {
			slot = storeKeySlot{leaf: leaf}
		}
	} else {
		slot.leaf, changed = storeLeafDelete(slot.leaf, hash, key)
		if !changed {
			return root, false
		}
	}
	out := *root
	out.slots[i] = slot
	if storeKeyNodeEmpty(&out) {
		return nil, true
	}
	return &out, true
}

func storeLeafDelete(head *storeKeyLeaf, hash uint64, key string) (*storeKeyLeaf, bool) {
	if head == nil {
		return nil, false
	}
	if head.hash == hash && head.key == key {
		return head.next, true
	}
	next, changed := storeLeafDelete(head.next, hash, key)
	if !changed {
		return head, false
	}
	return &storeKeyLeaf{hash: head.hash, key: head.key, loc: head.loc, next: next}, true
}

func storeKeyNodeEmpty(node *storeKeyNode) bool {
	for i := range node.slots {
		if node.slots[i].child != nil || node.slots[i].leaf != nil {
			return false
		}
	}
	return true
}

// storeKeySingleton permits deletion to collapse a child with one occupied
// leaf slot back into its parent. A full-hash collision chain is one leaf slot
// and can be shared there unchanged.
func storeKeySingleton(node *storeKeyNode) (*storeKeyLeaf, bool) {
	if node == nil {
		return nil, false
	}
	var one *storeKeyLeaf
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

// storeKeyNodeLeafBucket flattens a promoted subtree as soon as deletion
// brings it back within the leaf-bucket bound. Counting uses a fixed stack
// buffer, so failed collapse checks do not allocate during churn.
func storeKeyNodeLeafBucket(node *storeKeyNode) (*storeKeyLeaf, bool) {
	if node == nil {
		return nil, false
	}
	var leaves [storeKeyLeafBucket]*storeKeyLeaf
	n := 0
	if !storeKeyCollectLeaves(node, &leaves, &n) || n == 0 {
		return nil, false
	}
	var head *storeKeyLeaf
	for i := n - 1; i >= 0; i-- {
		leaf := leaves[i]
		head = &storeKeyLeaf{hash: leaf.hash, key: leaf.key, loc: leaf.loc, next: head}
	}
	return head, true
}

func storeKeyCollectLeaves(node *storeKeyNode, leaves *[storeKeyLeafBucket]*storeKeyLeaf, n *int) bool {
	if node == nil {
		return true
	}
	for i := range node.slots {
		slot := node.slots[i]
		if slot.child != nil && !storeKeyCollectLeaves(slot.child, leaves, n) {
			return false
		}
		for leaf := slot.leaf; leaf != nil; leaf = leaf.next {
			if *n == len(leaves) {
				return false
			}
			leaves[*n] = leaf
			(*n)++
		}
	}
	return true
}
