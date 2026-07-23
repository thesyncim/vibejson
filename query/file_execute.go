package query

import (
	"bufio"
	"bytes"
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

	"github.com/thesyncim/slopjson"
)

// FileExecutionOptions controls bounded batch execution over a FileSnapshot.
// MemoryBytes is a working-memory target, not a limit on the returned Result:
// a caller asking for an unbounded projection necessarily owns output
// proportional to the number and size of selected rows. One oversized JSON
// document may also exceed the target by itself.
type FileExecutionOptions struct {
	Workers        int
	BatchRows      int
	BatchBytes     int64
	MemoryBytes    int64
	SpillDirectory string
	// Workspace retains late-bound index-planning storage across executions.
	// It is single-consumer; concurrent calls need independent workspaces.
	Workspace *FileExecutionWorkspace
}

// FileExecutionWorkspace owns reusable persistent-index planning storage. The
// zero value is ready to use. It does not own worker batches or returned
// Result cells, whose cardinality depends on each execution.
type FileExecutionWorkspace struct {
	planner       Workspace
	index         slopjson.FileIndexWorkspace
	overflow      []byte
	reductions    []slopjson.Float64Aggregate
	coverPaths    []string
	accs          []aggAcc
	indexGroups   []slopjson.FileIndexScalarGroup
	indexResidual []slopjson.StoreMask
	fileGroups    []fileGroup
}

// Release drops storage retained by durable index planning.
func (w *FileExecutionWorkspace) Release() {
	if w == nil {
		return
	}
	w.planner = Workspace{}
	w.index.Release()
	w.overflow = nil
	w.reductions = nil
	w.coverPaths = nil
	w.accs = nil
	w.indexGroups = nil
	w.indexResidual = nil
	w.fileGroups = nil
}

