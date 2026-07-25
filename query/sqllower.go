package query

import (
	"fmt"
	"math"

	sqlast "github.com/thesyncim/vibejson/sql"
)

// Lowering a parsed SQL statement onto the builder's plan.
//
// # Three-valued logic, and why it is implemented rather than documented away
//
// SQL's comparison against NULL is UNKNOWN: neither true nor false, dropped by
// WHERE, and — the part that bites — not flipped by NOT. This engine is
// two-valued. Its leaves answer false for a null or absent cell, so Not of a
// comparison on a null path answers true, and a row every other SQL database
// drops is kept.
//
// That divergence is not documentable. A driver that silently returns extra
// rows for "WHERE NOT (x = 1)" is worse than one that refuses the query, and it
// is invisible until a null appears in production data. So it is implemented,
// and the implementation needs nothing the engine does not already have.
//
// The insight that makes it cheap is that the engine's leaves are already
// exact for TRUE. "x = 1" answers true on exactly the rows SQL calls TRUE, and
// false on the union of SQL's FALSE and UNKNOWN. What is missing is a way to
// ask for FALSE alone. So lowering carries two functions rather than one:
// lowerTrue(e) builds a predicate true where e is TRUE, and lowerFalse(e) one
// true where e is FALSE. WHERE takes lowerTrue of its root, because WHERE keeps
// the rows a predicate is TRUE on.
//
//	T(x op v)   = x op v                        (already exact)
//	F(x op v)   = NOT (x IS NULL) AND NOT (x op v)
//	T(NOT p)    = F(p)
//	F(NOT p)    = T(p)
//	T(a AND b)  = T(a) AND T(b)     F(a AND b) = F(a) OR  F(b)
//	T(a OR  b)  = T(a) OR  T(b)     F(a OR  b) = F(a) AND F(b)
//
// Those are Kleene's tables written as two boolean functions, and the recursion
// is a proof by structural induction as long as each leaf's pair is right. The
// leaves are enumerated in leafForm below, each with the two facts the pair
// needs: whether the leaf can be UNKNOWN at all, and the predicate that says it
// is not.
//
// The cost lands only where the semantics differ. A positive predicate — the
// overwhelming majority — lowers to exactly what the builder would have built,
// with no added conjunct and no added column: T of a leaf is the leaf. The
// "NOT (x IS NULL)" guard appears only under an odd number of negations and on
// a negated leaf form (NOT IN, NOT BETWEEN), which is precisely where SQL and
// the engine disagreed.

// lower rebuilds and compiles the plan for one binding of args.
//
// It follows the JSON front end's shape exactly — rewind, prepare, build,
// compile, publish — because that shape is what makes a warmed compile refill
// the compiler's arenas instead of reallocating them. A statement re-lowered
// per bind is therefore free of heap allocation in the steady state, the same
// contract Compiler.New and Compiler.Parse carry.
func (s *Statement) lower(args []any) error {
	c := &s.c
	c.rewind()
	c.prepare(&s.q)
	s.stack = s.stack[:0]
	err := s.build(args)
	if err == nil {
		var p *plan
		p, err = c.compilePlan(&s.q)
		if err == nil {
			s.q.built = c.outcome(p, nil)
			s.cached = s.params == 0
			s.lowerErr = nil
			return nil
		}
	}
	// The failure is installed in the Query as well as returned, so a caller
	// that ignores the error and executes anyway sees this error rather than
	// whichever half-built clauses survived into the builder state.
	_ = c.fail(&s.q, err)
	// A statement that cannot lower at all cannot lower for any binding
	// either, so caching the failure is right even when placeholders are
	// present: re-deriving it per execution would only re-derive the same
	// message. A failure that depends on the argument — a bad type, a negative
	// LIMIT — is raised before this point, by the leaf that read it.
	s.cached = true
	s.lowerErr = err
	return err
}

// build lowers every clause of the parsed statement into the builder state the
// compiler is about to read.
func (s *Statement) build(args []any) error {
	if err := s.buildColumns(); err != nil {
		return err
	}
	if err := s.buildJoins(args); err != nil {
		return err
	}
	if err := s.buildGroupBy(); err != nil {
		return err
	}
	if err := s.buildOrderBy(); err != nil {
		return err
	}
	if err := s.buildHaving(args); err != nil {
		return err
	}
	return s.buildLimit(args)
}

// Collection returns the name of the driving collection — the FROM entry. A
// statement always has one, because the parser requires FROM.
func (s *Statement) Collection() string { return s.tree.From[0].Name }

