package storeio

import "math/bits"

const pageCacheNoBlock = ^uint32(0)

const smallLargeBoundary = 4

type pageCacheBlockClass uint8

const (
	pageCacheBlockUnassigned pageCacheBlockClass = iota
	pageCacheBlockSmall
	pageCacheBlockLarge
	pageCacheBlockClassCount
)

// pageCacheBlockLink is the intrusive control record for one free block head.
// It contains no Go pointers and exists once per cache allocation quantum, not
// once per database page. Non-head slots and allocated heads use order 0xff.
type pageCacheBlockLink struct {
	next  uint32
	prev  uint32
	order uint8
	_     [3]byte
}

// pageCacheBlocks is a bounded buddy allocator for the cache arena. Logical
// extents may use any whole allocation-quantum span; their internal reservations
// are rounded to power-of-two spans before reaching this allocator, so splitting
// and merging needs no tree, map, allocation, or arena scan. Independent
// max-page-sized zones keep the number of orders fixed by MaxPageSize rather
// than cache size.
type pageCacheBlocks struct {
	// heads is class-major: each class owns one intrusive free-list head per
	// order. A zone's free blocks always reside in the lists named by its
	// current class.
	heads           []uint32
	links           []pageCacheBlockLink
	zoneClasses     []pageCacheBlockClass
	zoneSlots       uint32
	maxOrder        uint8
	crossClassTakes uint64
}

func newPageCacheBlocks(slots, maxSpan int) pageCacheBlocks {
	maxOrder := uint8(bits.TrailingZeros(uint(maxSpan)))
	blocks := pageCacheBlocks{
		heads:       make([]uint32, int(pageCacheBlockClassCount)*(int(maxOrder)+1)),
		links:       make([]pageCacheBlockLink, slots),
		zoneClasses: make([]pageCacheBlockClass, (slots+maxSpan-1)/maxSpan),
		zoneSlots:   uint32(maxSpan),
		maxOrder:    maxOrder,
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
	class := pageCacheBlockClassForSpan(span)
	index, order, ok := b.takeClass(class, want)
	if !ok {
		index, order, ok = b.takeClass(pageCacheBlockUnassigned, want)
		if ok {
			b.setZoneClass(index/b.zoneSlots, class)
		}
	}
	if !ok {
		fallback := pageCacheBlockSmall
		if class == pageCacheBlockSmall {
			fallback = pageCacheBlockLarge
		}
		index, order, ok = b.takeClass(fallback, want)
		if !ok {
			return 0, false
		}
		b.crossClassTakes++
	}
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
	if order == b.maxOrder {
		b.setZoneClass(current/b.zoneSlots, pageCacheBlockUnassigned)
	}
	b.add(current, order)
}

func pageCacheBlockClassForSpan(span int) pageCacheBlockClass {
	if span < smallLargeBoundary {
		return pageCacheBlockSmall
	}
	return pageCacheBlockLarge
}

func (b *pageCacheBlocks) takeClass(
	class pageCacheBlockClass, want uint8,
) (uint32, uint8, bool) {
	order := want
	for order <= b.maxOrder && b.head(class, order) == pageCacheNoBlock {
		order++
	}
	if order > b.maxOrder {
		return 0, 0, false
	}
	index := b.head(class, order)
	b.removeClass(index, order, class)
	return index, order, true
}

func (b *pageCacheBlocks) zoneClass(index uint32) pageCacheBlockClass {
	return b.zoneClasses[index/b.zoneSlots]
}

// setZoneClass relinks every free block in one bounded zone before publishing
// its new class. The removed allocation that triggered an assignment is no
// longer on a list, and a fully coalesced reset likewise has no linked
// remainder, but scanning all zone slots keeps both transitions uniform.
func (b *pageCacheBlocks) setZoneClass(zone uint32, class pageCacheBlockClass) {
	old := b.zoneClasses[zone]
	if old == class {
		return
	}
	start := zone * b.zoneSlots
	end := min(start+b.zoneSlots, uint32(len(b.links)))
	for index := start; index < end; index++ {
		order := b.links[index].order
		if order == ^uint8(0) {
			continue
		}
		b.removeClass(index, order, old)
		b.addClass(index, order, class)
	}
	b.zoneClasses[zone] = class
}

func (b *pageCacheBlocks) head(class pageCacheBlockClass, order uint8) uint32 {
	return b.heads[int(class)*(int(b.maxOrder)+1)+int(order)]
}

func (b *pageCacheBlocks) setHead(
	class pageCacheBlockClass, order uint8, index uint32,
) {
	b.heads[int(class)*(int(b.maxOrder)+1)+int(order)] = index
}

func (b *pageCacheBlocks) add(index uint32, order uint8) {
	b.addClass(index, order, b.zoneClass(index))
}

func (b *pageCacheBlocks) addClass(
	index uint32, order uint8, class pageCacheBlockClass,
) {
	head := b.head(class, order)
	b.links[index] = pageCacheBlockLink{
		next: head, prev: pageCacheNoBlock, order: order,
	}
	if head != pageCacheNoBlock {
		b.links[head].prev = index
	}
	b.setHead(class, order, index)
}

func (b *pageCacheBlocks) remove(index uint32, order uint8) {
	b.removeClass(index, order, b.zoneClass(index))
}

func (b *pageCacheBlocks) removeClass(
	index uint32, order uint8, class pageCacheBlockClass,
) {
	link := &b.links[index]
	if link.order != order {
		panic("storeio: page-cache free-block invariant")
	}
	if link.prev == pageCacheNoBlock {
		b.setHead(class, order, link.next)
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
