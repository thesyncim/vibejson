package query

import (
	"bufio"
	"container/heap"
	"encoding/gob"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/thesyncim/vibejson/internal/byteview"
	"github.com/thesyncim/vibejson/store"
	"github.com/thesyncim/vibejson/store/durable"
)

// ExecOptions controls bounded batch execution over a [durable.Snapshot], plus
// the one bound a heap join execution takes. The zero value selects one worker
// per GOMAXPROCS, the default batch and memory targets, and the default join
// threshold, so a caller who does not care never has to fill it in. Heap
// sources ignore everything but JoinMembershipMax, having neither worker
// batches nor a spill frontier to bound.
//
// MemoryBytes is a working-memory target, not a limit on the returned Result:
// a caller asking for an unbounded projection necessarily owns output
// proportional to the number and size of selected rows. One oversized JSON
// document may also exceed the target by itself.
type ExecOptions struct {
	Workers        int
	BatchRows      int
	BatchBytes     int64
	MemoryBytes    int64
	SpillDirectory string

	// JoinMembershipMax is how many inner join-key values a semi-join collects
	// before it abandons the membership strategy and drives the join as a
	// per-row keyed lookup instead. Zero selects the measured default; see
	// joinMembershipMax in join.go for how that number was arrived at.
	//
	// It is exposed because it is the one knob whose right value depends on the
	// caller's data rather than on this package's measurements, and because the
	// two strategies must be independently exercisable: forcing it to either
	// extreme makes the same query take the other branch, which is how the
	// differential proves the adaptive choice changes cost and never the answer.
	// It never changes which rows a query returns.
	JoinMembershipMax int

	// JoinFilterScanRatio is how many inner rows a semi-join clause that has
	// outgrown its membership will scan, per outer row, to build the prefilter
	// that lets it skip probes. Zero selects the measured default
	// (joinBloomScanRatio); a negative value declines the filter outright and
	// probes every outer row.
	//
	// It bounds the work spent on a filter whose selectivity cannot be known
	// until it is used, and it is exposed for the same two reasons the
	// threshold above is: a caller whose inner rows are unusually cheap or
	// unusually expensive to scan knows something this package cannot, and both
	// arms have to be independently exercisable to be independently measured.
	// Like the threshold, it never changes which rows a query returns — the
	// filter's error is one-sided and the exact probe behind it is
	// authoritative.
	JoinFilterScanRatio int
}

// fileWorkspace owns the reusable storage one durable execution needs beyond
// the general [Workspace]: the persistent-index probe I/O workspace, the
// residual bitmap, the typed cover reductions, the group frontier, and — since
// the batched executor stopped minting its scan storage per batch — the
// per-worker scan workspaces, the in-flight batch ring, and the merge
// frontier. It lives unexported inside [Exec] because it is neither
// configuration nor output — the previous separate handle in the options
// struct only made callers carry a second workspace whose lifetime already
// matched the Exec's.
//
// Everything here is high-water capacity, retained so a second execution of
// the same shape finds it warm. That is the whole point: the batched executor
// used to declare a Workspace, a batch pair of buffers, and an accumulator
// inside each batch, which meant no amount of Exec reuse could ever warm the
// scan — every BatchRows documents re-grew all of it from empty.
type fileWorkspace struct {
	index         durable.IndexWorkspace
	overflow      []byte
	reductions    []store.Float64Aggregate
	coverPaths    []string
	accs          []aggAcc
	indexGroups   []durable.IndexScalarGroup
	indexResidual []store.Mask
	fileGroups    []fileGroup

	// workers is one scan Workspace per worker goroutine, indexed by worker
	// number. Indexing by worker rather than by batch is deliberate: nothing a
	// Workspace holds outlives the makeFilePartial call that filled it (every
	// value a partial keeps is detached first), so a worker can reuse its own
	// across consecutive batches and keep its column, text, selection, and
	// shape-cache storage warm for the whole execution.
	workers []Workspace
	// segments is one scan Segment per worker goroutine, indexed like workers
	// and reset rather than rebuilt between batches. It is a slice of pointers
	// because a Segment carries a mutex, so a slice of values could not be grown
	// by copy, and because a worker's segment must keep its arena identity across
	// a resize.
	//
	// This is the same argument as workers, and it is where the batched
	// executor's remaining cost lived: nothing the Segment holds outlives the
	// makeFilePartial call that filled it, because every value a partial keeps is
	// copied out of the batch by ownScalars first, so a worker may reuse its own
	// across consecutive batches. Minting one per batch instead meant every batch
	// re-grew a source arena from 8 KiB and an entry arena from 512 entries,
	// doubling and discarding all the way to the batch's size — 411 of the 430
	// allocations a warm execution made, and essentially all of its bytes.
	segments []*store.Segment
	// slots is the in-flight batch ring, indexed by batch sequence modulo its
	// length. See fileSlot for why the modulus is safe.
	slots []fileSlot
	// rows and groups are the ordered-projection and grouped merge frontiers,
	// and mergedGroups the flattened group set handed to materialization.
	rows         []fileRow
	groups       map[string]*fileGroup
	mergedGroups []fileGroup
	rowRuns      []spillRun
	groupRuns    []spillRun
	spillFiles   map[string]struct{}
}

// release drops storage retained by durable index planning and batch scanning.
func (w *fileWorkspace) release() {
	if w == nil {
		return
	}
	w.index.Release()
	w.overflow = nil
	w.reductions = nil
	w.coverPaths = nil
	w.accs = nil
	w.indexGroups = nil
	w.indexResidual = nil
	w.fileGroups = nil
	w.workers = nil
	w.segments = nil
	w.slots = nil
	w.rows = nil
	w.groups = nil
	w.mergedGroups = nil
	w.rowRuns = nil
	w.groupRuns = nil
	w.spillFiles = nil
}

// A fileSlot is the scratch one in-flight batch sequence owns: the scan
// buffers the scanner fills, the partial-result storage the worker fills, and
// the reorder buffer the consumer parks the finished partial in until its turn
// comes. Slots form a ring indexed by batch sequence modulo len(slots).
//
// The ring is what makes batch storage reusable without a pool or a free list,
// and its correctness rests entirely on the credit protocol. The scanner
// acquires one credit before handing a batch to the workers and the consumer
// releases one after consuming that batch's partial in sequence order, so with
// a credit channel of capacity 2*workers the scanner can never be more than
// 2*workers batches ahead of the consumer. Sizing the ring at 2*workers+1
// therefore guarantees that by the time sequence s+len(slots) touches a slot,
// sequence s has been scanned, reduced, and consumed, and nothing still refers
// to what is about to be overwritten.
//
// Only storage the consumer copies out of belongs here. A projected row's
// detached scalars and a group's accumulators do not: the consumer retains
// those slices themselves until the execution materializes its Result, so they
// are allocated per row and per group and freed with the merge frontier — which
// is exactly what lets a spill hand their bytes back.
type fileSlot struct {
	batch   fileBatch
	accs    []aggAcc
	rows    []fileRow
	groups  []fileGroup
	byKey   map[string]int
	partial filePartial
	ready   bool
}

