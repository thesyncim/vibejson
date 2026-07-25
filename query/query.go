// Package query is a typed, single-table query engine over a
// [store.DocSet], heap [store.Snapshot], or a durable snapshot: the product
// layer that turns indexing, projection,
// containment, and grouping primitives into one compiled plan with a
// programmatic builder and an optional SQL front end. Each document is one row
// and columns are JSON paths. It answers SELECT of path projections and
// aggregates
// (COUNT, SUM, AVG, MIN, MAX); WHERE with comparisons, containment (@>),
// existence, and null tests combined by And/Or/Not; GROUP BY; ORDER BY; and
// LIMIT. Joins, subqueries, mutation, and full SQL are out of scope.
//
// The builder, the JSON document front end, and the optional SQL parser are
// front ends for one immutable compiled plan, cached inside the [Query] that
// produced it. Compiling resolves paths to compiled pointers and numeric
// slots, predicates to typed operators, and literals to typed constants. The
// query text and the builder tree are then discarded; none of them is
// interpreted during execution:
//
//	q := query.Select(query.Path("name"), query.Sum("score")).
//		Where(query.Cmp("active", query.Eq, true)).
//		GroupBy("team").
//		OrderBy("team", query.Asc).
//		Limit(10)
//	result, err := q.Run(query.FromDocSet(&docs))
//
// A query is also expressible as a JSON document, so one can be stored,
// logged, or received over a wire and compiled to the same plan. [New] takes
// that document as Go literals and [Parse] takes it as JSON text. Sibling keys
// conjoin, which is what makes the common all-of filter read as data rather
// than as a tree of constructors:
//
//	q, err := query.New(query.M{
//		"select":  query.A{"team", query.M{"total": query.M{"$sum": "score"}}},
//		"where":   query.M{"active": true, "tier": query.M{"$in": query.A{"pro", "team"}}},
//		"groupBy": "team",
//		"orderBy": query.A{query.M{"total": "desc"}},
//		"limit":   10,
//	})
//
// Execution has exactly two entry points. [Query.Run] answers a one-off, and
// [Query.RunInto] runs into a caller-owned [Exec] that retains the destination
// Result, the scratch Workspace, the durable backend's options, and its
// reported stats. Both take a [Source], the discriminated handle naming the
// collection: [FromDocSet], [FromSnapshot], or [FromFile]. One backend is
// therefore never a different call shape from another, and a hot loop is one
// Exec reused:
//
//	var e query.Exec
//	err = q.RunInto(&e, query.FromSnapshot(snapshot))
//
// [PrepareSQL] produces an identically compiled Query directly from SQL, and
// [Query.Prepare] forces compilation eagerly so a malformed query fails where
// it was written. Output has stable ordinal IDs through [Query.AppendSchema],
// and [Cell] exposes typed values plus caller-buffered [Cell.AppendJSON]. A
// transport encoder can therefore consume typed batches without header lookup
// or intermediate string formatting. Field-name bytes remain only in immutable compiled-path
// metadata because schemaless JSON has no external schema ID to replace them.
//
// The executor is column-oriented. Without an applicable posting bound it
// extracts each needed path as a dense column and evaluates WHERE in one full
// scan. With a selective bound it pushes the posting ordinals into extraction:
// [store.ShapeCache.AppendFieldRows] and
// [store.DocSet.AppendPointerRows] gather only candidate cells, then the
// same compiled predicate rechecks them exactly before reduction, grouping,
// ordering, and limiting. A compiled query is immutable and safe to run
// concurrently; Run owns its transient scan state, while concurrent RunInto
// calls use one independent Exec per goroutine.
// A [FromFile] execution first late-binds exact persistent indexes. A bounded
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
// methods. It is immutable once built; execution compiles it on first use and
// caches the compiled plan, so later executions reuse it and are safe to call
// concurrently — one Query, one [Exec] per goroutine. There is no separate
// prepared-plan type: a Query that has compiled is the prepared plan, which is
// why [Query.Prepare] only has to report whether compilation succeeded.
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
