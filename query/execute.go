package query

import (
	"bytes"
	"math"
	"math/bits"
	"slices"

	"github.com/thesyncim/slopjson"
	"github.com/thesyncim/slopjson/document"
)

// Workspace owns all transient query execution storage. Its zero value is
// ready to use. Reusing one Workspace with RunInto turns a warmed execution
// whose row, posting-frontier, decoded-text, and group high-water marks fit the
// retained capacity into a zero-allocation operation. That contract includes
// posting merges, escaped-string classification, containment indexing, stable
// ordering, aggregation, grouping, and result materialization.
//
// A Workspace is single-consumer and not safe for concurrent use. A compiled
// Query remains safe for concurrent use when each goroutine supplies a distinct
// Workspace and Result. Storage borrowed by a Result written by RunInto is
// valid only until the next RunInto using the same Workspace or Result.
type Workspace struct {
	ctx execCtx

	raws             [][]slopjson.RawValue
	numRaws          []slopjson.RawValue
	selected         []int
	candidates       [][]int
	candidateUsed    int
	emptyCandidate   [1]int
	storeMasks       [][]slopjson.StoreMask
	storeMaskUsed    int
	storeIndexProbes int
	storeRows        []slopjson.StoreRow
	storeIndexes     []slopjson.StoreIndexInfo
	emptyStoreMask   [1]slopjson.StoreMask

	containsEntries []slopjson.IndexEntry
	text            []byte

	accs       []aggAcc
	reductions []slopjson.Float64Aggregate
	reduced    []bool
	interner   slopjson.KeyInterner
	groups     []group
	groupKey   []byte
	groupOrder []int
	stringHash []uint32
	stringSlot []uint32
}

func (w *Workspace) nextStoreMasks() []slopjson.StoreMask {
	if w.storeMaskUsed == len(w.storeMasks) {
		w.storeMasks = append(w.storeMasks, nil)
	}
	i := w.storeMaskUsed
	w.storeMaskUsed++
	return w.storeMasks[i][:0]
}

func (w *Workspace) keepStoreMasks(masks []slopjson.StoreMask) {
	w.storeMasks[w.storeMaskUsed-1] = masks
}

// execCtx is the columnar state for one execution. Its inner column slices
// persist inside Workspace and are overwritten on the next call.
type execCtx struct {
	s      *slopjson.DocSet
	cache  slopjson.ShapeCache
	rows   int
	values [][]scalar
	nums   []numColumn
}

type numColumn struct {
	vals  []float64
	valid []bool
}

// nextCandidates returns an independent empty posting buffer. Candidate-tree
// evaluation never aliases its inputs with a merge output, so AND/OR can be
// assembled in linear passes without allocations after the buffers warm.
func (w *Workspace) nextCandidates() []int {
	if w.candidateUsed == len(w.candidates) {
		w.candidates = append(w.candidates, nil)
	}
	i := w.candidateUsed
	w.candidateUsed++
	return w.candidates[i][:0]
}

func (w *Workspace) keepCandidates(rows []int) {
	w.candidates[w.candidateUsed-1] = rows
}

// runInto executes p, overwriting dst while retaining its column and cell
// capacity. Callers must not reuse dst or w concurrently.
func (p *plan) runInto(dst *Result, s *slopjson.DocSet, w *Workspace) error {
	w.candidateUsed = 0
	w.text = w.text[:0]
	w.groupKey = w.groupKey[:0]
	w.groupOrder = w.groupOrder[:0]
	w.interner.Reset()

	candidates := p.candidateRows(s, w)
	compact := preferSparseRows(len(candidates), s.Len(), candidates != nil)
	var sourceRows []int
	if compact {
		sourceRows = candidates
	}

	ctx := &w.ctx
	ctx.s, ctx.rows = s, s.Len()
	if compact {
		ctx.rows = len(sourceRows)
	}
	if err := ctx.extract(p, sourceRows, w); err != nil {
		return err
	}
	selected := p.selectRows(ctx, candidates, compact, w)
	switch {
	case p.grouped:
		p.runGroupedInto(dst, ctx, selected, w)
	case p.singleRow:
		p.runAggregateInto(dst, ctx, selected, w)
	default:
		p.runProjectionInto(dst, ctx, selected)
	}
	return nil
}

