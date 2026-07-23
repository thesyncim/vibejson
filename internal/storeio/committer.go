package storeio

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

const (
	defaultCommitQueueSlots = 64
	defaultCommitGroupLimit = 32
	maxCommitDescriptors    = 1 << 20
	maxCommitCoalesce       = time.Second
)

var (
	// ErrBatchState reports use of a free, aborted, or already-published batch.
	ErrBatchState = errors.New("slopjson: Store persistence batch is not owned")
	// ErrTooManyPages reports a transaction larger than its configured bounded
	// page-descriptor capacity.
	ErrTooManyPages = errors.New("slopjson: Store persistence batch has too many pages")
	// ErrGenerationOrder reports a persistence generation that does not advance
	// the preceding published generation.
	ErrGenerationOrder = errors.New("slopjson: Store persistence generation is not increasing")
)

// CommitterOptions fixes automatic persistence queue memory. All descriptors
// are allocated during construction and reused until Close.
type CommitterOptions struct {
	// QueueSlots bounds reader-visible generations awaiting persistence. Zero
	// selects 64 and explicit values are rounded up to a power of two.
	QueueSlots int
	// MaxPagesPerBatch bounds changed data/directory pages in one generation;
	// the alternate root uses one additional buffer. Zero uses every Device
	// buffer except one.
	MaxPagesPerBatch int
	// GroupLimit bounds adjacent generations collapsed into one durable root.
	// Zero selects 32. Grouping never crosses the available buffer/page scratch.
	GroupLimit int
	// CoalesceDelay is the maximum time the background worker waits after the
	// first queued generation for adjacent publications to arrive. Publication
	// itself remains immediate; zero preserves eager durability.
	CoalesceDelay time.Duration
}

func (o CommitterOptions) normalized(bufferCount int) (CommitterOptions, error) {
	if o.QueueSlots == 0 {
		o.QueueSlots = defaultCommitQueueSlots
	}
	if o.QueueSlots < 1 || o.QueueSlots > 1<<16 {
		return CommitterOptions{}, fmt.Errorf("%w: queue slots %d", ErrInvalidWrite, o.QueueSlots)
	}
	o.QueueSlots = nextPowerOfTwo(o.QueueSlots)
	if o.MaxPagesPerBatch == 0 {
		o.MaxPagesPerBatch = bufferCount - 1
	}
	if o.MaxPagesPerBatch < 0 || o.MaxPagesPerBatch >= bufferCount {
		return CommitterOptions{}, fmt.Errorf("%w: max pages %d for %d buffers", ErrTooManyPages, o.MaxPagesPerBatch, bufferCount)
	}
	if o.MaxPagesPerBatch != 0 && o.QueueSlots > maxCommitDescriptors/o.MaxPagesPerBatch {
		return CommitterOptions{}, fmt.Errorf("%w: %d descriptor slots", ErrTooManyPages, uint64(o.QueueSlots)*uint64(o.MaxPagesPerBatch))
	}
	if o.GroupLimit == 0 {
		o.GroupLimit = min(defaultCommitGroupLimit, o.QueueSlots)
	}
	if o.GroupLimit < 1 || o.GroupLimit > o.QueueSlots {
		return CommitterOptions{}, fmt.Errorf("%w: group limit %d", ErrInvalidWrite, o.GroupLimit)
	}
	if o.CoalesceDelay < 0 || o.CoalesceDelay > maxCommitCoalesce {
		return CommitterOptions{}, fmt.Errorf("%w: coalesce delay %s", ErrInvalidWrite, o.CoalesceDelay)
	}
	return o, nil
}

func nextPowerOfTwo(value int) int {
	value--
	value |= value >> 1
	value |= value >> 2
	value |= value >> 4
	value |= value >> 8
	value |= value >> 16
	if ^uint(0)>>32 != 0 {
		value |= value >> 32
	}
	return value + 1
}

type commitFailure struct{ err error }

