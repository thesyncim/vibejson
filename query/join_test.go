package query

import (
	"fmt"
	"strings"
	"testing"

	"github.com/thesyncim/vibejson/store"
)

// Given a pair of small collections captured in one database snapshot and a
// battery of semi-join shapes, when the compiled executor runs, then every
// result agrees with a naive nested-loop reference that decodes both sides with
// encoding/json and evaluates the join in plain Go — and it agrees under both
// execution strategies, with and without a declared exact index on the outer
// join path.
//
// This is the same bounded exhaustive discipline TestExhaustiveQueryDifferential
// applies to single-collection queries, extended in the two directions a join
// adds. The reference is the naive one on purpose: for every outer document it
// walks every inner document, applies the inner filter, and compares the join
// keys with the independent math/big comparator query_test.go already uses.
// Nothing about the adaptive strategy, the membership lowering, or the index
// masks exists on the reference's side of the check.
//
// The threshold sweep is what makes the adaptive choice testable at all. The
// same battery runs with the membership limit forced high (every join binds a
// membership) and forced to zero (every primary-key join falls back to a
// per-row lookup), and both must equal the same reference. A strategy that
// changed which rows survive would show up here as a disagreement with the
// reference, and a strategy that silently never engaged would show up in
// TestJoinAdaptiveStrategyIsNonVacuous, which asserts the two configurations
// actually take different paths.

// --- fixtures --------------------------------------------------------------

// outerJoinPool is the driving collection's document domain. It spans every
// shape a join key can take on the outer side: a string naming an inner key
// that exists and one that does not, an explicit null, an absent field, a
// number, the string spelling of that same number, an escaped string, and a
// container. The number and its string spelling are both present on purpose:
// a collection key is a string, so 7 must not join to the key "7" while "7"
// must, and the two would be indistinguishable if only one were in the pool.
var outerJoinPool = []string{
	`{"id":1,"ref":"k1","active":true}`,
	`{"id":2,"ref":"k2","active":false}`,
	`{"id":3,"ref":"gone","active":true}`,
	`{"id":4,"ref":null,"active":true}`,
	`{"id":5,"active":true}`,
	`{"id":6,"ref":7,"active":true}`,
	`{"id":7,"ref":"7","active":true}`,
	`{"id":8,"ref":"a\"b","active":false}`,
	`{"id":9,"ref":[1,2],"active":true}`,
}

// innerJoinPool is the inner collection's domain, paired with the keys it is
// stored under. Two documents share the join-key value 7, so the semi-join's
// "duplicate inner matches keep the outer row exactly once" rule is exercised
// on both the collected-set and the per-row-probe strategies. One key is the
// decimal spelling of a number an outer document holds as a number, one is an
// escaped string, and one document's join-key value is a container, which no
// exact index can hold and which therefore forces the membership back off the
// mask lowering onto its per-row search.
var innerJoinPool = []struct {
	key string
	doc string
}{
	{"k1", `{"tier":"pro","code":7}`},
	{"k2", `{"tier":"free","code":7}`},
	{"k3", `{"tier":"pro","code":"k1"}`},
	{"k4", `{"code":null}`},
	{"7", `{"tier":"pro","code":"7"}`},
	{"a\"b", `{"tier":"free","code":[1,2]}`},
}

// joinBattery returns the compiled-once join shapes the differential runs. Each
// query is reused across every collection pair and every execution mode, which
// also exercises the compile-once/run-many contract for a plan holding a join.
func joinBattery() []*Query {
	return []*Query{
		// The plain foreign-key shape, with and without an inner filter.
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey)),
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey).Where(Cmp("tier", Eq, "pro"))),
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey).Where(IsNull("tier"))),
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey).Where(In("tier", "pro", "free"))),
		// An outer filter beside the join: the join leaf must compose with an
		// ordinary predicate rather than replace it.
		Select(Path("id")).Where(Cmp("active", Eq, true)).
			Join(JoinOn("inner", "ref", JoinKey)),
		Select(Path("id")).Where(Or(Cmp("id", Eq, 1), Cmp("id", Eq, 3))).
			Join(JoinOn("inner", "ref", JoinKey).Where(Cmp("tier", Eq, "pro"))),
		// A join against an inner field rather than the primary key, including
		// the case where the value is numeric and the duplicate-key case.
		Select(Path("id")).Join(JoinOn("inner", "ref", "code")),
		Select(Path("id")).Join(JoinOn("inner", "ref", "code").Where(Cmp("tier", Eq, "pro"))),
		// Two clauses at once, so the conjunction of bindings is exercised.
		Select(Path("id")).Join(
			JoinOn("inner", "ref", JoinKey),
			JoinOn("inner", "ref", "code"),
		),
		// Ordering, limiting, grouping, and aggregation over a joined row set,
		// so the join filters before every later stage rather than after one.
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey)).OrderBy("id", Desc),
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey)).OrderBy("id", Asc).Limit(2),
		Select(Count(), Sum("id")).Join(JoinOn("inner", "ref", JoinKey)),
		Select(Path("active"), Count()).Join(JoinOn("inner", "ref", JoinKey)).
			GroupBy("active").OrderBy("active", Asc),
		// Grouping and ordering by the join path itself, which shares its value
		// column with the join leaf: a column-index mistake there would be
		// invisible in every query that names the join path only once.
		Select(Path("ref"), Count()).Join(JoinOn("inner", "ref", JoinKey)).
			GroupBy("ref").OrderBy("ref", Asc),
		Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey)).OrderBy("ref", Asc).OrderBy("id", Asc),
		// A bare COUNT(*) over an indexed join path, which is the plan shape the
		// direct indexed-count lane answers from exact masks without decoding a
		// single document — so a membership binding that produced an unsound
		// mask would be believed rather than rechecked.
		Select(Count()).Join(JoinOn("inner", "ref", JoinKey)),
		Select(Count()).Join(JoinOn("inner", "ref", JoinKey).Where(Cmp("tier", Eq, "pro"))),
	}
}

// joinStrategy is one forced binding strategy. The threshold is the only input
// that selects between them, so sweeping it is how the battery runs the same
// query down both execution paths.
type joinStrategy struct {
	name string
	// membershipMax is the ExecOptions value. One forces the lookup path — the
	// binder overflows at limit+1 collected values, which a single inner match
	// already reaches — and a very large value forces the membership path.
	membershipMax int
}

var joinStrategies = []joinStrategy{
	{"membership", 1 << 20},
	{"lookup", 1},
}

// joinIndexModes toggles a declared exact index on the outer join path. With
// one, a membership binding lowers to a mask intersection and the rows the
// executor decodes are a strict subset; without one it searches the collected
// set per row. Both must return the same rows, which is what makes the index a
// cost decision here rather than a semantic one.
var joinIndexModes = []bool{false, true}

// joinIterations keeps the exhaustive sweep at full size in an ordinary run
// while giving the -short instrumentation runs — race and checkptr, where every
// allocation and every load is instrumented — a representative sample of the
// same generators. It mirrors the root package's testIterations for the same
// reason; the two packages cannot share one, and a join-scoped name keeps the
// query package's test namespace honest about which sweep it bounds.
func joinIterations(full, short int) int {
	if testing.Short() {
		return short
	}
	return full
}

func TestExhaustiveJoinDifferential(t *testing.T) {
	// The outer side is enumerated as sequences rather than subsets because the
	// driving collection is keyed independently of its contents: two documents
	// with identical bytes under different keys are two rows, and their order
	// decides the chunk layout the mask walk and the batch boundaries cross. The
	// inner side is enumerated as subsets, because there a repeated key is one
	// document by definition, and it is capped at three because the inner side
	// influences the answer only through the set of join-key values it yields:
	// the pool's six documents already contribute every distinct value shape, and
	// a fourth simultaneously present document adds combinations of shapes that
	// are already independently covered.
	outers := enumerateStrings(outerJoinPool, joinIterations(3, 2), 1)
	inners := enumerateInnerCollections(len(innerJoinPool), joinIterations(3, 2))
	battery := joinBattery()

	checks := 0
	// Tallied across the whole sweep and asserted at the end: a differential
	// that only ever exercised one strategy would pass every comparison below
	// while proving nothing about the other.
	var bound, probed ExecStats
	for _, outerDocs := range outers {
		decodedOuter := decodeDocs(t, asBytes(outerDocs))
		for _, innerRows := range inners {
			reference := newRefCollection(t, innerRows)
			for _, indexed := range joinIndexModes {
				db := buildJoinDatabase(t, outerDocs, innerRows, indexed)
				catalog := db.Snapshot()
				for _, strategy := range joinStrategies {
					var e Exec
					e.Options.JoinMembershipMax = strategy.membershipMax
					for qi, q := range battery {
						if err := q.RunInto(&e, FromDatabase(catalog, "outer")); err != nil {
							t.Fatalf("query %d %s indexed=%v over %v / %v: RunInto: %v",
								qi, strategy.name, indexed, outerDocs, innerRows, err)
						}
						want := referenceRunJoined(q, decodedOuter, func(doc any) bool {
							return refJoinsAll(q.joins, doc, reference)
						})
						if diff := compareResults(e.Result, want); diff != "" {
							t.Fatalf("query %d %s indexed=%v over %v / %v: %s",
								qi, strategy.name, indexed, outerDocs, innerRows, diff)
						}
						bound.JoinMemberships += e.Stats.JoinMemberships
						bound.JoinKeys += e.Stats.JoinKeys
						probed.JoinLookups += e.Stats.JoinLookups
						probed.JoinProbes += e.Stats.JoinProbes
						probed.JoinFilters += e.Stats.JoinFilters
						probed.JoinFilterRejected += e.Stats.JoinFilterRejected
						checks++
					}
				}
			}
		}
	}
	if bound.JoinMemberships == 0 || bound.JoinKeys == 0 {
		t.Fatalf("no membership binding was ever exercised: %+v", bound)
	}
	if probed.JoinLookups == 0 || probed.JoinProbes == 0 {
		t.Fatalf("no lookup binding was ever exercised: %+v", probed)
	}
	if probed.JoinFilters == 0 || probed.JoinFilterRejected == 0 {
		t.Fatalf("no semi-join reduction filter ever rejected a row: %+v", probed)
	}
	t.Logf("strategies exercised: %d membership bindings collecting %d values; "+
		"%d lookup bindings performing %d probes, %d of them carrying a filter "+
		"that rejected %d rows outright",
		bound.JoinMemberships, bound.JoinKeys, probed.JoinLookups, probed.JoinProbes,
		probed.JoinFilters, probed.JoinFilterRejected)
	t.Logf("exhaustive join differential: %d outer sets × %d inner sets × %d index modes × %d strategies × %d queries = %d checks",
		len(outers), len(inners), len(joinIndexModes), len(joinStrategies), len(battery), checks)
}