func (p *plan) runSnapshotInto(dst *Result, snapshot slopjson.Snapshot, w *Workspace) error {
	w.candidateUsed = 0
	w.storeMaskUsed = 0
	w.text = w.text[:0]
	w.groupKey = w.groupKey[:0]
	w.groupOrder = w.groupOrder[:0]
	w.interner.Reset()
	if p.runDirectSnapshotAggregate(dst, snapshot, w) {
		return nil
	}
	if handled, err := p.runDirectSnapshotStringCountGroups(dst, snapshot, w); err != nil {
		return err
	} else if handled {
		return nil
	}
	masks, err := p.storeCandidateMasks(snapshot, w)
	if err != nil {
		return err
	}
	candidateCount := 0
	for _, mask := range masks {
		candidateCount += bits.OnesCount64(mask.Bits)
	}
	compact := masks != nil && candidateCount <= snapshot.Len()/2
	w.storeRows = w.storeRows[:0]
	if compact {
		for _, mask := range masks {
			for word := mask.Bits; word != 0; word &= word - 1 {
				w.storeRows = append(w.storeRows, slopjson.StoreRow{
					Chunk: mask.Chunk,
					Slot:  uint8(bits.TrailingZeros64(word)),
				})
			}
		}
	}

	ctx := &w.ctx
	ctx.s, ctx.rows = nil, snapshot.Len()
	if compact {
		ctx.rows = len(w.storeRows)
	}
	if err := ctx.extractSnapshot(p, snapshot, w.storeRows, compact, w); err != nil {
		return err
	}
	selected := p.selectRows(ctx, nil, compact, w)
	switch {
	case p.grouped:
		p.runGroupedInto(dst, ctx, selected, w)
	case p.singleRow:
		p.runAggregateInto(dst, ctx, selected, w)
	default:
		p.runProjectionInto(dst, ctx, selected)
	}
	return nil
}

func preferSparseRows(candidates, total int, hasBound bool) bool {
	return hasBound && candidates <= total/2
}

// extract gathers each raw value column before classifying any strings. That
// permits one exact pre-growth of the decoded-text arena, so escaped strings
// can append without moving views produced for earlier columns.
func (ctx *execCtx) extract(p *plan, sourceRows []int, w *Workspace) error {
	w.raws = resize(w.raws, len(p.valuePaths))
	textNeed := 0
	for i, cp := range p.valuePaths {
		raws, err := ctx.rawColumn(w.raws[i][:0], cp, sourceRows)
		if err != nil {
			return err
		}
		w.raws[i] = raws
		for _, r := range raws {
			b := r.Bytes()
			if r.Kind() == document.String && bytes.IndexByte(b, '\\') >= 0 {
				textNeed += len(b)
			}
		}
	}
	ctx.classifyRawColumns(p, w, textNeed)

	ctx.nums = resize(ctx.nums, len(p.numPaths))
	for i, cp := range p.numPaths {
		nc, err := ctx.numericColumn(ctx.nums[i], cp, sourceRows, w)
		if err != nil {
			return err
		}
		ctx.nums[i] = nc
	}
	return nil
}

func (ctx *execCtx) classifyRawColumns(p *plan, w *Workspace, textNeed int) {
	if cap(w.text) < textNeed {
		w.text = make([]byte, 0, growCap(cap(w.text), textNeed))
	}
	ctx.values = resize(ctx.values, len(p.valuePaths))
	for i, raws := range w.raws {
		col := resize(ctx.values[i], len(raws))
		for j, r := range raws {
			col[j] = classifyRawInto(r, &w.text)
		}
		ctx.values[i] = col
	}
}

func (ctx *execCtx) extractSnapshot(p *plan, snapshot slopjson.Snapshot, sourceRows []slopjson.StoreRow, compact bool, w *Workspace) error {
	w.raws = resize(w.raws, len(p.valuePaths))
	textNeed := 0
	for i, cp := range p.valuePaths {
		var raws []slopjson.RawValue
		var err error
		if compact {
			raws, err = snapshot.AppendPointerRows(w.raws[i][:0], sourceRows, cp.pointerForStore())
		} else {
			raws, err = snapshot.AppendPointer(w.raws[i][:0], cp.pointerForStore())
		}
		if err != nil {
			return err
		}
		w.raws[i] = raws
		for _, r := range raws {
			b := r.Bytes()
			if r.Kind() == document.String && bytes.IndexByte(b, '\\') >= 0 {
				textNeed += len(b)
			}
		}
	}
	ctx.classifyRawColumns(p, w, textNeed)

	ctx.nums = resize(ctx.nums, len(p.numPaths))
	for i, cp := range p.numPaths {
		if !compact && cp.single {
			vals, valid := snapshot.AppendFieldFloat64(
				ctx.nums[i].vals[:0], ctx.nums[i].valid[:0], cp.name, &ctx.cache,
			)
			ctx.nums[i] = numColumn{vals: vals, valid: valid}
			continue
		}
		var raws []slopjson.RawValue
		var err error
		if compact {
			raws, err = snapshot.AppendPointerRows(w.numRaws[:0], sourceRows, cp.pointerForStore())
		} else {
			raws, err = snapshot.AppendPointer(w.numRaws[:0], cp.pointerForStore())
		}
		if err != nil {
			return err
		}
		w.numRaws = raws
		ctx.nums[i] = numericRaws(ctx.nums[i], raws)
	}
	return nil
}

