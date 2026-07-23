package query

import (
	"fmt"
	"strconv"
	"strings"
)

// The SQL-text front end. Compile lexes and parses the supported subset, then
// lowers it into the same typed Plan as the programmatic builder, so a prepared
// SQL query and its hand-built twin are indistinguishable to the executor. The grammar is
// deliberately basic — one table (the FROM name is accepted and ignored, since
// the table is the DocSet Run is handed), columns that are path projections or
// aggregates, and a WHERE of comparisons, containment, existence, and null
// tests combined with AND/OR/NOT:
//
//	SELECT <col> [, <col>]*
//	FROM   <table>
//	[WHERE <predicate>]
//	[GROUP BY <path> [, <path>]*]
//	[ORDER BY <path> [ASC|DESC] [, <path> [ASC|DESC]]*]
//	[LIMIT <n>]
//
//	<col>       := <path> | COUNT(*) | COUNT(<path>) | SUM|AVG|MIN|MAX(<path>)
//	<predicate> := <predicate> OR <predicate>
//	             | <predicate> AND <predicate>
//	             | NOT <predicate>
//	             | '(' <predicate> ')'
//	             | EXISTS '(' <path> ')'
//	             | <path> IS [NOT] NULL
//	             | <path> '@>' <jsonLiteral>
//	             | <path> ('=' | '!=' | '<>' | '<' | '<=' | '>' | '>=') <literal>
//	<literal>   := number | string | TRUE | FALSE
//	<path>      := <ident> ( '.' <ident> | '[' <index> ']' | '[' <string> ']' )*
//
// Keywords and the boolean literals are case-insensitive. A string literal is
// single-quoted with '' for an embedded quote; the '@>' right-hand side is a
// JSON value in JSON syntax (double-quoted strings), captured verbatim and
// validated when the query compiles. Paths render to the same spec the Path
// builder takes — a bare field name stays a name (the fused columnar path), and
// anything nested or indexed becomes an RFC 6901 pointer — computed identically
// wherever a path appears, so a GROUP BY or ORDER BY path matches its projection.
//
// Compile parses, then compiles the plan once so a malformed query is reported
// eagerly; Run reuses the cached plan and is otherwise unchanged. A syntax error
// is a *ParseError carrying the byte offset; a well-formed query that violates a
// plan rule (a projection absent from GROUP BY, say) returns the executor's
// compile error, exactly as the builder would.

// A ParseError reports a syntax error in a SQL query, with the byte offset in
// the source where the parser stopped.
type ParseError struct {
	// Pos is the byte offset of the offending token in the query text.
	Pos int
	// Msg describes what was expected or what was wrong.
	Msg string
}

// Error formats the parse error with its offset.
func (e *ParseError) Error() string {
	return fmt.Sprintf("query: parse error at offset %d: %s", e.Pos, e.Msg)
}

// Compile parses one SQL query in the supported subset and returns a compiled
// Query equivalent to the builder plan it denotes. A syntax error is a
// *ParseError with the offending offset; a syntactically valid query that fails
// a plan rule returns the same error a Run of the equivalent builder query
// would. The returned Query is compiled and ready to Run over any DocSet.
func Compile(sql string) (*Query, error) {
	p := &parser{lx: lexer{src: sql}}
	p.advance()
	q, err := p.parseQuery()
	if err != nil {
		return nil, err
	}
	if _, err := q.compiled(); err != nil {
		return nil, err
	}
	return q, nil
}

// --- lexer ----------------------------------------------------------------

// tokKind enumerates the lexical tokens. Keywords are not distinct kinds; they
// are identifiers the parser matches case-insensitively, so a field may share a
// keyword's spelling wherever the grammar is unambiguous.
type tokKind uint8

const (
	tEOF tokKind = iota
	tError
	tIdent
	tNumber
	tString
	tStar
	tComma
	tLParen
	tRParen
	tLBracket
	tRBracket
	tDot
	tEq
	tNe
	tLt
	tLe
	tGt
	tGe
	tContains // @>
)

// A token is one lexeme: its kind, its source text (decoded, for a string), and
// the byte offset it began at for error reporting.
type token struct {
	kind tokKind
	text string
	pos  int
}

