package storeio

import (
	"errors"
	"fmt"
	"slices"
	"sync"
)

var (
	// ErrLeaseCapacity applies bounded backpressure when every configured
	// snapshot-generation lease is active.
	ErrLeaseCapacity = errors.New("vibejson: Store snapshot lease capacity exhausted")
	// ErrGenerationLeasesClosed reports acquisition after shutdown starts.
	ErrGenerationLeasesClosed = errors.New("vibejson: Store snapshot leases are closed")
	// ErrLeasesActive reports an attempted close while readers still protect
	// generations and their physical extents.
	ErrLeasesActive = errors.New("vibejson: Store snapshot leases are still active")
	// ErrRetiredExtentCapacity reports that reclamation metadata reached its
	// configured bound before readers or recovery roots released old extents.
	ErrRetiredExtentCapacity = errors.New("vibejson: Store retired extent capacity exhausted")
)

// GenerationLeaseOptions fixes snapshot tracking memory at construction.
type GenerationLeaseOptions struct {
	// MaxLeases is the maximum number of concurrently retained snapshots.
	MaxLeases int
}

func (o GenerationLeaseOptions) normalized() (GenerationLeaseOptions, error) {
	if o.MaxLeases == 0 {
		o.MaxLeases = 1024
	}
	if o.MaxLeases < 1 || o.MaxLeases > 1<<20 {
		return GenerationLeaseOptions{}, fmt.Errorf("%w: maximum leases %d", ErrInvalidWrite, o.MaxLeases)
	}
	return o, nil
}

type generationLeaseSlot struct {
	generation uint64
	token      uint64
	active     bool
}

// GenerationLeaseStats is an O(configured lease slots) accounting snapshot.
type GenerationLeaseStats struct {
	Capacity          uint64
	Active            uint64
	MinimumGeneration uint64
}

// GenerationLeases owns a fixed snapshot table. Explicit leases make page
// lifetime independent of Go GC timing and let copy-on-write reclamation prove
// that no reader can still dereference a retired extent.
type GenerationLeases struct {
	mu      sync.Mutex
	slots   []generationLeaseSlot
	free    []uint32
	next    uint64
	closing bool
	closed  bool
}

// NewGenerationLeases allocates the complete fixed lease table.
func NewGenerationLeases(options GenerationLeaseOptions) (*GenerationLeases, error) {
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	l := &GenerationLeases{
		slots: make([]generationLeaseSlot, normalized.MaxLeases),
		free:  make([]uint32, normalized.MaxLeases),
	}
	for i := range l.free {
		l.free[i] = uint32(len(l.free) - 1 - i)
	}
	return l, nil
}

// GenerationLease is a single-owner snapshot token. Do not copy it after
// first use. Release is idempotent for one value.
type GenerationLease struct {
	owner      *GenerationLeases
	index      uint32
	generation uint64
	token      uint64
}

// Generation returns the protected immutable Store generation.
func (l *GenerationLease) Generation() uint64 {
	if l == nil || l.owner == nil {
		return 0
	}
	return l.generation
}

// Release stops protecting the generation.
func (l *GenerationLease) Release() {
	if l == nil || l.owner == nil {
		return
	}
	owner := l.owner
	owner.release(l.index, l.token)
	l.owner = nil
	l.generation = 0
	l.token = 0
}

