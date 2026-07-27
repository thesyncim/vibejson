package vibejson

import (
	"math/bits"

	simdkernels "github.com/thesyncim/vibejson/internal/kernels"
)

// ScanFirstRawTrusted returns the JSON Pointer target as a raw source slice
// without validating src. It is the explicit spelling of a trusted navigation
// contract: navigation trusts the document.
//
// For src that is valid JSON the three results are identical to
// [ScanFirstRaw], including its rule that each pointer token resolves to the
// first matching object member under which the rest of the pointer resolves.
// For src that is not valid JSON the results are unspecified — the call may
// report a garbage span or an absent target — but it is always memory-safe:
// it never panics, never reads outside src, and always terminates. It
// performs only the structural scanning navigation needs — escape-aware
// string skipping and bracket matching — with no UTF-8 validation, no number
// or literal grammar checks, and no validation of skipped subtrees or of any
// byte past the target.
//
// It is for inputs that have already been validated once (previously
// validated, indexed, or decoded, or produced by a trusted encoder) and for
// callers that accept garbage in, garbage out. All other callers should use
// [ScanFirstRaw] or [GetRaw]. Pointer syntax is still fully checked and
// returns a [document.PointerError], and the maximum nesting depth is still
// enforced exactly as in ScanFirstRaw, so trusted scans reject the same
// deeply nested documents with a [SyntaxError]. An absent target returns a
// zero RawValue, false, and nil. The returned RawValue aliases src.
//
// This spelling walks the pointer text as it descends and allocates nothing,
// matching the package-level [ScanFirstRaw] and [GetRaw]. Compiling once with
// [CompilePointer] still pays off on hot paths — it hashes and unescapes the
// tokens ahead of time — so reuse [CompiledPointer.ScanFirstRawTrusted] when
// the same pointer resolves many documents.
func ScanFirstRawTrusted(src []byte, pointer string) (RawValue, bool, error) {
	return ScanFirstRawTrustedOptions(src, pointer, Options{})
}

// ScanFirstRawTrustedOptions is [ScanFirstRawTrusted] with parser options.
//
// It streams the pointer instead of compiling it, so a one-shot trusted scan
// costs no allocation. Syntax is validated up front — the whole pointer, not
// only the traversed prefix — so a malformed token past an early miss is still
// rejected, exactly as the compiling spelling was.
func ScanFirstRawTrustedOptions(src []byte, pointer string, opts Options) (RawValue, bool, error) {
	if err := validatePointerSyntax(pointer); err != nil {
		return RawValue{}, false, err
	}
	s := trustedSeeker{src: src, maxDepth: opts.MaxDepth}
	if s.maxDepth <= 0 {
		s.maxDepth = defaultMaxDepth
	}
	s.i = skipSpace(src, 0)
	if pointer == "" {
		return s.capture(0)
	}
	return s.findString(0, 1, pointer)
}

// ScanFirstRawTrusted returns p's target without validating src, with the
// borrowing, duplicate-key, absence, depth, and safety semantics of the
// package-level [ScanFirstRawTrusted]: identical results to
// [CompiledPointer.ScanFirstRaw] on valid JSON, unspecified but memory-safe
// results otherwise.
func (p CompiledPointer) ScanFirstRawTrusted(src []byte) (RawValue, bool, error) {
	return p.ScanFirstRawTrustedOptions(src, Options{})
}

// ScanFirstRawTrustedOptions is [CompiledPointer.ScanFirstRawTrusted] with
// parser options.
func (p CompiledPointer) ScanFirstRawTrustedOptions(src []byte, opts Options) (RawValue, bool, error) {
	s := trustedSeeker{src: src, maxDepth: opts.MaxDepth}
	if s.maxDepth <= 0 {
		s.maxDepth = DefaultMaxDepth
	}
	s.i = SkipSpace(src, 0)
	return s.find(0, 0, p)
}

// GetRawTrusted resolves p with GetRaw's last-duplicate semantics over input
// that a stronger owner has already validated. It is intentionally internal:
// The durable Store uses it only after page-cache admission has validated every inline
// JSON document. Unlike ScanFirstRawTrusted it consumes later duplicates, but
// it still skips JSON grammar and UTF-8 checks already paid at admission.
func (p CompiledPointer) GetRawTrusted(src []byte) (RawValue, bool, error) {
	s := trustedSeeker{src: src, maxDepth: DefaultMaxDepth, lastWins: true}
	s.i = SkipSpace(src, 0)
	return s.find(0, 0, p)
}