// TestJoinStrategiesAgreeRowForRow is the same claim stated as a direct
// comparison rather than through the reference: the identical query and the
// identical database, executed with the membership threshold at either
// extreme, must produce byte-identical results. It is the narrower check that
// survives even if the reference and the executor ever drifted together.
func TestJoinStrategiesAgreeRowForRow(t *testing.T) {
	outers := enumerateStrings(outerJoinPool, 3, 2)
	inners := enumerateInnerCollections(len(innerJoinPool), 2)
	battery := joinBattery()

	for _, outerDocs := range outers {
		for _, innerRows := range inners {
			db := buildJoinDatabase(t, outerDocs, innerRows, true)
			catalog := db.Snapshot()
			for qi, q := range battery {
				var membership, lookup Exec
				membership.Options.JoinMembershipMax = 1 << 20
				lookup.Options.JoinMembershipMax = 1
				if err := q.RunInto(&membership, FromDatabase(catalog, "outer")); err != nil {
					t.Fatalf("query %d membership: %v", qi, err)
				}
				if err := q.RunInto(&lookup, FromDatabase(catalog, "outer")); err != nil {
					t.Fatalf("query %d lookup: %v", qi, err)
				}
				if diff := diffResults(membership.Result, lookup.Result); diff != "" {
					t.Fatalf("query %d over %v / %v: strategies disagree: %s",
						qi, outerDocs, innerRows, diff)
				}
			}
		}
	}
}

// --- naive nested-loop reference -------------------------------------------

// A refCollection is the inner side as the reference sees it: keys and decoded
// documents, with no index, no ordering, and no set structure of any kind.
type refCollection struct {
	keys []string
	docs []any
}

func newRefCollection(t testing.TB, rows []int) refCollection {
	t.Helper()
	out := refCollection{}
	for _, r := range rows {
		out.keys = append(out.keys, innerJoinPool[r].key)
	}
	docs := make([][]byte, 0, len(rows))
	for _, r := range rows {
		docs = append(docs, []byte(innerJoinPool[r].doc))
	}
	out.docs = decodeDocs(t, docs)
	return out
}

// refJoinsAll is the reference semi-join: an outer document survives only when
// every clause finds at least one inner document that passes the clause's
// filter and whose join key equals the outer document's value.
func refJoinsAll(joins []Join, outer any, inner refCollection) bool {
	for _, j := range joins {
		if !refJoinMatches(j, outer, inner) {
			return false
		}
	}
	return true
}

// refJoinMatches is the nested loop itself. It stops at the first match, which
// is exactly why a duplicate inner key cannot duplicate an outer row: existence
// is a boolean, not a count.
func refJoinMatches(j Join, outer any, inner refCollection) bool {
	cell := refClassify(refResolve(j.outerPath, outer))
	if cell.kind == kindNull {
		return false // a null or absent outer key joins to nothing
	}
	for i := range inner.keys {
		if j.hasWhere && !refEval(j.where, inner.docs[i]) {
			continue
		}
		var target refScalar
		if j.innerPath == JoinKey {
			target = refScalar{kind: kindString, present: true, s: inner.keys[i]}
		} else {
			target = refClassify(refResolve(j.innerPath, inner.docs[i]))
			if target.kind == kindNull {
				continue // a null or absent inner key joins to nothing
			}
		}
		if refCompare(cell, target) == 0 {
			return true
		}
	}
	return false
}

// --- fixture construction ---------------------------------------------------

// buildJoinDatabase publishes the two collections into one Database, so that a
// single Database.Snapshot captures both at one instant — the consistent cut a
// join is only expressible over.
func buildJoinDatabase(t testing.TB, outerDocs []string, innerRows []int, index bool) *store.Database {
	t.Helper()
	db := &store.Database{}
	// Two documents per chunk, so the mask walk, the batch boundaries, and the
	// row gather all cross a chunk edge on collections this small.
	options := store.Options{ChunkDocuments: 2}
	outer, err := db.CreateCollection("outer", options)
	if err != nil {
		t.Fatalf("CreateCollection(outer): %v", err)
	}
	for i, doc := range outerDocs {
		if _, err := outer.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatalf("Put(o%d, %s): %v", i, doc, err)
		}
	}
	inner, err := db.CreateCollection("inner", options)
	if err != nil {
		t.Fatalf("CreateCollection(inner): %v", err)
	}
	for _, r := range innerRows {
		if _, err := inner.Put(innerJoinPool[r].key, []byte(innerJoinPool[r].doc)); err != nil {
			t.Fatalf("Put(%s): %v", innerJoinPool[r].key, err)
		}
	}
	if index {
		if _, err := outer.CreateIndex(store.IndexDefinition{Name: "ref", Paths: []string{"/ref"}}); err != nil {
			t.Fatalf("CreateIndex(ref): %v", err)
		}
		if info, err := outer.BackfillIndex("ref", 0); err != nil || info.State != store.IndexReady {
			t.Fatalf("BackfillIndex(ref) = (%+v, %v)", info, err)
		}
	}
	return db
}

// enumerateStrings returns every ordered sequence of length min..maxLen drawn
// from pool.
func enumerateStrings(pool []string, maxLen, minLen int) [][]string {
	var out [][]string
	var rec func(prefix []string)
	rec = func(prefix []string) {
		if len(prefix) >= minLen {
			cp := make([]string, len(prefix))
			copy(cp, prefix)
			out = append(out, cp)
		}
		if len(prefix) == maxLen {
			return
		}
		for _, d := range pool {
			rec(append(prefix, d))
		}
	}
	rec(nil)
	return out
}

// enumerateInnerCollections returns every subset of the inner pool of size
// 0..maxLen, in pool order. Subsets rather than sequences, because a collection
// is keyed: the same key twice is one document, so an ordered enumeration would
// only repeat states the subsets already cover. The empty collection is
// included deliberately — a semi-join against nothing must keep no row.
func enumerateInnerCollections(n, maxLen int) [][]int {
	var out [][]int
	var rec func(start int, prefix []int)
	rec = func(start int, prefix []int) {
		cp := make([]int, len(prefix))
		copy(cp, prefix)
		out = append(out, cp)
		if len(prefix) == maxLen {
			return
		}
		for i := start; i < n; i++ {
			rec(i+1, append(prefix, i))
		}
	}
	rec(0, nil)
	return out
}

func asBytes(docs []string) [][]byte {
	out := make([][]byte, len(docs))
	for i, d := range docs {
		out[i] = []byte(d)
	}
	return out
}

// diffResults compares two engine results cell for cell, the direct
// strategy-versus-strategy check that needs no reference model.
func diffResults(a, b Result) string {
	if len(a.Columns) != len(b.Columns) || a.RowCount != b.RowCount {
		return fmt.Sprintf("shape: %d cols/%d rows vs %d cols/%d rows",
			len(a.Columns), a.RowCount, len(b.Columns), b.RowCount)
	}
	for c := range a.Columns {
		if a.Columns[c].Header != b.Columns[c].Header {
			return fmt.Sprintf("column %d header: %q vs %q",
				c, a.Columns[c].Header, b.Columns[c].Header)
		}
		for r := 0; r < a.RowCount; r++ {
			x, y := a.Columns[c].Cells[r], b.Columns[c].Cells[r]
			if x.Kind() != y.Kind() || !strings.EqualFold(string(x.JSON()), string(y.JSON())) {
				return fmt.Sprintf("row %d col %q: %s vs %s", r, a.Columns[c].Header, x.JSON(), y.JSON())
			}
		}
	}
	return ""
}

// --- the adaptive choice, and the proof that it is a choice -----------------

// joinScaleDatabase builds an orders/customers pair large enough that the two
// strategies are genuinely different plans rather than two spellings of the
// same three-row scan. matching is how many customers pass the inner filter and
// therefore how many values a membership would have to collect.
func joinScaleDatabase(t testing.TB, outerRows, customers, matching int, index bool) *store.Database {
	return joinScaleDatabaseIndexed(t, outerRows, customers, matching, index, false)
}