// lexer scans src left to right. pos is the offset of the next unlexed byte;
// after next returns a token, pos sits just past that token, the invariant the
// parser's raw JSON scan (for '@>') relies on.
type lexer struct {
	src string
	pos int
}

// next returns the next token and advances past it, skipping leading
// whitespace. An unexpected byte yields a tError token carrying the message.
func (lx *lexer) next() token {
	lx.skipSpace()
	start := lx.pos
	if lx.pos >= len(lx.src) {
		return token{kind: tEOF, pos: start}
	}
	c := lx.src[lx.pos]
	switch {
	case isIdentStart(c):
		return lx.lexIdent()
	case c >= '0' && c <= '9':
		return lx.lexNumber()
	case c == '-' && lx.pos+1 < len(lx.src) && lx.src[lx.pos+1] >= '0' && lx.src[lx.pos+1] <= '9':
		return lx.lexNumber()
	case c == '\'' || c == '"':
		return lx.lexString(c)
	}
	lx.pos++
	switch c {
	case '*':
		return token{kind: tStar, pos: start}
	case ',':
		return token{kind: tComma, pos: start}
	case '(':
		return token{kind: tLParen, pos: start}
	case ')':
		return token{kind: tRParen, pos: start}
	case '[':
		return token{kind: tLBracket, pos: start}
	case ']':
		return token{kind: tRBracket, pos: start}
	case '.':
		return token{kind: tDot, pos: start}
	case '=':
		return token{kind: tEq, pos: start}
	case '!':
		if lx.accept('=') {
			return token{kind: tNe, pos: start}
		}
		return errTok(start, "expected '=' after '!'")
	case '<':
		if lx.accept('=') {
			return token{kind: tLe, pos: start}
		}
		if lx.accept('>') {
			return token{kind: tNe, pos: start}
		}
		return token{kind: tLt, pos: start}
	case '>':
		if lx.accept('=') {
			return token{kind: tGe, pos: start}
		}
		return token{kind: tGt, pos: start}
	case '@':
		if lx.accept('>') {
			return token{kind: tContains, pos: start}
		}
		return errTok(start, "expected '>' after '@'")
	}
	return errTok(start, fmt.Sprintf("unexpected character %q", string(c)))
}

func (lx *lexer) skipSpace() {
	for lx.pos < len(lx.src) {
		switch lx.src[lx.pos] {
		case ' ', '\t', '\n', '\r':
			lx.pos++
		default:
			return
		}
	}
}

// accept consumes the next byte if it equals c.
func (lx *lexer) accept(c byte) bool {
	if lx.pos < len(lx.src) && lx.src[lx.pos] == c {
		lx.pos++
		return true
	}
	return false
}

func (lx *lexer) lexIdent() token {
	start := lx.pos
	for lx.pos < len(lx.src) && isIdentPart(lx.src[lx.pos]) {
		lx.pos++
	}
	return token{kind: tIdent, text: lx.src[start:lx.pos], pos: start}
}

// lexNumber scans a JSON-shaped numeric literal: an optional sign, an integer
// part, an optional fraction, and an optional exponent. Its text is validated
// when parsed into a literal value.
func (lx *lexer) lexNumber() token {
	start := lx.pos
	lx.accept('-')
	for lx.pos < len(lx.src) && lx.src[lx.pos] >= '0' && lx.src[lx.pos] <= '9' {
		lx.pos++
	}
	if lx.accept('.') {
		for lx.pos < len(lx.src) && lx.src[lx.pos] >= '0' && lx.src[lx.pos] <= '9' {
			lx.pos++
		}
	}
	if lx.pos < len(lx.src) && (lx.src[lx.pos] == 'e' || lx.src[lx.pos] == 'E') {
		lx.pos++
		if lx.pos < len(lx.src) && (lx.src[lx.pos] == '+' || lx.src[lx.pos] == '-') {
			lx.pos++
		}
		for lx.pos < len(lx.src) && lx.src[lx.pos] >= '0' && lx.src[lx.pos] <= '9' {
			lx.pos++
		}
	}
	return token{kind: tNumber, text: lx.src[start:lx.pos], pos: start}
}

