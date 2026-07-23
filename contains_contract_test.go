package simdjson

import (
	"bytes"
	"encoding/json"
	"io"
	"math/big"
	"math/rand/v2"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/thesyncim/simdjson/document"
)

// ---------------------------------------------------------------------------
// Containment semantics must match PostgreSQL's documented jsonb @> operator.
//
// Three oracles pin the contract from independent directions:
//
//  1. The curated table testdata/contains_oracle.tsv transcribes the
//     documented behavior rule by rule and is verified against a real
//     server by benchmarks/pgbaseline/run-pg-contains.sh; the recorded
//     run lives in benchmarks/results/pg/contains-oracle.log.
//  2. refContains below re-implements the same semantics naively over
//     encoding/json's decoded form, with math/big.Rat as the exact
//     numeric comparator — sharing no code with the evaluator under test.
//  3. Metamorphic properties (reflexivity, the empty needle, the deletion
//     lattice, numeric respelling) hold on generated documents by
//     construction, without any evaluator at all.
//
// Every deterministic check runs the evaluator through all its spellings:
// RawContains, and Node.Contains over plain and enriched (HashKeys)
// indexes of both operands.
// ---------------------------------------------------------------------------

type containsOracleRow struct {
	name     string
	haystack []byte
	needle   []byte
	want     bool
	// pgVerified marks rows the run-pg-contains.sh script asserts against
	// a live server; the rest document the exact-decimal extension beyond
	// numeric's range, where the server errors instead of answering.
	pgVerified bool
}

// loadContainsOracle parses the curated table. Format errors fail the test:
// the table is an artifact shared with the PostgreSQL verification script,
// so both readers must agree on every byte.
func loadContainsOracle(t testing.TB) []containsOracleRow {
	t.Helper()
	data, err := os.ReadFile("testdata/contains_oracle.tsv")
	if err != nil {
		t.Fatal(err)
	}
	var rows []containsOracleRow
	for lineno, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, "\t")
		if len(fields) != 5 || (fields[3] != "t" && fields[3] != "f") || (fields[4] != "y" && fields[4] != "s") {
			t.Fatalf("contains_oracle.tsv line %d: malformed row %q", lineno+1, line)
		}
		rows = append(rows, containsOracleRow{
			name:       fields[0],
			haystack:   []byte(fields[1]),
			needle:     []byte(fields[2]),
			want:       fields[3] == "t",
			pgVerified: fields[4] == "y",
		})
	}
	return rows
}

// containsAllRoutes evaluates containment through every spelling and fails
// unless all agree on want: RawContains, and Node.Contains over plain and
// enriched indexes.
func containsAllRoutes(t *testing.T, haystack, needle []byte, want bool) {
	t.Helper()
	got, err := RawContains(haystack, needle)
	if err != nil {
		t.Fatalf("RawContains(%s, %s): %v", haystack, needle, err)
	}
	if got != want {
		t.Errorf("RawContains(%s, %s) = %v, want %v", haystack, needle, got, want)
	}
	plain := mustBuildIndex(t, haystack).Root().Contains(mustBuildIndex(t, needle).Root())
	if plain != want {
		t.Errorf("Contains(%s, %s) over plain indexes = %v, want %v", haystack, needle, plain, want)
	}
	enriched := mustBuildEnrichedIndex(t, haystack).Root().Contains(mustBuildEnrichedIndex(t, needle).Root())
	if enriched != want {
		t.Errorf("Contains(%s, %s) over enriched indexes = %v, want %v", haystack, needle, enriched, want)
	}
}

func mustBuildEnrichedIndex(t testing.TB, src []byte) Index {
	t.Helper()
	count, err := RequiredIndexEntries(src)
	if err != nil {
		t.Fatal(err)
	}
	index, err := BuildIndexOptions(src, make([]IndexEntry, count), document.IndexOptions{HashKeys: true})
	if err != nil {
		t.Fatal(err)
	}
	return index
}