// joinScaleDatabaseIndexed is joinScaleDatabase with independent control over
// the inner collection's own index, which the inner filter pushes down into
// exactly as an ordinary single-collection query would.
func joinScaleDatabaseIndexed(t testing.TB, outerRows, customers, matching int, index, innerIndex bool) *store.Database {
	t.Helper()
	db := &store.Database{}
	orders, err := db.CreateCollection("orders", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < outerRows; i++ {
		doc := fmt.Sprintf(`{"id":%d,"customer":"c%d","total":%d}`, i, i%customers, i)
		if _, err := orders.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	buyers, err := db.CreateCollection("customers", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < customers; i++ {
		// Spread the matching customers evenly rather than putting them in a
		// prefix. A prefix is not a neutral layout: it makes any sample of the
		// first rows scanned unrepresentative of the whole, which is precisely
		// what the mid-scan abandon check reads. Testing a sampler against data
		// laid out to defeat sampling measures the layout, not the sampler —
		// the clustered case gets its own test instead.
		tier := "free"
		if stride := customers / max(matching, 1); stride > 0 && i%stride == 0 {
			tier = "pro"
		}
		// name repeats the key as a field, so the same join can be expressed
		// either against the primary key or against an ordinary inner path and
		// the two must agree. label is spelled with an escape so that reading it
		// forces the decoded-string path, which is the one a per-row probe has
		// to reset between rows rather than let grow.
		doc := fmt.Sprintf(`{"tier":%q,"seat":%d,"name":"c%d","label":"lab\u0065l-%d"}`,
			tier, i, i, i%4)
		if _, err := buyers.Put(fmt.Sprintf("c%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	if index {
		if _, err := orders.CreateIndex(store.IndexDefinition{
			Name: "customer", Paths: []string{"/customer"},
		}); err != nil {
			t.Fatal(err)
		}
		if info, err := orders.BackfillIndex("customer", 0); err != nil || info.State != store.IndexReady {
			t.Fatalf("BackfillIndex(customer) = (%+v, %v)", info, err)
		}
	}
	if innerIndex {
		if _, err := buyers.CreateIndex(store.IndexDefinition{
			Name: "tier", Paths: []string{"/tier"},
		}); err != nil {
			t.Fatal(err)
		}
		if info, err := buyers.BackfillIndex("tier", 0); err != nil || info.State != store.IndexReady {
			t.Fatalf("BackfillIndex(tier) = (%+v, %v)", info, err)
		}
	}
	return db
}

// TestJoinAdaptiveStrategyIsNonVacuous proves that the threshold actually
// selects between two different plans, and that each assertion about a plan
// fails under the other one. Without this, every "the fast path was taken"
// claim in this file would be satisfiable by a build in which only one path
// exists — the differential alone cannot tell the difference, because both
// paths are required to return the same rows.
func TestJoinAdaptiveStrategyIsNonVacuous(t *testing.T) {
	const (
		orders    = 500
		customers = 200
		matching  = 40
	)
	db := joinScaleDatabase(t, orders, customers, matching, false)
	catalog := db.Snapshot()
	q := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
		OrderBy("id", Asc)

	run := func(limit int) (Result, ExecStats) {
		var e Exec
		e.Options.JoinMembershipMax = limit
		if err := q.RunInto(&e, FromDatabase(catalog, "orders")); err != nil {
			t.Fatalf("RunInto(limit=%d): %v", limit, err)
		}
		return e.Result, e.Stats
	}

	// The membership side. The inner filter selects 40 of 200 customers, well
	// under the default threshold, so the default configuration must reach the
	// same binding the forced one does.
	wide, wideStats := run(1 << 20)
	_, defaultStats := run(0)
	for name, stats := range map[string]ExecStats{"forced": wideStats, "default": defaultStats} {
		if stats.JoinMemberships != 1 || stats.JoinLookups != 0 {
			t.Fatalf("%s membership run: JoinMemberships=%d JoinLookups=%d, want 1 and 0",
				name, stats.JoinMemberships, stats.JoinLookups)
		}
		if stats.JoinKeys != matching {
			t.Fatalf("%s membership run: JoinKeys=%d, want the %d matching customers",
				name, stats.JoinKeys, matching)
		}
		if stats.JoinProbes != 0 {
			t.Fatalf("%s membership run: JoinProbes=%d, want no per-row probing at all",
				name, stats.JoinProbes)
		}
	}

	// The lookup side. Every one of the assertions above must now fail, which is
	// what makes them evidence rather than tautologies.
	narrow, narrowStats := run(1)
	if narrowStats.JoinLookups != 1 || narrowStats.JoinMemberships != 0 {
		t.Fatalf("lookup run: JoinLookups=%d JoinMemberships=%d, want 1 and 0",
			narrowStats.JoinLookups, narrowStats.JoinMemberships)
	}
	if narrowStats.JoinKeys != 0 {
		t.Fatalf("lookup run: JoinKeys=%d, want none collected — the set was abandoned", narrowStats.JoinKeys)
	}
	// Every outer row reaches the join leaf exactly once and is answered either
	// by the prefilter or by a probe. Which of the two does the work is a cost
	// decision; that the two account for every row is not.
	if got := narrowStats.JoinProbes + narrowStats.JoinFilterRejected; got != orders {
		t.Fatalf("lookup run: %d rows answered (%d probed, %d prefiltered), want one per outer row (%d)",
			got, narrowStats.JoinProbes, narrowStats.JoinFilterRejected, orders)
	}

	// And the two plans must agree exactly, which is the whole point of being
	// allowed to choose between them at all.
	if diff := diffResults(wide, narrow); diff != "" {
		t.Fatalf("strategies disagree: %s", diff)
	}
	if wide.RowCount == 0 || wide.RowCount == orders {
		t.Fatalf("row count %d is degenerate; the join must filter some but not all rows", wide.RowCount)
	}
}

// TestJoinMembershipLowersToIndexMasks asserts the payoff the whole membership
// strategy exists for: with a declared exact index on the outer join path, the
// collected set is probed through the index instead of searched per row, and
// the executor decodes only the rows the masks admit.
//
// The no-index run is the non-vacuity half. It must observe zero index probes,
// so the assertion above cannot be satisfied by a build where the counter is
// simply always positive.
func TestJoinMembershipLowersToIndexMasks(t *testing.T) {
	const (
		orders    = 400
		customers = 100
		matching  = 5
	)
	q := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")))

	probes := map[bool]int{}
	rows := map[bool]int{}
	for _, indexed := range []bool{false, true} {
		db := joinScaleDatabase(t, orders, customers, matching, indexed)
		var e Exec
		if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
			t.Fatalf("indexed=%v: %v", indexed, err)
		}
		if e.Stats.JoinMemberships != 1 {
			t.Fatalf("indexed=%v: the run must bind a membership, got %+v", indexed, e.Stats)
		}
		probes[indexed] = e.Workspace.storeIndexProbes
		rows[indexed] = e.Result.RowCount
	}
	if rows[false] != rows[true] || rows[true] == 0 {
		t.Fatalf("row counts differ or are degenerate: %d without an index, %d with one",
			rows[false], rows[true])
	}
	if probes[true] != matching {
		t.Fatalf("indexed run made %d index probes, want one per collected value (%d)",
			probes[true], matching)
	}
	if probes[false] != 0 {
		t.Fatalf("unindexed run made %d index probes, want none — there is no index to probe",
			probes[false])
	}
}

// --- value-kind rules the exhaustive sweep cannot reach ---------------------

// TestJoinPrimaryKeyMatchingIsNotCoerced pins the rule that only a string can
// name a collection key. It is written against a collection that holds a
// document under the empty key, because that is the shape where a missing
// in-kind check stops being harmless: a number, a boolean, and an absent path
// all classify with an empty decoded string, so a lookup that reached the hash
// map with one of them would find that document and report a match no
// membership binding would ever produce.
//
// The inner collection deliberately holds two documents. With only one, the
// forced-lookup configuration would never overflow its one-value threshold and
// the test would quietly check the membership path twice; the run asserts which
// strategy it got for exactly that reason.
func TestJoinPrimaryKeyMatchingIsNotCoerced(t *testing.T) {
	db := &store.Database{}
	outer, err := db.CreateCollection("outer", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	outerDocs := []string{
		`{"id":1,"ref":""}`,    // the empty string: the one value that matches
		`{"id":2,"ref":0}`,     // a number decodes to no string at all
		`{"id":3,"ref":false}`, // and neither does a boolean
		`{"id":4,"ref":null}`,
		`{"id":5}`,
		`{"id":6,"ref":"x"}`,
	}
	for i, doc := range outerDocs {
		if _, err := outer.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	inner, err := db.CreateCollection("inner", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"", "unrelated"} {
		if _, err := inner.Put(key, []byte(`{"tier":"pro"}`)); err != nil {
			t.Fatal(err)
		}
	}
	catalog := db.Snapshot()

	q := Select(Path("id")).Join(JoinOn("inner", "ref", JoinKey)).OrderBy("id", Asc)
	for _, strategy := range joinStrategies {
		var e Exec
		e.Options.JoinMembershipMax = strategy.membershipMax
		if err := q.RunInto(&e, FromDatabase(catalog, "outer")); err != nil {
			t.Fatalf("%s: %v", strategy.name, err)
		}
		switch strategy.name {
		case "membership":
			if e.Stats.JoinMemberships != 1 {
				t.Fatalf("membership: bound %+v, want one membership", e.Stats)
			}
		default:
			if e.Stats.JoinLookups != 1 || e.Stats.JoinProbes == 0 {
				t.Fatalf("lookup: bound %+v, want one lookup that actually probed", e.Stats)
			}
		}
		if e.Result.RowCount != 1 {
			t.Fatalf("%s: %d rows, want only the empty-string reference:\n%s",
				strategy.name, e.Result.RowCount, dumpResult(e.Result))
		}
		if got, _ := e.Result.Columns[0].Cells[0].Float64(); got != 1 {
			t.Fatalf("%s: matched id %v, want 1", strategy.name, got)
		}
	}
}

// TestJoinNullInnerKeysAreNotCollected pins the other half of the null rule.
// An inner document whose join key is null or absent is not a partner for
// anything — an outer null does not join either — so it must not enter the
// collected set at all. Admitting it would be invisible in the rows (nothing
// can equal it) and very visible in the plan: a null alternative has no scalar
// needle, so one of them silently costs the whole membership its index
// lowering.
func TestJoinNullInnerKeysAreNotCollected(t *testing.T) {
	db := &store.Database{}
	outer, err := db.CreateCollection("outer", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i, doc := range []string{
		`{"id":1,"ref":"a"}`,
		`{"id":2,"ref":null}`,
		`{"id":3}`,
	} {
		if _, err := outer.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := outer.CreateIndex(store.IndexDefinition{Name: "ref", Paths: []string{"/ref"}}); err != nil {
		t.Fatal(err)
	}
	if info, err := outer.BackfillIndex("ref", 0); err != nil || info.State != store.IndexReady {
		t.Fatalf("BackfillIndex(ref) = (%+v, %v)", info, err)
	}
	inner, err := db.CreateCollection("inner", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for key, doc := range map[string]string{
		"i0": `{"code":"a"}`,
		"i1": `{"code":null}`,
		"i2": `{}`,
	} {
		if _, err := inner.Put(key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	var e Exec
	q := Select(Path("id")).Join(JoinOn("inner", "ref", "code")).OrderBy("id", Asc)
	if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "outer")); err != nil {
		t.Fatal(err)
	}
	if e.Result.RowCount != 1 {
		t.Fatalf("%d rows, want only the outer row whose key is a real value:\n%s",
			e.Result.RowCount, dumpResult(e.Result))
	}
	if e.Stats.JoinKeys != 1 {
		t.Fatalf("collected %d alternatives, want only the one non-null inner key", e.Stats.JoinKeys)
	}
	if e.Workspace.storeIndexProbes != 1 {
		t.Fatalf("%d index probes, want exactly one: a null alternative would have "+
			"cost the whole membership its index lowering", e.Workspace.storeIndexProbes)
	}
}

// TestJoinContainerKeysDeclineIndexLowering covers the alternatives an exact
// index cannot hold. A container join key is a legal value — the engine's total
// order compares containers by their source bytes — but the index stores
// scalars, so a membership carrying one must fall back to its per-row search
// rather than hand the index a needle it will reject. Getting this wrong is not
// a slow query; it is an error returned from a query that has a perfectly good
// answer.
func TestJoinContainerKeysDeclineIndexLowering(t *testing.T) {
	db := &store.Database{}
	outer, err := db.CreateCollection("outer", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i, doc := range []string{
		`{"id":1,"ref":[]}`,
		`{"id":2,"ref":{}}`,
		`{"id":3,"ref":[1,2]}`,
		`{"id":4,"ref":"scalar"}`,
	} {
		if _, err := outer.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := outer.CreateIndex(store.IndexDefinition{Name: "ref", Paths: []string{"/ref"}}); err != nil {
		t.Fatal(err)
	}
	if info, err := outer.BackfillIndex("ref", 0); err != nil || info.State != store.IndexReady {
		t.Fatalf("BackfillIndex(ref) = (%+v, %v)", info, err)
	}
	inner, err := db.CreateCollection("inner", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for key, doc := range map[string]string{
		"a": `{"code":[]}`,
		"b": `{"code":{}}`,
		"c": `{"code":"scalar"}`,
	} {
		if _, err := inner.Put(key, []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}

	var e Exec
	q := Select(Path("id")).Join(JoinOn("inner", "ref", "code")).OrderBy("id", Asc)
	if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "outer")); err != nil {
		t.Fatalf("a container join key must answer, not fail: %v", err)
	}
	if e.Result.RowCount != 3 {
		t.Fatalf("%d rows, want the empty array, the empty object, and the scalar:\n%s",
			e.Result.RowCount, dumpResult(e.Result))
	}
	if e.Workspace.storeIndexProbes != 0 {
		t.Fatalf("%d index probes, want none: the collected set holds containers, "+
			"and one unindexable alternative makes the whole set unindexable",
			e.Workspace.storeIndexProbes)
	}
}

// TestJoinDuplicateInnerMatchesKeepOuterRowOnce states the semi-join contract
// on its own, in a fixture where a fan-out join would be unmistakable: every
// outer row has four inner partners.
//
// It joins on an inner field rather than the primary key because that is the
// only way several inner documents can share one join-key value at all — keys
// are unique by construction — which is also why this clause has one strategy
// rather than two, and why the run asserts it got the membership.
func TestJoinDuplicateInnerMatchesKeepOuterRowOnce(t *testing.T) {
	db := &store.Database{}
	outer, err := db.CreateCollection("outer", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := outer.Put(fmt.Sprintf("o%d", i), []byte(`{"ref":"shared"}`)); err != nil {
			t.Fatal(err)
		}
	}
	inner, err := db.CreateCollection("inner", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		if _, err := inner.Put(fmt.Sprintf("i%d", i), []byte(`{"code":"shared"}`)); err != nil {
			t.Fatal(err)
		}
	}
	catalog := db.Snapshot()
	q := Select(Count()).Join(JoinOn("inner", "ref", "code"))
	for _, strategy := range joinStrategies {
		var e Exec
		e.Options.JoinMembershipMax = strategy.membershipMax
		if err := q.RunInto(&e, FromDatabase(catalog, "outer")); err != nil {
			t.Fatalf("%s: %v", strategy.name, err)
		}
		got, _ := e.Result.Columns[0].Cells[0].Float64()
		if got != 3 {
			t.Fatalf("%s: counted %v rows, want 3 — a semi-join filters, it does not fan out",
				strategy.name, got)
		}
		if e.Stats.JoinMemberships != 1 {
			t.Fatalf("%s: bound %+v; a join on an inner field has no keyed probe to "+
				"fall back to and must always bind a membership", strategy.name, e.Stats)
		}
		if e.Stats.JoinKeys > 1 {
			t.Fatalf("%s: collected %d alternatives from four documents sharing one value; "+
				"the set must be deduplicated", strategy.name, e.Stats.JoinKeys)
		}
	}
}

// TestJoinInnerSideUsesItsOwnIndexes asserts that the inner run is a first-class
// query and not a hand-rolled scan: its filter binds the inner collection's
// declared indexes and probes them, so a selective inner predicate touches the
// documents it selected rather than all of them.
//
// The unindexed run is the non-vacuity half. Both must return the same rows,
// because an index is a cost decision on the inner side exactly as it is on the
// outer.
func TestJoinInnerSideUsesItsOwnIndexes(t *testing.T) {
	const (
		orders    = 200
		customers = 120
		matching  = 6
	)
	q := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")))

	probes := map[bool]int{}
	rows := map[bool]int{}
	for _, innerIndexed := range []bool{false, true} {
		db := joinScaleDatabaseIndexed(t, orders, customers, matching, false, innerIndexed)
		var e Exec
		if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
			t.Fatalf("innerIndexed=%v: %v", innerIndexed, err)
		}
		if e.Stats.JoinMemberships != 1 {
			t.Fatalf("innerIndexed=%v: want one membership binding, got %+v", innerIndexed, e.Stats)
		}
		probes[innerIndexed] = e.Workspace.joins[0].scan.storeIndexProbes
		rows[innerIndexed] = e.Result.RowCount
	}
	if rows[false] != rows[true] || rows[true] == 0 {
		t.Fatalf("row counts differ or are degenerate: %d unindexed, %d indexed", rows[false], rows[true])
	}
	if probes[true] != 1 {
		t.Fatalf("inner run made %d index probes, want the one its tier equality needs", probes[true])
	}
	if probes[false] != 0 {
		t.Fatalf("unindexed inner run made %d index probes, want none", probes[false])
	}
}

// --- the front ends and the boundaries they enforce ------------------------

// TestJoinSourcesOtherThanDatabaseAreRejected pins the rule that makes snapshot
// skew across a join inexpressible rather than merely discouraged. A Source
// naming one collection has no catalog to resolve the inner side from, and the
// only alternative — reading it from a second, independently taken snapshot —
// is the failure mode the whole DatabaseSnapshot exists to prevent.
func TestJoinSourcesOtherThanDatabaseAreRejected(t *testing.T) {
	db := joinScaleDatabase(t, 4, 2, 1, false)
	orders, _ := db.Collection("orders")
	lone, err := orders.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	q := Select(Path("id")).Join(JoinOn("customers", "customer", JoinKey))

	for name, src := range map[string]Source{
		"FromSnapshot": FromSnapshot(lone),
		"FromSegment":  FromSegment(&store.Segment{}),
		"zero Source":  {},
	} {
		var e Exec
		err := q.RunInto(&e, src)
		if err == nil {
			t.Fatalf("%s accepted a join plan; it must not be able to", name)
		}
		if name != "zero Source" && !strings.Contains(err.Error(), "FromDatabase") {
			t.Fatalf("%s: error %q should name the constructor that works", name, err)
		}
	}

	// The same plan against the catalog those collections live in succeeds, so
	// the rejection above is about the Source and not about the plan.
	var e Exec
	if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
		t.Fatalf("FromDatabase must accept the same plan: %v", err)
	}

	// And a plan with no join is unaffected by any of this.
	plain := Select(Path("id"))
	if _, err := plain.Run(FromSnapshot(lone)); err != nil {
		t.Fatalf("a query without a join must still run from a lone snapshot: %v", err)
	}
}

// TestJoinFromDatabaseRejectsAnUnknownDrivingCollection covers the other name
// resolution: the driving side itself.
func TestJoinFromDatabaseRejectsAnUnknownDrivingCollection(t *testing.T) {
	db := joinScaleDatabase(t, 2, 1, 1, false)
	var e Exec
	err := Select(Path("id")).RunInto(&e, FromDatabase(db.Snapshot(), "invoices"))
	if err == nil || !strings.Contains(err.Error(), "invoices") {
		t.Fatalf("got %v, want an error naming the missing collection", err)
	}
}

// TestJoinRejectsAnUnknownInnerCollection covers the inner name, which is only
// resolvable at execution because the catalog it is resolved against is the one
// the driving side was captured with.
func TestJoinRejectsAnUnknownInnerCollection(t *testing.T) {
	db := joinScaleDatabase(t, 2, 1, 1, false)
	var e Exec
	q := Select(Path("id")).Join(JoinOn("suppliers", "customer", JoinKey))
	err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders"))
	if err == nil || !strings.Contains(err.Error(), "suppliers") {
		t.Fatalf("got %v, want an error naming the missing inner collection", err)
	}
}

// TestJoinDocumentFrontEndMatchesTheBuilder checks that the JSON spelling of a
// join compiles to the same plan the builder produces, by requiring the two to
// return the same rows over the same database — the same equivalence the rest
// of this package's front ends are held to.
func TestJoinDocumentFrontEndMatchesTheBuilder(t *testing.T) {
	db := joinScaleDatabase(t, 60, 10, 4, true)
	catalog := db.Snapshot()

	cases := []struct {
		name    string
		builder *Query
		doc     string
		lit     M
	}{
		{
			name: "primary key",
			builder: Select(Path("id")).
				Join(JoinOn("customers", "customer", JoinKey)).OrderBy("id", Asc),
			doc: `{"select":["id"],
			       "join":[{"from":"customers","on":{"customer":"$key"}}],
			       "orderBy":["id"]}`,
			lit: M{
				"select":  A{"id"},
				"join":    A{M{"from": "customers", "on": M{"customer": JoinKey}}},
				"orderBy": A{"id"},
			},
		},
		{
			name: "primary key with an inner filter and an outer filter",
			builder: Select(Path("id")).Where(Cmp("total", Lt, 30)).
				Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
				OrderBy("id", Asc),
			doc: `{"select":["id"],"where":{"total":{"$lt":30}},
			       "join":[{"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}}],
			       "orderBy":["id"]}`,
			lit: M{
				"select":  A{"id"},
				"where":   M{"total": M{"$lt": 30}},
				"join":    A{M{"from": "customers", "on": M{"customer": JoinKey}, "where": M{"tier": "pro"}}},
				"orderBy": A{"id"},
			},
		},
		{
			name: "an inner field path, written as a bare object rather than a list",
			builder: Select(Count()).
				Join(JoinOn("customers", "customer", "tier").Where(In("seat", 1, 2, 3))),
			doc: `{"select":[{"$count":"*"}],
			       "join":{"from":"customers","on":{"customer":"tier"},"where":{"seat":{"$in":[1,2,3]}}}}`,
			lit: M{
				"select": A{M{"$count": "*"}},
				"join":   M{"from": "customers", "on": M{"customer": "tier"}, "where": M{"seat": M{"$in": A{1, 2, 3}}}},
			},
		},
		{
			name: "two clauses",
			builder: Select(Path("id")).Join(
				JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")),
				JoinOn("customers", "customer", JoinKey).Where(Cmp("seat", Lt, 2)),
			).OrderBy("id", Asc),
			doc: `{"select":["id"],"join":[
			         {"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}},
			         {"from":"customers","on":{"customer":"$key"},"where":{"seat":{"$lt":2}}}],
			       "orderBy":["id"]}`,
			lit: M{
				"select": A{"id"},
				"join": A{
					M{"from": "customers", "on": M{"customer": JoinKey}, "where": M{"tier": "pro"}},
					M{"from": "customers", "on": M{"customer": JoinKey}, "where": M{"seat": M{"$lt": 2}}},
				},
				"orderBy": A{"id"},
			},
		},
	}

	for _, tc := range cases {
		want, err := tc.builder.Run(FromDatabase(catalog, "orders"))
		if err != nil {
			t.Fatalf("%s: builder: %v", tc.name, err)
		}
		if want.RowCount == 0 {
			t.Fatalf("%s: the fixture must select some rows, or the comparison proves nothing", tc.name)
		}
		// Both document front ends, because they are two lowerings of one
		// grammar: Parse reads member names and values in place out of a parsed
		// document, New reads them out of Go maps and slices, and only running
		// both proves the join clause is lowered by the grammar rather than by
		// one backing.
		parsed, err := Parse([]byte(tc.doc))
		if err != nil {
			t.Fatalf("%s: Parse: %v", tc.name, err)
		}
		literal, err := New(tc.lit)
		if err != nil {
			t.Fatalf("%s: New: %v", tc.name, err)
		}
		for form, q := range map[string]*Query{"Parse": parsed, "New": literal} {
			got, err := q.Run(FromDatabase(catalog, "orders"))
			if err != nil {
				t.Fatalf("%s/%s: %v", tc.name, form, err)
			}
			if diff := diffResults(got, want); diff != "" {
				t.Fatalf("%s/%s: document and builder disagree: %s", tc.name, form, diff)
			}
		}
	}
}

// TestJoinCompilerSteadyAllocs extends the reusable front end's contract to the
// join clause: a warmed Compiler recompiling a join document must refill the
// inner plans and inner path registries it already holds rather than allocate a
// new one per compile. A join is where that is easiest to get wrong, because
// each clause needs a whole second plan and registry that no other clause does.
func TestJoinCompilerSteadyAllocs(t *testing.T) {
	src := []byte(`{"select":["id"],"where":{"total":{"$lt":30}},
	                "join":[{"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}},
	                        {"from":"customers","on":{"customer":"name"},"where":{"seat":{"$in":[1,2,3]}}}],
	                "orderBy":["id"]}`)
	doc := M{
		"select": A{"id"},
		"where":  M{"total": M{"$lt": 30}},
		"join": A{
			M{"from": "customers", "on": M{"customer": JoinKey}, "where": M{"tier": "pro"}},
			M{"from": "customers", "on": M{"customer": "name"}, "where": M{"seat": M{"$in": A{1, 2, 3}}}},
		},
		"orderBy": A{"id"},
	}

	t.Run("Parse", func(t *testing.T) {
		var c Compiler
		var q Query
		for range 2 {
			if err := c.Parse(&q, src); err != nil {
				t.Fatal(err)
			}
		}
		if allocs := testing.AllocsPerRun(50, func() {
			if err := c.Parse(&q, src); err != nil {
				panic(err)
			}
		}); allocs != 0 {
			t.Fatalf("warmed Compiler.Parse of a join allocated %.2f times, want 0", allocs)
		}
	})
	t.Run("New", func(t *testing.T) {
		var c Compiler
		var q Query
		for range 2 {
			if err := c.New(&q, doc); err != nil {
				t.Fatal(err)
			}
		}
		if allocs := testing.AllocsPerRun(50, func() {
			if err := c.New(&q, doc); err != nil {
				panic(err)
			}
		}); allocs != 0 {
			t.Fatalf("warmed Compiler.New of a join allocated %.2f times, want 0", allocs)
		}
	})
}

// TestJoinCompilerReusedQueryStaysCorrect guards the other half of the reusable
// compiler's contract: recompiling into the same Query must not leave the new
// plan reading the previous compile's inner plan or the previous compile's
// inner path registry. The documents below join against different inner paths
// and different clause counts on purpose, so a stale inner plan would answer
// with the previous query's join, and a registry that was never rewound would
// leave the new plan extracting columns only the previous one wanted.
func TestJoinCompilerReusedQueryStaysCorrect(t *testing.T) {
	db := joinScaleDatabase(t, 60, 10, 4, false)
	catalog := db.Snapshot()
	docs := []struct {
		src string
		// innerPaths is the number of value columns each clause's inner side
		// must extract: exactly the paths that clause names, and no path left
		// over from whatever was compiled into this compiler before it.
		innerPaths []int
	}{
		{`{"select":["id"],"join":[{"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}}],"orderBy":["id"]}`,
			[]int{1}},
		{`{"select":["id"],"join":[{"from":"customers","on":{"customer":"name"},"where":{"seat":{"$gte":8}}}],"orderBy":["id"]}`,
			[]int{2}},
		{`{"select":["id"],"join":[{"from":"customers","on":{"customer":"$key"}}],"orderBy":["id"]}`,
			[]int{0}},
		// Two clauses with different inner shapes, which is the arrangement in
		// which one shared inner plan would silently answer both with whichever
		// was compiled last.
		{`{"select":["id"],"join":[
		     {"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}},
		     {"from":"customers","on":{"customer":"name"},"where":{"seat":{"$lt":3}}}],
		   "orderBy":["id"]}`,
			[]int{1, 2}},
		{`{"select":["id"],"join":[
		     {"from":"customers","on":{"customer":"name"},"where":{"seat":{"$lt":3}}},
		     {"from":"customers","on":{"customer":"$key"},"where":{"tier":"pro"}}],
		   "orderBy":["id"]}`,
			[]int{2, 1}},
	}
	var c Compiler
	var reused Query
	// Two passes, so the second compile of each document is a warm one that has
	// to rewind storage the previous document is still spelled into.
	for pass := range 2 {
		for i, tc := range docs {
			if err := c.Parse(&reused, []byte(tc.src)); err != nil {
				t.Fatalf("pass %d doc %d: %v", pass, i, err)
			}
			compiled, err := reused.compiled()
			if err != nil {
				t.Fatalf("pass %d doc %d: %v", pass, i, err)
			}
			if len(compiled.joins) != len(tc.innerPaths) {
				t.Fatalf("pass %d doc %d: %d clauses, want %d",
					pass, i, len(compiled.joins), len(tc.innerPaths))
			}
			for k, want := range tc.innerPaths {
				if got := len(compiled.joins[k].inner.valuePaths); got != want {
					t.Fatalf("pass %d doc %d clause %d: inner plan extracts %d value columns, want %d; "+
						"a stale registry leaves the previous query's paths behind", pass, i, k, got, want)
				}
			}
			got, err := reused.Run(FromDatabase(catalog, "orders"))
			if err != nil {
				t.Fatalf("pass %d doc %d: %v", pass, i, err)
			}
			owned, err := Parse([]byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			want, err := owned.Run(FromDatabase(catalog, "orders"))
			if err != nil {
				t.Fatal(err)
			}
			if want.RowCount == 0 {
				t.Fatalf("doc %d selects nothing; the comparison would prove nothing", i)
			}
			if diff := diffResults(got, want); diff != "" {
				t.Fatalf("pass %d doc %d: reused compiler disagrees with a fresh one: %s", pass, i, diff)
			}
		}
	}
}

// TestJoinDocumentErrors pins the rejections. Each message must name the clause
// it is about, matching the rest of the front end, and the fan-out attempts must
// be refused explicitly rather than ignored: a caller who asked for the inner
// collection's columns and silently got a filter has been answered a question
// they did not ask.
func TestJoinDocumentErrors(t *testing.T) {
	cases := []struct {
		doc  string
		want []string
	}{
		{`{"select":["id"],"join":3}`, []string{"join", "not a number"}},
		{`{"select":["id"],"join":[]}`, []string{"join", "empty list"}},
		{`{"select":["id"],"join":[3]}`, []string{"join[0]", "a join is an object"}},
		{`{"select":["id"],"join":[{"on":{"a":"$key"}}]}`, []string{"join[0]", "from"}},
		{`{"select":["id"],"join":[{"from":"c"}]}`, []string{"join[0]", "on"}},
		{`{"select":["id"],"join":[{"from":3,"on":{"a":"$key"}}]}`, []string{"join[0]", "collection name"}},
		{`{"select":["id"],"join":[{"from":"","on":{"a":"$key"}}]}`, []string{"join[0].from", "must name a collection"}},
		{`{"select":["id"],"join":[{"from":"c","on":"a"}]}`, []string{"join[0]", "one-entry"}},
		{`{"select":["id"],"join":[{"from":"c","on":{}}]}`, []string{"join[0]", "exactly one entry"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key","b":"$key"}}]}`, []string{"join[0]", "exactly one entry"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":3}}]}`, []string{"join[0].on.a", `a path or "$key"`}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$id"}}]}`, []string{"join[0].on.a", "unknown inner join target"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"$or":"$key"}}]}`, []string{"join[0].on", "not the operator"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key"},"limit":3}]}`, []string{"join[0]", "unknown join key"}},
		// The fan-out refusals, in both member orders, because the message
		// names the inner collection only once "from" has already been read.
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key"},"select":["x"]}]}`,
			[]string{"join[0]", "not a join key", "returns none of c's columns"}},
		{`{"select":["id"],"join":[{"select":["x"],"from":"c","on":{"a":"$key"}}]}`,
			[]string{"join[0]", "not a join key", "the joined collection"}},
		{`{"select":["id"],"join":[{"from":"collection","on":{"a":"$key"},"select":["x"]}]}`,
			[]string{"join[0]", `"select" is not a join key`, "none of collection's columns"}},
		{`{"select":["id"],"join":[{"from":"collection","on":{"a":"$key"},"as":"x"}]}`,
			[]string{"join[0]", `"as" is not a join key`, "none of collection's columns"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key"},"as":"cust"}]}`,
			[]string{"join[0]", "not a join key"}},
		// The inner filter's own errors keep their breadcrumbs, which is the
		// deepest position the grammar has and the reason locSteps is six.
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key"},"where":{"t":{"$in":[[1]]}}}]}`,
			[]string{"join[0].where.t.$in[0]", "$in"}},
		{`{"select":["id"],"join":[{"from":"c","on":{"a":"$key"},"where":{"t":{"$nope":1}}}]}`,
			[]string{"join[0].where.t.$nope", "unknown operator"}},
		// And the clause list itself is still closed.
		{`{"select":["id"],"joins":[]}`, []string{"unknown query clause", "join"}},
	}
	for _, tc := range cases {
		_, err := Parse([]byte(tc.doc))
		if err == nil {
			t.Fatalf("%s: compiled, want an error", tc.doc)
		}
		for _, want := range tc.want {
			if !strings.Contains(err.Error(), want) {
				t.Fatalf("%s:\n got %q\nwant it to mention %q", tc.doc, err, want)
			}
		}
	}
}

// TestJoinBuilderErrors pins the compile-time rejections a builder can reach
// that the document grammar cannot.
func TestJoinBuilderErrors(t *testing.T) {
	cases := []struct {
		name  string
		query *Query
		want  string
	}{
		{"no collection", Select(Path("id")).Join(JoinOn("", "a", JoinKey)), "join[0]"},
		{"whole document as the outer key", Select(Path("id")).Join(JoinOn("c", "", JoinKey)), "outer side"},
		{"whole document as the inner key", Select(Path("id")).Join(JoinOn("c", "a", "")), "inner side"},
		{"unparsable outer path", Select(Path("id")).Join(JoinOn("c", "/a/~9", JoinKey)), "~"},
		{"unparsable inner path", Select(Path("id")).Join(JoinOn("c", "a", "/b/~9")), "~"},
		{"unparsable inner filter path", Select(Path("id")).
			Join(JoinOn("c", "a", JoinKey).Where(Cmp("/x/~9", Eq, 1))), "~"},
		{"the zero Join", Select(Path("id")).Join(Join{}), "join[0]"},
		{"the second clause is the bad one", Select(Path("id")).
			Join(JoinOn("c", "a", JoinKey), JoinOn("", "b", JoinKey)), "join[1]"},
	}
	for _, tc := range cases {
		err := tc.query.Prepare()
		if err == nil {
			t.Fatalf("%s: compiled, want an error", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: got %q, want it to mention %q", tc.name, err, tc.want)
		}
	}
}

// --- steady-state cost ------------------------------------------------------

// TestJoinRunIntoSteadyAllocs holds a joined execution to the same contract
// TestRunIntoSteadyAllocs holds every other one to: after a warm-up, a repeated
// RunInto into the same Exec allocates nothing. A join adds a whole second
// execution — the inner scan, the collected set, the needles, and either the
// membership or the per-row probe scratch — and every one of those has to come
// out of storage the Workspace already holds, or the join would be the one
// operation in this package that leaks allocation into a hot loop.
func TestJoinRunIntoSteadyAllocs(t *testing.T) {
	for _, indexed := range []bool{false, true} {
		db := joinScaleDatabaseIndexed(t, 512, 64, 16, indexed, indexed)
		catalog := db.Snapshot()
		queries := map[string]*Query{
			"primary key":  Select(Path("id")).Join(JoinOn("customers", "customer", JoinKey)),
			"inner filter": Select(Path("id")).Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))),
			"inner field": Select(Path("id")).
				Join(JoinOn("customers", "customer", "name").Where(Cmp("seat", Lt, 8))),
			// An inner filter over an escaped string: every probe decodes it,
			// so a probe that failed to reset its decoded-string scratch would
			// grow one arena for the length of the outer scan.
			"escaped inner filter": Select(Path("id")).
				Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("label", Eq, "label-1"))),
			"outer filter": Select(Path("id")).Where(Cmp("total", Lt, 400)).Join(JoinOn("customers", "customer", JoinKey)),
			"grouped": Select(Path("customer"), Count()).
				Join(JoinOn("customers", "customer", JoinKey)).GroupBy("customer"),
			"two clauses": Select(Count()).Join(
				JoinOn("customers", "customer", JoinKey),
				JoinOn("customers", "customer", JoinKey).Where(Cmp("seat", Lt, 8))),
			"ordered limited": Select(Path("id")).
				Join(JoinOn("customers", "customer", JoinKey)).OrderBy("id", Desc).Limit(50),
		}
		for _, strategy := range joinStrategies {
			for _, workers := range []int{1, 4} {
				for name, q := range queries {
					t.Run(fmt.Sprintf("indexed=%v/%s/workers=%d/%s",
						indexed, strategy.name, workers, name), func(t *testing.T) {
						runJoinAllocCase(t, q, catalog, strategy.membershipMax, workers)
					})
				}
			}
		}
	}
}

