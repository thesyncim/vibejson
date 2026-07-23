package query

import (
	"fmt"
	"math"
	"strconv"

	"github.com/thesyncim/simdjson"
	"github.com/thesyncim/simdjson/document"
)

// An Op is a scalar comparison operator for Cmp.
type Op uint8

const (
	Eq Op = iota // =
	Ne           // !=
	Lt           // <
	Le           // <=
	Gt           // >
	Ge           // >=
)

// A Predicate is a WHERE condition: a leaf comparison, containment, existence,
// or null test, or a boolean combination of predicates. Predicates are plain
// values built by the constructors below; they compile with the query. The
// zero Predicate is not a valid condition — build one with a constructor.
type Predicate struct {
	kind  predKind
	path  string
	op    Op
	value any    // Cmp literal, inferred at compile
	json  string // Contains needle
	kids  []Predicate
}

type predKind uint8

const (
	predCmp predKind = iota
	predContains
	predExists
	predIsNull
	predAnd
	predOr
	predNot
)

// Cmp compares the value at path against a typed literal. The literal's Go
// type fixes the comparison type: bool, string, any signed or unsigned
// integer, or float32/float64. Comparison is within type — numbers by exact
// decimal value, strings by content, bools by value — with a defined
// cross-type order (null < bool < number < string); a null or absent value
// never satisfies a comparison (test it with IsNull or Exists). An unsupported
// literal type is reported when the query compiles.
func Cmp(path string, op Op, value any) Predicate {
	return Predicate{kind: predCmp, path: path, op: op, value: value}
}

// Contains tests PostgreSQL jsonb containment (@>): whether the value at path
// contains jsonLiteral, which must be one JSON document. It compiles to the
// core's RawContains, whose exact-decimal number comparison and structural
// semantics this inherits. An absent value contains nothing.
func Contains(path, jsonLiteral string) Predicate {
	return Predicate{kind: predContains, path: path, json: jsonLiteral}
}

// Exists tests whether path is present in the document, whatever its value —
// including an explicit null. An absent path is not present.
func Exists(path string) Predicate {
	return Predicate{kind: predExists, path: path}
}

// IsNull tests whether the value at path is null or the path is absent, the
// two the query treats as one.
func IsNull(path string) Predicate {
	return Predicate{kind: predIsNull, path: path}
}

// And is the conjunction of its operands; with no operands it is always true.
func And(preds ...Predicate) Predicate {
	return Predicate{kind: predAnd, kids: preds}
}

// Or is the disjunction of its operands; with no operands it is always false.
func Or(preds ...Predicate) Predicate {
	return Predicate{kind: predOr, kids: preds}
}

// Not is the negation of pred.
func Not(pred Predicate) Predicate {
	return Predicate{kind: predNot, kids: []Predicate{pred}}
}

// A literal is a typed WHERE constant, resolved from Cmp's value at compile.
type literal struct {
	kind  scalarKind
	bval  bool
	num   []byte
	isInt bool
	ival  int64
	sval  string
}

// makeLiteral infers a typed literal from a Go value. Integers keep an exact
// int64 fast path; a uint64 beyond int64 and every float keep an exact decimal
// spelling instead, so comparison never rounds.
func makeLiteral(value any) (literal, error) {
	switch v := value.(type) {
	case bool:
		return literal{kind: kindBool, bval: v}, nil
	case string:
		return literal{kind: kindString, sval: v}, nil
	case int:
		return intLiteral(int64(v)), nil
	case int8:
		return intLiteral(int64(v)), nil
	case int16:
		return intLiteral(int64(v)), nil
	case int32:
		return intLiteral(int64(v)), nil
	case int64:
		return intLiteral(v), nil
	case uint:
		return uintLiteral(uint64(v)), nil
	case uint8:
		return intLiteral(int64(v)), nil
	case uint16:
		return intLiteral(int64(v)), nil
	case uint32:
		return intLiteral(int64(v)), nil
	case uint64:
		return uintLiteral(v), nil
	case float32:
		return floatLiteral(float64(v)), nil
	case float64:
		return floatLiteral(v), nil
	default:
		return literal{}, fmt.Errorf("query: unsupported literal type %T", value)
	}
}

func intLiteral(i int64) literal {
	return literal{kind: kindNumber, num: strconv.AppendInt(nil, i, 10), isInt: true, ival: i}
}

func uintLiteral(u uint64) literal {
	if u <= math.MaxInt64 {
		return intLiteral(int64(u))
	}
	return literal{kind: kindNumber, num: strconv.AppendUint(nil, u, 10)}
}

func floatLiteral(f float64) literal {
	return literal{kind: kindNumber, num: strconv.AppendFloat(nil, f, 'g', -1, 64)}
}

// A compiledPredicate is a WHERE tree resolved for repeated evaluation: leaf
// predicates reference a column by index, comparisons carry their classified
// literal, and containment carries its validated needle bytes. A leaf also
// carries its posting probe (postNone when unpostable), the descriptor
// candidates.go uses to prune candidate rows through DocSet.Postings.
type compiledPredicate struct {
	kind        predKind
	col         int
	op          Op
	lit         scalar
	needle      simdjson.Index
	probe       postProbe
	boundPath   string
	containPlan *compiledPredicate
	kids        []*compiledPredicate
}

// compilePredicate resolves a predicate tree, registering every path it reads
// in reg so the executor extracts each needed column once.
func compilePredicate(p Predicate, reg *pathRegistry) (*compiledPredicate, error) {
	switch p.kind {
	case predCmp:
		col, err := reg.add(p.path)
		if err != nil {
			return nil, err
		}
		lit, err := makeLiteral(p.value)
		if err != nil {
			return nil, err
		}
		cp := &compiledPredicate{kind: predCmp, col: col, op: p.op, lit: classifyLiteral(lit)}
		// Every equality compiles its exact scalar needle for declared Store
		// indexes, including nested paths. The older DocSet posting family is
		// limited to one top-level field and receives the same needle only when
		// that narrower contract applies.
		if p.op == Eq {
			if needle, ok := eqNeedle(cp.lit); ok {
				idx, err := buildNeedleIndex(needle)
				if err != nil {
					return nil, err
				}
				cp.needle = idx
				if reg.paths[col].single {
					cp.probe = postProbe{kind: postEq, path: reg.paths[col].name, needle: idx}
				}
			}
		}
		return cp, nil
	case predContains:
		col, err := reg.add(p.path)
		if err != nil {
			return nil, err
		}
		needle, scalarNeedle, err := containsNeedleIndex(p.json)
		if err != nil {
			return nil, fmt.Errorf("query: Contains literal: %w", err)
		}
		cp := &compiledPredicate{kind: predContains, col: col, needle: needle}
		// Only a scalar needle over a single top-level field prunes: the value
		// buckets index scalars, and a structured needle would fall to a full
		// scan inside WhereContains anyway, so leaving it unpostable avoids
		// scanning twice.
		if scalarNeedle && reg.paths[col].single {
			cp.probe = postProbe{kind: postContains, path: reg.paths[col].name, needle: needle}
		}
		if key, value, ok, probeErr := singleScalarObjectContainmentProbe(needle); probeErr != nil {
			return nil, probeErr
		} else if ok {
			base := reg.paths[col].indexPath()
			// DocSet postings address one top-level field. A root-object
			// containment predicate can therefore use the same derived scalar
			// equality as a sound candidate bound; nested forms are handled by
			// declared Store indexes below.
			if base == "" {
				cp.probe = postProbe{kind: postEq, path: key, needle: value}
			}
		}
		cp.containPlan, err = scalarObjectContainmentPlan(
			needle, reg.paths[col].indexPath(),
		)
		if err != nil {
			return nil, err
		}
		return cp, nil
	case predExists:
		col, err := reg.add(p.path)
		if err != nil {
			return nil, err
		}
		cp := &compiledPredicate{kind: predExists, col: col}
		if reg.paths[col].single {
			cp.probe = postProbe{kind: postExists, path: reg.paths[col].name}
		}
		return cp, nil
	case predIsNull:
		col, err := reg.add(p.path)
		if err != nil {
			return nil, err
		}
		return &compiledPredicate{kind: predIsNull, col: col}, nil
	case predAnd, predOr, predNot:
		kids := make([]*compiledPredicate, 0, len(p.kids))
		for _, kid := range p.kids {
			ck, err := compilePredicate(kid, reg)
			if err != nil {
				return nil, err
			}
			kids = append(kids, ck)
		}
		return &compiledPredicate{kind: p.kind, kids: kids}, nil
	default:
		return nil, fmt.Errorf("query: invalid predicate")
	}
}

const maxIndexedContainmentLeaves = 64

type scalarContainmentLeaf struct {
	path string
	raw  []byte
}

// scalarObjectContainmentPlan lowers an object needle made entirely of
// non-container leaves to exact path equalities. Object containment is exactly
// the conjunction of those leaves under the core's last-duplicate rule.
// Arrays and empty objects carry structural information that equality indexes
// cannot prove, so the rewrite is deliberately all-or-nothing.
func scalarObjectContainmentPlan(needle simdjson.Index, base string) (*compiledPredicate, error) {
	leaves := make([]scalarContainmentLeaf, 0, 4)
	if !appendScalarContainmentLeaves(&leaves, needle.Root(), base) ||
		len(leaves) == 0 || len(leaves) > maxIndexedContainmentLeaves {
		return nil, nil
	}
	kids := make([]*compiledPredicate, 0, len(leaves))
	for _, leaf := range leaves {
		value, err := buildNeedleIndex(leaf.raw)
		if err != nil {
			return nil, err
		}
		kids = append(kids, &compiledPredicate{
			kind: predCmp, col: -1, op: Eq, needle: value,
			boundPath: leaf.path,
		})
	}
	if len(kids) == 1 {
		return kids[0], nil
	}
	return &compiledPredicate{kind: predAnd, kids: kids}, nil
}

// indexPath returns the declared-index spelling for a comparison leaf.
// Scalar containment lowering sets boundPath without registering a value
// column: the derived equality exists only to choose an exact index and must
// never turn an array-shaped containment non-match into a JSON Pointer error.
func (p *compiledPredicate) indexPath(paths []compiledPath) string {
	if p.boundPath != "" {
		return p.boundPath
	}
	return paths[p.col].indexPath()
}

func appendScalarContainmentLeaves(dst *[]scalarContainmentLeaf, node simdjson.Node, base string) bool {
	count, ok := node.ObjectLen()
	if !ok || count == 0 {
		return false
	}
	type effectiveMember struct {
		key   string
		value simdjson.Node
	}
	members := make([]effectiveMember, 0, count)
	positions := make(map[string]int, count)
	iterator, _ := node.ObjectIter()
	for {
		key, value, ok := iterator.Next()
		if !ok {
			break
		}
		decoded, textOK := key.AppendText(nil)
		if !textOK {
			return false
		}
		name := string(decoded)
		if position, exists := positions[name]; exists {
			members[position].value = value
			continue
		}
		positions[name] = len(members)
		members = append(members, effectiveMember{key: name, value: value})
	}
	for _, member := range members {
		path := base + "/" + escapePointerSegment(member.key)
		switch member.value.Kind() {
		case document.Object:
			if !appendScalarContainmentLeaves(dst, member.value, path) {
				return false
			}
		case document.Array, document.Invalid:
			return false
		default:
			*dst = append(*dst, scalarContainmentLeaf{
				path: path, raw: member.value.Raw().Bytes(),
			})
			if len(*dst) > maxIndexedContainmentLeaves {
				return false
			}
		}
	}
	return true
}