// trustedSeeker is rawSeeker's non-validating counterpart. It mirrors the
// validating seeker's traversal structure exactly — same member loops, same
// consume-then-continue handling of matched members whose subtree does not
// resolve, same depth accounting — so that on valid input the two produce
// identical results, and replaces every validation step with structural
// scanning. On malformed input it reports the target absent instead of
// diagnosing a syntax error; the only errors it returns are pointer errors
// and the depth limit.
//
// Safety discipline: every read is behind a length check, every loop
// iteration either returns or strictly advances i, and the only recursion is
// one frame per pointer token, so arbitrary input cannot fault, hang, or
// overflow the stack.
type trustedSeeker struct {
	src      []byte
	i        int
	maxDepth int
	done     bool
	lastWins bool
}

func (s *trustedSeeker) find(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if tokenIndex >= len(pointer.Tokens) {
		return s.capture(depth)
	}
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	if s.i >= len(s.src) {
		return RawValue{}, false, nil
	}
	switch s.src[s.i] {
	case '{':
		return s.findObject(depth+1, tokenIndex, pointer)
	case '[':
		return s.findArray(depth+1, tokenIndex, pointer)
	default:
		// A scalar cannot contain the remaining pointer tokens. Consume it so
		// the enclosing member loop stays positioned, exactly like the
		// validating seeker.
		if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		return RawValue{}, false, nil
	}
}

// findString is find's streaming counterpart. It walks the pointer text token
// by token — parsing each token straight from the string as it descends —
// instead of indexing a precompiled token slice, which is what lets the
// package-level [ScanFirstRawTrusted] resolve a pointer with no per-call
// allocation, the same zero-allocation contract the validating [ScanFirstRaw]
// meets. tokenStart is the offset in pointer just past the slash that
// introduces the current token (1 for the first token); a tokenStart past the
// end means every token has been consumed and the value at s.i is the target.
// Its traversal structure mirrors find exactly, so on valid input the two
// spellings return identical spans.
func (s *trustedSeeker) findString(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if tokenStart > len(pointer) {
		return s.capture(depth)
	}
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	if s.i >= len(s.src) {
		return RawValue{}, false, nil
	}
	switch s.src[s.i] {
	case '{':
		return s.findObjectString(depth+1, tokenStart, pointer)
	case '[':
		return s.findArrayString(depth+1, tokenStart, pointer)
	default:
		// A scalar cannot contain the remaining pointer tokens. Consume it so
		// the enclosing member loop stays positioned, exactly like find.
		if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		return RawValue{}, false, nil
	}
}

