package query

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/thesyncim/simdjson"
)

// Given a battery of SQL queries paired with the builder query each is meant to
// denote, when both run over a set of corpora, then their column-oriented
// Results are identical — SQL and builder syntax lower to one typed plan. A
// second battery of malformed inputs asserts each yields a *ParseError with a
// position and never a panic. Together these are a differential (parser vs the
// builder it must match) plus a rejection suite over a bounded input domain.

// parserCorpora returns the corpora the parser differential runs every pair
// over; between them they carry every field, nesting, array, and container the
// SQL battery names.
func parserCorpora(t *testing.T) []*simdjson.DocSet {
	t.Helper()
	return []*simdjson.DocSet{
		mustDocSet(t,
			`{"a":1,"b":2,"c":3,"active":true}`,
			`{"a":2,"b":1,"active":false}`,
			`{"a":1,"b":5,"c":1}`,
			`{"a":null,"b":2,"c":9}`,
			`{"b":7}`,
			`{"a":2,"b":2,"c":2}`,
		),
		mustDocSet(t,
			`{"user":{"name":"amy"},"xs":[10,20,30],"tags":["a","b","c"],"m":{"x":1}}`,
			`{"user":{"name":"bob"},"xs":[40,50],"tags":["a"],"m":{"x":2}}`,
			`{"tags":["x"],"n":9007199254740993,"p":1.5}`,
			`{"n":9007199254740992,"p":2.5,"tags":["a","b"]}`,
			`{"user":{"name":"amy"},"m":{"x":1,"y":9}}`,
		),
	}
}