// singleScalarObjectContainmentProbe recognizes the exact implication
//
//	container @> {key: scalar}  =>  container/key = scalar
//
// for a one-member object needle. The derived equality is safe as a DocSet
// posting bound because JSON object containment resolves that member through
// the same last-key and exact-scalar semantics as an exact index. Declared
// Store indexes use scalarObjectContainmentPlan for wider nested scalar
// objects. Compilation may allocate for an escaped key or the tiny scalar
// tape; execution does not.
func singleScalarObjectContainmentProbe(needle simdjson.Index) (string, simdjson.Index, bool, error) {
	root := needle.Root()
	count, ok := root.ObjectLen()
	if !ok || count != 1 {
		return "", simdjson.Index{}, false, nil
	}
	it, _ := root.ObjectIter()
	key, value, _ := it.Next()
	switch value.Kind() {
	case document.Array, document.Object, document.Invalid:
		return "", simdjson.Index{}, false, nil
	}
	decoded, _ := key.AppendText(nil)
	valueNeedle, err := buildNeedleIndex(value.Raw().Bytes())
	if err != nil {
		return "", simdjson.Index{}, false, err
	}
	return string(decoded), valueNeedle, true, nil
}

// containsNeedleScalar validates that s is exactly one JSON document — the
// requirement RawContains places on a containment needle — and reports whether
// that document is a scalar (as opposed to an array or object). It reuses the
// core validator by building the needle's index once; the root kind then tells
// the compiler whether the value postings can prune the leaf.
func containsNeedleIndex(s string) (simdjson.Index, bool, error) {
	src := []byte(s)
	entries, err := simdjson.RequiredIndexEntries(src)
	if err != nil {
		return simdjson.Index{}, false, err
	}
	idx, err := simdjson.BuildIndex(src, make([]simdjson.IndexEntry, entries))
	if err != nil {
		return simdjson.Index{}, false, err
	}
	switch idx.Root().Kind() {
	case document.Array, document.Object:
		return idx, false, nil
	default:
		return idx, true, nil
	}
}

func buildNeedleIndex(src []byte) (simdjson.Index, error) {
	entries, err := simdjson.RequiredIndexEntries(src)
	if err != nil {
		return simdjson.Index{}, err
	}
	return simdjson.BuildIndex(src, make([]simdjson.IndexEntry, entries))
}

// eval evaluates the predicate for one row against the extracted columns.
func (p *compiledPredicate) eval(cols [][]scalar, row int, entries *[]simdjson.IndexEntry) bool {
	switch p.kind {
	case predCmp:
		return evalCmp(cols[p.col][row], p.op, p.lit)
	case predContains:
		cell := cols[p.col][row]
		if len(cell.raw) == 0 {
			return false // absent haystack contains nothing
		}
		need, err := simdjson.RequiredIndexEntries(cell.raw)
		if err != nil {
			return false
		}
		if cap(*entries) < need {
			*entries = make([]simdjson.IndexEntry, need)
		}
		haystack, err := simdjson.BuildIndex(cell.raw, (*entries)[:need])
		return err == nil && haystack.Root().Contains(p.needle.Root())
	case predExists:
		return present(cols[p.col][row])
	case predIsNull:
		return cols[p.col][row].kind == kindNull
	case predAnd:
		for _, kid := range p.kids {
			if !kid.eval(cols, row, entries) {
				return false
			}
		}
		return true
	case predOr:
		for _, kid := range p.kids {
			if kid.eval(cols, row, entries) {
				return true
			}
		}
		return false
	default: // predNot
		return !p.kids[0].eval(cols, row, entries)
	}
}

// evalCmp evaluates one comparison. A null or absent cell never satisfies a
// value comparison — the SQL-like rule that keeps null out of ordered results
// and out of the filter.
func evalCmp(cell scalar, op Op, lit scalar) bool {
	if cell.kind == kindNull {
		return false
	}
	c := compareScalar(cell, lit)
	switch op {
	case Eq:
		return c == 0
	case Ne:
		return c != 0
	case Lt:
		return c < 0
	case Le:
		return c <= 0
	case Gt:
		return c > 0
	case Ge:
		return c >= 0
	default:
		return false
	}
}

// present reports whether a classified cell came from a path that resolved to
// a value — including an explicit null, whose raw bytes are non-empty — as
// opposed to an absent path, whose classification carries no bytes.
func present(cell scalar) bool {
	return cell.kind != kindNull || len(cell.raw) > 0
}