// lexString scans a quoted string closed by quote. A doubled quote is one
// literal quote; every other byte is taken verbatim. An unterminated string is
// an error.
func (lx *lexer) lexString(quote byte) token {
	start := lx.pos
	lx.pos++ // opening quote
	var b strings.Builder
	for lx.pos < len(lx.src) {
		c := lx.src[lx.pos]
		if c == quote {
			if lx.pos+1 < len(lx.src) && lx.src[lx.pos+1] == quote {
				b.WriteByte(quote)
				lx.pos += 2
				continue
			}
			lx.pos++ // closing quote
			return token{kind: tString, text: b.String(), pos: start}
		}
		b.WriteByte(c)
		lx.pos++
	}
	return errTok(start, "unterminated string literal")
}

func errTok(pos int, msg string) token { return token{kind: tError, text: msg, pos: pos} }

func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

func isIdentPart(c byte) bool { return isIdentStart(c) || (c >= '0' && c <= '9') }

// --- parser ---------------------------------------------------------------

// parser is a recursive-descent parser over a single current token. lx.pos
// always sits just past tok, so parseContainsNeedle can scan a raw JSON value
// straight from the source after consuming '@>'.
type parser struct {
	lx  lexer
	tok token
}

// advance loads the next token, propagating a lexer error the parser surfaces
// on its next expectation.
func (p *parser) advance() { p.tok = p.lx.next() }

// errAt builds a ParseError at pos.
func (p *parser) errAt(pos int, format string, args ...any) error {
	return &ParseError{Pos: pos, Msg: fmt.Sprintf(format, args...)}
}

// errHere builds a ParseError at the current token, reporting a lexer error's
// own message when the current token is one.
func (p *parser) errHere(format string, args ...any) error {
	if p.tok.kind == tError {
		return &ParseError{Pos: p.tok.pos, Msg: p.tok.text}
	}
	return &ParseError{Pos: p.tok.pos, Msg: fmt.Sprintf(format, args...)}
}

// isKeyword reports whether the current token is the given keyword, matched
// case-insensitively.
func (p *parser) isKeyword(kw string) bool {
	return p.tok.kind == tIdent && strings.EqualFold(p.tok.text, kw)
}

// acceptKeyword consumes the current token when it is the keyword kw.
func (p *parser) acceptKeyword(kw string) bool {
	if p.isKeyword(kw) {
		p.advance()
		return true
	}
	return false
}

// expectKeyword consumes kw or reports a parse error.
func (p *parser) expectKeyword(kw string) error {
	if !p.acceptKeyword(kw) {
		return p.errHere("expected %s", kw)
	}
	return nil
}

// parseQuery parses a whole query and requires the input to end after it.
func (p *parser) parseQuery() (*Query, error) {
	if err := p.expectKeyword("SELECT"); err != nil {
		return nil, err
	}
	cols, err := p.parseColumns()
	if err != nil {
		return nil, err
	}
	if err := p.expectKeyword("FROM"); err != nil {
		return nil, err
	}
	if err := p.parseTableName(); err != nil {
		return nil, err
	}
	q := Select(cols...)

	if p.acceptKeyword("WHERE") {
		pred, err := p.parsePredicate()
		if err != nil {
			return nil, err
		}
		q.Where(pred)
	}
	if p.acceptKeyword("GROUP") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		paths, err := p.parsePathList()
		if err != nil {
			return nil, err
		}
		q.GroupBy(paths...)
	}
	if p.acceptKeyword("ORDER") {
		if err := p.expectKeyword("BY"); err != nil {
			return nil, err
		}
		if err := p.parseOrderBy(q); err != nil {
			return nil, err
		}
	}
	if p.acceptKeyword("LIMIT") {
		if err := p.parseLimit(q); err != nil {
			return nil, err
		}
	}
	if p.tok.kind != tEOF {
		return nil, p.errHere("unexpected trailing input")
	}
	return q, nil
}

