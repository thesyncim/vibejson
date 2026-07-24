package store

import (
	"context"
	"slices"
	"time"
)

// TTL is deliberately absent from Store snapshots. Expiration metadata lives
// beside the writer in an indexed four-ary deadline heap: one node per expiring
// key, in-place deadline changes, O(log4 n) removal, no stale generations, and
// therefore no cleanup/compaction debt when a TTL changes repeatedly. A due
// key becomes invisible only when ExpireDue publishes a normal chunk-local
// delete. Snapshot GetRaw/Get execute no clock call, TTL lookup, or branch.

type storeDeadline struct {
	key      string
	Deadline StoreInstant
}

// storeTTLKey is a stable-slot address packed without pointers. TTL metadata
// is writer-owned and every delete removes its entry before the slot can be
// reused, so no generation or key spelling is needed in the heap or pos map.
type storeTTLKey uint64

func StoreTTLKeyOf(loc StoreLocation) storeTTLKey {
	return storeTTLKey(uint64(loc.Chunk)<<8 | uint64(loc.Slot))
}

func (k storeTTLKey) location() StoreLocation {
	return StoreLocation{Chunk: uint32(uint64(k) >> 8), Slot: uint8(k)}
}

// StoreInstant preserves nanosecond precision without time.UnixNano's 1678 to
// 2262 range restriction. Lexicographic (seconds,nanoseconds) order is time
// order because Time.Unix normalizes nanoseconds into [0,1e9).
type StoreInstant struct {
	sec  int64
	nsec int32
}

func instantOf(t time.Time) StoreInstant {
	return StoreInstant{sec: t.Unix(), nsec: int32(t.Nanosecond())}
}

func (i StoreInstant) before(other StoreInstant) bool {
	return i.sec < other.sec || i.sec == other.sec && i.nsec < other.nsec
}

func (i StoreInstant) after(other StoreInstant) bool { return other.before(i) }

func (i StoreInstant) Time() time.Time { return time.Unix(i.sec, int64(i.nsec)) }

func (i StoreInstant) sub(t time.Time) time.Duration {
	return i.Time().Sub(t)
}

type StoreTTLState struct {
	Heap []storeTTLItem
	Pos  map[storeTTLKey]int
	wake chan struct{}
}

type storeTTLItem struct {
	key      storeTTLKey
	Deadline StoreInstant
}

type storeExpiryItem struct {
	hash uint64
	loc  StoreLocation
}

func (t *StoreTTLState) upsert(key storeTTLKey, deadline StoreInstant) {
	if t.Pos == nil {
		t.Pos = make(map[storeTTLKey]int)
	}
	if i, ok := t.Pos[key]; ok {
		old := t.Heap[i].Deadline
		t.Heap[i].Deadline = deadline
		if deadline.before(old) {
			t.up(i)
		} else {
			t.down(i)
		}
		return
	}
	i := len(t.Heap)
	t.Heap = append(t.Heap, storeTTLItem{key: key, Deadline: deadline})
	t.Pos[key] = i
	t.up(i)
}

func (t *StoreTTLState) remove(key storeTTLKey) bool {
	i, ok := t.Pos[key]
	if !ok {
		return false
	}
	t.removeAt(i)
	return true
}

func (t *StoreTTLState) removeAt(i int) storeTTLItem {
	removed := t.Heap[i]
	last := len(t.Heap) - 1
	delete(t.Pos, removed.key)
	if i == last {
		t.Heap[last] = storeTTLItem{}
		t.Heap = t.Heap[:last]
		return removed
	}
	t.Heap[i] = t.Heap[last]
	t.Heap[last] = storeTTLItem{}
	t.Heap = t.Heap[:last]
	t.Pos[t.Heap[i].key] = i
	if i > 0 && t.Heap[i].Deadline.before(t.Heap[(i-1)/4].Deadline) {
		t.up(i)
	} else {
		t.down(i)
	}
	return removed
}

func (t *StoreTTLState) up(i int) {
	for i > 0 {
		parent := (i - 1) / 4
		if !t.Heap[i].Deadline.before(t.Heap[parent].Deadline) {
			return
		}
		t.swap(parent, i)
		i = parent
	}
}

