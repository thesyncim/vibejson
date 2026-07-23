// Package query is a typed, single-table query engine over a
// [simdjson.DocSet], heap [simdjson.Snapshot], or durable
// [simdjson.FileSnapshot]: the product layer that turns indexing, projection,
// containment, and grouping primitives into one compiled plan with a
// programmatic builder and an optional SQL front end. Each document is one row
// and columns are JSON paths. It answers SELECT of path projections and
// aggregates
// (COUNT, SUM, AVG, MIN, MAX); WHERE with comparisons, containment (@>),
// existence, and null tests combined by And/Or/Not; GROUP BY; ORDER BY; and
// LIMIT. Joins, subqueries, mutation, and full SQL are out of scope.
//
// The builder and optional SQL parser are front ends for one immutable [Plan].
// Preparing resolves paths to compiled pointers and numeric slots, predicates
// to typed operators, and literals to typed constants. SQL text and the
// builder tree are then discarded; neither is interpreted during execution:
//
//	q := query.Select(query.Path("name"), query.Sum("score")).
//		Where(query.Cmp("active", query.Eq, true)).
//		GroupBy("team").
//		OrderBy("team", query.Asc).
//		Limit(10)
//	plan, err := q.Prepare()
//	result, err := plan.Run(&docs)
//
// Hot paths retain their destination and scratch storage:
//
//	var result query.Result
//	var workspace query.Workspace
//	err = plan.RunInto(&result, &docs, &workspace)
//
// [PrepareSQL] produces the identical Plan directly from SQL. Plan output has
// stable ordinal IDs through [Plan.AppendSchema], and [Cell] exposes typed
// values plus caller-buffered [Cell.AppendJSON]. A transport encoder can
// therefore consume typed batches without header lookup or intermediate
// string formatting. Field-name bytes remain only in immutable compiled-path
// metadata because schemaless JSON has no external schema ID to replace them.
//
// The executor is column-oriented. Without an applicable posting bound it
// extracts each needed path as a dense column and evaluates WHERE in one full
// scan. With a selective bound it pushes the posting ordinals into extraction:
// [simdjson.ShapeCache.AppendFieldRows] and
// [simdjson.DocSet.AppendPointerRows] gather only candidate cells, then the
// same compiled predicate rechecks them exactly before reduction, grouping,
// ordering, and limiting. A compiled query is immutable and safe to run
// concurrently; Run owns its transient scan state, while concurrent RunInto
// calls use one independent Result and Workspace pair per goroutine.
// [Query.RunFileSnapshot] first late-binds exact persistent indexes. A bounded
// plan admits only its collision-rechecked stable-slot masks; an unbounded plan
// scans every row. It then indexes bounded raw batches in parallel, restores
// source order before partial reductions, and externally merges ordered
// projections or groups when their transient frontier reaches the configured
// memory target. The caller owns the final result, whose size is necessarily
// outside that working-memory target.
//
// # Value semantics
//
// The engine defines the following, so results are predictable across every
// document shape:
//
//   - An absent path and an explicit JSON null are one value, "null". IsNull
//     tests for it; Exists distinguishes a present null from an absent path.
//   - Comparisons are within type. Numbers compare by exact decimal value —
//     1, 1.0, and 1e2 versus 100 compare as equals, and integers past
//     float64's mantissa stay distinct — strings by decoded content, bools by
//     value. Across types there is a defined total order (null < bool <
//     number < string < container) for ORDER BY and GROUP BY, and inequality
//     for =/!=; a null or absent value never satisfies a comparison.
//   - SUM, AVG, MIN, and MAX skip null and non-numeric values and are null
//     over an empty input. COUNT(path) counts present, non-null values;
//     COUNT(*) counts rows.
//   - Duplicate object keys resolve to the last occurrence, matching the
//     core's Node.Get.
package query

import (
	"fmt"
	"sync"

	"github.com/thesyncim/simdjson"
)

// A Direction is an ORDER BY sort direction.
type Direction uint8

const (
	// Asc sorts ascending, nulls first.
	Asc Direction = iota
	// Desc sorts descending, nulls last.
	Desc
)

// A Query is a compiled, reusable query plan built by Select and the chaining
// methods. It is immutable once built; Run compiles it on first use and caches
// the compiled plan, so later Runs reuse it and are safe to call concurrently.
type Query struct {
	columns  []Column
	where    Predicate
	hasWhere bool
	groupBy  []string
	orderBy  []orderSpec
	limit    int
	hasLimit bool

	once       sync.Once
	plan       *plan
	compileErr error
}

type orderSpec struct {
	path string
	dir  Direction
}