const (
	batchFree uint32 = iota
	batchOwned
	batchPublished
)

// Batch is one preallocated persistence generation. Its methods belong to the
// Committer's single producer. After Publish or Abort, every Batch method is
// invalid until Begin returns that slot again.
type Batch struct {
	committer      *Committer
	pages          []Write
	root           Write
	rootGeneration uint64
	generation     uint64
	index          uint32
	state          atomic.Uint32
}

// PageBuffer returns page's reusable staging buffer.
func (b *Batch) PageBuffer(page int) ([]byte, error) {
	if b == nil || b.state.Load() != batchOwned || page < 0 || page >= len(b.pages) {
		return nil, ErrBatchState
	}
	return b.committer.buffers[b.pages[page].Buffer], nil
}

// SetPage records the initialized prefix and physical offset of one page.
func (b *Batch) SetPage(page int, offset int64, length int) error {
	return b.setPage(page, offset, length, 0)
}

// setStorePage records the kind of a page already verified by
// TransactionPage.Stage. The marker is private to the durable batching
// optimizer; generic Batch users retain byte-for-byte write semantics.
func (b *Batch) setStorePage(
	page int,
	offset int64,
	length int,
	kind PageKind,
) error {
	return b.setPage(page, offset, length, kind)
}

func (b *Batch) setPage(
	page int,
	offset int64,
	length int,
	kind PageKind,
) error {
	if b == nil || b.state.Load() != batchOwned || page < 0 || page >= len(b.pages) {
		return ErrBatchState
	}
	if length < 0 || uint64(length) > uint64(^uint32(0)) {
		return ErrInvalidWrite
	}
	b.pages[page].Offset = offset
	b.pages[page].Length = uint32(length)
	b.pages[page].kind = kind
	return nil
}

// ResizePages returns unused trailing page buffers before publication. It is
// useful when a bounded copy-on-write operation reserves its worst-case tree
// height before it knows whether a split or prune is required. A batch cannot
// grow after Begin; every retained page must still be initialized with
// SetPage.
func (b *Batch) ResizePages(pageCount int) error {
	if b == nil || b.state.Load() != batchOwned || pageCount < 0 || pageCount > len(b.pages) {
		return ErrBatchState
	}
	for i := pageCount; i < len(b.pages); i++ {
		b.committer.freeBuffers.push(uint32(b.pages[i].Buffer))
		b.pages[i] = Write{}
	}
	b.pages = b.pages[:pageCount]
	return nil
}

// RootBuffer returns the alternate-root staging buffer.
func (b *Batch) RootBuffer() ([]byte, error) {
	if b == nil || b.state.Load() != batchOwned {
		return nil, ErrBatchState
	}
	return b.committer.buffers[b.root.Buffer], nil
}

// SetRoot records the initialized root prefix and physical offset.
func (b *Batch) SetRoot(offset int64, length int) error {
	if b == nil || b.state.Load() != batchOwned {
		return ErrBatchState
	}
	if length < 0 || uint64(length) > uint64(^uint32(0)) {
		return ErrInvalidWrite
	}
	b.root.Offset = offset
	b.root.Length = uint32(length)
	b.rootGeneration = 0
	return nil
}

// SetSuperblock encodes a checksummed double-root record into the reusable root
// buffer and selects its alternate fixed page. The complete page is cleared
// and committed: besides removing stale tail bytes, page-sized root writes
// retain the offset/length alignment required by direct I/O. Recovery decodes
// only the fixed record prefix. Publish must receive the same generation,
// preventing a durable-generation counter from naming different on-disk
// state. No allocation is performed.
func (b *Batch) SetSuperblock(root Superblock) error {
	if b == nil || b.state.Load() != batchOwned {
		return ErrBatchState
	}
	buffer := b.committer.buffers[b.root.Buffer]
	if uint64(root.PageSize) > uint64(len(buffer)) {
		return ErrInvalidWrite
	}
	page := buffer[:root.PageSize]
	clear(page)
	_, err := EncodeSuperblock(page, root)
	if err != nil {
		return err
	}
	offset, err := SuperblockOffset(root.Generation, root.PageSize)
	if err != nil {
		return err
	}
	b.root.Offset = offset
	b.root.Length = root.PageSize
	b.rootGeneration = root.Generation
	return nil
}

