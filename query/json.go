package query

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thesyncim/vibejson"
)

// The JSON document front end. A query is itself a JSON object, so the same
// text that travels over a wire or sits in a config file compiles to the same
// immutable compiled plan the builder and the SQL subset produce. [New] takes that
// object as Go literals; [Parse] takes it as JSON bytes. Neither retains the
// document: it is lowered to Columns and Predicates and discarded.
//
//	{
//	  "select":  ["profile.country", {"total": {"$sum": "score"}}],
//	  "where":   {"tenant": "acme", "score": {"$gte": 5}},
//	  "groupBy": ["profile.country"],
//	  "orderBy": [{"profile.country": "desc"}],
//	  "limit":   20
//	}
//
// Sibling keys of an object conjoin, so the common all-of filter needs no
// explicit operator. Every clause accepts a bare scalar wherever it accepts a
// one-element array, so "groupBy": "team" and "groupBy": ["team"] agree.
//
// Paths are the same spec the builder's [Path] takes: dotted
// ("profile.country") or an RFC 6901 pointer ("/profile/country").
//
// Both forms are read through one lowering: see qvalue in json_value.go, which
// is why Parse never materializes the document as Go maps and slices.

// M is a JSON object written as a Go literal. Its values are the JSON value
// space: M, A, string, bool, nil, any Go numeric type, and [Number].
type M map[string]any

// A is a JSON array written as a Go literal, with the same element space as M.
type A []any

// Number is a JSON number held as its original decimal spelling, so a value
// too large for float64 — an integer past the 2^53 mantissa, say — compiles to
// an exact literal rather than a rounded one. [Parse] produces Number for every
// number it reads. Comparison against a Number is by exact decimal value, the
// same rule the engine applies to document numbers.
type Number string

// A lowerer holds the reusable scratch a single compilation needs. Its zero
// value is ready to use.
type lowerer struct {
	scratch []byte // unescaping object keys and strings
	needle  []byte // rendering containment needles
}

// New compiles a query document written as Go literals. A malformed document
// or a document that violates a plan rule is reported here rather than at Run.
//
//	q, err := query.New(query.M{
//		"select": query.A{"team", query.M{"n": query.M{"$count": nil}}},
//		"where":  query.M{"active": true, "score": query.M{"$gte": 5}},
//		"groupBy": "team",
//	})
func New(doc M) (*Query, error) {
	var l lowerer
	return l.compile(litValue(doc))
}

// Parse compiles a query document from JSON text. It is exactly New over the
// parsed document, with every number preserved as its original spelling, so a
// query that arrives as bytes and one written as Go literals compile to the
// same plan.
//
// Parse borrows src only while it compiles. Everything the returned Query
// retains — paths, aliases, literals, and containment needles — is copied, so
// src may be reused or modified as soon as Parse returns.
func Parse(src []byte) (*Query, error) {
	entries, err := vibejson.RequiredIndexEntries(src)
	if err != nil {
		return nil, err
	}
	index, err := vibejson.BuildIndex(src, make([]vibejson.IndexEntry, entries))
	if err != nil {
		return nil, err
	}
	var l lowerer
	return l.compile(nodeValue(index.Root()))
}

// compile lowers a query document and compiles the resulting plan, so a
// malformed query fails here rather than at the first Run.
func (l *lowerer) compile(root qvalue) (*Query, error) {
	q, err := l.buildQuery(root)
	if err != nil {
		return nil, err
	}
	if _, err := q.compiled(); err != nil {
		return nil, err
	}
	return q, nil
}