func (t *StoreTTLState) down(i int) {
	for {
		first := i*4 + 1
		if first >= len(t.Heap) {
			return
		}
		best := first
		end := min(first+4, len(t.Heap))
		for child := first + 1; child < end; child++ {
			if t.Heap[child].Deadline.before(t.Heap[best].Deadline) {
				best = child
			}
		}
		if !t.Heap[best].Deadline.before(t.Heap[i].Deadline) {
			return
		}
		t.swap(i, best)
		i = best
	}
}

func (t *StoreTTLState) swap(a, b int) {
	t.Heap[a], t.Heap[b] = t.Heap[b], t.Heap[a]
	t.Pos[t.Heap[a].key] = a
	t.Pos[t.Heap[b].key] = b
}

// SetTTL assigns a duration from the current clock and reports whether key
// exists. A non-positive duration deletes the key immediately. Replacing a
// document with Put preserves its current TTL; Persist removes it explicitly.
// The error return always reports nil; it exists so Store satisfies the same
// [Table] shape as durable.Store, whose TTL methods can fail on I/O.
func (s *Store) SetTTL(key string, ttl time.Duration) (bool, error) {
	if ttl <= 0 {
		return s.Delete(key)
	}
	return s.SetDeadline(key, time.Now().Add(ttl))
}

// SetDeadline assigns an absolute expiration and reports whether key exists.
// A deadline not after the current clock deletes immediately.
func (s *Store) SetDeadline(key string, deadline time.Time) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		return false, nil
	}
	hash := maphashString(state.seed, key)
	_, loc, ok := storeStateKeyLookupChunk(state, hash, key)
	if !ok {
		return false, nil
	}
	now := time.Now()
	if !deadline.After(now) {
		return s.deleteLocked(key), nil
	}
	s.ttl.upsert(StoreTTLKeyOf(loc), instantOf(deadline))
	s.notifyExpiryLocked()
	return true, nil
}

// Persist removes key's expiration without changing the document.
func (s *Store) Persist(key string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		return false, nil
	}
	_, loc, ok := storeStateKeyLookupChunk(state, maphashString(state.seed, key), key)
	if !ok {
		return false, nil
	}
	removed := s.ttl.remove(StoreTTLKeyOf(loc))
	if removed {
		s.notifyExpiryLocked()
	}
	return removed, nil
}

// Deadline returns key's assigned deadline. It consults writer-side metadata;
// ordinary document reads do not call it implicitly.
func (s *Store) Deadline(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		return time.Time{}, false
	}
	_, loc, ok := storeStateKeyLookupChunk(state, maphashString(state.seed, key), key)
	if !ok {
		return time.Time{}, false
	}
	i, ok := s.ttl.Pos[StoreTTLKeyOf(loc)]
	if !ok {
		return time.Time{}, false
	}
	return s.ttl.Heap[i].Deadline.Time(), true
}

// TTLAt returns the deadline minus now. A negative duration means the deadline
// passed but an expiry publisher has not yet processed it; the key remains in
// current and older snapshots until ExpireDue publishes its delete.
func (s *Store) TTLAt(key string, now time.Time) (time.Duration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state.Load()
	if state == nil {
		return 0, false
	}
	_, loc, ok := storeStateKeyLookupChunk(state, maphashString(state.seed, key), key)
	if !ok {
		return 0, false
	}
	i, ok := s.ttl.Pos[StoreTTLKeyOf(loc)]
	if !ok {
		return 0, false
	}
	return s.ttl.Heap[i].Deadline.sub(now), true
}

