package simdjson

import (
	"bytes"
	"math/bits"

	"github.com/thesyncim/simdjson/internal/byteview"
)

// ObjectProbe is a constant-time member lookup table over one object, built
// with [BuildObjectProbe] and queried with [ObjectProbe.Get]. [Node.Get]
// scans members linearly, so a wide object that receives many lookups pays
// its width on every query; a probe pays one build pass and then answers
// hits and misses alike in constant expected time regardless of width.
//
// The probe is open addressing over the tape:
//
//	table    +------------+------------+------------+--
//	         | hash, off  |  0 (empty) | hash, off  | ...  power-of-two slots,
//	         +------------+------------+------------+--    linear probing,
//	escaped  [ off, off, ... ]  document order             at most half full
//
// A slot's off is the key entry's offset from the object header, so a slot
// stores tape coordinates — eight bytes, no key copies, no pointers — and
// the zero value doubles as the empty marker: offset zero is the header
// itself, never a key. Escaped keys overflow into the side list because
// their stored hash covers the raw spelling, which a decoded query cannot be
// compared against; Get scans those few with the decoding comparison after
// the probe. Offsets are also document order, which is what lets Get merge
// the two candidate sources with a single max (the duplicate rule below).
//
// A probe borrows the Node's source and index and aliases the storage passed
// to BuildObjectProbe; none of the three may be modified or reused while the
// probe is in use. Concurrent Gets are safe while all three remain immutable.
// The zero ObjectProbe resolves nothing.
type ObjectProbe struct {
	src    *byte
	header *IndexEntry
	// table is the open-addressed hash table: a power-of-two slot count with
	// linear probing, each occupied slot holding one key's content hash and
	// its entry offset from the object header. Offset zero — the header
	// itself, never a key — marks an empty slot. Only unescaped keys are
	// inserted, and among byte-identical duplicates the table keeps the later
	// member (see probeInsert).
	table []ProbeSlot
	// escaped lists the escaped-key members in document order, offset word
	// only. An escaped key's stored hash covers its raw spelling, which a
	// decoded query cannot be compared against, so Get scans these few with
	// the decoding comparison after the probe.
	escaped []ProbeSlot
	mask    uint32
}

// ProbeSlot is one slot of [ObjectProbe] storage. Its fields are private so
// callers can provide reusable storage without being coupled to the layout.
type ProbeSlot struct {
	hash uint32
	off  uint32
}

// probeCapacity returns the table slot count for an object of count members:
// the smallest power of two keeping load at or below one half, so probe
// chains stay short and an absent key terminates at an empty slot after a
// constant expected number of steps.
func probeCapacity(count int) int {
	return 1 << bits.Len(uint(2*count-1))
}

// RequiredProbeSlots returns the exact storage length [BuildObjectProbe]
// needs for v: the hash table plus one slot per escaped key. A non-object or
// an empty object needs none and returns zero.
func RequiredProbeSlots(v Node) int {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return 0
	}
	escaped := 0
	keyEntry := tapeEntryOffset(v.entry, 1)
	for member := 0; member < count; member++ {
		if keyEntry.flags()&tapeFlagEscaped != 0 {
			escaped++
		}
		if member+1 < count {
			valueEntry := tapeEntryOffset(keyEntry, 1)
			keyEntry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
		}
	}
	return probeCapacity(count) + escaped
}