// Publish transfers the batch to the background writer without allocating.
// generation must be greater than every previously published generation.
func (b *Batch) Publish(generation uint64) error {
	if b == nil || b.state.Load() != batchOwned {
		return ErrBatchState
	}
	return b.committer.publish(b, generation)
}

// Abort returns every buffer and descriptor without publishing the batch.
func (b *Batch) Abort() error {
	if b == nil || b.state.Load() != batchOwned {
		return ErrBatchState
	}
	b.committer.release(b)
	return nil
}

// CommitterStats is a lock-free snapshot of automatic persistence progress.
type CommitterStats struct {
	Backend              Backend
	PublishedGeneration  uint64
	DurableGeneration    uint64
	QueuedGenerations    uint64
	DeviceCommits        uint64
	CommittedBatches     uint64
	LargestGroup         uint32
	SuppressedRootWrites uint64
	SuppressedRootBytes  uint64
}

// Committer turns synchronous Device commits into automatic asynchronous
// persistence. One serialized Store writer is the producer; one private
// background worker is the consumer and sole Device owner. Readers never load
// Committer state.
type Committer struct {
	deviceOptions DeviceOptions
	options       CommitterOptions
	device        Device
	backend       Backend

	buffers      [][]byte
	bufferSize   int
	bufferCount  int
	freeBuffers  *indexPool
	freeBatches  *indexPool
	batches      []Batch
	writeStorage []Write
	producerSeen []uint64

	pending     []*Batch
	pendingMask uint64
	head        atomic.Uint64
	tail        atomic.Uint64
	wake        chan struct{}
	workerWait  atomic.Uint32
	stop        chan struct{}
	done        chan struct{}
	closeOnce   sync.Once
	closing     atomic.Bool
	publishers  atomic.Uint32

	published atomic.Uint64
	durable   atomic.Uint64
	failure   atomic.Pointer[commitFailure]
	failed    chan struct{}
	failOnce  sync.Once

	waitMu sync.Mutex
	wait   *sync.Cond

	commitScratch        []Write
	groupScratch         []*Batch
	deviceCommits        atomic.Uint64
	batchesDone          atomic.Uint64
	largestGroup         atomic.Uint32
	suppressedRootWrites atomic.Uint64
	suppressedRootBytes  atomic.Uint64
}

type committerInit struct{ err error }

type deviceOpener func(*os.File, DeviceOptions) (Device, error)

// NewCommitter starts a bounded background writer over file. Construction
// waits until the selected Device and every reusable buffer are ready.
func NewCommitter(file *os.File, deviceOptions DeviceOptions, options CommitterOptions) (*Committer, error) {
	return newCommitter(file, deviceOptions, options, OpenDevice)
}