// findArrayString is findArray for a streamed pointer: it reads the current
// token straight from pointer and classifies it as an array index, then
// mirrors findArray's element loop and non-validating skips.
func (s *trustedSeeker) findArrayString(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	tokenEnd, nextToken := pointerToken(pointer, tokenStart)
	token, err := unescapePointerToken(pointer[tokenStart:tokenEnd])
	if err != nil {
		return RawValue{}, false, err
	}
	index, indexOK, err := parsePointerIndex(token)
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.i = skipSpace(s.src, s.i)
	if s.i < len(s.src) && s.src[s.i] == ']' {
		s.i++
		return RawValue{}, false, nil
	}

	for elem := 0; ; elem++ {
		s.i = skipSpace(s.src, s.i)
		if indexOK && elem == index {
			raw, ok, err := s.findString(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
			// The target under this element is absent. Keep consuming the
			// array so the enclosing loops stay positioned; the result is
			// already known to be absent because indices are unique.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = skipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

// findObjectString is findObject for a streamed pointer: it reads the current
// token straight from pointer and matches it against each member key, then
// mirrors findObject's member loop and non-validating skips.
func (s *trustedSeeker) findObjectString(depth, tokenStart int, pointer string) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	tokenEnd, nextToken := pointerToken(pointer, tokenStart)
	token, err := unescapePointerToken(pointer[tokenStart:tokenEnd])
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	for {
		s.i = skipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			return RawValue{}, false, nil
		}
		if s.src[s.i] == '}' {
			s.i++
			return RawValue{}, false, nil
		}
		if s.src[s.i] != '"' {
			return RawValue{}, false, nil
		}
		keyStart, keyEnd, escaped := s.scanKey()
		matched := s.trustedKeyMatches(token, keyStart, keyEnd, escaped)
		s.i = skipSpace(s.src, s.i)
		if s.i >= len(s.src) || s.src[s.i] != ':' {
			return RawValue{}, false, nil
		}
		s.i++
		s.i = skipSpace(s.src, s.i)
		if matched {
			raw, ok, err := s.findString(depth, nextToken, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
			// The target under this member is absent, but a later duplicate
			// of the same key may still resolve; the subtree has been
			// consumed, so continue the member loop like findObject does.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = skipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

// capture consumes the value at s.i and returns its span. On valid input the
// structural skip ends exactly where validation would, so the span matches
// the validating seeker's byte for byte.
func (s *trustedSeeker) capture(depth int) (RawValue, bool, error) {
	start := s.i
	if err := s.skipValue(depth); err != nil {
		return RawValue{}, false, err
	}
	if s.i == start {
		// Nothing to capture: end of input or a stray delimiter, which valid
		// JSON never puts at a value position.
		return RawValue{}, false, nil
	}
	if !s.lastWins {
		s.done = true
	}
	return RawValue{Src: s.src[start:s.i]}, true, nil
}

func (s *trustedSeeker) findArray(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	index, indexOK, err := pointer.Tokens[tokenIndex].arrayIndex()
	if err != nil {
		return RawValue{}, false, err
	}

	s.i++
	s.i = SkipSpace(s.src, s.i)
	if s.i < len(s.src) && s.src[s.i] == ']' {
		s.i++
		return RawValue{}, false, nil
	}

	var selected RawValue
	var selectedOK bool
	for elem := 0; ; elem++ {
		s.i = SkipSpace(s.src, s.i)
		if indexOK && elem == index {
			raw, ok, err := s.find(depth, tokenIndex+1, pointer)
			if err != nil || s.done {
				return raw, ok, err
			}
			if s.lastWins {
				selected, selectedOK = raw, ok
			}
			// The target under this element is absent. Keep consuming the
			// array so the enclosing loops stay positioned; the result is
			// already known to be absent because indices are unique.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return selected, selectedOK, nil
			}
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			if s.lastWins {
				return selected, selectedOK, nil
			}
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

func (s *trustedSeeker) findObject(depth, tokenIndex int, pointer CompiledPointer) (RawValue, bool, error) {
	if depth > s.maxDepth {
		return RawValue{}, false, syntaxError(s.src, s.i, "maximum nesting depth exceeded")
	}
	token := pointer.Tokens[tokenIndex].Text

	s.i++
	var last RawValue
	var lastOK bool
	for {
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		if s.src[s.i] == '}' {
			s.i++
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		if s.src[s.i] != '"' {
			return RawValue{}, false, nil
		}
		keyStart, keyEnd, escaped := s.scanKey()
		matched := s.trustedKeyMatches(token, keyStart, keyEnd, escaped)
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) || s.src[s.i] != ':' {
			return RawValue{}, false, nil
		}
		s.i++
		s.i = SkipSpace(s.src, s.i)
		if matched {
			raw, ok, err := s.find(depth, tokenIndex+1, pointer)
			if err != nil {
				return RawValue{}, false, err
			}
			if s.lastWins {
				last, lastOK = raw, ok
			} else if s.done {
				return raw, ok, err
			}
			// The target under this member is absent, but a later duplicate
			// of the same key may still resolve; the subtree has been
			// consumed, so continue the member loop like the validating
			// seeker does.
		} else if err := s.skipValue(depth); err != nil {
			return RawValue{}, false, err
		}
		s.i = SkipSpace(s.src, s.i)
		if s.i >= len(s.src) {
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		}
		switch s.src[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			if s.lastWins {
				return last, lastOK, nil
			}
			return RawValue{}, false, nil
		default:
			return RawValue{}, false, nil
		}
	}
}

// scanKey scans the object key whose opening quote sits at s.i and leaves s.i
// just past the closing quote. It finds the closing quote with the same
// escape awareness as the validating seeker — a preceding backslash never
// terminates the key — but validates neither the escapes nor the bytes. An
// unterminated key exhausts the input; the caller's loop then reports the
// target absent.
func (s *trustedSeeker) scanKey() (start, end int, escaped bool) {
	s.i++
	start = s.i
	for {
		j := scanStringSyntax(s.src, s.i)
		if j >= len(s.src) {
			s.i = len(s.src)
			return start, len(s.src), escaped
		}
		switch s.src[j] {
		case '"':
			s.i = j + 1
			return start, j, escaped
		case '\\':
			escaped = true
			s.i = j + 2
			if s.i > len(s.src) {
				s.i = len(s.src)
				return start, len(s.src), escaped
			}
		default:
			// A raw control byte never appears in a valid string; carry it as
			// content.
			s.i = j + 1
		}
	}
}

// trustedKeyMatches reports whether the scanned key equals token. Escaped
// keys decode through the parser; a key whose escapes are malformed cannot
// have a decoded spelling, so it simply does not match.
func (s *trustedSeeker) trustedKeyMatches(token string, keyStart, keyEnd int, escaped bool) bool {
	if !escaped {
		return BytesEqualString(s.src[keyStart:keyEnd], token)
	}
	p := parser{src: s.src, i: keyStart - 1, maxDepth: s.maxDepth, zeroCopy: true}
	key, err := p.parseString()
	if err != nil {
		return false
	}
	return key == token
}

// skipValue consumes the value at s.i using structural scanning only. On
// valid input it consumes exactly the bytes the validator would. The only
// error is the depth limit; truncated input exhausts src.
func (s *trustedSeeker) skipValue(depth int) error {
	if s.i >= len(s.src) {
		return nil
	}
	switch s.src[s.i] {
	case '{', '[':
		return s.skipComposite(depth)
	case '"':
		s.i = skipStringTrusted(s.src, s.i+1)
		return nil
	default:
		s.i = skipScalarTrusted(s.src, s.i)
		return nil
	}
}

// skipComposite consumes the object or array opening at s.i with the
// stage-1 bitmap pipeline: each 64-byte block classifies into quote,
// backslash, and bracket masks, escape resolution and the prefix-XOR string
// mask silence everything inside strings, and the surviving brackets adjust
// the nesting count — by popcount when the block provably cannot close the
// composite or exceed the depth limit, and bit by bit otherwise. depth is
// the nesting depth of the enclosing value position, exactly as the
// validator passes it, and each opened container is charged against the same
// limit at the same byte offset, so valid documents that are too deep fail
// identically under both seekers.
func (s *trustedSeeker) skipComposite(depth int) error {
	src := s.src
	i := s.i
	budget := s.maxDepth - depth
	nest := 0
	var carry simdkernels.Stage1Carry
	var m simdkernels.Stage1BracketMasks
	for i < len(src) {
		block := (*[64]byte)(nil)
		if len(src)-i >= 64 {
			block = (*[64]byte)(src[i:])
		} else {
			// The zero padding classifies as control bytes: no quotes, no
			// brackets, so a truncated composite simply exhausts the input.
			var tail [64]byte
			copy(tail[:], src[i:])
			block = &tail
		}
		simdkernels.Stage1BlockBrackets(block, &m)
		inString := simdkernels.Stage1PrefixXOR(m.Quote&^simdkernels.Stage1Escaped(m.Backslash, &carry), &carry)
		open := m.Open &^ inString
		closes := m.Close &^ inString
		opened := bits.OnesCount64(open)
		closed := bits.OnesCount64(closes)
		// Even with every close first the count stays positive, and even
		// with every open first it stays within budget: take the whole block
		// by popcount.
		if nest-closed > 0 && nest+opened <= budget {
			nest += opened - closed
			i += 64
			continue
		}
		if nest+opened <= budget {
			// The block cannot exceed the budget, so only the zero crossing
			// needs a position, and the count crosses zero only at a close:
			// walk close bits alone and batch the opens below each one by
			// popcount. The crossing close sees exactly one level above zero
			// because the count moves one level per bracket and the composite
			// opens at bit zero of the first block.
			ordinal := 0
			for br := closes; br != 0; br &= br - 1 {
				p := bits.TrailingZeros64(br)
				if nest+bits.OnesCount64(open&(uint64(1)<<p-1))-ordinal == 1 {
					s.i = i + p + 1
					return nil
				}
				ordinal++
			}
			nest += opened - closed
			i += 64
			continue
		}
		for br := open | closes; br != 0; br &= br - 1 {
			if bit := br & (^br + 1); open&bit != 0 {
				nest++
				if nest > budget {
					pos := i + bits.TrailingZeros64(br)
					s.i = pos
					return syntaxError(src, pos, "maximum nesting depth exceeded")
				}
			} else {
				nest--
				if nest == 0 {
					s.i = i + bits.TrailingZeros64(br) + 1
					return nil
				}
			}
		}
		i += 64
	}
	s.i = len(src)
	return nil
}

// skipStringTrusted returns the index just past the closing quote of the
// string whose first content byte is at i, or len(src) when the string is
// unterminated. It relies on the string-syntax kernel for the long spans and
// steps over each backslash and the byte it escapes, so an escaped quote
// never terminates the string; that is the entire escape treatment, which is
// exact for every valid string because the four hex digits of a Unicode
// escape contain neither quotes nor backslashes.
func skipStringTrusted(src []byte, i int) int {
	for {
		j := scanStringSyntax(src, i)
		if j >= len(src) {
			return len(src)
		}
		switch src[j] {
		case '"':
			return j + 1
		case '\\':
			i = j + 2
			if i > len(src) {
				return len(src)
			}
		default:
			// Raw control bytes are string content in trusted mode.
			i = j + 1
		}
	}
}

// skipScalarTrusted consumes a number, literal, or arbitrary scalar-position
// bytes up to the next byte that can follow a scalar in valid JSON. Valid
// scalars contain none of the stop bytes, so on valid input this ends
// exactly where grammar validation would.
func skipScalarTrusted(src []byte, i int) int {
	for i < len(src) {
		c := src[i]
		if c <= ' ' || c == ',' || c == '}' || c == ']' {
			return i
		}
		i++
	}
	return len(src)
}