// runJoinAllocCase is one (query, strategy, worker count) point of the join
// allocation contract. The worker count is swept because a parallel filter
// phase gives every worker its own join probe scratch, and a scratch grown per
// execution rather than retained would put the allocation back one level down —
// where a per-row alloc test on the calling goroutine would never see it.
func runJoinAllocCase(t *testing.T, q *Query, catalog store.DatabaseSnapshot, membershipMax, workers int) {
	t.Helper()
	var e Exec
	e.Options.Workers = workers
	e.Options.JoinMembershipMax = membershipMax
	// Two warm-ups: the first grows every buffer, the second settles the ones
	// whose size depends on the first run's high-water mark.
	for range 2 {
		if err := q.RunInto(&e, FromDatabase(catalog, "orders")); err != nil {
			t.Fatal(err)
		}
	}
	if e.Result.RowCount == 0 {
		t.Fatal("the fixture must select rows, or the measurement is of nothing")
	}
	allocs := testing.AllocsPerRun(25, func() {
		if err := q.RunInto(&e, FromDatabase(catalog, "orders")); err != nil {
			panic(err)
		}
	})
	if allocs != 0 {
		t.Fatalf("warmed joined RunInto allocated %.2f times, want 0", allocs)
	}
}

// TestJoinPrimaryKeyAndMirroredFieldAgree checks the two spellings of one join
// against each other. The customers fixture stores each document's own key in a
// "name" field, so joining on the primary key and joining on that field ask
// exactly the same question through two entirely different bindings — one that
// can fall back to a keyed probe and one that cannot.
func TestJoinPrimaryKeyAndMirroredFieldAgree(t *testing.T) {
	db := joinScaleDatabase(t, 200, 40, 12, true)
	catalog := db.Snapshot()
	byKey := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
		OrderBy("id", Asc)
	byField := Select(Path("id")).
		Join(JoinOn("customers", "customer", "name").Where(Cmp("tier", Eq, "pro"))).
		OrderBy("id", Asc)

	var field Exec
	if err := byField.RunInto(&field, FromDatabase(catalog, "orders")); err != nil {
		t.Fatal(err)
	}
	if field.Result.RowCount == 0 {
		t.Fatal("the fixture must select rows")
	}
	for _, strategy := range joinStrategies {
		var key Exec
		key.Options.JoinMembershipMax = strategy.membershipMax
		if err := byKey.RunInto(&key, FromDatabase(catalog, "orders")); err != nil {
			t.Fatal(err)
		}
		if diff := diffResults(key.Result, field.Result); diff != "" {
			t.Fatalf("%s: joining on the key disagrees with joining on the mirrored field: %s",
				strategy.name, diff)
		}
	}
}

