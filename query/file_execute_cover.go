package query

import (
	"math/bits"

	"github.com/thesyncim/simdjson"
)

type directFileIndexStats struct {
	rows         uint64
	rechecks     uint64
	certificates uint64
	lookups      int
	postingPages int
	chunks       int
	bounded      bool
}

type directFileGroupStats struct {
	rechecks     uint64
	certificates uint64
	lookups      int
	postingPages int
	groups       int
}

// runDirectFileIndexedCount recognizes COUNT(*) over a predicate whose entire
// truth set is covered by persistent exact indexes. The exact probe performs
// collision rechecks once, after which popcount answers the aggregate without
// rebuilding DocSets, extracting columns, or evaluating @> a second time.
//
// An object containment needle made entirely of scalar leaves is eligible
// because compilation proves it equivalent to exact nested path equalities.
// A matching compound index can certify the complete conjunction in one
// probe. Arrays and empty objects retain the structural containment evaluator.
func (p *plan) runDirectFileIndexedCount(
	dst *Result,
	snapshot *simdjson.FileSnapshot,
	w *FileExecutionWorkspace,
) (directFileIndexStats, bool, error) {
	if p.where == nil || p.grouped || !p.singleRow {
		return directFileIndexStats{}, false, nil
	}
	for _, column := range p.columns {
		if column.agg != aggCount || column.value >= 0 {
			return directFileIndexStats{}, false, nil
		}
	}
	if p.hasLimit && p.limit == 0 {
		prepareResult(dst, p, 0)
		return directFileIndexStats{}, true, nil
	}
	masks, rechecks, certificates, postingPages, exact, err := p.fileExactCandidateMasks(
		snapshot, &w.index, &w.planner,
	)
	if err != nil {
		return directFileIndexStats{
			rechecks: rechecks, certificates: certificates,
			postingPages: postingPages, bounded: true,
		}, true, err
	}
	if !exact {
		return directFileIndexStats{}, false, nil
	}
	var rows uint64
	chunks := 0
	for _, mask := range masks {
		count := bits.OnesCount64(mask.Bits)
		if count == 0 {
			continue
		}
		rows += uint64(count)
		chunks++
	}
	if rows > uint64(^uint(0)>>1) {
		return directFileIndexStats{
			rows: rows, rechecks: rechecks, certificates: certificates,
			lookups: w.planner.storeIndexProbes, postingPages: postingPages,
			chunks: chunks, bounded: true,
		}, true, simdjson.ErrStoreTooLarge
	}
	w.accs = resize(w.accs, len(p.columns))
	clear(w.accs)
	for i := range w.accs {
		w.accs[i].count = int(rows)
	}
	prepareResult(dst, p, 1)
	p.fillAggregateCells(dst, 0, w.accs, nil, &w.planner)
	return directFileIndexStats{
		rows: rows, rechecks: rechecks, certificates: certificates,
		lookups: w.planner.storeIndexProbes, postingPages: postingPages,
		chunks: chunks, bounded: true,
	}, true, nil
}

// runDirectFileAggregate recognizes an unfiltered scalar aggregate whose
// numeric inputs all have persistent typed covers. It bypasses worker startup,
// JSON admission, parsing, and per-row validity columns. COUNT(path) declines
// because a numeric cover intentionally omits present non-numeric values.
func (p *plan) runDirectFileAggregate(
	dst *Result,
	snapshot *simdjson.FileSnapshot,
	w *FileExecutionWorkspace,
) (coveringColumns int, handled bool, err error) {
	if p.where != nil || p.grouped || !p.singleRow {
		return 0, false, nil
	}
	for _, column := range p.columns {
		switch column.agg {
		case aggCount:
			if column.value >= 0 {
				return 0, false, nil
			}
		case aggSum, aggAvg, aggMin, aggMax:
			if column.num < 0 || column.num >= len(p.numPaths) {
				return 0, false, nil
			}
		default:
			return 0, false, nil
		}
	}
	if p.hasLimit && p.limit == 0 {
		prepareResult(dst, p, 0)
		return 0, true, nil
	}
	if snapshot.Len() > uint64(^uint(0)>>1) {
		return 0, true, simdjson.ErrStoreTooLarge
	}

	w.reductions = resize(w.reductions, len(p.numPaths))
	clear(w.reductions)
	w.coverPaths = resize(w.coverPaths, len(p.numPaths))
	for i, path := range p.numPaths {
		w.coverPaths[i] = path.indexPath()
	}
	covered, reduceErr := snapshot.ReduceFloat64PathsInto(w.reductions, w.coverPaths)
	if reduceErr != nil {
		return 0, true, reduceErr
	}
	if !covered {
		return 0, false, nil
	}
	coveringColumns = len(p.numPaths)
	w.accs = resize(w.accs, len(p.columns))
	clear(w.accs)
	for resultColumn, column := range p.columns {
		if column.agg == aggCount {
			w.accs[resultColumn].count = int(snapshot.Len())
			continue
		}
		reduction := w.reductions[column.num]
		w.accs[resultColumn] = aggAcc{
			n: reduction.Count, sum: reduction.Sum,
			min: reduction.Min, max: reduction.Max,
		}
	}

	prepareResult(dst, p, 1)
	p.fillAggregateCells(dst, 0, w.accs, nil, &w.planner)
	return coveringColumns, true, nil
}

