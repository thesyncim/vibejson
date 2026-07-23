package storeio

import (
	"errors"
	"fmt"
	"sync"
)

var (
	// ErrLeaseCapacity applies bounded backpressure when every configured
	// snapshot-generation lease is active.
	ErrLeaseCapacity = errors.New("slopjson: Store snapshot lease capacity exhausted")
	// ErrGenerationLeasesClosed reports acquisition after shutdown starts.
	ErrGenerationLeasesClosed = errors.New("slopjson: Store snapshot leases are closed")
	// ErrLeasesActive reports an attempted close while readers still protect
	// generations and their physical extents.
	ErrLeasesActive = errors.New("slopjson: Store snapshot leases are still active")
	// ErrRetiredExtentCapacity reports that reclamation metadata reached its
	// configured bound before readers or recovery roots released old extents.
	ErrRetiredExtentCapacity = errors.New("slopjson: Store retired extent capacity exhausted")
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
	mu      sync.Mutex
	leases  *GenerationLeases
	pending []FreeExtent
	limit   int
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
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(extents) > r.limit-len(r.pending) {
		return ErrRetiredExtentCapacity
	}
	for i, extent := range extents {
		if extent.Offset == 0 || extent.Length == 0 || extent.RetiredGeneration == 0 ||
			extent.Offset > ^uint64(0)-extent.Length {
			return fmt.Errorf("%w: retired extent", ErrInvalidWrite)
		}
		end := extent.Offset + extent.Length
		for _, pending := range r.pending {
			pendingEnd := pending.Offset + pending.Length
			if extent.Offset < pendingEnd && pending.Offset < end {
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
	r.pending = append(r.pending, extents...)
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
	first := len(r.pending)
	for first > 0 && r.pending[first-1].RetiredGeneration == generation {
		first--
	}
	for i := 0; i < first; i++ {
		if r.pending[i].RetiredGeneration == generation {
			return fmt.Errorf("%w: non-tail retired generation", ErrInvalidWrite)
		}
	}
	clear(r.pending[first:])
	r.pending = r.pending[:first]
	return nil
}

// AppendReusable appends and removes extents whose retired generation is
// older than both every active reader and oldestRecoveryGeneration. dst lets
// the serialized allocator reuse its own scratch without allocation.
func (r *ExtentReclaimer) AppendReusable(dst []FreeExtent, currentGeneration, oldestRecoveryGeneration uint64) []FreeExtent {
	if r == nil {
		return dst
	}
	readerFloor := r.leases.Minimum(currentGeneration)
	floor := min(readerFloor, oldestRecoveryGeneration)
	r.mu.Lock()
	kept := r.pending[:0]
	for _, extent := range r.pending {
		if extent.RetiredGeneration < floor {
			dst = append(dst, extent)
		} else {
			kept = append(kept, extent)
		}
	}
	clear(r.pending[len(kept):])
	r.pending = kept
	r.mu.Unlock()
	return dst
}

// Stats reports bounded retirement pressure.
func (r *ExtentReclaimer) Stats() ExtentReclaimerStats {
	if r == nil {
		return ExtentReclaimerStats{}
	}
	r.mu.Lock()
	stats := ExtentReclaimerStats{Capacity: uint64(r.limit), Pending: uint64(len(r.pending))}
	for _, extent := range r.pending {
		stats.PendingBytes += extent.Length
		if stats.OldestRetired == 0 || extent.RetiredGeneration < stats.OldestRetired {
			stats.OldestRetired = extent.RetiredGeneration
		}
	}
	r.mu.Unlock()
	return stats
}
