// Package vibesql is a read-only database/sql driver over vibejson's query
// engine, registered under the name "vibejson".
//
//	db, err := sql.Open("vibejson", "/var/lib/app/users.vj")
//	rows, err := db.Query(`SELECT name, age FROM users WHERE age >= ? ORDER BY age`, 21)
//
// It is a front end and nothing more. A statement is parsed by [sql], lowered
// by [query.PrepareStatement] onto the same compiled plan the programmatic
// builder produces, and executed by the same executor. There is no second
// evaluator, no second comparator, and no interpretation of SQL at run time.
//
// # What it costs
//
// SQL parsing is not free per query. It is free in steady state, and the
// difference is which API you use.
//
// A prepared statement — a *sql.Stmt, or any query database/sql prepares on
// your behalf and keeps — parses and compiles once. If it holds no placeholder,
// every later execution hands the identical plan to the identical executor:
// there is nothing left of SQL at that point, and the work is byte for byte the
// native builder's. If it holds placeholders, each execution re-lowers the plan,
// because a literal compiles into the plan as a typed constant; that re-lowering
// refills a retained compiler's arenas rather than allocating, so it is free of
// heap allocation but not free of work.
//
// An ad-hoc query — db.Query with a statement the pool does not keep — parses
// and compiles on every call and throws the plan away. In a loop that is the
// wrong shape. Hold a *sql.Stmt.
//
// The benchmarks measure all three against the equivalent builder query over
// the same source, rather than asserting the difference is small. On an Apple
// M4 Max, Go 1.26, 2000 documents, reading every cell as sql.RawBytes:
//
//	                      native   prepared     ad-hoc   rows out
//	point query          2.95 us    3.89 us    7.31 us          1
//	  no placeholder                3.58 us
//	filtered scan         125 us     161 us     164 us        600
//	grouped aggregate     127 us     128 us     131 us          3
//	primary-key join      352 us     410 us     421 us       2000
//
// Read it as three separate numbers. The per-execution overhead is the point
// query's 0.6-0.9 us, which is database/sql's pooled handoff, the *sql.Rows,
// and — for a placeholder statement — one re-lowering of the plan; a statement
// without placeholders shows the re-lowering as the 0.3 us between the two
// prepared rows. The per-row overhead is the gap that grows with rows out:
// about 60 ns per row on the scan, and it is one interface box per projected
// cell, because driver.Value is any and an interface value cannot be reused
// across rows. Every database/sql driver pays exactly that; the tests measure
// it as a slope and hold it at one allocation per cell and nothing more. And
// the parse-and-compile is ad-hoc minus prepared, 3.4 us on the point query,
// which is invisible on the grouped aggregate only because the scan behind it
// costs forty times as much.
//
// # The DSN
//
// The DSN is a filesystem path to a durable collection file, or "memory:" plus
// the name of a source this process registered with [Attach] or
// [AttachCollection]. It is not a parameter language and does not have options.
//
// A path can only open a collection created with default options, because a
// durable collection's index catalog is frozen at creation and Open refuses a
// mismatch. A collection with declared indexes is opened by the application,
// which knows its options, and handed over with [AttachCollection].
//
// A JOIN needs [Attach]. Both sides of a join are read from one
// [store.DatabaseSnapshot] — the consistent cut that makes snapshot skew across
// a join inexpressible — and only a heap [store.Database] produces one; a
// durable file holds a single collection and has no catalog.
//
// # Types
//
// driver.Value is a closed set and JSON is not, so the mapping is chosen to
// change no value on the way out:
//
//	JSON null, or an absent path   nil
//	JSON true / false              bool
//	an integer int64 can hold      int64
//	any other number               []byte, its exact decimal digits
//	a string                       []byte, decoded
//	an object or an array          []byte, its exact JSON
//
// Numbers do not go through float64, and that is deliberate. This engine
// compares numbers by exact decimal value: 9007199254740992 and
// 9007199254740993 are two values here and one float64. A driver that returned
// float64 would return the wrong integer for an ordinary 19-digit identifier
// and would do it silently. Digits scan into a float64, a string, a
// json.Number, or a math/big value as you choose; a collapsed float cannot be
// uncollapsed.
//
// Strings are []byte because a projected string borrows the collection's bytes
// or the execution workspace's decoded-text buffer, both reused by the next
// execution. database/sql copies a []byte into every Scan destination except
// sql.RawBytes, so the copy happens exactly once, where the caller decides to
// keep the value. Scanning into a *string or a *[]byte works as expected;
// scanning into an *any yields []byte.
//
// ColumnTypeScanType reports any for a projection, because a schemaless path
// genuinely has no single type, and int64 for COUNT, which does.
//
// # Where this dialect and SQL disagree
//
// These are the deviations that remain after lowering. Anything not listed here
// behaves as SQL does, and the differential tests in this package and in
// [query] check that against an independent reference.
//
// Null comparison is three-valued, as SQL's is. This was the one divergence
// worth fixing rather than documenting: the engine's comparison answers false
// for a null cell, so its NOT answers true, and a row every other SQL database
// drops would be kept. Lowering builds a predicate for "is TRUE" and a separate
// one for "is FALSE" and recurses through Kleene's tables, so WHERE, NOT, AND,
// OR, IN, and BETWEEN all read as SQL reads them. No known shape diverges. The
// cost falls only where the dialects differed: a positive predicate lowers to
// exactly what the builder would have built.
//
// An absent path and an explicit null are one value. IS NULL is true for both,
// and SQL has no notion of an absent column at all. The distinction is
// available as "IS [NOT] MISSING", which is true only when the path resolves to
// nothing.
//
// Comparison is within type, with a cross-type total order. SQL raises a type
// error or casts; here values compare by exact decimal value within numbers, by
// decoded content within strings, and across types by the fixed order null <
// bool < number < string < container. So "age > '5'" is false for every numeric
// age rather than an error.
//
// MIN and MAX are numeric. They extract their argument as a numeric column and
// skip non-numeric values, so MIN over a string field is null rather than the
// least string.
//
// SUM and AVG skip non-numeric values rather than failing, so a column of mixed
// types produces a total over its numbers rather than a type error.
//
// ORDER BY puts nulls first ascending and last descending. SQL leaves the
// placement implementation-defined and PostgreSQL answers the other way for
// ASC. NULLS FIRST and NULLS LAST are refused rather than silently ignored.
//
// Duplicate object keys resolve to the last occurrence. SQL has no equivalent,
// because a row cannot have two columns of one name.
//
// '@>' — jsonb-style containment — is two-valued rather than three. Its left
// operand is a JSON document, and JSON null is a value a needle can match, so
// declining to answer for it would make "x @> null" unanswerable rather than
// precise. It is also not standard SQL, so there is no standard reading to
// diverge from. Inside HAVING it is refused outright, along with IS MISSING;
// see below.
//
// JOIN is an inner join only where at most one inner row can match. The
// engine's join is a semi-join: it filters the FROM collection by the existence
// of a matching document and contributes no column of its own. SQL's inner join
// emits one row per matching pair. The two agree exactly when the inner join
// key is the inner collection's primary key, spelled o."$key", and only that
// shape lowers — every other JOIN is refused with the reason, and so is any
// projection of a joined collection's column, which has no operator at all.
// A condition in WHERE that reads only the joined collection becomes that
// join's inner filter; a condition reading two collections at once is refused.
//
// HAVING is applied after execution rather than by the plan, using the engine's
// own comparator over the result's cells. It therefore costs the statement its
// LIMIT pushdown, since a plan that stopped early would be cutting rows the
// filter had not yet rejected. Two leaf forms are refused inside it: IS MISSING,
// because a result cell records that a key is null and not whether it was
// absent, and '@>', because containment is answered on the execution path and a
// second copy of it here is exactly the duplication this design avoids.
//
// OFFSET is applied after execution too, and after HAVING, which is the order
// SQL specifies. Without HAVING the plan is asked for offset+limit rows so that
// skipping offset of them leaves limit behind.
//
// # What it will not do
//
// It is read-only. The dialect parses SELECT and refuses every other statement,
// so Exec returns an error rather than reporting zero rows affected, and Begin
// refuses rather than returning a transaction that does nothing. Each statement
// already reads one consistent snapshot; there is no cross-statement
// transaction to open.
//
// A context cancels a query before it starts and not once it is running. The
// executor has no cancellation hook, and checking a context in a loop that does
// not own the scan would be theatre.
//
// Named parameters are refused; the dialect has '?' placeholders, bound in
// source order.
//
// # Concurrency
//
// database/sql hands one connection to one goroutine at a time and runs many
// connections at once. Each connection owns its [query.Exec] — the buffers
// whose retained capacity makes a warmed execution allocation-free — and each
// prepared statement owns its own compiler and plan, so nothing execution
// touches is shared. The source is shared and is safe for concurrent readers by
// its own contract. Rows read the connection's Exec in place, which is safe
// precisely because database/sql holds the connection until they are closed.
//
// # The grammar
//
// See the [sql] package for the accepted grammar, the path language, the
// range-variable rule, and the list of constructs refused at parse time with a
// position.
package vibesql