// buildQuery lowers a query document onto the builder's own representation.
// The clauses are collected in one pass and applied in a fixed order, because
// Select must construct the Query before the other clauses can attach to it.
func (l *lowerer) buildQuery(root qvalue) (*Query, error) {
	if root.kind() != qObject {
		return nil, fmt.Errorf("query: a query document must be a JSON object, not %s", root.describeKind())
	}

	var selectClause, whereClause, groupClause, orderClause, limitClause qvalue
	err := root.fields(&l.scratch, func(key qkey, value qvalue) error {
		switch {
		case key.equals("select"):
			selectClause = value
		case key.equals("where"):
			whereClause = value
		case key.equals("groupBy"):
			groupClause = value
		case key.equals("orderBy"):
			orderClause = value
		case key.equals("limit"):
			limitClause = value
		default:
			return fmt.Errorf(
				"query: unknown query clause %q: expected select, where, groupBy, orderBy, or limit", key)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	columns, err := l.buildSelect(selectClause)
	if err != nil {
		return nil, err
	}
	q := Select(columns...)

	if !whereClause.missing() {
		predicate, err := l.buildPredicate(whereClause, at("where"))
		if err != nil {
			return nil, err
		}
		q.Where(predicate)
	}
	if !groupClause.missing() {
		paths, err := l.buildPaths(groupClause, at("groupBy"))
		if err != nil {
			return nil, err
		}
		q.GroupBy(paths...)
	}
	if !orderClause.missing() {
		if err := l.buildOrderBy(q, orderClause); err != nil {
			return nil, err
		}
	}
	if !limitClause.missing() {
		n, ok := limitClause.wholeNumberOf()
		if !ok {
			return nil, fmt.Errorf("query: limit: expected a whole number, not %s", limitClause.describeKind())
		}
		q.Limit(int(n))
	}
	return q, nil
}

// wholeDocument is the default projection: the row itself, the shape a
// document store returns when a query names no columns.
var wholeDocument = Column{agg: aggNone, spec: "", header: "*"}

// buildSelect lowers the select clause. An absent clause projects the whole
// document.
func (l *lowerer) buildSelect(spec qvalue) ([]Column, error) {
	if spec.missing() {
		return []Column{wholeDocument}, nil
	}
	if spec.kind() != qArray {
		column, err := l.buildColumn(spec, at("select"))
		if err != nil {
			return nil, err
		}
		return []Column{column}, nil
	}
	if spec.length() == 0 {
		return nil, fmt.Errorf(
			"query: select: an empty list projects nothing; omit the clause for the whole document")
	}
	columns := make([]Column, 0, spec.length())
	err := spec.elements(func(index int, element qvalue) error {
		column, err := l.buildColumn(element, at("select").elem(index))
		if err != nil {
			return err
		}
		columns = append(columns, column)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return columns, nil
}

// buildColumn lowers one select entry: a bare path, an aggregate, or either of
// those under an output name.
func (l *lowerer) buildColumn(spec qvalue, where loc) (Column, error) {
	if path, ok := spec.text(&l.scratch); ok {
		return Path(path), nil
	}
	if spec.kind() != qObject {
		return Column{}, fmt.Errorf(
			"query: %s: a column is a path string or an object, not %s", where, spec.describeKind())
	}
	name, value, ok := spec.onlyField(&l.scratch)
	if !ok {
		return Column{}, fmt.Errorf(
			"query: %s: a column object holds exactly one entry, an aggregate or an output name, not %d",
			where, spec.length())
	}
	if name.isOperator() {
		return l.buildAggregate(name, value, where)
	}
	named, err := l.buildNamedColumn(value, where.field(name))
	if err != nil {
		return Column{}, err
	}
	named.header = name.owned()
	return named, nil
}

// buildNamedColumn lowers the value side of an aliased column.
func (l *lowerer) buildNamedColumn(spec qvalue, where loc) (Column, error) {
	if path, ok := spec.text(&l.scratch); ok {
		return Path(path), nil
	}
	name, value, ok := spec.onlyField(&l.scratch)
	if !ok {
		return Column{}, fmt.Errorf(
			"query: %s: a named column is a path string or a one-entry aggregate object", where)
	}
	if !name.isOperator() {
		return Column{}, fmt.Errorf(
			"query: %s: %q is not an aggregate; aggregate names begin with $", where, name)
	}
	return l.buildAggregate(name, value, where)
}

// buildAggregate lowers one $-prefixed reduction.
func (l *lowerer) buildAggregate(op qkey, arg qvalue, where loc) (Column, error) {
	if op.equals("$count") {
		switch arg.kind() {
		case qNull:
			return Count(), nil
		case qBool:
			if enabled, _ := arg.boolean(); !enabled {
				return Column{}, fmt.Errorf("query: %s: $count takes true, a path, or null", where)
			}
			return Count(), nil
		case qString:
			path, _ := arg.text(&l.scratch)
			if path == "*" {
				return Count(), nil
			}
			return Count(path), nil
		case qObject:
			if arg.length() == 0 {
				return Count(), nil
			}
		}
		return Column{}, fmt.Errorf(
			"query: %s: $count takes a path, \"*\", {}, true, or null, not %s", where, arg.describeKind())
	}

	var build func(string) Column
	switch {
	case op.equals("$sum"):
		build = Sum
	case op.equals("$avg"):
		build = Avg
	case op.equals("$min"):
		build = Min
	case op.equals("$max"):
		build = Max
	default:
		return Column{}, fmt.Errorf(
			"query: %s: unknown aggregate %q: expected $count, $sum, $avg, $min, or $max", where, op)
	}
	path, ok := arg.text(&l.scratch)
	if !ok {
		return Column{}, fmt.Errorf(
			"query: %s: %s takes a path string, not %s", where, op, arg.describeKind())
	}
	return build(path), nil
}

// buildPredicate lowers a filter object. Sibling keys conjoin.
func (l *lowerer) buildPredicate(spec qvalue, where loc) (Predicate, error) {
	if spec.kind() != qObject {
		return Predicate{}, fmt.Errorf(
			"query: %s: a filter is an object, not %s", where, spec.describeKind())
	}
	conjuncts := make([]Predicate, 0, spec.length())
	err := spec.fields(&l.scratch, func(key qkey, value qvalue) error {
		predicate, err := l.buildFilterEntry(key, value, where)
		if err != nil {
			return err
		}
		conjuncts = append(conjuncts, predicate)
		return nil
	})
	if err != nil {
		return Predicate{}, err
	}
	if len(conjuncts) == 1 {
		return conjuncts[0], nil
	}
	return And(conjuncts...), nil
}

// buildFilterEntry lowers one filter entry: a boolean combinator, or a path
// constrained by a literal or an operator object.
func (l *lowerer) buildFilterEntry(key qkey, value qvalue, where loc) (Predicate, error) {
	switch {
	case key.equals("$and"), key.equals("$or"):
		operands, err := l.buildPredicateList(value, where.field(key))
		if err != nil {
			return Predicate{}, err
		}
		if key.equals("$and") {
			return And(operands...), nil
		}
		return Or(operands...), nil
	case key.equals("$not"):
		inner, err := l.buildPredicate(value, where.field(key))
		if err != nil {
			return Predicate{}, err
		}
		return Not(inner), nil
	case key.isOperator():
		return Predicate{}, fmt.Errorf(
			"query: %s: unknown operator %q: expected $and, $or, or $not at filter level", where, key)
	}
	return l.buildPathFilter(key.owned(), value, where.field(key))
}

// buildPredicateList lowers the operand array of $and or $or.
func (l *lowerer) buildPredicateList(spec qvalue, where loc) ([]Predicate, error) {
	if spec.kind() != qArray {
		return nil, fmt.Errorf("query: %s: expected an array of filters, not %s", where, spec.describeKind())
	}
	operands := make([]Predicate, 0, spec.length())
	err := spec.elements(func(index int, element qvalue) error {
		predicate, err := l.buildPredicate(element, where.elem(index))
		if err != nil {
			return err
		}
		operands = append(operands, predicate)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return operands, nil
}

// buildPathFilter lowers the constraint on one path: a bare literal is
// equality (null is the null test), an operator object is the conjunction of
// its operators.
func (l *lowerer) buildPathFilter(path string, spec qvalue, where loc) (Predicate, error) {
	switch spec.kind() {
	case qObject:
		if spec.length() == 0 {
			return Predicate{}, fmt.Errorf("query: %s: an operator object needs at least one operator", where)
		}
		conjuncts := make([]Predicate, 0, spec.length())
		err := spec.fields(&l.scratch, func(key qkey, value qvalue) error {
			if !key.isOperator() {
				return fmt.Errorf(
					"query: %s: %q is not an operator; address a nested field by its path (%q) "+
						"or test structure with $contains", where, key, path+"."+key.owned())
			}
			predicate, err := l.buildOperator(path, key, value, where.field(key))
			if err != nil {
				return err
			}
			conjuncts = append(conjuncts, predicate)
			return nil
		})
		if err != nil {
			return Predicate{}, err
		}
		if len(conjuncts) == 1 {
			return conjuncts[0], nil
		}
		return And(conjuncts...), nil
	case qArray:
		return Predicate{}, fmt.Errorf(
			"query: %s: match one of several values with $in, or an array value with $contains", where)
	case qNull:
		return IsNull(path), nil
	}
	value, err := l.comparableLiteral(spec, where)
	if err != nil {
		return Predicate{}, err
	}
	return Cmp(path, Eq, value), nil
}

// buildOperator lowers one $-prefixed operator applied to path.
func (l *lowerer) buildOperator(path string, name qkey, arg qvalue, where loc) (Predicate, error) {
	switch {
	case name.equals("$eq"), name.equals("$ne"):
		if arg.isNull() {
			if name.equals("$eq") {
				return IsNull(path), nil
			}
			return Not(IsNull(path)), nil
		}
		value, err := l.comparableLiteral(arg, where)
		if err != nil {
			return Predicate{}, err
		}
		if name.equals("$eq") {
			return Cmp(path, Eq, value), nil
		}
		return Cmp(path, Ne, value), nil

	case name.equals("$lt"), name.equals("$lte"), name.equals("$gt"), name.equals("$gte"):
		value, err := l.comparableLiteral(arg, where)
		if err != nil {
			return Predicate{}, err
		}
		var op Op
		switch {
		case name.equals("$lt"):
			op = Lt
		case name.equals("$lte"):
			op = Le
		case name.equals("$gt"):
			op = Gt
		default:
			op = Ge
		}
		return Cmp(path, op, value), nil

	case name.equals("$in"), name.equals("$nin"):
		if arg.kind() != qArray {
			return Predicate{}, fmt.Errorf(
				"query: %s: %s takes an array, not %s", where, name, arg.describeKind())
		}
		// The comparable alternatives become one In, whose sorted set the
		// executor binary-searches. A null alternative is not a comparison at
		// all — it is the null test — so it joins as a separate disjunct
		// rather than degrading the membership into a chain of equalities.
		values := make([]any, 0, arg.length())
		nullable := false
		err := arg.elements(func(index int, element qvalue) error {
			if element.isNull() {
				nullable = true
				return nil
			}
			value, err := l.comparableLiteral(element, where.elem(index))
			if err != nil {
				return err
			}
			values = append(values, value)
			return nil
		})
		if err != nil {
			return Predicate{}, err
		}
		var membership Predicate
		switch {
		case !nullable:
			membership = In(path, values...)
		case len(values) == 0:
			membership = IsNull(path)
		default:
			membership = Or(In(path, values...), IsNull(path))
		}
		if name.equals("$in") {
			return membership, nil
		}
		return Not(membership), nil

	case name.equals("$exists"):
		present, ok := arg.boolean()
		if !ok {
			return Predicate{}, fmt.Errorf(
				"query: %s: $exists takes true or false, not %s", where, arg.describeKind())
		}
		if present {
			return Exists(path), nil
		}
		return Not(Exists(path)), nil

	case name.equals("$null"):
		isNull, ok := arg.boolean()
		if !ok {
			return Predicate{}, fmt.Errorf(
				"query: %s: $null takes true or false, not %s", where, arg.describeKind())
		}
		if isNull {
			return IsNull(path), nil
		}
		return Not(IsNull(path)), nil

	case name.equals("$contains"):
		// A parsed needle is already JSON text in the document, so this copies
		// it rather than re-serializing it; a literal needle is rendered.
		needle, err := arg.rawJSON(l.needle[:0], where.String())
		if err != nil {
			return Predicate{}, err
		}
		l.needle = needle
		return Contains(path, string(needle)), nil

	case name.equals("$not"):
		inner, err := l.buildPathFilter(path, arg, where)
		if err != nil {
			return Predicate{}, err
		}
		return Not(inner), nil

	default:
		return Predicate{}, fmt.Errorf(
			"query: %s: unknown operator %q: expected $eq, $ne, $lt, $lte, $gt, $gte, $in, $nin, "+
				"$exists, $null, $contains, or $not", where, name)
	}
}

// comparableLiteral narrows a document value to one a comparison accepts: a
// string, a bool, or a number. Containers and null are rejected here so the
// caller sees a message naming the operator that would have accepted them.
func (l *lowerer) comparableLiteral(spec qvalue, where loc) (any, error) {
	switch spec.kind() {
	case qBool, qString, qNumber:
		value, ok := spec.literal(&l.scratch)
		if !ok {
			return nil, fmt.Errorf("query: %s: unreadable literal", where)
		}
		if number, isNumber := value.(Number); isNumber {
			if err := number.validate(where.String()); err != nil {
				return nil, err
			}
		}
		return value, nil
	case qNull:
		return nil, fmt.Errorf("query: %s: compare against null with $null or $exists", where)
	case qObject:
		return nil, fmt.Errorf("query: %s: compare against an object with $contains", where)
	case qArray:
		return nil, fmt.Errorf("query: %s: compare against several values with $in", where)
	default:
		return nil, fmt.Errorf("query: %s: unsupported literal type %s", where, spec.describeKind())
	}
}

// validate reports whether n is one JSON number. Parse only ever produces
// valid spellings; a hand-written Number is checked here so a malformed
// literal fails at compile rather than silently never matching.
func (n Number) validate(where string) error {
	text := string(n)
	if text == "" || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) ||
		!vibejson.Valid([]byte(text)) {
		return fmt.Errorf("query: %s: %q is not a JSON number", where, text)
	}
	return nil
}

// buildPaths lowers a clause that names one or more paths.
func (l *lowerer) buildPaths(spec qvalue, where loc) ([]string, error) {
	if path, ok := spec.text(&l.scratch); ok {
		return []string{path}, nil
	}
	if spec.kind() != qArray {
		return nil, fmt.Errorf(
			"query: %s: expected a path or an array of paths, not %s", where, spec.describeKind())
	}
	paths := make([]string, 0, spec.length())
	err := spec.elements(func(index int, element qvalue) error {
		path, ok := element.text(&l.scratch)
		if !ok {
			return fmt.Errorf(
				"query: %s: expected a path string, not %s", where.elem(index), element.describeKind())
		}
		paths = append(paths, path)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return paths, nil
}

// buildOrderBy lowers the sort clause: a path sorts ascending, and an object
// pairs a path with a direction.
func (l *lowerer) buildOrderBy(q *Query, spec qvalue) error {
	if spec.kind() != qArray {
		return l.buildOrderKey(q, spec, at("orderBy"))
	}
	return spec.elements(func(index int, element qvalue) error {
		return l.buildOrderKey(q, element, at("orderBy").elem(index))
	})
}

// buildOrderKey lowers one sort key.
func (l *lowerer) buildOrderKey(q *Query, spec qvalue, where loc) error {
	if path, ok := spec.text(&l.scratch); ok {
		q.OrderBy(path, Asc)
		return nil
	}
	name, value, ok := spec.onlyField(&l.scratch)
	if !ok {
		return fmt.Errorf(
			"query: %s: expected a path string or a one-entry {path: direction} object", where)
	}
	direction, err := l.buildDirection(value, where.field(name))
	if err != nil {
		return err
	}
	q.OrderBy(name.owned(), direction)
	return nil
}

// buildDirection reads a sort direction as "asc"/"desc" or as 1/-1.
func (l *lowerer) buildDirection(spec qvalue, where loc) (Direction, error) {
	if text, ok := spec.text(&l.scratch); ok {
		switch strings.ToLower(text) {
		case "asc", "ascending":
			return Asc, nil
		case "desc", "descending":
			return Desc, nil
		}
		return Asc, fmt.Errorf(
			"query: %s: unknown sort direction %q: expected \"asc\" or \"desc\"", where, text)
	}
	n, ok := spec.wholeNumberOf()
	if !ok {
		return Asc, fmt.Errorf(
			"query: %s: expected \"asc\", \"desc\", 1, or -1, not %s", where, spec.describeKind())
	}
	switch n {
	case 1:
		return Asc, nil
	case -1:
		return Desc, nil
	}
	return Asc, fmt.Errorf("query: %s: expected 1 or -1 for a numeric sort direction, not %d", where, n)
}

// wholeNumber narrows a Go numeric literal to an integer.
func wholeNumber(spec any) (int64, bool) {
	switch value := spec.(type) {
	case int:
		return int64(value), true
	case int8:
		return int64(value), true
	case int16:
		return int64(value), true
	case int32:
		return int64(value), true
	case int64:
		return value, true
	case uint:
		// Widened before comparing: uint is 32 bits on some platforms, where
		// the untyped bound would overflow it and fail to compile.
		return int64(value), uint64(value) <= 1<<62
	case uint8:
		return int64(value), true
	case uint16:
		return int64(value), true
	case uint32:
		return int64(value), true
	case uint64:
		return int64(value), value <= 1<<62
	case float32:
		return int64(value), float64(value) == float64(int64(value))
	case float64:
		return int64(value), value == float64(int64(value))
	case Number:
		n, err := strconv.ParseInt(string(value), 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}

// appendJSONLiteral renders a Go literal as JSON text, the form [Contains]
// takes. Numbers keep their exact spelling.
func appendJSONLiteral(dst []byte, spec any, where string) ([]byte, error) {
	switch value := spec.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, value), nil
	case string:
		return appendJSONString(dst, value), nil
	case Number:
		if err := value.validate(where); err != nil {
			return nil, err
		}
		return append(dst, value...), nil
	}
	if object, ok := asObject(spec); ok {
		dst = append(dst, '{')
		for i, key := range sortedKeys(object) {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJSONString(dst, key)
			dst = append(dst, ':')
			var err error
			dst, err = appendJSONLiteral(dst, object[key], where)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, '}'), nil
	}
	if elements, ok := asArray(spec); ok {
		dst = append(dst, '[')
		for i, element := range elements {
			if i > 0 {
				dst = append(dst, ',')
			}
			var err error
			dst, err = appendJSONLiteral(dst, element, where)
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	}
	if n, ok := numericText(spec); ok {
		return append(dst, n...), nil
	}
	return nil, fmt.Errorf("query: %s: unsupported JSON literal type %T", where, spec)
}

// numericText renders a Go numeric value as its exact JSON spelling.
func numericText(spec any) (string, bool) {
	switch value := spec.(type) {
	case int:
		return strconv.FormatInt(int64(value), 10), true
	case int8:
		return strconv.FormatInt(int64(value), 10), true
	case int16:
		return strconv.FormatInt(int64(value), 10), true
	case int32:
		return strconv.FormatInt(int64(value), 10), true
	case int64:
		return strconv.FormatInt(value, 10), true
	case uint:
		return strconv.FormatUint(uint64(value), 10), true
	case uint8:
		return strconv.FormatUint(uint64(value), 10), true
	case uint16:
		return strconv.FormatUint(uint64(value), 10), true
	case uint32:
		return strconv.FormatUint(uint64(value), 10), true
	case uint64:
		return strconv.FormatUint(value, 10), true
	case float32:
		return strconv.FormatFloat(float64(value), 'g', -1, 32), true
	case float64:
		return strconv.FormatFloat(value, 'g', -1, 64), true
	default:
		return "", false
	}
}

// asObject accepts both M and a plain map[string]any, so a document decoded by
// another JSON package needs no conversion.
func asObject(spec any) (M, bool) {
	switch value := spec.(type) {
	case M:
		return value, true
	case map[string]any:
		return M(value), true
	default:
		return nil, false
	}
}

// asArray accepts both A and a plain []any.
func asArray(spec any) (A, bool) {
	switch value := spec.(type) {
	case A:
		return value, true
	case []any:
		return A(value), true
	default:
		return nil, false
	}
}

// sortedKeys orders a literal object's keys so a document built from a Go map
// lowers to the same predicate tree on every run. Conjunction and disjunction
// are commutative, so the order is free to choose and worth fixing. A parsed
// document needs none of this: its member order is the document's own.
func sortedKeys(object M) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