// TestParserMatchesBuilder is the parser differential: Compile(sql).Run must
// equal the paired builder query's Run over every corpus.
func TestParserMatchesBuilder(t *testing.T) {
	cases := []struct {
		sql  string
		want *Query
	}{
		// Projections.
		{`SELECT a FROM t`, Select(Path("a"))},
		{`SELECT a, b, c FROM t`, Select(Path("a"), Path("b"), Path("c"))},
		{`SELECT user.name FROM docs`, Select(Path("user.name"))},
		{`SELECT xs[1] FROM docs`, Select(Path("/xs/1"))},
		{`SELECT a["k"] FROM docs`, Select(Path("a.k"))},

		// Aggregates.
		{`SELECT COUNT(*) FROM t`, Select(Count())},
		{`SELECT COUNT(a) FROM t`, Select(Count("a"))},
		{`SELECT SUM(a), AVG(b), MIN(c), MAX(a) FROM t`, Select(Sum("a"), Avg("b"), Min("c"), Max("a"))},

		// Comparisons and literal types.
		{`SELECT a FROM t WHERE a = 1`, Select(Path("a")).Where(Cmp("a", Eq, 1))},
		{`SELECT a FROM t WHERE a != 1`, Select(Path("a")).Where(Cmp("a", Ne, 1))},
		{`SELECT a FROM t WHERE a <> 1`, Select(Path("a")).Where(Cmp("a", Ne, 1))},
		{`SELECT a FROM t WHERE a < 2`, Select(Path("a")).Where(Cmp("a", Lt, 2))},
		{`SELECT a FROM t WHERE a <= 1`, Select(Path("a")).Where(Cmp("a", Le, 1))},
		{`SELECT a FROM t WHERE a > 0`, Select(Path("a")).Where(Cmp("a", Gt, 0))},
		{`SELECT a FROM t WHERE a >= 2`, Select(Path("a")).Where(Cmp("a", Ge, 2))},
		{`SELECT a FROM t WHERE a = 'x'`, Select(Path("a")).Where(Cmp("a", Eq, "x"))},
		{`SELECT a FROM t WHERE active = true`, Select(Path("a")).Where(Cmp("active", Eq, true))},
		{`SELECT a FROM t WHERE active = false`, Select(Path("a")).Where(Cmp("active", Eq, false))},
		{`SELECT p FROM docs WHERE p >= 1.5`, Select(Path("p")).Where(Cmp("p", Ge, 1.5))},

		// Exact integer past float64's mantissa: parsed as int64, kept distinct.
		{`SELECT COUNT(*) FROM docs WHERE n = 9007199254740993`,
			Select(Count()).Where(Cmp("n", Eq, int64(9007199254740993)))},

		// Boolean combinators, precedence, and parentheses.
		{`SELECT a FROM t WHERE a = 1 AND b >= 1`,
			Select(Path("a")).Where(And(Cmp("a", Eq, 1), Cmp("b", Ge, 1)))},
		{`SELECT a FROM t WHERE a = 1 OR a = 2`,
			Select(Path("a")).Where(Or(Cmp("a", Eq, 1), Cmp("a", Eq, 2)))},
		{`SELECT a FROM t WHERE NOT a = 1`,
			Select(Path("a")).Where(Not(Cmp("a", Eq, 1)))},
		{`SELECT a FROM t WHERE a = 1 OR a = 2 AND b = 2`,
			Select(Path("a")).Where(Or(Cmp("a", Eq, 1), And(Cmp("a", Eq, 2), Cmp("b", Eq, 2))))},
		{`SELECT a FROM t WHERE (a = 1 OR a = 2) AND b = 2`,
			Select(Path("a")).Where(And(Or(Cmp("a", Eq, 1), Cmp("a", Eq, 2)), Cmp("b", Eq, 2)))},

		// Existence and null.
		{`SELECT COUNT(*) FROM t WHERE EXISTS(a)`, Select(Count()).Where(Exists("a"))},
		{`SELECT COUNT(*) FROM t WHERE NOT EXISTS(a)`, Select(Count()).Where(Not(Exists("a")))},
		{`SELECT COUNT(*) FROM t WHERE a IS NULL`, Select(Count()).Where(IsNull("a"))},
		{`SELECT COUNT(*) FROM t WHERE a IS NOT NULL`, Select(Count()).Where(Not(IsNull("a")))},

		// Containment.
		{`SELECT COUNT(*) FROM docs WHERE tags @> ["a","b"]`,
			Select(Count()).Where(Contains("tags", `["a","b"]`))},
		{`SELECT COUNT(*) FROM docs WHERE tags @> "x"`,
			Select(Count()).Where(Contains("tags", `"x"`))},
		{`SELECT COUNT(*) FROM docs WHERE m @> {"x": 1}`,
			Select(Count()).Where(Contains("m", `{"x": 1}`))},

		// GROUP BY, ORDER BY, LIMIT.
		{`SELECT a, COUNT(*) FROM t GROUP BY a`,
			Select(Path("a"), Count()).GroupBy("a")},
		{`SELECT a, b, COUNT(*) FROM t GROUP BY a, b`,
			Select(Path("a"), Path("b"), Count()).GroupBy("a", "b")},
		{`SELECT a, SUM(b) FROM t GROUP BY a ORDER BY a DESC LIMIT 2`,
			Select(Path("a"), Sum("b")).GroupBy("a").OrderBy("a", Desc).Limit(2)},
		{`SELECT a FROM t ORDER BY a`,
			Select(Path("a")).OrderBy("a", Asc)},
		{`SELECT a FROM t ORDER BY a ASC, b DESC`,
			Select(Path("a")).OrderBy("a", Asc).OrderBy("b", Desc)},
		{`SELECT a FROM t WHERE EXISTS(a) ORDER BY a ASC LIMIT 3`,
			Select(Path("a")).Where(Exists("a")).OrderBy("a", Asc).Limit(3)},

		// Case-insensitive keywords, with case-sensitive field names.
		{`select a FROM t where a = 1`, Select(Path("a")).Where(Cmp("a", Eq, 1))},
		{`SeLeCt COUNT(*) FrOm t WhErE a Is NuLl`, Select(Count()).Where(IsNull("a"))},
	}

	corpora := parserCorpora(t)
	for _, tc := range cases {
		got, err := Compile(tc.sql)
		if err != nil {
			t.Fatalf("Compile(%q): %v", tc.sql, err)
		}
		for ci, set := range corpora {
			gotRes, err := got.Run(set)
			if err != nil {
				t.Fatalf("compiled %q Run over corpus %d: %v", tc.sql, ci, err)
			}
			wantRes, err := tc.want.Run(set)
			if err != nil {
				t.Fatalf("builder for %q Run over corpus %d: %v", tc.sql, ci, err)
			}
			if a, b := resultKey(gotRes), resultKey(wantRes); a != b {
				t.Fatalf("%q over corpus %d disagrees with builder:\n got: %s\nwant: %s", tc.sql, ci, a, b)
			}
		}
	}
	t.Logf("parser differential: %d SQL/builder pairs × %d corpora = %d Run comparisons",
		len(cases), len(corpora), len(cases)*len(corpora))
}

// resultKey renders a Result to a canonical string for equality: two Results
// with the same headers, row count, and per-cell kind and JSON bytes are equal.
// Both sides run the same executor, so a projected or computed cell that agrees
// on value agrees byte for byte.
func resultKey(r Result) string {
	var b strings.Builder
	for _, c := range r.Columns {
		b.WriteByte('|')
		b.WriteString(c.Header)
	}
	b.WriteByte('\n')
	for row := 0; row < r.RowCount; row++ {
		for _, c := range r.Columns {
			cell := c.Cells[row]
			fmt.Fprintf(&b, "%d:%s|", cell.Kind(), cell.JSON())
		}
		b.WriteByte('\n')
	}
	return b.String()
}

// TestParserErrors asserts every malformed query is rejected with a *ParseError
// carrying a byte offset, and none panics.
func TestParserErrors(t *testing.T) {
	bad := []string{
		``,                   // empty
		`   `,                // only whitespace
		`SELECT`,             // no columns
		`SELECT a`,           // no FROM
		`SELECT a b FROM t`,  // two columns without a comma
		`SELECT a, FROM t`,   // trailing comma
		`SELECT * FROM t`,    // star projection unsupported
		`SELECT a FROM`,      // missing table name
		`SELECT a FROM t t2`, // unexpected trailing input
		`SELECT a FROM t GARBAGE`,
		`SELECT COUNT( FROM t`, // unterminated aggregate
		`SELECT COUNT(a b) FROM t`,
		`SELECT SUM() FROM t`,   // SUM needs a path
		`SELECT SUM(*) FROM t`,  // only COUNT takes *
		`SELECT a FROM t WHERE`, // dangling WHERE
		`SELECT a FROM t WHERE a =`,
		`SELECT a FROM t WHERE a = )`,
		`SELECT a FROM t WHERE a = null`, // null is not a comparison operand
		`SELECT a FROM t WHERE a === 1`,
		`SELECT a FROM t WHERE (a = 1`, // missing ')'
		`SELECT a FROM t WHERE a = 1 AND`,
		`SELECT a FROM t WHERE NOT`,
		`SELECT a FROM t WHERE a IS`,      // IS without NULL
		`SELECT a FROM t WHERE a IS 1`,    // IS must be followed by [NOT] NULL
		`SELECT a FROM t WHERE a @>`,      // @> without a JSON value
		`SELECT a FROM t WHERE a @> {bad`, // unterminated JSON
		`SELECT a FROM t WHERE a @> [1,2`, // unbalanced JSON array
		`SELECT a FROM t WHERE a = 'oops`, // unterminated string
		`SELECT a FROM t WHERE a ! 1`,     // stray '!'
		`SELECT a FROM t WHERE a = @ 1`,   // stray '@'
		`SELECT a FROM t GROUP`,           // GROUP without BY
		`SELECT a FROM t GROUP BY`,        // GROUP BY without a path
		`SELECT a FROM t ORDER a`,         // ORDER without BY
		`SELECT a FROM t ORDER BY`,        // ORDER BY without a path
		`SELECT a FROM t LIMIT`,           // LIMIT without a number
		`SELECT a FROM t LIMIT x`,         // LIMIT with a non-number
		`SELECT a FROM t LIMIT -1`,        // negative LIMIT
		`SELECT a FROM t WHERE a = 1 EXTRA`,
		`SELECT a[ FROM t`,    // unterminated bracket
		`SELECT a[-1] FROM t`, // negative index
	}
	for _, sql := range bad {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Fatalf("Compile(%q) panicked: %v", sql, r)
				}
			}()
			q, err := Compile(sql)
			if err == nil {
				t.Fatalf("Compile(%q) = nil error, want a ParseError (query %+v)", sql, q)
			}
			var pe *ParseError
			if !errors.As(err, &pe) {
				t.Fatalf("Compile(%q) error %v (%T), want *ParseError", sql, err, err)
			}
			if pe.Pos < 0 || pe.Pos > len(sql) {
				t.Fatalf("Compile(%q) ParseError offset %d out of range [0,%d]", sql, pe.Pos, len(sql))
			}
		}()
	}
	t.Logf("parser rejection suite: %d malformed inputs, each a positioned ParseError", len(bad))
}

// TestParserSemanticErrorNotParseError shows the split: a syntactically valid
// query that breaks a plan rule returns the executor's compile error, not a
// *ParseError.
func TestParserSemanticErrorNotParseError(t *testing.T) {
	cases := []string{
		`SELECT a, b FROM t GROUP BY a`,                   // projection not in GROUP BY
		`SELECT a, SUM(b) FROM t`,                         // projection mixed with aggregate, no GROUP BY
		`SELECT a, COUNT(*) FROM t GROUP BY a ORDER BY b`, // ORDER BY ungrouped path
	}
	for _, sql := range cases {
		q, err := Compile(sql)
		if err == nil {
			t.Fatalf("Compile(%q) = nil error, want a plan error (query %+v)", sql, q)
		}
		var pe *ParseError
		if errors.As(err, &pe) {
			t.Fatalf("Compile(%q) returned a ParseError %v; a plan-rule violation is not a syntax error", sql, err)
		}
	}
}

// TestParserRunsAfterCompile is a smoke check that a compiled query executes and
// projects the expected values, independent of the builder differential.
func TestParserRunsAfterCompile(t *testing.T) {
	set := mustDocSet(t, `{"a":1,"b":10}`, `{"a":2,"b":20}`, `{"a":1,"b":5}`)
	q, err := Compile(`SELECT a, SUM(b) FROM t WHERE a = 1 GROUP BY a`)
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	res := mustRun(t, q, set)
	if res.RowCount != 1 {
		t.Fatalf("RowCount = %d want 1", res.RowCount)
	}
	if c, _ := res.Column("sum(b)"); mustFloat(t, c.Cells[0]) != 15 {
		t.Fatalf("sum(b) = %s want 15", c.Cells[0])
	}
}