// parseTableName consumes the ignored table reference: an identifier, optionally
// dotted (schema.table). The table Run reads is the DocSet, so the name is not
// retained.
func (p *parser) parseTableName() error {
	if p.tok.kind != tIdent {
		return p.errHere("expected table name")
	}
	p.advance()
	for p.tok.kind == tDot {
		p.advance()
		if p.tok.kind != tIdent {
			return p.errHere("expected identifier after '.'")
		}
		p.advance()
	}
	return nil
}

// parseColumns parses the comma-separated SELECT list.
func (p *parser) parseColumns() ([]Column, error) {
	var cols []Column
	for {
		col, err := p.parseColumn()
		if err != nil {
			return nil, err
		}
		cols = append(cols, col)
		if p.tok.kind != tComma {
			return cols, nil
		}
		p.advance()
	}
}

// aggNames maps the aggregate keywords to a small builder for the parsed spec.
var aggNames = map[string]func(string) Column{
	"SUM": Sum,
	"AVG": Avg,
	"MIN": Min,
	"MAX": Max,
}

// parseColumn parses one SELECT column: an aggregate call when an aggregate
// keyword is immediately followed by '(', otherwise a path projection (so a
// field named like an aggregate still projects).
func (p *parser) parseColumn() (Column, error) {
	if p.tok.kind == tStar {
		return Column{}, p.errHere("SELECT * is not supported; list explicit paths or aggregates")
	}
	if p.tok.kind != tIdent {
		return Column{}, p.errHere("expected a column")
	}
	name := p.tok.text
	upper := strings.ToUpper(name)
	// COUNT and the numeric aggregates are calls only when a '(' follows; the
	// identifier is otherwise the first segment of a projected path.
	if upper == "COUNT" || aggNames[upper] != nil {
		p.advance()
		if p.tok.kind == tLParen {
			return p.parseAggregate(upper)
		}
		spec, err := p.continuePath(name)
		if err != nil {
			return Column{}, err
		}
		return Path(spec), nil
	}
	spec, err := p.parsePath()
	if err != nil {
		return Column{}, err
	}
	return Path(spec), nil
}

// parseAggregate parses the argument list of an aggregate whose name keyword was
// just consumed and whose current token is '('.
func (p *parser) parseAggregate(name string) (Column, error) {
	p.advance() // '('
	if name == "COUNT" {
		if p.tok.kind == tStar {
			p.advance()
			if err := p.expect(tRParen, "')'"); err != nil {
				return Column{}, err
			}
			return Count(), nil
		}
		spec, err := p.parsePath()
		if err != nil {
			return Column{}, err
		}
		if err := p.expect(tRParen, "')'"); err != nil {
			return Column{}, err
		}
		return Count(spec), nil
	}
	build := aggNames[name]
	spec, err := p.parsePath()
	if err != nil {
		return Column{}, err
	}
	if err := p.expect(tRParen, "')'"); err != nil {
		return Column{}, err
	}
	return build(spec), nil
}

// expect consumes the current token when it is kind, or reports a parse error
// naming what was wanted.
func (p *parser) expect(kind tokKind, what string) error {
	if p.tok.kind != kind {
		return p.errHere("expected %s", what)
	}
	p.advance()
	return nil
}

// --- paths ----------------------------------------------------------------

// pathSeg is one parsed path segment: an object key, or an array index.
type pathSeg struct {
	key     string
	index   int
	isIndex bool
}

// parsePathList parses a comma-separated list of paths (GROUP BY).
func (p *parser) parsePathList() ([]string, error) {
	var paths []string
	for {
		spec, err := p.parsePath()
		if err != nil {
			return nil, err
		}
		paths = append(paths, spec)
		if p.tok.kind != tComma {
			return paths, nil
		}
		p.advance()
	}
}

// parsePath parses a path beginning at the current identifier.
func (p *parser) parsePath() (string, error) {
	if p.tok.kind != tIdent {
		return "", p.errHere("expected a path")
	}
	first := p.tok.text
	p.advance()
	return p.continuePath(first)
}