// TestContainsOracle checks every curated row through every evaluator
// spelling and against the independent naive reference.
func TestContainsOracle(t *testing.T) {
	for _, row := range loadContainsOracle(t) {
		t.Run(row.name, func(t *testing.T) {
			containsAllRoutes(t, row.haystack, row.needle, row.want)
			if wildExponent(row.haystack) || wildExponent(row.needle) {
				// The naive reference materializes numbers as exact
				// rationals; exponents beyond its guard are covered by the
				// evaluator's own verdict above.
				return
			}
			h, ok := refDecode(row.haystack)
			if !ok {
				t.Fatalf("reference decode failed for %s", row.haystack)
			}
			n, ok := refDecode(row.needle)
			if !ok {
				t.Fatalf("reference decode failed for %s", row.needle)
			}
			if ref := refContains(h, n); ref != row.want {
				t.Errorf("naive reference disagrees with curated verdict: got %v, want %v", ref, row.want)
			}
		})
	}
}

// TestContainsInvalidOperands pins RawContains's error contract: each
// operand must be exactly one valid document.
func TestContainsInvalidOperands(t *testing.T) {
	for _, test := range []struct{ haystack, needle string }{
		{`{`, `{}`},
		{`{}`, `{`},
		{``, `{}`},
		{`{}`, ``},
		{`{} {}`, `{}`},
		{`{}`, `1 2`},
	} {
		if got, err := RawContains([]byte(test.haystack), []byte(test.needle)); err == nil {
			t.Errorf("RawContains(%q, %q) = %v, want error", test.haystack, test.needle, got)
		}
	}
}

// TestContainsInvalidNode pins the zero Node: it contains nothing and is
// contained in nothing.
func TestContainsInvalidNode(t *testing.T) {
	valid := mustBuildIndex(t, []byte(`{"a":1}`)).Root()
	if (Node{}).Contains(valid) || valid.Contains(Node{}) || (Node{}).Contains(Node{}) {
		t.Error("invalid Node participated in containment")
	}
}

// TestContainsEscapedStringSteadyAllocs guards the incremental two-escaped-
// string path. Its inputs exceed the former stack materialization buffer, so
// any regression to decode-then-compare is visible as a heap allocation.
func TestContainsEscapedStringSteadyAllocs(t *testing.T) {
	a := []byte(`"` + strings.Repeat(`\u0061`, 96) + `\u00e9"`)
	b := []byte(`"` + strings.Repeat(`\u0061`, 96) + `\u00E9"`)
	haystack := mustBuildIndex(t, a).Root()
	needle := mustBuildIndex(t, b).Root()
	if !haystack.Contains(needle) {
		t.Fatal("equivalent escaped strings did not contain one another")
	}
	if n := testing.AllocsPerRun(100, func() {
		containsResultSink = haystack.Contains(needle)
	}); n != 0 {
		t.Fatalf("Contains allocated %.1f times for two long escaped strings", n)
	}

	keyA := `"` + strings.Repeat(`\u0061`, 96) + `\u00e9"`
	keyB := `"` + strings.Repeat(`\u0061`, 96) + `\u00E9"`
	haystack = mustBuildIndex(t, []byte(`{`+keyA+`:1,"other":2}`)).Root()
	needle = mustBuildIndex(t, []byte(`{`+keyB+`:1}`)).Root()
	if !haystack.Contains(needle) {
		t.Fatal("equivalent long escaped object keys did not match")
	}
	if n := testing.AllocsPerRun(100, func() {
		containsResultSink = haystack.Contains(needle)
	}); n != 0 {
		t.Fatalf("Contains allocated %.1f times for a long escaped object key", n)
	}
}

var containsResultSink bool

// ---------------------------------------------------------------------------
// The naive reference evaluator.
// ---------------------------------------------------------------------------

// refDecode decodes exactly one JSON document the way the reference
// evaluator consumes it: objects as maps (duplicate keys collapse to the
// last occurrence, jsonb's rule), numbers as json.Number spellings.
func refDecode(src []byte) (any, bool) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, false
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	return v, true
}

// refContains is the naive top-level evaluator: refDeepContains plus the
// documented array-contains-scalar exception, applied only here.
func refContains(haystack, needle any) bool {
	if h, ok := haystack.([]any); ok {
		switch needle.(type) {
		case map[string]any, []any:
		default:
			for _, element := range h {
				if refScalarEqual(element, needle) {
					return true
				}
			}
			return false
		}
	}
	return refDeepContains(haystack, needle)
}

func refDeepContains(h, n any) bool {
	switch n := n.(type) {
	case map[string]any:
		hm, ok := h.(map[string]any)
		if !ok {
			return false
		}
		for key, value := range n {
			hv, ok := hm[key]
			if !ok || !refDeepContains(hv, value) {
				return false
			}
		}
		return true
	case []any:
		ha, ok := h.([]any)
		if !ok {
			return false
		}
		for _, element := range n {
			found := false
			for _, candidate := range ha {
				if refDeepContains(candidate, element) {
					found = true
					break
				}
			}
			if !found {
				return false
			}
		}
		return true
	default:
		return refScalarEqual(h, n)
	}
}

// refScalarEqual compares scalars: same kind, same value, numbers as exact
// rationals.
func refScalarEqual(a, b any) bool {
	switch a := a.(type) {
	case nil:
		return b == nil
	case bool:
		bb, ok := b.(bool)
		return ok && a == bb
	case string:
		bs, ok := b.(string)
		return ok && a == bs
	case json.Number:
		bn, ok := b.(json.Number)
		if !ok {
			return false
		}
		ra, okA := new(big.Rat).SetString(a.String())
		rb, okB := new(big.Rat).SetString(bn.String())
		return okA && okB && ra.Cmp(rb) == 0
	default:
		return false
	}
}