// buildColumns lowers the SELECT list, then appends any hidden column a HAVING
// leaf needs.
func (s *Statement) buildColumns() error {
	for i := range s.tree.Columns {
		col := &s.tree.Columns[i]
		if err := s.requireDriving(col.Path, "a projected column"); err != nil {
			return err
		}
		s.q.columns = append(s.q.columns, Column{
			agg:    aggKind(col.Agg),
			spec:   s.spec(col.Path),
			header: s.names[i],
		})
	}
	return nil
}

// buildGroupBy lowers GROUP BY.
func (s *Statement) buildGroupBy() error {
	for _, key := range s.tree.GroupBy {
		if err := s.requireDriving(key, "a GROUP BY key"); err != nil {
			return err
		}
		s.q.groupBy = append(s.q.groupBy, s.spec(key))
	}
	return nil
}

// buildOrderBy lowers ORDER BY.
//
// The engine orders nulls first ascending and last descending, which SQL
// leaves implementation-defined and PostgreSQL answers the other way round for
// ASC. That one is genuinely not repairable here: the plan has no nulls-first
// or nulls-last knob, and the ordering happens inside the executor's sort. It
// is documented as a deviation rather than papered over, and NULLS FIRST and
// NULLS LAST are refused by the parser instead of being silently ignored.
func (s *Statement) buildOrderBy() error {
	for i := range s.tree.OrderBy {
		term := &s.tree.OrderBy[i]
		if err := s.requireDriving(term.Path, "an ORDER BY key"); err != nil {
			return err
		}
		dir := Asc
		if term.Desc {
			dir = Desc
		}
		s.q.orderBy = append(s.q.orderBy, orderSpec{path: s.spec(term.Path), dir: dir})
	}
	return nil
}

// buildLimit resolves LIMIT and OFFSET, deciding which of them the plan can
// carry and which the cursor has to apply.
//
// The engine's plan has a limit and no offset, so OFFSET is always the
// cursor's. LIMIT is pushed into the plan as offset+limit whenever nothing
// drops rows after execution, because the plan stopping at offset+limit leaves
// exactly limit rows behind once the cursor has skipped offset of them. A
// HAVING clause does drop rows after execution, so it takes the pushdown away
// entirely: a plan that stopped early would be cutting rows the filter had not
// yet had the chance to reject.
func (s *Statement) buildLimit(args []any) error {
	s.offset, s.limit, s.hasLimit = 0, 0, false
	s.driverLimit = s.tree.Having != nil
	if s.tree.Offset != nil {
		n, err := s.count(*s.tree.Offset, args, "OFFSET")
		if err != nil {
			return err
		}
		s.offset = n
	}
	if s.tree.Limit != nil {
		n, err := s.count(*s.tree.Limit, args, "LIMIT")
		if err != nil {
			return err
		}
		s.limit, s.hasLimit = n, true
	}
	if s.hasLimit && !s.driverLimit {
		bound := s.offset + s.limit
		if bound < s.offset {
			bound = math.MaxInt // offset+limit overflowed; no bound is reachable
		}
		s.q.limit, s.q.hasLimit = bound, true
	}
	return nil
}

// count resolves a LIMIT or OFFSET operand, which the grammar allows to be an
// integer literal or a placeholder.
func (s *Statement) count(o sqlast.Operand, args []any, clause string) (int, error) {
	if o.Kind == sqlast.OperandNumber {
		n, ok := int64Spelling(o.Text)
		if !ok || n < 0 || int64(int(n)) != n {
			return 0, fmt.Errorf("query: %s %s is not a non-negative count", clause, o.Text)
		}
		return int(n), nil
	}
	if o.Kind != sqlast.OperandParam {
		return 0, fmt.Errorf("query: %s must be an integer or a placeholder", clause)
	}
	switch v := args[o.Ordinal].(type) {
	case int64:
		if v < 0 || int64(int(v)) != v {
			return 0, fmt.Errorf("query: %s was bound to %d, which is not a non-negative count", clause, v)
		}
		return int(v), nil
	case int:
		if v < 0 {
			return 0, fmt.Errorf("query: %s was bound to %d, which is not a non-negative count", clause, v)
		}
		return v, nil
	default:
		return 0, fmt.Errorf(
			"query: %s was bound to %T; a row count must be a non-negative integer", clause, args[o.Ordinal])
	}
}

