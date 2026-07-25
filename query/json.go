package query

import (
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/thesyncim/vibejson"
	"github.com/thesyncim/vibejson/document"
)

// The JSON document front end. A query is itself a JSON object, so the same
// text that travels over a wire or sits in a config file compiles to the same
// immutable [Plan] the builder and the SQL subset produce. [New] takes that
// object as Go literals; [Parse] takes it as JSON bytes. Neither retains the
// document: it is lowered to Columns and Predicates and discarded.
//
//	{
//	  "select":  ["profile.country", {"total": {"$sum": "score"}}],
//	  "where":   {"tenant": "acme", "score": {"$gte": 5}},
//	  "groupBy": ["profile.country"],
//	  "orderBy": [{"total": "desc"}],
//	  "limit":   20
//	}
//
// Sibling keys of an object conjoin, so the common all-of filter needs no
// explicit operator. Every clause accepts a bare scalar wherever it accepts a
// one-element array, so "groupBy": "team" and "groupBy": ["team"] agree.
//
// Paths are the same spec the builder's [Path] takes: dotted ("profile.country")
// or an RFC 6901 pointer ("/profile/country").

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

// New compiles a query document written as Go literals. A malformed document
// or a document that violates a plan rule is reported here rather than at Run.
//
//	q, err := query.New(query.M{
//		"select": query.A{"team", query.M{"n": query.M{"$count": nil}}},
//		"where":  query.M{"active": true, "score": query.M{"$gte": 5}},
//		"groupBy": "team",
//	})
func New(doc M) (*Query, error) {
	q, err := buildQuery(doc)
	if err != nil {
		return nil, err
	}
	if _, err := q.compiled(); err != nil {
		return nil, err
	}
	return q, nil
}

// Parse compiles a query document from JSON text. It is exactly New over the
// parsed document, with every number preserved as its original spelling, so a
// query that arrives as bytes and one written as Go literals compile to the
// same plan.
func Parse(src []byte) (*Query, error) {
	value, err := vibejson.Parse(src)
	if err != nil {
		return nil, err
	}
	doc, err := documentValue(value, "")
	if err != nil {
		return nil, err
	}
	object, ok := asObject(doc)
	if !ok {
		return nil, fmt.Errorf("query: a query document must be a JSON object")
	}
	return New(object)
}

