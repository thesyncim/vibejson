package storeio

import "math/bits"

const pageCacheNoBlock = ^uint32(0)

// pageCacheBlockLink is the intrusive control record for one free block head.
// It contains no Go pointers and exists once per cache allocation quantum, not
// once per database page. Non-head slots and allocated heads use order 0xff.
type pageCacheBlockLink struct {
	next  uint32
	prev  uint32
	order uint8
	_     [3]byte
}

// pageCacheBlocks is a bounded buddy allocator for the cache arena. Pages are
// power-of-two multiples of the allocation quantum, so splitting and merging
// needs no tree, map, allocation, or arena scan. Independent max-page-sized
// zones keep the number of orders fixed by MaxPageSize rather than cache size.
type pageCacheBlocks struct {
	heads     []uint32
	links     []pageCacheBlockLink
	zoneSlots uint32
	maxOrder  uint8
}

func newPageCacheBlocks(slots, maxSpan int) pageCacheBlocks {
	maxOrder := uint8(bits.TrailingZeros(uint(maxSpan)))
	blocks := pageCacheBlocks{
		heads:     make([]uint32, int(maxOrder)+1),
		links:     make([]pageCacheBlockLink, slots),
		zoneSlots: uint32(maxSpan),
		maxOrder:  maxOrder,
	}
	for index := range blocks.heads {
		blocks.heads[index] = pageCacheNoBlock
	}
	for index := range blocks.links {
		blocks.links[index] = pageCacheBlockLink{
			next: pageCacheNoBlock, prev: pageCacheNoBlock, order: ^uint8(0),
		}
	}
	for start := 0; start < slots; {
		remaining := min(maxSpan, slots-start)
		for remaining != 0 {
			order := uint8(bits.Len(uint(remaining)) - 1)
			span := 1 << order
			blocks.add(uint32(start), order)
			start += span
			remaining -= span
		}
	}
	return blocks
}

// take removes one span from the smallest available size class. Larger blocks
// split toward their lower address; the unused halves return to their lists.
func (b *pageCacheBlocks) take(span int) (int, bool) {
	if b == nil || span <= 0 || span > int(b.zoneSlots) || span&(span-1) != 0 {
		return 0, false
	}
	want := uint8(bits.TrailingZeros(uint(span)))
	order := want
	for order <= b.maxOrder && b.heads[order] == pageCacheNoBlock {
		order++
	}
	if order > b.maxOrder {
		return 0, false
	}
	index := b.heads[order]
	b.remove(index, order)
	for order > want {
		order--
		b.add(index+uint32(1<<order), order)
	}
	return int(index), true
}

// put returns one allocated span and coalesces free buddies within its
// max-page-sized zone. Cache extents never cross a zone, so all supported page
// sizes remain allocatable even when the total slot count is not a power of two.
func (b *pageCacheBlocks) put(index, span int) {
	order := uint8(bits.TrailingZeros(uint(span)))
	current := uint32(index)
	zoneStart := current &^ (b.zoneSlots - 1)
	zoneEnd := min(zoneStart+b.zoneSlots, uint32(len(b.links)))
	for order < b.maxOrder {
		buddy := zoneStart + ((current - zoneStart) ^ uint32(1<<order))
		if buddy >= zoneEnd || b.links[buddy].order != order {
			break
		}
		b.remove(buddy, order)
		current = min(current, buddy)
		order++
	}
	b.add(current, order)
}

func (b *pageCacheBlocks) add(index uint32, order uint8) {
	head := b.heads[order]
	b.links[index] = pageCacheBlockLink{
		next: head, prev: pageCacheNoBlock, order: order,
	}
	if head != pageCacheNoBlock {
		b.links[head].prev = index
	}
	b.heads[order] = index
}

func (b *pageCacheBlocks) remove(index uint32, order uint8) {
	link := &b.links[index]
	if link.order != order {
		panic("storeio: page-cache free-block invariant")
	}
	if link.prev == pageCacheNoBlock {
		b.heads[order] = link.next
	} else {
		b.links[link.prev].next = link.next
	}
	if link.next != pageCacheNoBlock {
		b.links[link.next].prev = link.prev
	}
	*link = pageCacheBlockLink{
		next: pageCacheNoBlock, prev: pageCacheNoBlock, order: ^uint8(0),
	}
}