func (ctx *execCtx) rawColumn(dst []slopjson.RawValue, cp compiledPath, sourceRows []int) ([]slopjson.RawValue, error) {
	if sourceRows != nil {
		if cp.single {
			return ctx.cache.AppendFieldRows(dst, ctx.s, sourceRows, cp.name), nil
		}
		return ctx.s.AppendPointerRows(dst, sourceRows, cp.pointer)
	}
	if cp.single {
		return ctx.cache.AppendField(dst, ctx.s, cp.name), nil
	}
	return ctx.s.AppendPointer(dst, cp.pointer)
}

func (ctx *execCtx) numericColumn(dst numColumn, cp compiledPath, sourceRows []int, w *Workspace) (numColumn, error) {
	if sourceRows == nil && cp.single {
		vals, valid := ctx.cache.AppendFieldFloat64(dst.vals[:0], dst.valid[:0], ctx.s, cp.name)
		return numColumn{vals: vals, valid: valid}, nil
	}
	raws, err := ctx.rawColumn(w.numRaws[:0], cp, sourceRows)
	if err != nil {
		return numColumn{}, err
	}
	w.numRaws = raws
	return numericRaws(dst, raws), nil
}

func numericRaws(dst numColumn, raws []slopjson.RawValue) numColumn {
	vals := resize(dst.vals, len(raws))
	valid := resize(dst.valid, len(raws))
	clear(valid)
	for i, r := range raws {
		if f, ok := r.Float64(); ok {
			vals[i], valid[i] = f, true
		}
	}
	return numColumn{vals: vals, valid: valid}
}

func (p *plan) selectRows(ctx *execCtx, candidates []int, compact bool, w *Workspace) []int {
	selected := w.selected[:0]
	if compact || candidates == nil {
		for row := 0; row < ctx.rows; row++ {
			if p.where == nil || p.where.eval(ctx.values, row, &w.containsEntries) {
				selected = append(selected, row)
			}
		}
	} else {
		for _, row := range candidates {
			if p.where == nil || p.where.eval(ctx.values, row, &w.containsEntries) {
				selected = append(selected, row)
			}
		}
	}
	w.selected = selected
	return selected
}

func (p *plan) candidateRows(s *slopjson.DocSet, w *Workspace) []int {
	if p.where == nil || !s.Postings {
		return nil
	}
	rows, ok := p.where.candidates(s, w)
	if !ok {
		return nil
	}
	if rows == nil {
		return w.emptyCandidate[:0]
	}
	return rows
}

func (p *plan) runProjectionInto(dst *Result, ctx *execCtx, selected []int) {
	if len(p.order) != 0 {
		slices.SortStableFunc(selected, func(a, b int) int {
			return p.compareRows(ctx, a, b)
		})
	}
	if p.hasLimit && len(selected) > p.limit {
		selected = selected[:p.limit]
	}
	prepareResult(dst, p, len(selected))
	for r, row := range selected {
		for c, col := range p.columns {
			dst.Columns[c].Cells[r] = cellFromScalar(ctx.values[col.value][row])
		}
	}
}