func newCommitter(file *os.File, deviceOptions DeviceOptions, options CommitterOptions, open deviceOpener) (*Committer, error) {
	normalizedDevice, err := deviceOptions.normalized()
	if err != nil {
		return nil, err
	}
	normalizedCommitter, err := options.normalized(normalizedDevice.BufferCount)
	if err != nil {
		return nil, err
	}
	c := &Committer{
		deviceOptions: normalizedDevice,
		options:       normalizedCommitter,
		bufferSize:    normalizedDevice.BufferSize,
		bufferCount:   normalizedDevice.BufferCount,
		freeBuffers:   newIndexPool(normalizedDevice.BufferCount),
		freeBatches:   newIndexPool(normalizedCommitter.QueueSlots),
		batches:       make([]Batch, normalizedCommitter.QueueSlots),
		writeStorage:  make([]Write, normalizedCommitter.QueueSlots*normalizedCommitter.MaxPagesPerBatch),
		producerSeen:  make([]uint64, (normalizedDevice.BufferCount+63)/64),
		pending:       make([]*Batch, normalizedCommitter.QueueSlots),
		pendingMask:   uint64(normalizedCommitter.QueueSlots - 1),
		wake:          make(chan struct{}, 1),
		stop:          make(chan struct{}),
		done:          make(chan struct{}),
		failed:        make(chan struct{}),
		commitScratch: make([]Write, 0, normalizedDevice.BufferCount),
		groupScratch:  make([]*Batch, 0, normalizedCommitter.GroupLimit),
	}
	c.wait = sync.NewCond(&c.waitMu)
	for i := range c.batches {
		start := i * normalizedCommitter.MaxPagesPerBatch
		batch := &c.batches[i]
		batch.committer = c
		batch.index = uint32(i)
		batch.pages = c.writeStorage[start : start : start+normalizedCommitter.MaxPagesPerBatch]
	}
	initialized := make(chan committerInit, 1)
	go c.run(file, initialized, open)
	if result := <-initialized; result.err != nil {
		return nil, result.err
	}
	return c, nil
}

// Begin acquires one reusable descriptor and pageCount+1 staging buffers. It
// applies bounded backpressure when the persistence worker owns all capacity.
func (c *Committer) Begin(pageCount int) (*Batch, error) {
	if c == nil {
		return nil, ErrClosed
	}
	if failure := c.failure.Load(); failure != nil {
		return nil, failure.err
	}
	if c.closing.Load() {
		return nil, ErrClosed
	}
	if pageCount < 0 || pageCount > c.options.MaxPagesPerBatch {
		return nil, ErrTooManyPages
	}
	batchIndex, err := c.acquire(c.freeBatches)
	if err != nil {
		return nil, err
	}
	batch := &c.batches[batchIndex]
	batch.pages = batch.pages[:pageCount]
	for i := 0; i <= pageCount; i++ {
		buffer, acquireErr := c.acquire(c.freeBuffers)
		if acquireErr != nil {
			c.releasePartial(batch, i)
			return nil, acquireErr
		}
		if i == pageCount {
			batch.root = Write{Buffer: uint16(buffer)}
		} else {
			batch.pages[i] = Write{Buffer: uint16(buffer)}
		}
	}
	if failure := c.failure.Load(); failure != nil {
		c.release(batch)
		return nil, failure.err
	}
	if c.closing.Load() {
		c.release(batch)
		return nil, ErrClosed
	}
	batch.state.Store(batchOwned)
	return batch, nil
}

func (c *Committer) acquire(pool *indexPool) (uint32, error) {
	for {
		if failure := c.failure.Load(); failure != nil {
			return 0, failure.err
		}
		if c.closing.Load() {
			return 0, ErrClosed
		}
		if index, ok := pool.pop(); ok {
			return index, nil
		}
		pool.waiter.Add(1)
		if index, ok := pool.pop(); ok {
			pool.waiter.Add(^uint32(0))
			return index, nil
		}
		select {
		case <-pool.notify:
		case <-c.failed:
			pool.waiter.Add(^uint32(0))
			return 0, c.failure.Load().err
		case <-c.stop:
			pool.waiter.Add(^uint32(0))
			return 0, ErrClosed
		}
		pool.waiter.Add(^uint32(0))
	}
}