// fileSlotCount is the batch ring length for w workers. See fileSlot.
func fileSlotCount(workers int) int { return workers*2 + 1 }

// clearTail zeroes the elements of s past n, releasing whatever a shorter
// refill left reachable through retained capacity while keeping the storage.
func clearTail[T any](s []T, n int) {
	if n < len(s) {
		clear(s[n:])
	}
}

// ExecStats describes the physical work performed by the last execution into
// an [Exec]. The durable backend measures the scan and index fields; heap
// sources reset them. The Join fields are the exception: they are written by
// whichever backend bound the join clauses, which today is the heap database
// source, because which strategy a join measured its way into is the one
// execution decision this package makes that a caller cannot predict from the
// plan.
// RowsTotal is the snapshot cardinality while
// RowsScanned is the number of JSON documents admitted to execution after
// persistent-index pushdown. IndexCertificateRows were decided from a
// collision-free posting representative or compact categorical cover without
// opening JSON;
// IndexRecheckRows required exact document comparison. An ordinary
// IndexBounded execution still evaluates the complete predicate. BufferedBytes
// is the largest observed batch or in-memory merge frontier; it excludes the
// caller-owned final Result. CoveringColumns counts distinct typed columns
// reduced without admitting JSON.
type ExecStats struct {
	Workers              int
	RowsTotal            uint64
	RowsScanned          uint64
	Batches              uint64
	PeakBatchRows        int
	PeakBatchBytes       int64
	BufferedBytes        int64
	SpillRuns            uint64
	SpilledBytes         int64
	IndexBounded         bool
	IndexLookups         int
	IndexPostingPages    int
	IndexCertificateRows uint64
	IndexRecheckRows     uint64
	CandidateRows        uint64
	CandidateChunks      int
	CoveringColumns      int
	IndexGroupedRows     uint64
	IndexGroups          int

	// JoinMemberships counts the join clauses whose inner side fit under the
	// threshold and were pushed into the outer predicate as a membership;
	// JoinLookups counts those that overflowed it and are answered by a probe
	// per outer row. The two sum to the plan's join count.
	JoinMemberships int
	JoinLookups     int
	// JoinKeys is the total number of distinct inner join-key values collected
	// by the membership-bound clauses, after deduplication. JoinProbes is the
	// number of inner-collection lookups the lookup-bound clauses performed —
	// after any prefilter, so it is the probes that actually ran rather than
	// the outer rows that reached the join.
	JoinKeys   uint64
	JoinProbes uint64
	// JoinFilters counts the lookup-bound clauses that also built a semi-join
	// reduction filter, and JoinFilterKeys the inner keys those filters
	// summarize. JoinFilterRejected is how many outer rows the filters answered
	// on their own; it is the work the filters saved, and it is zero for a
	// filter that turned out to admit everything.
	JoinFilters        int
	JoinFilterKeys     uint64
	JoinFilterRejected uint64
	// JoinBuilds counts the clauses that fanned out, and JoinBuildRows the
	// joined rows those builds materialized. JoinPairs is how many (driving,
	// joined) pairs the expansion produced, which is the result's row count
	// before grouping, ordering, and limiting. A query with no fan-out reports
	// zero for all three, which is how a caller tells the two operators apart
	// without inspecting the plan.
	JoinBuilds    int
	JoinBuildRows uint64
	JoinPairs     uint64
}

const (
	defaultFileMemory = int64(64 << 20)
	defaultBatchRows  = 4096
	maxSpillFanIn     = 32
)

var errFileExecutionStopped = errors.New("query: file execution stopped")

type normalizedFileOptions struct {
	workers     int
	batchRows   int
	batchBytes  int64
	memoryBytes int64
	mergeBytes  int64
	spillDir    string
}

func normalizeFileOptions(opts ExecOptions) (normalizedFileOptions, error) {
	workers := opts.Workers
	if workers == 0 {
		workers = runtime.GOMAXPROCS(0)
	}
	if workers < 1 {
		return normalizedFileOptions{}, fmt.Errorf("query: Workers must be positive")
	}
	memoryBytes := opts.MemoryBytes
	if memoryBytes == 0 {
		memoryBytes = defaultFileMemory
	}
	if memoryBytes < 64<<10 {
		return normalizedFileOptions{}, fmt.Errorf("query: MemoryBytes must be at least 64 KiB")
	}
	batchRows := opts.BatchRows
	if batchRows == 0 {
		batchRows = defaultBatchRows
	}
	if batchRows < 1 {
		return normalizedFileOptions{}, fmt.Errorf("query: BatchRows must be positive")
	}
	batchBytes := opts.BatchBytes
	if batchBytes == 0 {
		// At most two batches per worker are admitted by the job queue. Leave
		// half the target for indexes, worker columns, and the merge frontier.
		batchBytes = memoryBytes / int64(workers*4)
		if batchBytes < 16<<10 {
			batchBytes = 16 << 10
		}
	}
	if batchBytes < 1 {
		return normalizedFileOptions{}, fmt.Errorf("query: BatchBytes must be positive")
	}
	return normalizedFileOptions{
		workers: workers, batchRows: batchRows, batchBytes: batchBytes,
		memoryBytes: memoryBytes, mergeBytes: memoryBytes / 2,
		spillDir: opts.SpillDirectory,
	}, nil
}

// runFileInto executes p over a page-backed snapshot in parallel batches,
// under e.Options and into e's Result, Workspace, and durable planning
// storage. Ordered projections and grouped reductions spill sorted runs once
// their merge frontier reaches MemoryBytes; spill merges open at most 32 files
// at a time. Repeated calls reuse the Result's column cells and packed
// variable-width value arena, so once their observed high-water marks fit,
// result materialization allocates nothing. Materialized cells own their bytes
// and stay valid after the snapshot is closed, until e is reused or released.
// The snapshot stays owned by the caller.
func (p *plan) runFileInto(e *Exec, snapshot *durable.Snapshot) error {
	e.Result.fileData = e.Result.fileData[:0]
	e.Stats = ExecStats{}
	n, err := normalizeFileOptions(e.Options)
	if err != nil {
		return err
	}
	if snapshot == nil {
		return fmt.Errorf("query: FromFile was given a nil snapshot")
	}
	stats := ExecStats{Workers: n.workers, RowsTotal: snapshot.Len()}
	directIndex, handled, directErr := p.runDirectFileIndexedCount(snapshot, e)
	if handled {
		stats.IndexBounded = directIndex.bounded
		stats.IndexLookups = directIndex.lookups
		stats.IndexPostingPages = directIndex.postingPages
		stats.IndexCertificateRows = directIndex.certificates
		stats.IndexRecheckRows = directIndex.rechecks
		stats.CandidateRows = directIndex.rows
		stats.CandidateChunks = directIndex.chunks
		e.Stats = stats
		return directErr
	}
	coveringColumns, handled, directErr := p.runDirectFileAggregate(snapshot, e)
	if handled {
		stats.CoveringColumns = coveringColumns
		e.Stats = stats
		return directErr
	}
	groupStats, handled, directErr := p.runDirectFileIndexGroups(snapshot, e)
	if handled {
		stats.IndexLookups = groupStats.lookups
		stats.IndexPostingPages = groupStats.postingPages
		stats.IndexCertificateRows = groupStats.certificates
		stats.IndexRecheckRows = groupStats.rechecks
		stats.RowsScanned = groupStats.rechecks
		stats.IndexGroupedRows = groupStats.certificates
		stats.IndexGroups = groupStats.groups
		e.Stats = stats
		return directErr
	}
	result, stats, err := p.runFileSnapshotBatched(e, snapshot, n, stats)
	e.Result, e.Stats = result, stats
	return err
}

