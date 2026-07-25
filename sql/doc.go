// Package sql parses a deliberately small SQL dialect into an abstract syntax
// tree, for a front end that lowers to the [query] engine's compiled plan.
//
// It is a parser and an AST and nothing else: it holds no data, executes
// nothing, and depends on nothing outside the standard library and one internal
// view helper. Lowering the tree to a [query.Query] and exposing the result as
// a database/sql driver are separate layers, built on the types here.
//
// # The governing rule
//
// The dialect is bounded by what the engine can execute, not by what SQL can
// express. A construct is accepted only where it maps onto something the
// executor already has — comparison, membership, null and existence tests,
// jsonb containment, boolean combination, projection, grouping, the five
// reductions, ordering, and an inner equi-join. Everything else is refused
// here, with a position and a reason.
//
// That is a stronger rule than it sounds, and it is the reason for most of the
// choices below. A parser that accepted a window function and failed at
// lowering would report the failure from a place that has no statement text
// left: no offset, no line, no quoted token, and an author who has already been
// told their SQL parsed. Refusing at parse time costs a longer keyword table
// and buys every rejection an actionable message.
//
// Two clauses are accepted despite having no executor counterpart today, and
// both are flagged on their fields: [SelectStmt.Having] and
// [SelectStmt.Offset]. See "Accepted ahead of the engine" below.
//
// # Grammar
//
// Keywords are case-insensitive; everything else in the grammar below is
// literal.
//
//	statement    = "SELECT" [ "ALL" ] select-list
//	               "FROM" table-ref { join }
//	               [ "WHERE" predicate ]
//	               [ "GROUP" "BY" path { "," path } ]
//	               [ "HAVING" predicate ]
//	               [ "ORDER" "BY" sort-key { "," sort-key } ]
//	               [ limit-offset ] [ ";" ] EOF ;
//
//	select-list  = result-column { "," result-column } ;
//	result-column= ( "*" | ident "." "*" | path | aggregate ) [ "AS" name ] ;
//	aggregate    = "COUNT" "(" ( "*" | path ) ")"
//	             | ( "SUM" | "AVG" | "MIN" | "MAX" ) "(" path ")" ;
//
//	table-ref    = name [ [ "AS" ] name ] ;
//	join         = [ "INNER" ] "JOIN" table-ref "ON" join-cond ;
//	join-cond    = [ "(" ] path "=" path [ ")" ] ;
//
//	predicate    = disjunction ;
//	disjunction  = conjunction { "OR" conjunction } ;
//	conjunction  = negation { "AND" negation } ;
//	negation     = "NOT" negation | primary ;
//	primary      = "(" predicate ")" | leaf ;
//	leaf         = left "IS" [ "NOT" ] ( "NULL" | "MISSING" )
//	             | left [ "NOT" ] "IN" "(" operand { "," operand } ")"
//	             | left [ "NOT" ] "BETWEEN" operand "AND" operand
//	             | left "@>" json-document
//	             | left comparison operand ;
//	left         = path | aggregate ;          (* aggregate only in HAVING *)
//	comparison   = "=" | "!=" | "<>" | "<" | "<=" | ">" | ">=" ;
//	operand      = string | number | "TRUE" | "FALSE" | "?" ;
//
//	sort-key     = ( path | output-alias ) [ "ASC" | "DESC" ] ;
//	limit-offset = "LIMIT" count [ "OFFSET" count ]
//	             | "OFFSET" count [ "LIMIT" count ] ;
//	count        = integer | "?" ;
//
//	path         = name { "." name | "[" integer "]" | "[" string "]" } ;
//	name         = ident | quoted-ident ;
//
// A string literal is single-quoted and a quoted identifier is double-quoted;
// in both, an embedded quote is written by doubling it. Numbers follow
// JSON's grammar rather than SQL's looser one, because the literal is bound for
// the engine's exact-decimal literal space, which validates its spelling as
// JSON: "007" and "1." are refused here rather than at lowering. Comments are
// "-- to end of line" and "/* ... */".
//
// # Nested paths, and the one genuinely new decision
//
// Documents are nested and schemaless; SQL assumes flat columns. Three
// spellings were available for reaching into a document: dotted paths, the
// SQL/JSON arrow operators (u.address->>'city'), and SQL:2016's JSON_VALUE.
// This dialect uses dotted paths with bracket subscripts:
//
//	u.address.city        u.tags[0]        u.meta['weird.key']
//
// The reason is not brevity. query already has exactly one path language — a
// dotted name, or an RFC 6901 JSON Pointer when the string starts with '/' —
// and its compilePath turns both into the same compiled pointer. Introducing
// arrow operators would have put a second path language into one codebase, so
// that a path written in SQL and the same path written in a query document
// would be different syntax for the same thing, with two implementations to
// keep in agreement. [PathExpr.AppendSpec] renders a parsed path into exactly
// the spelling that existing compiler takes, and renders it deterministically,
// so every clause naming the same path produces byte-identical output — which
// is what lets query's path registry extract that path once no matter how many
// clauses read it.
//
// The rendering keeps the two forms distinct on purpose. A single clean field
// name stays a bare name, because that is the form compilePath marks as a
// single top-level field and routes through the fused columnar fast path;
// several clean names join with dots; and a subscript, an empty key, or a key
// containing '.', '/', or '~' forces the JSON Pointer form, whose tokens escape
// '~' as '~0' and '/' as '~1'. Array subscripts therefore work exactly as they
// already do in query's path syntax rather than being a new idea.
//
// # The ambiguity rule
//
// Dotted paths have one real cost, and joins are what expose it: in
// "u.address.city", "u" may be a range variable, or it may be a top-level field
// of a document in a single-source query. SQL never has to decide this, because
// SQL knows its tables' columns. A schemaless store does not, so the rule has
// to be syntactic:
//
//   - A leading identifier immediately followed by '.' is a range variable if
//     the statement declares one by that name in FROM or JOIN — either an
//     explicit AS alias or, absent one, the collection name itself. The rest of
//     the chain is then the path into that source's documents.
//   - Otherwise the whole chain, leading identifier included, is a path into
//     the statement's only source. A statement with more than one source has no
//     "only source", so an unqualified path there is rejected rather than
//     guessed at.
//
// Range variables therefore shadow top-level fields of the same name. That is
// the only choice that keeps "u.city" meaning what a join author expects, and
// it costs nothing in reach, because the shadowed field is still addressable by
// the same rule — qualify it. In a source aliased "u", the field "u" is "u.u",
// and its member "city" is "u.u.city". A name with no dot after it is never a
// range variable, so "u" alone and "u[0]" are the field, not the source.
//
// Identifiers are case-sensitive, unlike keywords. They are overwhelmingly JSON
// object keys, and JSON keys are case-sensitive, so folding them would make
// "SELECT Name" and "SELECT name" read one field and leave the other silently
// empty. The rule applies to range variables too, so one spelling never means
// two things in two clauses. Quoting an identifier therefore does not change
// its case; it only lets it hold a reserved word, a space, or punctuation.
//
// # Accepted ahead of the engine
//
// HAVING and OFFSET are in the grammar and have no executor counterpart today:
// query's plan has no filter between reduction and ordering, and no row skip.
// They are accepted rather than refused because both are fully resolvable here
// and neither needs anything but a small step added to an existing one. HAVING
// in particular is validated to the point where it cannot fail late: the parser
// binds every HAVING leaf either to an aggregate the SELECT list already
// computes — recording which output column, in [Expr.Column] — or to a GROUP BY
// key, so a HAVING that survives parsing needs a filter-after-reduce and no
// second aggregation pass.
//
// A lowering pass that cannot yet execute them must reject a non-nil Having or
// Offset explicitly. Ignoring either returns wrong rows silently, which is the
// one outcome worse than an error.
//
// # Where this dialect and SQL disagree
//
// These are semantic differences, not gaps, and they are what a caller needs to
// know before this is described as SQL compatibility.
//
// Null is two-valued here, not three. SQL's comparison against NULL yields
// UNKNOWN, which is neither true nor false and which NOT does not flip; the
// engine's evalCmp returns false for a null cell, and NOT of that is true. So
// where x is null, SQL drops a row matching NOT (x = 1) and the engine keeps
// it. The parser removes the one spelling whose reading would depend on which
// semantics the reader had in mind — "x = NULL" and a NULL inside an IN list
// are both refused, pointing at IS NULL — but it cannot remove the difference,
// which lives in NOT over any comparison on a null-or-absent path.
//
// Absent and null are one value. query treats a path that resolves to nothing
// and a path holding an explicit null identically, and IS NULL is true for
// both. SQL has no notion of an absent column at all. The distinction is
// available as "IS [NOT] MISSING", which is this dialect's spelling of the
// engine's existence test; it is spelled that way rather than as EXISTS(path)
// because EXISTS takes a subquery in SQL, and that spelling is reserved for
// refusing subqueries with an accurate message.
//
// Comparison is within type, with a cross-type total order. In SQL, comparing a
// number column to a string is a type error or an implicit cast. Here, values
// compare by exact decimal value within numbers, by decoded content within
// strings, and across types by the fixed order null < bool < number < string <
// container. So "age > '5'" is false for every numeric age rather than an error
// or a coercion.
//
// MIN and MAX are numeric. query extracts their argument as a numeric column
// and skips non-numeric values, so MIN over a string field is null rather than
// the least string. SUM and AVG skip non-numeric values instead of failing, so
// a column of mixed types produces a total over its numbers rather than a type
// error.
//
// Ordering puts nulls first ascending and last descending, which SQL leaves
// implementation-defined and PostgreSQL answers the other way for ASC. NULLS
// FIRST and NULLS LAST are refused rather than silently ignored.
//
// Duplicate object keys resolve to the last occurrence, matching the core's
// Node.Get. SQL has no equivalent because a row cannot have two columns of one
// name.
//
// # What is refused, and why
//
// Each of these is refused with a message naming the missing capability:
// SELECT DISTINCT and COUNT(DISTINCT ...) (no distinct operator); LIKE, ILIKE,
// SIMILAR TO, and regular-expression operators (no pattern operator);
// subqueries in any position, including EXISTS and IN (SELECT ...) (no nested
// execution); outer, cross, and natural joins and comma-separated FROM items
// (the engine matches a key on both sides, so a non-match has no row to emit);
// JOIN ... USING (schemaless documents have no declared columns to match by
// name); set operations, common table expressions, window functions, CASE,
// CAST, arithmetic, string concatenation, and scalar functions (the engine
// evaluates predicates over stored values, not computed expressions); ORDER BY
// and GROUP BY over output positions or aggregates; and every statement kind
// but SELECT.
//
// A join condition is deliberately not the general predicate grammar. The
// engine joins on one key equality, so ON accepts exactly "left.key =
// right.key" and refuses a conjunction or an inequality, rather than building a
// tree that reads as valid and has no executor.
//
// The plan rules query enforces when it compiles are restated here so they can
// be reported with a position: a projection under GROUP BY must be a grouping
// key, a plain path cannot be selected alongside an aggregate without GROUP BY,
// and a sort key under GROUP BY must be a grouping key.
//
// # Performance
//
// A [Parser] holds chunked arenas that a warmed parse refills rather than
// reallocates, the same shape as query's Compiler and for the same reason. The
// steady state is zero allocation: on an Apple M4 Max, a simple SELECT parses
// in about 118 ns, a two-collection join in about 730 ns, and a grouped
// aggregate with HAVING and ORDER BY in about 730 ns, each at 0 allocs/op and
// roughly 180 MB/s of statement text. The package-level [Parse] is the owning
// convenience form and allocates (about 11 allocations for a simple SELECT); a
// caller preparing statements in a loop holds a Parser.
//
// Parsing is on the prepare path, so this matters far less than the executor's
// per-row work — but a driver that prepares per request is an ordinary shape,
// and a front end that allocated per statement would be the only part of the
// pipeline that did.
package sql
