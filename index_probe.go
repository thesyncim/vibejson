package vibejson

import (
	"bytes"
	"math/bits"

	"github.com/thesyncim/vibejson/x/byteview"
)

// ObjectProbe is an open-addressed lookup table over one object. It borrows
// the source, index, and caller-provided storage. Unescaped keys occupy the
// table; escaped keys use a side list in document order.
type ObjectProbe struct {
	src    *byte
	header *IndexEntry
	// table holds unescaped key hashes and entry offsets.
	table []ProbeSlot
	// escaped lists escaped-key offsets in document order.
	escaped []ProbeSlot
	mask    uint32
}

// ProbeSlot is one slot of [ObjectProbe] storage. Its fields are private so
// callers can provide reusable storage without being coupled to the layout.
type ProbeSlot struct {
	hash uint32
	off  uint32
}

// ProbeCapacity returns the smallest power-of-two table at half load.
func ProbeCapacity(count int) int {
	return 1 << bits.Len(uint(2*count-1))
}

// RequiredProbeSlots returns the table plus escaped-key storage required.
func RequiredProbeSlots(v Node) int {
	count, ok := v.ObjectLen()
	if !ok || count == 0 {
		return 0
	}
	escaped := 0
	keyEntry := EntryAt(v.Entry, 1)
	for member := 0; member < count; member++ {
		if keyEntry.Flags()&TapeFlagEscaped != 0 {
			escaped++
		}
		if member+1 < count {
			valueEntry := EntryAt(keyEntry, 1)
			keyEntry = EntryAt(valueEntry, uintptr(valueEntry.Next))
		}
	}
	return ProbeCapacity(count) + escaped
}

// BuildObjectProbe builds a member lookup table in caller-owned storage.
func BuildObjectProbe(v Node, storage []ProbeSlot) (ObjectProbe, bool) {
	count, ok := v.ObjectLen()
	if !ok {
		return ObjectProbe{}, false
	}
	if count == 0 {
		return ObjectProbe{src: v.Src, header: v.Entry}, true
	}
	capacity := ProbeCapacity(count)
	if cap(storage) < capacity {
		storage = make([]ProbeSlot, 0, capacity+count)
	}
	table := storage[:capacity]
	clear(table)
	escaped := storage[capacity:capacity]
	mask := uint32(capacity - 1)
	hashed := v.Entry.KeysHashed()
	keyEntry := EntryAt(v.Entry, 1)
	off := uint32(1)
	for member := 0; member < count; member++ {
		if keyEntry.Flags()&TapeFlagEscaped != 0 {
			if len(escaped) == cap(escaped) {
				// Grow once for the remaining possible escaped keys.
				grown := make([]ProbeSlot, len(escaped), len(escaped)+count-member)
				copy(grown, escaped)
				escaped = grown
			}
			escaped = append(escaped, ProbeSlot{off: off})
		} else {
			hash := keyEntry.Next
			if !hashed {
				hash = HashKeyContent(byteview.SliceRange(v.Src, keyEntry.Start+1, keyEntry.End-1))
			}
			probeInsert(table, mask, v.Src, v.Entry, hash, off)
		}
		if member+1 < count {
			valueEntry := EntryAt(keyEntry, 1)
			keyEntry = EntryAt(valueEntry, uintptr(valueEntry.Next))
			off += 1 + valueEntry.Next
		}
	}
	return ObjectProbe{src: v.Src, header: v.Entry, table: table, escaped: escaped, mask: mask}, true
}

// probeInsert inserts a key and replaces an earlier byte-identical duplicate.
func probeInsert(table []ProbeSlot, mask uint32, src *byte, header *IndexEntry, hash, off uint32) {
	idx := hash & mask
	for {
		slot := &table[idx]
		if slot.off == 0 {
			*slot = ProbeSlot{hash: hash, off: off}
			return
		}
		if slot.hash == hash {
			prev := EntryAt(header, uintptr(slot.off))
			cur := EntryAt(header, uintptr(off))
			if bytes.Equal(byteview.SliceRange(src, prev.Start+1, prev.End-1),
				byteview.SliceRange(src, cur.Start+1, cur.End-1)) {
				slot.off = off
				return
			}
		}
		idx = (idx + 1) & mask
	}
}

// Get returns the last object member with key in expected constant time.
func (p *ObjectProbe) Get(key string) (Node, bool) {
	if p.header == nil {
		return Node{}, false
	}
	hash := HashKey(key)
	// best is the winning key-entry offset; offsets are in document order.
	best := uint32(0)
	if table := p.table; len(table) != 0 {
		idx := hash & p.mask
		for {
			slot := table[idx]
			if slot.off == 0 {
				break
			}
			if slot.hash == hash {
				keyEntry := EntryAt(p.header, uintptr(slot.off))
				if BytesEqualString(byteview.SliceRange(p.src, keyEntry.Start+1, keyEntry.End-1), key) {
					best = slot.off
					break
				}
			}
			idx = (idx + 1) & p.mask
		}
	}
	for i := range p.escaped {
		off := p.escaped[i].off
		if off > best {
			keyEntry := EntryAt(p.header, uintptr(off))
			if tapeKeyEqual(byteview.SliceRange(p.src, keyEntry.Start, keyEntry.End), keyEntry.Flags(), key) {
				best = off
			}
		}
	}
	if best == 0 {
		return Node{}, false
	}
	return Node{Src: p.src, Entry: EntryAt(p.header, uintptr(best)+1)}, true
}