func (c *Committer) publish(batch *Batch, generation uint64) error {
	if failure := c.failure.Load(); failure != nil {
		return failure.err
	}
	if !c.enterPublish() {
		if failure := c.failure.Load(); failure != nil {
			return failure.err
		}
		return ErrClosed
	}
	defer c.publishers.Add(^uint32(0))
	if failure := c.failure.Load(); failure != nil {
		return failure.err
	}
	if generation == 0 || generation <= c.published.Load() {
		return ErrGenerationOrder
	}
	if batch.rootGeneration != 0 && batch.rootGeneration != generation {
		return ErrGenerationOrder
	}
	if err := validateCommit(c.bufferCount, c.bufferSize, c.producerSeen, batch.pages, batch.root); err != nil {
		return err
	}
	if c.closing.Load() {
		return ErrClosed
	}
	tail := c.tail.Load()
	if tail-c.head.Load() >= uint64(len(c.pending)) {
		return ErrQueueFull
	}
	batch.generation = generation
	batch.state.Store(batchPublished)
	c.pending[tail&c.pendingMask] = batch
	c.published.Store(generation)
	c.tail.Store(tail + 1)
	if c.workerWait.Load() != 0 {
		select {
		case c.wake <- struct{}{}:
		default:
		}
	}
	return nil
}

func (c *Committer) enterPublish() bool {
	if c.closing.Load() {
		return false
	}
	c.publishers.Add(1)
	if c.closing.Load() {
		c.publishers.Add(^uint32(0))
		return false
	}
	return true
}

// PublishedGeneration returns the newest generation accepted by Publish.
func (c *Committer) PublishedGeneration() uint64 {
	if c == nil {
		return 0
	}
	return c.published.Load()
}

// DurableGeneration returns the newest generation whose root passed the final
// data-integrity barrier.
func (c *Committer) DurableGeneration() uint64 {
	if c == nil {
		return 0
	}
	return c.durable.Load()
}

// InitializeGeneration seeds a newly opened Committer with the generation
// already selected by crash recovery. It must be called before Begin or
// Publish and keeps Flush/Stats meaningful before the first new mutation.
func (c *Committer) InitializeGeneration(generation uint64) error {
	if c == nil || generation == 0 || c.closing.Load() || c.head.Load() != c.tail.Load() ||
		c.published.Load() != 0 || c.durable.Load() != 0 {
		return ErrGenerationOrder
	}
	c.published.Store(generation)
	c.durable.Store(generation)
	return nil
}

// Wait blocks until generation is durable or persistence fails/closes.
func (c *Committer) Wait(generation uint64) error {
	if c == nil {
		return ErrClosed
	}
	if generation > c.published.Load() {
		return ErrGenerationOrder
	}
	c.waitMu.Lock()
	defer c.waitMu.Unlock()
	for c.durable.Load() < generation {
		if failure := c.failure.Load(); failure != nil {
			return failure.err
		}
		select {
		case <-c.done:
			return ErrClosed
		default:
		}
		c.wait.Wait()
	}
	return nil
}

// Flush waits for the newest generation published before the call.
func (c *Committer) Flush() error { return c.Wait(c.PublishedGeneration()) }

// Stats returns current queue and group-commit counters.
func (c *Committer) Stats() CommitterStats {
	if c == nil {
		return CommitterStats{}
	}
	published := c.published.Load()
	durable := c.durable.Load()
	queued := c.tail.Load() - c.head.Load()
	return CommitterStats{
		Backend:              c.backend,
		PublishedGeneration:  published,
		DurableGeneration:    durable,
		QueuedGenerations:    queued,
		DeviceCommits:        c.deviceCommits.Load(),
		CommittedBatches:     c.batchesDone.Load(),
		LargestGroup:         c.largestGroup.Load(),
		SuppressedRootWrites: c.suppressedRootWrites.Load(),
		SuppressedRootBytes:  c.suppressedRootBytes.Load(),
	}
}

// Close drains every published batch, closes the Device, and returns any
// sticky persistence failure. It is idempotent.
func (c *Committer) Close() error {
	if c == nil {
		return nil
	}
	c.closeOnce.Do(func() {
		c.stopAccepting()
		close(c.stop)
	})
	<-c.done
	if failure := c.failure.Load(); failure != nil {
		return failure.err
	}
	return nil
}
