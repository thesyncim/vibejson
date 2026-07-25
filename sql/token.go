package sql

// tokenKind enumerates the lexemes. Keywords are not distinct kinds: a keyword
// is a tokIdent whose kw field resolved, so an unquoted identifier that happens
// to spell a keyword is still available as a field name wherever the grammar is
// unambiguous — which matters more here than in a schema'd SQL, because a JSON
// document is free to contain a key named "order" or "count".
type tokenKind uint8

const (
	tokEOF tokenKind = iota
	tokError
	tokIdent       // unquoted identifier; kw names its keyword, if any
	tokQuotedIdent // "..."; never a keyword, whatever it spells
	tokNumber      // JSON number grammar, kept as its exact source spelling
	tokString      // '...'
	tokParam       // ?
	tokStar        // *
	tokComma
	tokSemicolon
	tokLParen
	tokRParen
	tokLBracket
	tokRBracket
	tokDot
	tokEq
	tokNe
	tokLt
	tokLe
	tokGt
	tokGe
	tokContains // @>
)

// A token is one lexeme. text is a slice of the source for identifiers,
// numbers, and the undecoded body of a quoted token; esc says whether that body
// contains a doubled quote and therefore still needs decoding.
//
// Holding the raw body plus a flag, rather than a decoded string, is what keeps
// the lexer allocation-free. The obvious spelling — build the decoded value in
// a strings.Builder as the string is scanned — costs one heap allocation per
// string literal in every statement, on a package whose whole point is to reach
// a zero-allocation steady state. Almost no literal contains a doubled quote,
// so almost no literal needs decoding at all, and the ones that do decode into
// the parser's shared scratch on their way into the arena.
type token struct {
	kind tokenKind
	kw   keyword
	text string
	pos  int
	esc  bool
}

// A keyword identifies a reserved word. Keywords are matched case-insensitively
// (SQL's rule); identifiers are not (see the package documentation — they are
// JSON keys, and JSON keys are case-sensitive).
type keyword uint8

const (
	kwNone keyword = iota
	kwAll
	kwAnd
	kwAs
	kwAsc
	kwAvg
	kwBetween
	kwBy
	kwCase
	kwCast
	kwCollate
	kwCount
	kwCross
	kwDelete
	kwDesc
	kwDistinct
	kwEscape
	kwExcept
	kwExists
	kwFalse
	kwFetch
	kwFirst
	kwFrom
	kwFull
	kwGroup
	kwHaving
	kwIlike
	kwIn
	kwInner
	kwInsert
	kwIntersect
	kwIs
	kwJoin
	kwLast
	kwLeft
	kwLike
	kwLimit
	kwMax
	kwMin
	kwMissing
	kwNatural
	kwNot
	kwNull
	kwNulls
	kwOffset
	kwOn
	kwOr
	kwOrder
	kwOuter
	kwOver
	kwRight
	kwSelect
	kwSimilar
	kwSum
	kwTrue
	kwUnion
	kwUpdate
	kwUsing
	kwValues
	kwWhen
	kwWhere
	kwWindow
	kwWith
)

// maxKeywordLen is the longest keyword above. It bounds the stack buffer
// keywordOf folds into, so keyword recognition needs no map and no heap.
const maxKeywordLen = 9 // INTERSECT