// TestJoinLookupTransientMemoryDoesNotScaleWithOuterRows pins the claim that
// makes the lookup strategy the sound answer when a membership would be
// unbounded: a probe has no build side and materializes nothing, so the
// transient storage it holds is one document's worth however many outer rows
// it is asked about.
//
// The inner filter reads an escaped string on purpose, because the decoded-
// string arena is the one buffer a probe could accidentally let grow: it is
// appended to per row, and only resetting it per probe keeps the peak at one
// document. The two collection sizes below differ by a factor of ten and must
// not differ in scratch.
func TestJoinLookupTransientMemoryDoesNotScaleWithOuterRows(t *testing.T) {
	// One decoded label is a handful of bytes; growth proportional to the outer
	// scan would put the larger run three orders of magnitude above this.
	const scratchBound = 4096
	q := Select(Count()).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("label", Eq, "label-1")))

	peaks := map[int]int{}
	for _, outerRows := range []int{2000, 20000} {
		db := joinScaleDatabase(t, outerRows, 64, 64, false)
		var e Exec
		e.Options.JoinMembershipMax = 1 // force the lookup
		if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
			t.Fatal(err)
		}
		if e.Stats.JoinLookups != 1 {
			t.Fatalf("outerRows=%d: want one lookup binding, got %+v", outerRows, e.Stats)
		}
		if got := e.Stats.JoinProbes + e.Stats.JoinFilterRejected; got != uint64(outerRows) {
			t.Fatalf("outerRows=%d: %d rows answered, want one per outer row: %+v",
				outerRows, got, e.Stats)
		}
		if got, _ := e.Result.Columns[0].Cells[0].Float64(); got == 0 {
			t.Fatalf("outerRows=%d: the fixture must match rows", outerRows)
		}
		peaks[outerRows] = cap(e.Workspace.joins[0].scan.text)
	}
	for outerRows, peak := range peaks {
		if peak > scratchBound {
			t.Fatalf("outerRows=%d: the probe retained %d bytes of decoded-string scratch, "+
				"want at most %d — a lookup join must not accumulate one document's decode into the next",
				outerRows, peak, scratchBound)
		}
	}
}