// Select begins a query that projects and aggregates the given columns. The
// columns become the result's columns, in order.
func Select(columns ...Column) *Query {
	return &Query{columns: columns}
}

// Where sets the query's filter predicate. A later Where replaces an earlier
// one; combine conditions with And, Or, and Not.
func (q *Query) Where(p Predicate) *Query {
	q.where = p
	q.hasWhere = true
	return q
}

// GroupBy groups rows by the values at the given paths. Every non-aggregate
// projected column must then be one of these paths.
func (q *Query) GroupBy(paths ...string) *Query {
	q.groupBy = append(q.groupBy, paths...)
	return q
}

// OrderBy adds a sort key. Keys apply in the order added. Without GROUP BY the
// key is a per-row path; with GROUP BY it must be a grouped path.
func (q *Query) OrderBy(path string, dir Direction) *Query {
	q.orderBy = append(q.orderBy, orderSpec{path: path, dir: dir})
	return q
}

// Limit caps the number of result rows. A negative n means no limit.
func (q *Query) Limit(n int) *Query {
	if n < 0 {
		q.hasLimit = false
		return q
	}
	q.limit = n
	q.hasLimit = true
	return q
}

// A plan is a compiled query: value columns to extract, a compiled predicate,
// aggregate reductions, grouping, ordering, and the row limit.
type plan struct {
	valuePaths []compiledPath // extracted as scalar columns
	numPaths   []compiledPath // extracted as numeric columns (aggregate args)

	headers []string // result schema; cold metadata, parallel to columns
	columns []planColumn
	where   *compiledPredicate

	grouped   bool
	groupCols []int // value-column indices of GROUP BY paths

	hasAggregate bool
	singleRow    bool // aggregates without GROUP BY: one result row

	order    []planOrder
	limit    int
	hasLimit bool
}

// A planColumn is one compiled SELECT column.
type planColumn struct {
	agg   aggKind
	value int // scalar-column index for a projection or COUNT(path); -1 for COUNT(*)
	num   int // numeric-column index for SUM/AVG/MIN/MAX; -1 otherwise
	slot  int // for a grouped projection, its position in groupCols; -1 otherwise
}

// A planOrder is one compiled ORDER BY key.
type planOrder struct {
	value int // scalar-column index
	slot  int // grouped: position in groupCols; -1 when ordering per row
	dir   Direction
}

// pathRegistry assigns each distinct value path one column index, so a path
// used by several clauses is extracted once.
type pathRegistry struct {
	index map[string]int
	paths []compiledPath
}

func newPathRegistry() *pathRegistry {
	return &pathRegistry{index: map[string]int{}}
}

func (r *pathRegistry) add(spec string) (int, error) {
	if i, ok := r.index[spec]; ok {
		return i, nil
	}
	cp, err := compilePath(spec)
	if err != nil {
		return 0, err
	}
	i := len(r.paths)
	r.paths = append(r.paths, cp)
	r.index[spec] = i
	return i, nil
}

// compiled returns the query's compiled plan, compiling once on first call.
func (q *Query) compiled() (*plan, error) {
	q.once.Do(func() {
		q.plan, q.compileErr = q.compile()
	})
	return q.plan, q.compileErr
}

// compile validates the builder state and lowers it to a plan.
func (q *Query) compile() (*plan, error) {
	if len(q.columns) == 0 {
		return nil, fmt.Errorf("query: Select requires at least one column")
	}
	values := newPathRegistry()
	numReg := newPathRegistry()

	grouped := len(q.groupBy) > 0
	groupSet := map[string]bool{}
	for _, g := range q.groupBy {
		groupSet[g] = true
	}

	p := &plan{
		grouped:  grouped,
		limit:    q.limit,
		hasLimit: q.hasLimit,
	}
	p.headers = make([]string, 0, len(q.columns))
	p.columns = make([]planColumn, 0, len(q.columns))

	hasProjection := false
	for _, col := range q.columns {
		pc := planColumn{agg: col.agg, value: -1, num: -1, slot: -1}
		switch col.agg {
		case aggNone:
			hasProjection = true
			if grouped && !groupSet[col.spec] {
				return nil, fmt.Errorf("query: projected column %q must appear in GROUP BY", col.spec)
			}
			idx, err := values.add(col.spec)
			if err != nil {
				return nil, err
			}
			pc.value = idx
		case aggCount:
			p.hasAggregate = true
			if col.spec != "" {
				idx, err := values.add(col.spec)
				if err != nil {
					return nil, err
				}
				pc.value = idx
			}
		default: // SUM, AVG, MIN, MAX
			p.hasAggregate = true
			idx, err := numReg.add(col.spec)
			if err != nil {
				return nil, err
			}
			pc.num = idx
		}
		p.headers = append(p.headers, col.header)
		p.columns = append(p.columns, pc)
	}

	if p.hasAggregate && hasProjection && !grouped {
		return nil, fmt.Errorf("query: cannot mix a projection with an aggregate without GROUP BY")
	}
	p.singleRow = p.hasAggregate && !grouped

	if q.hasWhere {
		cp, err := compilePredicate(q.where, values)
		if err != nil {
			return nil, err
		}
		p.where = cp
	}

	groupSlot := make(map[int]int, len(q.groupBy))
	for _, g := range q.groupBy {
		idx, err := values.add(g)
		if err != nil {
			return nil, err
		}
		if _, seen := groupSlot[idx]; !seen {
			groupSlot[idx] = len(p.groupCols)
			p.groupCols = append(p.groupCols, idx)
		}
	}
	// Resolve each grouped projection to its group-key slot.
	for i := range p.columns {
		if p.columns[i].agg == aggNone && grouped {
			p.columns[i].slot = groupSlot[p.columns[i].value]
		}
	}

	if err := q.compileOrder(p, values, groupSet, groupSlot); err != nil {
		return nil, err
	}

	p.valuePaths = values.paths
	p.numPaths = numReg.paths
	return p, nil
}