// requireDriving refuses a path that names a joined collection outside the ON
// condition.
//
// This is the semi-join's boundary made explicit. The engine's join is
// existential: it keeps an outer row when a matching inner document exists, and
// contributes no inner column to the result. A statement that projects, groups,
// or orders by an inner path is therefore asking for an operator that does not
// exist, and it must be told so rather than answered with a plausible result
// built from the wrong columns.
func (s *Statement) requireDriving(p *sqlast.PathExpr, what string) error {
	if p == nil || p.Source == 0 {
		return nil
	}
	return fmt.Errorf(
		"query: %s names %q, a joined collection; this engine's join is a semi-join "+
			"that filters the FROM collection and returns no column of its own, so only "+
			"%q may be read here",
		what, s.tree.From[p.Source].Alias, s.tree.From[0].Alias)
}

// --- joins -----------------------------------------------------------------

// buildJoins lowers the JOIN clauses, and refuses the ones the engine's
// semi-join would answer differently from SQL.
//
// SQL's inner join emits one row per matching pair, so an outer row with three
// matching inner documents appears three times. The engine's join is
// existential and emits that row once. Those agree on exactly one shape: when
// at most one inner document can match, which is guaranteed when the inner join
// key is the inner collection's primary key and by nothing else this engine can
// check. So a join whose inner side is the primary key lowers, and every other
// join is refused with the reason.
//
// The primary key is spelled in SQL as the quoted identifier "$key", matching
// [JoinKey], because '$' is not an identifier byte in the dialect's lexer and
// quoting is how the grammar admits a name its identifier rule would not. That
// is also the only shape with an O(1) probe, so the join that lowers is the
// join the executor is fastest at.
func (s *Statement) buildJoins(args []any) error {
	if len(s.tree.From) == 1 {
		return s.buildWhere(args)
	}
	for i := 1; i < len(s.tree.From); i++ {
		ref := &s.tree.From[i]
		cond := ref.On
		if cond.Left.Source != 0 {
			return fmt.Errorf(
				"query: ON matches %q against %q, but this engine joins every collection "+
					"against the FROM collection %q; a chained join has no plan",
				s.tree.From[cond.Left.Source].Alias, ref.Alias, s.tree.From[0].Alias)
		}
		if s.spec(cond.Right) != JoinKey {
			return fmt.Errorf(
				"query: ON matches %s.%s, which is not %s's primary key. SQL's join emits one "+
					"row per matching pair and this engine's join emits the FROM row once, so the "+
					"two agree only when at most one %s document can match; write %s.%q to join on "+
					"the primary key",
				ref.Alias, s.spec(cond.Right), ref.Alias, ref.Alias, ref.Alias, JoinKey)
		}
		s.q.joins = append(s.q.joins, JoinOn(ref.Name, s.spec(cond.Left), JoinKey))
	}
	return s.buildWhere(args)
}

// buildWhere lowers WHERE, splitting it across the driving collection and the
// joined ones.
//
// A statement with a join writes every condition in one WHERE, including the
// conditions that narrow the joined collection. The engine puts those on the
// join clause itself, so the top-level conjunction is partitioned by which
// collection each conjunct reads: conjuncts over the FROM collection stay in
// the query's own filter, and a conjunct over exactly one joined collection
// becomes that clause's inner filter. The split is sound because WHERE keeps
// rows every conjunct is TRUE on, and an inner conjunct restricts which inner
// documents count as a match — which, when at most one can match, is the same
// question.
//
// A conjunct naming two collections at once, or an inner condition reached
// through OR rather than AND, has no such form and is refused.
func (s *Statement) buildWhere(args []any) error {
	if s.tree.Where == nil {
		return nil
	}
	conjuncts := []*sqlast.Expr{s.tree.Where}
	if s.tree.Where.Kind == sqlast.ExprAnd {
		conjuncts = s.tree.Where.Kids
	}
	base := len(s.stack)
	defer func() { s.stack = s.stack[:base] }()
	for _, conjunct := range conjuncts {
		source, mixed := exprSource(conjunct)
		if mixed {
			return fmt.Errorf(
				"query: a WHERE condition reads more than one collection; this engine tests each " +
					"collection separately, so a condition must name one of them")
		}
		pred, err := s.lowerNode(conjunct, true, args)
		if err != nil {
			return err
		}
		if source <= 0 {
			s.stack = append(s.stack, pred)
			continue
		}
		if err := s.narrowJoin(source, pred); err != nil {
			return err
		}
	}
	outer := s.stack[base:]
	switch len(outer) {
	case 0:
	case 1:
		s.q.where, s.q.hasWhere = outer[0], true
	default:
		kids := s.c.operands(len(outer))
		kids = append(kids, outer...)
		s.q.where, s.q.hasWhere = Predicate{kind: predAnd, kids: kids}, true
	}
	return nil
}