// documentValue converts a parsed value into the M/A literal space, keeping
// each number's exact spelling.
func documentValue(v vibejson.Value, at string) (any, error) {
	switch v.Kind() {
	case document.Object:
		members, _ := v.Object()
		out := make(M, len(members))
		for _, member := range members {
			child, err := documentValue(member.Value, join(at, member.Key))
			if err != nil {
				return nil, err
			}
			out[member.Key] = child
		}
		return out, nil
	case document.Array:
		elements, _ := v.Array()
		out := make(A, 0, len(elements))
		for i, element := range elements {
			child, err := documentValue(element, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
			out = append(out, child)
		}
		return out, nil
	case document.String:
		text, _ := v.Text()
		return text, nil
	case document.Number:
		text, _ := v.NumberText()
		return Number(text), nil
	case document.Bool:
		b, _ := v.Bool()
		return b, nil
	case document.Null:
		return nil, nil
	default:
		return nil, fmt.Errorf("query: %s: unsupported JSON value", describe(at))
	}
}

// buildQuery lowers a query document onto the builder's own representation.
func buildQuery(doc M) (*Query, error) {
	for key := range doc {
		switch key {
		case "select", "where", "groupBy", "orderBy", "limit":
		default:
			return nil, fmt.Errorf(
				"query: unknown query clause %q: expected select, where, groupBy, orderBy, or limit", key)
		}
	}

	columns, err := buildSelect(doc["select"])
	if err != nil {
		return nil, err
	}
	q := Select(columns...)

	if where, ok := doc["where"]; ok && where != nil {
		predicate, err := buildPredicate(where, "where")
		if err != nil {
			return nil, err
		}
		q.Where(predicate)
	}

	if group, ok := doc["groupBy"]; ok && group != nil {
		paths, err := buildPaths(group, "groupBy")
		if err != nil {
			return nil, err
		}
		q.GroupBy(paths...)
	}

	if order, ok := doc["orderBy"]; ok && order != nil {
		if err := buildOrderBy(q, order); err != nil {
			return nil, err
		}
	}

	if limit, ok := doc["limit"]; ok && limit != nil {
		n, err := buildLimit(limit)
		if err != nil {
			return nil, err
		}
		q.Limit(n)
	}
	return q, nil
}

// wholeDocument is the default projection: the row itself, the shape a
// document store returns when a query names no columns.
var wholeDocument = Column{agg: aggNone, spec: "", header: "*"}

// buildSelect lowers the select clause. An absent clause projects the whole
// document.
func buildSelect(spec any) ([]Column, error) {
	if spec == nil {
		return []Column{wholeDocument}, nil
	}
	elements, ok := asArray(spec)
	if !ok {
		column, err := buildColumn(spec, "select")
		if err != nil {
			return nil, err
		}
		return []Column{column}, nil
	}
	if len(elements) == 0 {
		return nil, fmt.Errorf("query: select: an empty list projects nothing; omit the clause for the whole document")
	}
	columns := make([]Column, 0, len(elements))
	for i, element := range elements {
		column, err := buildColumn(element, fmt.Sprintf("select[%d]", i))
		if err != nil {
			return nil, err
		}
		columns = append(columns, column)
	}
	return columns, nil
}

// buildColumn lowers one select entry: a bare path, an aggregate, or either of
// those under an output name.
func buildColumn(spec any, at string) (Column, error) {
	if path, ok := spec.(string); ok {
		return Path(path), nil
	}
	object, ok := asObject(spec)
	if !ok {
		return Column{}, fmt.Errorf(
			"query: %s: a column is a path string or an object, not %s", describe(at), kindOf(spec))
	}
	if len(object) != 1 {
		return Column{}, fmt.Errorf(
			"query: %s: a column object holds exactly one entry, an aggregate or an output name, not %d",
			describe(at), len(object))
	}
	for key, value := range object {
		if strings.HasPrefix(key, "$") {
			return buildAggregate(key, value, at)
		}
		named, err := buildNamedColumn(value, join(at, key))
		if err != nil {
			return Column{}, err
		}
		named.header = key
		return named, nil
	}
	panic("unreachable: one-entry object")
}

// buildNamedColumn lowers the value side of an aliased column.
func buildNamedColumn(spec any, at string) (Column, error) {
	if path, ok := spec.(string); ok {
		return Path(path), nil
	}
	object, ok := asObject(spec)
	if !ok || len(object) != 1 {
		return Column{}, fmt.Errorf(
			"query: %s: a named column is a path string or a one-entry aggregate object", describe(at))
	}
	for key, value := range object {
		if !strings.HasPrefix(key, "$") {
			return Column{}, fmt.Errorf(
				"query: %s: %q is not an aggregate; aggregate names begin with $", describe(at), key)
		}
		return buildAggregate(key, value, at)
	}
	panic("unreachable: one-entry object")
}

// buildAggregate lowers one $-prefixed reduction.
func buildAggregate(op string, arg any, at string) (Column, error) {
	if op == "$count" {
		switch value := arg.(type) {
		case nil:
			return Count(), nil
		case bool:
			if !value {
				return Column{}, fmt.Errorf("query: %s: $count takes true, a path, or null", describe(at))
			}
			return Count(), nil
		case string:
			if value == "*" {
				return Count(), nil
			}
			return Count(value), nil
		default:
			if object, ok := asObject(arg); ok && len(object) == 0 {
				return Count(), nil
			}
			return Column{}, fmt.Errorf(
				"query: %s: $count takes a path, \"*\", {}, true, or null, not %s", describe(at), kindOf(arg))
		}
	}

	var build func(string) Column
	switch op {
	case "$sum":
		build = Sum
	case "$avg":
		build = Avg
	case "$min":
		build = Min
	case "$max":
		build = Max
	default:
		return Column{}, fmt.Errorf(
			"query: %s: unknown aggregate %q: expected $count, $sum, $avg, $min, or $max", describe(at), op)
	}
	path, ok := arg.(string)
	if !ok {
		return Column{}, fmt.Errorf(
			"query: %s: %s takes a path string, not %s", describe(at), op, kindOf(arg))
	}
	return build(path), nil
}

// buildPredicate lowers a filter object. Sibling keys conjoin.
func buildPredicate(spec any, at string) (Predicate, error) {
	object, ok := asObject(spec)
	if !ok {
		return Predicate{}, fmt.Errorf(
			"query: %s: a filter is an object, not %s", describe(at), kindOf(spec))
	}
	conjuncts := make([]Predicate, 0, len(object))
	for _, key := range sortedKeys(object) {
		predicate, err := buildFilterEntry(key, object[key], at)
		if err != nil {
			return Predicate{}, err
		}
		conjuncts = append(conjuncts, predicate)
	}
	if len(conjuncts) == 1 {
		return conjuncts[0], nil
	}
	return And(conjuncts...), nil
}

// buildFilterEntry lowers one filter entry: a boolean combinator, or a path
// constrained by a literal or an operator object.
func buildFilterEntry(key string, value any, at string) (Predicate, error) {
	where := join(at, key)
	switch key {
	case "$and", "$or":
		operands, err := buildPredicateList(value, where)
		if err != nil {
			return Predicate{}, err
		}
		if key == "$and" {
			return And(operands...), nil
		}
		return Or(operands...), nil
	case "$not":
		inner, err := buildPredicate(value, where)
		if err != nil {
			return Predicate{}, err
		}
		return Not(inner), nil
	}
	if strings.HasPrefix(key, "$") {
		return Predicate{}, fmt.Errorf(
			"query: %s: unknown operator %q: expected $and, $or, or $not at filter level", describe(at), key)
	}
	return buildPathFilter(key, value, where)
}

// buildPredicateList lowers the operand array of $and or $or.
func buildPredicateList(spec any, at string) ([]Predicate, error) {
	elements, ok := asArray(spec)
	if !ok {
		return nil, fmt.Errorf("query: %s: expected an array of filters, not %s", describe(at), kindOf(spec))
	}
	operands := make([]Predicate, 0, len(elements))
	for i, element := range elements {
		predicate, err := buildPredicate(element, fmt.Sprintf("%s[%d]", at, i))
		if err != nil {
			return nil, err
		}
		operands = append(operands, predicate)
	}
	return operands, nil
}

// buildPathFilter lowers the constraint on one path: a bare literal is
// equality (null is the null test), an operator object is the conjunction of
// its operators.
func buildPathFilter(path string, spec any, at string) (Predicate, error) {
	if object, ok := asObject(spec); ok {
		operators := sortedKeys(object)
		if len(operators) == 0 {
			return Predicate{}, fmt.Errorf("query: %s: an operator object needs at least one operator", describe(at))
		}
		for _, name := range operators {
			if !strings.HasPrefix(name, "$") {
				return Predicate{}, fmt.Errorf(
					"query: %s: %q is not an operator; address a nested field by its path (%q) "+
						"or test structure with $contains", describe(at), name, path+"."+name)
			}
		}
		conjuncts := make([]Predicate, 0, len(operators))
		for _, name := range operators {
			predicate, err := buildOperator(path, name, object[name], join(at, name))
			if err != nil {
				return Predicate{}, err
			}
			conjuncts = append(conjuncts, predicate)
		}
		if len(conjuncts) == 1 {
			return conjuncts[0], nil
		}
		return And(conjuncts...), nil
	}
	if _, ok := asArray(spec); ok {
		return Predicate{}, fmt.Errorf(
			"query: %s: match one of several values with $in, or an array value with $contains", describe(at))
	}
	if spec == nil {
		return IsNull(path), nil
	}
	literal, err := comparableLiteral(spec, at)
	if err != nil {
		return Predicate{}, err
	}
	return Cmp(path, Eq, literal), nil
}

// buildOperator lowers one $-prefixed operator applied to path.
func buildOperator(path, name string, arg any, at string) (Predicate, error) {
	switch name {
	case "$eq", "$ne":
		if arg == nil {
			if name == "$eq" {
				return IsNull(path), nil
			}
			return Not(IsNull(path)), nil
		}
		literal, err := comparableLiteral(arg, at)
		if err != nil {
			return Predicate{}, err
		}
		if name == "$eq" {
			return Cmp(path, Eq, literal), nil
		}
		return Cmp(path, Ne, literal), nil

	case "$lt", "$lte", "$gt", "$gte":
		literal, err := comparableLiteral(arg, at)
		if err != nil {
			return Predicate{}, err
		}
		ops := map[string]Op{"$lt": Lt, "$lte": Le, "$gt": Gt, "$gte": Ge}
		return Cmp(path, ops[name], literal), nil

	case "$in", "$nin":
		elements, ok := asArray(arg)
		if !ok {
			return Predicate{}, fmt.Errorf("query: %s: %s takes an array, not %s", describe(at), name, kindOf(arg))
		}
		// The comparable alternatives become one In, whose sorted set the
		// executor binary-searches. A null alternative is not a comparison at
		// all — it is the null test — so it joins as a separate disjunct
		// rather than degrading the membership into a chain of equalities.
		values := make([]any, 0, len(elements))
		nullable := false
		for i, element := range elements {
			if element == nil {
				nullable = true
				continue
			}
			literal, err := comparableLiteral(element, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return Predicate{}, err
			}
			values = append(values, literal)
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
		if name == "$in" {
			return membership, nil
		}
		return Not(membership), nil

	case "$exists":
		present, ok := arg.(bool)
		if !ok {
			return Predicate{}, fmt.Errorf("query: %s: $exists takes true or false, not %s", describe(at), kindOf(arg))
		}
		if present {
			return Exists(path), nil
		}
		return Not(Exists(path)), nil

	case "$null":
		isNull, ok := arg.(bool)
		if !ok {
			return Predicate{}, fmt.Errorf("query: %s: $null takes true or false, not %s", describe(at), kindOf(arg))
		}
		if isNull {
			return IsNull(path), nil
		}
		return Not(IsNull(path)), nil

	case "$contains":
		needle, err := appendJSONLiteral(nil, arg, at)
		if err != nil {
			return Predicate{}, err
		}
		return Contains(path, string(needle)), nil

	case "$not":
		inner, err := buildPathFilter(path, arg, at)
		if err != nil {
			return Predicate{}, err
		}
		return Not(inner), nil

	default:
		return Predicate{}, fmt.Errorf(
			"query: %s: unknown operator %q: expected $eq, $ne, $lt, $lte, $gt, $gte, $in, $nin, "+
				"$exists, $null, $contains, or $not", describe(at), name)
	}
}

// comparableLiteral narrows a document value to one a comparison accepts:
// a string, a bool, or a number. Containers and null are rejected here so the
// caller sees a message naming the operator that would have accepted them.
func comparableLiteral(spec any, at string) (any, error) {
	switch value := spec.(type) {
	case string, bool:
		return value, nil
	case Number:
		if err := value.validate(at); err != nil {
			return nil, err
		}
		return value, nil
	case int, int8, int16, int32, int64,
		uint, uint8, uint16, uint32, uint64, float32, float64:
		return value, nil
	case nil:
		return nil, fmt.Errorf("query: %s: compare against null with $null or $exists", describe(at))
	default:
		if _, ok := asObject(spec); ok {
			return nil, fmt.Errorf("query: %s: compare against an object with $contains", describe(at))
		}
		if _, ok := asArray(spec); ok {
			return nil, fmt.Errorf("query: %s: compare against several values with $in", describe(at))
		}
		return nil, fmt.Errorf("query: %s: unsupported literal type %T", describe(at), spec)
	}
}

// validate reports whether n is one JSON number. Parse only ever produces
// valid spellings; a hand-written Number is checked here so a malformed
// literal fails at compile rather than silently never matching.
func (n Number) validate(at string) error {
	text := string(n)
	if text == "" || (text[0] != '-' && (text[0] < '0' || text[0] > '9')) ||
		!vibejson.Valid([]byte(text)) {
		return fmt.Errorf("query: %s: %q is not a JSON number", describe(at), text)
	}
	return nil
}

// buildPaths lowers a clause that names one or more paths.
func buildPaths(spec any, at string) ([]string, error) {
	if path, ok := spec.(string); ok {
		return []string{path}, nil
	}
	elements, ok := asArray(spec)
	if !ok {
		return nil, fmt.Errorf("query: %s: expected a path or an array of paths, not %s", describe(at), kindOf(spec))
	}
	paths := make([]string, 0, len(elements))
	for i, element := range elements {
		path, ok := element.(string)
		if !ok {
			return nil, fmt.Errorf("query: %s[%d]: expected a path string, not %s", describe(at), i, kindOf(element))
		}
		paths = append(paths, path)
	}
	return paths, nil
}

// buildOrderBy lowers the sort clause: a path sorts ascending, and an object
// pairs a path with a direction.
func buildOrderBy(q *Query, spec any) error {
	elements, ok := asArray(spec)
	if !ok {
		elements = A{spec}
	}
	for i, element := range elements {
		at := fmt.Sprintf("orderBy[%d]", i)
		if path, ok := element.(string); ok {
			q.OrderBy(path, Asc)
			continue
		}
		object, ok := asObject(element)
		if !ok || len(object) != 1 {
			return fmt.Errorf(
				"query: %s: expected a path string or a one-entry {path: direction} object", describe(at))
		}
		for path, value := range object {
			direction, err := buildDirection(value, join(at, path))
			if err != nil {
				return err
			}
			q.OrderBy(path, direction)
		}
	}
	return nil
}

// buildDirection reads a sort direction as "asc"/"desc" or as 1/-1.
func buildDirection(spec any, at string) (Direction, error) {
	if text, ok := spec.(string); ok {
		switch strings.ToLower(text) {
		case "asc", "ascending":
			return Asc, nil
		case "desc", "descending":
			return Desc, nil
		}
		return Asc, fmt.Errorf("query: %s: unknown sort direction %q: expected \"asc\" or \"desc\"", describe(at), text)
	}
	n, ok := wholeNumber(spec)
	if !ok {
		return Asc, fmt.Errorf(
			"query: %s: expected \"asc\", \"desc\", 1, or -1, not %s", describe(at), kindOf(spec))
	}
	switch n {
	case 1:
		return Asc, nil
	case -1:
		return Desc, nil
	}
	return Asc, fmt.Errorf("query: %s: expected 1 or -1 for a numeric sort direction, not %d", describe(at), n)
}

// buildLimit reads the row cap. A negative limit means unbounded, matching
// [Query.Limit].
func buildLimit(spec any) (int, error) {
	n, ok := wholeNumber(spec)
	if !ok {
		return 0, fmt.Errorf("query: limit: expected a whole number, not %s", kindOf(spec))
	}
	return int(n), nil
}

// wholeNumber narrows a document value to an integer, accepting every Go
// numeric spelling plus an exact Number whose value has no fraction.
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
		return int64(value), value <= 1<<62
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

// appendJSONLiteral renders a document value as JSON text, the form
// [Contains] takes. Numbers keep their exact spelling.
func appendJSONLiteral(dst []byte, spec any, at string) ([]byte, error) {
	switch value := spec.(type) {
	case nil:
		return append(dst, "null"...), nil
	case bool:
		return strconv.AppendBool(dst, value), nil
	case string:
		return appendJSONString(dst, value), nil
	case Number:
		if err := value.validate(at); err != nil {
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
			dst, err = appendJSONLiteral(dst, object[key], join(at, key))
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
			dst, err = appendJSONLiteral(dst, element, fmt.Sprintf("%s[%d]", at, i))
			if err != nil {
				return nil, err
			}
		}
		return append(dst, ']'), nil
	}
	if n, ok := numericText(spec); ok {
		return append(dst, n...), nil
	}
	return nil, fmt.Errorf("query: %s: unsupported JSON literal type %T", describe(at), spec)
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

// sortedKeys orders an object's keys so a document built from a Go map lowers
// to the same predicate tree on every run. Conjunction and disjunction are
// commutative, so the order is free to choose and worth fixing.
func sortedKeys(object M) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}

// join extends a breadcrumb path used in error messages.
func join(at, key string) string {
	if at == "" {
		return key
	}
	return at + "." + key
}

// describe renders a breadcrumb, naming the document root when empty.
func describe(at string) string {
	if at == "" {
		return "query document"
	}
	return at
}

// kindOf names a document value's JSON kind for an error message.
func kindOf(spec any) string {
	switch spec.(type) {
	case nil:
		return "null"
	case string:
		return "a string"
	case bool:
		return "a boolean"
	case Number:
		return "a number"
	}
	if _, ok := asObject(spec); ok {
		return "an object"
	}
	if _, ok := asArray(spec); ok {
		return "an array"
	}
	if _, ok := numericText(spec); ok {
		return "a number"
	}
	return fmt.Sprintf("%T", spec)
}