// compileOrder resolves the ORDER BY keys, enforcing the grouped-path rule and
// skipping ordering for a single-row aggregate result.
func (q *Query) compileOrder(p *plan, values *pathRegistry, groupSet map[string]bool, groupSlot map[int]int) error {
	if p.singleRow {
		return nil // one result row; nothing to order
	}
	for _, o := range q.orderBy {
		if p.grouped && !groupSet[o.path] {
			return fmt.Errorf("query: ORDER BY %q must appear in GROUP BY", o.path)
		}
		idx, err := values.add(o.path)
		if err != nil {
			return err
		}
		po := planOrder{value: idx, slot: -1, dir: o.dir}
		if p.grouped {
			po.slot = groupSlot[idx]
		}
		p.order = append(p.order, po)
	}
	return nil
}

// Run executes the query over s and returns the column-oriented result. It
// compiles the query on first use. Run does not modify s and may be called
// concurrently on a compiled query, each call using its own scan state.
func (q *Query) Run(s *simdjson.DocSet) (Result, error) {
	var result Result
	var workspace Workspace
	err := q.RunInto(&result, s, &workspace)
	return result, err
}

// RunInto executes the query into caller-owned result and workspace storage.
// The zero values of Result and Workspace are ready to use. After one warm-up,
// executions whose row count, posting frontier, decoded text, and group count
// fit the retained high-water marks allocate no heap memory, including stable
// ordering, grouping, containment indexing, typed aggregates, and escaped
// string decoding. Use Cell.AppendJSON with a retained destination to format
// computed aggregates without allocating.
//
// RunInto overwrites dst. Result cells may borrow both s and w; they remain
// valid until s is modified, dst is reused, or the next RunInto using w. A
// Workspace and Result are single-consumer. The Query itself remains safe for
// concurrent execution with a distinct pair per goroutine.
func (q *Query) RunInto(dst *Result, s *simdjson.DocSet, w *Workspace) error {
	if dst == nil || s == nil || w == nil {
		return fmt.Errorf("query: RunInto requires non-nil result, DocSet, and Workspace")
	}
	p, err := q.compiled()
	if err != nil {
		return err
	}
	return p.runInto(dst, s, w)
}

// RunSnapshot executes q over an immutable Store snapshot. Declared exact
// indexes are bound from that snapshot's catalog at execution time, so a Query
// compiled before online index creation can use it once Ready without being
// recompiled.
func (q *Query) RunSnapshot(s simdjson.Snapshot) (Result, error) {
	var result Result
	var workspace Workspace
	err := q.RunSnapshotInto(&result, s, &workspace)
	return result, err
}

// RunSnapshotInto is the caller-owned, zero-steady-allocation form of
// [Query.RunSnapshot]. Candidate predicates combine native stable-slot masks;
// only surviving rows are decoded before exact predicate recheck and late
// projection, grouping, aggregation, ordering, and limiting.
func (q *Query) RunSnapshotInto(dst *Result, s simdjson.Snapshot, w *Workspace) error {
	if dst == nil || w == nil {
		return fmt.Errorf("query: RunSnapshotInto requires non-nil result and Workspace")
	}
	p, err := q.compiled()
	if err != nil {
		return err
	}
	return p.runSnapshotInto(dst, s, w)
}