// runFileSnapshotBatched is kept outside the direct covering dispatcher so
// goroutine captures in the general executor cannot force the fast path's
// stats and fallback workspace onto the heap.
func (p *plan) runFileSnapshotBatched(
	e *Exec,
	snapshot *durable.Snapshot,
	n normalizedFileOptions,
	stats ExecStats,
) (Result, ExecStats, error) {
	// The result travels as a local value so the merge and materialization
	// tails below stay one expression each; the caller stores it back into the
	// Exec. Taking it from e keeps its retained cell and arena capacity.
	result := e.Result
	candidateMasks, err := p.fileCandidateMasks(snapshot, &e.file.index, &e.Workspace)
	if err != nil {
		return result, stats, err
	}
	stats.IndexLookups = e.Workspace.storeIndexProbes
	stats.IndexBounded = candidateMasks != nil
	if stats.IndexBounded {
		for _, mask := range candidateMasks {
			rows := bits.OnesCount64(mask.Bits)
			if rows == 0 {
				continue
			}
			stats.CandidateRows += uint64(rows)
			stats.CandidateChunks++
		}
	}
	spills := newSpillManager(n.spillDir, &stats, e.file.spillFiles)
	defer func() { e.file.spillFiles = spills.cleanup() }()

	slots := resize(e.file.slots, fileSlotCount(n.workers))
	e.file.slots = slots
	e.file.workers = resize(e.file.workers, n.workers)
	e.file.segments = resize(e.file.segments, n.workers)

	jobs := make(chan fileBatch, n.workers)
	partials := make(chan filePartial, n.workers)
	credits := make(chan struct{}, n.workers*2)
	scanDone := make(chan fileScanResult, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	cancel := func() { stopOnce.Do(func() { close(stop) }) }

	go scanFileBatches(snapshot, candidateMasks, &e.file.overflow, slots, n, jobs, credits, scanDone, stop)
	var workers sync.WaitGroup
	workers.Add(n.workers)
	for worker := range n.workers {
		go func() {
			defer workers.Done()
			w := &e.file.workers[worker]
			// The worker's scan Segment is minted on the execution that first
			// needs this worker number and reset by every batch after, so a
			// warm Exec allocates none at all.
			if e.file.segments[worker] == nil {
				e.file.segments[worker] = &store.Segment{}
			}
			docs := e.file.segments[worker]
			for batch := range jobs {
				part := p.makeFilePartial(batch, w, docs, slots)
				if part.err != nil {
					cancel()
				}
				partials <- part
			}
		}()
	}
	go func() {
		workers.Wait()
		close(partials)
	}()

	var firstErr error
	rows := e.file.rows[:0]
	var rowBytes int64
	rowRuns := e.file.rowRuns[:0]
	var accs []aggAcc
	if e.file.groups == nil {
		e.file.groups = make(map[string]*fileGroup)
	}
	groups := e.file.groups
	clear(groups)
	var groupBytes int64
	groupRuns := e.file.groupRuns[:0]

	consume := func(part filePartial) {
		if part.err != nil {
			if firstErr == nil {
				firstErr = part.err
			}
			return
		}
		if firstErr != nil {
			return
		}
		switch {
		case p.grouped:
			for i := range part.groups {
				g := &part.groups[i]
				if dst := groups[g.key]; dst != nil {
					mergeAggs(dst.accs, g.accs)
					if g.first < dst.first {
						dst.first = g.first
					}
					continue
				}
				copy := *g
				groups[g.key] = &copy
				groupBytes += g.bytes
			}
			if groupBytes >= n.mergeBytes && len(groups) != 0 {
				run, spillErr := spills.writeGroups(groups)
				if spillErr != nil {
					firstErr = spillErr
					cancel()
				} else {
					groupRuns = append(groupRuns, run)
					// Clearing rather than replacing the map keeps its
					// bucket capacity for the next fill while still
					// dropping every reference to the spilled groups —
					// handing that memory back is the whole point of
					// the spill.
					clear(groups)
					groupBytes = 0
				}
			}
		case p.singleRow:
			if accs == nil {
				accs = resize(e.file.accs, len(p.columns))
				clear(accs)
				e.file.accs = accs
			}
			mergeAggs(accs, part.accs)
		default:
			rows = append(rows, part.rows...)
			rowBytes += part.bytes
			// An unordered LIMIT needs only the earliest source ordinals.
			if len(p.order) == 0 && p.hasLimit && len(rows) > p.limit*2+1 {
				slices.SortFunc(rows, compareFileOrdinal)
				rows = rows[:p.limit]
				rowBytes = estimateRows(rows)
			}
			if len(p.order) != 0 && rowBytes >= n.mergeBytes && len(rows) != 0 {
				run, spillErr := spills.writeRows(p, rows)
				if spillErr != nil {
					firstErr = spillErr
					cancel()
				} else {
					rowRuns = append(rowRuns, run)
					// The spilled rows own the bytes they detached, so
					// truncating alone would leave the retained header
					// array pinning every one of them and the frontier
					// would never actually shrink. Clearing first drops
					// the references and keeps only the capacity.
					clear(rows)
					rows = rows[:0]
					rowBytes = 0
				}
			}
		}
		if part.bytes > stats.BufferedBytes {
			stats.BufferedBytes = part.bytes
		}
		if rowBytes+groupBytes > stats.BufferedBytes {
			stats.BufferedBytes = rowBytes + groupBytes
		}
	}
	// Partials arrive in completion order and must be consumed in sequence
	// order. The reorder buffer is the batch ring rather than a map keyed by
	// sequence: the credit protocol already bounds the number of in-flight
	// sequences to the ring's length, so a slot is always free when its
	// sequence arrives, and a fixed ring costs no hashing and no per-execution
	// map.
	nextSequence := uint64(0)
	for part := range partials {
		slots[part.seq%uint64(len(slots))].partial = part
		slots[part.seq%uint64(len(slots))].ready = true
		for {
			slot := &slots[nextSequence%uint64(len(slots))]
			if !slot.ready {
				break
			}
			slot.ready = false
			part := slot.partial
			slot.partial = filePartial{}
			consume(part)
			<-credits
			nextSequence++
		}
	}
	// The locals above grew by append; handing their backing arrays back to
	// the Exec is what makes the next execution of the same shape find them
	// warm.
	e.file.rows, e.file.rowRuns, e.file.groupRuns = rows, rowRuns, groupRuns
	scan := <-scanDone
	stats.RowsScanned = scan.rows
	stats.Batches = scan.batches
	stats.PeakBatchRows = scan.peakRows
	stats.PeakBatchBytes = scan.peakBytes
	if scan.err != nil && !errors.Is(scan.err, errFileExecutionStopped) && firstErr == nil {
		firstErr = scan.err
	}
	if firstErr != nil {
		clear(rows)
		e.file.rows = rows[:0]
		return result, stats, firstErr
	}

	switch {
	case p.grouped:
		if len(groupRuns) != 0 {
			if len(groups) != 0 {
				run, spillErr := spills.writeGroups(groups)
				if spillErr != nil {
					return result, stats, spillErr
				}
				groupRuns = append(groupRuns, run)
			}
			groupRuns, err = spills.reduceGroupRuns(groupRuns)
			e.file.groupRuns = groupRuns
			if err != nil {
				return result, stats, err
			}
			var merged []fileGroup
			merged, err = spills.readMergedGroups(e.file.mergedGroups[:0], groupRuns)
			e.file.mergedGroups = merged
			if err != nil {
				return result, stats, err
			}
			return p.fileGroupResultInto(result, merged), stats, nil
		}
		merged := resize(e.file.mergedGroups, 0)
		for _, g := range groups {
			merged = append(merged, *g)
		}
		e.file.mergedGroups = merged
		return p.fileGroupResultInto(result, merged), stats, nil
	case p.singleRow:
		if accs == nil {
			accs = resize(e.file.accs, len(p.columns))
			clear(accs)
			e.file.accs = accs
		}
		resultRows := 1
		if p.hasLimit && p.limit == 0 {
			resultRows = 0
		}
		prepareResult(&result, p, resultRows)
		if resultRows != 0 {
			p.fillAggregateCells(&result, 0, accs, nil, &e.Workspace)
		}
		return result, stats, nil
	default:
		if len(rowRuns) != 0 {
			if len(rows) != 0 {
				run, spillErr := spills.writeRows(p, rows)
				if spillErr != nil {
					return result, stats, spillErr
				}
				rowRuns = append(rowRuns, run)
				clear(rows)
				rows = rows[:0]
			}
			rowRuns, err = spills.reduceRowRuns(p, rowRuns)
			e.file.rowRuns = rowRuns
			if err != nil {
				return result, stats, err
			}
			rows, err = spills.readMergedRows(p, rowRuns, p.resultLimit(), rows[:0])
			if err != nil {
				e.file.rows = rows
				return result, stats, err
			}
		} else if len(p.order) != 0 {
			slices.SortStableFunc(rows, p.compareFileRows)
		} else {
			slices.SortFunc(rows, compareFileOrdinal)
		}
		if limit := p.resultLimit(); limit >= 0 && len(rows) > limit {
			rows = rows[:limit]
		}
		result = p.fileRowResultInto(result, rows)
		// Every cell now owns its bytes inside the Result's arena, so the
		// frontier's detached scalars have no reader left. Keeping the header
		// array but clearing it retains the capacity that makes the next
		// execution allocation-free without pinning a whole result's worth of
		// copied document bytes between calls.
		clear(rows)
		e.file.rows = rows[:0]
		return result, stats, nil
	}
}

type fileBatch struct {
	seq   uint64
	base  uint64
	data  []byte
	ends  []int
	bytes int64
}

// takeFileBatch rewinds the ring slot sequence seq owns and returns it ready to
// fill. The buffers survive from the last time this slot was used, which is why
// a warm execution scans without allocating: the document bytes of a batch are
// appended into a buffer that already reached the corpus's batch high-water
// mark, instead of one that starts at 64 KiB and doubles its way there once per
// batch, discarding every intermediate.
//
// Any buffer bought here goes into the slot before the batch is returned, not
// only when the batch is later flushed. A scan always takes one batch it never
// fills — the one after the last flush — and on a corpus whose batch count is
// below the ring length that batch's slot is reached by nothing else, so leaving
// the write-back to flush alone dropped a fresh 64 KiB buffer on the floor once
// per execution, forever. Storing at take time costs one slice-header write and
// makes the trailing batch as warm as every other.
func takeFileBatch(slots []fileSlot, seq, base uint64, opts normalizedFileOptions) fileBatch {
	slot := &slots[seq%uint64(len(slots))]
	b := slot.batch
	if cap(b.data) == 0 {
		b.data = make([]byte, 0, int(min(opts.batchBytes, 64<<10)))
	}
	if cap(b.ends) == 0 {
		b.ends = make([]int, 0, min(opts.batchRows, defaultBatchRows))
	}
	b.seq, b.base, b.bytes = seq, base, 0
	b.data, b.ends = b.data[:0], b.ends[:0]
	slot.batch = b
	return b
}

type fileScanResult struct {
	err       error
	rows      uint64
	batches   uint64
	peakRows  int
	peakBytes int64
}

func scanFileBatches(snapshot *durable.Snapshot, masks []store.Mask, overflow *[]byte, slots []fileSlot, opts normalizedFileOptions, jobs chan<- fileBatch, credits chan struct{}, done chan<- fileScanResult, stop <-chan struct{}) {
	defer close(jobs)
	var out fileScanResult
	batch := takeFileBatch(slots, 0, 0, opts)
	flush := func() error {
		if len(batch.ends) == 0 {
			return nil
		}
		// The grown buffers go back into the slot before the batch is
		// published: the slice headers travelling down the jobs channel are a
		// copy, so without this the capacity every append just bought would be
		// dropped on the floor the moment the local is reassigned.
		slots[batch.seq%uint64(len(slots))].batch = batch
		select {
		case credits <- struct{}{}:
		case <-stop:
			return errFileExecutionStopped
		}
		select {
		case jobs <- batch:
			out.batches++
			batch = takeFileBatch(slots, batch.seq+1, out.rows, opts)
			return nil
		case <-stop:
			<-credits
			return errFileExecutionStopped
		}
	}
	appendRow := func(_, value []byte) error {
		select {
		case <-stop:
			return errFileExecutionStopped
		default:
		}
		batch.data = append(batch.data, value...)
		batch.ends = append(batch.ends, len(batch.data))
		batch.bytes += int64(len(value))
		out.rows++
		if len(batch.ends) > out.peakRows {
			out.peakRows = len(batch.ends)
		}
		if batch.bytes > out.peakBytes {
			out.peakBytes = batch.bytes
		}
		if len(batch.ends) >= opts.batchRows || batch.bytes >= opts.batchBytes {
			return flush()
		}
		return nil
	}
	if masks == nil {
		*overflow, out.err = snapshot.RangeRawReadAheadBuffer((*overflow)[:0], appendRow)
	} else {
		*overflow, out.err = snapshot.RangeMasksRawBuffer(masks, (*overflow)[:0], appendRow)
	}
	if out.err == nil {
		out.err = flush()
	}
	done <- out
}

type filePartial struct {
	seq    uint64
	rows   []fileRow
	groups []fileGroup
	accs   []aggAcc
	bytes  int64
	err    error
}

type fileRow struct {
	values  []scalar
	order   []scalar
	ordinal uint64
}

type fileGroup struct {
	key     string
	scalars []scalar
	accs    []aggAcc
	first   uint64
	bytes   int64
}

// makeFilePartial reduces one batch of raw documents to a partial result. w is
// the calling worker's retained scan workspace, docs its retained scan Segment,
// and slots the in-flight batch ring; everything the partial hands to the
// consumer either comes from the ring slot this batch owns or is freshly
// allocated because the consumer keeps it past the slot's next reuse.
//
// Resetting docs rather than replacing it is safe for the same reason reusing w
// is: the partial this call returns holds nothing that points into the Segment.
// Projected and grouped values are copied into their own arena by ownScalars, a
// group key is cloned into a string, and an aggregate is a number, so by the
// time the worker takes its next batch the consumer's view of this one is
// already detached. See Segment.Reset for the contract that makes the
// distinction load-bearing: after the reset a retained Index would report the
// next batch's bytes rather than fault.
func (p *plan) makeFilePartial(batch fileBatch, w *Workspace, docs *store.Segment, slots []fileSlot) filePartial {
	part := filePartial{seq: batch.seq}
	slot := &slots[batch.seq%uint64(len(slots))]
	// The batch Segment carries no postings, and that is a decision, not an
	// omission. Postings are an inverted index over a Segment's top-level keys
	// and scalars; building one costs a hash and a bucket append per member of
	// every document, and this Segment is probed exactly once and then dropped,
	// so the build can never amortize. Measured on an unindexed filtered count
	// at 1% selectivity — the case most favourable to pruning, since the probe
	// discards 99% of the batch — enabling them cost 4.2 allocations per
	// scanned document, 210 of every 211 the whole backend made, and ran 1.9x
	// slower than the honest columnar scan they were meant to avoid. Real
	// pushdown happens before the scan, against the snapshot's persistent
	// indexes (fileCandidateMasks); once a batch is in hand the compiled
	// predicate's single pass over at most BatchRows rows is already cheaper
	// than indexing them. Candidate selection and the full scan return the same
	// rows by construction, so this changes cost only.
	docs.Reset()
	start := 0
	for _, end := range batch.ends {
		if _, err := docs.Append(batch.data[start:end]); err != nil {
			part.err = err
			return part
		}
		start = end
	}
	w.text = w.text[:0]
	w.groupKey = w.groupKey[:0]
	ctx := &w.ctx
	ctx.s, ctx.rows = docs, docs.Len()
	if err := ctx.extract(p, nil, w); err != nil {
		part.err = err
		return part
	}
	selected := p.selectRows(ctx, nil, false, w)
	switch {
	case p.grouped:
		if slot.byKey == nil {
			slot.byKey = make(map[string]int)
		}
		byKey := slot.byKey
		clear(byKey)
		prev := slot.groups
		groups := prev[:0]
		for _, row := range selected {
			// The grown key buffer goes back into the Workspace. Building the
			// key into w.groupKey[:0] and dropping the result meant every row
			// re-grew the encoding from zero capacity — one to two allocations
			// per grouped row, which no amount of Exec reuse could warm away
			// because nothing retained what the growth bought.
			w.groupKey = p.groupKey(w.groupKey[:0], ctx, row)
			id, ok := byKey[string(w.groupKey)]
			if !ok {
				id = len(groups)
				// One clone serves both the lookup key and the group's own
				// key. Cloning twice stored the same bytes twice for every
				// distinct group in every batch.
				key := string(w.groupKey)
				byKey[key] = id
				g := fileGroup{key: key, first: batch.base + uint64(row)}
				g.scalars, g.bytes = ownScalars(ctx, row, nil, nil, p.groupCols)
				g.accs = make([]aggAcc, len(p.columns))
				g.bytes += int64(len(g.key) + len(g.accs)*40 + 64)
				groups = append(groups, g)
				part.bytes += g.bytes
			}
			p.accumulate(groups[id].accs, ctx, row)
		}
		// Entries past this batch's group count still hold the previous
		// batch's key, detached scalars, and accumulators. Truncating alone
		// leaves them reachable through the retained capacity, so every ring
		// slot would pin one stale group set for the Exec's whole lifetime;
		// clearing releases them and keeps the storage.
		clearTail(prev, len(groups))
		slot.groups, part.groups = groups, groups
	case p.singleRow:
		accs := resize(slot.accs, len(p.columns))
		clear(accs)
		for _, row := range selected {
			p.accumulate(accs, ctx, row)
		}
		slot.accs, part.accs = accs, accs
		part.bytes = int64(len(accs) * 40)
	default:
		if len(p.order) != 0 {
			slices.SortStableFunc(selected, func(a, b int) int { return p.compareRows(ctx, a, b) })
		}
		if p.hasLimit && len(selected) > p.limit {
			selected = selected[:p.limit]
		}
		prev := slot.rows
		rows := prev[:0]
		for _, row := range selected {
			r := fileRow{ordinal: batch.base + uint64(row)}
			// The projected and ordering scalars share one header array split
			// in two, and every byte they detach is packed into one buffer, so
			// a row of c columns ordered by k keys costs two allocations
			// instead of the 2+2(c+k) that a slice per role plus a clone per
			// field used to cost.
			var used int64
			r.values, used = ownScalars(ctx, row, p.columns, p.order, nil)
			r.order = r.values[len(p.columns):len(r.values):len(r.values)]
			r.values = r.values[:len(p.columns):len(p.columns)]
			part.bytes += fileRowStructBytes + used
			rows = append(rows, r)
		}
		// Same reason as the grouped branch: a stale row header still owns the
		// bytes it detached from a batch two rings ago.
		clearTail(prev, len(rows))
		slot.rows, part.rows = rows, rows
	}
	return part
}

// ownScalars detaches one row's scalars — the value column of each entry in
// cols, then of each entry in order, then each of groupCols — into a single
// scalar header array backed by a single byte arena, and reports the retained
// size the spill accounting charges for them. Callers pass the two roles they
// have: a projected row passes cols and order, a group passes groupCols.
//
// Packing is what makes it worth having: the header array and the arena are one
// allocation each regardless of how many columns a row projects or orders by,
// and the arena is sized exactly, in one sizing pass, from the same aliasing
// tests ownScalarInto applies — so the two passes cannot disagree and overrun
// the buffer. Sizing exactly also means the arena is never grown, which is the
// invariant the returned views depend on: a later append that moved it would
// silently repoint every scalar handed out for an earlier column of the row.
func ownScalars(ctx *execCtx, row int, cols []planColumn, order []planOrder, groupCols []int) ([]scalar, int64) {
	n := len(cols) + len(order) + len(groupCols)
	if n == 0 {
		return nil, 0
	}
	need := 0
	for i := range cols {
		need += scalarOwnBytes(ctx.values[cols[i].value][row])
	}
	for i := range order {
		need += scalarOwnBytes(ctx.values[order[i].value][row])
	}
	for _, col := range groupCols {
		need += scalarOwnBytes(ctx.values[col][row])
	}
	// A zero-length arena still has to be non-nil, so that a scalar whose raw
	// bytes are empty but not nil keeps that shape — Cell.JSON distinguishes
	// the two. Hence the floor of one byte.
	arena := make([]byte, 0, max(need, 1))
	out := make([]scalar, 0, n)
	for i := range cols {
		var s scalar
		arena, s = ownScalarInto(arena, ctx.values[cols[i].value][row])
		out = append(out, s)
	}
	for i := range order {
		var s scalar
		arena, s = ownScalarInto(arena, ctx.values[order[i].value][row])
		out = append(out, s)
	}
	for _, col := range groupCols {
		var s scalar
		arena, s = ownScalarInto(arena, ctx.values[col][row])
		out = append(out, s)
	}
	return out, int64(n)*scalarStructBytes + int64(need)
}

// A classified scalar's fields overlap: classifyRawInto gives a number the same
// slice for num and raw, and gives an unescaped string an sval that points
// inside raw, between the quotes. Detaching each field independently would
// therefore store a number's digits twice and an unescaped string's content
// twice. Copying raw once and re-deriving the views that came from it keeps
// every field's contents identical while copying each byte once.
//
// The alias tests are exact rather than heuristic. A number's num is raw by
// construction, checked by pointer identity. An unescaped string is exactly its
// raw bytes minus the two quotes, and any escape shrinks the decoded form by at
// least one byte, so len(sval)+2 == len(raw) holds precisely when no escape was
// resolved — which is the only case where sval points into raw. An escaped
// string's sval lives in the classification arena and is detached on its own.
//
// They are functions rather than inline expressions because three places must
// agree on them exactly: the sizing pass, the packing pass, and the spill
// accounting. A disagreement between the first two would overrun a row's arena
// and silently repoint scalars already handed out; a disagreement with the
// third would mis-budget the spill threshold.
func numAliasesRaw(s scalar) bool {
	return len(s.num) != 0 && len(s.num) == len(s.raw) && &s.num[0] == &s.raw[0]
}

func svalAliasesRaw(s scalar) bool {
	return s.kind == kindString && len(s.raw) == len(s.sval)+2
}

// scalarOwnBytes is the number of arena bytes detaching s copies.
func scalarOwnBytes(s scalar) int {
	n := len(s.raw)
	if !numAliasesRaw(s) {
		n += len(s.num)
	}
	if !svalAliasesRaw(s) {
		n += len(s.sval)
	}
	return n
}

// ownScalarInto detaches s from the document it was classified against, so it
// can outlive the batch that produced it, by packing the bytes it must own into
// arena. It returns the extended arena and the detached scalar.
//
// The caller must have reserved scalarOwnBytes(s) of spare capacity: the views
// returned for earlier scalars point into arena, so an append that grew it
// would leave them addressing freed storage. Reserving exactly what the whole
// row needs up front is what makes one allocation serve every column of a row.
func ownScalarInto(arena []byte, s scalar) ([]byte, scalar) {
	numAliases, svalAliases := numAliasesRaw(s), svalAliasesRaw(s)
	raw := arena
	if s.raw != nil {
		arena, raw = appendArena(arena, s.raw)
	} else {
		raw = nil
	}
	s.raw = raw
	switch {
	case numAliases:
		s.num = raw
	case s.num != nil:
		arena, s.num = appendArena(arena, s.num)
	}
	switch {
	case svalAliases:
		s.sval = byteview.String(raw[1 : len(raw)-1])
	case s.sval != "":
		var view []byte
		arena, view = appendArena(arena, byteview.Bytes(s.sval))
		s.sval = byteview.String(view)
	}
	return arena, s
}

// appendArena copies src onto arena and returns the extended arena together
// with a capacity-clipped view of the copy, so a later append into the arena
// can never be seen through the view.
func appendArena(arena, src []byte) ([]byte, []byte) {
	start := len(arena)
	arena = append(arena, src...)
	return arena, arena[start:len(arena):len(arena)]
}

// The struct sizes the spill accounting charges. They are constants rather
// than unsafe.Sizeof so this package keeps no unsafe scope of its own;
// TestSpillAccountingStructSizes asserts they still match the real layouts, so
// a field added to either type cannot silently un-budget the spill threshold.
//
// The previous constant here was 48 for both, which under-charged a scalar by
// 40 bytes and made a spill trigger at roughly twice its configured target.
const (
	scalarStructBytes  = 88
	fileRowStructBytes = 56
)

// scalarBytes is the retained size of one detached scalar: the struct itself
// plus the storage ownScalar cloned for it. num and sval are counted only when
// they are separate allocations, mirroring ownScalar's aliasing exactly, so the
// spill threshold reflects bytes actually held.
func scalarBytes(s scalar) int64 {
	return int64(scalarStructBytes) + int64(scalarOwnBytes(s))
}

func rowBytes(r fileRow) int64 {
	n := int64(fileRowStructBytes)
	for _, s := range r.values {
		n += scalarBytes(s)
	}
	for _, s := range r.order {
		n += scalarBytes(s)
	}
	return n
}

func estimateRows(rows []fileRow) int64 {
	var n int64
	for i := range rows {
		n += rowBytes(rows[i])
	}
	return n
}

func mergeAggs(dst, src []aggAcc) {
	for i := range dst {
		d := &dst[i]
		s := src[i]
		d.count += s.count
		if s.n == 0 {
			continue
		}
		if d.n == 0 {
			d.min, d.max = s.min, s.max
		} else {
			if s.min < d.min {
				d.min = s.min
			}
			if s.max > d.max {
				d.max = s.max
			}
		}
		d.n += s.n
		d.sum += s.sum
	}
}

func (p *plan) compareFileRows(a, b fileRow) int {
	for i, o := range p.order {
		c := compareScalar(a.order[i], b.order[i])
		if o.dir == Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return compareFileOrdinal(a, b)
}

func compareFileOrdinal(a, b fileRow) int {
	switch {
	case a.ordinal < b.ordinal:
		return -1
	case a.ordinal > b.ordinal:
		return 1
	default:
		return 0
	}
}

func (p *plan) compareFileGroups(a, b fileGroup) int {
	for _, o := range p.order {
		c := compareScalar(a.scalars[o.slot], b.scalars[o.slot])
		if o.dir == Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	switch {
	case a.first < b.first:
		return -1
	case a.first > b.first:
		return 1
	default:
		return 0
	}
}

func (p *plan) resultLimit() int {
	if p.hasLimit {
		return p.limit
	}
	return -1
}

func (p *plan) fileRowResultInto(result Result, rows []fileRow) Result {
	prepareResult(&result, p, len(rows))
	for row := range rows {
		for col := range p.columns {
			result.Columns[col].Cells[row] = result.ownFileCell(
				cellFromScalar(rows[row].values[col]),
			)
		}
	}
	return result
}

func (p *plan) fileGroupResultInto(result Result, groups []fileGroup) Result {
	if len(p.order) != 0 {
		slices.SortStableFunc(groups, p.compareFileGroups)
	} else {
		slices.SortStableFunc(groups, func(a, b fileGroup) int {
			switch {
			case a.first < b.first:
				return -1
			case a.first > b.first:
				return 1
			default:
				return 0
			}
		})
	}
	if p.hasLimit && len(groups) > p.limit {
		groups = groups[:p.limit]
	}
	prepareResult(&result, p, len(groups))
	var w Workspace
	for row := range groups {
		g := group{scalars: groups[row].scalars, accs: groups[row].accs}
		p.fillAggregateCells(&result, row, g.accs, &g, &w)
		for column := range result.Columns {
			result.Columns[column].Cells[row] = result.ownFileCell(
				result.Columns[column].Cells[row],
			)
		}
	}
	return result
}

// The spill representation uses exported fields so encoding/gob can stream
// one record at a time without retaining an entire run in memory.
type diskScalar struct {
	Kind  uint8
	Bool  bool
	Num   []byte
	IsInt bool
	Int   int64
	Text  string
	Raw   []byte
}

type diskRow struct {
	Values  []diskScalar
	Order   []diskScalar
	Ordinal uint64
}

type diskAgg struct {
	Count int
	N     int
	Sum   float64
	Min   float64
	Max   float64
}

type diskGroup struct {
	Key     string
	Scalars []diskScalar
	Aggs    []diskAgg
	First   uint64
	Bytes   int64
}

func scalarToDisk(s scalar) diskScalar {
	return diskScalar{Kind: uint8(s.kind), Bool: s.bval, Num: s.num, IsInt: s.isInt, Int: s.ival, Text: s.sval, Raw: s.raw}
}

func scalarFromDisk(s diskScalar) scalar {
	return scalar{kind: scalarKind(s.Kind), bval: s.Bool, num: s.Num, isInt: s.IsInt, ival: s.Int, sval: s.Text, raw: s.Raw}
}

func rowToDisk(r fileRow) diskRow {
	d := diskRow{Values: make([]diskScalar, len(r.values)), Order: make([]diskScalar, len(r.order)), Ordinal: r.ordinal}
	for i := range r.values {
		d.Values[i] = scalarToDisk(r.values[i])
	}
	for i := range r.order {
		d.Order[i] = scalarToDisk(r.order[i])
	}
	return d
}

func rowFromDisk(d diskRow) fileRow {
	r := fileRow{values: make([]scalar, len(d.Values)), order: make([]scalar, len(d.Order)), ordinal: d.Ordinal}
	for i := range d.Values {
		r.values[i] = scalarFromDisk(d.Values[i])
	}
	for i := range d.Order {
		r.order[i] = scalarFromDisk(d.Order[i])
	}
	return r
}

func groupToDisk(g fileGroup) diskGroup {
	d := diskGroup{Key: g.key, Scalars: make([]diskScalar, len(g.scalars)), Aggs: make([]diskAgg, len(g.accs)), First: g.first, Bytes: g.bytes}
	for i := range g.scalars {
		d.Scalars[i] = scalarToDisk(g.scalars[i])
	}
	for i, a := range g.accs {
		d.Aggs[i] = diskAgg{Count: a.count, N: a.n, Sum: a.sum, Min: a.min, Max: a.max}
	}
	return d
}

func groupFromDisk(d diskGroup) fileGroup {
	g := fileGroup{key: d.Key, scalars: make([]scalar, len(d.Scalars)), accs: make([]aggAcc, len(d.Aggs)), first: d.First, bytes: d.Bytes}
	for i := range d.Scalars {
		g.scalars[i] = scalarFromDisk(d.Scalars[i])
	}
	for i, a := range d.Aggs {
		g.accs[i] = aggAcc{count: a.Count, n: a.N, sum: a.Sum, min: a.Min, max: a.Max}
	}
	return g
}

type spillRun struct {
	path string
	size int64
}

type spillManager struct {
	dir   string
	files map[string]struct{}
	stats *ExecStats
}

// newSpillManager reuses files as its live-run set. An execution that never
// spills — the overwhelming majority — must not pay for a map it never writes
// to, so the set is created on the first create and handed back by cleanup for
// the next execution to reuse.
func newSpillManager(dir string, stats *ExecStats, files map[string]struct{}) *spillManager {
	return &spillManager{dir: dir, files: files, stats: stats}
}

func (s *spillManager) create(pattern string) (*os.File, error) {
	f, err := os.CreateTemp(s.dir, pattern)
	if err == nil {
		if s.files == nil {
			s.files = make(map[string]struct{})
		}
		s.files[f.Name()] = struct{}{}
	}
	return f, err
}

func (s *spillManager) finish(f *os.File) (spillRun, error) {
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return spillRun{}, err
	}
	info, err := f.Stat()
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return spillRun{}, err
	}
	run := spillRun{path: f.Name(), size: info.Size()}
	s.stats.SpillRuns++
	s.stats.SpilledBytes += run.size
	return run, nil
}

func (s *spillManager) remove(run spillRun) {
	_ = os.Remove(run.path)
	delete(s.files, run.path)
}

// cleanup removes every run this execution left behind and returns the emptied
// set so the next execution can reuse its buckets.
func (s *spillManager) cleanup() map[string]struct{} {
	for path := range s.files {
		_ = os.Remove(path)
	}
	clear(s.files)
	return s.files
}

func (s *spillManager) writeRows(p *plan, rows []fileRow) (spillRun, error) {
	slices.SortStableFunc(rows, p.compareFileRows)
	f, err := s.create("vibejson-query-rows-*")
	if err != nil {
		return spillRun{}, err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	enc := gob.NewEncoder(w)
	for i := range rows {
		if err := enc.Encode(rowToDisk(rows[i])); err != nil {
			_ = f.Close()
			return spillRun{}, err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return spillRun{}, err
	}
	return s.finish(f)
}

func (s *spillManager) writeGroups(groups map[string]*fileGroup) (spillRun, error) {
	ordered := make([]*fileGroup, 0, len(groups))
	for _, g := range groups {
		ordered = append(ordered, g)
	}
	slices.SortFunc(ordered, func(a, b *fileGroup) int { return strings.Compare(a.key, b.key) })
	f, err := s.create("vibejson-query-groups-*")
	if err != nil {
		return spillRun{}, err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	enc := gob.NewEncoder(w)
	for _, g := range ordered {
		if err := enc.Encode(groupToDisk(*g)); err != nil {
			_ = f.Close()
			return spillRun{}, err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return spillRun{}, err
	}
	return s.finish(f)
}

type rowCursor struct {
	file *os.File
	dec  *gob.Decoder
	row  fileRow
}

func openRowCursor(run spillRun) (*rowCursor, error) {
	f, err := os.Open(run.path)
	if err != nil {
		return nil, err
	}
	c := &rowCursor{file: f, dec: gob.NewDecoder(bufio.NewReaderSize(f, 64<<10))}
	if err := c.next(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return c, nil
}

func (c *rowCursor) next() error {
	var d diskRow
	if err := c.dec.Decode(&d); err != nil {
		return err
	}
	c.row = rowFromDisk(d)
	return nil
}

type rowHeap struct {
	p     *plan
	items []*rowCursor
}

func (h rowHeap) Len() int           { return len(h.items) }
func (h rowHeap) Less(i, j int) bool { return h.p.compareFileRows(h.items[i].row, h.items[j].row) < 0 }
func (h rowHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *rowHeap) Push(x any)        { h.items = append(h.items, x.(*rowCursor)) }
func (h *rowHeap) Pop() any {
	n := len(h.items) - 1
	x := h.items[n]
	h.items = h.items[:n]
	return x
}

func openRowHeap(p *plan, runs []spillRun) (*rowHeap, error) {
	h := &rowHeap{p: p}
	for _, run := range runs {
		c, err := openRowCursor(run)
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil {
			closeRowHeap(h)
			return nil, err
		}
		heap.Push(h, c)
	}
	return h, nil
}

func closeRowHeap(h *rowHeap) {
	for _, c := range h.items {
		_ = c.file.Close()
	}
}

func (s *spillManager) mergeRowRuns(p *plan, runs []spillRun) (spillRun, error) {
	h, err := openRowHeap(p, runs)
	if err != nil {
		return spillRun{}, err
	}
	defer closeRowHeap(h)
	f, err := s.create("vibejson-query-rowmerge-*")
	if err != nil {
		return spillRun{}, err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	enc := gob.NewEncoder(w)
	for h.Len() != 0 {
		c := heap.Pop(h).(*rowCursor)
		if err := enc.Encode(rowToDisk(c.row)); err != nil {
			_ = f.Close()
			return spillRun{}, err
		}
		if err := c.next(); err == nil {
			heap.Push(h, c)
		} else if errors.Is(err, io.EOF) {
			_ = c.file.Close()
		} else {
			_ = f.Close()
			return spillRun{}, err
		}
	}
	if err := w.Flush(); err != nil {
		_ = f.Close()
		return spillRun{}, err
	}
	return s.finish(f)
}

func (s *spillManager) reduceRowRuns(p *plan, runs []spillRun) ([]spillRun, error) {
	for len(runs) > maxSpillFanIn {
		next := make([]spillRun, 0, (len(runs)+maxSpillFanIn-1)/maxSpillFanIn)
		for start := 0; start < len(runs); start += maxSpillFanIn {
			end := min(start+maxSpillFanIn, len(runs))
			merged, err := s.mergeRowRuns(p, runs[start:end])
			if err != nil {
				return nil, err
			}
			for _, run := range runs[start:end] {
				s.remove(run)
			}
			next = append(next, merged)
		}
		runs = next
	}
	return runs, nil
}

func (s *spillManager) readMergedRows(p *plan, runs []spillRun, limit int, rows []fileRow) ([]fileRow, error) {
	h, err := openRowHeap(p, runs)
	if err != nil {
		return rows, err
	}
	defer closeRowHeap(h)
	for h.Len() != 0 && (limit < 0 || len(rows) < limit) {
		c := heap.Pop(h).(*rowCursor)
		rows = append(rows, c.row)
		if err := c.next(); err == nil {
			heap.Push(h, c)
		} else if errors.Is(err, io.EOF) {
			_ = c.file.Close()
		} else {
			return rows, err
		}
	}
	return rows, nil
}

type groupCursor struct {
	file  *os.File
	dec   *gob.Decoder
	group fileGroup
}

func openGroupCursor(run spillRun) (*groupCursor, error) {
	f, err := os.Open(run.path)
	if err != nil {
		return nil, err
	}
	c := &groupCursor{file: f, dec: gob.NewDecoder(bufio.NewReaderSize(f, 64<<10))}
	if err := c.next(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return c, nil
}

func (c *groupCursor) next() error {
	var d diskGroup
	if err := c.dec.Decode(&d); err != nil {
		return err
	}
	c.group = groupFromDisk(d)
	return nil
}

type groupHeap struct{ items []*groupCursor }

func (h groupHeap) Len() int           { return len(h.items) }
func (h groupHeap) Less(i, j int) bool { return h.items[i].group.key < h.items[j].group.key }
func (h groupHeap) Swap(i, j int)      { h.items[i], h.items[j] = h.items[j], h.items[i] }
func (h *groupHeap) Push(x any)        { h.items = append(h.items, x.(*groupCursor)) }
func (h *groupHeap) Pop() any {
	n := len(h.items) - 1
	x := h.items[n]
	h.items = h.items[:n]
	return x
}

func openGroupHeap(runs []spillRun) (*groupHeap, error) {
	h := &groupHeap{}
	for _, run := range runs {
		c, err := openGroupCursor(run)
		if errors.Is(err, io.EOF) {
			continue
		}
		if err != nil {
			closeGroupHeap(h)
			return nil, err
		}
		heap.Push(h, c)
	}
	return h, nil
}

func closeGroupHeap(h *groupHeap) {
	for _, c := range h.items {
		_ = c.file.Close()
	}
}

func (s *spillManager) mergeGroups(runs []spillRun, emit func(fileGroup) error) error {
	h, err := openGroupHeap(runs)
	if err != nil {
		return err
	}
	defer closeGroupHeap(h)
	var current *fileGroup
	for h.Len() != 0 {
		c := heap.Pop(h).(*groupCursor)
		g := c.group
		if current == nil || current.key != g.key {
			if current != nil {
				if err := emit(*current); err != nil {
					return err
				}
			}
			copy := g
			current = &copy
		} else {
			mergeAggs(current.accs, g.accs)
			if g.first < current.first {
				current.first = g.first
			}
		}
		if err := c.next(); err == nil {
			heap.Push(h, c)
		} else if errors.Is(err, io.EOF) {
			_ = c.file.Close()
		} else {
			return err
		}
	}
	if current != nil {
		return emit(*current)
	}
	return nil
}

func (s *spillManager) mergeGroupRuns(runs []spillRun) (spillRun, error) {
	f, err := s.create("vibejson-query-groupmerge-*")
	if err != nil {
		return spillRun{}, err
	}
	w := bufio.NewWriterSize(f, 64<<10)
	enc := gob.NewEncoder(w)
	err = s.mergeGroups(runs, func(g fileGroup) error { return enc.Encode(groupToDisk(g)) })
	if err == nil {
		err = w.Flush()
	}
	if err != nil {
		_ = f.Close()
		return spillRun{}, err
	}
	return s.finish(f)
}

func (s *spillManager) reduceGroupRuns(runs []spillRun) ([]spillRun, error) {
	for len(runs) > maxSpillFanIn {
		next := make([]spillRun, 0, (len(runs)+maxSpillFanIn-1)/maxSpillFanIn)
		for start := 0; start < len(runs); start += maxSpillFanIn {
			end := min(start+maxSpillFanIn, len(runs))
			merged, err := s.mergeGroupRuns(runs[start:end])
			if err != nil {
				return nil, err
			}
			for _, run := range runs[start:end] {
				s.remove(run)
			}
			next = append(next, merged)
		}
		runs = next
	}
	return runs, nil
}

func (s *spillManager) readMergedGroups(groups []fileGroup, runs []spillRun) ([]fileGroup, error) {
	err := s.mergeGroups(runs, func(g fileGroup) error {
		groups = append(groups, g)
		return nil
	})
	return groups, err
}