// continuePath parses the '.'-segment and '[...]'-index tail of a path whose
// first identifier is already consumed, then renders the whole to a path spec.
func (p *parser) continuePath(first string) (string, error) {
	segs := []pathSeg{{key: first}}
	for {
		switch p.tok.kind {
		case tDot:
			p.advance()
			if p.tok.kind != tIdent {
				return "", p.errHere("expected identifier after '.'")
			}
			segs = append(segs, pathSeg{key: p.tok.text})
			p.advance()
		case tLBracket:
			p.advance()
			seg, err := p.parseBracket()
			if err != nil {
				return "", err
			}
			segs = append(segs, seg)
			if err := p.expect(tRBracket, "']'"); err != nil {
				return "", err
			}
		default:
			return pathSpec(segs), nil
		}
	}
}

// parseBracket parses the content of a '[...]' path accessor: a non-negative
// integer index, or a quoted string key.
func (p *parser) parseBracket() (pathSeg, error) {
	switch p.tok.kind {
	case tNumber:
		n, err := strconv.Atoi(p.tok.text)
		if err != nil || n < 0 {
			return pathSeg{}, p.errHere("array index must be a non-negative integer")
		}
		p.advance()
		return pathSeg{index: n, isIndex: true}, nil
	case tString:
		key := p.tok.text
		p.advance()
		return pathSeg{key: key}, nil
	default:
		return pathSeg{}, p.errHere("expected an index or quoted key inside '[]'")
	}
}

// pathSpec renders parsed segments to the string the Path builder compiles,
// deterministically so every clause naming the same path produces the same
// spec. A lone clean field name stays a dotted name (the fused single-field
// column path); several clean keys join with '.'; an array index or a key
// carrying a '.', '/', or '~' forces an RFC 6901 pointer, whose segments escape
// '~' and '/' per the standard.
func pathSpec(segs []pathSeg) string {
	pointer := false
	for _, s := range segs {
		if s.isIndex || strings.ContainsAny(s.key, "./~") {
			pointer = true
			break
		}
	}
	if !pointer {
		if len(segs) == 1 {
			return segs[0].key
		}
		keys := make([]string, len(segs))
		for i, s := range segs {
			keys[i] = s.key
		}
		return strings.Join(keys, ".")
	}
	var b strings.Builder
	for _, s := range segs {
		b.WriteByte('/')
		if s.isIndex {
			b.WriteString(strconv.Itoa(s.index))
			continue
		}
		b.WriteString(escapePointerToken(s.key))
	}
	return b.String()
}

// escapePointerToken applies RFC 6901's token escapes: '~' -> '~0', '/' -> '~1'.
func escapePointerToken(seg string) string {
	if !strings.ContainsAny(seg, "~/") {
		return seg
	}
	var b strings.Builder
	for i := 0; i < len(seg); i++ {
		switch seg[i] {
		case '~':
			b.WriteString("~0")
		case '/':
			b.WriteString("~1")
		default:
			b.WriteByte(seg[i])
		}
	}
	return b.String()
}

// --- predicates -----------------------------------------------------------

// parsePredicate parses a full WHERE predicate (lowest precedence: OR).
func (p *parser) parsePredicate() (Predicate, error) { return p.parseOr() }

// parseOr parses OR-separated conjunctions.
func (p *parser) parseOr() (Predicate, error) {
	left, err := p.parseAnd()
	if err != nil {
		return Predicate{}, err
	}
	for p.acceptKeyword("OR") {
		right, err := p.parseAnd()
		if err != nil {
			return Predicate{}, err
		}
		left = Or(left, right)
	}
	return left, nil
}

// parseAnd parses AND-separated negations.
func (p *parser) parseAnd() (Predicate, error) {
	left, err := p.parseNot()
	if err != nil {
		return Predicate{}, err
	}
	for p.acceptKeyword("AND") {
		right, err := p.parseNot()
		if err != nil {
			return Predicate{}, err
		}
		left = And(left, right)
	}
	return left, nil
}

// parseNot parses an optional NOT prefix over a primary predicate.
func (p *parser) parseNot() (Predicate, error) {
	if p.acceptKeyword("NOT") {
		inner, err := p.parseNot()
		if err != nil {
			return Predicate{}, err
		}
		return Not(inner), nil
	}
	return p.parsePrimary()
}