// narrowJoin conjoins pred onto the inner filter of the join at source index i.
func (s *Statement) narrowJoin(i int, pred Predicate) error {
	j := &s.q.joins[i-1]
	if !j.hasWhere {
		j.where, j.hasWhere = pred, true
		return nil
	}
	j.where = Predicate{kind: predAnd, kids: s.c.pair(j.where, pred)}
	return nil
}

// exprSource answers the single range variable a predicate reads, and whether
// it reads more than one. A predicate that reads no path at all — an empty
// boolean node — answers -1, which callers treat as the driving collection.
func exprSource(e *sqlast.Expr) (source int, mixed bool) {
	source = -1
	var walk func(*sqlast.Expr)
	walk = func(e *sqlast.Expr) {
		if mixed {
			return
		}
		for _, kid := range e.Kids {
			walk(kid)
		}
		if e.Path == nil {
			return
		}
		switch {
		case source < 0:
			source = e.Path.Source
		case source != e.Path.Source:
			mixed = true
		}
	}
	walk(e)
	return source, mixed
}

// --- predicates ------------------------------------------------------------

// lowerNode builds the predicate that is true exactly where e is TRUE, when
// wantTrue, and exactly where e is FALSE otherwise. See the file comment for
// why the two directions are one recursion.
func (s *Statement) lowerNode(e *sqlast.Expr, wantTrue bool, args []any) (Predicate, error) {
	switch e.Kind {
	case sqlast.ExprAnd:
		if wantTrue {
			return s.combine(predAnd, e.Kids, true, args)
		}
		return s.combine(predOr, e.Kids, false, args)
	case sqlast.ExprOr:
		if wantTrue {
			return s.combine(predOr, e.Kids, true, args)
		}
		return s.combine(predAnd, e.Kids, false, args)
	case sqlast.ExprNot:
		return s.lowerNode(e.Kids[0], !wantTrue, args)
	case sqlast.ExprBetween:
		return s.lowerBetween(e, wantTrue, args)
	default:
		form, err := s.leafForm(e, args)
		if err != nil {
			return Predicate{}, err
		}
		return s.resolve(form, wantTrue != e.Negated), nil
	}
}

// combine builds an n-ary boolean node over lowered operands, folding away the
// constants the leaf rules produce.
//
// The folding is not cosmetic. A leaf bound to a NULL argument is UNKNOWN for
// every row, which lowers to a constant, and leaving those constants in the
// tree would put a node the executor evaluates per row where the answer was
// already known — and, worse, would leave an empty disjunction inside a
// conjunction, which is a plan that reads as if it filtered on something.
func (s *Statement) combine(kind predKind, kids []*sqlast.Expr, wantTrue bool, args []any) (Predicate, error) {
	base := len(s.stack)
	defer func() { s.stack = s.stack[:base] }()
	for _, kid := range kids {
		p, err := s.lowerNode(kid, wantTrue, args)
		if err != nil {
			return Predicate{}, err
		}
		if kind == predAnd {
			if alwaysTrue(p) {
				continue
			}
			if alwaysFalse(p) {
				return neverTrue(), nil
			}
		} else {
			if alwaysFalse(p) {
				continue
			}
			if alwaysTrue(p) {
				return alwaysTruePred(), nil
			}
		}
		s.stack = append(s.stack, p)
	}
	operands := s.stack[base:]
	switch len(operands) {
	case 0:
		if kind == predAnd {
			return alwaysTruePred(), nil
		}
		return neverTrue(), nil
	case 1:
		return operands[0], nil
	default:
		nodes := s.c.operands(len(operands))
		nodes = append(nodes, operands...)
		return Predicate{kind: kind, kids: nodes}, nil
	}
}

