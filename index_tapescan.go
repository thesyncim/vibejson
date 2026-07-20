package simdjson

import (
	"github.com/thesyncim/simdjson/document"
	"github.com/thesyncim/simdjson/internal/byteview"
)

// Vectorized navigation over the tape itself. A flat, key-hash-enriched
// object (see enrichKeyHashes) lays its key entries at a fixed two-entry
// stride from the header, each carrying its content hash in the next word and
// its flags in the info word. That regular layout admits a scan that tests
// several members per loop iteration against one query hash and byte-verifies
// only the rare hash hits, instead of walking members one at a time. The
// span-chased (non-flat) lookup keeps its scalar loop: its stride is
// irregular, so there is nothing to unroll against.

// tapeScanFlatHash resolves key inside one flat, key-hash-enriched object:
// header.next is 2*count+1, every key entry's next word holds its content
// hash, and count is at least 1. It returns the value entry of the same
// member Get would return — the last member whose key byte-matches — or nil
// when the key is absent.
//
// The scan runs backward with early exit: candidates are byte-verified from
// the last member toward the first, and the first verified match is exactly
// the last-duplicate-wins winner, so a hit inspects only the members past it.
// Members are tested four per iteration. A member is a candidate when its
// stored hash equals the query hash or its key is escaped — an escaped key's
// stored hash covers the raw spelling, which a decoded query cannot be
// compared against, so it always falls through to the byte comparison,
// exactly as getHashed treats it. True hash matches and collisions alike are
// candidates, and every candidate is byte-verified, so the hash stays a pure
// pre-filter. Candidates are rare, so the combined test almost always falls
// through in one branch per four members.
//
// Bounds: the scan touches key entries 1, 3, ..., 2*count-1 from the header
// and returns value entries at most 2*count from it, all inside the object's
// own span on the tape; it never reads past the object's extent.
func tapeScanFlatHash(src *byte, header *IndexEntry, count int, key string, queryHash uint32) *IndexEntry {
	const escapedInfo = uint32(tapeFlagEscaped) << infoFlagsShift
	m := count
	for m >= 4 {
		k3 := tapeEntryOffset(header, uintptr(2*m)-1)
		k2 := tapeEntryOffset(header, uintptr(2*m)-3)
		k1 := tapeEntryOffset(header, uintptr(2*m)-5)
		k0 := tapeEntryOffset(header, uintptr(2*m)-7)
		if k3.next == queryHash || k2.next == queryHash ||
			k1.next == queryHash || k0.next == queryHash ||
			(k3.info|k2.info|k1.info|k0.info)&escapedInfo != 0 {
			if value := tapeVerifyFlatRange(src, header, m-1, m-4, key, queryHash); value != nil {
				return value
			}
		}
		m -= 4
	}
	if m > 0 {
		return tapeVerifyFlatRange(src, header, m-1, 0, key, queryHash)
	}
	return nil
}

// tapeVerifyFlatRange byte-verifies the candidate members from hi down to lo,
// inclusive, and returns the first verified match's value entry, or nil. It
// is the scalar resolve behind tapeScanFlatHash's combined test; descending
// order preserves the last-duplicate-wins rule.
func tapeVerifyFlatRange(src *byte, header *IndexEntry, hi, lo int, key string, queryHash uint32) *IndexEntry {
	for member := hi; member >= lo; member-- {
		keyEntry := tapeEntryOffset(header, uintptr(2*member)+1)
		flags := keyEntry.flags()
		if flags&tapeFlagEscaped == 0 && keyEntry.next != queryHash {
			continue
		}
		if tapeKeyEqual(byteview.SliceRange(src, keyEntry.start, keyEntry.end), flags, key) {
			return tapeEntryOffset(keyEntry, 1)
		}
	}
	return nil
}

// AppendColumn appends one RawValue per element of array v to dst: the value
// of the element's member key when the element is an object containing it —
// the last such member, exactly as [Node.Get] resolves — and the zero
// RawValue when the element is not an object or has no such member, so
// dst[i] stays aligned with element i, the [DocSet.AppendPointer] absence
// convention. It returns the extended slice and reports false, with dst
// unchanged, when v is not an array.
//
// The gather is one forward pass over the array's entries. On a key-hash
// enriched index (document.IndexOptions.HashKeys) with flat object elements —
// every member value a scalar — each element resolves by the vectorized tape
// scan with the key hashed once for the whole column; irregular elements fall
// back to the per-element lookup. Appended values borrow v's document under
// the usual RawValue lifetime rules.
func (v Node) AppendColumn(dst []RawValue, key string) ([]RawValue, bool) {
	count, ok := v.ArrayLen()
	if !ok {
		return dst, false
	}
	queryHash := hashKeyString(key)
	src := v.src
	if count == 0 {
		return dst, true
	}
	elem := tapeEntryOffset(v.entry, 1)
	for i := 0; ; i++ {
		dst = append(dst, columnValue(src, elem, key, queryHash))
		if i+1 == count {
			return dst, true
		}
		// Advance only between elements: stepping past the last one could
		// form a pointer beyond the tape.
		elem = tapeEntryOffset(elem, uintptr(elem.next))
	}
}

// columnValue resolves one column element: the raw value of the last member
// of object element entry whose key matches, or the zero RawValue. It routes
// a flat enriched object to the tape scan and anything irregular to the exact
// per-element lookups.
func columnValue(src *byte, entry *IndexEntry, key string, queryHash uint32) RawValue {
	if entry.Kind() != document.Object {
		return RawValue{}
	}
	memberCount := int(entry.Count())
	if memberCount == 0 {
		return RawValue{}
	}
	if entry.keysHashed() {
		if entry.next == 2*uint32(memberCount)+1 {
			if value := tapeScanFlatHash(src, entry, memberCount, key, queryHash); value != nil {
				return RawValue{src: byteview.SliceRange(src, value.start, value.end)}
			}
			return RawValue{}
		}
		if value, found := (Node{src: src, entry: entry}).getHashedQuery(key, queryHash, memberCount); found {
			return value.Raw()
		}
		return RawValue{}
	}
	if value, found := (Node{src: src, entry: entry}).Get(key); found {
		return value.Raw()
	}
	return RawValue{}
}