// runDirectFileIndexGroups lowers an unfiltered scalar COUNT GROUP BY to a
// matching single-column exact index. A compact generation can answer from
// bounded O(groups) aggregate catalog pages. Otherwise stable-slot postings
// and their exact representatives decide certified rows; missing, container,
// legacy, and collision-marked rows form a residual bitmap and take the
// ordinary compiled-pointer path. Both lanes preserve exact group semantics
// and first-row ordering.
func (p *plan) runDirectFileIndexGroups(
	dst *Result,
	snapshot *simdjson.FileSnapshot,
	w *FileExecutionWorkspace,
) (directFileGroupStats, bool, error) {
	if p.where != nil || !p.grouped || len(p.groupCols) != 1 {
		return directFileGroupStats{}, false, nil
	}
	for _, column := range p.columns {
		switch {
		case column.agg == aggNone && column.slot == 0:
		case column.agg == aggCount && column.value < 0:
		default:
			return directFileGroupStats{}, false, nil
		}
	}
	if snapshot.Len() > uint64(^uint(0)>>1) {
		return directFileGroupStats{}, true, simdjson.ErrStoreTooLarge
	}
	path := p.valuePaths[p.groupCols[0]]
	indexes := snapshot.AppendIndexes(w.planner.storeIndexes[:0])
	w.planner.storeIndexes = indexes
	indexName := ""
	for _, index := range indexes {
		if index.Kind == simdjson.StoreIndexExact &&
			index.State == simdjson.StoreIndexReady &&
			index.ColumnCount == 1 &&
			index.Columns[0] == path.indexPath() {
			indexName = index.Name
			break
		}
	}
	if indexName == "" {
		return directFileGroupStats{}, false, nil
	}
	if p.hasLimit && p.limit == 0 {
		prepareResult(dst, p, 0)
		return directFileGroupStats{lookups: 1}, true, nil
	}

	w.indexGroups = w.indexGroups[:0]
	w.indexResidual = w.indexResidual[:0]
	var ok bool
	var err error
	w.indexGroups, w.indexResidual, ok, err =
		snapshot.AppendIndexScalarGroupsInto(
			w.indexGroups, w.indexResidual, &w.index, indexName,
		)
	probe := w.index.LastProbeStats()
	stats := directFileGroupStats{
		certificates: probe.CertificateRows,
		lookups:      1,
		postingPages: probe.PostingPages,
	}
	if err != nil {
		return stats, true, err
	}
	if !ok {
		return directFileGroupStats{}, false, nil
	}

	w.planner.groupKey = w.planner.groupKey[:0]
	w.planner.text = w.planner.text[:0]
	w.planner.interner.Reset()
	groupCount := 0
	add := func(value scalar, count int, first uint64) {
		w.planner.groupKey = appendGroupKey(w.planner.groupKey[:0], value)
		id := int(w.planner.interner.Intern(w.planner.groupKey))
		if id == groupCount {
			w.fileGroups = resize(w.fileGroups, groupCount+1)
			group := &w.fileGroups[id]
			group.key = ""
			group.scalars = resize(group.scalars, 1)
			// The scalar borrows the file execution workspace only until
			// fileGroupResultInto packs it into caller-owned Result storage.
			group.scalars[0] = value
			group.accs = resize(group.accs, len(p.columns))
			clear(group.accs)
			group.first = first
			group.bytes = 0
			groupCount++
		} else if first < w.fileGroups[id].first {
			w.fileGroups[id].first = first
		}
		for column, planColumn := range p.columns {
			if planColumn.agg == aggCount {
				w.fileGroups[id].accs[column].count += count
			}
		}
	}
	for _, group := range w.indexGroups {
		add(
			classifyRawInto(group.Value, &w.planner.text),
			int(group.Count), group.First,
		)
	}

	w.overflow, err = snapshot.RangeMasksRawRowsBuffer(
		w.indexResidual, w.overflow[:0],
		func(row simdjson.StoreRow, _ []byte, document []byte) error {
			raw, found, pointerErr := path.pointerForStore().GetRaw(document)
			if pointerErr != nil {
				return pointerErr
			}
			if !found {
				raw = simdjson.RawValue{}
			}
			add(
				classifyRawInto(raw, &w.planner.text), 1,
				uint64(row.Chunk)<<6|uint64(row.Slot),
			)
			stats.rechecks++
			return nil
		},
	)
	if err != nil {
		return stats, true, err
	}
	w.fileGroups = w.fileGroups[:groupCount]
	stats.groups = groupCount
	*dst = p.fileGroupResultInto(*dst, w.fileGroups)
	return stats, true, nil
}