// lowerBetween lowers BETWEEN as the conjunction it denotes.
//
// Expanding it here rather than in the parser is what keeps the three-valued
// rules exact: "x BETWEEN ? AND 5" with a NULL first argument is UNKNOWN when
// x <= 5 and FALSE when it is not, which only falls out if each bound is a leaf
// with its own UNKNOWN case. Treating the whole range as one leaf would have
// made the pair UNKNOWN in both cases and quietly dropped a row SQL keeps.
func (s *Statement) lowerBetween(e *sqlast.Expr, wantTrue bool, args []any) (Predicate, error) {
	low, err := s.compareForm(e.Path, sqlast.OpGe, e.List[0], args)
	if err != nil {
		return Predicate{}, err
	}
	high, err := s.compareForm(e.Path, sqlast.OpLe, e.List[1], args)
	if err != nil {
		return Predicate{}, err
	}
	inRange := wantTrue != e.Negated
	a, b := s.resolve(low, inRange), s.resolve(high, inRange)
	kind := predAnd
	if !inRange {
		// FALSE of a conjunction is the disjunction of its operands' FALSEs.
		kind = predOr
	}
	switch {
	case kind == predAnd && (alwaysFalse(a) || alwaysFalse(b)):
		return neverTrue(), nil
	case kind == predAnd && alwaysTrue(a):
		return b, nil
	case kind == predAnd && alwaysTrue(b):
		return a, nil
	case kind == predOr && (alwaysTrue(a) || alwaysTrue(b)):
		return alwaysTruePred(), nil
	case kind == predOr && alwaysFalse(a):
		return b, nil
	case kind == predOr && alwaysFalse(b):
		return a, nil
	}
	return Predicate{kind: kind, kids: s.c.pair(a, b)}, nil
}

// A leafForm is one SQL leaf reduced to the three facts the three-valued rules
// need: the engine predicate that is true exactly where the leaf is TRUE, and
// whether the leaf can be FALSE or TRUE at all.
//
// guard is the predicate that holds where the leaf is determinate, and is
// meaningful only when the leaf can be UNKNOWN. total marks the leaves that
// never can be — the null and existence tests, which are defined over every
// value including the absent one, and containment, which this engine answers
// over a JSON null haystack rather than declining to.
type leafForm struct {
	pred  Predicate
	guard Predicate
	total bool
	// noTrue and noFalse mark a leaf that a NULL-bound operand has made
	// constantly UNKNOWN in one or both directions.
	noTrue  bool
	noFalse bool
}

// resolve turns a leaf form into the predicate for the requested direction.
func (s *Statement) resolve(f leafForm, wantTrue bool) Predicate {
	if wantTrue {
		if f.noTrue {
			return neverTrue()
		}
		return f.pred
	}
	if f.noFalse {
		return neverTrue()
	}
	if f.total {
		return s.c.not(f.pred)
	}
	return Predicate{kind: predAnd, kids: s.c.pair(f.guard, s.c.not(f.pred))}
}

// leafForm reduces one non-boolean predicate node to its three-valued form.
func (s *Statement) leafForm(e *sqlast.Expr, args []any) (leafForm, error) {
	spec := s.spec(e.Path)
	switch e.Kind {
	case sqlast.ExprCompare:
		return s.compareForm(e.Path, e.Op, e.Value, args)
	case sqlast.ExprIn:
		return s.inForm(e, spec, args)
	case sqlast.ExprIsNull:
		// IS NULL is two-valued in both dialects, and it is where this engine
		// and SQL disagree about what null means rather than about logic: an
		// absent path is null here and SQL has no absent column at all. The
		// distinction is available as IS MISSING.
		return leafForm{pred: IsNull(spec), total: true}, nil
	case sqlast.ExprIsMissing:
		return leafForm{pred: s.c.not(Exists(spec)), total: true}, nil
	case sqlast.ExprContains:
		// Containment is deliberately two-valued. Its left operand is a JSON
		// document, and JSON null is a value that document can be and that a
		// needle can match, so declining to answer for it would make
		// "x @> 'null'" unanswerable rather than precise. PostgreSQL reaches
		// the same place from the other direction: its jsonb @> is UNKNOWN only
		// for a SQL NULL column, which this model does not have.
		return leafForm{pred: Contains(spec, e.Value.Text), total: true}, nil
	default:
		return leafForm{}, fmt.Errorf("query: unsupported predicate in SQL lowering")
	}
}

// compareForm builds the form of one comparison leaf.
func (s *Statement) compareForm(
	path *sqlast.PathExpr, op sqlast.CmpOp, operand sqlast.Operand, args []any,
) (leafForm, error) {
	spec := s.spec(path)
	value, known, err := s.operand(operand, args)
	if err != nil {
		return leafForm{}, err
	}
	if !known {
		// Comparison against NULL is UNKNOWN for every row, in both
		// directions. SQL spells the same thing "x = NULL", which the parser
		// refuses precisely because its reading depends on which semantics the
		// reader has in mind; a placeholder bound to nil is the one way the
		// shape can still arrive, and it means what SQL means.
		return leafForm{noTrue: true, noFalse: true}, nil
	}
	return leafForm{
		pred:  Cmp(spec, Op(op), value),
		guard: s.c.not(IsNull(spec)),
	}, nil
}