// parsePrimary parses a parenthesized predicate, EXISTS, or a path-led leaf
// (IS [NOT] NULL, '@>', or a comparison).
func (p *parser) parsePrimary() (Predicate, error) {
	if p.tok.kind == tLParen {
		p.advance()
		inner, err := p.parseOr()
		if err != nil {
			return Predicate{}, err
		}
		if err := p.expect(tRParen, "')'"); err != nil {
			return Predicate{}, err
		}
		return inner, nil
	}
	if p.acceptKeyword("EXISTS") {
		if err := p.expect(tLParen, "'(' after EXISTS"); err != nil {
			return Predicate{}, err
		}
		spec, err := p.parsePath()
		if err != nil {
			return Predicate{}, err
		}
		if err := p.expect(tRParen, "')'"); err != nil {
			return Predicate{}, err
		}
		return Exists(spec), nil
	}
	spec, err := p.parsePath()
	if err != nil {
		return Predicate{}, err
	}
	return p.parseLeafTail(spec)
}

// parseLeafTail parses what follows a predicate path: IS [NOT] NULL, '@>' with
// a JSON needle, or a comparison operator and literal.
func (p *parser) parseLeafTail(spec string) (Predicate, error) {
	if p.acceptKeyword("IS") {
		negated := p.acceptKeyword("NOT")
		if err := p.expectKeyword("NULL"); err != nil {
			return Predicate{}, err
		}
		if negated {
			return Not(IsNull(spec)), nil
		}
		return IsNull(spec), nil
	}
	if p.tok.kind == tContains {
		needle, err := p.parseContainsNeedle()
		if err != nil {
			return Predicate{}, err
		}
		return Contains(spec, needle), nil
	}
	op, ok := comparisonOp(p.tok.kind)
	if !ok {
		return Predicate{}, p.errHere("expected a comparison operator, IS, or @>")
	}
	p.advance()
	value, err := p.parseComparisonLiteral()
	if err != nil {
		return Predicate{}, err
	}
	return Cmp(spec, op, value), nil
}

// comparisonOp maps a comparison token to its predicate Op.
func comparisonOp(k tokKind) (Op, bool) {
	switch k {
	case tEq:
		return Eq, true
	case tNe:
		return Ne, true
	case tLt:
		return Lt, true
	case tLe:
		return Le, true
	case tGt:
		return Gt, true
	case tGe:
		return Ge, true
	default:
		return 0, false
	}
}

// parseComparisonLiteral parses a comparison right-hand side: a number, a
// string, or a boolean. null is rejected here — the null test is IS [NOT] NULL,
// since a comparison against null is never satisfied by the value semantics.
func (p *parser) parseComparisonLiteral() (any, error) {
	switch p.tok.kind {
	case tNumber:
		v, err := parseNumberLiteral(p.tok.text)
		if err != nil {
			return nil, p.errHere("invalid number %q", p.tok.text)
		}
		p.advance()
		return v, nil
	case tString:
		s := p.tok.text
		p.advance()
		return s, nil
	case tIdent:
		switch strings.ToUpper(p.tok.text) {
		case "TRUE":
			p.advance()
			return true, nil
		case "FALSE":
			p.advance()
			return false, nil
		case "NULL":
			return nil, p.errHere("null is not a comparison operand; use IS [NOT] NULL")
		}
	}
	return nil, p.errHere("expected a number, string, or boolean literal")
}

// parseNumberLiteral converts a numeric literal to the Go type Cmp infers from:
// an exact int64 when it is an integer that fits, a float64 otherwise, so
// integers past float64's mantissa keep their exact comparison.
func parseNumberLiteral(text string) (any, error) {
	if !strings.ContainsAny(text, ".eE") {
		if n, err := strconv.ParseInt(text, 10, 64); err == nil {
			return n, nil
		}
	}
	f, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return nil, err
	}
	return f, nil
}