func (p *plan) compareRows(ctx *execCtx, a, b int) int {
	for _, o := range p.order {
		c := compareScalar(ctx.values[o.value][a], ctx.values[o.value][b])
		if o.dir == Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func (p *plan) runAggregateInto(dst *Result, ctx *execCtx, selected []int, w *Workspace) {
	w.accs = resize(w.accs, len(p.columns))
	clear(w.accs)
	for _, row := range selected {
		p.accumulate(w.accs, ctx, row)
	}
	rows := 1
	if p.hasLimit && p.limit == 0 {
		rows = 0
	}
	prepareResult(dst, p, rows)
	if rows != 0 {
		p.fillAggregateCells(dst, 0, w.accs, nil, w)
	}
}

func (p *plan) runGroupedInto(dst *Result, ctx *execCtx, selected []int, w *Workspace) {
	groupCount := 0
	for _, row := range selected {
		w.groupKey = p.groupKey(w.groupKey[:0], ctx, row)
		id := int(w.interner.Intern(w.groupKey))
		if id == groupCount {
			w.groups = resize(w.groups, groupCount+1)
			g := &w.groups[id]
			g.scalars = resize(g.scalars, len(p.groupCols))
			g.accs = resize(g.accs, len(p.columns))
			clear(g.accs)
			for i, gc := range p.groupCols {
				g.scalars[i] = ctx.values[gc][row]
			}
			groupCount++
		}
		p.accumulate(w.groups[id].accs, ctx, row)
	}
	w.groups = w.groups[:groupCount]
	w.groupOrder = resize(w.groupOrder[:0], groupCount)
	for i := range w.groupOrder {
		w.groupOrder[i] = i
	}
	if len(p.order) != 0 {
		slices.SortStableFunc(w.groupOrder, func(a, b int) int {
			return p.compareGroups(&w.groups[a], &w.groups[b])
		})
	}
	if p.hasLimit && len(w.groupOrder) > p.limit {
		w.groupOrder = w.groupOrder[:p.limit]
	}
	prepareResult(dst, p, len(w.groupOrder))
	for row, id := range w.groupOrder {
		g := &w.groups[id]
		p.fillAggregateCells(dst, row, g.accs, g, w)
	}
}

func (p *plan) compareGroups(a, b *group) int {
	for _, o := range p.order {
		c := compareScalar(a.scalars[o.slot], b.scalars[o.slot])
		if o.dir == Desc {
			c = -c
		}
		if c != 0 {
			return c
		}
	}
	return 0
}

func (p *plan) groupKey(dst []byte, ctx *execCtx, row int) []byte {
	for _, gc := range p.groupCols {
		dst = appendGroupKey(dst, ctx.values[gc][row])
	}
	return dst
}

type group struct {
	scalars []scalar
	accs    []aggAcc
}

type aggAcc struct {
	count int
	n     int
	sum   float64
	min   float64
	max   float64
}

func (p *plan) accumulate(accs []aggAcc, ctx *execCtx, row int) {
	for c, col := range p.columns {
		switch col.agg {
		case aggCount:
			if col.value < 0 || present(ctx.values[col.value][row]) {
				accs[c].count++
			}
		case aggSum, aggAvg, aggMin, aggMax:
			nc := ctx.nums[col.num]
			if !nc.valid[row] {
				continue
			}
			v := nc.vals[row]
			a := &accs[c]
			if a.n == 0 {
				a.min, a.max = v, v
			} else {
				if v < a.min {
					a.min = v
				}
				if v > a.max {
					a.max = v
				}
			}
			a.sum += v
			a.n++
		}
	}
}

func (p *plan) fillAggregateCells(dst *Result, row int, accs []aggAcc, g *group, w *Workspace) {
	for c, col := range p.columns {
		var cell Cell
		switch col.agg {
		case aggNone:
			cell = cellFromScalar(g.scalars[col.slot])
		case aggCount:
			cell = w.countCell(accs[c].count)
		case aggSum:
			cell = w.numericOrNull(accs[c].n, accs[c].sum)
		case aggAvg:
			if accs[c].n == 0 {
				cell = nullCell()
			} else {
				cell = w.floatCell(accs[c].sum / float64(accs[c].n))
			}
		case aggMin:
			cell = w.numericOrNull(accs[c].n, accs[c].min)
		case aggMax:
			cell = w.numericOrNull(accs[c].n, accs[c].max)
		}
		dst.Columns[c].Cells[row] = cell
	}
}

func (w *Workspace) floatCell(f float64) Cell {
	return Cell{kind: KindNumber, word: math.Float64bits(f)}
}

func (w *Workspace) countCell(n int) Cell {
	return Cell{kind: KindNumber, flag: cellInteger, word: uint64(n)}
}

func (w *Workspace) numericOrNull(n int, v float64) Cell {
	if n == 0 {
		return nullCell()
	}
	return w.floatCell(v)
}

func prepareResult(dst *Result, p *plan, rows int) {
	if cap(dst.Columns) < len(p.columns) {
		dst.Columns = make([]ResultColumn, len(p.columns))
	} else {
		for i := len(p.columns); i < len(dst.Columns); i++ {
			clear(dst.Columns[i].Cells)
		}
		dst.Columns = dst.Columns[:len(p.columns)]
	}
	for i := range p.columns {
		cells := dst.Columns[i].Cells
		if rows < len(cells) {
			clear(cells[rows:])
		}
		cells = resize(cells, rows)
		dst.Columns[i].Header = p.headers[i]
		dst.Columns[i].Cells = cells
	}
	dst.RowCount = rows
}

func resize[T any](s []T, n int) []T {
	if cap(s) < n {
		out := make([]T, n, growCap(cap(s), n))
		copy(out, s)
		return out
	}
	return s[:n]
}

func growCap(old, need int) int {
	n := old * 2
	if n < 8 {
		n = 8
	}
	if n < need {
		n = need
	}
	return n
}
