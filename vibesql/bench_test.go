package vibesql

import (
	"database/sql"
	"fmt"
	"strconv"
	"testing"

	"github.com/thesyncim/vibejson/query"
	"github.com/thesyncim/vibejson/store"
)

// What the driver costs on top of the engine.
//
// Every shape is measured three ways over one corpus, so the differences are
// attributable rather than assumed:
//
//	Native      the builder query, one query.Exec, RunInto, cells read directly
//	Prepared    a *sql.Stmt reused across executions
//	AdHoc       db.Query with the statement text, parsed and compiled per call
//
// Native is the floor. Prepared minus Native is what database/sql and this
// package add per execution: the pool handoff, the driver.Rows, the Scan
// conversions, and — for a statement with placeholders — one re-lowering of the
// plan. AdHoc minus Prepared is the parse and compile an unprepared query pays
// on every call, which is the number the package documentation refuses to hide.

const benchRows = 2000

// benchCorpus builds a corpus large enough that per-row work dominates the
// per-execution overhead in the scan benchmarks and small enough to stay in
// cache.
func benchCorpus() []string {
	tiers := []string{"free", "pro", "team"}
	docs := make([]string, 0, benchRows)
	for i := 0; i < benchRows; i++ {
		docs = append(docs, fmt.Sprintf(
			`{"id":%d,"name":"user-%d","age":%d,"tier":%q,"score":%d}`,
			i, i, 18+i%60, tiers[i%3], i*7%1000))
	}
	return docs
}

// benchDatabase attaches a heap database holding the corpus and returns the
// *sql.DB, the equivalent Segment, and the store.Database the join benchmark
// needs.
func benchDatabase(b *testing.B, collections map[string][]string) (*sql.DB, *store.Database) {
	b.Helper()
	db := &store.Database{}
	for name, docs := range collections {
		collection, err := db.CreateCollection(name, store.Options{})
		if err != nil {
			b.Fatal(err)
		}
		for i, doc := range docs {
			if _, err := collection.Put(strconv.Itoa(i), []byte(doc)); err != nil {
				b.Fatal(err)
			}
		}
	}
	name := fmt.Sprintf("b%p", db)
	dsn, err := Attach(name, db)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { Detach(name) })
	handle, err := sql.Open("vibejson", dsn)
	if err != nil {
		b.Fatal(err)
	}
	handle.SetMaxOpenConns(1)
	b.Cleanup(func() { _ = handle.Close() })
	return handle, db
}

// benchNative runs the builder query against exactly the source the driver
// resolves for the same statement — the same database, the same per-execution
// snapshot — so the difference between the two is the front end and nothing
// else. Comparing against a bare Segment instead would have measured the
// absence of the snapshot's posting indexes and called it driver overhead.
func benchNative(b *testing.B, db *store.Database, collection string, q *query.Query) {
	b.Helper()
	if err := q.Prepare(); err != nil {
		b.Fatal(err)
	}
	var e query.Exec
	if err := q.RunInto(&e, query.FromDatabase(db.Snapshot(), collection)); err != nil {
		b.Fatal(err)
	}
	sinkResult(b, &e.Result)

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := q.RunInto(&e, query.FromDatabase(db.Snapshot(), collection)); err != nil {
			b.Fatal(err)
		}
		sinkResult(b, &e.Result)
	}
}

// --- point query ------------------------------------------------------------
//
// A single-row lookup by an equality on a top-level field. It is the shape
// where per-execution overhead is the whole cost, so it is where the driver's
// own price is most visible.
//
// It is spelled as an equality rather than as a primary-key probe because the
// '$key' pseudo-path is an outer-path form the engine does not yet have; when
// it lands, this statement becomes a keyed lookup without any change here.

func BenchmarkPointNative(b *testing.B) {
	_, db := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchNative(b, db, "docs", query.Select(query.Path("name"), query.Path("score")).
		Where(query.Cmp("id", query.Eq, 1234)))
}

func BenchmarkPointPrepared(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	stmt, err := db.Prepare(`SELECT name, score FROM docs WHERE id = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	benchScan(b, func() (*sql.Rows, error) { return stmt.Query(1234) })
}

func BenchmarkPointPreparedLiteral(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	stmt, err := db.Prepare(`SELECT name, score FROM docs WHERE id = 1234`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	benchScan(b, func() (*sql.Rows, error) { return stmt.Query() })
}

func BenchmarkPointAdHoc(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchScan(b, func() (*sql.Rows, error) {
		return db.Query(`SELECT name, score FROM docs WHERE id = ?`, 1234)
	})
}

// --- filtered scan ----------------------------------------------------------

func BenchmarkScanNative(b *testing.B) {
	_, db := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchNative(b, db, "docs", query.Select(query.Path("name"), query.Path("score")).
		Where(query.Cmp("age", query.Ge, 60)))
}

func BenchmarkScanPrepared(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	stmt, err := db.Prepare(`SELECT name, score FROM docs WHERE age >= ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	benchScan(b, func() (*sql.Rows, error) { return stmt.Query(60) })
}

func BenchmarkScanAdHoc(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchScan(b, func() (*sql.Rows, error) {
		return db.Query(`SELECT name, score FROM docs WHERE age >= ?`, 60)
	})
}

// --- grouped aggregate ------------------------------------------------------

func BenchmarkGroupNative(b *testing.B) {
	_, db := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchNative(b, db, "docs", query.Select(query.Path("tier"), query.Count(), query.Sum("score")).
		GroupBy("tier").OrderBy("tier", query.Asc))
}

func BenchmarkGroupPrepared(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	stmt, err := db.Prepare(
		`SELECT tier, COUNT(*), SUM(score) FROM docs GROUP BY tier ORDER BY tier`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	benchScan(b, func() (*sql.Rows, error) { return stmt.Query() })
}

func BenchmarkGroupAdHoc(b *testing.B) {
	db, _ := benchDatabase(b, map[string][]string{"docs": benchCorpus()})
	benchScan(b, func() (*sql.Rows, error) {
		return db.Query(`SELECT tier, COUNT(*), SUM(score) FROM docs GROUP BY tier ORDER BY tier`)
	})
}

// --- join -------------------------------------------------------------------
//
// The one join shape that lowers: a semi-join whose inner key is the inner
// collection's primary key. See the package documentation for why every other
// JOIN is refused.

func benchJoinCollections() map[string][]string {
	orders := make([]string, 0, benchRows)
	for i := 0; i < benchRows; i++ {
		orders = append(orders, fmt.Sprintf(
			`{"id":%d,"cust":%q,"total":%d}`, i, strconv.Itoa(i%500), i*3%900))
	}
	customers := make([]string, 0, 500)
	tiers := []string{"free", "pro"}
	for i := 0; i < 500; i++ {
		customers = append(customers, fmt.Sprintf(`{"tier":%q}`, tiers[i%2]))
	}
	return map[string][]string{"orders": orders, "customers": customers}
}

func BenchmarkJoinNative(b *testing.B) {
	_, db := benchDatabase(b, benchJoinCollections())
	benchNative(b, db, "orders", query.Select(query.Path("id"), query.Path("total")).
		Join(query.JoinOn("customers", "cust", query.JoinKey).
			Where(query.Cmp("tier", query.Eq, "pro"))))
}

func BenchmarkJoinPrepared(b *testing.B) {
	db, _ := benchDatabase(b, benchJoinCollections())
	stmt, err := db.Prepare(
		`SELECT o.id, o.total FROM orders o JOIN customers c ON c."$key" = o.cust ` +
			`WHERE c.tier = ?`)
	if err != nil {
		b.Fatal(err)
	}
	defer stmt.Close()
	benchScan(b, func() (*sql.Rows, error) { return stmt.Query("pro") })
}

func BenchmarkJoinAdHoc(b *testing.B) {
	db, _ := benchDatabase(b, benchJoinCollections())
	benchScan(b, func() (*sql.Rows, error) {
		return db.Query(
			`SELECT o.id, o.total FROM orders o JOIN customers c ON c."$key" = o.cust `+
				`WHERE c.tier = ?`, "pro")
	})
}

// --- harness ----------------------------------------------------------------

// benchScan runs one query per iteration and reads every row through
// sql.RawBytes, which is the one Scan destination database/sql does not copy
// into. Scanning into a string or a []byte would measure database/sql's own
// per-value copy, which this package neither performs nor can avoid; RawBytes
// measures what reaches the driver.
func benchScan(b *testing.B, run func() (*sql.Rows, error)) {
	b.Helper()
	// One warm-up execution, so the connection's Exec, the statement's
	// compiler arenas, and the pool are all at their steady state before the
	// timer starts.
	rows, err := run()
	if err != nil {
		b.Fatal(err)
	}
	columns, err := rows.Columns()
	if err != nil {
		b.Fatal(err)
	}
	drain(b, rows, len(columns))

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := run()
		if err != nil {
			b.Fatal(err)
		}
		drain(b, rows, len(columns))
	}
}

var sink int

func drain(b *testing.B, rows *sql.Rows, columns int) {
	b.Helper()
	cells := make([]sql.RawBytes, columns)
	into := make([]any, columns)
	for i := range cells {
		into[i] = &cells[i]
	}
	for rows.Next() {
		if err := rows.Scan(into...); err != nil {
			b.Fatal(err)
		}
		for _, cell := range cells {
			sink += len(cell)
		}
	}
	if err := rows.Err(); err != nil {
		b.Fatal(err)
	}
	if err := rows.Close(); err != nil {
		b.Fatal(err)
	}
}

// sinkResult reads every cell of a native result, so the builder side does the
// same materialization work the driver side does.
func sinkResult(b *testing.B, r *query.Result) {
	b.Helper()
	for row := 0; row < r.RowCount; row++ {
		for _, column := range r.Columns {
			sink += len(column.Cells[row].Payload())
		}
	}
}