// parseContainsNeedle consumes the '@>' operator and scans the JSON value that
// follows straight from the source, returning it verbatim for Contains to
// validate at compile. The current token is '@>' on entry, so lx.pos sits just
// past it and the JSON begins there.
func (p *parser) parseContainsNeedle() (string, error) {
	needle, end, err := scanJSONValue(p.lx.src, p.lx.pos)
	if err != nil {
		return "", err
	}
	p.lx.pos = end
	p.advance() // load the token after the JSON value
	return needle, nil
}

// parseOrderBy parses one or more ORDER BY keys, each an optional ASC/DESC.
func (p *parser) parseOrderBy(q *Query) error {
	for {
		spec, err := p.parsePath()
		if err != nil {
			return err
		}
		dir := Asc
		switch {
		case p.acceptKeyword("ASC"):
			dir = Asc
		case p.acceptKeyword("DESC"):
			dir = Desc
		}
		q.OrderBy(spec, dir)
		if p.tok.kind != tComma {
			return nil
		}
		p.advance()
	}
}

// parseLimit parses the LIMIT row cap: a non-negative integer.
func (p *parser) parseLimit(q *Query) error {
	if p.tok.kind != tNumber {
		return p.errHere("expected an integer after LIMIT")
	}
	n, err := strconv.Atoi(p.tok.text)
	if err != nil || n < 0 {
		return p.errHere("LIMIT must be a non-negative integer")
	}
	p.advance()
	q.Limit(n)
	return nil
}

// --- JSON value scanner (for '@>') ----------------------------------------

// scanJSONValue reads one JSON value from src starting at from (leading
// whitespace skipped) and returns its exact source text and the offset just
// past it. It delimits the value structurally — balancing objects and arrays
// with string awareness, and taking a scalar run to its natural end — so the
// parser can resume after the needle; full JSON validation is the compile step's
// job (Contains builds the needle's index). A malformed or missing value is a
// *ParseError at the offending offset.
func scanJSONValue(src string, from int) (string, int, error) {
	i := from
	for i < len(src) && isSpace(src[i]) {
		i++
	}
	if i >= len(src) {
		return "", 0, &ParseError{Pos: i, Msg: "expected a JSON value after @>"}
	}
	start := i
	switch src[i] {
	case '{', '[':
		end, err := scanJSONContainer(src, i)
		if err != nil {
			return "", 0, err
		}
		return src[start:end], end, nil
	case '"':
		end, err := scanJSONString(src, i)
		if err != nil {
			return "", 0, err
		}
		return src[start:end], end, nil
	default:
		end := scanJSONScalar(src, i)
		if end == start {
			return "", 0, &ParseError{Pos: start, Msg: "expected a JSON value after @>"}
		}
		return src[start:end], end, nil
	}
}

// scanJSONContainer returns the offset just past a balanced object or array,
// respecting strings so a brace inside a string does not disturb the balance.
func scanJSONContainer(src string, i int) (int, error) {
	depth := 0
	for i < len(src) {
		switch src[i] {
		case '{', '[':
			depth++
			i++
		case '}', ']':
			depth--
			i++
			if depth == 0 {
				return i, nil
			}
		case '"':
			end, err := scanJSONString(src, i)
			if err != nil {
				return 0, err
			}
			i = end
		default:
			i++
		}
	}
	return 0, &ParseError{Pos: i, Msg: "unterminated JSON value after @>"}
}

// scanJSONString returns the offset just past a JSON string, honoring backslash
// escapes so an escaped quote does not close the string early.
func scanJSONString(src string, i int) (int, error) {
	i++ // opening quote
	for i < len(src) {
		switch src[i] {
		case '\\':
			i += 2
		case '"':
			return i + 1, nil
		default:
			i++
		}
	}
	return 0, &ParseError{Pos: i, Msg: "unterminated JSON string after @>"}
}

// scanJSONScalar returns the offset just past a JSON scalar run (a number, or a
// true/false/null keyword), i.e. up to the first byte that cannot continue one.
func scanJSONScalar(src string, i int) int {
	for i < len(src) {
		c := src[i]
		if isSpace(c) || c == ',' || c == ')' || c == ']' || c == '}' {
			break
		}
		i++
	}
	return i
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }
