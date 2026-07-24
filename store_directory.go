package vibejson

import "github.com/thesyncim/vibejson/internal/storekey"

// storeLocation is the exact pointer-free row address retained by the key
// directory. An alias, rather than a mirror plus conversions, keeps lookup and
// mutation calls eligible for the same inlining and register ABI as before the
// directory moved out of this package.
type storeLocation = storekey.Location

type storeKeyNode = storekey.Node

func storeKeyLookup(root *storeKeyNode, hash uint64, key string) (storeLocation, bool) {
	return storekey.Lookup(root, hash, key)
}

func storeKeyInsert(root *storeKeyNode, hash uint64, key string, loc storeLocation) *storeKeyNode {
	return storekey.Insert(root, hash, key, loc)
}

func storeKeyDelete(root *storeKeyNode, hash uint64, key string) *storeKeyNode {
	return storekey.Delete(root, hash, key)
}
