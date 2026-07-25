// Package query is a typed, single-table query engine over a
// [store.Segment], heap [store.Snapshot], or a durable snapshot: the product
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
//	result, err := q.Run(query.FromSegment(&docs))
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
// [New] and [Parse] own everything they return. A service compiling a query
// document per request instead holds a [Compiler], whose arenas a warmed
// compilation refills rather than reallocates, making a steady-state compile
// free of heap allocation the same way a warmed [Query.RunInto] is. The Query
// it produces borrows that compiler and is valid until its next compile, the
// borrowed-lifetime rule [Result] and [Workspace] already carry.
//
// Execution has exactly two entry points. [Query.Run] answers a one-off, and
// [Query.RunInto] runs into a caller-owned [Exec] that retains the destination
// Result, the scratch Workspace, the durable backend's options, and its
// reported stats. Both take a [Source], the discriminated handle naming the
// collection: [FromSegment], [FromSnapshot], or [FromFile]. One backend is
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
// [store.Segment.AppendPointerRows] gather only candidate cells, then the
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

	// built is the outcome a [Compiler] installed directly. A Compiler has
	// already compiled by the time it returns, so consulting this first is
	// what lets it reuse one Query value across compiles: sync.Once is not
	// resettable, and a second compile into a Query whose Once had already
	// fired would otherwise keep answering with the first compile's plan.
	built *compileResult
}