// Acquire protects generation until the returned lease is released. It never
// grows tracking memory.
func (l *GenerationLeases) Acquire(generation uint64) (GenerationLease, error) {
	if l == nil {
		return GenerationLease{}, ErrGenerationLeasesClosed
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closing || l.closed {
		return GenerationLease{}, ErrGenerationLeasesClosed
	}
	if len(l.free) == 0 {
		return GenerationLease{}, ErrLeaseCapacity
	}
	last := len(l.free) - 1
	index := l.free[last]
	l.free = l.free[:last]
	l.next++
	if l.next == 0 {
		l.next++
	}
	token := l.next
	l.slots[index] = generationLeaseSlot{generation: generation, token: token, active: true}
	return GenerationLease{owner: l, index: index, generation: generation, token: token}, nil
}

func (l *GenerationLeases) release(index uint32, token uint64) {
	l.mu.Lock()
	if int(index) < len(l.slots) {
		slot := &l.slots[index]
		if slot.active && slot.token == token {
			*slot = generationLeaseSlot{}
			l.free = append(l.free, index)
		}
	}
	l.mu.Unlock()
}

// Minimum returns the oldest active generation. If no reader is active it
// returns successor, denoting that every generation through current is free of
// reader references.
func (l *GenerationLeases) Minimum(current uint64) uint64 {
	if l == nil {
		return generationSuccessor(current)
	}
	l.mu.Lock()
	minimum := generationSuccessor(current)
	for i := range l.slots {
		if l.slots[i].active && l.slots[i].generation < minimum {
			minimum = l.slots[i].generation
		}
	}
	l.mu.Unlock()
	return minimum
}

// SafeFromSnapshots reports whether a page first published at generation can
// be unreachable from every currently active user snapshot. A snapshot can
// retain a page whose generation is less than or equal to its own, so safety
// requires every active lease generation to be strictly less than generation.
// Equality is unsafe.
//
// The result is linearized with Acquire and Release by the lease mutex. It is a
// point-in-time observation: a writer that needs safety to remain true while it
// materializes or rewrites storage must already prevent new snapshot
// acquisition with its publication gate and keep that gate held through the
// operation. This method does not cover alternate recovery roots or page-cache
// pins, which have separate ownership fences.
//
// Generation zero is never a valid page generation and returns false. A nil
// table has no active snapshots and returns true for every non-zero generation.
func (l *GenerationLeases) SafeFromSnapshots(generation uint64) bool {
	if generation == 0 {
		return false
	}
	if l == nil {
		return true
	}
	l.mu.Lock()
	safe := true
	for i := range l.slots {
		if l.slots[i].active && l.slots[i].generation >= generation {
			safe = false
			break
		}
	}
	l.mu.Unlock()
	return safe
}

// Stats returns bounded lease usage and the current reclamation floor.
func (l *GenerationLeases) Stats(current uint64) GenerationLeaseStats {
	if l == nil {
		return GenerationLeaseStats{MinimumGeneration: generationSuccessor(current)}
	}
	l.mu.Lock()
	stats := GenerationLeaseStats{
		Capacity: uint64(len(l.slots)), MinimumGeneration: generationSuccessor(current),
	}
	for i := range l.slots {
		if l.slots[i].active {
			stats.Active++
			if l.slots[i].generation < stats.MinimumGeneration {
				stats.MinimumGeneration = l.slots[i].generation
			}
		}
	}
	l.mu.Unlock()
	return stats
}

// Close prevents new leases. It returns ErrLeasesActive until every existing
// lease is released, then becomes idempotent.
func (l *GenerationLeases) Close() error {
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closing = true
	if len(l.free) != len(l.slots) {
		return ErrLeasesActive
	}
	l.closed = true
	return nil
}

func generationSuccessor(generation uint64) uint64 {
	if generation == ^uint64(0) {
		return generation
	}
	return generation + 1
}

// ExtentReclaimerOptions fixes retained copy-on-write metadata.
type ExtentReclaimerOptions struct {
	MaxRetiredExtents int
}

func (o ExtentReclaimerOptions) normalized() (ExtentReclaimerOptions, error) {
	if o.MaxRetiredExtents == 0 {
		o.MaxRetiredExtents = 1 << 16
	}
	if o.MaxRetiredExtents < 1 || o.MaxRetiredExtents > 1<<24 {
		return ExtentReclaimerOptions{}, fmt.Errorf("%w: maximum retired extents %d", ErrInvalidWrite, o.MaxRetiredExtents)
	}
	return o, nil
}

// ExtentReclaimerStats accounts for extents waiting on snapshots or the
// alternate recovery root.
type ExtentReclaimerStats struct {
	Capacity      uint64
	Pending       uint64
	PendingBytes  uint64
	OldestRetired uint64
}

// ExtentReclaimer retains a fixed number of retired extents. The serialized
// Store writer calls Retire and Reclaim; readers only touch GenerationLeases.
type ExtentReclaimer struct {
	mu           sync.Mutex
	leases       *GenerationLeases
	pending      []FreeExtent
	pendingHead  int
	pendingBytes uint64
	limit        int
}

// NewExtentReclaimer creates bounded retirement tracking over leases.
func NewExtentReclaimer(leases *GenerationLeases, options ExtentReclaimerOptions) (*ExtentReclaimer, error) {
	if leases == nil {
		return nil, fmt.Errorf("%w: nil generation leases", ErrInvalidWrite)
	}
	normalized, err := options.normalized()
	if err != nil {
		return nil, err
	}
	return &ExtentReclaimer{
		leases: leases, pending: make([]FreeExtent, 0, normalized.MaxRetiredExtents),
		limit: normalized.MaxRetiredExtents,
	}, nil
}

// Retire records an extent made unreachable by the next root. Overlap with an
// already-pending extent is rejected before publication.
func (r *ExtentReclaimer) Retire(extent FreeExtent) error {
	return r.RetireBatch([]FreeExtent{extent})
}

// RetireBatch atomically reserves retirement metadata for a publication. No
// extent is recorded if capacity, shape, or overlap validation fails.
func (r *ExtentReclaimer) RetireBatch(extents []FreeExtent) error {
	if r == nil {
		return fmt.Errorf("%w: nil extent reclaimer", ErrInvalidWrite)
	}
	if len(extents) == 0 {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.activePendingLocked()
	if len(extents) > r.limit-len(pending) {
		return ErrRetiredExtentCapacity
	}
	addedBytes := uint64(0)
	for i, extent := range extents {
		if extent.Offset == 0 || extent.Length == 0 || extent.RetiredGeneration == 0 ||
			extent.Offset > ^uint64(0)-extent.Length {
			return fmt.Errorf("%w: retired extent", ErrInvalidWrite)
		}
		addedBytes += extent.Length
		end := extent.Offset + extent.Length
		for _, held := range pending {
			heldEnd := held.Offset + held.Length
			if extent.Offset < heldEnd && held.Offset < end {
				return fmt.Errorf("%w: overlapping retired extent", ErrInvalidWrite)
			}
		}
		for j := 0; j < i; j++ {
			other := extents[j]
			otherEnd := other.Offset + other.Length
			if extent.Offset < otherEnd && other.Offset < end {
				return fmt.Errorf("%w: overlapping retired extent", ErrInvalidWrite)
			}
		}
	}
	r.compactPendingLocked(len(extents))
	start := len(r.pending)
	r.pending = append(r.pending, extents...)
	r.pendingBytes += addedBytes
	added := r.pending[start:]
	if len(added) > 1 && !reclamationGenerationOrdered(added) {
		slices.SortStableFunc(added, compareReclamationGeneration)
	}
	if start != 0 &&
		r.pending[start-1].RetiredGeneration > r.pending[start].RetiredGeneration {
		// A bounded reclamation batch may be returned after a fold-budget trim.
		// That rare path can insert an older generation behind still-fenced
		// generations. Restore ordering here; ordinary serialized retirements
		// append in O(batch) time.
		slices.SortStableFunc(
			r.activePendingLocked(), compareReclamationGeneration,
		)
	}
	return nil
}

// CancelRetiredGeneration rolls back an unpublished serialized-writer
// reservation. The generation must occupy the complete pending tail.
func (r *ExtentReclaimer) CancelRetiredGeneration(generation uint64) error {
	if r == nil || generation == 0 {
		return fmt.Errorf("%w: retired generation", ErrInvalidWrite)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.activePendingLocked()
	first := len(r.pending)
	for first > r.pendingHead && r.pending[first-1].RetiredGeneration == generation {
		first--
	}
	for i := r.pendingHead; i < first; i++ {
		if r.pending[i].RetiredGeneration == generation {
			return fmt.Errorf("%w: non-tail retired generation", ErrInvalidWrite)
		}
	}
	for _, extent := range r.pending[first:] {
		r.pendingBytes -= extent.Length
	}
	clear(r.pending[first:])
	r.pending = r.pending[:first]
	if len(pending) == first-r.pendingHead {
		return nil
	}
	if first == r.pendingHead {
		r.resetPendingLocked()
	}
	return nil
}

// AppendReusable appends and removes extents whose retired generation is
// older than both every active reader and oldestRecoveryGeneration. dst lets
// the serialized allocator reuse its own scratch without allocation.
//
// limit caps how many extents are moved, because dst is backed by a fixed
// off-heap arena that must never be reallocated onto the Go heap. Extents past
// the limit stay pending and are offered again at the next call, so a caller
// with no room left reclaims nothing rather than losing anything. A
// non-positive limit moves nothing.
//
// Taking a limit is what lets the caller stop declining the whole batch when
// its arena is nearly full. Refusing to reclaim anything is not a smaller
// version of reclaiming: this is the only drain of the pending set, so a
// caller that declines stops the one process that would create the room it is
// waiting for, and the pending set then grows to its own capacity and fails
// every subsequent retirement.
func (r *ExtentReclaimer) AppendReusable(dst []FreeExtent, currentGeneration, oldestRecoveryGeneration uint64, limit int) []FreeExtent {
	if r == nil || limit <= 0 {
		return dst
	}
	readerFloor := r.leases.Minimum(currentGeneration)
	floor := min(readerFloor, oldestRecoveryGeneration)
	r.mu.Lock()
	pending := r.activePendingLocked()
	eligible := len(pending)
	if eligible != 0 {
		switch {
		case pending[0].RetiredGeneration >= floor:
			eligible = 0
		case pending[eligible-1].RetiredGeneration >= floor:
			for low, high := 0, eligible; low < high; {
				middle := int(uint(low+high) >> 1)
				if pending[middle].RetiredGeneration < floor {
					low = middle + 1
				} else {
					high = middle
				}
				eligible = low
			}
		}
	}
	moved := min(eligible, limit)
	dst = append(dst, pending[:moved]...)
	switch {
	case moved == 0:
	case moved == len(pending):
		r.resetPendingLocked()
	default:
		for _, extent := range pending[:moved] {
			r.pendingBytes -= extent.Length
		}
		clear(pending[:moved])
		r.pendingHead += moved
	}
	r.mu.Unlock()
	return dst
}

// AppendPending copies the extents still waiting on a reader or the alternate
// recovery root in nondecreasing retirement-generation order. There is no
// physical-offset ordering contract within one generation: consumers that
// merge physical ranges must sort the result by offset first. The durable free
// log carries them alongside the
// reusable set — they are free space, merely fenced — so a fold needs them to
// write a complete image, and dropping them from that image is what used to
// lose them at every restart.
func (r *ExtentReclaimer) AppendPending(dst []FreeExtent) []FreeExtent {
	if r == nil {
		return dst
	}
	r.mu.Lock()
	dst = append(dst, r.activePendingLocked()...)
	r.mu.Unlock()
	return dst
}

// Restore re-establishes a pending set replayed from the durable free log.
//
// It skips the pairwise overlap scan RetireBatch performs, because the replay
// has already proved the set disjoint — it refuses to produce an overlapping
// free set at all — and that scan is quadratic in the pending set, which is the
// one place this subsystem cannot afford it at a hundred thousand extents.
// Restoring into a non-empty reclaimer is refused rather than merged, because
// the only caller is the open path and a second call would mean the store
// replayed twice.
func (r *ExtentReclaimer) Restore(extents []FreeExtent) error {
	if r == nil {
		return fmt.Errorf("%w: nil extent reclaimer", ErrInvalidWrite)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	pending := r.activePendingLocked()
	if len(pending) != 0 {
		return fmt.Errorf("%w: reclaimer already holds %d extents", ErrInvalidWrite, len(pending))
	}
	if len(extents) > r.limit {
		return ErrRetiredExtentCapacity
	}
	pendingBytes := uint64(0)
	for _, extent := range extents {
		if extent.Offset == 0 || extent.Length == 0 || extent.RetiredGeneration == 0 ||
			extent.Offset > ^uint64(0)-extent.Length {
			return fmt.Errorf("%w: restored retired extent", ErrInvalidWrite)
		}
		pendingBytes += extent.Length
	}
	r.resetPendingLocked()
	r.pending = append(r.pending, extents...)
	slices.SortStableFunc(r.pending, compareReclamationGeneration)
	r.pendingBytes = pendingBytes
	return nil
}

// PendingCount reports how many extents are still fenced.
func (r *ExtentReclaimer) PendingCount() int {
	if r == nil {
		return 0
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending) - r.pendingHead
}

// Stats reports bounded retirement pressure.
func (r *ExtentReclaimer) Stats() ExtentReclaimerStats {
	if r == nil {
		return ExtentReclaimerStats{}
	}
	r.mu.Lock()
	pending := r.activePendingLocked()
	stats := ExtentReclaimerStats{
		Capacity: uint64(r.limit), Pending: uint64(len(pending)),
		PendingBytes: r.pendingBytes,
	}
	if len(pending) != 0 {
		stats.OldestRetired = pending[0].RetiredGeneration
	}
	r.mu.Unlock()
	return stats
}

func compareReclamationGeneration(a, b FreeExtent) int {
	switch {
	case a.RetiredGeneration < b.RetiredGeneration:
		return -1
	case a.RetiredGeneration > b.RetiredGeneration:
		return 1
	case a.Offset < b.Offset:
		return -1
	case a.Offset > b.Offset:
		return 1
	default:
		return 0
	}
}

func reclamationGenerationOrdered(extents []FreeExtent) bool {
	for i := 1; i < len(extents); i++ {
		if extents[i-1].RetiredGeneration > extents[i].RetiredGeneration {
			return false
		}
	}
	return true
}

func (r *ExtentReclaimer) activePendingLocked() []FreeExtent {
	return r.pending[r.pendingHead:]
}

// compactPendingLocked recovers reclaimed prefix capacity only when the next
// append needs it. The common pinned-snapshot query and bounded drain paths
// therefore move no retained entries.
func (r *ExtentReclaimer) compactPendingLocked(appendCount int) {
	if appendCount <= cap(r.pending)-len(r.pending) {
		return
	}
	pending := r.activePendingLocked()
	moved := copy(r.pending, pending)
	clear(r.pending[moved:])
	r.pending = r.pending[:moved]
	r.pendingHead = 0
}

func (r *ExtentReclaimer) resetPendingLocked() {
	clear(r.pending)
	r.pending = r.pending[:0]
	r.pendingHead = 0
	r.pendingBytes = 0
}