// wildExponent reports whether src may spell a number whose exponent
// literal exceeds six digits. big.Rat materializes 10^|exponent| exactly,
// so the reference must not follow the evaluator into that range.
func wildExponent(src []byte) bool {
	for i := 0; i < len(src); i++ {
		if src[i] != 'e' && src[i] != 'E' {
			continue
		}
		j := i + 1
		if j < len(src) && (src[j] == '+' || src[j] == '-') {
			j++
		}
		digits := 0
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
			digits++
		}
		if digits > 6 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Generated documents: the metamorphic lattice.
// ---------------------------------------------------------------------------

// genNode is one generated JSON value: an exact scalar spelling, or a
// container of children. Object keys are unique within each object so the
// deletion lattice below is sound (removing members of a duplicate-free
// object always yields a contained document).
type genNode struct {
	kind byte // 'z' null, 'b' bool, 'n' number, 's' string, 'o' object, 'a' array
	lit  string
	keys []string // quoted key spellings, parallel to kids, objects only
	kids []genNode
}

var genKeyAlphabet = []string{
	`"k00"`, `"k01"`, `"k02"`, `"k03"`, `"k04"`, `"k05"`, `"k06"`, `"k07"`,
	`"kéy"`, `"nested"`, `"tags"`, `"value"`,
}

func genValue(r *rand.Rand, depth int) genNode {
	if depth >= 4 || r.IntN(100) < 45 {
		return genScalar(r)
	}
	if r.IntN(2) == 0 {
		return genObject(r, depth)
	}
	n := genNode{kind: 'a'}
	for range r.IntN(5) {
		n.kids = append(n.kids, genValue(r, depth+1))
	}
	return n
}

func genObject(r *rand.Rand, depth int) genNode {
	n := genNode{kind: 'o'}
	perm := r.Perm(len(genKeyAlphabet))
	for _, k := range perm[:r.IntN(6)] {
		n.keys = append(n.keys, genKeyAlphabet[k])
		n.kids = append(n.kids, genValue(r, depth+1))
	}
	return n
}

func genScalar(r *rand.Rand) genNode {
	switch r.IntN(10) {
	case 0:
		return genNode{kind: 'z', lit: "null"}
	case 1:
		return genNode{kind: 'b', lit: "true"}
	case 2:
		return genNode{kind: 'b', lit: "false"}
	case 3, 4, 5:
		return genNode{kind: 'n', lit: genNumber(r)}
	default:
		return genNode{kind: 's', lit: genString(r)}
	}
}

func genNumber(r *rand.Rand) string {
	switch r.IntN(8) {
	case 0:
		return "0"
	case 1:
		return "-0"
	case 2:
		// An integer beyond float64's 53-bit mantissa.
		return "9007199254740993" + strconv.Itoa(r.IntN(10))
	default:
		lit := strconv.Itoa(r.IntN(2000001) - 1000000)
		if r.IntN(2) == 0 {
			lit += "." + strconv.Itoa(r.IntN(10000))
		}
		if r.IntN(3) == 0 {
			lit += "e" + strconv.Itoa(r.IntN(25)-12)
		}
		return lit
	}
}

func genString(r *rand.Rand) string {
	words := []string{`w`, `word`, `héllo`, `emoji😀`, `line\nbreak`, `escApe`, ``}
	return `"` + words[r.IntN(len(words))] + strconv.Itoa(r.IntN(100)) + `"`
}

// renderGen serializes a generated node to JSON text.
func renderGen(n genNode) []byte {
	var b []byte
	return appendGen(b, n)
}

func appendGen(b []byte, n genNode) []byte {
	switch n.kind {
	case 'o':
		b = append(b, '{')
		for i, kid := range n.kids {
			if i > 0 {
				b = append(b, ',')
			}
			b = append(b, n.keys[i]...)
			b = append(b, ':')
			b = appendGen(b, kid)
		}
		return append(b, '}')
	case 'a':
		b = append(b, '[')
		for i, kid := range n.kids {
			if i > 0 {
				b = append(b, ',')
			}
			b = appendGen(b, kid)
		}
		return append(b, ']')
	default:
		return append(b, n.lit...)
	}
}

// deleteSome returns a structural subset: a copy of n with a random
// selection of members and elements removed at every level. Objects here
// are duplicate-free by construction, so the result is always contained
// in the original.
func deleteSome(r *rand.Rand, n genNode) genNode {
	if n.kind != 'o' && n.kind != 'a' {
		return n
	}
	out := genNode{kind: n.kind}
	for i, kid := range n.kids {
		if r.IntN(4) == 0 {
			continue
		}
		if r.IntN(2) == 0 {
			kid = deleteSome(r, kid)
		}
		if n.kind == 'o' {
			out.keys = append(out.keys, n.keys[i])
		}
		out.kids = append(out.kids, kid)
	}
	return out
}

// respellNumbers rewrites a random selection of the tree's numbers into
// equivalent spellings, exercising exact numeric equality inside
// structural containment.
func respellNumbers(r *rand.Rand, n genNode) genNode {
	switch n.kind {
	case 'o', 'a':
		out := n
		out.kids = append([]genNode(nil), n.kids...)
		for i := range out.kids {
			out.kids[i] = respellNumbers(r, out.kids[i])
		}
		return out
	case 'n':
		if r.IntN(5) > 0 {
			n.lit = respellNumber(r, n.lit)
		}
		return n
	default:
		return n
	}
}

// respellNumber returns an equivalent spelling of one JSON number: the
// same sign, significant digits, and value with the decimal point and
// exponent moved. Zero respells to a zero spelling.
func respellNumber(r *rand.Rand, lit string) string {
	sign := ""
	rest := lit
	if rest[0] == '-' {
		sign, rest = "-", rest[1:]
	}
	mantissa, expPart, _ := strings.Cut(rest, "e")
	exp := 0
	if expPart != "" {
		exp, _ = strconv.Atoi(expPart)
	}
	intPart, fracPart, _ := strings.Cut(mantissa, ".")
	digits := intPart + fracPart
	first := strings.IndexFunc(digits, func(c rune) bool { return c != '0' })
	if first < 0 {
		return [...]string{"0", "-0", "0.0", "0e7"}[r.IntN(4)]
	}
	last := len(digits) - 1
	for digits[last] == '0' {
		last--
	}
	sig := digits[first : last+1]
	weight := len(intPart) - 1 - first + exp
	switch r.IntN(3) {
	case 0:
		if len(sig) == 1 {
			return sign + sig + "e" + strconv.Itoa(weight)
		}
		return sign + sig[:1] + "." + sig[1:] + "e" + strconv.Itoa(weight)
	case 1:
		return sign + "0." + sig + "e" + strconv.Itoa(weight+1)
	default:
		return sign + sig + "e" + strconv.Itoa(weight-len(sig)+1)
	}
}

// TestContainsProperties checks the metamorphic contract on generated
// documents: reflexivity, the empty needle, the deletion lattice with
// transitivity, numeric respelling, novel-key rejection, and agreement
// with the naive reference on independent pairs.
func TestContainsProperties(t *testing.T) {
	r := rand.New(rand.NewPCG(0x5eed, 0xc0ffee))
	for i := range testIterations(400, 60) {
		root := genValue(r, 0)
		if root.kind != 'o' && root.kind != 'a' {
			root = genObject(r, 0)
		}
		x := renderGen(root)

		containsAllRoutes(t, x, x, true)
		if root.kind == 'o' {
			containsAllRoutes(t, x, []byte(`{}`), true)
		} else {
			containsAllRoutes(t, x, []byte(`[]`), true)
		}

		yNode := deleteSome(r, root)
		y := renderGen(yNode)
		containsAllRoutes(t, x, y, true)
		z := renderGen(deleteSome(r, yNode))
		containsAllRoutes(t, y, z, true)
		containsAllRoutes(t, x, z, true)

		respelled := renderGen(respellNumbers(r, yNode))
		if hv, ok := refDecode(y); ok {
			// The respelling itself is verified against the exact
			// reference before it participates in the property.
			rv, ok := refDecode(respelled)
			if !ok || !refContains(hv, rv) || !refContains(rv, hv) {
				t.Fatalf("iteration %d: respelling changed the value: %s vs %s", i, y, respelled)
			}
		}
		containsAllRoutes(t, x, respelled, true)

		if root.kind == 'o' {
			novel := append([]byte(nil), x[:len(x)-1]...)
			if len(root.kids) > 0 {
				novel = append(novel, ',')
			}
			novel = append(novel, `"zz_novel":1}`...)
			containsAllRoutes(t, x, novel, false)
		}

		other := renderGen(genValue(r, 0))
		got, err := RawContains(x, other)
		if err != nil {
			t.Fatal(err)
		}
		h, okH := refDecode(x)
		n, okN := refDecode(other)
		if !okH || !okN {
			t.Fatalf("iteration %d: reference decode failed", i)
		}
		if want := refContains(h, n); got != want {
			t.Errorf("iteration %d: RawContains(%s, %s) = %v, reference says %v", i, x, other, got, want)
		}
	}
}

// FuzzContains fuzzes document pairs against the naive reference.
func FuzzContains(f *testing.F) {
	for _, seed := range [][2]string{
		{`{"a":1,"b":2}`, `{"a":1}`},
		{`[1,2,3]`, `3`},
		{`{"a":[1,2]}`, `{"a":1}`},
		{`{"a":1,"a":2}`, `{"a":2}`},
		{`[1.0]`, `[1]`},
		{`{"kéy":[{"n":1e2}]}`, `{"kéy":[{"n":100}]}`},
		{`12345678901234567890123456789`, `1.2345678901234567890123456789e28`},
	} {
		f.Add([]byte(seed[0]), []byte(seed[1]))
	}
	f.Fuzz(func(t *testing.T, haystack, needle []byte) {
		if len(haystack) > 1<<13 || len(needle) > 1<<13 {
			return
		}
		got, err := RawContains(haystack, needle)
		if err != nil {
			return
		}
		if wildExponent(haystack) || wildExponent(needle) {
			return
		}
		h, okH := refDecode(haystack)
		n, okN := refDecode(needle)
		if !okH || !okN {
			return
		}
		if want := refContains(h, n); got != want {
			t.Errorf("RawContains(%s, %s) = %v, reference says %v", haystack, needle, got, want)
		}
		enriched := mustBuildEnrichedIndex(t, haystack).Root().Contains(mustBuildEnrichedIndex(t, needle).Root())
		if enriched != got {
			t.Errorf("enriched Contains(%s, %s) = %v, RawContains says %v", haystack, needle, enriched, got)
		}
	})
}