// A compileResult is a compiled plan or the failure that prevented one. It is
// one heap object rather than two Query fields so a Compiler can publish both
// halves with a single pointer store.
type compileResult struct {
	plan *plan
	err  error
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
//
// The specs are scanned linearly rather than hashed. A query names a handful
// of paths, so the map this replaced cost one allocation to build plus a hash
// per lookup in order to save a few string comparisons that a length mismatch
// already rejects on their first word.
type pathRegistry struct {
	paths []compiledPath
}

// reset empties the registry while keeping its storage for the next compile.
func (r *pathRegistry) reset() {
	r.paths = r.paths[:0]
}

// addPath returns spec's column index, compiling and registering it the first
// time this query names it.
func (c *Compiler) addPath(r *pathRegistry, spec string) (int, error) {
	for i := range r.paths {
		if r.paths[i].spec == spec {
			return i, nil
		}
	}
	cp, err := c.compilePath(spec)
	if err != nil {
		return 0, err
	}
	r.paths = append(r.paths, cp)
	return len(r.paths) - 1, nil
}

// compiled returns the query's compiled plan, compiling once on first call.
func (q *Query) compiled() (*plan, error) {
	if built := q.built; built != nil {
		return built.plan, built.err
	}
	q.once.Do(func() {
		q.plan, q.compileErr = q.compile()
	})
	return q.plan, q.compileErr
}

// compile validates the builder state and lowers it to a plan, with storage
// the returned plan owns outright. A query the builder or the SQL front end
// produced has no Compiler behind it, so it gets a throwaway one whose arenas
// nothing else can rewind — which is exactly what "the plan owns its storage"
// means here.
func (q *Query) compile() (*plan, error) {
	var c Compiler
	c.forOneShot()
	return c.compilePlan(q)
}

// compilePlan validates the builder state and lowers it to a plan in c's
// storage. The reused slices are taken back whatever the outcome, so a failed
// compile does not throw away the capacity the compiler had already grown.
func (c *Compiler) compilePlan(q *Query) (*plan, error) {
	p := c.planStorage()
	err := c.buildPlan(q, p)
	c.headers = p.headers
	c.planCols = p.columns
	c.groupCols = p.groupCols
	c.planOrder = p.order
	c.keep(q)
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (c *Compiler) buildPlan(q *Query, p *plan) error {
	if len(q.columns) == 0 {
		return fmt.Errorf("query: Select requires at least one column")
	}
	values := &c.values
	numReg := &c.numbers

	grouped := len(q.groupBy) > 0

	*p = plan{
		grouped:  grouped,
		limit:    q.limit,
		hasLimit: q.hasLimit,
	}
	// The column-shaped slices are sized from the select list and the path
	// registries from every clause that can name one, so a one-shot compile
	// fills each in a single allocation rather than growing into it.
	p.headers = reserve(c.headers[:0], len(q.columns))
	p.columns = reserve(c.planCols[:0], len(q.columns))
	p.groupCols = reserve(c.groupCols[:0], len(q.groupBy))
	p.order = reserve(c.planOrder[:0], len(q.orderBy))
	values.paths = reserve(values.paths, len(q.columns)+len(q.groupBy)+len(q.orderBy))

	hasProjection := false
	for _, col := range q.columns {
		pc := planColumn{agg: col.agg, value: -1, num: -1, slot: -1}
		switch col.agg {
		case aggNone:
			hasProjection = true
			if grouped && !namesPath(q.groupBy, col.spec) {
				return fmt.Errorf("query: projected column %q must appear in GROUP BY", col.spec)
			}
			idx, err := c.addPath(values, col.spec)
			if err != nil {
				return err
			}
			pc.value = idx
		case aggCount:
			p.hasAggregate = true
			if col.spec != "" {
				idx, err := c.addPath(values, col.spec)
				if err != nil {
					return err
				}
				pc.value = idx
			}
		default: // SUM, AVG, MIN, MAX
			p.hasAggregate = true
			idx, err := c.addPath(numReg, col.spec)
			if err != nil {
				return err
			}
			pc.num = idx
		}
		p.headers = append(p.headers, col.header)
		p.columns = append(p.columns, pc)
	}

	if p.hasAggregate && hasProjection && !grouped {
		return fmt.Errorf("query: cannot mix a projection with an aggregate without GROUP BY")
	}
	p.singleRow = p.hasAggregate && !grouped

	if q.hasWhere {
		cp, err := c.compilePredicate(q.where, values)
		if err != nil {
			return err
		}
		p.where = cp
	}

	for _, g := range q.groupBy {
		idx, err := c.addPath(values, g)
		if err != nil {
			return err
		}
		if !hasColumn(p.groupCols, idx) {
			p.groupCols = append(p.groupCols, idx)
		}
	}
	// Resolve each grouped projection to its group-key slot.
	for i := range p.columns {
		if p.columns[i].agg == aggNone && grouped {
			p.columns[i].slot = groupSlotOf(p.groupCols, p.columns[i].value)
		}
	}

	if err := c.compileOrder(q, p, values); err != nil {
		return err
	}

	p.valuePaths = values.paths
	p.numPaths = numReg.paths
	return nil
}

// compileOrder resolves the ORDER BY keys, enforcing the grouped-path rule and
// skipping ordering for a single-row aggregate result.
func (c *Compiler) compileOrder(q *Query, p *plan, values *pathRegistry) error {
	if p.singleRow {
		return nil // one result row; nothing to order
	}
	for _, o := range q.orderBy {
		if p.grouped && !namesPath(q.groupBy, o.path) {
			return fmt.Errorf("query: ORDER BY %q must appear in GROUP BY", o.path)
		}
		idx, err := c.addPath(values, o.path)
		if err != nil {
			return err
		}
		po := planOrder{value: idx, slot: -1, dir: o.dir}
		if p.grouped {
			po.slot = groupSlotOf(p.groupCols, idx)
		}
		p.order = append(p.order, po)
	}
	return nil
}

// namesPath reports whether paths contains spec. It replaces the set map the
// GROUP BY rules used to build, on the same reasoning as pathRegistry: a
// GROUP BY names a couple of paths, so the map was one allocation spent to
// avoid a handful of comparisons.
func namesPath(paths []string, spec string) bool {
	for _, p := range paths {
		if p == spec {
			return true
		}
	}
	return false
}

// hasColumn reports whether cols already registers the value column idx.
func hasColumn(cols []int, idx int) bool {
	for _, col := range cols {
		if col == idx {
			return true
		}
	}
	return false
}

// groupSlotOf answers a value column's position among the group keys. An
// unregistered column answers slot zero, matching the map lookup this
// replaced; the projection and ordering rules above have already made that
// case unreachable.
func groupSlotOf(groupCols []int, value int) int {
	for i, col := range groupCols {
		if col == value {
			return i
		}
	}
	return 0
}