// keywordOf recognizes s case-insensitively, or answers kwNone.
//
// The fold-into-an-array-then-switch shape is chosen over a package-level
// map[string]keyword because the map form requires an uppercased key, and
// strings.ToUpper allocates one string per identifier in every statement — a
// per-token heap allocation to save a comparison the compiler turns into a
// jump table anyway. `switch string(buf[:n])` is recognized by the compiler and
// does not copy the array.
func keywordOf(s string) keyword {
	if len(s) == 0 || len(s) > maxKeywordLen {
		return kwNone
	}
	var buf [maxKeywordLen]byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		buf[i] = c
	}
	switch string(buf[:len(s)]) {
	case "ALL":
		return kwAll
	case "AND":
		return kwAnd
	case "AS":
		return kwAs
	case "ASC":
		return kwAsc
	case "AVG":
		return kwAvg
	case "BETWEEN":
		return kwBetween
	case "BY":
		return kwBy
	case "CASE":
		return kwCase
	case "CAST":
		return kwCast
	case "COLLATE":
		return kwCollate
	case "COUNT":
		return kwCount
	case "CROSS":
		return kwCross
	case "DELETE":
		return kwDelete
	case "DESC":
		return kwDesc
	case "DISTINCT":
		return kwDistinct
	case "ESCAPE":
		return kwEscape
	case "EXCEPT":
		return kwExcept
	case "EXISTS":
		return kwExists
	case "FALSE":
		return kwFalse
	case "FETCH":
		return kwFetch
	case "FIRST":
		return kwFirst
	case "FROM":
		return kwFrom
	case "FULL":
		return kwFull
	case "GROUP":
		return kwGroup
	case "HAVING":
		return kwHaving
	case "ILIKE":
		return kwIlike
	case "IN":
		return kwIn
	case "INNER":
		return kwInner
	case "INSERT":
		return kwInsert
	case "INTERSECT":
		return kwIntersect
	case "IS":
		return kwIs
	case "JOIN":
		return kwJoin
	case "LAST":
		return kwLast
	case "LEFT":
		return kwLeft
	case "LIKE":
		return kwLike
	case "LIMIT":
		return kwLimit
	case "MAX":
		return kwMax
	case "MIN":
		return kwMin
	case "MISSING":
		return kwMissing
	case "NATURAL":
		return kwNatural
	case "NOT":
		return kwNot
	case "NULL":
		return kwNull
	case "NULLS":
		return kwNulls
	case "OFFSET":
		return kwOffset
	case "ON":
		return kwOn
	case "OR":
		return kwOr
	case "ORDER":
		return kwOrder
	case "OUTER":
		return kwOuter
	case "OVER":
		return kwOver
	case "RIGHT":
		return kwRight
	case "SELECT":
		return kwSelect
	case "SIMILAR":
		return kwSimilar
	case "SUM":
		return kwSum
	case "TRUE":
		return kwTrue
	case "UNION":
		return kwUnion
	case "UPDATE":
		return kwUpdate
	case "USING":
		return kwUsing
	case "VALUES":
		return kwValues
	case "WHEN":
		return kwWhen
	case "WHERE":
		return kwWhere
	case "WINDOW":
		return kwWindow
	case "WITH":
		return kwWith
	}
	return kwNone
}

// reserved reports whether kw may not name a field or a collection unless it
// is quoted.
//
// The split between reserved and non-reserved words is SQL's, and it is worth
// keeping here even though a schemaless store makes unquoted keywords more
// tempting: JSON documents really do have keys called "order" and "from", and
// admitting all of them unquoted is the obvious accommodation. It is the wrong
// one. A clause keyword that can also start a path makes the commonest typo in
// SQL — a stray comma before FROM — parse as a column list ending in a field
// named FROM, and the error then surfaces two tokens later pointing at the
// table name. Reserving the words the grammar itself uses keeps the error where
// the mistake is, and `"order"` remains available for the field.
//
// The aggregate names are deliberately not reserved. They are only calls when a
// '(' follows, so `SELECT count FROM t` is unambiguous, and a document with a
// field called "count" is not exotic. MISSING is not reserved either: it is
// this dialect's own addition, and reserving a word SQL does not would surprise
// an author who has never seen it.
func reserved(kw keyword) bool {
	switch kw {
	case kwNone,
		kwCount, kwSum, kwAvg, kwMin, kwMax,
		kwMissing, kwNulls, kwFirst, kwLast, kwEscape:
		return false
	}
	return true
}

// aggregateOf maps the aggregate keywords to their AggKind. A keyword is an
// aggregate call only when a '(' follows it, so this answering non-zero is a
// necessary but not sufficient condition; see parseResultColumn.
func aggregateOf(kw keyword) (AggKind, bool) {
	switch kw {
	case kwCount:
		return AggCount, true
	case kwSum:
		return AggSum, true
	case kwAvg:
		return AggAvg, true
	case kwMin:
		return AggMin, true
	case kwMax:
		return AggMax, true
	}
	return AggNone, false
}