// ExpireDue publishes deletes for up to limit deadlines <= now and returns the
// count removed. limit <= 0 processes every currently due key. All due keys in
// one call are grouped by chunk, each affected chunk is rebuilt once, and the
// entire batch becomes visible through one atomic publication. TTL metadata is
// removed first, so changing and cancelling TTLs leave no stale generations or
// deferred cleanup debt.
func (s *Store) ExpireDue(now time.Time, limit int) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	deadline := instantOf(now)
	state := s.state.Load()
	if state == nil {
		return 0
	}
	s.expireScratch = s.expireScratch[:0]
	for len(s.ttl.Heap) != 0 && !s.ttl.Heap[0].Deadline.after(deadline) && (limit <= 0 || len(s.expireScratch) < limit) {
		entry := s.ttl.removeAt(0)
		loc := entry.key.location()
		chunk := state.Chunks.Get(loc.Chunk)
		if chunk == nil || chunk.Live&(uint64(1)<<loc.Slot) == 0 {
			continue
		}
		hash := maphashString(state.seed, chunk.Key(int(loc.Slot)))
		s.expireScratch = append(s.expireScratch, storeExpiryItem{hash: hash, loc: loc})
	}
	if len(s.expireScratch) == 0 {
		return 0
	}

	// Chunk grouping turns an expiry storm into O(chunks touched) document
	// rebuilds instead of O(keys expired) rebuilds. The reusable scratch slice
	// makes grouping allocation-free after its high-water mark is established.
	slices.SortFunc(s.expireScratch, func(a, b storeExpiryItem) int {
		if a.loc.Chunk < b.loc.Chunk {
			return -1
		}
		if a.loc.Chunk > b.loc.Chunk {
			return 1
		}
		return int(a.loc.Slot) - int(b.loc.Slot)
	})
	next := *state
	next.Generation++
	next.keys = state.keys
	next.Chunks = state.Chunks
	catalogChanged, secondaryChanged := false, false
	for first := 0; first < len(s.expireScratch); {
		chunkID := s.expireScratch[first].loc.Chunk
		old := state.Chunks.Get(chunkID)
		last := first
		var remove uint64
		for last < len(s.expireScratch) && s.expireScratch[last].loc.Chunk == chunkID {
			item := s.expireScratch[last]
			remove |= uint64(1) << item.loc.Slot
			next.keys = storeKeyDelete(next.keys, item.hash, old.Key(int(item.loc.Slot)))
			next.Count--
			last++
		}
		chunk, err := buildStoreChunk(
			state.StateOptions, s.postingsRequiredLocked(), old,
			old.Live&^remove, -1, "", nil,
		)
		if err != nil {
			panic("vibejson: rebuilding validated Store chunk: " + err.Error())
		}
		next.detachMappedDocuments(old)
		next.Chunks = next.Chunks.set(chunkID, chunk)
		if chunk == nil {
			next.ChunkCount--
		}
		s.noteChunkPostingsLocked(chunkID, old, chunk)
		s.addFreeLocked(chunkID)
		catalog, secondary := s.noteIndexesForChunkLocked(chunkID, old, chunk, remove)
		catalogChanged = catalogChanged || catalog
		secondaryChanged = secondaryChanged || secondary
		first = last
	}
	if catalogChanged {
		next.Indexes = s.indexInfosLocked()
	}
	if secondaryChanged {
		next.secondary = s.indexSnapshotsLocked()
	}
	s.state.Store(&next)
	expired := len(s.expireScratch)
	clear(s.expireScratch)
	s.expireScratch = s.expireScratch[:0]
	return expired
}

func (s *Store) notifyExpiryLocked() {
	if s.ttl.wake == nil {
		return
	}
	select {
	case s.ttl.wake <- struct{}{}:
	default:
	}
}

// RunExpiry drives ExpireDue until ctx is cancelled. It sleeps when no key has
// a TTL and otherwise arms one timer for the earliest deadline; assigning an
// earlier deadline wakes and retargets it. resolution rounds timer deadlines
// upward and therefore bounds coalescing lateness; non-positive selects one
// millisecond. This is deadline-driven rather than an idle polling loop.
// Exactly one RunExpiry driver should serve a Store. Applications that already
// own an event loop can call ExpireDue directly and allocate no timer.
func (s *Store) RunExpiry(ctx context.Context, resolution time.Duration) {
	if resolution <= 0 {
		resolution = time.Millisecond
	}
	for {
		s.mu.Lock()
		if s.ttl.wake == nil {
			s.ttl.wake = make(chan struct{}, 1)
		}
		wake := s.ttl.wake
		if len(s.ttl.Heap) == 0 {
			s.mu.Unlock()
			select {
			case <-wake:
				continue
			case <-ctx.Done():
				return
			}
		}
		deadline := s.ttl.Heap[0].Deadline
		s.mu.Unlock()

		delay := deadline.sub(time.Now())
		if delay < 0 {
			delay = 0
		} else if remainder := delay % resolution; remainder != 0 {
			round := resolution - remainder
			if delay <= time.Duration(1<<63-1)-round {
				delay += round
			}
		}
		timer := time.NewTimer(delay)
		select {
		case now := <-timer.C:
			s.ExpireDue(now, 0)
		case <-wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return
		}
	}
}