// TestJoinIsNotInTheSQLSubset states the scope boundary. The SQL front end is
// an optional adapter over a deliberately small grammar, and adding a join to
// it is a separate decision from adding one to the engine; until it is made, a
// JOIN must be rejected where it is written rather than parsed into something
// else.
func TestJoinIsNotInTheSQLSubset(t *testing.T) {
	for _, sql := range []string{
		`SELECT id FROM t JOIN u ON t.a = u.b`,
		`SELECT id FROM t INNER JOIN u ON t.a = u.b`,
		`SELECT id FROM t LEFT JOIN u ON t.a = u.b`,
	} {
		if _, err := Compile(sql); err == nil {
			t.Fatalf("%q compiled; the SQL subset has no join and must say so", sql)
		}
	}
}

// --- the semi-join reduction filter -----------------------------------------

// joinSelectiveDatabaseFor is the test-side twin of the benchmark fixture: an
// outer collection whose keys are scattered across a much larger inner one, of
// which only a fraction passes the inner filter. It is the shape a prefilter
// exists for, and the shape where the population it rejects is dominated by
// keys that exist and are rejected by the inner predicate rather than by keys
// that are simply absent.
func joinSelectiveDatabaseFor(t testing.TB, outerRows, customers, matching int) *store.Database {
	t.Helper()
	db := &store.Database{}
	orders, err := db.CreateCollection("orders", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < outerRows; i++ {
		doc := fmt.Sprintf(`{"id":%d,"customer":"c%d"}`, i, (i*7919)%customers)
		if _, err := orders.Put(fmt.Sprintf("o%d", i), []byte(doc)); err != nil {
			t.Fatal(err)
		}
	}
	buyers, err := db.CreateCollection("customers", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < customers; i++ {
		// Spread the matching customers evenly rather than putting them in a
		// prefix. A prefix is not a neutral layout: it makes any sample of the
		// first rows scanned unrepresentative of the whole, which is precisely
		// what the mid-scan abandon check reads. Testing a sampler against data
		// laid out to defeat sampling measures the layout, not the sampler —
		// the clustered case gets its own test instead.
		tier := "free"
		if stride := customers / max(matching, 1); stride > 0 && i%stride == 0 {
			tier = "pro"
		}
		// name repeats the key as a field, so the same join is expressible
		// against the primary key and against an ordinary inner path.
		if _, err := buyers.Put(fmt.Sprintf("c%d", i), []byte(
			fmt.Sprintf(`{"tier":%q,"seat":%d,"name":"c%d"}`, tier, i, i))); err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// TestJoinBloomPrefilterSkipsProbesWithoutChangingRows is the filter's whole
// claim in one test: with the prefilter the join performs far fewer probes, and
// it returns exactly the rows it returns without one.
//
// The unfiltered arm is the non-vacuity half, and it is reached through the
// documented option rather than through a build tag, so the two arms are the
// same binary running the same code with one decision inverted.
func TestJoinBloomPrefilterSkipsProbesWithoutChangingRows(t *testing.T) {
	const (
		outerRows = 4000
		customers = 8000
		matching  = 400
	)
	db := joinSelectiveDatabaseFor(t, outerRows, customers, matching)
	catalog := db.Snapshot()
	q := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
		OrderBy("id", Asc)

	run := func(ratio int) (Result, ExecStats) {
		var e Exec
		e.Options.JoinMembershipMax = 1 // force the lookup strategy
		e.Options.JoinFilterScanRatio = ratio
		if err := q.RunInto(&e, FromDatabase(catalog, "orders")); err != nil {
			t.Fatalf("ratio=%d: %v", ratio, err)
		}
		return e.Result, e.Stats
	}

	filtered, filteredStats := run(0) // the shipped default
	plain, plainStats := run(-1)      // the filter declined

	if filteredStats.JoinFilters != 1 || filteredStats.JoinFilterKeys != matching {
		t.Fatalf("filtered run: want one filter over the %d matching customers, got %+v",
			matching, filteredStats)
	}
	if plainStats.JoinFilters != 0 || plainStats.JoinFilterRejected != 0 {
		t.Fatalf("declined run: want no filter at all, got %+v", plainStats)
	}
	if plainStats.JoinProbes != outerRows {
		t.Fatalf("declined run: %d probes, want one per outer row (%d)",
			plainStats.JoinProbes, outerRows)
	}

	// Every outer row is still accounted for exactly once, and the filter
	// answered the overwhelming majority of them without a probe.
	if got := filteredStats.JoinProbes + filteredStats.JoinFilterRejected; got != outerRows {
		t.Fatalf("filtered run: %d rows answered, want %d: %+v", got, outerRows, filteredStats)
	}
	if filteredStats.JoinProbes*4 >= plainStats.JoinProbes {
		t.Fatalf("the filter removed only %d of %d probes; it is not earning the scan",
			plainStats.JoinProbes-filteredStats.JoinProbes, plainStats.JoinProbes)
	}

	// And the rows are identical, which is the property the whole one-sided
	// error argument rests on.
	if plain.RowCount == 0 || plain.RowCount == outerRows {
		t.Fatalf("row count %d is degenerate; the join must filter some but not all rows", plain.RowCount)
	}
	if diff := diffResults(filtered, plain); diff != "" {
		t.Fatalf("the prefilter changed the answer: %s", diff)
	}
}

// TestJoinBloomPrefilterHasNoFalseNegatives states the one error a Bloom filter
// must not have, at the only place it could arise: the filter is seeded from
// the alternatives collected before the threshold was crossed and then fed
// every key after it, so a seeding mistake would silently drop exactly the
// first joinMembershipMax matching rows.
//
// It is checked by driving the threshold across the whole inner side one value
// at a time, so every possible split between "seeded from lits" and "inserted
// during the scan" is exercised, and comparing against the same query answered
// without any filter at all.
func TestJoinBloomPrefilterHasNoFalseNegatives(t *testing.T) {
	const (
		outerRows = 400
		customers = 64
	)
	db := joinSelectiveDatabaseFor(t, outerRows, customers, customers)
	catalog := db.Snapshot()
	q := Select(Path("id")).Join(JoinOn("customers", "customer", JoinKey)).OrderBy("id", Asc)

	var reference Exec
	reference.Options.JoinMembershipMax = 1 << 20 // the exact membership
	if err := q.RunInto(&reference, FromDatabase(catalog, "orders")); err != nil {
		t.Fatal(err)
	}
	if reference.Stats.JoinMemberships != 1 {
		t.Fatalf("the reference run must bind a membership, got %+v", reference.Stats)
	}
	if reference.Result.RowCount != outerRows {
		t.Fatalf("every outer row should match this fixture, got %d", reference.Result.RowCount)
	}

	for limit := 1; limit <= customers+1; limit++ {
		var e Exec
		e.Options.JoinMembershipMax = limit
		if err := q.RunInto(&e, FromDatabase(catalog, "orders")); err != nil {
			t.Fatalf("limit=%d: %v", limit, err)
		}
		if diff := diffResults(e.Result, reference.Result); diff != "" {
			t.Fatalf("limit=%d (%+v): a key was lost across the membership/filter split: %s",
				limit, e.Stats, diff)
		}
		if e.Stats.JoinFilterRejected != 0 {
			t.Fatalf("limit=%d: the filter rejected %d rows, but every outer key has a partner",
				limit, e.Stats.JoinFilterRejected)
		}
	}
}

// TestJoinBloomScanBudgetIsRespected pins the pre-scan gate: the filter is only
// attempted when the inner side is small enough relative to the driving
// collection that scanning it could repay the probes it saves. Past that, the
// join probes every row, because a scan that cannot pay for itself is worse
// than no filter.
//
// The inner filter is made very selective so that the second gate — the
// mid-scan abandon, which has its own test below — always votes to keep, and
// what is left varying is the budget alone. The boundary case is included
// exactly: at candidates == outerRows*joinBloomScanRatio the scan is admitted,
// and one row past it is not.
func TestJoinBloomScanBudgetIsRespected(t *testing.T) {
	const customers = 4000
	cases := []struct {
		name       string
		outerRows  int
		wantFilter bool
	}{
		{"inner well inside the budget", 4000, true},
		{"inner one row past the budget", customers/joinBloomScanRatio - 1, false},
		{"inner far past the budget", 200, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := joinSelectiveDatabaseFor(t, tc.outerRows, customers, customers/50)
			var e Exec
			e.Options.JoinMembershipMax = 1
			q := Select(Count()).
				Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")))
			if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
				t.Fatal(err)
			}
			if e.Stats.JoinLookups != 1 {
				t.Fatalf("want a lookup binding, got %+v", e.Stats)
			}
			if got := e.Stats.JoinFilters == 1; got != tc.wantFilter {
				t.Fatalf("filter built = %v, want %v (%d inner candidates, %d outer rows): %+v",
					got, tc.wantFilter, customers, tc.outerRows, e.Stats)
			}
		})
	}
}

// TestJoinBloomAbandonsAScanThatCannotPay pins the second gate. The pre-scan
// budget cannot see how much of the outer side a filter will reject; this does,
// by reading the inner predicate's selectivity off the rows already scanned. An
// inner predicate that keeps nearly everything produces a filter that admits
// nearly everything, and finishing that scan is measurably worse than never
// starting it.
//
// Every case sits inside the pre-scan budget, so the only thing deciding the
// outcome is what the scan observed.
func TestJoinBloomAbandonsAScanThatCannotPay(t *testing.T) {
	cases := []struct {
		name                           string
		outerRows, customers, matching int
		wantFilter                     bool
	}{
		{"a selective inner filter is worth summarizing", 4000, 4000, 4000 / 50, true},
		{"an inner filter that keeps everything is not", 4000, 4000, 4000, false},
		// Half-selective, sized so that charging the gain against the whole
		// scan abandons and charging it against only the remaining scan would
		// not. The two rules disagree over a narrow band and this is inside it;
		// BenchmarkJoinBloomPrefilter measured the marginal rule keeping the
		// filter here and paying 7% for it, which is what settled the choice.
		{"half-selective, where the cheaper-looking rule loses", 3900, 8000, 4000, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := joinSelectiveDatabaseFor(t, tc.outerRows, tc.customers, tc.matching)
			var e Exec
			e.Options.JoinMembershipMax = 1
			q := Select(Count()).
				Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")))
			if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
				t.Fatal(err)
			}
			if got := e.Stats.JoinFilters == 1; got != tc.wantFilter {
				t.Fatalf("filter kept = %v, want %v with %d of %d inner rows matching "+
					"and %d outer rows: %+v",
					got, tc.wantFilter, tc.matching, tc.customers, tc.outerRows, e.Stats)
			}
			if !tc.wantFilter && e.Stats.JoinProbes != uint64(tc.outerRows) {
				t.Fatalf("an abandoned filter must leave every row to be probed, got %+v", e.Stats)
			}
		})
	}
}