// inForm builds the form of a membership leaf.
//
// A NULL among the alternatives is the shape SQL authors get wrong most often,
// so it is worth stating what it lowers to. "x IN (1, NULL)" is TRUE when x is
// 1 and UNKNOWN otherwise — never FALSE — because the unmatched NULL leaves the
// disjunction undecided. So the membership is built over the alternatives that
// are known, which answers TRUE exactly right, and the leaf is marked as never
// FALSE, which makes "x NOT IN (1, NULL)" the empty result SQL says it is.
func (s *Statement) inForm(e *sqlast.Expr, spec string, args []any) (leafForm, error) {
	values := s.c.boxes.alloc(len(e.List))[:0]
	nullAlternative := false
	for _, operand := range e.List {
		value, known, err := s.operand(operand, args)
		if err != nil {
			return leafForm{}, err
		}
		if !known {
			nullAlternative = true
			continue
		}
		values = append(values, value)
	}
	return leafForm{
		pred:    In(spec, values...),
		guard:   s.c.not(IsNull(spec)),
		noFalse: nullAlternative,
	}, nil
}

// operand resolves a literal or a bound placeholder to the typed value
// [Compiler.makeLiteral] takes, reporting known=false for SQL NULL.
//
// Literal text is handed over by reference rather than copied: a parsed
// statement owns its own text, and the Statement owns the parse tree, so the
// spelling outlives every plan compiled from it. A bound argument is not
// owned by anything the statement can see, so its bytes are interned into the
// compiler's arena — which costs no allocation after the first bind of that
// shape and removes any question about whether database/sql may reuse the
// caller's buffer once the query returns.
func (s *Statement) operand(o sqlast.Operand, args []any) (any, bool, error) {
	switch o.Kind {
	case sqlast.OperandString:
		box := s.c.strs.one()
		*box = o.Text
		return box, true, nil
	case sqlast.OperandNumber:
		box := s.c.nums.one()
		*box = Number(o.Text)
		return box, true, nil
	case sqlast.OperandBool:
		// A bool needs no arena: Go interns the two boolean values, so boxing
		// one costs nothing.
		return o.Bool, true, nil
	case sqlast.OperandParam:
		return s.argument(args[o.Ordinal])
	default:
		return nil, false, fmt.Errorf("query: a JSON document is only an operand of '@>'")
	}
}

// argument converts one bound driver argument to a typed literal value.
//
// The pass-through cases return the caller's interface value rather than the
// type-switched local, which is the difference between reusing the box
// database/sql already made and allocating a second one on every bind.
func (s *Statement) argument(arg any) (any, bool, error) {
	switch v := arg.(type) {
	case nil:
		return nil, false, nil
	case bool, int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return arg, true, nil
	case string:
		box := s.c.strs.one()
		*box = s.c.internString(v)
		return box, true, nil
	case []byte:
		// A byte slice binds as a string literal, not as a JSON document.
		// database/sql hands text through as []byte from several drivers'
		// conventions, and a document has its own operand ('@>') that takes it
		// verbatim from the statement rather than from an argument.
		box := s.c.strs.one()
		*box = s.c.intern(v)
		return box, true, nil
	case Number:
		box := s.c.nums.one()
		*box = Number(s.c.internString(string(v)))
		return box, true, nil
	default:
		return nil, false, fmt.Errorf(
			"query: cannot bind %T as a SQL literal; bind a bool, an integer, a float, "+
				"a string, a []byte, a query.Number, or nil", arg)
	}
}

// --- boolean constants -----------------------------------------------------
//
// The engine already defines an empty conjunction as true and an empty
// disjunction as false, so the two constants need no new predicate kind and no
// new executor case: they are nodes the evaluator already reaches the right
// answer for, which is what makes folding them optional rather than required
// for correctness.

// neverTrue is the predicate no row satisfies.
func neverTrue() Predicate { return Predicate{kind: predOr} }

// alwaysTruePred is the predicate every row satisfies.
func alwaysTruePred() Predicate { return Predicate{kind: predAnd} }

func alwaysFalse(p Predicate) bool { return p.kind == predOr && len(p.kids) == 0 }
func alwaysTrue(p Predicate) bool  { return p.kind == predAnd && len(p.kids) == 0 }
