package storeio

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"unsafe"
)

// OffsetOf reports view's exact byte offset inside the leased complete page.
// Typed admitted views intentionally expose only borrowed slices; this keeps
// the one pointer-to-offset conversion beside the lease that owns the arena.
func (l *PageLease) OffsetOf(view []byte) (uint32, bool) {
	if l == nil || l.cache == nil || len(view) == 0 || len(l.page) == 0 {
		return 0, false
	}
	pageStart := uintptr(unsafe.Pointer(unsafe.SliceData(l.page)))
	viewStart := uintptr(unsafe.Pointer(unsafe.SliceData(view)))
	if viewStart < pageStart {
		return 0, false
	}
	offset := viewStart - pageStart
	if offset > uintptr(len(l.page)) ||
		uintptr(len(view)) > uintptr(len(l.page))-offset ||
		offset > uintptr(^uint32(0)) {
		return 0, false
	}
	return uint32(offset), true
}

// ReplaceLeasedCanonicalDirty re-homes and edits one exclusively owned
// canonical frame without copying its complete extent. The caller's resolving
// lease must be the frame's only pin. While the row bytes and checksum move,
// the frame is in a brief exclusive state and the frame lock prevents a new
// reader from acquiring a view over torn bytes.
//
// from and to keep the same logical page identity and extent size. A different
// physical to reference is the unpublished COW destination allocated for the
// next buffered root; a repeated update may pass the same reference and merely
// advance dirtyGeneration.
func (c *PageCache) ReplaceLeasedCanonicalDirty(
	lease *PageLease,
	from, to PageRef,
	valueOffset uint32,
	before, after []byte,
	dirtyGeneration uint64,
) (previousDirty uint64, err error) {
	if c == nil || lease == nil || lease.cache != c ||
		len(before) == 0 || len(before) != len(after) {
		return 0, ErrCanonicalPageBusy
	}
	fromKey, err := c.validateRef(from)
	if err != nil {
		return 0, err
	}
	toKey, err := c.validateRef(to)
	if err != nil {
		return 0, err
	}
	end := uint64(valueOffset) + uint64(len(after))
	if from.Length != to.Length || from.Kind != to.Kind ||
		from.LogicalID != to.LogicalID || from.Flags != to.Flags ||
		from.Aux != to.Aux || dirtyGeneration < to.Generation ||
		end > uint64(from.Length)-PageTrailerSize {
		return 0, fmt.Errorf("%w: canonical replacement shape", ErrPageCacheReference)
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closing.Load() || c.closed {
		return 0, ErrPageCacheClosed
	}
	index := lease.frame
	if index < 0 || index >= len(c.frames) {
		return 0, ErrCanonicalPageBusy
	}
	frame := &c.frames[index]
	frame.lock.Lock()
	defer frame.lock.Unlock()
	if frame.state != pageCacheReady || frame.key != fromKey ||
		lease.key != fromKey || frame.pins != 1 {
		return 0, ErrCanonicalPageBusy
	}
	page := c.extentBytes(index, from.Length)
	target := page[int(valueOffset):int(end)]
	if !bytes.Equal(target, before) {
		return 0, ErrCanonicalPageChanged
	}

	previousDirty = frame.dirty
	frame.state = pageCacheExclusive
	rekey := fromKey != toKey
	if rekey {
		c.removeLocked(cacheKeyHash(fromKey), fromKey)
	}
	copy(target, after)
	binary.LittleEndian.PutUint64(page[24:32], to.Generation)
	if _, sealErr := sealPage(page, false); sealErr != nil {
		copy(target, before)
		binary.LittleEndian.PutUint64(page[24:32], from.Generation)
		_, _ = sealPage(page, false)
		frame.state = pageCacheReady
		if rekey {
			c.insertLocked(cacheKeyHash(fromKey), index)
		}
		return 0, sealErr
	}

	if previousDirty == 0 {
		c.dirtyBytes += uint64(frame.key.length)
		c.dirtyReservedBytes +=
			uint64(frame.reservationSpan) * uint64(c.options.PageSize)
	} else if previousDirty != dirtyGeneration {
		c.removeDirtyFrameLocked(index, previousDirty)
	}
	frame.key = toKey
	frame.dirty = dirtyGeneration
	frame.referenced = true
	c.recordDirtyFrameLocked(index, dirtyGeneration)
	if rekey {
		c.insertLocked(cacheKeyHash(toKey), index)
	}
	lease.key = toKey
	frame.state = pageCacheReady
	return previousDirty, nil
}

// RestoreLeasedCanonicalDirty rolls back one replacement that has not been
// accepted by the committer. It is the exact inverse of
// ReplaceLeasedCanonicalDirty and retains the writer's resolving pin.
func (c *PageCache) RestoreLeasedCanonicalDirty(
	lease *PageLease,
	from, to PageRef,
	valueOffset uint32,
	before, after []byte,
	dirtyGeneration, previousDirty uint64,
) error {
	if c == nil || lease == nil || lease.cache != c ||
		len(before) == 0 || len(before) != len(after) {
		return ErrCanonicalPageChanged
	}
	fromKey, err := c.validateRef(from)
	if err != nil {
		return err
	}
	toKey, err := c.validateRef(to)
	if err != nil {
		return err
	}
	end := uint64(valueOffset) + uint64(len(after))
	if end > uint64(to.Length)-PageTrailerSize {
		return ErrCanonicalPageChanged
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	index := lease.frame
	if index < 0 || index >= len(c.frames) {
		return ErrCanonicalPageChanged
	}
	frame := &c.frames[index]
	frame.lock.Lock()
	defer frame.lock.Unlock()
	if frame.state != pageCacheReady || frame.key != toKey ||
		lease.key != toKey || frame.pins != 1 ||
		frame.dirty != dirtyGeneration {
		return ErrCanonicalPageChanged
	}
	page := c.extentBytes(index, to.Length)
	target := page[int(valueOffset):int(end)]
	if !bytes.Equal(target, after) {
		return ErrCanonicalPageChanged
	}

	frame.state = pageCacheExclusive
	rekey := fromKey != toKey
	if rekey {
		c.removeLocked(cacheKeyHash(toKey), toKey)
	}
	copy(target, before)
	binary.LittleEndian.PutUint64(page[24:32], from.Generation)
	if _, sealErr := sealPage(page, false); sealErr != nil {
		frame.state = pageCacheReady
		if rekey {
			c.insertLocked(cacheKeyHash(toKey), index)
		}
		return sealErr
	}

	c.removeDirtyFrameLocked(index, dirtyGeneration)
	if previousDirty == 0 {
		c.dirtyBytes -= uint64(frame.key.length)
		c.dirtyReservedBytes -=
			uint64(frame.reservationSpan) * uint64(c.options.PageSize)
	} else {
		c.recordDirtyFrameLocked(index, previousDirty)
	}
	frame.key = fromKey
	frame.dirty = previousDirty
	frame.referenced = true
	if rekey {
		c.insertLocked(cacheKeyHash(fromKey), index)
	}
	lease.key = fromKey
	frame.state = pageCacheReady
	return nil
}