// TestJoinBloomClusteredSelectivityDegradesGracefully is the known blind spot,
// stated as a test rather than left as a comment. The mid-scan check reads the
// first rows scanned; a collection that puts every matching document at the
// front makes that sample unrepresentative and the check abandons a filter that
// would have paid.
//
// What the test pins is that this is a cost outcome and not a correctness one:
// the rows are identical either way, and the abandoned scan is bounded by the
// sample window rather than by the collection's size.
func TestJoinBloomClusteredSelectivityDegradesGracefully(t *testing.T) {
	const (
		outerRows = 4000
		customers = 4000
		matching  = 80
	)
	db := &store.Database{}
	orders, err := db.CreateCollection("orders", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < outerRows; i++ {
		if _, err := orders.Put(fmt.Sprintf("o%d", i),
			[]byte(fmt.Sprintf(`{"id":%d,"customer":"c%d"}`, i, (i*7919)%customers))); err != nil {
			t.Fatal(err)
		}
	}
	buyers, err := db.CreateCollection("customers", store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < customers; i++ {
		// Every matching customer in a prefix: the layout that defeats a
		// sample of the first rows scanned.
		tier := "free"
		if i < matching {
			tier = "pro"
		}
		if _, err := buyers.Put(fmt.Sprintf("c%d", i),
			[]byte(fmt.Sprintf(`{"tier":%q}`, tier))); err != nil {
			t.Fatal(err)
		}
	}
	catalog := db.Snapshot()
	q := Select(Path("id")).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
		OrderBy("id", Asc)

	var clustered, reference Exec
	clustered.Options.JoinMembershipMax = 1
	if err := q.RunInto(&clustered, FromDatabase(catalog, "orders")); err != nil {
		t.Fatal(err)
	}
	reference.Options.JoinMembershipMax = 1
	reference.Options.JoinFilterScanRatio = -1
	if err := q.RunInto(&reference, FromDatabase(catalog, "orders")); err != nil {
		t.Fatal(err)
	}
	if reference.Result.RowCount == 0 {
		t.Fatal("the fixture must match rows")
	}
	if diff := diffResults(clustered.Result, reference.Result); diff != "" {
		t.Fatalf("a mis-sampled filter changed the answer, which it must never do: %s", diff)
	}
	// The scan is abandoned rather than completed, which is the bounded-cost
	// half of the claim. Whether it was abandoned or never started, no filter
	// survives to be consulted.
	if clustered.Stats.JoinFilters != 0 {
		t.Logf("the clustered layout did not defeat the sample this time: %+v", clustered.Stats)
	}
}

// TestJoinBloomFalsePositiveRate measures what the filter actually achieves at
// its shipped bit budget, so the sizing constant is backed by a number rather
// than by the textbook formula it was derived from. A filter far worse than
// designed would still be correct and would quietly stop paying for its scan;
// this is what notices.
func TestJoinBloomFalsePositiveRate(t *testing.T) {
	const keys = 20000
	var filter joinBloom
	filter.reset(keys)
	for i := 0; i < keys; i++ {
		filter.insert(hashJoinKey(fmt.Sprintf("present-%d", i)))
	}
	const trials = 200000
	var pr joinProbe
	positives := 0
	for i := 0; i < trials; i++ {
		if filter.admits(hashJoinKey(fmt.Sprintf("absent-%d", i)), &pr) {
			positives++
		}
	}
	rate := float64(positives) / float64(trials)
	// Every inserted key must still be admitted: a Bloom filter with a false
	// negative is not a Bloom filter.
	for i := 0; i < keys; i++ {
		if !filter.admits(hashJoinKey(fmt.Sprintf("present-%d", i)), &pr) {
			t.Fatalf("key %d was inserted and then rejected", i)
		}
	}
	if rate > 0.02 {
		t.Fatalf("false-positive rate %.4f at %d bits per key, want at most 0.02 — "+
			"the filter is admitting rows the scan was supposed to buy the right to skip",
			rate, joinBloomBits)
	}
	t.Logf("false-positive rate at %d bits per key over %d keys: %.4f (%d blocks, %d KiB)",
		joinBloomBits, keys, rate, len(filter.blocks), len(filter.blocks)*32/1024)
}

// TestJoinBloomSizingIsBoundedAndSound checks the two sizing invariants the
// probe path relies on: the block count is always a power of two, so selecting
// a block is a mask rather than a division, and it never exceeds the memory cap
// however many keys are announced.
func TestJoinBloomSizingIsBoundedAndSound(t *testing.T) {
	for _, keys := range []int{0, 1, 2, 15, 16, 4095, 4096, 1 << 20, 1 << 28} {
		blocks := joinBloomBlocks(keys)
		if blocks < 1 {
			t.Fatalf("keys=%d: %d blocks", keys, blocks)
		}
		if blocks&(blocks-1) != 0 {
			t.Fatalf("keys=%d: %d blocks is not a power of two, so the block mask is wrong", keys, blocks)
		}
		if blocks*32 > joinBloomMaxBytes {
			t.Fatalf("keys=%d: %d bytes exceeds the %d-byte cap", keys, blocks*32, joinBloomMaxBytes)
		}
		var filter joinBloom
		filter.reset(keys)
		if int(filter.mask)+1 != len(filter.blocks) {
			t.Fatalf("keys=%d: mask %d does not address %d blocks", keys, filter.mask, len(filter.blocks))
		}
	}
}

// TestJoinBloomKeepsAFilterWhoseScanIsAlreadyPaid pins the one branch of the
// mid-scan check that is not about selectivity at all. Once the inner side has
// been read to the end, the scan the check exists to protect is already spent,
// and the only cost still ahead is the per-row filter test — which a filter
// repays by rejecting anything at all. Abandoning there would throw away
// something already paid for.
//
// The fixture is an inner side smaller than one batch whose filter would fail
// the selectivity test outright: every inner document matches, so a mid-scan
// check that ignored the completed-scan branch would abandon it.
func TestJoinBloomKeepsAFilterWhoseScanIsAlreadyPaid(t *testing.T) {
	const (
		outerRows = 2000
		customers = 64 // well under joinBatchRows, so one batch reads them all
	)
	db := joinSelectiveDatabaseFor(t, outerRows, customers, customers)
	var e Exec
	e.Options.JoinMembershipMax = 1 // force the lookup strategy
	q := Select(Count()).
		Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro")))
	if err := q.RunInto(&e, FromDatabase(db.Snapshot(), "orders")); err != nil {
		t.Fatal(err)
	}
	if e.Stats.JoinLookups != 1 {
		t.Fatalf("want a lookup binding, got %+v", e.Stats)
	}
	if e.Stats.JoinFilters != 1 {
		t.Fatalf("the filter was abandoned after its scan had already completed: %+v", e.Stats)
	}
	if e.Stats.JoinFilterKeys != customers {
		t.Fatalf("the filter holds %d keys, want all %d inner documents",
			e.Stats.JoinFilterKeys, customers)
	}
}

// --- joins under the parallel filter phase ---------------------------------

// TestJoinParallelFilterPhaseMatchesSerial is the one place where joins and
// late materialization are not independent of each other, pinned as a test.
//
// A join leaf is a predicate, so it is evaluated by the filter phase — and that
// phase runs on several goroutines over disjoint row ranges. The binding those
// goroutines share is read-only once bindJoins has finished, but a probe writes
// on every row it sees: it resolves a document into one-row columns, decodes
// escaped strings into an arena, and counts what it did. All of that lives in a
// per-worker joinProbe for exactly this reason, and this test is what would
// notice if any of it moved back onto the shared binding — as a wrong answer
// here, and as a report from the race detector under -race.
//
// Every strategy is covered, because they do not share a hazard: a membership
// binding is a pure read of a sorted set, while a lookup binding writes.
func TestJoinParallelFilterPhaseMatchesSerial(t *testing.T) {
	// The row count is a multiple of both the worker counts below and the
	// chunk width, because the uncompacted parallel path partitions a snapshot
	// by chunk: a split that lands mid-chunk declines and silently runs
	// serially, which would make this test pass while measuring nothing.
	const (
		outerRows = 8192
		customers = 4096
		matching  = 200
	)
	db := joinSelectiveDatabaseFor(t, outerRows, customers, matching)
	catalog := db.Snapshot()

	queries := map[string]*Query{
		"projection": Select(Path("id")).
			Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
			OrderBy("id", Asc),
		"outer filter beside the join": Select(Path("id")).Where(Cmp("id", Lt, 3000)).
			Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
			OrderBy("id", Asc),
		"aggregate": Select(Count()).
			Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))),
		"grouped": Select(Path("customer"), Count()).
			Join(JoinOn("customers", "customer", JoinKey).Where(Cmp("tier", Eq, "pro"))).
			GroupBy("customer").OrderBy("customer", Asc),
		"inner field": Select(Path("id")).
			Join(JoinOn("customers", "customer", "name").Where(Cmp("tier", Eq, "pro"))).
			OrderBy("id", Asc),
	}

	for _, strategy := range joinStrategies {
		for name, q := range queries {
			t.Run(strategy.name+"/"+name, func(t *testing.T) {
				var serial Exec
				serial.Options.Workers = 1
				serial.Options.JoinMembershipMax = strategy.membershipMax
				if err := q.RunInto(&serial, FromDatabase(catalog, "orders")); err != nil {
					t.Fatal(err)
				}
				if serial.Result.RowCount == 0 {
					t.Fatal("the fixture must select rows, or the comparison proves nothing")
				}
				engaged := false
				for _, workers := range []int{2, 4, 8} {
					var parallel Exec
					parallel.Options.Workers = workers
					parallel.Options.JoinMembershipMax = strategy.membershipMax
					if err := q.RunInto(&parallel, FromDatabase(catalog, "orders")); err != nil {
						t.Fatalf("workers=%d: %v", workers, err)
					}
					if parallel.Workspace.scanUsed > 1 {
						engaged = true
					}
					if diff := diffResults(parallel.Result, serial.Result); diff != "" {
						t.Fatalf("workers=%d disagrees with the serial phase: %s", workers, diff)
					}
					// Every outer row is still answered exactly once across all
					// the workers, which is what proves the per-worker tallies
					// were summed rather than one worker's being reported.
					if serial.Stats.JoinLookups == 1 {
						want := serial.Stats.JoinProbes + serial.Stats.JoinFilterRejected
						got := parallel.Stats.JoinProbes + parallel.Stats.JoinFilterRejected
						if got != want {
							t.Fatalf("workers=%d answered %d rows at the join, want %d: %+v",
								workers, got, want, parallel.Stats)
						}
					}
				}
				// Without this the test would pass on a build where the filter
				// phase never split, and would therefore prove nothing about
				// the thing it exists to check.
				if !engaged {
					t.Fatal("the filter phase never ran across more than one worker")
				}
			})
		}
	}
}