// FileExecutionStats describes the physical work performed by
// [Query.RunFileSnapshot]. RowsTotal is the snapshot cardinality while
// RowsScanned is the number of JSON documents admitted to execution after
// persistent-index pushdown. IndexCertificateRows were decided from a
// collision-free posting representative or compact categorical cover without
// opening JSON;
// IndexRecheckRows required exact document comparison. An ordinary
// IndexBounded execution still evaluates the complete predicate. BufferedBytes
// is the largest observed batch or in-memory merge frontier; it excludes the
// caller-owned final Result. CoveringColumns counts distinct typed columns
// reduced without admitting JSON.
type FileExecutionStats struct {
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

func normalizeFileOptions(opts FileExecutionOptions) (normalizedFileOptions, error) {
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

// RunFileSnapshot executes q over a page-backed snapshot in parallel batches.
// Ordered projections and grouped reductions spill sorted runs once their
// merge frontier reaches MemoryBytes; spill merges open at most 32 files at a
// time. Returned cells own their bytes and remain valid after the snapshot is
// closed. The snapshot remains owned by the caller.
func (q *Query) RunFileSnapshot(snapshot *slopjson.FileSnapshot, opts FileExecutionOptions) (Result, FileExecutionStats, error) {
	return q.runFileSnapshot(Result{}, snapshot, opts)
}

// RunFileSnapshotInto executes q into caller-owned result storage. Repeated
// calls reuse the Result's column cells and packed variable-width value arena;
// once their observed high-water marks fit, result materialization allocates
// nothing. The returned cells remain valid after snapshot close and until dst
// is reused or released. dst is single-consumer and must be non-nil.
func (q *Query) RunFileSnapshotInto(
	dst *Result,
	snapshot *slopjson.FileSnapshot,
	opts FileExecutionOptions,
) (FileExecutionStats, error) {
	if dst == nil {
		return FileExecutionStats{}, fmt.Errorf(
			"query: RunFileSnapshotInto requires a non-nil Result",
		)
	}
	result, stats, err := q.runFileSnapshot(*dst, snapshot, opts)
	*dst = result
	return stats, err
}

func (q *Query) runFileSnapshot(
	result Result,
	snapshot *slopjson.FileSnapshot,
	opts FileExecutionOptions,
) (Result, FileExecutionStats, error) {
	result.fileData = result.fileData[:0]
	n, err := normalizeFileOptions(opts)
	if err != nil {
		return result, FileExecutionStats{}, err
	}
	if snapshot == nil {
		return result, FileExecutionStats{}, fmt.Errorf("query: RunFileSnapshot requires a non-nil snapshot")
	}
	p, err := q.compiled()
	if err != nil {
		return result, FileExecutionStats{}, err
	}
	stats := FileExecutionStats{Workers: n.workers, RowsTotal: snapshot.Len()}
	fileWorkspace := opts.Workspace
	if fileWorkspace == nil {
		fileWorkspace = new(FileExecutionWorkspace)
	}
	directIndex, handled, directErr := p.runDirectFileIndexedCount(&result, snapshot, fileWorkspace)
	if handled {
		stats.IndexBounded = directIndex.bounded
		stats.IndexLookups = directIndex.lookups
		stats.IndexPostingPages = directIndex.postingPages
		stats.IndexCertificateRows = directIndex.certificates
		stats.IndexRecheckRows = directIndex.rechecks
		stats.CandidateRows = directIndex.rows
		stats.CandidateChunks = directIndex.chunks
		return result, stats, directErr
	}
	coveringColumns, handled, directErr := p.runDirectFileAggregate(&result, snapshot, fileWorkspace)
	if handled {
		stats.CoveringColumns = coveringColumns
		return result, stats, directErr
	}
	groupStats, handled, directErr := p.runDirectFileIndexGroups(
		&result, snapshot, fileWorkspace,
	)
	if handled {
		stats.IndexLookups = groupStats.lookups
		stats.IndexPostingPages = groupStats.postingPages
		stats.IndexCertificateRows = groupStats.certificates
		stats.IndexRecheckRows = groupStats.rechecks
		stats.RowsScanned = groupStats.rechecks
		stats.IndexGroupedRows = groupStats.certificates
		stats.IndexGroups = groupStats.groups
		return result, stats, directErr
	}
	return p.runFileSnapshotBatched(
		result, snapshot, fileWorkspace, n, stats,
	)
}

// runFileSnapshotBatched is kept outside the direct covering dispatcher so
// goroutine captures in the general executor cannot force the fast path's
// stats and fallback workspace onto the heap.
func (p *plan) runFileSnapshotBatched(
	result Result,
	snapshot *slopjson.FileSnapshot,
	fileWorkspace *FileExecutionWorkspace,
	n normalizedFileOptions,
	stats FileExecutionStats,
) (Result, FileExecutionStats, error) {
	candidateMasks, err := p.fileCandidateMasks(snapshot, &fileWorkspace.index, &fileWorkspace.planner)
	if err != nil {
		return result, stats, err
	}
	stats.IndexLookups = fileWorkspace.planner.storeIndexProbes
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
	spills := newSpillManager(n.spillDir, &stats)
	defer spills.cleanup()

	jobs := make(chan fileBatch, n.workers)
	partials := make(chan filePartial, n.workers)
	credits := make(chan struct{}, n.workers*2)
	scanDone := make(chan fileScanResult, 1)
	stop := make(chan struct{})
	var stopOnce sync.Once
	cancel := func() { stopOnce.Do(func() { close(stop) }) }

	go scanFileBatches(snapshot, candidateMasks, &fileWorkspace.overflow, n, jobs, credits, scanDone, stop)
	var workers sync.WaitGroup
	workers.Add(n.workers)
	for range n.workers {
		go func() {
			defer workers.Done()
			for batch := range jobs {
				part := p.makeFilePartial(batch, stats.IndexBounded)
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
	var rows []fileRow
	var rowBytes int64
	var rowRuns []spillRun
	var accs []aggAcc
	groups := make(map[string]*fileGroup)
	var groupBytes int64
	var groupRuns []spillRun

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
					groups = make(map[string]*fileGroup)
					groupBytes = 0
				}
			}
		case p.singleRow:
			if accs == nil {
				accs = make([]aggAcc, len(p.columns))
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
					rows = nil
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
	pending := make(map[uint64]filePartial, n.workers*2)
	nextSequence := uint64(0)
	for part := range partials {
		pending[part.seq] = part
		for {
			part, ok := pending[nextSequence]
			if !ok {
				break
			}
			delete(pending, nextSequence)
			consume(part)
			<-credits
			nextSequence++
		}
	}
	scan := <-scanDone
	stats.RowsScanned = scan.rows
	stats.Batches = scan.batches
	stats.PeakBatchRows = scan.peakRows
	stats.PeakBatchBytes = scan.peakBytes
	if scan.err != nil && !errors.Is(scan.err, errFileExecutionStopped) && firstErr == nil {
		firstErr = scan.err
	}
	if firstErr != nil {
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
			if err != nil {
				return result, stats, err
			}
			var merged []fileGroup
			merged, err = spills.readMergedGroups(groupRuns)
			if err != nil {
				return result, stats, err
			}
			return p.fileGroupResultInto(result, merged), stats, nil
		}
		merged := make([]fileGroup, 0, len(groups))
		for _, g := range groups {
			merged = append(merged, *g)
		}
		return p.fileGroupResultInto(result, merged), stats, nil
	case p.singleRow:
		if accs == nil {
			accs = make([]aggAcc, len(p.columns))
		}
		var w Workspace
		resultRows := 1
		if p.hasLimit && p.limit == 0 {
			resultRows = 0
		}
		prepareResult(&result, p, resultRows)
		if resultRows != 0 {
			p.fillAggregateCells(&result, 0, accs, nil, &w)
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
			}
			rowRuns, err = spills.reduceRowRuns(p, rowRuns)
			if err != nil {
				return result, stats, err
			}
			rows, err = spills.readMergedRows(p, rowRuns, p.resultLimit())
			if err != nil {
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
		return p.fileRowResultInto(result, rows), stats, nil
	}
}

type fileBatch struct {
	seq   uint64
	base  uint64
	data  []byte
	ends  []int
	bytes int64
}

func newFileBatch(seq, base uint64, opts normalizedFileOptions) fileBatch {
	dataCapacity := int(min(opts.batchBytes, 64<<10))
	rowCapacity := min(opts.batchRows, defaultBatchRows)
	return fileBatch{
		seq: seq, base: base,
		data: make([]byte, 0, dataCapacity), ends: make([]int, 0, rowCapacity),
	}
}

type fileScanResult struct {
	err       error
	rows      uint64
	batches   uint64
	peakRows  int
	peakBytes int64
}

func scanFileBatches(snapshot *slopjson.FileSnapshot, masks []slopjson.StoreMask, overflow *[]byte, opts normalizedFileOptions, jobs chan<- fileBatch, credits chan struct{}, done chan<- fileScanResult, stop <-chan struct{}) {
	defer close(jobs)
	var out fileScanResult
	batch := newFileBatch(0, 0, opts)
	flush := func() error {
		if len(batch.ends) == 0 {
			return nil
		}
		select {
		case credits <- struct{}{}:
		case <-stop:
			return errFileExecutionStopped
		}
		select {
		case jobs <- batch:
			out.batches++
			batch = newFileBatch(batch.seq+1, out.rows, opts)
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

func (p *plan) makeFilePartial(batch fileBatch, indexBounded bool) filePartial {
	part := filePartial{seq: batch.seq}
	// Predicate-free work cannot consume postings, while an index-bounded file
	// scan has already admitted only the persistent index's candidate rows.
	// Both cases still evaluate the complete predicate where present, including
	// collision rechecks, without rebuilding a transient per-batch index.
	docs := &slopjson.DocSet{Postings: p.where != nil && !indexBounded}
	start := 0
	for _, end := range batch.ends {
		if _, err := docs.Append(batch.data[start:end]); err != nil {
			part.err = err
			return part
		}
		start = end
	}
	var w Workspace
	w.candidateUsed = 0
	candidates := p.candidateRows(docs, &w)
	compact := preferSparseRows(len(candidates), docs.Len(), candidates != nil)
	var sourceRows []int
	if compact {
		sourceRows = candidates
	}
	ctx := &w.ctx
	ctx.s, ctx.rows = docs, docs.Len()
	if compact {
		ctx.rows = len(sourceRows)
	}
	if err := ctx.extract(p, sourceRows, &w); err != nil {
		part.err = err
		return part
	}
	selected := p.selectRows(ctx, candidates, compact, &w)
	localOrdinal := func(row int) uint64 {
		if compact {
			return batch.base + uint64(sourceRows[row])
		}
		return batch.base + uint64(row)
	}
	switch {
	case p.grouped:
		byKey := make(map[string]int)
		for _, row := range selected {
			keyBytes := p.groupKey(w.groupKey[:0], ctx, row)
			key := string(keyBytes)
			id, ok := byKey[key]
			if !ok {
				id = len(part.groups)
				byKey[strings.Clone(key)] = id
				g := fileGroup{key: strings.Clone(key), first: localOrdinal(row)}
				g.scalars = make([]scalar, len(p.groupCols))
				for i, col := range p.groupCols {
					g.scalars[i] = ownScalar(ctx.values[col][row])
				}
				g.accs = make([]aggAcc, len(p.columns))
				g.bytes = int64(len(g.key) + len(g.accs)*40 + 64)
				for _, s := range g.scalars {
					g.bytes += scalarBytes(s)
				}
				part.groups = append(part.groups, g)
				part.bytes += g.bytes
			}
			p.accumulate(part.groups[id].accs, ctx, row)
		}
	case p.singleRow:
		part.accs = make([]aggAcc, len(p.columns))
		for _, row := range selected {
			p.accumulate(part.accs, ctx, row)
		}
		part.bytes = int64(len(part.accs) * 40)
	default:
		if len(p.order) != 0 {
			slices.SortStableFunc(selected, func(a, b int) int { return p.compareRows(ctx, a, b) })
		}
		if p.hasLimit && len(selected) > p.limit {
			selected = selected[:p.limit]
		}
		part.rows = make([]fileRow, 0, len(selected))
		for _, row := range selected {
			r := fileRow{ordinal: localOrdinal(row)}
			r.values = make([]scalar, len(p.columns))
			for i, col := range p.columns {
				r.values[i] = ownScalar(ctx.values[col.value][row])
			}
			r.order = make([]scalar, len(p.order))
			for i, order := range p.order {
				r.order[i] = ownScalar(ctx.values[order.value][row])
			}
			part.bytes += rowBytes(r)
			part.rows = append(part.rows, r)
		}
	}
	return part
}

func ownScalar(s scalar) scalar {
	s.num = bytes.Clone(s.num)
	s.raw = bytes.Clone(s.raw)
	s.sval = strings.Clone(s.sval)
	return s
}

func scalarBytes(s scalar) int64 { return int64(48 + len(s.num) + len(s.raw) + len(s.sval)) }

func rowBytes(r fileRow) int64 {
	n := int64(48)
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
	stats *FileExecutionStats
}

func newSpillManager(dir string, stats *FileExecutionStats) *spillManager {
	return &spillManager{dir: dir, files: make(map[string]struct{}), stats: stats}
}

func (s *spillManager) create(pattern string) (*os.File, error) {
	f, err := os.CreateTemp(s.dir, pattern)
	if err == nil {
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

func (s *spillManager) cleanup() {
	for path := range s.files {
		_ = os.Remove(path)
	}
}

func (s *spillManager) writeRows(p *plan, rows []fileRow) (spillRun, error) {
	slices.SortStableFunc(rows, p.compareFileRows)
	f, err := s.create("slopjson-query-rows-*")
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
	f, err := s.create("slopjson-query-groups-*")
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
	f, err := s.create("slopjson-query-rowmerge-*")
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

func (s *spillManager) readMergedRows(p *plan, runs []spillRun, limit int) ([]fileRow, error) {
	h, err := openRowHeap(p, runs)
	if err != nil {
		return nil, err
	}
	defer closeRowHeap(h)
	var rows []fileRow
	for h.Len() != 0 && (limit < 0 || len(rows) < limit) {
		c := heap.Pop(h).(*rowCursor)
		rows = append(rows, c.row)
		if err := c.next(); err == nil {
			heap.Push(h, c)
		} else if errors.Is(err, io.EOF) {
			_ = c.file.Close()
		} else {
			return nil, err
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
	f, err := s.create("slopjson-query-groupmerge-*")
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

func (s *spillManager) readMergedGroups(runs []spillRun) ([]fileGroup, error) {
	var groups []fileGroup
	err := s.mergeGroups(runs, func(g fileGroup) error {
		groups = append(groups, g)
		return nil
	})
	return groups, err
}