// BuildObjectProbe builds a member lookup table for object v in caller-owned
// storage, of which only the capacity is used. It reports false when v is not
// an object. The returned probe aliases storage; [RequiredProbeSlots] sizes
// it exactly, sufficient storage makes the build allocation-free, and
// undersized storage is replaced by one allocation. Precomputed key hashes
// (document.IndexOptions.HashKeys) speed the build but are not required.
func BuildObjectProbe(v Node, storage []ProbeSlot) (ObjectProbe, bool) {
	count, ok := v.ObjectLen()
	if !ok {
		return ObjectProbe{}, false
	}
	if count == 0 {
		return ObjectProbe{src: v.src, header: v.entry}, true
	}
	capacity := probeCapacity(count)
	if cap(storage) < capacity {
		// One allocation covering the worst case of every key escaped.
		storage = make([]ProbeSlot, 0, capacity+count)
	}
	table := storage[:capacity]
	clear(table)
	escaped := storage[capacity:capacity]
	mask := uint32(capacity - 1)
	hashed := v.entry.keysHashed()
	keyEntry := tapeEntryOffset(v.entry, 1)
	off := uint32(1)
	for member := 0; member < count; member++ {
		if keyEntry.flags()&tapeFlagEscaped != 0 {
			escaped = append(escaped, ProbeSlot{off: off})
		} else {
			hash := keyEntry.next
			if !hashed {
				hash = hashKeyContent(byteview.SliceRange(v.src, keyEntry.start+1, keyEntry.end-1))
			}
			probeInsert(table, mask, v.src, v.entry, hash, off)
		}
		if member+1 < count {
			valueEntry := tapeEntryOffset(keyEntry, 1)
			keyEntry = tapeEntryOffset(valueEntry, uintptr(valueEntry.next))
			off += 1 + valueEntry.next
		}
	}
	return ObjectProbe{src: v.src, header: v.entry, table: table, escaped: escaped, mask: mask}, true
}

// probeInsert claims the first free slot in the key's chain, or overwrites
// the chain's byte-identical earlier duplicate so the later member wins, the
// [Node.Get] duplicate rule. The table is at most half full, so a free slot
// always exists and the loop terminates.
func probeInsert(table []ProbeSlot, mask uint32, src *byte, header *IndexEntry, hash, off uint32) {
	idx := hash & mask
	for {
		slot := &table[idx]
		if slot.off == 0 {
			*slot = ProbeSlot{hash: hash, off: off}
			return
		}
		if slot.hash == hash {
			prev := tapeEntryOffset(header, uintptr(slot.off))
			cur := tapeEntryOffset(header, uintptr(off))
			if bytes.Equal(byteview.SliceRange(src, prev.start+1, prev.end-1),
				byteview.SliceRange(src, cur.start+1, cur.end-1)) {
				slot.off = off
				return
			}
		}
		idx = (idx + 1) & mask
	}
}

// Get returns the last object member with key, exactly as [Node.Get] on the
// probed node would, in constant expected time. An absent key or the zero
// probe returns a zero Node and false. Get does not allocate.
func (p *ObjectProbe) Get(key string) (Node, bool) {
	if p.header == nil {
		return Node{}, false
	}
	hash := hashKeyString(key)
	// best is the winning member's key-entry offset. The tape is in document
	// order, so the larger of the probe's match and the last escaped match is
	// the later member, preserving the last-duplicate rule across the split.
	best := uint32(0)
	if table := p.table; len(table) != 0 {
		idx := hash & p.mask
		for {
			slot := table[idx]
			if slot.off == 0 {
				break
			}
			if slot.hash == hash {
				keyEntry := tapeEntryOffset(p.header, uintptr(slot.off))
				if bytesEqualString(byteview.SliceRange(p.src, keyEntry.start+1, keyEntry.end-1), key) {
					best = slot.off
					break
				}
			}
			idx = (idx + 1) & p.mask
		}
	}
	for i := range p.escaped {
		off := p.escaped[i].off
		// Escaped members at or before the current winner cannot be later in
		// document order, so only offsets past it need the decoding compare.
		if off > best {
			keyEntry := tapeEntryOffset(p.header, uintptr(off))
			if tapeKeyEqual(byteview.SliceRange(p.src, keyEntry.start, keyEntry.end), keyEntry.flags(), key) {
				best = off
			}
		}
	}
	if best == 0 {
		return Node{}, false
	}
	return Node{src: p.src, entry: tapeEntryOffset(p.header, uintptr(best)+1)}, true
}
